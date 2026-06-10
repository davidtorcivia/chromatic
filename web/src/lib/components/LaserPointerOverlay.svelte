<script lang="ts">
    import { onMount } from "svelte";
    import { session } from "$lib/stores/session.svelte";
    import {
        clientToVideoCoords,
        getVideoContentRect,
    } from "$lib/video/coordinates";
    import {
        pruneTrail,
        trailAlpha,
        smoothingFactor,
        rippleAt,
        shouldAppendPoint,
        splitLongSegments,
        buildSplineSegments,
        trailStyle,
        TRAIL_FADE_MS,
        TRAIL_HEAD_WIDTH,
        TRAIL_GLOW_WIDTH_RATIO,
        TRAIL_GLOW_ALPHA,
        TRAIL_CORE_WIDTH_RATIO,
        TRAIL_CORE_ALPHA,
        TRAIL_CORE_MIN_POS,
        type TrailPoint,
    } from "$lib/video/laser";

    interface Cursor {
        participantId: string;
        participantName: string;
        color: string;
        // Smoothed (rendered) position, normalized 0-1 video coords
        x: number;
        y: number;
        // Latest network position we interpolate toward
        targetX: number;
        targetY: number;
        active: boolean;
        lastUpdate: number;
        trail: TrailPoint[];
    }

    interface Ripple {
        x: number;
        y: number;
        color: string;
        start: number;
    }

    interface Props {
        videoElement: HTMLVideoElement;
        enabled: boolean;
    }

    let { videoElement, enabled = false }: Props = $props();

    let isPointing = $state(false);
    let videoRect = $state({ x: 0, y: 0, width: 0, height: 0 });
    let activePointerId: number | null = null;
    let showUsageHint = $state(false);
    let hintTimer: ReturnType<typeof setTimeout> | null = null;

    // ~30Hz send rate while pointing
    const THROTTLE_MS = 33;
    let sendThrottle: ReturnType<typeof setTimeout> | null = null;
    let pendingSend: { x: number; y: number } | null = null;
    let lastSentPos: { x: number; y: number } | null = null;

    // Cursor/effect state intentionally lives outside Svelte reactivity:
    // everything is drawn imperatively on the canvas by a single RAF loop.
    const cursorMap = new Map<string, Cursor>();
    let ripples: Ripple[] = [];

    let canvasEl = $state<HTMLCanvasElement | null>(null);
    let ctx: CanvasRenderingContext2D | null = null;
    let rafId: number | null = null;
    let lastFrameTime = 0;
    let destroyed = false;
    let reducedMotion = false;

    function clearHintTimer() {
        if (hintTimer) {
            clearTimeout(hintTimer);
            hintTimer = null;
        }
    }

    function showHintForDuration() {
        clearHintTimer();
        showUsageHint = true;
        hintTimer = setTimeout(() => {
            showUsageHint = false;
            hintTimer = null;
        }, 5000);
    }

    function updateVideoRect() {
        if (videoElement) {
            videoRect = getVideoContentRect(videoElement);
        }
    }

    // Size the canvas backing store to the video content rect (DPR-aware)
    $effect(() => {
        const rect = videoRect;
        if (!canvasEl) return;
        const dpr = Math.min(window.devicePixelRatio || 1, 2);
        canvasEl.width = Math.max(1, Math.round(rect.width * dpr));
        canvasEl.height = Math.max(1, Math.round(rect.height * dpr));
        ctx = canvasEl.getContext("2d");
        startRenderLoop(); // repaint at least once after a resize
    });

    $effect(() => {
        videoElement.style.cursor = enabled ? "crosshair" : "";

        if (enabled) {
            showHintForDuration();
            return;
        }

        showUsageHint = false;
        clearHintTimer();
        if (isPointing) {
            activePointerId = null;
            isPointing = false;
            sendCursorEnd();
        }
    });

    onMount(() => {
        updateVideoRect();

        const motionQuery = window.matchMedia("(prefers-reduced-motion: reduce)");
        reducedMotion = motionQuery.matches;
        const handleMotionChange = (e: MediaQueryListEvent) => {
            reducedMotion = e.matches;
        };
        motionQuery.addEventListener("change", handleMotionChange);

        videoElement.addEventListener("loadedmetadata", updateVideoRect);
        window.addEventListener("resize", updateVideoRect);

        // Laser pointer only activates when explicitly enabled from the session controls.
        const handleVideoPointerDown = (e: PointerEvent) => {
            if (!enabled || e.pointerType === "touch" || e.button !== 0 || isPointing) return;
            showUsageHint = false;
            activePointerId = e.pointerId;
            isPointing = true;
            sendCursor(e);
        };

        const handleGlobalPointerMove = (e: PointerEvent) => {
            if (!isPointing || activePointerId !== e.pointerId) return;
            sendCursor(e);
        };

        const handleGlobalPointerUp = (e: PointerEvent) => {
            if (!isPointing || activePointerId !== e.pointerId) return;
            activePointerId = null;
            isPointing = false;
            sendCursorEnd();
        };

        const handleWindowBlur = () => {
            if (!isPointing) return;
            activePointerId = null;
            isPointing = false;
            sendCursorEnd();
        };

        videoElement.addEventListener("pointerdown", handleVideoPointerDown);
        window.addEventListener("pointermove", handleGlobalPointerMove);
        window.addEventListener("pointerup", handleGlobalPointerUp);
        window.addEventListener("pointercancel", handleGlobalPointerUp);
        window.addEventListener("blur", handleWindowBlur);

        // Subscribe to cursor updates from WebSocket (unsubscribed in cleanup
        // so handlers don't pile up across remounts)
        const unsubscribeCursor = session.onMessage("cursor", (payload) => {
            const data = payload as {
                participantId: string;
                participantName: string;
                color: string;
                x: number;
                y: number;
                active: boolean;
                release?: boolean;
            };
            const now = Date.now();
            // Color is server-authoritative (assigned at join, echoed in every
            // cursor message). Fall back to the room participant list, then a
            // palette default, so a missing field never paints an invalid color.
            const color =
                data.color ||
                session.state.room?.participants.find((p) => p.id === data.participantId)
                    ?.color ||
                "#e63946";
            let cursor = cursorMap.get(data.participantId);
            if (!cursor) {
                cursor = {
                    participantId: data.participantId,
                    participantName: data.participantName,
                    color,
                    x: data.x,
                    y: data.y,
                    targetX: data.x,
                    targetY: data.y,
                    active: data.active,
                    lastUpdate: now,
                    trail: [],
                };
                cursorMap.set(data.participantId, cursor);
            } else {
                // Snap when a new stroke starts so the streak doesn't whip
                // across the frame from the previous stroke's end point.
                if (!cursor.active && data.active) {
                    cursor.x = data.x;
                    cursor.y = data.y;
                    cursor.trail = [];
                }
                cursor.participantName = data.participantName;
                cursor.color = color;
                cursor.targetX = data.x;
                cursor.targetY = data.y;
                cursor.active = data.active;
                cursor.lastUpdate = now;
            }

            if (data.release && !reducedMotion) {
                ripples.push({ x: data.x, y: data.y, color: cursor.color, start: now });
            }
            startRenderLoop();
        });

        // Clean up stale cursors every 500ms (covers dropped "end" messages).
        // Trails fade within 2s, so anything idle for 3s has nothing left to draw.
        const cleanupInterval = setInterval(() => {
            const now = Date.now();
            for (const [id, cursor] of cursorMap) {
                if (now - cursor.lastUpdate > 3000) {
                    cursorMap.delete(id);
                }
            }
        }, 500);

        return () => {
            destroyed = true;
            motionQuery.removeEventListener("change", handleMotionChange);
            videoElement.removeEventListener("loadedmetadata", updateVideoRect);
            window.removeEventListener("resize", updateVideoRect);
            videoElement.removeEventListener("pointerdown", handleVideoPointerDown);
            window.removeEventListener("pointermove", handleGlobalPointerMove);
            window.removeEventListener("pointerup", handleGlobalPointerUp);
            window.removeEventListener("pointercancel", handleGlobalPointerUp);
            window.removeEventListener("blur", handleWindowBlur);
            clearHintTimer();
            if (sendThrottle) {
                clearTimeout(sendThrottle);
                sendThrottle = null;
            }
            if (rafId !== null) {
                cancelAnimationFrame(rafId);
                rafId = null;
            }
            clearInterval(cleanupInterval);
            unsubscribeCursor();
        };
    });

    // --- Sending ----------------------------------------------------------

    function sendCursor(e: PointerEvent) {
        const coords = clientToVideoCoords(e.clientX, e.clientY, videoElement);
        lastSentPos = { x: coords.x, y: coords.y };

        if (sendThrottle) {
            // Trailing edge: remember the latest position so the throttle
            // flush sends it instead of dropping it.
            pendingSend = { x: coords.x, y: coords.y };
            return;
        }
        transmitCursor(coords.x, coords.y);
    }

    function transmitCursor(x: number, y: number) {
        session.send("cursor", { x, y, active: true });
        sendThrottle = setTimeout(() => {
            sendThrottle = null;
            if (pendingSend && isPointing) {
                const p = pendingSend;
                pendingSend = null;
                transmitCursor(p.x, p.y);
            } else {
                pendingSend = null;
            }
        }, THROTTLE_MS);
    }

    function sendCursorEnd() {
        pendingSend = null;
        if (sendThrottle) {
            clearTimeout(sendThrottle);
            sendThrottle = null;
        }
        const pos = lastSentPos ?? { x: 0, y: 0 };
        // `release: true` tells every client to play the expanding ripple
        // at the final cursor position.
        session.send("cursor", {
            x: pos.x,
            y: pos.y,
            active: false,
            release: lastSentPos !== null,
        });
        lastSentPos = null;
    }

    // --- Rendering (single RAF loop, idles when there is nothing to draw) --

    function startRenderLoop() {
        if (rafId !== null || destroyed) return;
        lastFrameTime = performance.now();
        rafId = requestAnimationFrame(frame);
    }

    function frame(now: number) {
        rafId = null;
        if (destroyed) return;
        const dt = Math.min(now - lastFrameTime, 100);
        lastFrameTime = now;
        if (renderFrame(dt)) {
            rafId = requestAnimationFrame(frame);
        }
        // Otherwise the loop idles (no CPU burn on static content);
        // incoming cursor messages or a resize restart it.
    }

    /** Draws one frame. Returns true if another frame is needed. */
    function renderFrame(dt: number): boolean {
        if (!ctx || !canvasEl) return false;

        const w = videoRect.width;
        const h = videoRect.height;
        const dpr = canvasEl.width / Math.max(1, w);
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
        ctx.clearRect(0, 0, w, h);

        if (w <= 0 || h <= 0) return false;

        const now = Date.now();
        let hasWork = false;

        for (const cursor of cursorMap.values()) {
            if (reducedMotion) {
                // Plain dots only: snap to target, no trails or glow
                cursor.x = cursor.targetX;
                cursor.y = cursor.targetY;
                cursor.trail.length = 0;
                if (cursor.active) {
                    drawCursorDot(ctx, cursor, w, h, true);
                    hasWork = true;
                }
                continue;
            }

            // Interpolate toward the network target so remote cursors glide
            // at display refresh rate even with ~30Hz input.
            const dx = cursor.targetX - cursor.x;
            const dy = cursor.targetY - cursor.y;
            if (dx * dx + dy * dy > 1e-8) {
                const f = smoothingFactor(dt);
                cursor.x += dx * f;
                cursor.y += dy * f;
                hasWork = true;
            } else {
                cursor.x = cursor.targetX;
                cursor.y = cursor.targetY;
            }

            // Record the smoothed position into the streak while pointing.
            // Points closer than ~2px to the previous one are skipped — the
            // smoothed curve doesn't need them, and dense sub-pixel points
            // are what made the additive trail blow out and look jittery.
            if (cursor.active) {
                const last = cursor.trail[cursor.trail.length - 1];
                if (shouldAppendPoint(last, cursor.x, cursor.y, w, h)) {
                    cursor.trail.push({ x: cursor.x, y: cursor.y, t: now });
                }
            }

            pruneTrail(cursor.trail, now);

            if (cursor.trail.length > 0) {
                drawTrail(ctx, cursor, w, h, now);
                hasWork = true;
            }

            if (cursor.active) {
                drawCursorDot(ctx, cursor, w, h, false);
                hasWork = true;
            }
        }

        // Release ripples (multiple may coexist). Same vivid treatment as
        // the trail: full-alpha colored ring with a colored glow and a thin
        // white hot edge. A ring stroke never overlaps itself, so 'lighter'
        // is safe here at full alpha.
        if (ripples.length > 0) {
            const remaining: Ripple[] = [];
            for (const ripple of ripples) {
                const v = rippleAt(now - ripple.start);
                if (v.done) continue;
                ctx.save();
                ctx.globalCompositeOperation = "lighter";
                ctx.beginPath();
                ctx.arc(ripple.x * w, ripple.y * h, v.radius, 0, Math.PI * 2);
                // Faint colored fill for a soft "ping"
                ctx.globalAlpha = v.alpha * 0.12;
                ctx.fillStyle = ripple.color;
                ctx.fill();
                // Main colored ring with glow
                ctx.globalAlpha = v.alpha;
                ctx.strokeStyle = ripple.color;
                ctx.lineWidth = 2.5;
                ctx.shadowColor = ripple.color;
                ctx.shadowBlur = 10;
                ctx.stroke();
                ctx.shadowBlur = 0;
                // Thin white hot edge
                ctx.globalAlpha = v.alpha * 0.35;
                ctx.strokeStyle = "#fff";
                ctx.lineWidth = 1;
                ctx.stroke();
                ctx.restore();
                remaining.push(ripple);
            }
            ripples = remaining;
            if (ripples.length > 0) hasWork = true;
        }

        return hasWork;
    }

    // Trail rendering: three passes over one centripetal Catmull-Rom
    // spline that interpolates every recorded point (G1-continuous, so
    // no visible angles at joints) and ends exactly at the live cursor.
    //
    //   1. GLOW  — 'lighter', ONE stroke of the whole spline at
    //      TRAIL_GLOW_WIDTH_RATIO x head width, TRAIL_GLOW_ALPHA, with a
    //      colored shadow blur. A single stroke never overlaps itself,
    //      so additive compositing cannot white-clip here.
    //   2. BODY  — 'source-over', per-segment strokes tapering in width
    //      and alpha (trailStyle) from ~TRAIL_BODY_MAX_ALPHA at the head
    //      to the tail. Normal alpha compositing means the overlapping
    //      round caps between adjacent segments only saturate the
    //      participant color — full vivid alpha without clipping.
    //   3. CORE  — 'lighter', ONE thin near-white stroke over just the
    //      head portion (pos >= TRAIL_CORE_MIN_POS) for the hot-laser
    //      look; fades with the head's life after release.
    function drawTrail(
        c: CanvasRenderingContext2D,
        cursor: Cursor,
        w: number,
        h: number,
        now: number
    ) {
        // Recorded points are thinned to >=2px spacing, so while pointing we
        // append a virtual head at the live (smoothed) position. The spline
        // passes through it with a continuous tangent, so the streak meets
        // the cursor dot without a kink.
        let points: TrailPoint[] = cursor.trail;
        if (cursor.active) {
            const last = points[points.length - 1];
            if (!last || last.x !== cursor.x || last.y !== cursor.y) {
                points = [...points, { x: cursor.x, y: cursor.y, t: now }];
            }
        }
        if (points.length < 2) return;

        // Fast flicks leave sparse 30Hz samples; subdivide long chords so
        // the taper and age fade stay smooth along them.
        const segs = buildSplineSegments(splitLongSegments(points, w, h));
        if (segs.length === 0) return;
        const headLife = trailAlpha(now - points[points.length - 1].t, TRAIL_FADE_MS);

        c.save();
        c.lineCap = "round";
        c.lineJoin = "round";

        // Pass 1: outer glow.
        c.globalCompositeOperation = "lighter";
        c.strokeStyle = cursor.color;
        c.globalAlpha = TRAIL_GLOW_ALPHA * headLife;
        c.lineWidth = TRAIL_HEAD_WIDTH * TRAIL_GLOW_WIDTH_RATIO;
        c.shadowColor = cursor.color;
        c.shadowBlur = 8;
        c.beginPath();
        c.moveTo(segs[0].x0 * w, segs[0].y0 * h);
        for (const s of segs) {
            c.bezierCurveTo(s.c1x * w, s.c1y * h, s.c2x * w, s.c2y * h, s.x1 * w, s.y1 * h);
        }
        c.stroke();
        c.shadowBlur = 0;

        // Pass 2: vivid body, oldest -> newest so the head draws on top.
        c.globalCompositeOperation = "source-over";
        for (const s of segs) {
            const life = trailAlpha(now - s.t, TRAIL_FADE_MS);
            const { width, alpha } = trailStyle(s.pos, life);
            if (alpha <= 0.004 || width <= 0.05) continue;
            c.globalAlpha = alpha;
            c.lineWidth = width;
            c.beginPath();
            c.moveTo(s.x0 * w, s.y0 * h);
            c.bezierCurveTo(s.c1x * w, s.c1y * h, s.c2x * w, s.c2y * h, s.x1 * w, s.y1 * h);
            c.stroke();
        }

        // Pass 3: hot white core at the head only.
        const coreAlpha = TRAIL_CORE_ALPHA * headLife;
        if (coreAlpha > 0.01) {
            c.globalCompositeOperation = "lighter";
            c.strokeStyle = "#fff";
            c.globalAlpha = coreAlpha;
            c.lineWidth = TRAIL_HEAD_WIDTH * TRAIL_CORE_WIDTH_RATIO;
            let started = false;
            c.beginPath();
            for (const s of segs) {
                if (s.pos < TRAIL_CORE_MIN_POS) continue;
                if (!started) {
                    c.moveTo(s.x0 * w, s.y0 * h);
                    started = true;
                }
                c.bezierCurveTo(s.c1x * w, s.c1y * h, s.c2x * w, s.c2y * h, s.x1 * w, s.y1 * h);
            }
            if (started) c.stroke();
        }
        c.restore();
    }

    function drawCursorDot(
        c: CanvasRenderingContext2D,
        cursor: Cursor,
        w: number,
        h: number,
        plain: boolean
    ) {
        const px = cursor.x * w;
        const py = cursor.y * h;

        c.save();
        c.fillStyle = cursor.color;
        c.beginPath();
        c.arc(px, py, 5.5, 0, Math.PI * 2);
        if (plain) {
            // Reduced motion: flat dot, no glow
            c.fill();
        } else {
            // Colored glow, filled twice so the halo reads on dark footage
            c.shadowColor = cursor.color;
            c.shadowBlur = 12;
            c.fill();
            c.fill();
            c.shadowBlur = 0;
        }
        // Subtle white-hot center for the classic laser look
        c.fillStyle = "rgba(255, 255, 255, 0.92)";
        c.beginPath();
        c.arc(px, py, 2.2, 0, Math.PI * 2);
        c.fill();
        c.restore();

        // Name label below the dot
        const label = cursor.participantName;
        if (!label) return;
        c.save();
        c.font = "600 10px system-ui, -apple-system, sans-serif";
        c.textBaseline = "middle";
        const padX = 6;
        const textW = c.measureText(label).width;
        const boxW = textW + padX * 2;
        const boxH = 16;
        const boxX = px - boxW / 2;
        const boxY = py + 11;
        c.fillStyle = cursor.color;
        c.beginPath();
        if (typeof c.roundRect === "function") {
            c.roundRect(boxX, boxY, boxW, boxH, 4);
        } else {
            c.rect(boxX, boxY, boxW, boxH);
        }
        c.fill();
        c.fillStyle = "#fff";
        c.fillText(label, boxX + padX, boxY + boxH / 2 + 0.5);
        c.restore();
    }
