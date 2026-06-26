<script lang="ts">
    /**
     * Live scopes for the program feed: luma waveform, RGB overlay
     * waveform, RGB parade, or vectorscope. Computed on the CPU from a
     * 256×144 downsample (~37k px, well under 2ms) at ~24fps.
     *
     * The panel is a small window: drag it anywhere by the header, resize
     * from the corner grip; layout persists. Deliberately independent of
     * the controls auto-hide — a colorist keeps scopes up while the rest
     * of the UI sleeps.
     */
    import { untrack } from "svelte";
    import { scale } from "svelte/transition";
    import { quintOut } from "svelte/easing";
    import { getFrameBitmap } from "$lib/glass/frameSource";
    import { degradedInterval, setReviewToolActive } from "$lib/perf/loadMonitor";

    interface Props {
        videoElement: HTMLVideoElement | null;
        open: boolean;
        onClose: () => void;
    }

    let { videoElement, open, onClose }: Props = $props();

    const SRC_W = 256;
    const SRC_H = 144;
    const OUT_W = 256;
    const OUT_H = 128;
    const FRAME_MS = 42; // ~24fps
    const LAYOUT_KEY = "chromatic_scopes_layout";

    const MODES = [
        { id: "luma", label: "Luma" },
        { id: "rgb", label: "RGB" },
        { id: "parade", label: "Parade" },
        { id: "vector", label: "Vector" },
    ] as const;
    type Mode = (typeof MODES)[number]["id"];

    let mode = $state<Mode>("luma");
    let panelEl = $state<HTMLDivElement | null>(null);
    let displayCanvas = $state<HTMLCanvasElement | null>(null);
    let pos = $state<{ x: number; y: number } | null>(null);
    let width = $state(288);
    let sampleCanvas: HTMLCanvasElement | null = null;
    let raf = 0;
    let lastDrawAt = 0;
    let lastAnalyzed: ImageBitmap | null = null;
    let lastAnalyzedMode: Mode | null = null;
    let pointerInteractionCleanup: (() => void) | null = null;
    // Reused across ticks: a fresh 131KB buffer per analysis was ~6MB/s
    // of garbage for the lifetime of an open scopes panel.
    const outBuf = new Uint8ClampedArray(OUT_W * OUT_H * 4);
    const outImage = new ImageData(outBuf, OUT_W, OUT_H);

    try {
        const saved = JSON.parse(localStorage.getItem(LAYOUT_KEY) ?? "null");
        if (saved && typeof saved.w === "number") {
            width = Math.min(520, Math.max(220, saved.w));
            if (typeof saved.x === "number" && typeof saved.y === "number") {
                pos = { x: saved.x, y: saved.y };
            }
        }
    } catch {
        // Default layout.
    }

    function persistLayout() {
        try {
            localStorage.setItem(
                LAYOUT_KEY,
                JSON.stringify({ x: pos?.x, y: pos?.y, w: width }),
            );
        } catch {
            // In-session layout still applies.
        }
    }

    function clampPos(x: number, y: number): { x: number; y: number } {
        const wrapper = panelEl?.parentElement;
        if (!wrapper || !panelEl) return { x, y };
        const maxX = wrapper.clientWidth - panelEl.offsetWidth;
        const maxY = wrapper.clientHeight - panelEl.offsetHeight;
        return { x: Math.min(Math.max(0, x), Math.max(0, maxX)), y: Math.min(Math.max(0, y), Math.max(0, maxY)) };
    }

    function clearPointerInteraction() {
        pointerInteractionCleanup?.();
        pointerInteractionCleanup = null;
    }

    function startDrag(e: PointerEvent) {
        if (e.button !== 0 || !panelEl) return;
        if ((e.target as HTMLElement).closest("button")) return;
        e.preventDefault();
        clearPointerInteraction();
        const wrapper = panelEl.parentElement!;
        const wrapperRect = wrapper.getBoundingClientRect();
        const panelRect = panelEl.getBoundingClientRect();
        const dx = e.clientX - panelRect.left;
        const dy = e.clientY - panelRect.top;
        const move = (ev: PointerEvent) => {
            pos = clampPos(ev.clientX - wrapperRect.left - dx, ev.clientY - wrapperRect.top - dy);
        };
        const up = () => {
            clearPointerInteraction();
            persistLayout();
        };
        window.addEventListener("pointermove", move);
        window.addEventListener("pointerup", up);
        window.addEventListener("pointercancel", up);
        window.addEventListener("blur", up);
        pointerInteractionCleanup = () => {
            window.removeEventListener("pointermove", move);
            window.removeEventListener("pointerup", up);
            window.removeEventListener("pointercancel", up);
            window.removeEventListener("blur", up);
        };
    }

    function startResize(e: PointerEvent) {
        if (e.button !== 0) return;
        e.preventDefault();
        e.stopPropagation();
        clearPointerInteraction();
        const startX = e.clientX;
        const startW = width;
        const move = (ev: PointerEvent) => {
            width = Math.min(520, Math.max(220, startW + (ev.clientX - startX)));
        };
        const up = () => {
            clearPointerInteraction();
            if (pos) pos = clampPos(pos.x, pos.y);
            persistLayout();
        };
        window.addEventListener("pointermove", move);
        window.addEventListener("pointerup", up);
        window.addEventListener("pointercancel", up);
        window.addEventListener("blur", up);
        pointerInteractionCleanup = () => {
            window.removeEventListener("pointermove", move);
            window.removeEventListener("pointerup", up);
            window.removeEventListener("pointercancel", up);
            window.removeEventListener("blur", up);
        };
    }

    function stopDrawLoop() {
        if (raf) cancelAnimationFrame(raf);
        raf = 0;
    }

    function requestDrawLoop() {
        if (!open || raf || document.hidden) return;
        raf = requestAnimationFrame(drawScope);
    }

    function drawScope() {
        raf = 0;
        if (!open || document.hidden) return;
        const video = videoElement;
        if (!video || video.readyState < 2 || !displayCanvas) {
            requestDrawLoop();
            return;
        }
        const now = performance.now();
        // Lowest rung on the priority ladder: under load the scopes drop
        // to ~10fps before anything else is touched.
        if (now - lastDrawAt < degradedInterval("scopes", FRAME_MS)) {
            requestDrawLoop();
            return;
        }
        lastDrawAt = now;

        // Identical frame, same mode: the trace would be identical too.
        const bmp = getFrameBitmap(video);
        if (bmp && bmp === lastAnalyzed && mode === lastAnalyzedMode) {
            requestDrawLoop();
            return;
        }
        lastAnalyzed = bmp;
        lastAnalyzedMode = mode;

        if (!sampleCanvas) {
            sampleCanvas = document.createElement("canvas");
            sampleCanvas.width = SRC_W;
            sampleCanvas.height = SRC_H;
        }
        // Sample from the shared ~1024px frame bitmap when available:
        // scaling the raw (possibly 4K) video onto a CPU-readable canvas
        // every tick was a main-thread stall that dragged the whole room.
        const sctx = sampleCanvas.getContext("2d", { willReadFrequently: true })!;
        sctx.drawImage(bmp ?? video, 0, 0, SRC_W, SRC_H);
        const src = sctx.getImageData(0, 0, SRC_W, SRC_H).data;

        const out = outBuf;
        out.fill(0);

        if (mode === "vector") {
            // Graticule: center cross + 75% circle
            const cx = OUT_W / 2;
            const cy = OUT_H / 2;
            const radius = OUT_H * 0.46;
            for (let a = 0; a < 360; a += 2) {
                const gx = Math.round(cx + Math.cos((a * Math.PI) / 180) * radius * 0.75);
                const gy = Math.round(cy + Math.sin((a * Math.PI) / 180) * radius * 0.75);
                const gi = (gy * OUT_W + gx) * 4;
                out[gi] = out[gi + 1] = out[gi + 2] = 38;
                out[gi + 3] = 255;
            }
            for (let d = -6; d <= 6; d++) {
                let i = (cy * OUT_W + cx + d) * 4;
                out[i] = out[i + 1] = out[i + 2] = 38;
                out[i + 3] = 255;
                i = ((cy + d) * OUT_W + cx) * 4;
                out[i] = out[i + 1] = out[i + 2] = 38;
                out[i + 3] = 255;
            }
            for (let sy = 0; sy < SRC_H; sy++) {
                for (let sx = 0; sx < SRC_W; sx++) {
                    const si = (sy * SRC_W + sx) * 4;
                    const r = src[si];
                    const g = src[si + 1];
                    const b = src[si + 2];
                    const y = 0.2126 * r + 0.7152 * g + 0.0722 * b;
                    // BT.709 chroma, scaled into the plot radius
                    const cb = (b - y) * 0.5389;
                    const cr = (r - y) * 0.635;
                    const ox = Math.round(cx + (cb / 128) * radius);
                    const oy = Math.round(cy - (cr / 128) * radius);
                    if (ox < 0 || ox >= OUT_W || oy < 0 || oy >= OUT_H) continue;
                    const oi = (oy * OUT_W + ox) * 4;
                    // Trace tinted by its own chroma
                    out[oi] = Math.min(255, out[oi] + 14 + r * 0.07);
                    out[oi + 1] = Math.min(255, out[oi + 1] + 14 + g * 0.07);
                    out[oi + 2] = Math.min(255, out[oi + 2] + 14 + b * 0.07);
                    out[oi + 3] = 255;
                }
            }
        } else {
            // Shared graticule for the waveform modes (25/50/75%)
            for (const f of [0.25, 0.5, 0.75]) {
                const y = Math.round((1 - f) * (OUT_H - 1));
                for (let x = 0; x < OUT_W; x++) {
                    const i = (y * OUT_W + x) * 4;
                    out[i] = out[i + 1] = out[i + 2] = 34;
                    out[i + 3] = 255;
                }
            }
            if (mode === "luma") {
                for (let sy = 0; sy < SRC_H; sy++) {
                    for (let sx = 0; sx < SRC_W; sx++) {
                        const si = (sy * SRC_W + sx) * 4;
                        const luma =
                            0.2126 * src[si] + 0.7152 * src[si + 1] + 0.0722 * src[si + 2];
                        const oy = Math.round((1 - luma / 255) * (OUT_H - 1));
                        const oi = (oy * OUT_W + sx) * 4;
                        out[oi + 1] = Math.min(255, out[oi + 1] + 26);
                        out[oi] = Math.min(160, out[oi] + 8);
                        out[oi + 2] = Math.min(160, out[oi + 2] + 8);
                        out[oi + 3] = 255;
                    }
                }
            } else if (mode === "rgb") {
                // All three channels overlaid full-width, each in its color
                for (let sy = 0; sy < SRC_H; sy++) {
                    for (let sx = 0; sx < SRC_W; sx++) {
                        const si = (sy * SRC_W + sx) * 4;
                        for (let c = 0; c < 3; c++) {
                            const oy = Math.round((1 - src[si + c] / 255) * (OUT_H - 1));
                            const oi = (oy * OUT_W + sx) * 4;
                            out[oi + c] = Math.min(255, out[oi + c] + 26);
                            out[oi + 3] = 255;
                        }
                    }
                }
            } else {
                // Parade: thirds, one channel each
                const third = OUT_W / 3;
                for (let sy = 0; sy < SRC_H; sy++) {
                    for (let sx = 0; sx < SRC_W; sx++) {
                        const si = (sy * SRC_W + sx) * 4;
                        for (let c = 0; c < 3; c++) {
                            const ox = Math.floor((sx / SRC_W) * (third - 1) + c * third);
                            const oy = Math.round((1 - src[si + c] / 255) * (OUT_H - 1));
                            const oi = (oy * OUT_W + ox) * 4;
                            out[oi + c] = Math.min(255, out[oi + c] + 30);
                            out[oi + 3] = 255;
                        }
                    }
                }
            }
        }

        const dctx = displayCanvas.getContext("2d")!;
        dctx.putImageData(outImage, 0, 0);
        requestDrawLoop();
    }

    $effect(() => {
        const registered = open;
        if (registered) setReviewToolActive("scopes", true);
        if (open) {
            requestDrawLoop();
        } else {
            clearPointerInteraction();
            stopDrawLoop();
        }
        return () => {
            if (registered) setReviewToolActive("scopes", false);
            clearPointerInteraction();
            stopDrawLoop();
        };
    });

    $effect(() => {
        const handleVisibility = () => {
            if (document.hidden) {
                stopDrawLoop();
            } else {
                requestDrawLoop();
            }
        };
        document.addEventListener("visibilitychange", handleVisibility);
        return () => document.removeEventListener("visibilitychange", handleVisibility);
    });

    // A position persisted on a wide window can land entirely outside a
    // smaller one — with the drag handle unreachable. Clamp on open and
    // again whenever the window resizes. The clamp must be untracked AND
    // change-guarded: clampPos returns a fresh object, so an effect that
    // reads pos and unconditionally reassigns it retriggers itself forever
    // (this froze the tab the moment scopes opened with a saved layout).
    function clampPosInPlace() {
        if (!pos) return;
        const c = clampPos(pos.x, pos.y);
        if (c.x !== pos.x || c.y !== pos.y) pos = c;
    }

    $effect(() => {
        if (!open || !panelEl) return;
        untrack(clampPosInPlace);
        window.addEventListener("resize", clampPosInPlace);
        return () => window.removeEventListener("resize", clampPosInPlace);
    });
