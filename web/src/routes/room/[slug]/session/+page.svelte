<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { page } from "$app/stores";
    import { session } from "$lib/stores/session.svelte";
    import { unlockAudio } from "$lib/audio/context";
    import { WebRTCManager } from "$lib/webrtc/manager";
    import { AudioDuckingManager } from "$lib/audio/ducking";
    import LaserPointerOverlay from "$lib/components/LaserPointerOverlay.svelte";
    import ChatPanel from "$lib/components/ChatPanel.svelte";
    import WatermarkOverlay from "$lib/components/WatermarkOverlay.svelte";
    import BrowserToast from "$lib/components/BrowserToast.svelte";

    const slug = $page.params.slug!;

    let videoElement: HTMLVideoElement;
    let isChatOpen = $state(false);
    let isControlsVisible = $state(true);
    let controlsTimer: ReturnType<typeof setTimeout>;
    let participantName = $state("Viewer");
    let webrtcManager: WebRTCManager | null = null;
    let audioDuckingManager: AudioDuckingManager | null = null;
    let iceServers: RTCIceServer[] = [];
    let hasStream = $state(false);
    let isMuted = $state(false); // Stream audio mute state
    let isMicEnabled = $state(false); // Microphone state
    let hasMicPermission = $state(false); // Whether mic permission was granted

    // Get session data from storage
    let sessionData: {
        participantId: string;
        token: string;
        color: string;
        name?: string;
    } | null = null;

    onMount(async () => {
        // Get session data
        const stored = sessionStorage.getItem(`chromatic_session_${slug}`);
        if (!stored) {
            window.location.href = `/room/${slug}`;
            return;
        }

        sessionData = JSON.parse(stored);

        // Get participant name from session data or localStorage
        const storedName = localStorage.getItem('chromatic_name');
        if (storedName) {
            participantName = storedName;
        }

        // Unlock audio (must be from user interaction, but we've already had one)
        await unlockAudio();

        // Connect WebSocket
        session.connect(slug, sessionData!.token, participantName);

        // Handle ICE servers from room state
        session.onMessage("iceServers", (servers: unknown) => {
            iceServers = servers as RTCIceServer[];
            console.log('Received ICE servers:', iceServers);
            initializeWebRTC();
        });

        // Handle WebRTC offer from server
        session.onMessage("signal:offer", async (payload: unknown) => {
            const data = payload as { sdp: string };
            console.log('Received WebRTC offer');

            if (!webrtcManager) {
                initializeWebRTC();
            }

            if (webrtcManager) {
                await webrtcManager.handleOffer(data.sdp);
            }
        });

        // Handle ICE candidates from server
        session.onMessage("signal:candidate", async (payload: unknown) => {
            const data = payload as { candidate: string; sdpMid?: string; sdpMLineIndex?: number };
            if (webrtcManager) {
                await webrtcManager.handleCandidate({
                    candidate: data.candidate,
                    sdpMid: data.sdpMid ?? null,
                    sdpMLineIndex: data.sdpMLineIndex ?? null
                });
            }
        });

        // Handle room going live
        session.onMessage("room:live", () => {
            console.log('Room is now live');
            // WebRTC offer will be sent by server
        });

        // Handle room ending
        session.onMessage("room:ended", () => {
            alert("The session has ended.");
            cleanupWebRTC();
            window.location.href = `/room/${slug}`;
        });

        // Handle kicked
        session.onMessage("kicked", (payload: unknown) => {
            const data = payload as { reason?: string };
            alert(data.reason || "You have been removed from the session.");
            cleanupWebRTC();
            window.location.href = `/room/${slug}`;
        });

        // Handle voice tracks from other participants
        session.onMessage("voice:track", async (payload: unknown) => {
            const data = payload as { participantId: string; track: MediaStreamTrack };
            if (audioDuckingManager && data.track) {
                await audioDuckingManager.addVoiceTrack(data.participantId, data.track);
            }
        });

        // Handle answer to our voice offer (when we send mic audio)
        session.onMessage("signal:voice-answer", async (payload: unknown) => {
            const data = payload as { sdp: string };
            if (webrtcManager) {
                await webrtcManager.handleVoiceAnswer(data.sdp);
            }
        });

        // Handle watermark tampering
        window.addEventListener("chromatic:tampering", handleTampering);

        // Auto-hide controls
        startControlsTimer();
    });

    onDestroy(() => {
        cleanupWebRTC();
        if (audioDuckingManager) {
            audioDuckingManager.destroy();
            audioDuckingManager = null;
        }
        session.disconnect();
        window.removeEventListener("chromatic:tampering", handleTampering);
    });

    function initializeWebRTC() {
        if (webrtcManager) return;

        webrtcManager = new WebRTCManager({
            iceServers,
            onTrack: handleTrack,
            onVoiceTrack: handleVoiceTrack,
            sendSignal: (type, payload) => session.send(type, payload)
        });

        console.log('WebRTC manager initialized');
    }

    function handleTrack(event: RTCTrackEvent) {
        console.log('Received track:', event.track.kind, event.streams);

        if (videoElement && event.streams[0]) {
            videoElement.srcObject = event.streams[0];
            hasStream = true;
            console.log('Attached stream to video element');

            // Initialize audio ducking manager for the stream
            // Determine if user is admin (check role in session)
            const isAdmin = roomState?.participants?.find(
                (p: { id: string }) => p.id === sessionData?.participantId
            )?.role === 'admin';
            audioDuckingManager = new AudioDuckingManager(videoElement, isAdmin ?? false);
        }
    }

    function handleVoiceTrack(participantId: string, track: MediaStreamTrack) {
        console.log('Received voice track from:', participantId);
        if (audioDuckingManager) {
            audioDuckingManager.addVoiceTrack(participantId, track);
        }
    }

    function cleanupWebRTC() {
        if (webrtcManager) {
            webrtcManager.close();
            webrtcManager = null;
        }
        hasStream = false;
    }

    function handleTampering() {
        session.disconnect();
        alert("Session terminated due to policy violation.");
        window.location.href = "/";
    }

    function startControlsTimer() {
        clearTimeout(controlsTimer);
        isControlsVisible = true;
        controlsTimer = setTimeout(() => {
            isControlsVisible = false;
        }, 3000);
    }

    function handleMouseMove() {
        startControlsTimer();
    }

    function toggleChat() {
        isChatOpen = !isChatOpen;
    }

    function toggleFullscreen() {
        if (document.fullscreenElement) {
            document.exitFullscreen();
        } else {
            document.documentElement.requestFullscreen();
        }
    }

    function toggleMute() {
        isMuted = !isMuted;
        if (videoElement) {
            videoElement.muted = isMuted;
        }
    }

    async function toggleMic() {
        if (!hasMicPermission) {
            // Request microphone permission first
            if (webrtcManager) {
                const granted = await webrtcManager.requestMicrophone();
                if (granted) {
                    hasMicPermission = true;
                    isMicEnabled = true;
                    webrtcManager.setMicEnabled(true);
                } else {
                    alert("Microphone access is required for voice chat.");
                }
            }
        } else {
            // Toggle mic state
            isMicEnabled = !isMicEnabled;
            webrtcManager?.setMicEnabled(isMicEnabled);
        }

        // Notify server of media toggle
        session.send("media:toggle", { audio: isMicEnabled });
    }

    // Get room state
    let roomState = $derived(session.state.room);
    let participants = $derived(roomState?.participants || []);
    let isLive = $derived(roomState?.isLive || false);
