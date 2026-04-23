<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { goto } from "$app/navigation";
    import { page } from "$app/stores";
    import { session } from "$lib/stores/session.svelte";
    import { chatStore } from "$lib/stores/chat.svelte";
    import { unlockAudio, getAudioContext } from "$lib/audio/context";
    import { WebRTCManager } from "$lib/webrtc/manager";
    import { AudioDuckingManager } from "$lib/audio/ducking";
    import LaserPointerOverlay from "$lib/components/LaserPointerOverlay.svelte";
    import ChatPanel from "$lib/components/ChatPanel.svelte";
    import WatermarkOverlay from "$lib/components/WatermarkOverlay.svelte";
    import BrowserToast from "$lib/components/BrowserToast.svelte";

    const slug = $page.params.slug!;

    let videoElement = $state<HTMLVideoElement | null>(null);
    let isChatOpen = $state(false);
    let isControlsVisible = $state(true);
    let controlsTimer: ReturnType<typeof setTimeout> | null = null;
    let participantName = $state("Viewer");
    let webrtcManager: WebRTCManager | null = null;
    let audioDuckingManager: AudioDuckingManager | null = null;
    let iceServers: RTCIceServer[] = [];
    let hasStream = $state(false);
    let isMuted = $state(true); // Start muted for autoplay compliance
    let isMicEnabled = $state(false);
    let hasMicPermission = $state(false);
    type MicPromptState = "hidden" | "requesting" | "granted" | "denied";
    let micPromptState = $state<MicPromptState>("hidden");
    let micAutoRequestStarted = false;
    let micAutoEnablePending = false;
    let initialOfferHandled = false;
    let micPromptTimer: ReturnType<typeof setTimeout> | null = null;
    let selectedParticipant = $state<{ id: string; name: string; role: string } | null>(null);
    let adminMenuPosition = $state({ x: 0, y: 0 });
    let currentRtt = $state<number | null>(null);
    let statsInterval: ReturnType<typeof setInterval> | null = null;
    let streamVolume = $state(1.0);
    let voiceVolume = $state(1.0);
    let showVolumeControls = $state(false);
    let streamError = $state<string | null>(null);
    let needsPlayClick = $state(false); // Autoplay fallback
    let streamPaused = $state(false); // Stream temporarily disconnected
    let isLaserEnabled = $state(false);
    let showParticipantList = $state(false);
    let speakingParticipants = $state<Set<string>>(new Set());
    // VAD analysers share the page's AudioContext; only the analyser node is
    // per-track. The context is torn down in cleanupWebRTC, not per-track.
    let voiceAnalysers = new Map<string, AnalyserNode>();
    let vadFrame: ReturnType<typeof requestAnimationFrame> | null = null;
    // Buffer voice tracks that arrive before audioDuckingManager is created
    // (can happen when voice relay tracks are in the initial offer and ontrack
    // fires for them before the main video/audio track triggers handleTrack).
    let pendingVoiceTracks = new Map<string, MediaStreamTrack>();

    // Get session data from storage
    let sessionData = $state<{
        participantId: string;
        token: string;
        color: string;
        name?: string;
    } | null>(null);

    onMount(async () => {
        // Get session data
        const stored = sessionStorage.getItem(`chromatic_session_${slug}`);
        if (!stored) {
            goto(`/room/${slug}`);
            return;
        }

        sessionData = JSON.parse(stored);

        const storedName = localStorage.getItem('chromatic_name');
        if (storedName) {
            participantName = storedName;
        }

        await unlockAudio();

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
                initialOfferHandled = true;
                tryEnablePendingAutoMic();
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

        session.onMessage("room:live", () => {
            console.log('Room is now live');
        });

        session.onMessage("stream:paused", (payload: unknown) => {
            const data = payload as { message?: string };
            streamError = null;
            streamPaused = true;
            if (data?.message) console.log(data.message);
        });

        session.onMessage("stream:resumed", () => {
            streamError = null;
            streamPaused = false;
        });

        session.onMessage("room:ended", () => {
            alert("The session has ended.");
            cleanupWebRTC();
            goto(`/room/${slug}`);
        });

        session.onMessage("kicked", (payload: unknown) => {
            const data = payload as { reason?: string };
            alert(data.reason || "You have been removed from the session.");
            cleanupWebRTC();
            goto(`/room/${slug}`);
        });

        session.onMessage("admin:muted", (payload: unknown) => {
            const data = payload as { participantId: string };
            if (data.participantId === sessionData?.participantId) {
                isMicEnabled = false;
                micAutoEnablePending = false;
                webrtcManager?.setMicEnabled(false);
                session.send("media:toggle", { audio: false });
            }
        });

        session.onMessage("voice:track", async (payload: unknown) => {
            const data = payload as { participantId: string; track: MediaStreamTrack };
            if (audioDuckingManager && data.track) {
                await audioDuckingManager.addVoiceTrack(data.participantId, data.track);
            }
        });

        session.onMessage("signal:voice-answer", async (payload: unknown) => {
            const data = payload as { sdp: string };
            if (webrtcManager) await webrtcManager.handleVoiceAnswer(data.sdp);
        });

        session.onMessage("signal:renegotiate", async (payload: unknown) => {
            const data = payload as { sdp: string; participantId?: string };
            if (webrtcManager) await webrtcManager.handleRenegotiation(data.sdp, data.participantId);
        });

        session.onMessage("signal:answer", async (payload: unknown) => {
            const data = payload as { sdp: string };
            if (webrtcManager) await webrtcManager.handleVoiceAnswer(data.sdp);
        });

        // Release per-participant audio graph (VAD + ducking) when someone
        // leaves the room — otherwise their AnalyserNode and GainNode stay
        // connected to the shared AudioContext and the graph grows with churn.
        session.onMessage("participant:left", (payload: unknown) => {
            const data = payload as { participantId: string };
            if (data?.participantId) {
                removeVoiceAnalyser(data.participantId);
                audioDuckingManager?.removeVoiceTrack(data.participantId);
            }
        });

        // Connect only after handlers are registered so early messages aren't dropped.
        session.connect(slug, sessionData!.token, participantName);

        window.addEventListener("chromatic:tampering", handleTampering);
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
        clearMicPromptTimer();
    });

    function initializeWebRTC() {
        if (webrtcManager) return;

        webrtcManager = new WebRTCManager({
            iceServers,
            onTrack: handleTrack,
            onVoiceTrack: handleVoiceTrack,
            sendSignal: (type, payload) => session.send(type, payload),
            onIceRestartFailed: () => {
                streamError = "Connection failed. The stream could not be reached. Please try refreshing the page.";
            }
        });

        console.log('WebRTC manager initialized');
        session.send("media:toggle", { audio: false });
        void startAutoMicConnection();
    }

    function clearMicPromptTimer() {
        if (micPromptTimer) {
            clearTimeout(micPromptTimer);
            micPromptTimer = null;
        }
    }

    function hideMicPromptLater(delayMs = 1400) {
        clearMicPromptTimer();
        micPromptTimer = setTimeout(() => {
            micPromptState = "hidden";
            micPromptTimer = null;
        }, delayMs);
    }

    async function startAutoMicConnection() {
        if (!webrtcManager || micAutoRequestStarted || hasMicPermission) return;
        micAutoRequestStarted = true;
        micPromptState = "requesting";

        const granted = await webrtcManager.requestMicrophone();
        if (!granted) {
            micPromptState = "denied";
            micAutoEnablePending = false;
            isMicEnabled = false;
            session.send("media:toggle", { audio: false });
            return;
        }

        hasMicPermission = true;
        micAutoEnablePending = true;
        tryEnablePendingAutoMic();
    }

    function tryEnablePendingAutoMic() {
        if (!webrtcManager || !hasMicPermission || !micAutoEnablePending || !initialOfferHandled) return;

        micAutoEnablePending = false;
        isMicEnabled = true;
        webrtcManager.setMicEnabled(true);
        session.send("media:toggle", { audio: true });
        micPromptState = "granted";
        hideMicPromptLater();
    }

    async function retryMicConnection() {
        micAutoRequestStarted = false;
        micAutoEnablePending = false;
        await startAutoMicConnection();
    }

    function dismissMicPrompt() {
        micPromptState = "hidden";
    }

    function handleTrack(event: RTCTrackEvent) {
        console.log('Received track:', event.track.kind, event.streams);
        streamError = null;

        if (!videoElement) {
            streamError = "Video player not ready. Please refresh the page.";
            return;
        }

        if (!event.streams || event.streams.length === 0 || !event.streams[0]) {
            streamError = "Stream not available. The host may need to restart streaming.";
            return;
        }

        try {
            // Only set srcObject if it's a new stream. During renegotiation the
            // browser may fire ontrack again for existing tracks; resetting
            // srcObject causes Firefox to briefly go black.
            if (videoElement.srcObject === event.streams[0]) {
                return;
            }

            videoElement.srcObject = event.streams[0];

            // Attempt autoplay (muted for browser compliance)
            const playPromise = videoElement.play();
            if (playPromise) {
                playPromise.then(() => {
                    hasStream = true;
                    needsPlayClick = false;
                }).catch(() => {
                    // Autoplay blocked - show play button
                    hasStream = true;
                    needsPlayClick = true;
                });
            } else {
                hasStream = true;
            }

            if (!audioDuckingManager) {
                const isUserAdmin = roomState?.participants?.find(
                    (p: { id: string }) => p.id === sessionData?.participantId
                )?.role === 'admin';
                audioDuckingManager = new AudioDuckingManager(videoElement, isUserAdmin ?? false);
                // Flush any voice tracks that arrived before we were created
                for (const [pid, vTrack] of pendingVoiceTracks) {
                    audioDuckingManager.addVoiceTrack(pid, vTrack);
                }
                pendingVoiceTracks.clear();
            }
            startStatsPolling();
        } catch (err) {
            console.error('Failed to attach stream:', err);
            streamError = "Failed to display stream. Please try refreshing the page.";
            hasStream = false;
        }
    }

    function handlePlayClick() {
        if (videoElement) {
            videoElement.play();
            needsPlayClick = false;
        }
    }

    function startStatsPolling() {
        if (statsInterval) return;
        let inFlight = false;
        statsInterval = setInterval(async () => {
            if (!webrtcManager || inFlight) return;
            inFlight = true;
            try {
                const stats = await webrtcManager.getStats();
                currentRtt = stats.rtt ?? null;
            } finally {
                inFlight = false;
            }
        }, 2000);
    }

    function stopStatsPolling() {
        if (statsInterval) {
            clearInterval(statsInterval);
            statsInterval = null;
        }
    }

    function handleVoiceTrack(participantId: string, track: MediaStreamTrack) {
        if (audioDuckingManager) {
            audioDuckingManager.addVoiceTrack(participantId, track);
        } else {
            // Buffer track for when audioDuckingManager is created — voice
            // relay tracks can arrive in the initial offer before the main
            // video/audio track has triggered handleTrack (which creates
            // audioDuckingManager). Without this buffer, early joiners miss
            // voice until the next renegotiation.
            pendingVoiceTracks.set(participantId, track);
        }

        // Set up voice activity detection for speaking indicator.
        // Reuse the shared AudioContext (already resumed) rather than
        // creating new ones — Firefox keeps new AudioContexts suspended
        // when created outside a user-gesture handler, so the AnalyserNode
        // would return silence and the VAD would never trigger.
        getAudioContext().then(ctx => {
            const stream = new MediaStream([track]);
            const source = ctx.createMediaStreamSource(stream);
            const analyser = ctx.createAnalyser();
            analyser.fftSize = 256;
            source.connect(analyser);
            voiceAnalysers.set(participantId, analyser);

            // Start VAD monitoring if not running
            if (!vadFrame) startVADMonitoring();
        }).catch(err => {
            console.warn('Failed to set up VAD for participant', participantId, err);
        });
    }

    function removeVoiceAnalyser(participantId: string) {
        const analyser = voiceAnalysers.get(participantId);
        if (!analyser) return;
        try {
            analyser.disconnect();
        } catch {
            // Already disconnected or destroyed.
        }
        voiceAnalysers.delete(participantId);
        if (voiceAnalysers.size === 0 && vadFrame) {
            cancelAnimationFrame(vadFrame);
            vadFrame = null;
        }
        if (speakingParticipants.has(participantId)) {
            const next = new Set(speakingParticipants);
            next.delete(participantId);
            speakingParticipants = next;
        }
    }

    function startVADMonitoring() {
        const check = () => {
            const newSpeaking = new Set<string>();
            for (const [pid, analyser] of voiceAnalysers) {
                const data = new Uint8Array(analyser.frequencyBinCount);
                analyser.getByteFrequencyData(data);
                const avg = data.reduce((sum, val) => sum + val, 0) / data.length;
                const db = 20 * Math.log10(avg / 255);
                if (db > -50) {
                    newSpeaking.add(pid);
                }
            }
            speakingParticipants = newSpeaking;
            vadFrame = requestAnimationFrame(check);
        };
        vadFrame = requestAnimationFrame(check);
    }

    function cleanupWebRTC() {
        stopStatsPolling();
        if (vadFrame) {
            cancelAnimationFrame(vadFrame);
            vadFrame = null;
        }
        // Disconnect analysers explicitly so they release their MediaStream
        // source and let the browser GC the tracks; otherwise the graph stays
        // wired until the shared AudioContext is closed.
        for (const analyser of voiceAnalysers.values()) {
            try {
                analyser.disconnect();
            } catch {
                // Already disconnected.
            }
        }
        voiceAnalysers.clear();
        speakingParticipants = new Set();
        if (webrtcManager) {
            webrtcManager.close();
            webrtcManager = null;
        }
        hasStream = false;
        currentRtt = null;
        initialOfferHandled = false;
        micAutoEnablePending = false;
        clearMicPromptTimer();
        micPromptState = "hidden";
        if (controlsTimer) {
            clearTimeout(controlsTimer);
            controlsTimer = null;
        }
    }

    function handleTampering() {
        session.disconnect();
        alert("Session terminated due to policy violation.");
        goto("/");
    }

    function startControlsTimer() {
        if (controlsTimer) clearTimeout(controlsTimer);
        isControlsVisible = true;
        controlsTimer = setTimeout(() => {
            if (!showParticipantList) {
                isControlsVisible = false;
            }
        }, 4000);
    }

    function handleMouseMove() {
        startControlsTimer();
    }

    function toggleChat() {
        isChatOpen = !isChatOpen;
    }

    function toggleParticipantList() {
        showParticipantList = !showParticipantList;
    }

    function handleResync() {
        webrtcManager?.requestResync();
    }

    function toggleLaser() {
        isLaserEnabled = !isLaserEnabled;
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
        if (!webrtcManager) return;

        if (!hasMicPermission) {
            const granted = await webrtcManager.requestMicrophone();
            if (granted) {
                hasMicPermission = true;
                micAutoEnablePending = false;
                isMicEnabled = true;
                webrtcManager.setMicEnabled(true);
                session.send("media:toggle", { audio: true });
                micPromptState = "granted";
                hideMicPromptLater();
                return;
            } else {
                micPromptState = "denied";
                return;
            }
        }

        isMicEnabled = !isMicEnabled;
        webrtcManager.setMicEnabled(isMicEnabled);
        session.send("media:toggle", { audio: isMicEnabled });
        if (isMicEnabled) {
            micPromptState = "granted";
            hideMicPromptLater();
        }
    }

    function handleParticipantClick(event: MouseEvent, participant: { id: string; name: string; role: string }) {
        if (!isAdmin || participant.id === sessionData?.participantId) return;
        const rect = (event.currentTarget as HTMLElement).getBoundingClientRect();
        adminMenuPosition = { x: rect.left, y: rect.top - 10 };
        selectedParticipant = participant;
    }

    function closeAdminMenu() { selectedParticipant = null; }

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

    // Derived state
    let unreadCount = $derived(chatStore.unreadCount);
    let roomState = $derived(session.state.room);
    let participants = $derived(roomState?.participants || []);
    let isLive = $derived(roomState?.isLive || false);
    let isAdmin = $derived(
        roomState?.participants?.find(
            (p: { id: string }) => p.id === sessionData?.participantId
        )?.role === 'admin'
    );
    let activeSpeakers = $derived(
        participants.filter((p: { id: string }) => speakingParticipants.has(p.id))
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

            {#if videoElement && hasStream && !needsPlayClick}
                <LaserPointerOverlay {videoElement} enabled={isLaserEnabled} />
            {/if}

            {#if roomState?.watermarkMode && roomState.watermarkMode !== 'none'}
                <WatermarkOverlay
                    mode={roomState.watermarkMode}
                    text={roomState.watermarkText || "{{ name }} - {{ date }}"}
                    logoUrl={roomState.watermarkLogoUrl}
                    logoPosition={roomState.watermarkLogoPosition || 'bottom-right'}
                    opacity={roomState.watermarkOpacity ?? 0.3}
                    {participantName}
                    roomName={roomState?.name || ""}
                />
            {/if}
        </div>

        <!-- Status overlays - OUTSIDE video-container so they stack above controls -->
        {#if needsPlayClick}
            <div class="stream-status-overlay">
                <button class="play-btn" aria-label="Play stream" onclick={handlePlayClick}>
                    <svg viewBox="0 0 24 24" fill="currentColor" width="48" height="48"><path d="M8 5v14l11-7z"/></svg>
                </button>
                <p>Tap to play stream</p>
            </div>
        {:else if streamError}
            <div class="stream-status-overlay error">
                <div class="error-icon">!</div>
                <p class="error-message">{streamError}</p>
                <button class="btn btn-primary" onclick={() => window.location.reload()}>
                    Refresh Page
                </button>
            </div>
        {:else if streamPaused}
            <div class="stream-status-overlay">
                <div class="paused-icon">
                    <svg viewBox="0 0 24 24" fill="currentColor" width="48" height="48"><path d="M6 19h4V5H6v14zm8-14v14h4V5h-4z"/></svg>
                </div>
                <p>Stream paused</p>
                <p class="stream-subtext">Waiting for host to reconnect...</p>
            </div>
        {:else if !hasStream}
            <div class="stream-status-overlay">
                <div class="waiting-spinner"></div>
                <p>{isLive ? "Connecting to stream..." : "Waiting for stream..."}</p>
            </div>
        {/if}

        {#if micPromptState !== "hidden"}
            <div class="mic-prompt" class:success={micPromptState === "granted"} class:error={micPromptState === "denied"} role="status" aria-live="polite">
                {#if micPromptState === "requesting"}
                    <div class="mic-spinner" aria-hidden="true"></div>
                    <div class="mic-prompt-copy">
                        <p class="mic-prompt-title">Connecting your microphone...</p>
                        <p class="mic-prompt-text">Allow browser mic access so you can talk right away.</p>
                    </div>
                {:else if micPromptState === "granted"}
                    <div class="mic-prompt-copy">
                        <p class="mic-prompt-title">Microphone connected</p>
                    </div>
                {:else}
                    <div class="mic-prompt-copy">
                        <p class="mic-prompt-title">Microphone access is blocked</p>
                        <p class="mic-prompt-text">Enable your mic to join voice chat.</p>
                    </div>
                    <div class="mic-prompt-actions">
                        <button class="mic-prompt-btn primary" onclick={retryMicConnection}>Enable Mic</button>
                        <button class="mic-prompt-btn" onclick={dismissMicPrompt}>Continue Muted</button>
                    </div>
                {/if}
            </div>
        {/if}

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
                    <button class="participant-count" onclick={toggleParticipantList} class:active={showParticipantList}>
                        <svg viewBox="0 0 24 24" fill="currentColor" width="16" height="16"><path d="M16 11c1.66 0 2.99-1.34 2.99-3S17.66 5 16 5c-1.66 0-3 1.34-3 3s1.34 3 3 3zm-8 0c1.66 0 2.99-1.34 2.99-3S9.66 5 8 5C6.34 5 5 6.34 5 8s1.34 3 3 3zm0 2c-2.33 0-7 1.17-7 3.5V19h14v-2.5c0-2.33-4.67-3.5-7-3.5zm8 0c-.29 0-.62.02-.97.05 1.16.84 1.97 1.97 1.97 3.45V19h6v-2.5c0-2.33-4.67-3.5-7-3.5z"/></svg>
                        {participants.length}
                    </button>
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
                                <span class="mic-indicator"></span>
                            {/if}
                        </button>
                    {/each}
                    {#if participants.length > 5}
                        <div class="participant-avatar more">
                            +{participants.length - 5}
                        </div>
                    {/if}
                </div>

                {#if selectedParticipant}
                    <!-- svelte-ignore a11y_click_events_have_key_events -->
                    <!-- svelte-ignore a11y_no_static_element_interactions -->
                    <div class="admin-menu-backdrop" onclick={closeAdminMenu}></div>
                    <div class="admin-menu" style="left: {adminMenuPosition.x}px; bottom: calc(100vh - {adminMenuPosition.y}px)">
                        <div class="admin-menu-header">{selectedParticipant.name}</div>
                        <button class="admin-menu-item" onclick={handleMuteParticipant}>Mute</button>
                        <button class="admin-menu-item danger" onclick={handleKickParticipant}>Remove</button>
                    </div>
                {/if}

                <!-- Main control bar - large, obvious buttons with labels -->
                <div class="control-bar">
                    <button
                        class="control-btn"
                        class:active={isMicEnabled}
                        class:off={!isMicEnabled}
                        onclick={toggleMic}
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            {#if isMicEnabled}
                                <path d="M12 14c1.66 0 2.99-1.34 2.99-3L15 5c0-1.66-1.34-3-3-3S9 3.34 9 5v6c0 1.66 1.34 3 3 3zm5.3-3c0 3-2.54 5.1-5.3 5.1S6.7 14 6.7 11H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c3.28-.48 6-3.3 6-6.72h-1.7z"/>
                            {:else}
                                <path d="M19 11h-1.7c0 .74-.16 1.43-.43 2.05l1.23 1.23c.56-.98.9-2.09.9-3.28zm-4.02.17c0-.06.02-.11.02-.17V5c0-1.66-1.34-3-3-3S9 3.34 9 5v.18l5.98 5.99zM4.27 3L3 4.27l6.01 6.01V11c0 1.66 1.33 3 2.99 3 .22 0 .44-.03.65-.08l1.66 1.66c-.71.33-1.5.52-2.31.52-2.76 0-5.3-2.1-5.3-5.1H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c.91-.13 1.77-.45 2.54-.9L19.73 21 21 19.73 4.27 3z"/>
                            {/if}
                        </svg>
                        <span class="control-label">{isMicEnabled ? "Mic On" : "Mic Off"}</span>
                    </button>

                    <button
                        class="control-btn"
                        class:off={isMuted}
                        onclick={toggleMute}
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            {#if isMuted}
                                <path d="M16.5 12c0-1.77-1.02-3.29-2.5-4.03v2.21l2.45 2.45c.03-.2.05-.41.05-.63zm2.5 0c0 .94-.2 1.82-.54 2.64l1.51 1.51C20.63 14.91 21 13.5 21 12c0-4.28-2.99-7.86-7-8.77v2.06c2.89.86 5 3.54 5 6.71zM4.27 3L3 4.27 7.73 9H3v6h4l5 5v-6.73l4.25 4.25c-.67.52-1.42.93-2.25 1.18v2.06c1.38-.31 2.63-.95 3.69-1.81L19.73 21 21 19.73l-9-9L4.27 3zM12 4L9.91 6.09 12 8.18V4z"/>
                            {:else}
                                <path d="M3 9v6h4l5 5V4L7 9H3zm13.5 3c0-1.77-1.02-3.29-2.5-4.03v8.05c1.48-.73 2.5-2.25 2.5-4.02zM14 3.23v2.06c2.89.86 5 3.54 5 6.71s-2.11 5.85-5 6.71v2.06c4.01-.91 7-4.49 7-8.77s-2.99-7.86-7-8.77z"/>
                            {/if}
                        </svg>
                        <span class="control-label">{isMuted ? "Sound Off" : "Sound On"}</span>
                    </button>

                    <button
                        class="control-btn chat-btn"
                        class:active={isChatOpen}
                        onclick={toggleChat}
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            <path d="M20 2H4c-1.1 0-1.99.9-1.99 2L2 22l4-4h14c1.1 0 2-.9 2-2V4c0-1.1-.9-2-2-2zM6 9h12v2H6V9zm8 5H6v-2h8v2zm4-6H6V6h12v2z"/>
                        </svg>
                        <span class="control-label">Chat</span>
                        {#if unreadCount > 0}
                            <span class="chat-badge">{unreadCount > 9 ? '9+' : unreadCount}</span>
                        {/if}
                    </button>

                    <button
                        class="control-btn"
                        onclick={handleResync}
                        title="Fix frozen stream"
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            <path d="M17.65 6.35C16.2 4.9 14.21 4 12 4c-4.42 0-7.99 3.58-7.99 8s3.57 8 7.99 8c3.73 0 6.84-2.55 7.73-6h-2.08c-.82 2.33-3.04 4-5.65 4-3.31 0-6-2.69-6-6s2.69-6 6-6c1.66 0 3.14.69 4.22 1.78L13 11h7V4l-2.35 2.35z"/>
                        </svg>
                        <span class="control-label">Resync</span>
                    </button>

                    <button
                        class="control-btn"
                        class:active={isLaserEnabled}
                        onclick={toggleLaser}
                        title="Toggle laser pointer mode"
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            <path d="M4 20h2l6-6-2-2-6 6v2zm7.4-7.4 2 2L18 10.01a1.41 1.41 0 0 0 0-2l-2.01-2.01a1.41 1.41 0 0 0-2 0L11.4 8.6zM19 14l-4 4 1 1a2.83 2.83 0 0 0 4 0 2.83 2.83 0 0 0 0-4l-1-1z"/>
                        </svg>
                        <span class="control-label">{isLaserEnabled ? "Laser On" : "Laser Off"}</span>
                    </button>

                    <button
                        class="control-btn"
                        onclick={toggleFullscreen}
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            <path d="M7 14H5v5h5v-2H7v-3zm-2-4h2V7h3V5H5v5zm12 7h-3v2h5v-5h-2v3zM14 5v2h3v3h2V5h-5z"/>
                        </svg>
                        <span class="control-label">Fullscreen</span>
                    </button>
                </div>
            </div>
        </div>

        <!-- Active speaker indicator (always visible when someone is speaking) -->
        {#if activeSpeakers.length > 0}
            <div class="active-speaker-indicator">
                {#each activeSpeakers as speaker (speaker.id)}
                    <div class="active-speaker-chip">
                        <span class="active-speaker-avatar" style="background-color: {speaker.color}">
                            {speaker.name.charAt(0).toUpperCase()}
                        </span>
                        <span class="active-speaker-name">{speaker.name}</span>
                        <span class="active-speaker-pulse"></span>
                    </div>
                {/each}
            </div>
        {/if}

        <!-- Participant list (outside controls overlay so it doesn't auto-hide) -->
        {#if showParticipantList}
            <div class="participant-list">
                {#each participants as p (p.id)}
                    <button
                        class="participant-list-item"
                        class:clickable={isAdmin && p.id !== sessionData?.participantId}
                        class:speaking={speakingParticipants.has(p.id)}
                        onclick={(e) => handleParticipantClick(e, p)}
                    >
                        <span class="participant-list-avatar" style="background-color: {p.color}">
                            {p.name.charAt(0).toUpperCase()}
                        </span>
                        <span class="participant-list-name">
                            {p.name}
                            {#if p.id === sessionData?.participantId}
                                <span class="you-label">(you)</span>
                            {/if}
                        </span>
                        {#if p.role === 'admin'}
                            <span class="role-badge">Host</span>
                        {/if}
                        {#if speakingParticipants.has(p.id)}
                            <span class="speaking-indicator" title="Speaking">
                                <svg viewBox="0 0 24 24" fill="currentColor" width="14" height="14"><path d="M12 14c1.66 0 2.99-1.34 2.99-3L15 5c0-1.66-1.34-3-3-3S9 3.34 9 5v6c0 1.66 1.34 3 3 3zm5.3-3c0 3-2.54 5.1-5.3 5.1S6.7 14 6.7 11H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c3.28-.48 6-3.3 6-6.72h-1.7z"/></svg>
                            </span>
                        {:else if p.audioEnabled}
                            <span class="mic-on-indicator" title="Mic on">
                                <svg viewBox="0 0 24 24" fill="currentColor" width="14" height="14"><path d="M12 14c1.66 0 2.99-1.34 2.99-3L15 5c0-1.66-1.34-3-3-3S9 3.34 9 5v6c0 1.66 1.34 3 3 3z" opacity="0.5"/></svg>
                            </span>
                        {/if}
                    </button>
                {/each}
            </div>
        {/if}
    </div>

    <ChatPanel
        isOpen={isChatOpen}
        onClose={() => (isChatOpen = false)}
        roomSlug={slug}
        joinToken={sessionData?.token || ""}
    />
    <BrowserToast />
</main>

<style>
    .session-page {
        display: flex;
        height: 100vh;
        height: 100dvh;
        background: #000;
        overflow: hidden;
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
        aspect-ratio: auto;
        border-radius: 0;
    }

    .video-container video {
        width: 100%;
        height: 100%;
        object-fit: contain;
        background: #000;
        /* Establish stacking context so overlays render above the video
           compositor layer in Firefox and other browsers */
        position: relative;
        z-index: 0;
    }

    .stream-status-overlay {
        position: absolute;
        inset: 0;
        z-index: 15;
        pointer-events: auto;
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        background: rgba(0, 0, 0, 0.8);
        color: var(--color-text-muted);
        gap: var(--space-md);
    }

    .stream-status-overlay .paused-icon { color: var(--color-text-muted); }
    .stream-status-overlay .stream-subtext { font-size: 0.875rem; color: var(--color-text-subtle); }
    .stream-status-overlay.error { gap: var(--space-lg); }
    .stream-status-overlay .error-icon {
        width: 60px; height: 60px;
        background: var(--color-error);
        border-radius: 50%;
        display: flex; align-items: center; justify-content: center;
        font-size: 2rem; font-weight: bold; color: white;
    }
    .stream-status-overlay .error-message {
        color: var(--color-text); font-size: 1rem; text-align: center; max-width: 300px;
    }

    .play-btn {
        width: 80px; height: 80px;
        border-radius: 50%;
        background: var(--color-primary);
        border: none;
        color: white;
        cursor: pointer;
        display: flex; align-items: center; justify-content: center;
        transition: transform 0.2s ease, background 0.2s ease;
    }
    .play-btn:hover { transform: scale(1.1); background: var(--color-primary-hover); }

    .mic-prompt {
        position: absolute;
        top: 18px;
        left: 50%;
        transform: translateX(-50%);
        z-index: 16;
        width: min(92vw, 460px);
        display: flex;
        flex-direction: column;
        gap: var(--space-sm);
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
        background: rgba(6, 18, 32, 0.92);
        border: 1px solid rgba(72, 182, 166, 0.35);
        color: var(--color-text);
        box-shadow: var(--shadow-lg);
        pointer-events: auto;
    }
    .mic-prompt.success {
        border-color: rgba(16, 185, 129, 0.5);
        background: rgba(7, 36, 28, 0.92);
    }
    .mic-prompt.error {
        border-color: rgba(239, 68, 68, 0.55);
        background: rgba(44, 9, 9, 0.95);
    }
    .mic-prompt-copy {
        display: flex;
        flex-direction: column;
        gap: 2px;
    }
    .mic-prompt-title {
        margin: 0;
        font-size: 0.95rem;
        font-weight: 600;
        color: #fff;
    }
    .mic-prompt-text {
        margin: 0;
        font-size: 0.8125rem;
        color: var(--color-text-muted);
    }
    .mic-spinner {
        width: 16px;
        height: 16px;
        border: 2px solid rgba(255, 255, 255, 0.2);
        border-top-color: var(--color-primary);
        border-radius: 50%;
        animation: mic-spin 0.8s linear infinite;
    }
    .mic-prompt-actions {
        display: flex;
        gap: var(--space-xs);
    }
    .mic-prompt-btn {
        border: 1px solid rgba(255, 255, 255, 0.22);
        background: rgba(255, 255, 255, 0.08);
        color: #fff;
        border-radius: var(--radius-sm);
        padding: 6px 10px;
        font-size: 0.75rem;
        font-weight: 600;
        cursor: pointer;
    }
    .mic-prompt-btn.primary {
        background: var(--color-primary);
        border-color: var(--color-primary);
        color: #041014;
    }
    .mic-prompt-btn:hover {
        filter: brightness(1.08);
    }
    @keyframes mic-spin {
        to { transform: rotate(360deg); }
    }

    /* Controls overlay */
    .controls-overlay {
        position: absolute;
        inset: 0;
        z-index: 10;
        display: flex;
        flex-direction: column;
        justify-content: space-between;
        padding: var(--space-md) var(--space-lg);
        pointer-events: none;
        opacity: 0;
        visibility: hidden;
        transition: opacity var(--transition-normal), visibility var(--transition-normal);
    }
    .controls-overlay.visible { opacity: 1; visibility: visible; }
    .controls-overlay.visible > * { pointer-events: auto; }

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
        font-size: 0.75rem; font-family: monospace;
        background: rgba(0, 0, 0, 0.6);
        padding: var(--space-xs) var(--space-sm);
        border-radius: var(--radius-sm);
        border: 1px solid transparent;
    }
    .latency-display.good { color: var(--color-success); border-color: var(--color-success); }
    .latency-display.warning { color: var(--color-warning); border-color: var(--color-warning); }
    .latency-display.bad { color: var(--color-error); border-color: var(--color-error); }

    .participant-count {
        display: flex; align-items: center; gap: 4px;
        font-size: 0.875rem;
        color: var(--color-text-muted);
        background: rgba(0, 0, 0, 0.6);
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
        border: 1px solid transparent;
        cursor: pointer;
        transition: all 0.15s ease;
    }
    .participant-count:hover { border-color: rgba(255,255,255,0.2); }
    .participant-count.active { border-color: var(--color-primary); color: var(--color-primary); }

    /* Participant list dropdown */
    .participant-list {
        position: absolute;
        top: 52px;
        right: var(--space-lg);
        background: rgba(0, 0, 0, 0.85);
        backdrop-filter: blur(12px);
        -webkit-backdrop-filter: blur(12px);
        border: 1px solid rgba(255,255,255,0.1);
        border-radius: var(--radius-md);
        min-width: 220px;
        max-height: 300px;
        overflow-y: auto;
        z-index: 50;
        padding: var(--space-xs) 0;
    }

    .participant-list-item {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: var(--space-sm) var(--space-md);
        width: 100%;
        background: none;
        border: none;
        color: var(--color-text);
        font-size: 0.8125rem;
        cursor: default;
        transition: background 0.1s ease;
        text-align: left;
    }
    .participant-list-item.clickable { cursor: pointer; }
    .participant-list-item.clickable:hover { background: rgba(255,255,255,0.08); }
    .participant-list-item.speaking {
        background: rgba(72, 182, 166, 0.1);
    }

    .participant-list-avatar {
        width: 1.75rem; height: 1.75rem;
        border-radius: 50%;
        display: flex; align-items: center; justify-content: center;
        font-size: 0.75rem; font-weight: 600; color: white;
        flex-shrink: 0;
    }
    .participant-list-name {
        flex: 1;
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
    }
    .you-label { color: var(--color-text-subtle); font-size: 0.75rem; }
    .role-badge {
        font-size: 0.625rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        color: var(--color-warning);
        background: rgba(245, 158, 11, 0.15);
        padding: 2px 6px;
        border-radius: var(--radius-sm);
    }
    .speaking-indicator {
        color: var(--color-success);
        display: flex; align-items: center;
        animation: pulse-speaking 1s infinite;
    }
    .mic-on-indicator {
        color: var(--color-text-subtle);
        display: flex; align-items: center;
    }
    @keyframes pulse-speaking {
        0%, 100% { opacity: 1; }
        50% { opacity: 0.5; }
    }

    .bottom-bar {
        display: flex;
        justify-content: space-between;
        align-items: flex-end;
    }

    .participants {
        display: flex;
        gap: var(--space-xs);
    }

    .participant-avatar {
        width: 2.5rem; height: 2.5rem;
        border-radius: 50%;
        display: flex; align-items: center; justify-content: center;
        font-size: 0.875rem; font-weight: 600; color: white;
        border: 2px solid var(--color-bg);
        position: relative; cursor: default; padding: 0;
    }
    .participant-avatar.clickable { cursor: pointer; transition: transform var(--transition-fast); }
    .participant-avatar.clickable:hover { transform: scale(1.1); box-shadow: 0 0 0 2px var(--color-primary); }
    .participant-avatar.is-admin { border-color: var(--color-warning); }
    .participant-avatar .mic-indicator {
        position: absolute; bottom: -2px; right: -2px;
        width: 10px; height: 10px;
        background: var(--color-success);
        border-radius: 50%;
        border: 2px solid #000;
    }
    .participant-avatar.more { background: var(--color-surface); font-size: 0.75rem; }

    /* Admin menu */
    .admin-menu-backdrop { position: fixed; inset: 0; z-index: 100; }
    .admin-menu {
        position: fixed; z-index: 101;
        background: var(--color-surface);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-md);
        box-shadow: var(--shadow-lg);
        min-width: 140px; overflow: hidden;
    }
    .admin-menu-header {
        padding: var(--space-sm) var(--space-md);
        font-weight: 600; font-size: 0.875rem;
        border-bottom: 1px solid var(--color-border);
    }
    .admin-menu-item {
        width: 100%; padding: var(--space-sm) var(--space-md);
        background: none; border: none; text-align: left; cursor: pointer;
        font-size: 0.875rem; color: var(--color-text);
        transition: background var(--transition-fast);
    }
    .admin-menu-item:hover { background: var(--color-bg-hover); }
    .admin-menu-item.danger { color: var(--color-error); }
    .admin-menu-item.danger:hover { background: rgba(239, 68, 68, 0.1); }

    /* ========== CONTROL BAR - Large, obvious, user-friendly ========== */
    .control-bar {
        display: flex;
        gap: var(--space-sm);
        background: rgba(0, 0, 0, 0.75);
        backdrop-filter: blur(12px);
        -webkit-backdrop-filter: blur(12px);
        padding: var(--space-sm) var(--space-md);
        border-radius: 1rem;
    }

    .control-btn {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: 4px;
        padding: 10px 16px;
        background: rgba(255, 255, 255, 0.08);
        border: 1px solid rgba(255, 255, 255, 0.1);
        border-radius: 12px;
        color: #fff;
        cursor: pointer;
        transition: all 0.15s ease;
        position: relative;
        min-width: 64px;
    }

    .control-btn:hover {
        background: rgba(255, 255, 255, 0.15);
        border-color: rgba(255, 255, 255, 0.2);
    }

    .control-btn.active {
        background: rgba(72, 182, 166, 0.2);
        border-color: var(--color-primary);
        color: var(--color-primary);
    }

    .control-btn.off {
        background: rgba(239, 68, 68, 0.15);
        border-color: rgba(239, 68, 68, 0.4);
        color: var(--color-error);
    }

    .control-btn svg {
        flex-shrink: 0;
    }

    .control-label {
        font-size: 0.625rem;
        font-weight: 500;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        white-space: nowrap;
    }

    .chat-badge {
        position: absolute;
        top: -4px;
        right: -4px;
        background: var(--color-error);
        color: white;
        font-size: 0.6rem;
        font-weight: 700;
        min-width: 1.1rem;
        height: 1.1rem;
        border-radius: var(--radius-full);
        display: flex;
        align-items: center;
        justify-content: center;
        padding: 0 3px;
        line-height: 1;
        border: 2px solid #000;
    }

    /* Active speaker indicator */
    .active-speaker-indicator {
        position: absolute;
        bottom: 100px;
        left: 50%;
        transform: translateX(-50%);
        z-index: 12;
        display: flex;
        gap: var(--space-xs);
        pointer-events: none;
    }
    .active-speaker-chip {
        display: flex;
        align-items: center;
        gap: var(--space-xs);
        background: rgba(0, 0, 0, 0.75);
        backdrop-filter: blur(12px);
        -webkit-backdrop-filter: blur(12px);
        padding: 6px 12px 6px 6px;
        border-radius: 999px;
        border: 1px solid rgba(72, 182, 166, 0.4);
        animation: speaker-fade-in 0.2s ease-out;
    }
    .active-speaker-avatar {
        width: 1.5rem;
        height: 1.5rem;
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 0.6875rem;
        font-weight: 600;
        color: white;
        flex-shrink: 0;
    }
    .active-speaker-name {
        font-size: 0.8125rem;
        font-weight: 500;
        color: #fff;
        white-space: nowrap;
    }
    .active-speaker-pulse {
        width: 8px;
        height: 8px;
        border-radius: 50%;
        background: var(--color-success);
        animation: pulse-speaking 1s infinite;
        flex-shrink: 0;
    }
    @keyframes speaker-fade-in {
        from { opacity: 0; transform: translateY(6px); }
        to { opacity: 1; transform: translateY(0); }
    }

    @media (max-width: 768px) {
        .session-page { flex-direction: column; }
        .video-wrapper { flex: none; height: 60vh; }
        .control-bar { gap: 4px; padding: var(--space-xs) var(--space-sm); }
        .control-btn { padding: 8px 10px; min-width: 52px; }
        .control-btn svg { width: 20px; height: 20px; }
        .control-label { font-size: 0.5625rem; }
        .active-speaker-indicator { bottom: 80px; }
    }
</style>
