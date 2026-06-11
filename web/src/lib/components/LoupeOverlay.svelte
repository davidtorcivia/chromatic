<script lang="ts">
    /**
     * Pixel loupe: a circular magnifier that follows the cursor over the
     * video. WebGL when available: the video uploads to a texture once
     * per content tick, and moving the cursor just re-samples it — so the
     * lens tracks at full display rate with zero copies, and the rim gets
     * true continuous refraction (the picture bends and compresses
     * through the edge, no stepped bands). Canvas2D fallback otherwise.
     *
     * Scroll (or two-finger trackpad swipe) changes magnification;
     * clicking the picture exits. Pointer position is applied directly in
     * the input handler — never waits for a frame.
     */
    import { fade } from "svelte/transition";
    import { underPressure } from "$lib/perf/loadMonitor";

    interface Props {
        videoElement: HTMLVideoElement;
        /** Screen-share pane, when active: the lens magnifies whichever
         *  video is under the cursor. */
        shareElement?: HTMLVideoElement | null;
        enabled: boolean;
        onExit: () => void;
    }

    let { videoElement, shareElement = null, enabled, onExit }: Props = $props();

    const LENS_SIZE = 220; // css px diameter
    const MIN_ZOOM = 2;
    const MAX_ZOOM = 8;
    const HINT_KEY = "chromatic_loupe_hint_v2";
    const CONTENT_MS = /Gecko\/\d/.test(navigator.userAgent) ? 48 : 32;

    const VERT = `#version 300 es
in vec2 a_pos;
out vec2 v_pos;
void main() {
  v_pos = vec2(a_pos.x * 0.5 + 0.5, 0.5 - a_pos.y * 0.5);
  gl_Position = vec4(a_pos, 0.0, 1.0);
}`;

    const FRAG = `#version 300 es
precision highp float;
uniform sampler2D u_tex;     // full video frame, v=0 at top
uniform vec2 u_videoSize;    // native px
uniform vec2 u_center;       // native px under the cursor
uniform float u_srcHalf;     // native px half-extent at base zoom
in vec2 v_pos;
out vec4 frag;

void main() {
  vec2 p = v_pos * 2.0 - 1.0;  // -1..1, y down
  float r = length(p);
  if (r > 1.0) { frag = vec4(0.0); return; }

  // Optically perfect center: zero distortion inside 84% of the radius
  // (this is an examination tool first). The bend lives only in the
  // outer rim, continuous derivative so it reads as glass, not rings.
  float bulge = 1.0 + 0.5 * pow(smoothstep(0.84, 1.0, r), 1.6);
  vec2 uv = (u_center + p * u_srcHalf * bulge) / u_videoSize;
  vec3 c = (uv.x < 0.0 || uv.x > 1.0 || uv.y < 0.0 || uv.y > 1.0)
    ? vec3(0.0)
    : texture(u_tex, uv).rgb;

  // All lighting confined to the rim band: the center stays untouched
  // for color and detail judgment.
  float rim = smoothstep(0.84, 1.0, r);
  float topness = clamp(-p.y * 0.5 + 0.5, 0.0, 1.0);
  c += vec3(1.0) * rim * 0.18 * topness;
  c *= 1.0 - rim * 0.26 * (1.0 - topness);

  float alpha = smoothstep(1.0, 0.985, r);
  frag = vec4(c * alpha, alpha);
}`;

    let canvasEl = $state<HTMLCanvasElement | null>(null);
    let chipEl = $state<HTMLDivElement | null>(null);
    let zoom = $state(3);
    let showHint = $state(false);
    let hintTimer: ReturnType<typeof setTimeout> | null = null;
    let px = -1;
    let py = -1;
    let raf = 0;
    let lastUploadAt = 0;

    let gl: WebGL2RenderingContext | null = null;
    let glTried = false;
    let uTex: WebGLTexture | null = null;
    let uniforms: Record<string, WebGLUniformLocation | null> = {};
    let texIsNearest = false;

    function initGL(): void {
        glTried = true;
        if (!canvasEl) return;
        try {
            const ctx = canvasEl.getContext("webgl2", {
                alpha: true,
                premultipliedAlpha: true,
                antialias: false,
                depth: false,
                stencil: false,
            });
            if (!ctx) return;
            const compile = (type: number, src: string) => {
                const sh = ctx.createShader(type)!;
                ctx.shaderSource(sh, src);
                ctx.compileShader(sh);
                if (!ctx.getShaderParameter(sh, ctx.COMPILE_STATUS)) {
                    throw new Error(ctx.getShaderInfoLog(sh) ?? "compile failed");
                }
                return sh;
            };
            const prog = ctx.createProgram()!;
            ctx.attachShader(prog, compile(ctx.VERTEX_SHADER, VERT));
            ctx.attachShader(prog, compile(ctx.FRAGMENT_SHADER, FRAG));
            ctx.linkProgram(prog);
            if (!ctx.getProgramParameter(prog, ctx.LINK_STATUS)) {
                throw new Error(ctx.getProgramInfoLog(prog) ?? "link failed");
            }
            const quad = ctx.createBuffer()!;
            ctx.bindBuffer(ctx.ARRAY_BUFFER, quad);
            ctx.bufferData(ctx.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), ctx.STATIC_DRAW);
            ctx.useProgram(prog);
            const loc = ctx.getAttribLocation(prog, "a_pos");
            ctx.enableVertexAttribArray(loc);
            ctx.vertexAttribPointer(loc, 2, ctx.FLOAT, false, 0, 0);
            uTex = ctx.createTexture()!;
            ctx.bindTexture(ctx.TEXTURE_2D, uTex);
            ctx.texParameteri(ctx.TEXTURE_2D, ctx.TEXTURE_MIN_FILTER, ctx.LINEAR);
            ctx.texParameteri(ctx.TEXTURE_2D, ctx.TEXTURE_MAG_FILTER, ctx.LINEAR);
            ctx.texParameteri(ctx.TEXTURE_2D, ctx.TEXTURE_WRAP_S, ctx.CLAMP_TO_EDGE);
            ctx.texParameteri(ctx.TEXTURE_2D, ctx.TEXTURE_WRAP_T, ctx.CLAMP_TO_EDGE);
            for (const name of ["u_tex", "u_videoSize", "u_center", "u_srcHalf"]) {
                uniforms[name] = ctx.getUniformLocation(prog, name);
            }
            ctx.uniform1i(uniforms.u_tex, 0);
            gl = ctx;
        } catch {
            gl = null;
        }
    }

    /** Displayed content rect of an object-fit: contain video, page coords. */
    function contentRect(video: HTMLVideoElement): DOMRect | null {
        const vw = video.videoWidth;
        const vh = video.videoHeight;
        if (!vw || !vh) return null;
        const box = video.getBoundingClientRect();
        if (!box.width || !box.height) return null;
        const scale = Math.min(box.width / vw, box.height / vh);
        const w = vw * scale;
        const h = vh * scale;
        return new DOMRect(box.left + (box.width - w) / 2, box.top + (box.height - h) / 2, w, h);
    }

    let lastSource: HTMLVideoElement | null = null;

    /** Whichever playing video sits under the cursor (share pane wins ties
     *  by document order being checked first). */
    function sourceUnderPointer(): { video: HTMLVideoElement; rect: DOMRect } | null {
        for (const v of [shareElement, videoElement]) {
            if (!v || v.readyState < 2) continue;
            const r = contentRect(v);
            if (r && px >= r.left && px <= r.right && py >= r.top && py <= r.bottom) {
                return { video: v, rect: r };
            }
        }
        return null;
    }

    function handleMove(e: PointerEvent) {
        px = e.clientX;
        py = e.clientY;
        // Position applies on the input event itself — the lens never
        // waits for a frame to follow the cursor.
        if (enabled && canvasEl) {
            canvasEl.style.transform = `translate(${px - LENS_SIZE / 2}px, ${py - LENS_SIZE / 2}px)`;
            if (chipEl) {
                chipEl.style.transform = `translate(${px - 24}px, ${py + LENS_SIZE / 2 + 10}px)`;
            }
        }
    }

    function handleWheel(e: WheelEvent) {
        if (!enabled) return;
        e.preventDefault();
        zoom = Math.min(MAX_ZOOM, Math.max(MIN_ZOOM, zoom - Math.sign(e.deltaY) * 0.5));
    }

    // Click the picture to put the loupe away.
    function handlePointerDown(e: PointerEvent) {
        if (!enabled || e.button !== 0) return;
        const target = e.target as HTMLElement | null;
        if (target?.closest(".video-container")) onExit();
    }

    function draw() {
        raf = 0;
        if (!enabled || !canvasEl) return;
        raf = requestAnimationFrame(draw);
        const source = sourceUnderPointer();
        if (!source) {
            canvasEl.style.opacity = "0";
            if (chipEl) chipEl.style.opacity = "0";
            return;
        }

        const { video, rect } = source;
        if (video !== lastSource) {
            lastSource = video;
            lastUploadAt = 0; // new source: refresh the texture immediately
        }

        canvasEl.style.transform = `translate(${px - LENS_SIZE / 2}px, ${py - LENS_SIZE / 2}px)`;
        canvasEl.style.opacity = "1";
        if (chipEl) {
            chipEl.style.transform = `translate(${px - 24}px, ${py + LENS_SIZE / 2 + 10}px)`;
            chipEl.style.opacity = "1";
        }

        const dpr = Math.min(window.devicePixelRatio || 1, 2);
        const size = LENS_SIZE * dpr;
        if (canvasEl.width !== size) {
            canvasEl.width = size;
            canvasEl.height = size;
            if (gl) gl.viewport(0, 0, size, size);
        }
        const nativePerDisplay = video.videoWidth / rect.width;
        const srcHalf = ((LENS_SIZE / zoom) * nativePerDisplay) / 2;
        const cx = (px - rect.left) * nativePerDisplay;
        const cy = (py - rect.top) * nativePerDisplay;

        if (!glTried) initGL();

        if (gl) {
            const now = performance.now();
            gl.bindTexture(gl.TEXTURE_2D, uTex);
            // Yield under load: content freshness halves, tracking doesn't
            if (now - lastUploadAt >= (underPressure() ? CONTENT_MS * 2 : CONTENT_MS)) {
                lastUploadAt = now;
                gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, video);
            }
            // True pixels once a source pixel maps to ≥1 lens pixel
            const wantNearest = size / (srcHalf * 2) >= 1;
            if (wantNearest !== texIsNearest) {
                texIsNearest = wantNearest;
                gl.texParameteri(
                    gl.TEXTURE_2D,
                    gl.TEXTURE_MAG_FILTER,
                    wantNearest ? gl.NEAREST : gl.LINEAR,
                );
            }
            gl.uniform2f(uniforms.u_videoSize, video.videoWidth, video.videoHeight);
            gl.uniform2f(uniforms.u_center, cx, cy);
            gl.uniform1f(uniforms.u_srcHalf, srcHalf);
            gl.clearColor(0, 0, 0, 0);
            gl.clear(gl.COLOR_BUFFER_BIT);
            gl.drawArrays(gl.TRIANGLES, 0, 3);
            return;
        }

        // Canvas2D fallback: throttled content copies
        const now = performance.now();
        if (now - lastUploadAt < CONTENT_MS) return;
        lastUploadAt = now;
        const srcSize = srcHalf * 2;
        const ctx = canvasEl.getContext("2d")!;
        ctx.imageSmoothingEnabled = size / srcSize < 1;
        ctx.clearRect(0, 0, size, size);
        ctx.drawImage(video, cx - srcHalf, cy - srcHalf, srcSize, srcSize, 0, 0, size, size);
        const sheen = ctx.createRadialGradient(size * 0.5, size * 0.12, 0, size * 0.5, size * 0.3, size * 0.7);
        sheen.addColorStop(0, "rgba(255, 255, 255, 0.10)");
        sheen.addColorStop(1, "rgba(255, 255, 255, 0)");
        ctx.fillStyle = sheen;
        ctx.fillRect(0, 0, size, size);
    }

    // Track the pointer ALWAYS, not just while enabled: the lens then
    // appears at the true cursor position on activation.
    $effect(() => {
        window.addEventListener("pointermove", handleMove);
        return () => window.removeEventListener("pointermove", handleMove);
    });

    $effect(() => {
        if (!enabled) {
            if (raf) cancelAnimationFrame(raf);
            raf = 0;
            if (canvasEl) canvasEl.style.opacity = "0";
            if (chipEl) chipEl.style.opacity = "0";
            return;
        }
        // One-time usage hint per browsing session
        try {
            if (!sessionStorage.getItem(HINT_KEY)) {
                sessionStorage.setItem(HINT_KEY, "1");
                showHint = true;
                hintTimer = setTimeout(() => (showHint = false), 3200);
            }
        } catch {
            // Storage unavailable; skip the hint.
        }
        window.addEventListener("pointerdown", handlePointerDown, true);
        window.addEventListener("wheel", handleWheel, { passive: false });
        if (!raf) raf = requestAnimationFrame(draw);
        return () => {
            window.removeEventListener("pointerdown", handlePointerDown, true);
            window.removeEventListener("wheel", handleWheel);
            if (raf) cancelAnimationFrame(raf);
            raf = 0;
            if (hintTimer) clearTimeout(hintTimer);
            showHint = false;
        };
    });