</script>

{#if open}
    <div
        class="scopes-panel"
        bind:this={panelEl}
        style="width: {width}px; {pos ? `left: ${pos.x}px; top: ${pos.y}px; bottom: auto;` : ''}"
        transition:scale={{ start: 0.95, duration: 220, easing: quintOut }}
        role="img"
        aria-label="Video scopes"
    >
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div class="scopes-header" onpointerdown={startDrag}>
            <div class="scopes-modes" role="tablist" aria-label="Scope mode">
                {#each MODES as m (m.id)}
                    <button
                        class="scopes-mode"
                        class:active={mode === m.id}
                        role="tab"
                        aria-selected={mode === m.id}
                        onclick={() => (mode = m.id)}
                    >{m.label}</button>
                {/each}
            </div>
            <button class="scopes-btn" onclick={onClose} aria-label="Close scopes">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="12" height="12" aria-hidden="true"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>
            </button>
        </div>
        <canvas bind:this={displayCanvas} width={OUT_W} height={OUT_H} class="scopes-canvas"></canvas>
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div class="scopes-resize" onpointerdown={startResize} aria-hidden="true">
            <svg viewBox="0 0 10 10" width="10" height="10"><path d="M9 1v8H1" fill="none" stroke="rgba(255,255,255,0.3)" stroke-width="1.5" stroke-linecap="round"/></svg>
        </div>
    </div>
{/if}

<style>
    .scopes-panel {
        position: absolute;
        left: var(--space-lg);
        bottom: calc(112px + env(safe-area-inset-bottom, 0px));
        z-index: 18;
        padding: var(--space-sm);
        background: var(--glass-bg-deep);
        backdrop-filter: var(--glass-backdrop-deep);
        -webkit-backdrop-filter: var(--glass-backdrop-deep);
        border: 1px solid var(--glass-edge);
        border-radius: 14px;
        box-shadow: var(--glass-specular), var(--glass-shadow);
        transform-origin: bottom left;
    }

    .scopes-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: var(--space-sm);
        margin-bottom: var(--space-xs);
        cursor: grab;
        touch-action: none;
    }
    .scopes-header:active {
        cursor: grabbing;
    }

    .scopes-modes {
        display: flex;
        gap: 2px;
        background: rgba(255, 255, 255, 0.05);
        border-radius: var(--radius-full);
        padding: 2px;
    }

    .scopes-mode {
        border: none;
        background: transparent;
        color: var(--color-text-subtle);
        font-size: 0.625rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.04em;
        padding: 3px 8px;
        border-radius: var(--radius-full);
        cursor: pointer;
        transition: background 0.12s ease, color 0.12s ease;
        white-space: nowrap;
    }
    .scopes-mode:hover {
        color: var(--color-text);
    }
    .scopes-mode.active {
        background: rgba(255, 255, 255, 0.14);
        color: var(--color-text);
    }

    .scopes-btn {
        display: flex;
        align-items: center;
        justify-content: center;
        width: 22px;
        height: 22px;
        flex-shrink: 0;
        background: rgba(255, 255, 255, 0.07);
        border: none;
        border-radius: var(--radius-full);
        color: var(--color-text-muted);
        cursor: pointer;
        transition: background 0.12s ease, color 0.12s ease;
    }
    .scopes-btn:hover {
        background: rgba(255, 255, 255, 0.14);
        color: var(--color-text);
    }

    .scopes-canvas {
        display: block;
        width: 100%;
        border-radius: 8px;
        background: #0a0a0c;
    }

    .scopes-resize {
        position: absolute;
        right: 3px;
        bottom: 3px;
        width: 16px;
        height: 16px;
        display: flex;
        align-items: flex-end;
        justify-content: flex-end;
        cursor: nwse-resize;
        touch-action: none;
    }

    @media (max-width: 768px) {
        .scopes-panel {
            left: var(--space-sm);
        }
    }
</style>
