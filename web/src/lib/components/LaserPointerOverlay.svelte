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

    let overlayEl: HTMLDivElement;
    let isPointing = $state(false);
    let videoRect = $state({ x: 0, y: 0, width: 0, height: 0 });
    let sendThrottle: ReturnType<typeof setTimeout> | null = null;
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
            cursorStore.stopCleanup();
        };
    });

    function handlePointerDown(e: PointerEvent) {
        isPointing = true;
        overlayEl.setPointerCapture(e.pointerId);
        sendCursor(e, true);
    }

    function handlePointerMove(e: PointerEvent) {
        if (!isPointing) return;
        sendCursor(e, true);
    }

    function handlePointerUp(e: PointerEvent) {
        isPointing = false;
        overlayEl.releasePointerCapture(e.pointerId);
        sendCursorEnd();
    }

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
    bind:this={overlayEl}
    class="overlay interactive"
    style="
    left: {videoRect.x}px;
    top: {videoRect.y}px;
    width: {videoRect.width}px;
    height: {videoRect.height}px;
    cursor: crosshair;
  "
    onpointerdown={handlePointerDown}
    onpointermove={handlePointerMove}
    onpointerup={handlePointerUp}
    onpointerleave={handlePointerUp}
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