</script>

<!-- Overlay positioned to match video content area -->
<div
    class="laser-overlay"
    style="
    left: {videoRect.x}px;
    top: {videoRect.y}px;
    width: {videoRect.width}px;
    height: {videoRect.height}px;
  "
>
    {#if showUsageHint}
        <div class="laser-hint">Laser enabled: click and drag on video to point</div>
    {/if}
    <canvas bind:this={canvasEl} class="laser-canvas"></canvas>
</div>

<style>
    .laser-overlay {
        position: absolute;
        cursor: default;
        pointer-events: none;
        z-index: 5;
        /* Force own compositing layer so the overlay renders above
           the video element's hardware compositor in all browsers */
        transform: translateZ(0);
    }

    .laser-canvas {
        position: absolute;
        inset: 0;
        width: 100%;
        height: 100%;
        pointer-events: none;
    }

    .laser-hint {
        position: absolute;
        top: 12px;
        left: 12px;
        background: rgba(0, 0, 0, 0.72);
        color: rgba(255, 255, 255, 0.92);
        border: 1px solid rgba(255, 255, 255, 0.22);
        border-radius: 999px;
        padding: 6px 10px;
        font-size: 0.72rem;
        font-weight: 500;
        letter-spacing: 0.01em;
        pointer-events: none;
        white-space: nowrap;
        z-index: 1;
    }
</style>
