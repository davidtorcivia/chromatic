// Svelte actions that attach MediaStreams to <video> elements (bindStream)
// and to canvas webcam pills (bindCanvasStream), sharing one play-retry
// mechanism.
//
// Why the retry machinery exists: Firefox can leave a freshly-attached
// getUserMedia stream painting a black frame — a lone synchronous play() that
// rejects is never retried, so the tile sits black over its #000 background
// even though the track is live. Playback is therefore re-kicked on the
// element's readiness events, on a track flipping mute→unmute, AND when a
// device switch swaps a new track INTO the same MediaStream (identity
// unchanged, so the action's `update` never fires — caught via the stream's
// 'addtrack' and re-kicked + rebound).

/**
 * Wires the shared retry machinery onto a <video>: 'loadeddata' on the
 * element, 'unmute' on every video track, and 'addtrack' rebinding on the
 * stream. `onActivity` fires whenever playback should be (re)kicked.
 */
function watchStream(video: HTMLVideoElement, onActivity: () => void) {
    let watched: MediaStream | null = null;
    const trackListeners: MediaStreamTrack[] = [];

    const detachTracks = () => {
        for (const t of trackListeners) t.removeEventListener("unmute", onActivity);
        trackListeners.length = 0;
    };
    const attachTracks = (s: MediaStream) => {
        for (const t of s.getVideoTracks()) {
            t.addEventListener("unmute", onActivity);
            trackListeners.push(t);
        }
    };
    const onAddTrack = () => {
        if (!watched) return;
        detachTracks();
        attachTracks(watched);
        onActivity();
    };

    video.addEventListener("loadeddata", onActivity);

    return {
        /** Attach a stream (or null to detach) and rewire all listeners. */
        set(s: MediaStream | null) {
            detachTracks();
            if (watched) {
                watched.removeEventListener("addtrack", onAddTrack);
                watched = null;
            }
            video.srcObject = s;
            if (s) {
                watched = s;
                s.addEventListener("addtrack", onAddTrack);
                attachTracks(s);
                onActivity();
            }
        },
        destroy() {
            video.removeEventListener("loadeddata", onActivity);
            detachTracks();
            if (watched) watched.removeEventListener("addtrack", onAddTrack);
            watched = null;
            video.srcObject = null;
        },
    };
}

/** Svelte action: attach a MediaStream to a <video> and keep it in sync. */
export function bindStream(node: HTMLVideoElement, stream: MediaStream | null) {
    let current: MediaStream | null = null;
    const tryPlay = () => void node.play().catch(() => {});
    const watcher = watchStream(node, tryPlay);

    const apply = (s: MediaStream | null) => {
        if (current === s) return;
        current = s;
        watcher.set(s);
    };
    apply(stream);

    return {
        update: apply,
        destroy() {
            watcher.destroy();
            current = null;
        },
    };
}

export type CanvasStreamOptions = {
    stream: MediaStream | null;
    mirror?: boolean;
};

/**
 * Svelte action: render a MediaStream into a circular canvas pill via a
 * hidden <video>. Canvas instead of a visible <video> avoids Windows/browser
 * video-overlay paths that can ignore CSS clips.
 *
 * Draws on new video frames, not on every display frame: cam capture is
 * ~24fps while displays run 60–120Hz (ProMotion), so an unconditional rAF
 * loop would redraw identical frames 2–5x — per pill, for the whole session.
 * requestVideoFrameCallback fires only when the source produced a frame; the
 * rAF fallback (older Firefox) is gated to ~30fps. Canvas backing-store size
 * is cached via ResizeObserver — a getBoundingClientRect inside the per-frame
 * draw would force a synchronous layout on every frame of every pill.
 */
