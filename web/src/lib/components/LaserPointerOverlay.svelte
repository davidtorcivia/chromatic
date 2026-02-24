<script lang="ts">
    import { onMount } from "svelte";
    import { session } from "$lib/stores/session.svelte";
    import {
        clientToVideoCoords,
        getVideoContentRect,
    } from "$lib/video/coordinates";

    interface Cursor {
        participantId: string;
        participantName: string;
        color: string;
        x: number;
        y: number;
        active: boolean;
        lastUpdate: number;
    }

    interface Props {
        videoElement: HTMLVideoElement;
        enabled: boolean;
    }

    let { videoElement, enabled = false }: Props = $props();

    let isPointing = $state(false);
    let videoRect = $state({ x: 0, y: 0, width: 0, height: 0 });
    let sendThrottle: ReturnType<typeof setTimeout> | null = null;
    let activePointerId: number | null = null;
    let showUsageHint = $state(false);
    let hintTimer: ReturnType<typeof setTimeout> | null = null;
    const THROTTLE_MS = 50;

    // Keep cursor state local to this component for reliable Svelte 5 reactivity.
    // Using a plain object + reassignment (instead of a cross-module $state Map)
    // guarantees $derived invalidation on every update.
    let cursorMap: Record<string, Cursor> = {};
    let cursorList = $state<Cursor[]>([]);

    function updateCursorList() {
        cursorList = Object.values(cursorMap);
    }

    function updateVideoRect() {
        if (videoElement) {
            videoRect = getVideoContentRect(videoElement);
        }
    }

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

        videoElement.addEventListener("loadedmetadata", updateVideoRect);
        window.addEventListener("resize", updateVideoRect);

        // Laser pointer only activates when explicitly enabled from the session controls.
        const handleVideoPointerDown = (e: PointerEvent) => {
            if (!enabled || e.pointerType === "touch" || e.button !== 0 || isPointing) return;
            showUsageHint = false;
            activePointerId = e.pointerId;
            isPointing = true;
            sendCursor(e, true);
        };

        const handleGlobalPointerMove = (e: PointerEvent) => {
            if (!isPointing || activePointerId !== e.pointerId) return;
            sendCursor(e, true);
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

        // Subscribe to cursor updates from WebSocket
        session.onMessage("cursor", (payload) => {
            const data = payload as {
                participantId: string;
                participantName: string;
                color: string;
                x: number;
                y: number;
                active: boolean;
            };
            cursorMap[data.participantId] = {
                ...data,
                lastUpdate: Date.now()
            };
            updateCursorList();
        });

        // Clean up stale cursors every 500ms
        const cleanupInterval = setInterval(() => {
            const now = Date.now();
            let changed = false;
            for (const id of Object.keys(cursorMap)) {
                if (now - cursorMap[id].lastUpdate > 3000) {
                    delete cursorMap[id];
                    changed = true;
                }
            }
            if (changed) updateCursorList();
        }, 500);

        return () => {
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
            clearInterval(cleanupInterval);
        };
    });

    function sendCursor(e: PointerEvent, active: boolean) {
        const coords = clientToVideoCoords(e.clientX, e.clientY, videoElement);

        // Throttle sends
        if (sendThrottle) return;

        sendThrottle = setTimeout(() => {
            sendThrottle = null;
        }, THROTTLE_MS);

        session.send("cursor", { x: coords.x, y: coords.y, active });
    }

    function sendCursorEnd() {
        session.send("cursor", { x: 0, y: 0, active: false });
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
    {#each cursorList as cursor (cursor.participantId)}
        <div
            class="cursor-pointer"
            style="
        left: {cursor.x * 100}%;
        top: {cursor.y * 100}%;
        opacity: {cursor.active ? 1 : 0};
        transition: opacity 150ms ease;
      "
        >
            <div
                class="cursor-dot"
                style="background-color: {cursor.color}; box-shadow: 0 0 8px {cursor.color};"
            ></div>
            <span
                class="cursor-label"
                style="background-color: {cursor.color};"
            >
                {cursor.participantName}
            </span>
        </div>
    {/each}
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

    .cursor-pointer {
        position: absolute;
        pointer-events: none;
        transform: translate(-50%, -50%);
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
    }

    .cursor-dot {
        width: 12px;
        height: 12px;
        border-radius: 50%;
        border: 2px solid white;
    }

    .cursor-label {
        position: absolute;
        top: 16px;
        left: 50%;
        transform: translateX(-50%);
        font-size: 0.625rem;
        font-weight: 600;
        color: white;
        padding: 2px 6px;
        border-radius: 4px;
        white-space: nowrap;
    }
</style>
