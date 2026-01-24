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
    let selectedParticipant = $state<{ id: string; name: string; role: string } | null>(null);
    let adminMenuPosition = $state({ x: 0, y: 0 });
    let currentRtt = $state<number | null>(null);
    let statsInterval: ReturnType<typeof setInterval> | null = null;
    let streamVolume = $state(1.0);
    let voiceVolume = $state(1.0);
    let showVolumeControls = $state(false);
    let streamError = $state<string | null>(null);

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

        // Handle admin muting a participant
        session.onMessage("admin:muted", (payload: unknown) => {
            const data = payload as { participantId: string };
            // If we are the muted participant, disable our mic
            if (data.participantId === sessionData?.participantId) {
                isMicEnabled = false;
                webrtcManager?.setMicEnabled(false);
            }
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

        // Handle server-initiated renegotiation (when receiving voice tracks from others)
        session.onMessage("signal:renegotiate", async (payload: unknown) => {
            const data = payload as { sdp: string; participantId?: string };
            console.log('Received renegotiation offer', data.participantId ? `for voice from ${data.participantId}` : '');
            if (webrtcManager) {
                await webrtcManager.handleRenegotiation(data.sdp, data.participantId);
            }
        });

        // Handle ICE restart answer from server
        session.onMessage("signal:answer", async (payload: unknown) => {
            const data = payload as { sdp: string };
            console.log('Received answer from server (ICE restart or renegotiation)');
            // This is handled by the ICE restart flow in WebRTCManager
            // The manager sets remote description when it receives the answer
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

        // Clear any previous error state
        streamError = null;

        // Validate we have the video element and stream
        if (!videoElement) {
            console.error('Video element not available');
            streamError = "Video player not ready. Please refresh the page.";
            return;
        }

        if (!event.streams || event.streams.length === 0 || !event.streams[0]) {
            console.error('No stream available in track event');
            streamError = "Stream not available. The host may need to restart streaming.";
            return;
        }

        try {
            videoElement.srcObject = event.streams[0];
            hasStream = true;
            console.log('Attached stream to video element');

            // Initialize audio ducking manager for the stream
            // Determine if user is admin (check role in session)
            const isUserAdmin = roomState?.participants?.find(
                (p: { id: string }) => p.id === sessionData?.participantId
            )?.role === 'admin';
            audioDuckingManager = new AudioDuckingManager(videoElement, isUserAdmin ?? false);

            // Start polling stats for latency display
            startStatsPolling();
        } catch (err) {
            console.error('Failed to attach stream to video element:', err);
            streamError = "Failed to display stream. Please try refreshing the page.";
            hasStream = false;
        }
    }

    function startStatsPolling() {
        if (statsInterval) return;

        statsInterval = setInterval(async () => {
            if (webrtcManager) {
                const stats = await webrtcManager.getStats();
                currentRtt = stats.rtt ?? null;
            }
        }, 2000); // Poll every 2 seconds
    }

    function stopStatsPolling() {
        if (statsInterval) {
            clearInterval(statsInterval);
            statsInterval = null;
        }
    }

    function handleVoiceTrack(participantId: string, track: MediaStreamTrack) {
        console.log('Received voice track from:', participantId);
        if (audioDuckingManager) {
            audioDuckingManager.addVoiceTrack(participantId, track);
        }
    }

    function cleanupWebRTC() {
        stopStatsPolling();
        if (webrtcManager) {
            webrtcManager.close();
            webrtcManager = null;
        }
        hasStream = false;
        currentRtt = null;
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

    function toggleVolumeControls() {
        showVolumeControls = !showVolumeControls;
    }

    function handleStreamVolumeChange(event: Event) {
        const target = event.target as HTMLInputElement;
        streamVolume = parseFloat(target.value);
        audioDuckingManager?.setStreamVolume(streamVolume);
    }

    function handleVoiceVolumeChange(event: Event) {
        const target = event.target as HTMLInputElement;
        voiceVolume = parseFloat(target.value);
        audioDuckingManager?.setVoiceVolume(voiceVolume);
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

    // Admin actions
    function handleParticipantClick(event: MouseEvent, participant: { id: string; name: string; role: string }) {
        // Only admins can manage participants (and not themselves)
        if (!isAdmin || participant.id === sessionData?.participantId) return;

        const rect = (event.currentTarget as HTMLElement).getBoundingClientRect();
        adminMenuPosition = { x: rect.left, y: rect.top - 10 };
        selectedParticipant = participant;
    }

    function closeAdminMenu() {
        selectedParticipant = null;
    }

    function handleMuteParticipant() {
        if (!selectedParticipant) return;
        session.send("admin:mute", { participantId: selectedParticipant.id });
        closeAdminMenu();
    }

    function handleKickParticipant() {
        if (!selectedParticipant) return;
        if (confirm(`Remove ${selectedParticipant.name} from the session?`)) {
            session.send("admin:kick", { participantId: selectedParticipant.id });
        }
        closeAdminMenu();
    }

    // Get room state
    let roomState = $derived(session.state.room);
    let participants = $derived(roomState?.participants || []);
    let isLive = $derived(roomState?.isLive || false);
    let isAdmin = $derived(
        roomState?.participants?.find(
            (p: { id: string }) => p.id === sessionData?.participantId
        )?.role === 'admin'
    );
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

            {#if roomState?.watermarkMode && roomState.watermarkMode !== 'none'}
                <WatermarkOverlay
                    mode={roomState.watermarkMode}
                    text={roomState.watermarkText || "{{ name }} - {{ date }}"}
                    logoUrl={roomState.watermarkLogoUrl}
                    logoPosition={roomState.watermarkLogoPosition || 'bottom-right'}
                    opacity={roomState.watermarkOpacity ?? 0.3}
                    {participantName}
                />
            {/if}

            {#if streamError}
                <div class="stream-offline error">
                    <div class="error-icon">!</div>
                    <p class="error-message">{streamError}</p>
                    <button class="btn btn-primary" onclick={() => window.location.reload()}>
                        Refresh Page
                    </button>
                </div>
            {:else if !hasStream}
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
                <div class="top-bar-right">
                    {#if isAdmin && currentRtt !== null}
                        <div class="latency-display" class:good={currentRtt < 100} class:warning={currentRtt >= 100 && currentRtt < 300} class:bad={currentRtt >= 300}>
                            ~{Math.round(currentRtt)}ms
                        </div>
                    {/if}
                    <div class="participant-count">
                        {participants.length} viewer{participants.length !== 1
                            ? "s"
                            : ""}
                    </div>
                </div>
            </div>

            <div class="bottom-bar">
                <div class="participants">
                    {#each participants.slice(0, 5) as p (p.id)}
                        <button
                            class="participant-avatar"
                            class:clickable={isAdmin && p.id !== sessionData?.participantId}
                            class:is-admin={p.role === 'admin'}
                            style="background-color: {p.color}"
                            title={p.name}
                            onclick={(e) => handleParticipantClick(e, p)}
                        >
                            {p.name.charAt(0).toUpperCase()}
                            {#if p.audioEnabled}
                                <span class="mic-indicator">🎤</span>
                            {/if}
                        </button>
                    {/each}
                    {#if participants.length > 5}
                        <div class="participant-avatar more">
                            +{participants.length - 5}
                        </div>
                    {/if}
                </div>

                <!-- Admin menu popup -->
                {#if selectedParticipant}
                    <!-- svelte-ignore a11y_click_events_have_key_events -->
                    <!-- svelte-ignore a11y_no_static_element_interactions -->
                    <div class="admin-menu-backdrop" onclick={closeAdminMenu}></div>
                    <div
                        class="admin-menu"
                        style="left: {adminMenuPosition.x}px; bottom: calc(100vh - {adminMenuPosition.y}px)"
                    >
                        <div class="admin-menu-header">{selectedParticipant.name}</div>
                        <button class="admin-menu-item" onclick={handleMuteParticipant}>
                            🔇 Mute
                        </button>
                        <button class="admin-menu-item danger" onclick={handleKickParticipant}>
                            ❌ Remove
                        </button>
                    </div>
                {/if}

                <div class="controls">
                    <button
                        class="btn btn-icon btn-ghost"
                        onclick={toggleMic}
                        title={isMicEnabled ? "Mute Mic" : "Enable Mic"}
                        class:mic-active={isMicEnabled}
                    >
                        {isMicEnabled ? "🎤" : "🎙️"}
                    </button>
                    <div class="volume-control-wrapper">
                        <button
                            class="btn btn-icon btn-ghost"
                            onclick={toggleMute}
                            title={isMuted ? "Unmute Stream" : "Mute Stream"}
                        >
                            {isMuted ? "🔇" : "🔊"}
                        </button>
                        <button
                            class="btn btn-icon btn-ghost btn-small"
                            onclick={toggleVolumeControls}
                            title="Volume Controls"
                            class:active={showVolumeControls}
                        >
                            ▲
                        </button>
                        {#if showVolumeControls}
                            <div class="volume-panel">
                                <div class="volume-slider-group">
                                    <label for="stream-volume">Stream</label>
                                    <input
                                        type="range"
                                        id="stream-volume"
                                        min="0"
                                        max="1"
                                        step="0.05"
                                        value={streamVolume}
                                        oninput={handleStreamVolumeChange}
                                    />
                                    <span class="volume-value">{Math.round(streamVolume * 100)}%</span>
                                </div>
                                <div class="volume-slider-group">
                                    <label for="voice-volume">Voice Chat</label>
                                    <input
                                        type="range"
                                        id="voice-volume"
                                        min="0"
                                        max="1"
                                        step="0.05"
                                        value={voiceVolume}
                                        oninput={handleVoiceVolumeChange}
                                    />
                                    <span class="volume-value">{Math.round(voiceVolume * 100)}%</span>
                                </div>
                            </div>
                        {/if}
                    </div>
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

    .stream-offline.error {
        gap: var(--space-lg);
    }

    .stream-offline .error-icon {
        width: 60px;
        height: 60px;
        background: var(--color-error);
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 2rem;
        font-weight: bold;
        color: white;
    }

    .stream-offline .error-message {
        color: var(--color-text);
        font-size: 1rem;
        text-align: center;
        max-width: 300px;
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

    .top-bar-right {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
    }

    .room-name {
        font-weight: 600;
        background: rgba(0, 0, 0, 0.6);
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
    }

    .latency-display {
        font-size: 0.75rem;
        font-family: monospace;
        background: rgba(0, 0, 0, 0.6);
        padding: var(--space-xs) var(--space-sm);
        border-radius: var(--radius-sm);
        border: 1px solid transparent;
    }

    .latency-display.good {
        color: var(--color-success);
        border-color: var(--color-success);
    }

    .latency-display.warning {
        color: var(--color-warning);
        border-color: var(--color-warning);
    }

    .latency-display.bad {
        color: var(--color-error);
        border-color: var(--color-error);
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
        position: relative;
        cursor: default;
        padding: 0;
    }

    .participant-avatar.clickable {
        cursor: pointer;
        transition: transform var(--transition-fast), box-shadow var(--transition-fast);
    }

    .participant-avatar.clickable:hover {
        transform: scale(1.1);
        box-shadow: 0 0 0 2px var(--color-primary);
    }

    .participant-avatar.is-admin {
        border-color: var(--color-warning);
    }

    .participant-avatar .mic-indicator {
        position: absolute;
        bottom: -4px;
        right: -4px;
        font-size: 0.6rem;
        background: var(--color-success);
        border-radius: 50%;
        width: 1rem;
        height: 1rem;
        display: flex;
        align-items: center;
        justify-content: center;
    }

    .participant-avatar.more {
        background: var(--color-surface);
        font-size: 0.75rem;
    }

    /* Admin menu styles */
    .admin-menu-backdrop {
        position: fixed;
        inset: 0;
        z-index: 100;
    }

    .admin-menu {
        position: fixed;
        z-index: 101;
        background: var(--color-surface);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-md);
        box-shadow: var(--shadow-lg);
        min-width: 140px;
        overflow: hidden;
    }

    .admin-menu-header {
        padding: var(--space-sm) var(--space-md);
        font-weight: 600;
        font-size: 0.875rem;
        border-bottom: 1px solid var(--color-border);
        color: var(--color-text);
    }

    .admin-menu-item {
        width: 100%;
        padding: var(--space-sm) var(--space-md);
        background: none;
        border: none;
        text-align: left;
        cursor: pointer;
        font-size: 0.875rem;
        color: var(--color-text);
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        transition: background var(--transition-fast);
    }

    .admin-menu-item:hover {
        background: var(--color-bg-hover);
    }

    .admin-menu-item.danger {
        color: var(--color-error);
    }

    .admin-menu-item.danger:hover {
        background: rgba(239, 68, 68, 0.1);
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

    /* Volume control styles */
    .volume-control-wrapper {
        display: flex;
        align-items: center;
        position: relative;
    }

    .btn-small {
        width: 1.5rem !important;
        height: 1.5rem !important;
        min-width: 1.5rem !important;
        font-size: 0.625rem !important;
        padding: 0 !important;
    }

    .btn-small.active {
        background: var(--color-primary);
        border-radius: var(--radius-sm);
    }

    .volume-panel {
        position: absolute;
        bottom: calc(100% + var(--space-sm));
        right: 0;
        background: var(--color-surface);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-md);
        padding: var(--space-md);
        min-width: 200px;
        box-shadow: var(--shadow-lg);
        display: flex;
        flex-direction: column;
        gap: var(--space-md);
    }

    .volume-slider-group {
        display: flex;
        flex-direction: column;
        gap: var(--space-xs);
    }

    .volume-slider-group label {
        font-size: 0.75rem;
        color: var(--color-text-muted);
        font-weight: 500;
    }

    .volume-slider-group input[type="range"] {
        width: 100%;
        accent-color: var(--color-primary);
        cursor: pointer;
    }

    .volume-value {
        font-size: 0.75rem;
        font-family: monospace;
        color: var(--color-text);
        text-align: right;
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
