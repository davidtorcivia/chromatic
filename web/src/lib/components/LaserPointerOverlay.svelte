<script lang="ts">
    import { onMount } from "svelte";
    import { session } from "$lib/stores/session.svelte";
    import {
        clientToVideoCoords,
        getVideoContentRect,
    } from "$lib/video/coordinates";
    import { cursorStore, type Cursor } from "$lib/stores/cursors.svelte";

    interface Props {
        videoElement: HTMLVideoElement;
    }

    let { videoElement }: Props = $props();

    let isPointing = $state(false);
    let videoRect = $state({ x: 0, y: 0, width: 0, height: 0 });
    let sendThrottle: ReturnType<typeof setTimeout> | null = null;
    let activePointerId: number | null = null;
    const THROTTLE_MS = 50;

    function updateVideoRect() {
        if (videoElement) {
            videoRect = getVideoContentRect(videoElement);
        }
    }

    onMount(() => {
        updateVideoRect();

        videoElement.addEventListener("loadedmetadata", updateVideoRect);
        window.addEventListener("resize", updateVideoRect);

        const handleGlobalPointerDown = (e: PointerEvent) => {
            if (e.pointerType === "touch" || e.button !== 0 || isPointing) return;

            const target = e.target instanceof Element ? e.target : null;
            if (
                target?.closest(
                    "button, a, input, textarea, select, [role='button'], .controls-overlay, .stream-status-overlay"
                )
            ) {
                return;
            }

            updateVideoRect();
            const withinVideo =
                e.clientX >= videoRect.x &&
                e.clientX <= videoRect.x + videoRect.width &&
                e.clientY >= videoRect.y &&
                e.clientY <= videoRect.y + videoRect.height;
            if (!withinVideo) return;

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

        window.addEventListener("pointerdown", handleGlobalPointerDown);
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
            cursorStore.update(data);
        });

        cursorStore.startCleanup();

        return () => {
            videoElement.removeEventListener("loadedmetadata", updateVideoRect);
            window.removeEventListener("resize", updateVideoRect);
            window.removeEventListener("pointerdown", handleGlobalPointerDown);
            window.removeEventListener("pointermove", handleGlobalPointerMove);
            window.removeEventListener("pointerup", handleGlobalPointerUp);
            window.removeEventListener("pointercancel", handleGlobalPointerUp);
            window.removeEventListener("blur", handleWindowBlur);
            if (sendThrottle) {
                clearTimeout(sendThrottle);
                sendThrottle = null;
            }
            cursorStore.stopCleanup();
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

    // Get current cursors (access the reactive state directly for proper tracking)
    let cursors: Cursor[] = $derived(Array.from(cursorStore.cursors.values()));
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
    {#each cursors as cursor (cursor.participantId)}
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
        cursor: crosshair;
        pointer-events: none;
        z-index: 5;
    }

    .cursor-pointer {
        position: absolute;
        pointer-events: none;
        transform: translate(-50%, -50%);
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