export function bindCanvasStream(node: HTMLCanvasElement, options: CanvasStreamOptions) {
    const video = document.createElement("video");
    video.muted = true;
    video.autoplay = true;
    video.playsInline = true;

    let current: MediaStream | null = null;
    let raf: number | null = null;
    let vfc: number | null = null;
    let lastFallbackDraw = 0;
    let mirror = options.mirror ?? false;
    let destroyed = false;
    const ctx = node.getContext("2d", { alpha: true });
    const hasVFC = typeof (video as any).requestVideoFrameCallback === "function";
    const FALLBACK_FRAME_MS = 1000 / 30;

    const clearCanvas = () => {
        if (!ctx) return;
        ctx.clearRect(0, 0, node.width, node.height);
    };

    const tryPlay = () => void video.play().catch(() => {});
    const onActivity = () => {
        tryPlay();
        scheduleDraw();
    };
    const watcher = watchStream(video, onActivity);

    const applyCanvasSize = (cssWidth: number, cssHeight: number) => {
        const dpr = Math.min(window.devicePixelRatio || 1, 2);
        const width = Math.max(1, Math.round((cssWidth || 64) * dpr));
        const height = Math.max(1, Math.round((cssHeight || 64) * dpr));
        if (node.width !== width) node.width = width;
        if (node.height !== height) node.height = height;
    };
    const initialRect = node.getBoundingClientRect();
    applyCanvasSize(initialRect.width, initialRect.height);
    const resizeObserver = new ResizeObserver((entries) => {
        for (const entry of entries) {
            applyCanvasSize(entry.contentRect.width, entry.contentRect.height);
        }
        scheduleDraw();
    });
    resizeObserver.observe(node);

    const drawFrame = () => {
        raf = null;
        vfc = null;
        if (destroyed || !current || !ctx) return;
        scheduleDraw();
        if (
            video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA ||
            !video.videoWidth ||
            !video.videoHeight
        ) {
            return;
        }

        const w = node.width;
        const h = node.height;
        if (!w || !h) return;

        // Cover-fit the (possibly non-square) camera frame into the circle.
        const canvasAspect = w / h;
        const videoAspect = video.videoWidth / video.videoHeight;
        let sx = 0;
        let sy = 0;
        let sw = video.videoWidth;
        let sh = video.videoHeight;
        if (videoAspect > canvasAspect) {
            sw = video.videoHeight * canvasAspect;
            sx = (video.videoWidth - sw) / 2;
        } else {
            sh = video.videoWidth / canvasAspect;
            sy = (video.videoHeight - sh) / 2;
        }

        ctx.clearRect(0, 0, w, h);
        ctx.save();
        ctx.beginPath();
        ctx.arc(w / 2, h / 2, Math.min(w, h) / 2, 0, Math.PI * 2);
        ctx.clip();
        if (mirror) {
            ctx.translate(w, 0);
            ctx.scale(-1, 1);
        }
        ctx.drawImage(video, sx, sy, sw, sh, 0, 0, w, h);
        ctx.restore();
    };

    const fallbackLoop = (ts: number) => {
        raf = null;
        if (destroyed || !current) return;
        if (ts - lastFallbackDraw < FALLBACK_FRAME_MS) {
            raf = requestAnimationFrame(fallbackLoop);
            return;
        }
        lastFallbackDraw = ts;
        drawFrame();
    };

    function scheduleDraw() {
        if (destroyed || !current) return;
        if (hasVFC) {
            if (vfc !== null) return;
            vfc = (video as any).requestVideoFrameCallback(drawFrame);
        } else {
            if (raf !== null) return;
            raf = requestAnimationFrame(fallbackLoop);
        }
    }

    const cancelDraws = () => {
        if (raf !== null) {
            cancelAnimationFrame(raf);
            raf = null;
        }
        if (vfc !== null) {
            (video as any).cancelVideoFrameCallback?.(vfc);
            vfc = null;
        }
    };

    const apply = (next: CanvasStreamOptions) => {
        mirror = next.mirror ?? false;
        const nextStream = next.stream;
        if (current === nextStream) {
            scheduleDraw();
            return;
        }

        cancelDraws();
        current = nextStream;
        watcher.set(nextStream);
        if (!nextStream) {
            clearCanvas();
            return;
        }
        scheduleDraw();
    };

    apply(options);

    return {
        update: apply,
        destroy() {
            destroyed = true;
            cancelDraws();
            resizeObserver.disconnect();
            watcher.destroy();
            video.pause();
            current = null;
            clearCanvas();
        },
    };
}