</script>

<!-- Always mounted: tearing the canvas down on toggle would orphan its
     WebGL context and a reopened lens would render nothing. -->
<canvas bind:this={canvasEl} class="loupe" aria-hidden="true"></canvas>
<div bind:this={chipEl} class="loupe-zoom" aria-hidden="true">{zoom.toFixed(1)}×</div>
{#if enabled && showHint}
    <div class="loupe-hint" transition:fade={{ duration: 180 }}>
        Scroll to zoom · Click the picture to exit
    </div>
{/if}

<style>
    /* Borderless liquid lens: convexity from light, not from a stroke. */
    .loupe {
        position: fixed;
        top: 0;
        left: 0;
        width: 220px;
        height: 220px;
        border-radius: 50%;
        border: none;
        box-shadow:
            inset 0 1px 1px rgba(255, 255, 255, 0.22),
            0 18px 50px rgba(0, 0, 0, 0.55);
        pointer-events: none;
        z-index: 40;
        opacity: 0;
        background: transparent;
    }

    .loupe-zoom {
        position: fixed;
        top: 0;
        left: 0;
        z-index: 40;
        pointer-events: none;
        opacity: 0;
        font-size: 0.6875rem;
        font-weight: 600;
        font-variant-numeric: tabular-nums;
        color: rgba(255, 255, 255, 0.85);
        background: rgba(15, 15, 18, 0.7);
        backdrop-filter: blur(8px);
        -webkit-backdrop-filter: blur(8px);
        border: 1px solid rgba(255, 255, 255, 0.1);
        border-radius: var(--radius-full);
        padding: 2px 10px;
    }

    .loupe-hint {
        position: absolute;
        bottom: 150px;
        left: 50%;
        transform: translateX(-50%);
        z-index: 41;
        pointer-events: none;
        white-space: nowrap;
        font-size: 0.8125rem;
        color: var(--color-text);
        background: var(--glass-bg-deep);
        backdrop-filter: var(--glass-backdrop-deep);
        -webkit-backdrop-filter: var(--glass-backdrop-deep);
        border: 1px solid var(--glass-edge);
        border-radius: var(--radius-full);
        padding: 7px 16px;
        box-shadow: var(--glass-specular);
    }
</style>
