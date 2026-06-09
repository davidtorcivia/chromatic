<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { fade, fly } from "svelte/transition";
    import { goto } from "$app/navigation";
    import { page } from "$app/stores";
    import { session } from "$lib/stores/session.svelte";
    import { chatStore, type ChatMessage } from "$lib/stores/chat.svelte";
    import { unlockAudio, getAudioContext, closeAudioContext } from "$lib/audio/context";
    import { WebRTCManager } from "$lib/webrtc/manager";
    import { AudioDuckingManager } from "$lib/audio/ducking";
    import LaserPointerOverlay from "$lib/components/LaserPointerOverlay.svelte";
    import ChatPanel from "$lib/components/ChatPanel.svelte";
    import ConfirmDialog from "$lib/components/ConfirmDialog.svelte";
    import WatermarkOverlay from "$lib/components/WatermarkOverlay.svelte";
    import BrowserToast from "$lib/components/BrowserToast.svelte";

    const slug = $page.params.slug!;

    let videoElement = $state<HTMLVideoElement | null>(null);
    let isChatOpen = $state(false);
    let isControlsVisible = $state(true);
    let controlsTimer: ReturnType<typeof setTimeout>;
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
    let kickTarget = $state<{ id: string; name: string } | null>(null);
    let endState = $state<{ title: string; body: string } | null>(null);
    let isFullscreen = $state(false);
    let soundBtnEl = $state<HTMLButtonElement | null>(null);
    let volumePopoverEl = $state<HTMLDivElement | null>(null);
    let participantListEl = $state<HTMLDivElement | null>(null);
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
    let voiceAnalysers = new Map<string, { analyser: AnalyserNode; source: MediaStreamAudioSourceNode }>();
    // Participants with a live (or pending) voice analyser registration. Used
    // to detect "participant left while the async analyser setup was in
    // flight" so the source node gets disconnected instead of leaking.
    let activeVoicePids = new Set<string>();
    let vadFrame: ReturnType<typeof requestAnimationFrame> | null = null;
    // Buffer voice tracks that arrive before audioDuckingManager is created
    // (can happen when voice relay tracks are in the initial offer and ontrack
    // fires for them before the main video/audio track triggers handleTrack).
    let pendingVoiceTracks = new Map<string, MediaStreamTrack>();

    // Screen sharing state
    let screenShareRequested = $state(false);
    let screenShareActive = $state(false);
    let screenShareParticipantId = $state<string | null>(null);
    let screenShareParticipantName = $state<string | null>(null);
    let screenShareStream = $state<MediaStream | null>(null);
    let screenShareVideoEl = $state<HTMLVideoElement | null>(null);
    let pendingScreenShareRequest = $state<{participantId: string, name: string} | null>(null);

    // Get session data from storage
    let sessionData = $state<{
        participantId: string;
        token: string;
        color: string;
        name?: string;
        role?: "admin" | "viewer";
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

        // Clean up any stale WebRTC state from a previous mount (e.g. page refresh)
        cleanupWebRTC();

        // Clear any stale message handlers from a previous session
        session.clearHandlers();

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
                try {
                    await webrtcManager.handleOffer(data.sdp);
                    initialOfferHandled = true;
                    tryEnablePendingAutoMic();
                } catch (err) {
                    console.error('Failed to handle offer:', err);
                }
            }
        });

        // Handle ICE candidates from server
        session.onMessage("signal:candidate", async (payload: unknown) => {
            const data = payload as { candidate: string; sdpMid?: string; sdpMLineIndex?: number };
            if (webrtcManager) {
                try {
                    await webrtcManager.handleCandidate({
                        candidate: data.candidate,
                        sdpMid: data.sdpMid ?? null,
                        sdpMLineIndex: data.sdpMLineIndex ?? null
                    });
                } catch (err) {
                    console.error('Failed to add ICE candidate:', err);
                }
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
            cleanupWebRTC();
            session.disconnect();
            endState = {
                title: "Session ended",
                body: "The host has ended this review session. Thanks for joining.",
            };
        });

        session.onMessage("kicked", (payload: unknown) => {
            const data = payload as { reason?: string };
            cleanupWebRTC();
            session.disconnect();
            endState = {
                title: "You were removed from the session",
                body: data.reason || "An admin removed you from this session.",
            };
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

        session.onMessage("signal:voice-answer", async (payload: unknown) => {
            const data = payload as { sdp: string };
            try {
                if (webrtcManager) await webrtcManager.handleVoiceAnswer(data.sdp);
            } catch (err) {
                console.error('Failed to handle voice answer:', err);
            }
        });

        session.onMessage("signal:renegotiate", async (payload: unknown) => {
            const data = payload as { sdp: string; participantId?: string };
            try {
                if (webrtcManager) await webrtcManager.handleRenegotiation(data.sdp, data.participantId);
            } catch (err) {
                console.error('Failed to handle renegotiation:', err);
            }
        });

        // ICE restart answers — route to handleVoiceAnswer which applies the
        // answer SDP to the same peer connection (same setRemoteDescription call).
        session.onMessage("signal:answer", async (payload: unknown) => {
            const data = payload as { sdp: string };
            try {
                if (webrtcManager) await webrtcManager.handleVoiceAnswer(data.sdp);
            } catch (err) {
                console.error('Failed to handle answer:', err);
            }
        });

        // Screen sharing messages
        session.onMessage("screenshare:approved", async () => {
            screenShareRequested = false;
            const ok = await webrtcManager?.startScreenShare();
            if (ok) {
                screenShareActive = true;
            } else {
                screenShareActive = false;
            }
        });

        session.onMessage("screenshare:denied", (payload: unknown) => {
            screenShareRequested = false;
            const data = payload as { reason?: string };
            console.log('Screen share denied:', data.reason || 'Request denied by admin');
        });

        session.onMessage("screenshare:started", (payload: unknown) => {
            const data = payload as { participantId: string; name: string };
            screenShareParticipantId = data.participantId;
            screenShareParticipantName = data.name;
            pendingScreenShareRequest = null;
        });

        session.onMessage("screenshare:stopped", () => {
            screenShareActive = false;
            screenShareParticipantId = null;
            screenShareParticipantName = null;
            screenShareStream = null;
        });

        session.onMessage("screenshare:pending", (payload: unknown) => {
            const data = payload as { participantId: string; name: string };
            pendingScreenShareRequest = data;
        });

        // Clean up voice resources when a participant leaves
        session.onMessage("participant:left", (payload: unknown) => {
            const data = payload as { participantId: string };
            cleanupParticipantVoice(data.participantId);
        });

        // Chat handlers live here (always active) rather than in ChatPanel,
        // which is conditionally rendered — otherwise history/messages
        // arriving before the panel is first opened would be missed.
        session.onMessage("chat:history", (payload: unknown) => {
            const data = payload as { messages: ChatMessage[] };
            chatStore.loadHistory(data.messages ?? []);
        });

        session.onMessage("chat:message", (payload: unknown) => {
            chatStore.addMessage(payload as ChatMessage);
        });

        // After a successful WS reconnect the server sends a fresh
        // room:state/iceServers/signal:offer sequence. Tear down all stale
        // per-connection state so it re-initializes cleanly instead of the
        // fresh offer hitting a dead peer connection.
        session.onReconnect(() => {
            // The session was terminated (ended/kicked) — don't resurrect it.
            if (endState) return;
            console.log("WebSocket reconnected, resetting WebRTC state");
            cleanupWebRTC();
            if (audioDuckingManager) {
                audioDuckingManager.destroy();
                audioDuckingManager = null;
            }
            // Allow the auto-mic flow to run again against the new manager
            // (the new peer connection has no local stream).
            micAutoRequestStarted = false;
            hasMicPermission = false;
            isMicEnabled = false;
            // Screen share state is re-sent by the server in room:state
            screenShareRequested = false;
            screenShareActive = false;
            screenShareParticipantId = null;
            screenShareParticipantName = null;
            screenShareStream = null;
            pendingScreenShareRequest = null;
            streamPaused = false;
            streamError = null;
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
        closeAudioContext();
        window.removeEventListener("chromatic:tampering", handleTampering);
        clearMicPromptTimer();
        clearTimeout(controlsTimer);
    });

    function initializeWebRTC() {
        if (webrtcManager) return;

        webrtcManager = new WebRTCManager({
            iceServers,
            onTrack: handleTrack,
            onVoiceTrack: handleVoiceTrack,
            onScreenShareTrack: handleScreenShareTrack,
            sendSignal: (type, payload) => session.send(type, payload),
            onIceRestartFailed: () => {
                streamError = "Connection failed. The stream could not be reached. Please try refreshing the page.";
            },
            onScreenShareEnded: () => {
                screenShareActive = false;
                session.send("screenshare:stop", {});
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
                audioDuckingManager = new AudioDuckingManager(videoElement, isAdmin);
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
            videoElement.play().then(() => {
                needsPlayClick = false;
            }).catch(err => {
                console.warn('Play still blocked:', err);
                // Keep the play button visible so user can retry
            });
        }
    }

    function startStatsPolling() {
        if (statsInterval) return;
        statsInterval = setInterval(async () => {
            if (webrtcManager) {
                const stats = await webrtcManager.getStats();
                currentRtt = stats.rtt ?? null;
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
            // Buffer track for when audioDuckingManager is created
            pendingVoiceTracks.set(participantId, track);
        }

        // Mark this participant as having a pending/active voice registration
        // so the async setup below can detect if they left in the meantime.
        activeVoicePids.add(participantId);

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

            // The participant may have left (or the connection may have been
            // cleaned up) while we awaited the AudioContext — don't register a
            // node that nobody will ever disconnect.
            if (!activeVoicePids.has(participantId)) {
                source.disconnect();
                return;
            }

            // Replace any previous analyser for this participant
            const existing = voiceAnalysers.get(participantId);
            if (existing) {
                existing.source.disconnect();
            }
            voiceAnalysers.set(participantId, { analyser, source });

            // Start VAD monitoring if not running
            if (!vadFrame) startVADMonitoring();
        }).catch(err => {
            console.warn('Failed to set up VAD for participant', participantId, err);
        });
    }

    function handleScreenShareTrack(participantId: string, track: MediaStreamTrack) {
        console.log('Received screen share track from', participantId);
        const stream = new MediaStream([track]);
        // Binding to the video element happens in the $effect below once the
        // element is rendered — no separate RAF path racing it.
        screenShareStream = stream;

        // Clean up when track ends
        track.onended = () => {
            if (screenShareStream === stream) {
                screenShareStream = null;
            }
        };
    }

    function cleanupParticipantVoice(participantId: string) {
        // Marks any in-flight async analyser setup as cancelled
        activeVoicePids.delete(participantId);
        const entry = voiceAnalysers.get(participantId);
        if (entry) {
            entry.source.disconnect();
            voiceAnalysers.delete(participantId);
        }
        pendingVoiceTracks.delete(participantId);
        audioDuckingManager?.removeVoiceTrack(participantId);
    }

    function toggleScreenShare() {
        if (screenShareActive) {
            // Stop sharing
            webrtcManager?.stopScreenShare();
            screenShareActive = false;
            session.send("screenshare:stop", {});
            return;
        }

        if (screenShareParticipantId) {
            // Someone else is sharing
            return;
        }

        // Request to share
        screenShareRequested = true;
        session.send("screenshare:request", {});
    }

    function approveScreenShare() {
        if (!pendingScreenShareRequest) return;
        session.send("admin:screenshare-approve", { participantId: pendingScreenShareRequest.participantId });
        pendingScreenShareRequest = null;
    }

    function denyScreenShare() {
        if (!pendingScreenShareRequest) return;
        session.send("admin:screenshare-deny", { participantId: pendingScreenShareRequest.participantId });
        pendingScreenShareRequest = null;
    }

    function stopScreenSharePip() {
        session.send("screenshare:stop", {});
        if (screenShareActive) {
            webrtcManager?.stopScreenShare();
            screenShareActive = false;
        }
    }

    function startVADMonitoring() {
        // Reuse a single buffer to avoid allocating on every frame
        let vadBuffer: Uint8Array<ArrayBuffer> | null = null;
        // Throttle to ~15fps (every 4th animation frame) — sufficient for
        // speaker detection while significantly reducing CPU usage on mobile.
        let vadFrameCount = 0;
        const check = () => {
            vadFrameCount++;
            if (vadFrameCount % 4 !== 0) {
                vadFrame = requestAnimationFrame(check);
                return;
            }
            let changed = false;
            const newSpeaking = new Set<string>();
            for (const [pid, { analyser }] of voiceAnalysers) {
                // Reuse or resize the buffer as needed
                if (!vadBuffer || vadBuffer.length !== analyser.frequencyBinCount) {
                    vadBuffer = new Uint8Array(analyser.frequencyBinCount) as Uint8Array<ArrayBuffer>;
                }
                analyser.getByteFrequencyData(vadBuffer);
                let sum = 0;
                for (let i = 0; i < vadBuffer.length; i++) sum += vadBuffer[i];
                const avg = sum / vadBuffer.length;
                const db = 20 * Math.log10(avg / 255);
                if (db > -50) {
                    newSpeaking.add(pid);
                }
            }
            // Only trigger reactivity if the set actually changed
            if (newSpeaking.size !== speakingParticipants.size) {
                changed = true;
            } else {
                for (const pid of newSpeaking) {
                    if (!speakingParticipants.has(pid)) { changed = true; break; }
                }
            }
            if (changed) {
                speakingParticipants = newSpeaking;
            }
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
        // Disconnect voice analyser source nodes before clearing
        for (const entry of voiceAnalysers.values()) {
            entry.source.disconnect();
        }
        voiceAnalysers.clear();
        // Cancel any in-flight async analyser setups
        activeVoicePids.clear();
        pendingVoiceTracks.clear();
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
    }

    function handleTampering() {
        cleanupWebRTC();
        session.disconnect();
        endState = {
            title: "Session terminated",
            body: "This session was closed due to a content protection policy violation.",
        };
    }

    // Leave control + end-state exit: clear stored credentials for this room
    // and return to the join page.
    function leaveToRoomPage() {
        sessionStorage.removeItem(`chromatic_session_${slug}`);
        goto(`/room/${slug}`);
    }

    function startControlsTimer() {
        clearTimeout(controlsTimer);
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

    function muteParticipant(participantId: string) {
        session.send("admin:mute", { participantId });
    }

    function confirmKickParticipant() {
        if (kickTarget) {
            session.send("admin:kick", { participantId: kickTarget.id });
        }
        kickTarget = null;
    }

    function handleFullscreenChange() {
        isFullscreen = !!document.fullscreenElement;
    }

    // Keyboard shortcuts: M mic, F fullscreen, C chat, L laser, Esc closes
    // popovers. Ignored while typing or while a dialog/end-state is up.
    function handleKeydown(e: KeyboardEvent) {
        const target = e.target as HTMLElement | null;
        if (
            target &&
            (target.tagName === "INPUT" ||
                target.tagName === "TEXTAREA" ||
                target.isContentEditable)
        ) {
            return;
        }
        if (e.metaKey || e.ctrlKey || e.altKey) return;
        if (endState || kickTarget) return;

        switch (e.key.toLowerCase()) {
            case "m":
                void toggleMic();
                break;
            case "f":
                toggleFullscreen();
                break;
            case "c":
                toggleChat();
                break;
            case "l":
                toggleLaser();
                break;
            case "escape":
                if (showVolumeControls) {
                    showVolumeControls = false;
                } else if (showParticipantList) {
                    showParticipantList = false;
                } else if (isChatOpen) {
                    isChatOpen = false;
                }
                break;
        }
    }

    // Close the volume popover on click/tap outside it.
    function handleWindowPointerDown(e: PointerEvent) {
        if (!showVolumeControls) return;
        const target = e.target as Node;
        if (volumePopoverEl?.contains(target) || soundBtnEl?.contains(target)) return;
        showVolumeControls = false;
    }

    // Touch: any tap brings the controls back.
    function handlePagePointerDown(e: PointerEvent) {
        if (e.pointerType !== "touch") return;
        startControlsTimer();
    }

    // Touch: a tap on the bare video toggles the controls (when laser is off).
    function handleVideoPointerDown(e: PointerEvent) {
        if (e.pointerType !== "touch" || isLaserEnabled) return;
        e.stopPropagation();
        if (isControlsVisible) {
            clearTimeout(controlsTimer);
            isControlsVisible = false;
        } else {
            startControlsTimer();
        }
    }

    // Derived state
    let unreadCount = $derived(chatStore.unreadCount);
    let roomState = $derived(session.state.room);
    let participants = $derived(roomState?.participants || []);
    let isLive = $derived(roomState?.isLive || false);
    // Prefer the live role from the server-pushed room state; fall back to the
    // role returned by the join API (persisted in sessionStorage).
    let isAdmin = $derived(
        (roomState?.participants?.find(
            (p: { id: string }) => p.id === sessionData?.participantId
        )?.role ?? sessionData?.role) === 'admin'
    );
    let activeSpeakers = $derived(
        participants.filter((p: { id: string }) => speakingParticipants.has(p.id))
    );
    let isScreenShareDisabled = $derived(
        !screenShareActive && screenShareParticipantId !== null && screenShareParticipantId !== sessionData?.participantId
    );
    // B7: connection quality bucket from the existing RTT polling
    let connectionQuality = $derived(
        currentRtt === null ? null : currentRtt < 100 ? "good" : currentRtt < 300 ? "fair" : "poor"
    );
    // Map of participant id → display color, used to tint chat author names
    let participantColors = $derived(
        Object.fromEntries(
            participants.map((p: { id: string; color: string }) => [p.id, p.color])
        ) as Record<string, string>
    );

    // Move focus into the participant list when it opens (Esc closes it via
    // the global keyboard handler).
    $effect(() => {
        if (showParticipantList && participantListEl) {
            participantListEl.focus();
        }
    });

    // Bind screen share stream to video element when both are available.
    // Cleanup nulls srcObject so a stale stream doesn't hold decoder resources.
    $effect(() => {
        const el = screenShareVideoEl;
        const stream = screenShareStream;
        if (el && stream) {
            el.srcObject = stream;
            el.play().catch(err => {
                console.warn('Failed to autoplay screen share video:', err);
            });
            return () => {
                el.srcObject = null;
            };
        }
    });
</script>

<svelte:head>
    <title>{roomState?.name || "Session"} | Chromatic</title>
</svelte:head>

<svelte:window onkeydown={handleKeydown} onpointerdown={handleWindowPointerDown} />
<svelte:document onfullscreenchange={handleFullscreenChange} />

<main class="session-page" onmousemove={handleMouseMove} onpointerdown={handlePagePointerDown}>
    <div class="video-wrapper" class:split-active={screenShareStream}>
        {#if screenShareStream}
            <div class="split-screenshare">
                <video bind:this={screenShareVideoEl} autoplay playsinline muted>
                    <track kind="captions" />
                </video>
                <div class="split-screenshare-label">
                    <span>{screenShareParticipantName || "Screen"}'s screen</span>
                    {#if isAdmin || screenShareParticipantId === sessionData?.participantId}
                        <button class="split-screenshare-stop" onclick={stopScreenSharePip} title="Stop screen share">
                            <svg viewBox="0 0 24 24" fill="currentColor" width="14" height="14"><path d="M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z"/></svg>
                            Stop
                        </button>
                    {/if}
                </div>
            </div>
        {/if}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div class="video-container" onpointerdown={handleVideoPointerDown}>
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

        <!-- Status overlays: dim layer never captures input, only the inner
             buttons are interactive, and the controls layer stacks above. -->
        {#if needsPlayClick}
            <div class="stream-status-overlay" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <button class="play-btn" aria-label="Play stream" onclick={handlePlayClick}>
                        <svg viewBox="0 0 24 24" fill="currentColor" width="48" height="48"><path d="M8 5v14l11-7z"/></svg>
                    </button>
                    <p>Tap to play stream</p>
                </div>
            </div>
        {:else if streamError}
            <div class="stream-status-overlay error" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <div class="error-icon">!</div>
                    <p class="error-message">{streamError}</p>
                    <button class="btn btn-primary" onclick={() => window.location.reload()}>
                        Refresh Page
                    </button>
                </div>
            </div>
        {:else if streamPaused}
            <div class="stream-status-overlay" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <div class="paused-icon">
                        <svg viewBox="0 0 24 24" fill="currentColor" width="48" height="48"><path d="M6 19h4V5H6v14zm8-14v14h4V5h-4z"/></svg>
                    </div>
                    <p>Stream paused</p>
                    <p class="stream-subtext">Waiting for host to reconnect...</p>
                </div>
            </div>
        {:else if !hasStream}
            <div class="stream-status-overlay" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <h2 class="stream-card-title">{roomState?.name || "Session"}</h2>
                    <span class="pulse-dot" aria-hidden="true"></span>
                    <p>
                        {isLive
                            ? "Connecting to stream..."
                            : "The host hasn't started streaming yet — you'll connect automatically."}
                    </p>
                </div>
            </div>
        {/if}

        <!-- Top banners share one stack so they never overlap -->
        <div class="banner-stack">
            {#if pendingScreenShareRequest}
                <div class="screenshare-approval" transition:fly={{ y: 8, duration: 200 }}>
                    <span class="screenshare-approval-text">{pendingScreenShareRequest.name} wants to share their screen</span>
                    <div class="screenshare-approval-actions">
                        <button class="screenshare-approval-btn approve" onclick={approveScreenShare}>Approve</button>
                        <button class="screenshare-approval-btn deny" onclick={denyScreenShare}>Deny</button>
                    </div>
                </div>
            {/if}

            {#if micPromptState !== "hidden"}
                <div class="mic-prompt" class:success={micPromptState === "granted"} class:error={micPromptState === "denied"} role="status" aria-live="polite" transition:fly={{ y: 8, duration: 200 }}>
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
        </div>

        <!-- Controls overlay -->
        <div class="controls-overlay" class:visible={isControlsVisible}>
            <div class="top-bar">
                <div class="room-name">{roomState?.name || "Session"}</div>
                <div class="top-bar-right">
                    <button
                        class="participant-count"
                        onclick={toggleParticipantList}
                        class:active={showParticipantList}
                        aria-label="Participants ({participants.length})"
                        aria-expanded={showParticipantList}
                        aria-haspopup="dialog"
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="16" height="16"><path d="M16 11c1.66 0 2.99-1.34 2.99-3S17.66 5 16 5c-1.66 0-3 1.34-3 3s1.34 3 3 3zm-8 0c1.66 0 2.99-1.34 2.99-3S9.66 5 8 5C6.34 5 5 6.34 5 8s1.34 3 3 3zm0 2c-2.33 0-7 1.17-7 3.5V19h14v-2.5c0-2.33-4.67-3.5-7-3.5zm8 0c-.29 0-.62.02-.97.05 1.16.84 1.97 1.97 1.97 3.45V19h6v-2.5c0-2.33-4.67-3.5-7-3.5z"/></svg>
                        {participants.length}
                    </button>
                </div>
            </div>

            <div class="bottom-bar">
                <div class="bottom-left" aria-hidden="true"></div>

                <!-- Main control bar - large, obvious buttons with labels -->
                <div class="control-bar-anchor">
                {#if showVolumeControls}
                    <div
                        class="volume-popover"
                        bind:this={volumePopoverEl}
                        transition:fly={{ y: 8, duration: 200 }}
                        role="dialog"
                        aria-label="Volume controls"
                    >
                        <div class="volume-row">
                            <label for="program-volume">Program</label>
                            <input
                                id="program-volume"
                                class="range-input"
                                type="range"
                                min="0"
                                max="1"
                                step="0.05"
                                value={streamVolume}
                                oninput={handleStreamVolumeChange}
                            />
                        </div>
                        <div class="volume-row">
                            <label for="voice-volume">Voice chat</label>
                            <input
                                id="voice-volume"
                                class="range-input"
                                type="range"
                                min="0"
                                max="1"
                                step="0.05"
                                value={voiceVolume}
                                oninput={handleVoiceVolumeChange}
                            />
                        </div>
                        <button class="volume-mute-btn" onclick={toggleMute} aria-pressed={isMuted}>
                            {isMuted ? "Unmute program audio" : "Mute program audio"}
                        </button>
                    </div>
                {/if}
                <div class="control-bar">
                    <button
                        class="control-btn"
                        class:active={isMicEnabled}
                        class:off={!isMicEnabled}
                        onclick={toggleMic}
                        aria-pressed={isMicEnabled}
                        aria-label="Microphone (M)"
                        title="Toggle microphone (M)"
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
                        class:active={showVolumeControls}
                        onclick={toggleVolumeControls}
                        bind:this={soundBtnEl}
                        aria-label="Sound and volume"
                        aria-expanded={showVolumeControls}
                        aria-haspopup="dialog"
                        title="Sound and volume"
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
                        aria-pressed={isChatOpen}
                        aria-label="Chat (C)"
                        title="Toggle chat (C)"
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
                        title="Fixes a frozen or stuck picture"
                        aria-label="Reload stream"
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            <path d="M17.65 6.35C16.2 4.9 14.21 4 12 4c-4.42 0-7.99 3.58-7.99 8s3.57 8 7.99 8c3.73 0 6.84-2.55 7.73-6h-2.08c-.82 2.33-3.04 4-5.65 4-3.31 0-6-2.69-6-6s2.69-6 6-6c1.66 0 3.14.69 4.22 1.78L13 11h7V4l-2.35 2.35z"/>
                        </svg>
                        <span class="control-label">Reload stream</span>
                    </button>

                    <button
                        class="control-btn"
                        class:active={isLaserEnabled}
                        onclick={toggleLaser}
                        title="Toggle laser pointer mode (L)"
                        aria-pressed={isLaserEnabled}
                        aria-label="Laser pointer (L)"
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            <path d="M4 20h2l6-6-2-2-6 6v2zm7.4-7.4 2 2L18 10.01a1.41 1.41 0 0 0 0-2l-2.01-2.01a1.41 1.41 0 0 0-2 0L11.4 8.6zM19 14l-4 4 1 1a2.83 2.83 0 0 0 4 0 2.83 2.83 0 0 0 0-4l-1-1z"/>
                        </svg>
                        <span class="control-label">{isLaserEnabled ? "Laser On" : "Laser Off"}</span>
                    </button>

                    <button
                        class="control-btn"
                        class:active={screenShareActive}
                        class:requesting={screenShareRequested}
                        disabled={isScreenShareDisabled}
                        onclick={toggleScreenShare}
                        title={isScreenShareDisabled ? "Screen share in progress" : screenShareActive ? "Stop sharing" : "Share screen"}
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            <path d="M20 18c1.1 0 1.99-.9 1.99-2L22 6c0-1.11-.9-2-2-2H4c-1.11 0-2 .89-2 2v10c0 1.1.89 2 2 2H0v2h24v-2h-4zM4 6h16v10H4V6z"/>
                        </svg>
                        <span class="control-label">{screenShareActive ? "Stop Share" : screenShareRequested ? "Pending..." : "Share"}</span>
                    </button>

                    <button
                        class="control-btn"
                        onclick={toggleFullscreen}
                        aria-pressed={isFullscreen}
                        aria-label={isFullscreen ? "Exit fullscreen (F)" : "Fullscreen (F)"}
                        title="Toggle fullscreen (F)"
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            {#if isFullscreen}
                                <path d="M5 16h3v3h2v-5H5v2zm3-8H5v2h5V5H8v3zm6 11h2v-3h3v-2h-5v5zm2-11V5h-2v5h5V8h-3z"/>
                            {:else}
                                <path d="M7 14H5v5h5v-2H7v-3zm-2-4h2V7h3V5H5v5zm12 7h-3v2h5v-5h-2v3zM14 5v2h3v3h2V5h-5z"/>
                            {/if}
                        </svg>
                        <span class="control-label">{isFullscreen ? "Exit Full" : "Fullscreen"}</span>
                    </button>

                    <button
                        class="control-btn leave-btn"
                        onclick={leaveToRoomPage}
                        aria-label="Leave session"
                        title="Leave the session"
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            <path d="M17 7l-1.41 1.41L18.17 11H8v2h10.17l-2.58 2.58L17 17l5-5zM4 5h8V3H4c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h8v-2H4V5z"/>
                        </svg>
                        <span class="control-label">Leave</span>
                    </button>
                </div>
                </div>

                <div class="bottom-right">
                    {#if isLive}
                        <span class="live-pill"><span class="live-dot" aria-hidden="true"></span>Live</span>
                    {/if}
                    {#if connectionQuality && currentRtt !== null}
                        <div
                            class="signal-indicator {connectionQuality}"
                            role="img"
                            title="Connection: {connectionQuality} ({Math.round(currentRtt)}ms)"
                            aria-label="Connection: {connectionQuality} ({Math.round(currentRtt)}ms)"
                        >
                            <span class="signal-bar"></span>
                            <span class="signal-bar"></span>
                            <span class="signal-bar"></span>
                        </div>
                    {/if}
                    {#if isAdmin && currentRtt !== null}
                        <div class="latency-display" class:good={currentRtt < 100} class:warning={currentRtt >= 100 && currentRtt < 300} class:bad={currentRtt >= 300}>
                            ~{Math.round(currentRtt)}ms
                        </div>
                    {/if}
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
            <div
                class="participant-list"
                role="dialog"
                aria-label="Participants"
                tabindex="-1"
                bind:this={participantListEl}
                transition:fly={{ y: 8, duration: 200 }}
            >
                {#each participants as p (p.id)}
                    <div
                        class="participant-list-item"
                        class:speaking={speakingParticipants.has(p.id)}
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
                        {#if isAdmin && p.id !== sessionData?.participantId}
                            <span class="participant-actions">
                                <button
                                    class="participant-action"
                                    onclick={() => muteParticipant(p.id)}
                                    title="Mute {p.name}"
                                >Mute</button>
                                <button
                                    class="participant-action danger"
                                    onclick={() => (kickTarget = { id: p.id, name: p.name })}
                                    title="Remove {p.name} from the session"
                                >Remove</button>
                            </span>
                        {/if}
                    </div>
                {/each}
            </div>
        {/if}

        <!-- Full-screen end state (session ended / removed / terminated) -->
        {#if endState}
            <div class="end-state-overlay" transition:fade={{ duration: 150 }}>
                <div class="end-state-card" role="alertdialog" aria-labelledby="end-state-title">
                    <div class="end-state-icon" aria-hidden="true">
                        <svg viewBox="0 0 24 24" fill="currentColor" width="28" height="28">
                            <path d="M17 7l-1.41 1.41L18.17 11H8v2h10.17l-2.58 2.58L17 17l5-5zM4 5h8V3H4c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h8v-2H4V5z"/>
                        </svg>
                    </div>
                    <h2 id="end-state-title">{endState.title}</h2>
                    <p>{endState.body}</p>
                    <button class="btn btn-primary" onclick={leaveToRoomPage}>
                        Back to room page
                    </button>
                </div>
            </div>
        {/if}
    </div>

    <ChatPanel
        isOpen={isChatOpen}
        onClose={() => (isChatOpen = false)}
        roomSlug={slug}
        joinToken={sessionData?.token || ""}
        {participantColors}
        selfId={sessionData?.participantId || ""}
    />
    <BrowserToast />

    <ConfirmDialog
        open={kickTarget !== null}
        title="Remove participant"
        body={kickTarget ? `Remove ${kickTarget.name} from the session? They can rejoin from the room page.` : ""}
        confirmLabel="Remove"
        danger
        onConfirm={confirmKickParticipant}
        onCancel={() => (kickTarget = null)}
    />
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

    /* Dim layer never captures input (chat/mic stay reachable); only the
       inner buttons are interactive. Sits below the controls layer. */
    .stream-status-overlay {
        position: absolute;
        inset: 0;
        z-index: 15;
        pointer-events: none;
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        background: rgba(0, 0, 0, 0.65);
        color: var(--color-text-muted);
        gap: var(--space-md);
    }
    .stream-status-overlay .play-btn,
    .stream-status-overlay .btn {
        pointer-events: auto;
    }

    .stream-card {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: var(--space-md);
        max-width: 380px;
        padding: var(--space-xl);
        text-align: center;
        background: rgba(12, 12, 14, 0.72);
        border: 1px solid rgba(255, 255, 255, 0.08);
        border-radius: var(--radius-lg);
    }
    .stream-card p { margin: 0; }
    .stream-card-title {
        margin: 0;
        font-size: 1.125rem;
        font-weight: 600;
        color: var(--color-text);
    }
    .pulse-dot {
        width: 10px;
        height: 10px;
        border-radius: 50%;
        background: var(--color-text-muted);
        animation: pulse-dot 2s ease-in-out infinite;
    }
    @keyframes pulse-dot {
        0%, 100% { opacity: 0.3; transform: scale(0.85); }
        50% { opacity: 1; transform: scale(1); }
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

    /* All top banners share one stack so they never overlap */
    .banner-stack {
        position: absolute;
        top: 18px;
        left: 50%;
        transform: translateX(-50%);
        z-index: 25;
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: 8px;
        pointer-events: none;
    }
    .banner-stack > * {
        pointer-events: auto;
    }

    .mic-prompt {
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

    /* Controls overlay — stacks above the stream-status dim layer so chat,
       mic and the rest stay reachable while waiting for the stream. */
    .controls-overlay {
        position: absolute;
        inset: 0;
        z-index: 20;
        display: flex;
        flex-direction: column;
        justify-content: space-between;
        padding: var(--space-md) var(--space-lg);
        padding-bottom: calc(var(--space-md) + env(safe-area-inset-bottom, 0px));
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
        min-width: 280px;
        max-height: 300px;
        overflow-y: auto;
        z-index: 50;
        padding: var(--space-xs) 0;
    }

    .participant-list:focus {
        outline: none;
    }

    .participant-list-item {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: var(--space-sm) var(--space-md);
        width: 100%;
        color: var(--color-text);
        font-size: 0.8125rem;
        transition: background 0.1s ease;
        text-align: left;
    }
    .participant-list-item:hover { background: rgba(255,255,255,0.06); }
    .participant-list-item.speaking {
        background: rgba(72, 182, 166, 0.1);
    }

    /* Admin per-row actions, revealed on hover/focus */
    .participant-actions {
        display: flex;
        gap: var(--space-xs);
        opacity: 0;
        transition: opacity 0.12s ease;
    }
    .participant-list-item:hover .participant-actions,
    .participant-list-item:focus-within .participant-actions {
        opacity: 1;
    }
    .participant-action {
        background: rgba(255, 255, 255, 0.08);
        border: 1px solid rgba(255, 255, 255, 0.15);
        border-radius: var(--radius-sm);
        color: var(--color-text);
        font-size: var(--text-min);
        font-weight: 500;
        padding: 2px 8px;
        cursor: pointer;
        transition: background 0.12s ease, border-color 0.12s ease;
    }
    .participant-action:hover {
        background: rgba(255, 255, 255, 0.16);
        border-color: rgba(255, 255, 255, 0.3);
    }
    .participant-action.danger {
        color: var(--color-error);
        border-color: rgba(239, 68, 68, 0.4);
    }
    .participant-action.danger:hover {
        background: rgba(239, 68, 68, 0.15);
        border-color: rgba(239, 68, 68, 0.6);
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

    /* 3-column grid keeps the control bar truly centered */
    .bottom-bar {
        display: grid;
        grid-template-columns: 1fr auto 1fr;
        align-items: end;
        gap: var(--space-sm);
    }

    .bottom-right {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        justify-self: end;
    }

    .live-pill {
        display: inline-flex;
        align-items: center;
        gap: 6px;
        font-size: 0.75rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.06em;
        color: #fff;
        background: rgba(0, 0, 0, 0.6);
        padding: var(--space-xs) var(--space-sm);
        border-radius: var(--radius-full);
        border: 1px solid rgba(255, 255, 255, 0.15);
    }
    .live-dot {
        width: 8px;
        height: 8px;
        border-radius: 50%;
        background: var(--color-error);
        animation: pulse-speaking 1.6s infinite;
    }

    /* Discreet 3-bar connection quality indicator */
    .signal-indicator {
        display: flex;
        align-items: flex-end;
        gap: 2px;
        height: 14px;
        padding: var(--space-xs) var(--space-sm);
        box-sizing: content-box;
        background: rgba(0, 0, 0, 0.6);
        border-radius: var(--radius-sm);
    }
    .signal-bar {
        width: 3px;
        border-radius: 1px;
        background: rgba(255, 255, 255, 0.25);
    }
    .signal-bar:nth-child(1) { height: 6px; }
    .signal-bar:nth-child(2) { height: 10px; }
    .signal-bar:nth-child(3) { height: 14px; }
    .signal-indicator.good .signal-bar { background: var(--color-success); }
    .signal-indicator.fair .signal-bar:nth-child(-n + 2) { background: var(--color-warning); }
    .signal-indicator.poor .signal-bar:nth-child(1) { background: var(--color-error); }

    /* Anchor for the volume popover above the control bar */
    .control-bar-anchor {
        position: relative;
    }

    .volume-popover {
        position: absolute;
        bottom: calc(100% + 8px);
        left: 50%;
        width: 240px;
        margin-left: -120px;
        display: flex;
        flex-direction: column;
        gap: var(--space-md);
        padding: var(--space-md);
        background: rgba(0, 0, 0, 0.85);
        backdrop-filter: blur(12px);
        -webkit-backdrop-filter: blur(12px);
        border: 1px solid rgba(255, 255, 255, 0.12);
        border-radius: var(--radius-md);
        box-shadow: var(--shadow-lg);
        z-index: 30;
    }
    .volume-row {
        display: flex;
        flex-direction: column;
        gap: var(--space-xs);
    }
    .volume-row label {
        font-size: var(--text-min);
        font-weight: 500;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        color: var(--color-text-muted);
    }
    .volume-mute-btn {
        background: rgba(255, 255, 255, 0.08);
        border: 1px solid rgba(255, 255, 255, 0.15);
        border-radius: var(--radius-sm);
        color: var(--color-text);
        font-size: var(--text-meta);
        padding: 6px 10px;
        cursor: pointer;
        transition: background 0.12s ease;
    }
    .volume-mute-btn:hover { background: rgba(255, 255, 255, 0.16); }

    /* Full-screen end-state panel */
    .end-state-overlay {
        position: absolute;
        inset: 0;
        z-index: 60;
        display: flex;
        align-items: center;
        justify-content: center;
        background: rgba(0, 0, 0, 0.85);
        padding: var(--space-lg);
    }
    .end-state-card {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: var(--space-md);
        max-width: 400px;
        padding: var(--space-xl);
        text-align: center;
        background: var(--color-surface);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-lg);
        box-shadow: var(--shadow-lg);
    }
    .end-state-icon {
        width: 56px;
        height: 56px;
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        background: var(--color-neutral-bg);
        color: var(--color-text-muted);
    }
    .end-state-card h2 {
        margin: 0;
        font-size: 1.25rem;
        color: var(--color-text);
    }
    .end-state-card p {
        margin: 0;
        color: var(--color-text-muted);
        font-size: var(--text-body);
    }

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

    /* Neutral active state: saturated UI next to the picture contaminates
       color judgment, so "on" reads as brighter, not teal. */
    .control-btn.active {
        background: rgba(255, 255, 255, 0.22);
        border-color: rgba(255, 255, 255, 0.45);
        color: #fff;
    }

    .control-btn.off {
        background: rgba(239, 68, 68, 0.15);
        border-color: rgba(239, 68, 68, 0.4);
        color: var(--color-error);
    }

    .control-btn.leave-btn:hover {
        background: rgba(239, 68, 68, 0.18);
        border-color: rgba(239, 68, 68, 0.5);
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
        background: var(--color-primary);
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

    /* Split-screen layout for screen share */
    .video-wrapper.split-active {
        flex-direction: row;
        gap: 2px;
        align-items: stretch;
    }
    .video-wrapper.split-active .video-container {
        flex: 2;
        min-width: 0;
        width: auto;
    }
    .split-screenshare {
        flex: 3;
        min-width: 0;
        position: relative;
        background: #000;
        display: flex;
        align-items: center;
        justify-content: center;
        overflow: hidden;
    }
    .split-screenshare video {
        width: 100%;
        height: 100%;
        object-fit: contain;
        background: #000;
    }
    .split-screenshare-label {
        position: absolute;
        top: 8px;
        left: 8px;
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: 4px 10px;
        border-radius: var(--radius-sm);
        background: rgba(0, 0, 0, 0.7);
        color: #fff;
        font-size: 0.75rem;
        font-weight: 500;
        z-index: 5;
    }
    .split-screenshare-stop {
        display: flex;
        align-items: center;
        gap: 4px;
        padding: 2px 8px;
        border-radius: var(--radius-sm);
        background: rgba(239, 68, 68, 0.8);
        border: none;
        color: #fff;
        font-size: 0.7rem;
        cursor: pointer;
        transition: background 0.15s ease;
    }
    .split-screenshare-stop:hover {
        background: rgba(239, 68, 68, 1);
    }

    /* Screen share admin approval prompt */
    .screenshare-approval {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
        background: rgba(6, 18, 32, 0.92);
        border: 1px solid rgba(72, 182, 166, 0.35);
        color: #fff;
        box-shadow: var(--shadow-lg);
        pointer-events: auto;
    }
    .screenshare-approval-text {
        font-size: 0.875rem;
        font-weight: 500;
    }
    .screenshare-approval-actions {
        display: flex;
        gap: var(--space-xs);
    }
    .screenshare-approval-btn {
        border: none;
        border-radius: var(--radius-sm);
        padding: 6px 12px;
        font-size: 0.75rem;
        font-weight: 600;
        cursor: pointer;
        transition: filter 0.15s ease;
    }
    .screenshare-approval-btn:hover {
        filter: brightness(1.1);
    }
    .screenshare-approval-btn.approve {
        background: var(--color-primary);
        color: #041014;
    }
    .screenshare-approval-btn.deny {
        background: rgba(239, 68, 68, 0.8);
        color: #fff;
    }

    /* Screen share requesting state (pulsing) */
    .control-btn.requesting {
        animation: share-pulse 1.5s ease-in-out infinite;
        border-color: var(--color-warning);
        color: var(--color-warning);
    }
    @keyframes share-pulse {
        0%, 100% { opacity: 1; }
        50% { opacity: 0.5; }
    }
    .control-btn:disabled {
        opacity: 0.4;
        cursor: not-allowed;
    }

    @media (max-width: 768px) {
        .session-page { flex-direction: column; }
        .video-wrapper { flex: 1; min-height: 0; }
        .control-bar { gap: 4px; padding: var(--space-xs) var(--space-sm); }
        .control-btn { padding: 8px 10px; min-width: 52px; }
        .control-btn svg { width: 20px; height: 20px; }
        .control-label { font-size: 0.5625rem; }
        .active-speaker-indicator { bottom: 80px; }
        .video-wrapper.split-active { flex-direction: column; }
        .video-wrapper.split-active .video-container { flex: 1; }
        .split-screenshare { flex: 1; }
    }

    /* Icon-only controls on small phones so all buttons fit a 375px screen */
    @media (max-width: 480px) {
        .controls-overlay { padding-left: var(--space-sm); padding-right: var(--space-sm); }
        .control-label { display: none; }
        .control-bar { gap: 2px; }
        .control-btn {
            min-width: 44px;
            min-height: 44px;
            padding: 10px;
            justify-content: center;
        }
        .bottom-bar {
            display: flex;
            flex-direction: column;
            align-items: center;
            gap: var(--space-xs);
        }
        .bottom-left { display: none; }
    }
</style>