</script>

<svelte:head>
    <title>{roomState?.name || "Session"} | Chromatic</title>
</svelte:head>

<main class="session-page" onmousemove={handleMouseMove}>
    <div class="video-wrapper">
        <div class="video-container">
            <video bind:this={videoElement} autoplay playsinline muted={isMuted}>
                <track kind="captions" />
            </video>

            {#if videoElement}
                <LaserPointerOverlay {videoElement} />
            {/if}

            <WatermarkOverlay
                mode="text"
                text={"{{ name }} - {{ date }}"}
                opacity={0.3}
                {participantName}
            />

            {#if !hasStream}
                <div class="stream-offline">
                    <div class="waiting-spinner"></div>
                    <p>{isLive ? "Connecting to stream..." : "Waiting for stream..."}</p>
                </div>
            {/if}
        </div>

        <!-- Controls overlay -->
        <div class="controls-overlay" class:visible={isControlsVisible}>
            <div class="top-bar">
                <div class="room-name">{roomState?.name || "Session"}</div>
                <div class="participant-count">
                    {participants.length} viewer{participants.length !== 1
                        ? "s"
                        : ""}
                </div>
            </div>

            <div class="bottom-bar">
                <div class="participants">
                    {#each participants.slice(0, 5) as p (p.id)}
                        <div
                            class="participant-avatar"
                            style="background-color: {p.color}"
                            title={p.name}
                        >
                            {p.name.charAt(0).toUpperCase()}
                        </div>
                    {/each}
                    {#if participants.length > 5}
                        <div class="participant-avatar more">
                            +{participants.length - 5}
                        </div>
                    {/if}
                </div>

                <div class="controls">
                    <button
                        class="btn btn-icon btn-ghost"
                        onclick={toggleMic}
                        title={isMicEnabled ? "Mute Mic" : "Enable Mic"}
                        class:mic-active={isMicEnabled}
                    >
                        {isMicEnabled ? "🎤" : "🎙️"}
                    </button>
                    <button
                        class="btn btn-icon btn-ghost"
                        onclick={toggleMute}
                        title={isMuted ? "Unmute Stream" : "Mute Stream"}
                    >
                        {isMuted ? "🔇" : "🔊"}
                    </button>
                    <button
                        class="btn btn-icon btn-ghost"
                        onclick={toggleChat}
                        title="Chat"
                    >
                        💬
                    </button>
                    <button
                        class="btn btn-icon btn-ghost"
                        onclick={toggleFullscreen}
                        title="Fullscreen"
                    >
                        ⛶
                    </button>
                </div>
            </div>
        </div>
    </div>

    <ChatPanel
        isOpen={isChatOpen}
        onClose={() => (isChatOpen = false)}
        roomSlug={slug}
        participantId={sessionData?.participantId || ""}
    />
    <BrowserToast />
</main>

<style>
    .session-page {
        display: flex;
        height: 100vh;
        background: #000;
    }

    .video-wrapper {
        flex: 1;
        position: relative;
        display: flex;
        align-items: center;
        justify-content: center;
    }

    .video-container {
        position: relative;
        width: 100%;
        height: 100%;
    }

    .video-container video {
        width: 100%;
        height: 100%;
        object-fit: contain;
        background: #000;
    }

    .stream-offline {
        position: absolute;
        inset: 0;
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        background: rgba(0, 0, 0, 0.8);
        color: var(--color-text-muted);
        gap: var(--space-md);
    }

    .controls-overlay {
        position: absolute;
        inset: 0;
        display: flex;
        flex-direction: column;
        justify-content: space-between;
        padding: var(--space-lg);
        pointer-events: none;
        opacity: 0;
        transition: opacity var(--transition-normal);
    }

    .controls-overlay.visible {
        opacity: 1;
    }

    .controls-overlay > * {
        pointer-events: auto;
    }

    .top-bar {
        display: flex;
        justify-content: space-between;
        align-items: center;
    }

    .room-name {
        font-weight: 600;
        background: rgba(0, 0, 0, 0.6);
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
    }

    .participant-count {
        font-size: 0.875rem;
        color: var(--color-text-muted);
        background: rgba(0, 0, 0, 0.6);
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
    }

    .bottom-bar {
        display: flex;
        justify-content: space-between;
        align-items: center;
    }

    .participants {
        display: flex;
        gap: var(--space-xs);
    }

    .participant-avatar {
        width: 2.5rem;
        height: 2.5rem;
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 0.875rem;
        font-weight: 600;
        color: white;
        border: 2px solid var(--color-bg);
    }

    .participant-avatar.more {
        background: var(--color-surface);
        font-size: 0.75rem;
    }

    .controls {
        display: flex;
        gap: var(--space-sm);
        background: rgba(0, 0, 0, 0.6);
        padding: var(--space-sm);
        border-radius: var(--radius-lg);
    }

    .controls button.mic-active {
        background: var(--color-success);
        border-radius: var(--radius-md);
    }

    @media (max-width: 768px) {
        .session-page {
            flex-direction: column;
        }

        .video-wrapper {
            flex: none;
            height: 60vh;
        }
    }
</style>
