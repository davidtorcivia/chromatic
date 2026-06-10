<script lang="ts">
    import { onMount } from "svelte";
    import { session } from "$lib/stores/session.svelte";
    import {
        clientToVideoCoords,
        getVideoContentRect,
    } from "$lib/video/coordinates";
    import {
        midpointSlice,
        flattenSlice,
        fadeForAge,
        bucketExpired,
        pruneBuckets,
        smoothingFactor,
        rippleAt,
        subsampleBatch,
        TRAIL_BODY_WIDTH,
        TRAIL_BODY_ALPHA,
        TRAIL_GLOW_WIDTH_RATIO,
        TRAIL_GLOW_ALPHA,
        MIN_STAMP_DIST_PX,
        CURSOR_SEND_INTERVAL_MS,
        type BatchPoint,
        type QuadSlice,
    } from "$lib/video/laser";

    interface Cursor {
        participantId: string;
        participantName: string;
        color: string;
        // Rendered position, normalized 0-1 video coords. For remote
        // cursors this is smoothed toward (targetX, targetY); for the
        // local cursor it snaps to the target (the true pointer position).
        x: number;
        y: number;
        // Latest known position: live pointer for local, newest batched
        // network point for remote.
        targetX: number;
        targetY: number;
        active: boolean;
        lastUpdate: number;
        // The last two stamped trail points (normalized coords) of the
        // current stroke. Each new point stamps one midpoint-quadratic
        // slice through prev1 into the cursor's current age bucket.
        // These anchors persist across bucket rotation, so the chained
        // curve continues seamlessly across bucket boundaries (bucket
        // N+1's first slice starts exactly where bucket N's last ended).
        prev1: BatchPoint | null;
        prev2: BatchPoint | null;
        // The bucket currently receiving this cursor's slices, or null
        // between strokes. Rotated (closed, new one opened) when it is
        // older than TRAIL_BUCKET_MS; the closed bucket then lives only
        // in trailBuckets, immutable, until its fade ends.
        bucket: TrailBucket | null;
        // True for the sender's own cursor: driven directly by local
        // pointer events with zero network latency; server echoes for
        // this participantId are ignored.
        local: boolean;
    }

    interface Ripple {
        x: number;
        y: number;
        color: string;
        start: number;
    }

    // One age bucket of trail geometry. Slices stamped within a
    // TRAIL_BUCKET_MS window accumulate into the Path2D (canvas-px
    // coordinates, built once, never rebuilt); after the window closes
    // the bucket is immutable. Each frame the bucket is stroked twice
    // (glow + body — the glow is just a wider stroke of the same path)
    // at an alpha derived purely from its age, and it is dropped
    // entirely once fadeForAge reaches 0.
    interface TrailBucket {
        openedAt: number; // performance.now() of the first slice
        color: string;
        path: Path2D;
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

    // Network batching: coalesced pointer samples accumulate here and are
    // flushed as one {points: [...]} message per ~30Hz send tick.
    let sendTimer: ReturnType<typeof setTimeout> | null = null;
    let pendingPoints: BatchPoint[] = [];
    let lastQueuedPos: BatchPoint | null = null;

    // Cursor/effect state intentionally lives outside Svelte reactivity:
    // everything is drawn imperatively on the canvases by a single RAF loop.
    const cursorMap = new Map<string, Cursor>();
    let ripples: Ripple[] = [];

    // Two stacked canvases:
    //  - trailCanvas (below): cleared and redrawn each frame from the
    //    live age buckets — at most ~ceil(TRAIL_FADE_MS/TRAIL_BUCKET_MS)
    //    cached Path2D strokes (x2 passes) per cursor, a constant cost
    //    no matter how long or fast the user draws. No destination-out
    //    anywhere: each bucket's alpha comes from its age, reaches
    //    exactly 0, and the bucket is dropped — no quantization stall,
    //    no residue ghost.
    //  - cursorCanvas (above): cleared and redrawn each frame with the
    //    cursor dots, name labels, and release ripples (tiny constant cost).
    let trailCanvas = $state<HTMLCanvasElement | null>(null);
    let cursorCanvas = $state<HTMLCanvasElement | null>(null);
    let trailCtx: CanvasRenderingContext2D | null = null;
    let cursorCtx: CanvasRenderingContext2D | null = null;

    // All live trail buckets across every cursor (each carries its own
    // color), oldest first so newer strokes paint on top. A bucket
    // enters when a cursor opens it, becomes immutable when the cursor
    // rotates past it, and leaves via pruneBuckets when fully faded.
    const trailBuckets: TrailBucket[] = [];
    // True while the trail canvas may hold pixels, so the render loop
    // knows to clear it exactly once after the last bucket fades, then
    // park (idle = zero work).
    let trailDirty = false;

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

    // Size both canvas backing stores to the video content rect
    // (DPR-aware). Setting width/height wipes canvas content, and the
    // bucket paths are in canvas space so they would be misplaced at the
    // new size — drop them all; a momentary trail loss on resize is fine
    // (cursors keep stamping fresh slices into new buckets).
    $effect(() => {
        const rect = videoRect;
        if (!trailCanvas || !cursorCanvas) return;
        const dpr = Math.min(window.devicePixelRatio || 1, 2);
        const bw = Math.max(1, Math.round(rect.width * dpr));
        const bh = Math.max(1, Math.round(rect.height * dpr));
        trailCanvas.width = bw;
        trailCanvas.height = bh;
        cursorCanvas.width = bw;
        cursorCanvas.height = bh;
        trailCtx = trailCanvas.getContext("2d");
        cursorCtx = cursorCanvas.getContext("2d");
        dropAllBuckets(); // canvas-space geometry is invalid at the new size
        trailDirty = false; // resize wiped the layer
        startRenderLoop(); // repaint at least once after a resize
    });

    /** Forget every bucket (resize): open ones too, so no cursor keeps
     *  stamping into a path that is no longer drawn. */
    function dropAllBuckets() {
        trailBuckets.length = 0;
        for (const cursor of cursorMap.values()) {
            cursor.bucket = null;
        }
    }

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
            endLocalStroke();
            sendCursorEnd();
        }
        // No trail cleanup needed: the live buckets fade out on their own.
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
            beginLocalStroke(e);
            sendCursor(e);
        };

        const handleGlobalPointerMove = (e: PointerEvent) => {
            if (!isPointing || activePointerId !== e.pointerId) return;
            extendLocalStroke(e);
            sendCursor(e);
        };

        const handleGlobalPointerUp = (e: PointerEvent) => {
            if (!isPointing || activePointerId !== e.pointerId) return;
            activePointerId = null;
            isPointing = false;
            endLocalStroke();
            sendCursorEnd();
        };

        const handleWindowBlur = () => {
            if (!isPointing) return;
            activePointerId = null;
            isPointing = false;
            endLocalStroke();
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
                // Batched format: dense coalesced samples for this send tick.
                points?: { x: number; y: number }[];
                // Legacy single-point format (also the last batch point).
                x: number;
                y: number;
                active: boolean;
                release?: boolean;
            };
            // The local cursor is driven directly by pointer events with
            // zero latency; ignore the server echo of our own messages.
            if (data.participantId === session.state.participantId) return;

            // Normalize to a batch: prefer the dense points array, fall
            // back to the legacy single {x,y}. Drop non-finite samples.
            let batch: BatchPoint[] = Array.isArray(data.points)
                ? data.points.filter(
                      (p) => Number.isFinite(p?.x) && Number.isFinite(p?.y)
                  )
                : [];
            if (batch.length === 0) {
                if (!Number.isFinite(data.x) || !Number.isFinite(data.y)) return;
                batch = [{ x: data.x, y: data.y }];
            }
            const head = batch[batch.length - 1];

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
            const wasActive = cursor?.active ?? false;
            if (!cursor) {
                cursor = {
                    participantId: data.participantId,
                    participantName: data.participantName,
                    color,
                    x: head.x,
                    y: head.y,
                    targetX: head.x,
                    targetY: head.y,
                    active: data.active,
                    lastUpdate: now,
                    prev1: null,
                    prev2: null,
                    bucket: null,
                    local: false,
                };
                cursorMap.set(data.participantId, cursor);
            } else if (!wasActive && data.active) {
                // Snap when a new stroke starts so the dot doesn't whip
                // across the frame from the previous stroke's end point,
                // and reset the stamp anchors so the new trail doesn't
                // connect to the old one.
                cursor.x = batch[0].x;
                cursor.y = batch[0].y;
                cursor.prev1 = null;
                cursor.prev2 = null;
                cursor.bucket = null; // new stroke, new bucket
            }

            cursor.participantName = data.participantName;
            cursor.color = color;

            // Stamp the received points onto the trail layer on arrival —
            // the actual network geometry, in batch order. The final
            // (active: false) message's points are stamped too, then the
            // stroke is capped so the trail ends exactly at the release
            // point.
            if (!reducedMotion && (data.active || wasActive)) {
                for (const p of batch) {
                    stampCursorPoint(cursor, p.x, p.y);
                }
                if (!data.active) finishStroke(cursor);
            }

            // The dot smooths toward the newest point; the batch path is
            // already stamped on the trail layer behind it.
            cursor.targetX = head.x;
            cursor.targetY = head.y;
            cursor.active = data.active;
            cursor.lastUpdate = now;

            if (data.release && !reducedMotion) {
                ripples.push({ x: head.x, y: head.y, color: cursor.color, start: now });
            }
            startRenderLoop();
        });

        // Clean up stale cursors every 500ms (covers dropped "end" messages).
        // The dot disappears; the trail needs no cleanup — it just fades.
        const cleanupInterval = setInterval(() => {
            const now = Date.now();
            for (const [id, cursor] of cursorMap) {
                // Never expire our own cursor mid-stroke (holding the
                // pointer still produces no updates).
                if (cursor.local && isPointing) continue;
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
            if (sendTimer) {
                clearTimeout(sendTimer);
                sendTimer = null;
            }
            if (rafId !== null) {
                cancelAnimationFrame(rafId);
                rafId = null;
            }
            clearInterval(cleanupInterval);
            unsubscribeCursor();
        };
    });

    // --- Coalesced sampling -------------------------------------------------

    // Browsers coalesce pointer events: one pointermove can carry many true
    // samples. Capture them all so fast flicks keep their real geometry.
    function coalescedCoords(e: PointerEvent): BatchPoint[] {
        const events =
            typeof e.getCoalescedEvents === "function" && e.getCoalescedEvents().length > 0
                ? e.getCoalescedEvents()
                : [e];
        const out: BatchPoint[] = [];
        for (const ce of events) {
            const c = clientToVideoCoords(ce.clientX, ce.clientY, videoElement);
            out.push({ x: c.x, y: c.y });
        }
        return out;
    }

    // --- Trail stamping -------------------------------------------------------
    // Each new point (local coalesced sample or remote batch point) stamps
    // ONE midpoint-quadratic slice — from mid(prev2, prev1) through prev1
    // to mid(prev1, new) — into the cursor's CURRENT age bucket's Path2D.
    // Adjacent slices share endpoints and tangents (including across
    // bucket boundaries: the prev1/prev2 anchors persist through
    // rotation), so the chained curve is G1-continuous at any speed,
    // each slice's geometry is built exactly once, and there is no point
    // list to prune, thin, or decimate. Rendering happens in renderFrame
    // by stroking the cached bucket paths at their age-derived alpha.

    function trailDpr(): number {
        return trailCanvas ? trailCanvas.width / Math.max(1, videoRect.width) : 1;
    }

    /** The bucket currently accepting slices for this cursor, opening a
     *  fresh one when there is none or the current one's window closed
     *  (it then becomes immutable and just fades out). */
    function openBucket(cursor: Cursor, now: number): TrailBucket {
        let bucket = cursor.bucket;
        if (!bucket || bucketExpired(bucket.openedAt, now) || bucket.color !== cursor.color) {
            bucket = { openedAt: now, color: cursor.color, path: new Path2D() };
            trailBuckets.push(bucket);
            cursor.bucket = bucket;
        }
        return bucket;
    }

    /** Append one slice (canvas px coords) to a bucket's cached path. */
    function addSliceToBucket(bucket: TrailBucket, s: QuadSlice) {
        const path = bucket.path;
        // Each slice is its own subpath; consecutive slices share their
        // boundary point exactly, and the round caps/joins close the
        // seam, so the stroked result reads as one continuous curve.
        path.moveTo(s.x0, s.y0);
        // Slices whose chord exceeds SLICE_MAX_CHORD_PX (fast flick) are
        // subdivided by sampling the quadratic so no stamp reads as a
        // single long rod.
        const subdivided = flattenSlice(s);
        if (subdivided) {
            for (const p of subdivided) path.lineTo(p.x, p.y);
        } else {
            path.quadraticCurveTo(s.cx, s.cy, s.x1, s.y1);
        }
    }

    /**
     * Feed one new trail point (normalized coords) for a cursor. The
     * first point of a stroke only seeds the anchors; every subsequent
     * point at least MIN_STAMP_DIST_PX away stamps one slice into the
     * cursor's current bucket synchronously — zero latency for local
     * samples, on-arrival for remote batches.
     */
    function stampCursorPoint(cursor: Cursor, x: number, y: number) {
        const w = videoRect.width;
        const h = videoRect.height;
        if (w <= 0 || h <= 0) return;

        const p1 = cursor.prev1;
        if (!p1) {
            cursor.prev1 = { x, y };
            return;
        }
        // Skip sub-pixel jitter so round caps don't pile up on one spot.
        const dx = (x - p1.x) * w;
        const dy = (y - p1.y) * h;
        if (dx * dx + dy * dy < MIN_STAMP_DIST_PX * MIN_STAMP_DIST_PX) return;

        const p2 = cursor.prev2 ?? p1; // first slice starts at prev1 itself
        addSliceToBucket(
            openBucket(cursor, performance.now()),
            midpointSlice(
                { x: p2.x * w, y: p2.y * h },
                { x: p1.x * w, y: p1.y * h },
                { x: x * w, y: y * h }
            )
        );
        cursor.prev2 = p1;
        cursor.prev1 = { x, y };
        startRenderLoop();
    }

    /**
     * Cap a finished stroke: the last stamped slice ended at
     * mid(prev2, prev1), so stamp the remaining half-segment up to the
     * final point (midpointSlice with next === prev1 ends exactly
     * there), then reset the anchors and close the bucket — it becomes
     * immutable and simply fades out with age.
     */
    function finishStroke(cursor: Cursor) {
        const w = videoRect.width;
        const h = videoRect.height;
        const p1 = cursor.prev1;
        const p2 = cursor.prev2;
        if (p1 && p2 && w > 0 && h > 0) {
            addSliceToBucket(
                openBucket(cursor, performance.now()),
                midpointSlice(
                    { x: p2.x * w, y: p2.y * h },
                    { x: p1.x * w, y: p1.y * h },
                    { x: p1.x * w, y: p1.y * h }
                )
            );
        }
        cursor.prev1 = null;
        cursor.prev2 = null;
        cursor.bucket = null;
    }

    // --- Local echo ---------------------------------------------------------
    // The sender's own laser renders with zero network latency: pointer
    // events drive the local cursor/trail/ripple directly through the same
    // cursorMap the remote rendering uses, and the server echo for our own
    // participantId is ignored.

    function localCursor(): Cursor | null {
        const id = session.state.participantId;
        if (!id) return null;
        let cursor = cursorMap.get(id);
        if (cursor) return cursor;
        const me = session.state.room?.participants.find((p) => p.id === id);
        cursor = {
            participantId: id,
            participantName: me?.name ?? "",
            color: me?.color ?? "#e63946",
            x: 0,
            y: 0,
            targetX: 0,
            targetY: 0,
            active: false,
            lastUpdate: Date.now(),
            prev1: null,
            prev2: null,
            bucket: null,
            local: true,
        };
        cursorMap.set(id, cursor);
        return cursor;
    }

    function appendLocalBatch(cursor: Cursor, batch: BatchPoint[]) {
        // Stamp every coalesced sample synchronously on capture (zero
        // latency, full input geometry).
        if (!reducedMotion) {
            for (const p of batch) {
                stampCursorPoint(cursor, p.x, p.y);
            }
        }
        const head = batch[batch.length - 1];
        // True pointer position every frame: no smoothing for the local
        // cursor (renderFrame snaps x/y to target for local cursors).
        cursor.targetX = head.x;
        cursor.targetY = head.y;
        cursor.x = head.x;
        cursor.y = head.y;
        cursor.lastUpdate = Date.now();
    }

    function beginLocalStroke(e: PointerEvent) {
        const cursor = localCursor();
        if (!cursor) return;
        cursor.prev1 = null;
        cursor.prev2 = null;
        cursor.active = true;
        appendLocalBatch(cursor, coalescedCoords(e));
        startRenderLoop();
    }

    function extendLocalStroke(e: PointerEvent) {
        const cursor = localCursor();
        if (!cursor) return;
        appendLocalBatch(cursor, coalescedCoords(e));
        startRenderLoop();
    }

    function endLocalStroke() {
        const id = session.state.participantId;
        const cursor = id ? cursorMap.get(id) : undefined;
        if (!cursor || !cursor.active) return;
        cursor.active = false;
        cursor.lastUpdate = Date.now();
        if (!reducedMotion) {
            finishStroke(cursor);
            ripples.push({
                x: cursor.targetX,
                y: cursor.targetY,
                color: cursor.color,
                start: Date.now(),
            });
        }
        startRenderLoop();
    }

    // --- Sending ------------------------------------------------------------
    // Coalesced samples accumulate in pendingPoints; one batched message
    // {points: [...], active} goes out per ~30Hz tick (first sample of a
    // stroke flushes immediately so remote strokes start without delay).

    function sendCursor(e: PointerEvent) {
        const batch = coalescedCoords(e);
        pendingPoints.push(...batch);
        lastQueuedPos = batch[batch.length - 1];
        if (!sendTimer) flushSend();
    }

    function flushSend() {
        if (pendingPoints.length > 0 && isPointing) {
            // Cap the batch (evenly subsampled, endpoints kept) so a
            // high-rate input device cannot inflate message size.
            const points = subsampleBatch(pendingPoints);
            pendingPoints = [];
            session.send("cursor", { points, active: true });
        }
        sendTimer = setTimeout(() => {
            sendTimer = null;
            if (pendingPoints.length > 0 && isPointing) flushSend();
        }, CURSOR_SEND_INTERVAL_MS);
    }

    function sendCursorEnd() {
        if (sendTimer) {
            clearTimeout(sendTimer);
            sendTimer = null;
        }
        // Flush any samples still pending plus the final position, and ask
        // every client to play the expanding ripple there (`release`).
        if (pendingPoints.length === 0) {
            pendingPoints.push(lastQueuedPos ?? { x: 0, y: 0 });
        }
        const points = subsampleBatch(pendingPoints);
        pendingPoints = [];
        session.send("cursor", {
            points,
            active: false,
            release: lastQueuedPos !== null,
        });
        lastQueuedPos = null;
    }

    // --- Rendering (single RAF loop, idles when there is nothing to draw) --
    // Per-frame work is small and CONSTANT: clear the trail layer and
    // stroke each live bucket's cached Path2D twice (glow + body) at its
    // age-derived alpha — bounded at ~ceil(TRAIL_FADE_MS/TRAIL_BUCKET_MS)
    // buckets per cursor — plus a full redraw of the tiny cursor layer.
    // Bucket geometry is never rebuilt here. The loop parks when there
    // are no cursors/ripples AND no live buckets (after one final clear).

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
        if (renderFrame(dt, now)) {
            rafId = requestAnimationFrame(frame);
        }
        // Otherwise the loop idles (no CPU burn on static content);
        // pointer input, incoming cursor messages, or a resize restart it.
    }

    /** Draws one frame. Returns true if another frame is needed. */
    function renderFrame(dt: number, frameNow: number): boolean {
        if (!cursorCtx || !cursorCanvas) return false;

        const w = videoRect.width;
        const h = videoRect.height;
        const dpr = cursorCanvas.width / Math.max(1, w);
        cursorCtx.setTransform(dpr, 0, 0, dpr, 0, 0);
        cursorCtx.clearRect(0, 0, w, h);

        if (w <= 0 || h <= 0) return false;

        // Trail layer: clear, drop fully-faded buckets, then re-stroke
        // each surviving bucket's cached path at its age-derived alpha
        // (glow pass then body pass). Alpha comes from wall-clock age, so
        // the fade duration is exact at any refresh rate and reaches
        // exactly 0 — the final clear leaves zero residue, ever.
        pruneBuckets(trailBuckets, frameNow);
        if (trailCtx && (trailDirty || trailBuckets.length > 0)) {
            const tc = trailCtx;
            tc.setTransform(trailDpr(), 0, 0, trailDpr(), 0, 0);
            tc.clearRect(0, 0, w, h);
            tc.lineCap = "round";
            tc.lineJoin = "round";
            for (const bucket of trailBuckets) {
                const fade = fadeForAge(frameNow - bucket.openedAt);
                if (fade <= 0) continue;
                tc.strokeStyle = bucket.color;
                // Glow pass: wide, faint, additive. The glow is the pure
                // participant color at low alpha, so same-color overlap
                // (crossings, spun circles) saturates toward the vivid
                // hue — additive compositing cannot white-clip a
                // single-hue source.
                tc.globalCompositeOperation = "lighter";
                tc.globalAlpha = TRAIL_GLOW_ALPHA * fade;
                tc.lineWidth = TRAIL_BODY_WIDTH * TRAIL_GLOW_WIDTH_RATIO;
                tc.stroke(bucket.path);
                // Body pass: normal width, high alpha, source-over —
                // crossings (including other participants' trails)
                // repaint cleanly instead of accumulating.
                tc.globalCompositeOperation = "source-over";
                tc.globalAlpha = TRAIL_BODY_ALPHA * fade;
                tc.lineWidth = TRAIL_BODY_WIDTH;
                tc.stroke(bucket.path);
            }
            tc.globalAlpha = 1;
            tc.globalCompositeOperation = "source-over";
            trailDirty = trailBuckets.length > 0;
        }

        const now = Date.now();
        let hasWork = false;

        for (const cursor of cursorMap.values()) {
            if (reducedMotion) {
                // Plain dots only: snap to target, no trails or glow
                cursor.x = cursor.targetX;
                cursor.y = cursor.targetY;
                if (cursor.active) {
                    drawCursorDot(cursorCtx, cursor, w, h, true);
                    hasWork = true;
                }
                continue;
            }

            // The local cursor tracks the true pointer position with no
            // smoothing (zero added latency); the latest stamped slice
            // ends at most half a sample behind it, under the dot. Remote
            // cursors interpolate toward the newest batched network point
            // so they glide at display refresh rate even with ~30Hz input;
            // the batch path is already stamped on the trail layer.
            if (cursor.local) {
                cursor.x = cursor.targetX;
                cursor.y = cursor.targetY;
            } else {
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
            }

            if (cursor.active) {
                drawCursorDot(cursorCtx, cursor, w, h, false);
                hasWork = true;
            }
        }

        // Release ripples (multiple may coexist). Same vivid treatment as
        // the trail: full-alpha colored ring with a layered-stroke glow and
        // a thin white hot edge (no shadowBlur — a wide low-alpha stroke of
        // the same arc reads the same and costs a fraction). A ring stroke
        // never overlaps itself, so 'lighter' is safe here at full alpha.
        if (ripples.length > 0) {
            const c = cursorCtx;
            let keep = 0;
            for (const ripple of ripples) {
                const v = rippleAt(now - ripple.start);
                if (v.done) continue;
                c.save();
                c.globalCompositeOperation = "lighter";
                c.beginPath();
                c.arc(ripple.x * w, ripple.y * h, v.radius, 0, Math.PI * 2);
                // Faint colored fill for a soft "ping"
                c.globalAlpha = v.alpha * 0.12;
                c.fillStyle = ripple.color;
                c.fill();
                // Wide faint halo ring (replaces the old shadowBlur glow)
                c.globalAlpha = v.alpha * 0.28;
                c.strokeStyle = ripple.color;
                c.lineWidth = 7;
                c.stroke();
                // Main colored ring
                c.globalAlpha = v.alpha;
                c.lineWidth = 2.5;
                c.stroke();
                // Thin white hot edge
                c.globalAlpha = v.alpha * 0.35;
                c.strokeStyle = "#fff";
                c.lineWidth = 1;
                c.stroke();
                c.restore();
                ripples[keep++] = ripple;
            }
            ripples.length = keep;
            if (keep > 0) hasWork = true;
        }

        return hasWork || trailBuckets.length > 0;
    }

    // Pre-rendered radial-gradient glow sprites, one per participant
    // color. drawImage of a small offscreen canvas replaces the old
    // shadowBlur halo (which re-blurred the dot every frame); the sprite
    // is rasterized once and reused forever.
    const glowSprites = new Map<string, HTMLCanvasElement>();
    const GLOW_SPRITE_SIZE = 64; // backing store px
    const DOT_GLOW_RADIUS = 18; // on-screen halo radius px

    function getGlowSprite(color: string): HTMLCanvasElement {
        let sprite = glowSprites.get(color);
        if (sprite) return sprite;
        sprite = document.createElement("canvas");
        sprite.width = GLOW_SPRITE_SIZE;
        sprite.height = GLOW_SPRITE_SIZE;
        const g = sprite.getContext("2d")!;
        const half = GLOW_SPRITE_SIZE / 2;
        // Soft alpha falloff in white, then tint with the participant
        // color via source-in (keeps destination alpha, takes source
        // color) — works for any CSS color string without parsing it.
        const grad = g.createRadialGradient(half, half, 0, half, half, half);
        grad.addColorStop(0, "rgba(255, 255, 255, 0.55)");
        grad.addColorStop(0.4, "rgba(255, 255, 255, 0.22)");
        grad.addColorStop(1, "rgba(255, 255, 255, 0)");
        g.fillStyle = grad;
        g.fillRect(0, 0, GLOW_SPRITE_SIZE, GLOW_SPRITE_SIZE);
        g.globalCompositeOperation = "source-in";
        g.fillStyle = color;
        g.fillRect(0, 0, GLOW_SPRITE_SIZE, GLOW_SPRITE_SIZE);
        glowSprites.set(color, sprite);
        return sprite;
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
        if (!plain) {
            // Colored glow halo: one cheap drawImage of the cached sprite
            // so the dot reads on dark footage (no shadowBlur).
            const r = DOT_GLOW_RADIUS;
            c.globalCompositeOperation = "lighter";
            c.drawImage(getGlowSprite(cursor.color), px - r, py - r, r * 2, r * 2);
            c.globalCompositeOperation = "source-over";
        }
        c.fillStyle = cursor.color;
        c.beginPath();
        c.arc(px, py, 5.5, 0, Math.PI * 2);
        c.fill();
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
    <!-- Trail layer (age-bucketed paths, redrawn each frame) below,
         cursor layer (dots, labels, ripples) above. -->
    <canvas bind:this={trailCanvas} class="laser-canvas"></canvas>
    <canvas bind:this={cursorCanvas} class="laser-canvas"></canvas>
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
