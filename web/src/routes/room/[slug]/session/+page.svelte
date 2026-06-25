<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { fade, fly } from "svelte/transition";
    import { goto } from "$app/navigation";
    import { page } from "$app/stores";
    import { session } from "$lib/stores/session.svelte";
    import { rooms } from "$lib/api/client";
    import { chatStore, type ChatMessage } from "$lib/stores/chat.svelte";
    import { unlockAudio, getAudioContext, closeAudioContext } from "$lib/audio/context";
    import { WebRTCManager, getStoredMicDeviceId, storeMicDeviceId } from "$lib/webrtc/manager";
    import { deriveStreamOverlayState } from "$lib/video/stream-overlay";
    import { AudioDuckingManager } from "$lib/audio/ducking";
    import { playShareRequestChime, playWaitingRoomChime } from "$lib/audio/chimes";
    import LaserPointerOverlay from "$lib/components/LaserPointerOverlay.svelte";
    import ChatPanel from "$lib/components/ChatPanel.svelte";
    import ConfirmDialog from "$lib/components/ConfirmDialog.svelte";
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
    let kickTarget = $state<{ id: string; name: string } | null>(null);
    let endState = $state<{ title: string; body: string } | null>(null);
    let isFullscreen = $state(false);
    let soundBtnEl = $state<HTMLButtonElement | null>(null);
    let volumePopoverEl = $state<HTMLDivElement | null>(null);
    let participantListEl = $state<HTMLDivElement | null>(null);
    let currentRtt = $state<number | null>(null);
    let currentVideoBufferDelay = $state<number | null>(null);
    let statsInterval: ReturnType<typeof setInterval> | null = null;
    // Cloudflare TURN credentials default to a 1 h TTL; long color-grading
    // sessions (4–8 h) outlive that. Refresh every 30 min over the existing
    // WebSocket so any ICE restart later always has fresh creds to gather
    // with. The live media allocation is unaffected by the refresh.
    let iceServerRefreshInterval: ReturnType<typeof setInterval> | null = null;
    let streamVolume = $state(1.0);
    let voiceVolume = $state(1.0);
    let showVolumeControls = $state(false);
    let streamError = $state<string | null>(null);
    let needsPlayClick = $state(false); // Autoplay fallback
    let streamPaused = $state(false); // Stream temporarily disconnected
    // True once the video element fires 'playing' — the only reliable signal
    // that frames are rendering. play()'s promise can stay pending forever on
    // a stream waiting for a keyframe (BUG 1), so we never gate UI on it.
    let isVideoPlaying = $state(false);
    // Keyframe nudge: if tracks are bound but the video hasn't started
    // playing shortly after, request a resync (PLI). The server's single PLI
    // at subscriber-creation can be lost (sent before ICE finished), leaving
    // a reloading viewer stuck waiting for a decodable keyframe.
    let playNudgeTimer: ReturnType<typeof setTimeout> | null = null;
    let playNudgeAttempts = 0;
    const PLAY_NUDGE_MAX_ATTEMPTS = 3;
    const PLAY_NUDGE_INTERVAL_MS = 350;
    const MEDIA_STALL_GRACE_MS = 750;
    let mediaStallTimer: ReturnType<typeof setTimeout> | null = null;
    // Full re-subscription fallback: when ICE restart can't repair the media
    // path (dead TURN allocation, server-side subscriber gone), ask the server
    // for a brand-new subscriber instead of stranding the viewer.
    const RESUBSCRIBE_MAX_ATTEMPTS = 3;
    const CONNECTING_WATCHDOG_MS = 15000;
    let resubscribeAttempts = $state(0);
    let connectingWatchdog: ReturnType<typeof setTimeout> | null = null;
    let subscriptionRetryTimer: ReturnType<typeof setTimeout> | null = null;
    // Controls auto-hide (ITEM 3)
    let isPointerOverControls = $state(false);
    let controlsHaveFocus = $state(false);
    // Audio settings popover (ITEM 4)
    let showAudioSettings = $state(false);
    let audioSettingsBtnEl = $state<HTMLButtonElement | null>(null);
    let audioSettingsPopoverEl = $state<HTMLDivElement | null>(null);
    let audioInputs = $state<MediaDeviceInfo[]>([]);
    let audioOutputs = $state<MediaDeviceInfo[]>([]);
    let activeMicId = $state<string | null>(null);
    let selectedSpeakerId = $state<string | null>(null);
    let micSwitchPending = $state(false);
    const SPEAKER_DEVICE_STORAGE_KEY = "chromatic_speaker_device";
    const supportsSinkSelection =
        typeof HTMLMediaElement !== "undefined" &&
        "setSinkId" in HTMLMediaElement.prototype;
    // Chat notification cues (ITEM 5)
    let chatPulseActive = $state(false);
    let chatPulseTimer: ReturnType<typeof setTimeout> | null = null;
    let chatToast = $state<{ id: string; name: string; text: string } | null>(null);
    let chatToastTimer: ReturnType<typeof setTimeout> | null = null;
    const prefersReducedMotion =
        typeof window !== "undefined" &&
        window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    let isLaserEnabled = $state(false);
    let showParticipantList = $state(false);
    let speakingParticipants = $state<Set<string>>(new Set());
    // VAD analysers share the page's AudioContext; only the analyser/source
    // nodes are per-track. The context is torn down on page destroy.
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

    // Waiting-room approval stack (admins): cards persist until resolved by
    // ANY admin (waiting:resolved), never auto-dismissed.
    type WaitingRequest = { participantId: string; name: string; joinedAt: string };
    let waitingRequests = $state<WaitingRequest[]>([]);
    // Countdown-lobby headcount for the admin "open room now" banner
    let lobbyCount = $state(0);
    let openRoomPending = $state(false);
    let roomOpenedEarly = $state(false);
    // 1 Hz tick driving the subtle elapsed-time labels on approval cards;
    // only runs while cards are visible (see $effect below).
    let waitingNow = $state(Date.now());

    // Screen sharing state
    let screenShareRequested = $state(false);
    let screenShareActive = $state(false);
    let screenShareParticipantId = $state<string | null>(null);
    let screenShareParticipantName = $state<string | null>(null);
    let screenShareStream = $state<MediaStream | null>(null);
    let screenShareVideoEl = $state<HTMLVideoElement | null>(null);
    let pendingScreenShareRequest = $state<{participantId: string, name: string} | null>(null);
    // Share was approved; waiting for the user's click to open the OS picker
    // (getDisplayMedia must be called from a user gesture).
    let shareApprovedPrompt = $state(false);
    // Local self-preview of the sharer's own capture (BUG 4)
    let selfShareStream = $state<MediaStream | null>(null);

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
        // The join token is signed over the name used at join time; that name
        // is stored with the session payload. Prefer it over the global
        // localStorage name, which a later join (other room/tab) may have
        // overwritten — a mismatch made the WS upgrade fail on refresh and
        // the page hang on "the host hasn't started streaming yet".
        if (sessionData?.name) {
            participantName = sessionData.name;
        }

        // Fire-and-forget: without a user gesture (e.g. after F5) audio
        // unlock can't complete until the user interacts — it must NEVER
        // block the connection flow below (this await once hung the whole
        // session page on reload).
        void unlockAudio();

        // Clean up any stale WebRTC state from a previous mount (e.g. page refresh)
        cleanupWebRTC();

        // Clear any stale message handlers from a previous session
        session.clearHandlers();

        // Handle ICE servers from room state (initial delivery in room:state)
        session.onMessage("iceServers", (servers: unknown) => {
            iceServers = servers as RTCIceServer[];
            console.log('Received ICE servers:', iceServers);
            initializeWebRTC();
            // Start the periodic refresh only once we have a live manager.
            startICEServerRefresh();
        });

        // Handle refreshed ICE servers (periodic credential rotation)
        session.onMessage("signal:ice-servers", (payload: unknown) => {
            const data = payload as { iceServers?: RTCIceServer[] };
            if (!data?.iceServers) return;
            iceServers = data.iceServers;
            webrtcManager?.updateICEServers(data.iceServers);
        });

        // Handle WebRTC offer from server
        session.onMessage("signal:offer", async (payload: unknown) => {
            const data = payload as { sdp: string; offerId?: string };
            console.log('Received WebRTC offer');

            if (!webrtcManager) {
                initializeWebRTC();
            }

            if (webrtcManager) {
                try {
                    await webrtcManager.handleOffer(data.sdp, data.offerId);
                    initialOfferHandled = true;
                    tryEnablePendingAutoMic();
                } catch (err) {
                    console.error('Failed to handle offer:', err);
                }
            }
        });

        // Handle ICE candidates from server
        session.onMessage("signal:candidate", async (payload: unknown) => {
            const data = payload as { candidate: string; sdpMid?: string; sdpMLineIndex?: number; offerId?: string };
            if (webrtcManager) {
                try {
                    await webrtcManager.handleCandidate({
                        candidate: data.candidate,
                        sdpMid: data.sdpMid ?? null,
                        sdpMLineIndex: data.sdpMLineIndex ?? null
                    }, data.offerId);
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
                setSelfAudio(false);
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

        // Publisher PC answers (mic + screen share ride a dedicated
        // client-offers-only connection).
        session.onMessage("publish:answer", async (payload: unknown) => {
            const data = payload as { sdp: string; offerId?: string };
            try {
                if (webrtcManager) await webrtcManager.handlePublishAnswer(data.sdp, data.offerId);
            } catch (err) {
                console.error('Failed to handle publish answer:', err);
            }
        });

        session.onMessage("publish:error", (payload: unknown) => {
            const data = payload as { message?: string };
            console.error('Publisher negotiation failed server-side:', data?.message);
        });

        session.onMessage("signal:renegotiate", async (payload: unknown) => {
            const data = payload as { sdp: string; participantId?: string; offerId?: string };
            try {
                if (webrtcManager) await webrtcManager.handleRenegotiation(data.sdp, data.participantId, data.offerId);
            } catch (err) {
                console.error('Failed to handle renegotiation:', err);
            }
        });

        // ICE restart answers — route to handleVoiceAnswer which applies the
        // answer SDP to the same peer connection (same setRemoteDescription call).
        session.onMessage("signal:answer", async (payload: unknown) => {
            const data = payload as { sdp: string; offerId?: string };
            try {
                if (webrtcManager) await webrtcManager.handleVoiceAnswer(data.sdp, data.offerId);
            } catch (err) {
                console.error('Failed to handle answer:', err);
            }
        });

        // Screen sharing messages
        session.onMessage("screenshare:approved", () => {
            const wasRequested = screenShareRequested;
            screenShareRequested = false;
            // Approval can also arrive proactively (admin granting permanent
            // permission from the participant list). Only prompt when the
            // user actually asked to share right now.
            if (!wasRequested) return;
            // getDisplayMedia() requires transient activation (a fresh user
            // gesture). This handler runs from a websocket message, so calling
            // it here makes Chrome silently refuse and the picker never
            // appears. Show a button instead — the click is the gesture.
            shareApprovedPrompt = true;
        });

        session.onMessage("screenshare:denied", (payload: unknown) => {
            screenShareRequested = false;
            shareApprovedPrompt = false;
            const data = payload as { reason?: string };
            console.log('Screen share denied:', data.reason || 'Request denied by admin');
        });

        session.onMessage("screenshare:started", (payload: unknown) => {
            const data = payload as { participantId: string; name: string };
            screenShareParticipantId = data.participantId;
            screenShareParticipantName = data.name;
            pendingScreenShareRequest = null;
            // Someone else grabbed the share slot while our prompt was up.
            if (data.participantId !== sessionData?.participantId) {
                shareApprovedPrompt = false;
            }
        });

        session.onMessage("screenshare:stopped", () => {
            // If WE were the sharer (e.g. an admin stopped or revoked our
            // share), also stop the local capture so the browser's
            // "sharing this screen" indicator goes away.
            if (screenShareActive) {
                webrtcManager?.stopScreenShare();
            }
            screenShareActive = false;
            screenShareParticipantId = null;
            screenShareParticipantName = null;
            screenShareStream = null;
            selfShareStream = null;
        });

        session.onMessage("screenshare:pending", (payload: unknown) => {
            const data = payload as { participantId: string; name: string };
            pendingScreenShareRequest = data;
            playShareRequestChime();
        });

        // Waiting-room popups (admins only — the server only sends these to
        // admin clients). Cards stay until waiting:resolved arrives, so a
        // request resolved by another admin dismisses everywhere.
        session.onMessage("waiting:joined", (payload: unknown) => {
            const data = payload as WaitingRequest;
            if (!data?.participantId) return;
            if (waitingRequests.some((r) => r.participantId === data.participantId)) return;
            waitingRequests = [...waitingRequests, data];
            playWaitingRoomChime();
        });

        session.onMessage("waiting:list", (payload: unknown) => {
            const data = payload as { participants?: WaitingRequest[] };
            waitingRequests = data?.participants ?? [];
        });

        session.onMessage("waiting:resolved", (payload: unknown) => {
            const data = payload as { participantId: string; action: string };
            if (!data?.participantId) return;
            waitingRequests = waitingRequests.filter(
                (r) => r.participantId !== data.participantId
            );
        });

        // Countdown-lobby headcount (waiting-room-disabled scheduled rooms):
        // drives the admin "open room now" banner.
        session.onMessage("lobby:count", (payload: unknown) => {
            const data = payload as { count?: number };
            lobbyCount = data?.count ?? 0;
        });

        // Release per-participant audio graph (VAD + ducking) when someone
        // leaves the room — otherwise their analyser/source nodes and GainNode
        // stay connected to the shared AudioContext and the graph grows with churn.
        session.onMessage("participant:left", (payload: unknown) => {
            const data = payload as { participantId: string };
            if (data?.participantId) cleanupParticipantVoice(data.participantId);
        });

        // Chat handlers live here (always active) rather than in ChatPanel,
        // which is conditionally rendered — otherwise history/messages
        // arriving before the panel is first opened would be missed.
        session.onMessage("chat:history", (payload: unknown) => {
            const data = payload as { messages: ChatMessage[] };
            chatStore.loadHistory(data.messages ?? []);
        });

        session.onMessage("chat:message", (payload: unknown) => {
            const msg = payload as ChatMessage;
            chatStore.addMessage(msg);
            notifyChatMessage(msg);
        });

        // Admin moderation: a message was removed from history server-side.
        session.onMessage("chat:message-deleted", (payload: unknown) => {
            const data = payload as { id: string };
            if (data?.id) chatStore.removeMessage(data.id);
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
            selfShareStream = null;
            pendingScreenShareRequest = null;
            shareApprovedPrompt = false;
            streamPaused = false;
            streamError = null;
        });

        // Server-reported signaling failure (e.g. subscriber setup failed).
        // Surface it as a stream error so the user knows to refresh instead
        // of sitting on "Connecting…" forever.
        session.onMessage("signal:error", (payload: unknown) => {
            const data = payload as { code?: string; message?: string };
            console.error('Stream error from server:', data);
            // Subscriber setup can fail transiently (e.g. TURN hiccup while
            // building the server-side peer connection) — retry with backoff
            // before telling the user to refresh.
            if (data?.code === 'subscription-failed' && resubscribeAttempts < RESUBSCRIBE_MAX_ATTEMPTS) {
                const delay = 2000 * (resubscribeAttempts + 1);
                clearSubscriptionRetryTimer();
                subscriptionRetryTimer = setTimeout(() => {
                    subscriptionRetryTimer = null;
                    requestResubscribe('server subscription failure');
                }, delay);
                return;
            }
            streamError = data?.message || 'Something interrupted the stream.';
        });

        // Connect only after handlers are registered so early messages aren't dropped.
        session.connect(slug, sessionData!.token, participantName);

        window.addEventListener("chromatic:tampering", handleTampering);

        // Audio device plumbing (ITEM 4): restore the persisted speaker choice
        // and keep the device lists fresh when hardware is (un)plugged.
        try {
            selectedSpeakerId = localStorage.getItem(SPEAKER_DEVICE_STORAGE_KEY);
        } catch {
            selectedSpeakerId = null;
        }
        if (selectedSpeakerId && supportsSinkSelection) {
            void applySpeakerDevice(selectedSpeakerId);
        }
        navigator.mediaDevices?.addEventListener?.("devicechange", refreshAudioDevices);
        void refreshAudioDevices();

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
        navigator.mediaDevices?.removeEventListener?.("devicechange", refreshAudioDevices);
        clearMicPromptTimer();
        clearConnectingWatchdog();
        clearSubscriptionRetryTimer();
        if (controlsTimer) {
            clearTimeout(controlsTimer);
            controlsTimer = null;
        }
        if (chatPulseTimer) {
            clearTimeout(chatPulseTimer);
            chatPulseTimer = null;
        }
        if (chatToastTimer) {
            clearTimeout(chatToastTimer);
            chatToastTimer = null;
        }
    });

    function clearConnectingWatchdog() {
        if (connectingWatchdog) {
            clearTimeout(connectingWatchdog);
            connectingWatchdog = null;
        }
    }

    function clearSubscriptionRetryTimer() {
        if (subscriptionRetryTimer) {
            clearTimeout(subscriptionRetryTimer);
            subscriptionRetryTimer = null;
        }
    }

    function initializeWebRTC() {
        if (webrtcManager) return;

        webrtcManager = new WebRTCManager({
            iceServers,
            onTrack: handleTrack,
            onVoiceTrack: handleVoiceTrack,
            onScreenShareTrack: handleScreenShareTrack,
            sendSignal: (type, payload) => session.send(type, payload),
            onIceRestartFailed: () => {
                // ICE restart couldn't repair the path — rebuild the whole
                // subscription before declaring the stream unreachable.
                requestResubscribe('ice restart failed');
            },
            onNegotiationWedged: () => {
                requestResubscribe('local renegotiation wedged');
            },
            onScreenShareEnded: () => {
                screenShareActive = false;
                selfShareStream = null;
                session.send("screenshare:stop", {});
            }
        });

        console.log('WebRTC manager initialized');
        setSelfAudio(false);
        void startAutoMicConnection();
    }

    // Broadcasts the local mic state AND mirrors it into our own participant
    // entry — media:toggle broadcasts exclude the sender, so without this the
    // presence dots/list would show our own mic state stale.
    function setSelfAudio(enabled: boolean) {
        session.send("media:toggle", { audio: enabled });
        if (session.state.room && sessionData) {
            session.state.room.participants = session.state.room.participants.map(p =>
                p.id === sessionData!.participantId ? { ...p, audioEnabled: enabled } : p
            );
        }
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
            setSelfAudio(false);
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
        setSelfAudio(true);
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
            streamError = "The video player didn't load correctly.";
            return;
        }

        if (!event.streams || event.streams.length === 0 || !event.streams[0]) {
            streamError = "The stream isn't available right now — the host may need to restart it.";
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

            // Tracks have arrived and are bound — mark the stream present NOW.
            // BUG 1: this used to be set only inside play()'s promise
            // callbacks, but that promise never settles while the element is
            // waiting for a decodable keyframe (common right after a reload,
            // when there is no user gesture and the initial PLI was lost), so
            // viewers sat on the waiting overlay despite a fully negotiated
            // connection. Whether frames are rendering is tracked separately
            // via the element's 'playing' event (isVideoPlaying).
            hasStream = true;
            attemptAutoplay();

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
            streamError = "The stream couldn't be displayed.";
            hasStream = false;
        }
    }

    // Try to start playback without a user gesture. If the autoplay policy
    // rejects (most likely unmuted after a reload), retry muted; if even
    // muted playback is blocked, surface the tap-to-play card — never the
    // "waiting for the host" copy.
    function attemptAutoplay() {
        const el = videoElement;
        if (!el) return;
        // Track binding is the earliest reliable browser-side signal that the
        // media path exists. Ask for a keyframe immediately so late joiners
        // and reloads do not sit on a black/connecting frame for the first
        // nudge interval before the publisher is prodded.
        webrtcManager?.requestResync();
        el.play()
            .then(() => {
                needsPlayClick = false;
            })
            .catch(() => {
                if (!el.muted) {
                    isMuted = true;
                    el.muted = true;
                    el.play()
                        .then(() => {
                            needsPlayClick = false;
                        })
                        .catch(() => {
                            needsPlayClick = true;
                        });
                } else {
                    needsPlayClick = true;
                }
            });
        scheduleKeyframeNudge();
    }

    // The 'playing' event is the ground truth for "frames are rendering".
    function handleVideoPlaying() {
        isVideoPlaying = true;
        needsPlayClick = false;
        clearMediaStallTimer();
        clearKeyframeNudge();
        // Media is flowing again — future failures get a fresh retry budget.
        resubscribeAttempts = 0;
    }

    function handleVideoStalled() {
        if (!hasStream || needsPlayClick || streamPaused) return;

        // Ask for a keyframe immediately. If playback resumes quickly, the
        // grace timer is cleared by 'playing' and the user never sees a false
        // reconnect overlay; if it does not, fall back to the existing
        // keyframe-nudge -> resubscribe recovery path.
        webrtcManager?.requestResync();
        if (mediaStallTimer) return;
        mediaStallTimer = setTimeout(() => {
            mediaStallTimer = null;
            if (!hasStream || needsPlayClick || streamPaused) return;
            isVideoPlaying = false;
            scheduleKeyframeNudge();
        }, MEDIA_STALL_GRACE_MS);
    }

    function clearMediaStallTimer() {
        if (mediaStallTimer) {
            clearTimeout(mediaStallTimer);
            mediaStallTimer = null;
        }
    }

    // Ask the server for a brand-new subscriber (fresh offer/answer/ICE).
    // Used when the current peer connection is beyond repair: ICE restart
    // failed, keyframe nudges exhausted, or the viewer sat on "connecting"
    // past the watchdog. Bounded so a genuinely unreachable network ends in
    // a clear error instead of an infinite silent loop.
    function requestResubscribe(reason: string): boolean {
        if (resubscribeAttempts >= RESUBSCRIBE_MAX_ATTEMPTS) {
            streamError = "We couldn't reach the stream after several attempts.";
            return false;
        }
        resubscribeAttempts++;
        console.warn(`Requesting fresh subscription (${reason}, attempt ${resubscribeAttempts}/${RESUBSCRIBE_MAX_ATTEMPTS})`);
        clearSubscriptionRetryTimer();
        // Reset the keyframe-nudge cycle: without this the stale attempt
        // counter immediately re-escalates on its next tick and the client
        // tears down each fresh subscription ~2.5s after it connects.
        clearKeyframeNudge();
        // Give the replacement subscriber a fresh recovery window. Otherwise
        // a stall/connecting timer from the failed path can fire against the
        // new offer and trigger another resubscribe before it has a chance to
        // render.
        clearMediaStallTimer();
        clearConnectingWatchdog();
        // The frozen last frame must not read as "playing" — drop to the
        // connecting overlay until the new subscriber delivers frames.
        isVideoPlaying = false;
        streamError = null;
        // The server replies to resubscribe with fresh ICE servers immediately
        // before the replacement offer, preserving websocket message order.
        session.send("signal:resubscribe", {});
        return true;
    }

    // Watchdog for the "Connecting to the stream…" overlay: if the room is
    // live but no frames render within the window (lost offer, failed ICE
    // with no state transition, refresh during a TURN outage), escalate to a
    // full re-subscription instead of sitting there forever.
    $effect(() => {
        if (overlayState === 'connecting') {
            if (!connectingWatchdog) {
                connectingWatchdog = setTimeout(function tick() {
                    connectingWatchdog = null;
                    if (overlayState !== 'connecting') return;
                    if (requestResubscribe('stuck on connecting')) {
                        connectingWatchdog = setTimeout(tick, CONNECTING_WATCHDOG_MS);
                    }
                }, CONNECTING_WATCHDOG_MS);
            }
        } else if (connectingWatchdog) {
            clearConnectingWatchdog();
        }
    });

    // If the stream is bound but never starts rendering, the decoder is most
    // likely waiting on a keyframe that was lost in flight. attemptAutoplay()
    // sends the first resync immediately; this loop sends a few quick follow-
    // ups before escalating to a fresh subscriber.
    function scheduleKeyframeNudge() {
        if (playNudgeTimer) return;
        playNudgeAttempts = 0;
        const tick = () => {
            playNudgeTimer = null;
            if (isVideoPlaying || !webrtcManager) return;
            if (playNudgeAttempts >= PLAY_NUDGE_MAX_ATTEMPTS) {
                // Keyframes alone aren't fixing it — the transport itself is
                // likely dead. Escalate to a full re-subscription.
                requestResubscribe('keyframe nudges exhausted');
                return;
            }
            playNudgeAttempts++;
            console.log(`Video not rendering yet, requesting keyframe (attempt ${playNudgeAttempts}/${PLAY_NUDGE_MAX_ATTEMPTS})`);
            webrtcManager.requestResync();
            playNudgeTimer = setTimeout(tick, PLAY_NUDGE_INTERVAL_MS * playNudgeAttempts);
        };
        playNudgeTimer = setTimeout(tick, PLAY_NUDGE_INTERVAL_MS);
    }

    function clearKeyframeNudge() {
        if (playNudgeTimer) {
            clearTimeout(playNudgeTimer);
            playNudgeTimer = null;
        }
        playNudgeAttempts = 0;
    }

    function handlePlayClick() {
        if (videoElement) {
            videoElement.play().then(() => {
                needsPlayClick = false;
            }).catch(err => {
                console.warn('Play still blocked:', err);
                // Keep the play button visible so user can retry
            });
            // The tap was a real user gesture, so if frames still don't
            // render the problem is media, not autoplay policy — run the
            // keyframe-nudge → resubscribe escalation rather than leaving
            // the tap to silently do nothing.
            scheduleKeyframeNudge();
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
                currentVideoBufferDelay = stats.videoJitterBufferDelay ?? null;
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

    function startICEServerRefresh() {
        if (iceServerRefreshInterval) return;
        // 30 min — well under Cloudflare's 1 h TTL minus the 60 s skew the
        // server applies, so creds are always fresh when gathered.
        const THIRTY_MIN = 30 * 60 * 1000;
        iceServerRefreshInterval = setInterval(() => {
            session.send("signal:ice-servers-request", {});
        }, THIRTY_MIN);
    }

    function stopICEServerRefresh() {
        if (iceServerRefreshInterval) {
            clearInterval(iceServerRefreshInterval);
            iceServerRefreshInterval = null;
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

            // Replace any previous analyser for this participant (e.g. a
            // reconnect delivered a fresh track) so we don't leak graph nodes
            // and so the VAD reflects the new source.
            const existing = voiceAnalysers.get(participantId);
            if (existing) {
                try { existing.source.disconnect(); } catch { /* already gone */ }
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
            try {
                entry.source.disconnect();
            } catch {
                // Already disconnected or destroyed.
            }
            voiceAnalysers.delete(participantId);
        }
        pendingVoiceTracks.delete(participantId);
        audioDuckingManager?.removeVoiceTrack(participantId);
        // Stop the VAD loop when the last analyser is gone
        if (voiceAnalysers.size === 0 && vadFrame) {
            cancelAnimationFrame(vadFrame);
            vadFrame = null;
        }
        // Drop any lingering speaking indicator for the departed participant
        if (speakingParticipants.has(participantId)) {
            const next = new Set(speakingParticipants);
            next.delete(participantId);
            speakingParticipants = next;
        }
    }

    function toggleScreenShare() {
        if (screenShareActive) {
            // Stop sharing
            webrtcManager?.stopScreenShare();
            screenShareActive = false;
            selfShareStream = null;
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

    // Open the OS share picker. Must be called directly from a click handler —
    // getDisplayMedia needs the gesture's transient activation.
    async function startApprovedShare() {
        shareApprovedPrompt = false;
        const ok = await webrtcManager?.startScreenShare();
        if (ok) {
            screenShareActive = true;
            selfShareStream = webrtcManager?.getScreenShareStream() ?? null;
        } else {
            screenShareActive = false;
            selfShareStream = null;
        }
    }

    function dismissApprovedShare() {
        shareApprovedPrompt = false;
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

    // Waiting-room approval actions: same shared server logic as the REST
    // admit/deny endpoints. The card is removed by the waiting:resolved
    // broadcast, not optimistically, so every admin's stack stays in sync.
    function approveWaiting(participantId: string) {
        session.send("admin:waiting-approve", { participantId });
    }

    function denyWaiting(participantId: string) {
        session.send("admin:waiting-deny", { participantId });
    }

    // Open a scheduled room ahead of the countdown (admin banner)
    async function openRoomNow() {
        if (openRoomPending) return;
        openRoomPending = true;
        try {
            await rooms.open(slug);
            roomOpenedEarly = true;
            lobbyCount = 0;
        } catch (err) {
            console.error("Failed to open room early:", err);
        } finally {
            openRoomPending = false;
        }
    }

    function waitingElapsedLabel(joinedAt: string, nowMs: number): string {
        const seconds = Math.max(0, Math.floor((nowMs - Date.parse(joinedAt)) / 1000));
        if (seconds < 60) return `${seconds}s`;
        const minutes = Math.floor(seconds / 60);
        if (minutes < 60) return `${minutes}m`;
        return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
    }

    function stopScreenSharePip() {
        session.send("screenshare:stop", {});
        if (screenShareActive) {
            webrtcManager?.stopScreenShare();
            screenShareActive = false;
            selfShareStream = null;
        }
    }

    // Persistent screen share permission controls (admin, BUG 3)
    function allowParticipantShare(participantId: string) {
        session.send("admin:screenshare-approve", { participantId });
    }

    function revokeParticipantShare(participantId: string) {
        session.send("admin:screenshare-revoke", { participantId });
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
        stopICEServerRefresh();
        if (vadFrame) {
            cancelAnimationFrame(vadFrame);
            vadFrame = null;
        }
        // Disconnect voice analyser source nodes explicitly so they release
        // their MediaStream source and let the browser GC the tracks;
        // otherwise the graph stays wired until the shared AudioContext closes.
        for (const entry of voiceAnalysers.values()) {
            try {
                entry.source.disconnect();
            } catch {
                // Already disconnected.
            }
        }
        voiceAnalysers.clear();
        // Cancel any in-flight async analyser setups
        activeVoicePids.clear();
        pendingVoiceTracks.clear();
        speakingParticipants = new Set();
        if (webrtcManager) {
            webrtcManager.close();
            webrtcManager = null;
        }
        hasStream = false;
        isVideoPlaying = false;
        needsPlayClick = false;
        clearMediaStallTimer();
        clearKeyframeNudge();
        clearSubscriptionRetryTimer();
        currentRtt = null;
        currentVideoBufferDelay = null;
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

    // Controls auto-hide (ITEM 3): idle for CONTROLS_HIDE_DELAY_MS with the
    // cursor away from the bars fades them out fully; any pointer movement or
    // touch brings them back. They never hide while the cursor is over a bar,
    // a control has keyboard focus, a popover (volume / participants / audio
    // settings) is open, or chat is open.
    const CONTROLS_HIDE_DELAY_MS = 3000;

    function startControlsTimer() {
        if (controlsTimer) clearTimeout(controlsTimer);
        isControlsVisible = true;
        controlsTimer = setTimeout(() => {
            controlsTimer = null;
            if (!controlsPinned) {
                isControlsVisible = false;
            }
        }, CONTROLS_HIDE_DELAY_MS);
    }

    function handleMouseMove() {
        startControlsTimer();
    }

    function handleBarsPointerEnter() {
        isPointerOverControls = true;
    }

    function handleBarsPointerLeave() {
        isPointerOverControls = false;
    }

    // Keyboard focus on any control reveals (and pins) the bars — tab users
    // must never have the control they're on fade away under them.
    function handleControlsFocusIn() {
        controlsHaveFocus = true;
    }

    function handleControlsFocusOut(e: FocusEvent) {
        const container = e.currentTarget as HTMLElement;
        const next = e.relatedTarget as Node | null;
        if (!next || !container.contains(next)) {
            controlsHaveFocus = false;
        }
    }

    function toggleChat() {
        isChatOpen = !isChatOpen;
        if (isChatOpen) dismissChatToast();
    }

    // Chat notification cues (ITEM 5): pulse the chat button and show a
    // transient toast for messages that arrive while the panel is closed.
    function notifyChatMessage(msg: ChatMessage) {
        if (isChatOpen) return;
        if (msg.participantId === sessionData?.participantId) return;

        // Retrigger the pulse animation even when messages arrive back-to-back.
        if (!prefersReducedMotion) {
            chatPulseActive = false;
            requestAnimationFrame(() => {
                chatPulseActive = true;
            });
            if (chatPulseTimer) clearTimeout(chatPulseTimer);
            chatPulseTimer = setTimeout(() => {
                chatPulseActive = false;
                chatPulseTimer = null;
            }, 700);
        }

        const raw =
            msg.type === "file"
                ? `Sent a file: ${msg.file?.name ?? "file"}`
                : msg.content;
        const text = raw.length > 60 ? `${raw.slice(0, 60)}…` : raw;
        chatToast = { id: msg.id || crypto.randomUUID(), name: msg.participantName, text };
        if (chatToastTimer) clearTimeout(chatToastTimer);
        chatToastTimer = setTimeout(() => {
            chatToast = null;
            chatToastTimer = null;
        }, 3000);
    }

    function dismissChatToast() {
        chatToast = null;
        if (chatToastTimer) {
            clearTimeout(chatToastTimer);
            chatToastTimer = null;
        }
    }

    function openChatFromToast() {
        dismissChatToast();
        isChatOpen = true;
    }

    // Audio settings (ITEM 4)
    async function refreshAudioDevices() {
        try {
            const devices = await navigator.mediaDevices.enumerateDevices();
            audioInputs = devices.filter((d) => d.kind === "audioinput" && d.deviceId);
            audioOutputs = devices.filter((d) => d.kind === "audiooutput" && d.deviceId);
        } catch (err) {
            console.warn("Failed to enumerate audio devices:", err);
        }
    }

    async function toggleAudioSettings() {
        showAudioSettings = !showAudioSettings;
        if (showAudioSettings) {
            await refreshAudioDevices();
            // Reflect what's actually capturing (persisted choice may have
            // fallen back to the default if the device was unplugged).
            activeMicId =
                webrtcManager?.getCurrentMicDeviceId() ?? getStoredMicDeviceId();
        }
    }

    async function selectMicDevice(deviceId: string) {
        if (!webrtcManager || micSwitchPending || deviceId === activeMicId) return;
        micSwitchPending = true;
        try {
            const ok = await webrtcManager.setMicDevice(deviceId);
            if (!ok) {
                console.warn("Could not switch to the selected microphone");
                return;
            }
            // setMicDevice acquired the mic, so permission is granted even if
            // the auto-request flow hadn't completed yet.
            hasMicPermission = true;
            activeMicId = webrtcManager.getCurrentMicDeviceId() ?? deviceId;
            storeMicDeviceId(deviceId);
            // Labels become available after the first successful capture.
            await refreshAudioDevices();
        } finally {
            micSwitchPending = false;
        }
    }

    async function selectSpeakerDevice(deviceId: string) {
        selectedSpeakerId = deviceId;
        try {
            localStorage.setItem(SPEAKER_DEVICE_STORAGE_KEY, deviceId);
        } catch {
            // Storage unavailable — the in-session choice still applies.
        }
        await applySpeakerDevice(deviceId);
    }

    // Route program audio (the stream video element) and voice chat (played
    // through the shared AudioContext) to the chosen output. Only reachable
    // where setSinkId exists (Chromium); the section is hidden elsewhere.
    async function applySpeakerDevice(deviceId: string) {
        if (!supportsSinkSelection) return;
        try {
            if (videoElement && "setSinkId" in videoElement) {
                await (
                    videoElement as HTMLMediaElement & {
                        setSinkId(id: string): Promise<void>;
                    }
                ).setSinkId(deviceId);
            }
            const ctx = await getAudioContext();
            const sinkCtx = ctx as AudioContext & {
                setSinkId?: (id: string) => Promise<void>;
            };
            if (typeof sinkCtx.setSinkId === "function") {
                await sinkCtx.setSinkId(deviceId);
            }
        } catch (err) {
            console.warn("Failed to apply speaker device:", err);
        }
    }

    // Request mic permission from the settings popover so device labels
    // populate (browsers hide labels until a capture has been granted).
    async function requestMicForLabels() {
        await retryMicConnection();
        await refreshAudioDevices();
        activeMicId = webrtcManager?.getCurrentMicDeviceId() ?? getStoredMicDeviceId();
    }

    function toggleParticipantList() {
        showParticipantList = !showParticipantList;
    }

    function handleResync() {
        webrtcManager?.requestResync();
        // Explicit user action: don't just ask for a keyframe, rebuild the
        // subscription with a fresh retry budget. This is what "Reload
        // stream" should mean when the transport is wedged.
        resubscribeAttempts = 0;
        requestResubscribe('user reload');
    }

    function toggleLaser() {
        isLaserEnabled = !isLaserEnabled;
    }

    // Safari shows native hover media controls over the share video and a
    // click (e.g. using the laser) pauses it — a paused live stream is never
    // meaningful, so resume immediately.
    function handleSharePause() {
        screenShareVideoEl?.play().catch(() => {});
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
                setSelfAudio(true);
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
        setSelfAudio(isMicEnabled);
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
                if (showAudioSettings) {
                    showAudioSettings = false;
                } else if (showVolumeControls) {
                    showVolumeControls = false;
                } else if (showParticipantList) {
                    showParticipantList = false;
                } else if (isChatOpen) {
                    isChatOpen = false;
                }
                break;
        }
    }

    // Close the volume / audio-settings popovers on click/tap outside them.
    function handleWindowPointerDown(e: PointerEvent) {
        const target = e.target as Node;
        if (showVolumeControls) {
            if (!volumePopoverEl?.contains(target) && !soundBtnEl?.contains(target)) {
                showVolumeControls = false;
            }
        }
        if (showAudioSettings) {
            if (
                !audioSettingsPopoverEl?.contains(target) &&
                !audioSettingsBtnEl?.contains(target)
            ) {
                showAudioSettings = false;
            }
        }
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
            if (controlsTimer) {
                clearTimeout(controlsTimer);
                controlsTimer = null;
            }
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
    let displayedLatency = $derived(currentVideoBufferDelay ?? currentRtt);
    let displayedLatencySource = $derived(currentVideoBufferDelay === null ? "Network RTT" : "Video buffer");
    let displayedLatencyTitle = $derived(
        currentVideoBufferDelay === null
            ? `Network RTT: ${Math.round(currentRtt ?? 0)}ms`
            : `Video buffer: ${Math.round(currentVideoBufferDelay)}ms${currentRtt === null ? "" : `; network RTT: ${Math.round(currentRtt)}ms`}`
    );
    let displayedLatencyQuality = $derived(
        displayedLatency === null ? null : displayedLatency < 100 ? "good" : displayedLatency < 300 ? "fair" : "poor"
    );
    // Surface WS connection trouble instead of leaving the misleading
    // "host hasn't started streaming" copy up forever (BUG 1 UX).
    let isReconnecting = $derived(session.state.reconnecting && !session.state.connected);
    let connectionLost = $derived(
        !session.state.connected && !session.state.reconnecting && session.state.error !== null
    );
    // Map of participant id → display color, used to tint chat author names
    let participantColors = $derived(
        Object.fromEntries(
            participants.map((p: { id: string; color: string }) => [p.id, p.color])
        ) as Record<string, string>
    );

    // Explicit overlay state machine (BUG 1). All inputs feed one pure,
    // tested function; "live" derives from room status OR track arrival so a
    // reloading/late-joining viewer is never stuck on the waiting copy.
    let overlayState = $derived(
        deriveStreamOverlayState({
            streamError,
            connectionLost,
            reconnecting: isReconnecting,
            needsPlayClick,
            streamPaused,
            roomLive: isLive,
            hasStream,
            isVideoPlaying
        })
    );

    // Admin "open room now" banner: scheduled room, not opened early yet,
    // stream not live, and guests waiting on the countdown.
    let showOpenEarlyBanner = $derived(
        isAdmin &&
            !roomOpenedEarly &&
            lobbyCount > 0 &&
            Boolean(roomState?.scheduledAt) &&
            !roomState?.openedAt &&
            !isLive
    );

    // Tick the approval-card elapsed labels only while cards are on screen.
    $effect(() => {
        if (isAdmin && waitingRequests.length > 0) {
            waitingNow = Date.now();
            const tick = setInterval(() => {
                waitingNow = Date.now();
            }, 1000);
            return () => clearInterval(tick);
        }
    });

    // Controls stay visible while any of these hold (ITEM 3).
    let controlsPinned = $derived(
        isPointerOverControls ||
            controlsHaveFocus ||
            showVolumeControls ||
            showParticipantList ||
            showAudioSettings ||
            isChatOpen
    );

    // While pinned, cancel the hide countdown; when unpinned, restart it.
    $effect(() => {
        if (controlsPinned) {
            if (controlsTimer) {
                clearTimeout(controlsTimer);
                controlsTimer = null;
            }
            isControlsVisible = true;
        } else {
            startControlsTimer();
        }
    });

    // Device labels are empty until the browser has granted a capture —
    // used to show the permission hint in the audio settings popover.
    let micLabelsAvailable = $derived(audioInputs.some((d) => d.label !== ""));

    // Move focus into the participant list when it opens (Esc closes it via
    // the global keyboard handler).
    $effect(() => {
        if (showParticipantList && participantListEl) {
            participantListEl.focus();
        }
    });

    // Bind the split-pane share video: a remote participant's relay stream,
    // or the sharer's own capture (so the sharer gets the same layout as
    // everyone else). Cleanup nulls srcObject so a stale stream doesn't hold
    // decoder resources.
    $effect(() => {
        const el = screenShareVideoEl;
        const stream = screenShareStream ?? selfShareStream;
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
    <div class="video-wrapper" class:split-active={screenShareStream || selfShareStream}>
        {#if screenShareStream || selfShareStream}
            <!-- The sharer sees their own capture in the same split position
                 as everyone else, so the room shares one layout. -->
            <div class="split-screenshare">
                <!-- svelte-ignore a11y_media_has_caption -->
                <video
                    bind:this={screenShareVideoEl}
                    autoplay
                    playsinline
                    muted
                    disablepictureinpicture
                    controlslist="nodownload nofullscreen noremoteplayback"
                    onpause={handleSharePause}
                ></video>

                {#if screenShareVideoEl}
                    <LaserPointerOverlay videoElement={screenShareVideoEl} enabled={isLaserEnabled} surface="share" />
                {/if}

                <div class="split-screenshare-label">
                    {#if screenShareStream}
                        <span>{screenShareParticipantName || "Screen"}'s screen</span>
                    {:else}
                        <span><span class="self-share-dot" aria-hidden="true"></span> You're sharing</span>
                    {/if}
                    {#if !screenShareStream || isAdmin || screenShareParticipantId === sessionData?.participantId}
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
            <video
                bind:this={videoElement}
                autoplay
                playsinline
                muted={isMuted}
                onplaying={handleVideoPlaying}
                onwaiting={handleVideoStalled}
                onstalled={handleVideoStalled}
            >
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
                    posX={roomState.watermarkPosX ?? null}
                    posY={roomState.watermarkPosY ?? null}
                    scale={roomState.watermarkScale ?? 1}
                    {participantName}
                    roomName={roomState?.name || ""}
                />
            {/if}
        </div>

        <!-- Status overlays: dim layer never captures input, only the inner
             buttons are interactive, and the controls layer stacks above. -->
        {#if overlayState === 'needs-click'}
            <div class="stream-status-overlay" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <button class="play-btn" aria-label="Start watching" onclick={handlePlayClick}>
                        <svg viewBox="0 0 24 24" fill="currentColor" width="44" height="44"><path d="M8 5v14l11-7z"/></svg>
                    </button>
                    <h2 class="stream-card-title">Tap to start watching</h2>
                    <p class="stream-card-body">The stream is ready — your browser just needs one tap to begin playback.</p>
                </div>
            </div>
        {:else if overlayState === 'error'}
            <div class="stream-status-overlay" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <div class="stream-card-icon error" aria-hidden="true">
                        <svg viewBox="0 0 24 24" fill="currentColor" width="22" height="22"><path d="M11 7h2v6h-2zM11 15h2v2h-2z"/><path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 18c-4.41 0-8-3.59-8-8s3.59-8 8-8 8 3.59 8 8-3.59 8-8 8z"/></svg>
                    </div>
                    <h2 class="stream-card-title">Stream unavailable</h2>
                    <p class="stream-card-body">{streamError}</p>
                    <button class="btn btn-primary" onclick={() => window.location.reload()}>
                        Refresh page
                    </button>
                </div>
            </div>
        {:else if overlayState === 'connection-lost'}
            <div class="stream-status-overlay" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <div class="stream-card-icon error" aria-hidden="true">
                        <svg viewBox="0 0 24 24" fill="currentColor" width="22" height="22"><path d="M11 7h2v6h-2zM11 15h2v2h-2z"/><path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 18c-4.41 0-8-3.59-8-8s3.59-8 8-8 8 3.59 8 8-3.59 8-8 8z"/></svg>
                    </div>
                    <h2 class="stream-card-title">Connection lost</h2>
                    <p class="stream-card-body">We couldn't reach the session. Refresh the page to reconnect.</p>
                    <button class="btn btn-primary" onclick={() => window.location.reload()}>
                        Refresh page
                    </button>
                </div>
            </div>
        {:else if overlayState === 'reconnecting'}
            <div class="stream-status-overlay" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <div class="connect-dots" aria-hidden="true"><span></span><span></span><span></span></div>
                    <h2 class="stream-card-title">Reconnecting</h2>
                    <p class="stream-card-body">Restoring your connection to the session…</p>
                    <p class="stream-card-meta">Attempt {session.state.reconnectAttempt}</p>
                </div>
            </div>
        {:else if overlayState === 'paused'}
            <div class="stream-status-overlay" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <div class="stream-card-icon" aria-hidden="true">
                        <svg viewBox="0 0 24 24" fill="currentColor" width="22" height="22"><path d="M6 19h4V5H6v14zm8-14v14h4V5h-4z"/></svg>
                    </div>
                    <h2 class="stream-card-title">Stream paused</h2>
                    <p class="stream-card-body">The host's connection was interrupted — the stream will resume automatically.</p>
                </div>
            </div>
        {:else if overlayState === 'waiting' || overlayState === 'connecting'}
            <!-- Branded connecting scrim: crossfades its copy as the state
                 machine moves waiting → connecting, then fades out over
                 ~400ms once frames are rendering. -->
            <div
                class="stream-status-overlay connect-scrim"
                in:fade={{ duration: 150 }}
                out:fade={{ duration: prefersReducedMotion ? 0 : 400 }}
            >
                <div class="connect-stage">
                    <div class="wordmark">Chromatic</div>
                    <h2 class="connect-room">{roomState?.name || "Session"}</h2>
                    <div class="connect-dots" aria-hidden="true"><span></span><span></span><span></span></div>
                    <div class="connect-copy" aria-live="polite">
                        {#key overlayState}
                            <p transition:fade={{ duration: prefersReducedMotion ? 0 : 200 }}>
                                {overlayState === 'connecting'
                                    ? "Connecting to the stream…"
                                    : "Waiting for the host to start streaming"}
                            </p>
                        {/key}
                    </div>
                </div>
            </div>
        {/if}

        <!-- Top banners share one stack so they never overlap -->
        <div class="banner-stack">
            {#if showOpenEarlyBanner}
                <div class="open-early-banner" transition:fly={{ y: 8, duration: prefersReducedMotion ? 0 : 200 }}>
                    <span class="open-early-text">
                        {lobbyCount === 1 ? "1 guest is" : `${lobbyCount} guests are`} waiting on the countdown
                    </span>
                    <button class="open-early-btn" onclick={openRoomNow} disabled={openRoomPending}>
                        {openRoomPending ? "Opening…" : "Open room now"}
                    </button>
                </div>
            {/if}

            {#if pendingScreenShareRequest}
                <div class="screenshare-approval" transition:fly={{ y: 8, duration: 200 }}>
                    <span class="screenshare-approval-text">{pendingScreenShareRequest.name} wants to share their screen</span>
                    <div class="screenshare-approval-actions">
                        <button class="screenshare-approval-btn approve" onclick={approveScreenShare}>Approve</button>
                        <button class="screenshare-approval-btn deny" onclick={denyScreenShare}>Deny</button>
                    </div>
                </div>
            {/if}

            {#if shareApprovedPrompt}
                <div class="screenshare-approval" transition:fly={{ y: 8, duration: 200 }}>
                    <span class="screenshare-approval-text">Share approved — choose what to share</span>
                    <div class="screenshare-approval-actions">
                        <button class="screenshare-approval-btn approve" onclick={startApprovedShare}>Start sharing</button>
                        <button class="screenshare-approval-btn deny" onclick={dismissApprovedShare}>Cancel</button>
                    </div>
                </div>
            {/if}

            {#if micPromptState !== "hidden"}
                <div class="mic-prompt" class:success={micPromptState === "granted"} class:error={micPromptState === "denied"} role="status" aria-live="polite" transition:fly={{ y: 8, duration: 200 }}>
                    {#if micPromptState === "requesting"}
                        <div class="mic-spinner" aria-hidden="true"></div>
                        <div class="mic-prompt-copy">
                            <p class="mic-prompt-title">Connecting your microphone…</p>
                            <p class="mic-prompt-text">Allow microphone access in your browser to talk with the room.</p>
                        </div>
                    {:else if micPromptState === "granted"}
                        <div class="mic-prompt-copy">
                            <p class="mic-prompt-title">Microphone connected</p>
                        </div>
                    {:else}
                        <div class="mic-prompt-copy">
                            <p class="mic-prompt-title">Microphone is blocked</p>
                            <p class="mic-prompt-text">You can watch and listen — enable your mic anytime to speak.</p>
                        </div>
                        <div class="mic-prompt-actions">
                            <button class="mic-prompt-btn primary" onclick={retryMicConnection}>Enable microphone</button>
                            <button class="mic-prompt-btn" onclick={dismissMicPrompt}>Continue muted</button>
                        </div>
                    {/if}
                </div>
            {/if}
        </div>

        <!-- Waiting-room approval stack (admins): persistent cards, top-right
             below the top bar. Resolved only via waiting:resolved — no
             auto-dismiss; scrolls past four pending requests. -->
        {#if isAdmin && waitingRequests.length > 0}
            <div class="waiting-stack" role="region" aria-label="Waiting room requests">
                {#each waitingRequests as request (request.participantId)}
                    <div
                        class="waiting-request-card"
                        class:pulse={!prefersReducedMotion}
                        in:fly={{ y: 8, duration: prefersReducedMotion ? 0 : 200 }}
                        out:fade={{ duration: prefersReducedMotion ? 0 : 150 }}
                    >
                        <span class="waiting-avatar" aria-hidden="true">
                            {request.name.charAt(0).toUpperCase()}
                        </span>
                        <div class="waiting-request-copy">
                            <span class="waiting-request-name">{request.name}</span>
                            <span class="waiting-request-meta">
                                wants to join
                                <span class="waiting-request-elapsed">· {waitingElapsedLabel(request.joinedAt, waitingNow)}</span>
                            </span>
                        </div>
                        <div class="waiting-request-actions">
                            <button
                                class="waiting-request-btn approve"
                                onclick={() => approveWaiting(request.participantId)}
                            >Approve</button>
                            <button
                                class="waiting-request-btn deny"
                                onclick={() => denyWaiting(request.participantId)}
                            >Deny</button>
                        </div>
                    </div>
                {/each}
            </div>
        {/if}

        <!-- Controls overlay -->
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
            class="controls-overlay"
            class:visible={isControlsVisible}
            onfocusin={handleControlsFocusIn}
            onfocusout={handleControlsFocusOut}
        >
            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <div
                class="top-bar"
                onpointerenter={handleBarsPointerEnter}
                onpointerleave={handleBarsPointerLeave}
            >
                <div class="room-name">{roomState?.name || "Session"}</div>
                <div class="top-bar-right">
                    <!-- Compact presence row: one dot per participant, ring
                         glows in their color while speaking, slash = muted -->
                    {#if participants.length > 1}
                        <div class="presence-row" aria-hidden="true">
                            {#each participants.slice(0, 8) as p (p.id)}
                                <span
                                    class="presence-dot"
                                    class:speaking={speakingParticipants.has(p.id)}
                                    class:muted={!p.audioEnabled}
                                    style="--participant-color: {p.color}"
                                    title="{p.name}{p.audioEnabled ? '' : ' (muted)'}"
                                >{p.name.charAt(0).toUpperCase()}</span>
                            {/each}
                            {#if participants.length > 8}
                                <span class="presence-overflow">+{participants.length - 8}</span>
                            {/if}
                        </div>
                    {/if}
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

            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <div
                class="bottom-bar"
                onpointerenter={handleBarsPointerEnter}
                onpointerleave={handleBarsPointerLeave}
            >
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
                {#if showAudioSettings}
                    <div
                        class="audio-settings-popover"
                        bind:this={audioSettingsPopoverEl}
                        transition:fly={{ y: 8, duration: 200 }}
                        role="dialog"
                        aria-label="Audio settings"
                    >
                        <div class="audio-settings-section">
                            <span class="audio-settings-title">Microphone</span>
                            {#if !micLabelsAvailable}
                                <p class="audio-settings-hint">
                                    Allow microphone access to see and choose your input devices.
                                </p>
                                <button class="audio-settings-grant" onclick={requestMicForLabels}>
                                    Enable microphone
                                </button>
                            {/if}
                            {#if audioInputs.length === 0 && micLabelsAvailable}
                                <p class="audio-settings-hint">No microphones found.</p>
                            {/if}
                            {#each audioInputs as device (device.deviceId)}
                                <button
                                    class="audio-device-option"
                                    class:selected={device.deviceId === activeMicId}
                                    disabled={micSwitchPending}
                                    onclick={() => selectMicDevice(device.deviceId)}
                                    aria-pressed={device.deviceId === activeMicId}
                                >
                                    <span class="audio-device-check" aria-hidden="true">
                                        {#if device.deviceId === activeMicId}
                                            <svg viewBox="0 0 24 24" fill="currentColor" width="12" height="12"><path d="M9 16.17L4.83 12l-1.42 1.41L9 19 21 7l-1.41-1.41z"/></svg>
                                        {/if}
                                    </span>
                                    <span class="audio-device-label">{device.label || "Microphone"}</span>
                                </button>
                            {/each}
                        </div>
                        {#if supportsSinkSelection && audioOutputs.length > 0}
                            <div class="audio-settings-section">
                                <span class="audio-settings-title">Speaker</span>
                                {#each audioOutputs as device (device.deviceId)}
                                    <button
                                        class="audio-device-option"
                                        class:selected={device.deviceId === (selectedSpeakerId ?? "default")}
                                        onclick={() => selectSpeakerDevice(device.deviceId)}
                                        aria-pressed={device.deviceId === (selectedSpeakerId ?? "default")}
                                    >
                                        <span class="audio-device-check" aria-hidden="true">
                                            {#if device.deviceId === (selectedSpeakerId ?? "default")}
                                                <svg viewBox="0 0 24 24" fill="currentColor" width="12" height="12"><path d="M9 16.17L4.83 12l-1.42 1.41L9 19 21 7l-1.41-1.41z"/></svg>
                                            {/if}
                                        </span>
                                        <span class="audio-device-label">{device.label || "Speaker"}</span>
                                    </button>
                                {/each}
                            </div>
                        {/if}
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
                        class:active={showAudioSettings}
                        onclick={toggleAudioSettings}
                        bind:this={audioSettingsBtnEl}
                        aria-label="Audio settings"
                        aria-expanded={showAudioSettings}
                        aria-haspopup="dialog"
                        title="Audio settings (input device)"
                    >
                        <svg viewBox="0 0 24 24" fill="currentColor" width="24" height="24">
                            <path d="M19.14 12.94c.04-.3.06-.61.06-.94 0-.32-.02-.64-.07-.94l2.03-1.58c.18-.14.23-.41.12-.61l-1.92-3.32c-.12-.22-.37-.29-.59-.22l-2.39.96c-.5-.38-1.03-.7-1.62-.94l-.36-2.54c-.04-.24-.24-.41-.48-.41h-3.84c-.24 0-.43.17-.47.41l-.36 2.54c-.59.24-1.13.57-1.62.94l-2.39-.96c-.22-.08-.47 0-.59.22L2.74 8.87c-.12.21-.08.47.12.61l2.03 1.58c-.05.3-.09.63-.09.94s.02.64.07.94l-2.03 1.58c-.18.14-.23.41-.12.61l1.92 3.32c.12.22.37.29.59.22l2.39-.96c.5.38 1.03.7 1.62.94l.36 2.54c.05.24.24.41.48.41h3.84c.24 0 .44-.17.47-.41l.36-2.54c.59-.24 1.13-.56 1.62-.94l2.39.96c.22.08.47 0 .59-.22l1.92-3.32c.12-.22.07-.47-.12-.61l-2.01-1.58zM12 15.6c-1.98 0-3.6-1.62-3.6-3.6s1.62-3.6 3.6-3.6 3.6 1.62 3.6 3.6-1.62 3.6-3.6 3.6z"/>
                        </svg>
                        <span class="control-label">Audio</span>
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
                        class:pulse={chatPulseActive}
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
                    {#if isAdmin && displayedLatency !== null}
                        <div
                            class="latency-display"
                            class:good={displayedLatencyQuality === "good"}
                            class:warning={displayedLatencyQuality === "fair"}
                            class:bad={displayedLatencyQuality === "poor"}
                            title={displayedLatencyTitle}
                            aria-label="{displayedLatencySource}: {Math.round(displayedLatency)}ms"
                        >
                            ~{Math.round(displayedLatency)}ms
                        </div>
                    {/if}
                </div>
            </div>
        </div>

        <!-- Transient chat toast (ITEM 5): shows incoming messages while the
             chat panel is closed; clicking it opens chat. -->
        {#if chatToast}
            {#key chatToast.id}
                <button
                    class="chat-toast"
                    transition:fade={{ duration: prefersReducedMotion ? 0 : 200 }}
                    onclick={openChatFromToast}
                    title="Open chat"
                >
                    <span class="chat-toast-name">{chatToast.name}</span>
                    <span class="chat-toast-text">{chatToast.text}</span>
                </button>
            {/key}
        {/if}

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
                        style="--participant-color: {p.color}"
                    >
                        <span
                            class="participant-list-avatar"
                            class:speaking={speakingParticipants.has(p.id)}
                            style="background-color: {p.color}"
                        >
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
                                <svg viewBox="0 0 24 24" fill="currentColor" width="14" height="14"><path d="M12 14c1.66 0 2.99-1.34 2.99-3L15 5c0-1.66-1.34-3-3-3S9 3.34 9 5v6c0 1.66 1.34 3 3 3zm5.3-3c0 3-2.54 5.1-5.3 5.1S6.7 14 6.7 11H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c3.28-.48 6-3.3 6-6.72h-1.7z"/></svg>
                            </span>
                        {:else}
                            <span class="mic-muted-indicator" title="Mic muted">
                                <svg viewBox="0 0 24 24" fill="currentColor" width="14" height="14"><path d="M19 11h-1.7c0 .74-.16 1.43-.43 2.05l1.23 1.23c.56-.98.9-2.09.9-3.28zm-4.02.17c0-.06.02-.11.02-.17V5c0-1.66-1.34-3-3-3S9 3.34 9 5v.18l5.98 5.99zM4.27 3L3 4.27l6.01 6.01V11c0 1.66 1.33 3 2.99 3 .22 0 .44-.03.65-.08l1.66 1.66c-.71.33-1.5.52-2.31.52-2.76 0-5.3-2.1-5.3-5.1H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c.91-.13 1.77-.45 2.54-.9L19.73 21 21 19.73 4.27 3z"/></svg>
                            </span>
                        {/if}
                        {#if isAdmin && p.id !== sessionData?.participantId}
                            <span class="participant-actions">
                                {#if p.role !== 'admin'}
                                    {#if p.canScreenshare}
                                        <button
                                            class="participant-action"
                                            onclick={() => revokeParticipantShare(p.id)}
                                            title="Revoke {p.name}'s screen share permission"
                                        >Revoke share</button>
                                    {:else}
                                        <button
                                            class="participant-action"
                                            onclick={() => allowParticipantShare(p.id)}
                                            title="Allow {p.name} to share their screen without asking"
                                        >Allow share</button>
                                    {/if}
                                {/if}
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
                        Back to the room page
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
        canModerate={isAdmin}
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

    /* Consistent card anatomy for paused / reconnecting / error states */
    .stream-card {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: var(--space-md);
        width: min(92vw, 380px);
        padding: var(--space-xl);
        text-align: center;
        background: rgba(14, 14, 16, 0.78);
        backdrop-filter: blur(16px);
        -webkit-backdrop-filter: blur(16px);
        border: 1px solid rgba(255, 255, 255, 0.08);
        border-radius: var(--radius-lg);
        box-shadow: var(--shadow-lg);
    }
    .stream-card p { margin: 0; }
    .stream-card-title {
        margin: 0;
        font-family: var(--font-display);
        font-size: 1.125rem;
        font-weight: 600;
        letter-spacing: -0.01em;
        color: var(--color-text);
    }
    .stream-card-body {
        font-size: var(--text-body);
        line-height: 1.55;
        color: var(--color-text-muted);
        max-width: 34ch;
    }
    .stream-card-meta {
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
    }
    .stream-card-icon {
        width: 48px;
        height: 48px;
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        background: var(--color-neutral-bg);
        color: var(--color-text-muted);
    }
    .stream-card-icon.error {
        background: var(--color-error-bg);
        color: var(--color-error);
    }

    /* Branded connecting scrim (waiting/connecting) */
    .stream-status-overlay.connect-scrim {
        background:
            radial-gradient(
                ellipse 60% 40% at 50% 35%,
                rgba(72, 182, 166, 0.05),
                transparent 70%
            ),
            rgba(10, 10, 12, 0.94);
    }
    .connect-stage {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: var(--space-md);
        padding: var(--space-xl);
        text-align: center;
        max-width: min(92vw, 480px);
    }
    .connect-room {
        margin: 0;
        font-family: var(--font-display);
        font-size: clamp(1.375rem, 4vw, 1.75rem);
        font-weight: 600;
        letter-spacing: -0.015em;
        text-wrap: balance;
        color: var(--color-text);
    }
    .connect-copy {
        display: grid;
        place-items: center;
        min-height: 1.5rem;
        width: 100%;
    }
    .connect-copy > p {
        grid-area: 1 / 1;
        margin: 0;
        font-size: var(--text-body);
        color: var(--color-text-muted);
        white-space: nowrap;
    }

    /* Three-dot progress shared by the scrim and the reconnecting card */
    .connect-dots {
        display: flex;
        gap: 6px;
        padding: var(--space-xs) 0;
    }
    .connect-dots span {
        width: 6px;
        height: 6px;
        border-radius: 50%;
        background: var(--color-text-muted);
        animation: connect-dot 1.4s ease-in-out infinite;
    }
    .connect-dots span:nth-child(2) { animation-delay: 0.2s; }
    .connect-dots span:nth-child(3) { animation-delay: 0.4s; }
    @keyframes connect-dot {
        0%, 80%, 100% { opacity: 0.25; transform: scale(0.8); }
        40% { opacity: 1; transform: scale(1); }
    }

    .play-btn {
        width: 80px; height: 80px;
        border-radius: 50%;
        background: var(--color-primary);
        border: none;
        color: white;
        cursor: pointer;
        display: flex; align-items: center; justify-content: center;
        padding-left: 6px; /* optical centering of the triangle */
        box-shadow: 0 0 0 8px rgba(72, 182, 166, 0.12);
        transition: transform 0.2s ease, background 0.2s ease, box-shadow 0.2s ease;
    }
    .play-btn:hover {
        transform: scale(1.06);
        background: var(--color-primary-hover);
        box-shadow: 0 0 0 12px rgba(72, 182, 166, 0.16);
    }
    .play-btn:active { transform: scale(0.98); }

    /* All top banners share one stack so they never overlap */
    .banner-stack {
        position: absolute;
        top: calc(18px + env(safe-area-inset-top, 0px));
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

    /* True-neutral surface so the banner sits quietly above the connecting
       scrim instead of fighting it; status reads from the border tint only. */
    .mic-prompt {
        width: min(92vw, 440px);
        display: flex;
        flex-direction: column;
        gap: var(--space-sm);
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
        background: rgba(14, 14, 16, 0.92);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        border: 1px solid rgba(255, 255, 255, 0.12);
        color: var(--color-text);
        box-shadow: var(--shadow-lg);
        pointer-events: auto;
    }
    .mic-prompt.success {
        border-color: rgba(47, 191, 113, 0.45);
    }
    .mic-prompt.error {
        border-color: rgba(239, 90, 90, 0.5);
    }
    .mic-prompt-copy {
        display: flex;
        flex-direction: column;
        gap: 2px;
    }
    .mic-prompt-title {
        margin: 0;
        font-size: 0.875rem;
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
        /* Idle fade-out is gentle (~300ms)… */
        transition: opacity 300ms ease, visibility 300ms ease;
    }
    .controls-overlay.visible {
        opacity: 1;
        visibility: visible;
        /* …but reappearing on movement is near-instant (~150ms). */
        transition: opacity 150ms ease, visibility 150ms ease;
    }
    @media (prefers-reduced-motion: reduce) {
        .controls-overlay,
        .controls-overlay.visible {
            transition: none;
        }
    }
    .controls-overlay.visible > * { pointer-events: auto; }
    .controls-overlay .top-bar,
    .controls-overlay .bottom-bar {
        transition: transform var(--transition-normal);
    }
    .controls-overlay:not(.visible) .top-bar { transform: translateY(-6px); }
    .controls-overlay:not(.visible) .bottom-bar { transform: translateY(6px); }

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
        font-size: 0.875rem;
        letter-spacing: 0.01em;
        background: rgba(10, 10, 12, 0.55);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        border: 1px solid rgba(255, 255, 255, 0.08);
        padding: 8px 16px;
        border-radius: var(--radius-md);
    }

    .latency-display {
        font-size: 0.75rem; font-family: monospace;
        background: rgba(10, 10, 12, 0.55);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        padding: var(--space-xs) var(--space-sm);
        border-radius: var(--radius-sm);
        border: 1px solid transparent;
    }
    .latency-display.good { color: var(--color-success); border-color: var(--color-success); }
    .latency-display.warning { color: var(--color-warning); border-color: var(--color-warning); }
    .latency-display.bad { color: var(--color-error); border-color: var(--color-error); }

    .participant-count {
        display: flex; align-items: center; gap: 6px;
        font-size: 0.875rem;
        color: var(--color-text-muted);
        background: rgba(10, 10, 12, 0.55);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        padding: 8px 16px;
        border-radius: var(--radius-md);
        border: 1px solid rgba(255, 255, 255, 0.08);
        cursor: pointer;
        transition: border-color 0.15s ease, color 0.15s ease, background 0.15s ease;
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
        /* Fixed compact width: admin row actions are overlaid on hover, so
           they must not stretch the box; long names ellipsize instead. */
        width: min(300px, calc(100vw - 2 * var(--space-lg)));
        max-height: 300px;
        overflow-y: auto;
        z-index: 50;
        padding: var(--space-xs) 0;
    }

    .participant-list:focus {
        outline: none;
    }

    .participant-list-item {
        position: relative;
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

    /* Admin per-row actions: overlaid on the right edge on hover/focus so
       they don't reserve row width and balloon the list. */
    .participant-actions {
        position: absolute;
        right: var(--space-sm);
        top: 50%;
        transform: translateY(-50%);
        display: flex;
        gap: var(--space-xs);
        padding: 2px 4px;
        background: rgba(10, 10, 10, 0.92);
        border-radius: var(--radius-sm);
        opacity: 0;
        pointer-events: none;
        transition: opacity 0.12s ease;
    }
    .participant-list-item:hover .participant-actions,
    .participant-list-item:focus-within .participant-actions {
        opacity: 1;
        pointer-events: auto;
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
    .mic-muted-indicator {
        color: var(--color-error);
        opacity: 0.85;
        display: flex; align-items: center;
    }
    /* Avatar ring glows in the participant's own color while speaking */
    .participant-list-avatar {
        box-shadow: 0 0 0 0 transparent;
        transition: box-shadow 0.2s ease;
    }
    .participant-list-avatar.speaking {
        box-shadow:
            0 0 0 2px rgba(0, 0, 0, 0.6),
            0 0 0 4px var(--participant-color, var(--color-success)),
            0 0 10px var(--participant-color, var(--color-success));
    }
    @keyframes pulse-speaking {
        0%, 100% { opacity: 1; }
        50% { opacity: 0.5; }
    }

    /* Compact always-visible presence row (BUG 5) */
    .presence-row {
        display: flex;
        align-items: center;
        padding: 6px 8px;
        gap: 0;
        background: rgba(10, 10, 12, 0.55);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        border: 1px solid rgba(255, 255, 255, 0.08);
        border-radius: var(--radius-full);
    }
    .presence-dot {
        width: 1.5rem;
        height: 1.5rem;
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 0.625rem;
        font-weight: 600;
        color: #fff;
        background-color: var(--participant-color, #555);
        border: 2px solid rgba(0, 0, 0, 0.55);
        margin-left: -6px;
        position: relative;
        transition: box-shadow 0.2s ease, opacity 0.2s ease;
    }
    .presence-dot:first-child { margin-left: 0; }
    .presence-dot.speaking {
        z-index: 1;
        box-shadow:
            0 0 0 2px var(--participant-color, var(--color-success)),
            0 0 8px var(--participant-color, var(--color-success));
    }
    .presence-dot.muted { opacity: 0.55; }
    .presence-dot.muted::after {
        content: "";
        position: absolute;
        left: 50%;
        top: 50%;
        width: 130%;
        height: 1.5px;
        background: var(--color-error);
        transform: translate(-50%, -50%) rotate(-45deg);
        border-radius: 1px;
    }
    .presence-overflow {
        margin-left: 4px;
        font-size: 0.625rem;
        font-weight: 600;
        color: var(--color-text-muted);
    }

    /* Live indicator dot in the sharer's split-pane label */
    .self-share-dot {
        width: 7px;
        height: 7px;
        border-radius: 50%;
        background: var(--color-success);
        animation: pulse-speaking 1.6s infinite;
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
        font-size: 0.6875rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.06em;
        color: rgba(255, 255, 255, 0.85);
        background: rgba(10, 10, 12, 0.55);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        padding: 4px 10px;
        border-radius: var(--radius-full);
        border: 1px solid rgba(255, 255, 255, 0.1);
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
        background: rgba(10, 10, 12, 0.55);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        border: 1px solid rgba(255, 255, 255, 0.08);
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

    /* Audio settings popover (ITEM 4) */
    .audio-settings-popover {
        position: absolute;
        bottom: calc(100% + 8px);
        left: 50%;
        width: 300px;
        margin-left: -150px;
        display: flex;
        flex-direction: column;
        gap: var(--space-md);
        padding: var(--space-md);
        max-height: 320px;
        overflow-y: auto;
        background: rgba(0, 0, 0, 0.85);
        backdrop-filter: blur(12px);
        -webkit-backdrop-filter: blur(12px);
        border: 1px solid rgba(255, 255, 255, 0.12);
        border-radius: var(--radius-md);
        box-shadow: var(--shadow-lg);
        z-index: 30;
    }
    .audio-settings-section {
        display: flex;
        flex-direction: column;
        gap: var(--space-xs);
    }
    .audio-settings-title {
        font-size: var(--text-min);
        font-weight: 500;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        color: var(--color-text-muted);
    }
    .audio-settings-hint {
        margin: 0;
        font-size: 0.75rem;
        color: var(--color-text-subtle);
    }
    .audio-settings-grant {
        align-self: flex-start;
        background: var(--color-primary);
        border: none;
        border-radius: var(--radius-sm);
        color: #041014;
        font-size: 0.75rem;
        font-weight: 600;
        padding: 6px 10px;
        cursor: pointer;
        transition: filter 0.15s ease;
    }
    .audio-settings-grant:hover { filter: brightness(1.08); }
    .audio-device-option {
        display: flex;
        align-items: center;
        gap: var(--space-xs);
        width: 100%;
        text-align: left;
        background: rgba(255, 255, 255, 0.04);
        border: 1px solid transparent;
        border-radius: var(--radius-sm);
        color: var(--color-text);
        font-size: 0.8125rem;
        padding: 6px 8px;
        cursor: pointer;
        transition: background 0.12s ease, border-color 0.12s ease;
    }
    .audio-device-option:hover { background: rgba(255, 255, 255, 0.1); }
    .audio-device-option.selected {
        border-color: rgba(255, 255, 255, 0.35);
        background: rgba(255, 255, 255, 0.12);
        color: #fff;
    }
    .audio-device-option:disabled {
        opacity: 0.5;
        cursor: wait;
    }
    .audio-device-check {
        width: 14px;
        display: inline-flex;
        align-items: center;
        justify-content: center;
        flex-shrink: 0;
        color: var(--color-primary);
    }
    .audio-device-label {
        flex: 1;
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
    }

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
        gap: 8px;
        background: linear-gradient(
            to bottom,
            rgba(16, 16, 18, 0.55),
            rgba(8, 8, 10, 0.7)
        );
        backdrop-filter: blur(18px);
        -webkit-backdrop-filter: blur(18px);
        border: 1px solid rgba(255, 255, 255, 0.08);
        box-shadow: 0 8px 24px rgba(0, 0, 0, 0.35);
        padding: 8px;
        border-radius: 16px;
    }

    .control-btn {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: 4px;
        padding: 10px 16px;
        background: rgba(255, 255, 255, 0.06);
        border: 1px solid rgba(255, 255, 255, 0.08);
        border-radius: 10px;
        color: rgba(255, 255, 255, 0.92);
        cursor: pointer;
        transition: background 0.15s ease, border-color 0.15s ease, color 0.15s ease;
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

    /* Per-message pulse on the chat button (ITEM 5) */
    .control-btn.chat-btn.pulse {
        animation: chat-pulse 0.6s ease-out;
    }
    @keyframes chat-pulse {
        0% { box-shadow: 0 0 0 0 rgba(72, 182, 166, 0.65); }
        70% { box-shadow: 0 0 0 12px rgba(72, 182, 166, 0); }
        100% { box-shadow: 0 0 0 0 rgba(72, 182, 166, 0); }
    }

    /* Transient chat toast above the bottom bar (ITEM 5) */
    .chat-toast {
        position: absolute;
        bottom: 150px;
        left: 50%;
        transform: translateX(-50%);
        z-index: 22;
        display: flex;
        align-items: baseline;
        gap: var(--space-xs);
        max-width: min(80vw, 420px);
        padding: 8px 14px;
        background: rgba(0, 0, 0, 0.8);
        backdrop-filter: blur(12px);
        -webkit-backdrop-filter: blur(12px);
        border: 1px solid rgba(72, 182, 166, 0.35);
        border-radius: var(--radius-full);
        color: var(--color-text);
        cursor: pointer;
        transition: border-color 0.15s ease, background 0.15s ease;
    }
    .chat-toast:hover {
        border-color: rgba(72, 182, 166, 0.7);
        background: rgba(0, 0, 0, 0.9);
    }
    .chat-toast-name {
        font-size: 0.75rem;
        font-weight: 700;
        color: var(--color-primary);
        white-space: nowrap;
        flex-shrink: 0;
    }
    .chat-toast-text {
        font-size: 0.8125rem;
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
    }

    @media (prefers-reduced-motion: reduce) {
        .control-btn.chat-btn.pulse { animation: none; }
        .chat-toast { transition: none; }
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
    /* Suppress WebKit's native hover/overlay media controls on the live
       share — clicking them pauses a livestream (and fights the laser). */
    .split-screenshare video::-webkit-media-controls,
    .split-screenshare video::-webkit-media-controls-enclosure,
    .split-screenshare video::-webkit-media-controls-overlay-play-button {
        display: none !important;
        -webkit-appearance: none;
    }
    .split-screenshare-label {
        position: absolute;
        /* Bottom-left: the top-left corner is occupied by the room-name
           popup, which was covering the Stop button. */
        bottom: 8px;
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
        background: rgba(14, 14, 16, 0.92);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        border: 1px solid rgba(255, 255, 255, 0.14);
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

    /* Admin "open room now" banner (guests waiting on the countdown) */
    .open-early-banner {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
        background: rgba(14, 14, 16, 0.92);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        border: 1px solid rgba(72, 182, 166, 0.4);
        color: #fff;
        box-shadow: var(--shadow-lg);
        pointer-events: auto;
    }
    .open-early-text {
        font-size: 0.875rem;
        font-weight: 500;
    }
    .open-early-btn {
        border: none;
        border-radius: var(--radius-sm);
        padding: 6px 12px;
        font-size: 0.75rem;
        font-weight: 600;
        background: var(--color-primary);
        color: #041014;
        cursor: pointer;
        transition: filter 0.15s ease;
    }
    .open-early-btn:hover {
        filter: brightness(1.1);
    }
    .open-early-btn:disabled {
        opacity: 0.6;
        cursor: wait;
    }

    /* Waiting-room approval stack: persistent admin cards, top-right below
       the top bar. Scrolls past ~4 cards; never auto-dismisses. */
    .waiting-stack {
        position: absolute;
        top: calc(64px + env(safe-area-inset-top, 0px));
        right: var(--space-md);
        z-index: 26;
        display: flex;
        flex-direction: column;
        gap: 8px;
        width: min(86vw, 320px);
        max-height: 312px; /* ~4 cards before scrolling */
        overflow-y: auto;
        overscroll-behavior: contain;
        scrollbar-width: thin;
        pointer-events: auto;
    }
    .waiting-request-card {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
        background: rgba(14, 14, 16, 0.92);
        backdrop-filter: blur(14px);
        -webkit-backdrop-filter: blur(14px);
        border: 1px solid rgba(255, 255, 255, 0.14);
        color: #fff;
        box-shadow: var(--shadow-lg);
        flex-shrink: 0;
    }
    /* Chime-free attention: one brief teal pulse on arrival, then settle. */
    .waiting-request-card.pulse {
        animation: waiting-card-pulse 1.2s ease-out 1;
    }
    @keyframes waiting-card-pulse {
        0% { border-color: rgba(72, 182, 166, 0.85); box-shadow: 0 0 0 0 rgba(72, 182, 166, 0.35), var(--shadow-lg); }
        60% { border-color: rgba(72, 182, 166, 0.45); box-shadow: 0 0 0 8px rgba(72, 182, 166, 0), var(--shadow-lg); }
        100% { border-color: rgba(255, 255, 255, 0.14); box-shadow: var(--shadow-lg); }
    }
    .waiting-avatar {
        flex-shrink: 0;
        width: 28px;
        height: 28px;
        display: grid;
        place-items: center;
        border-radius: 50%;
        background: rgba(72, 182, 166, 0.18);
        border: 1px solid rgba(72, 182, 166, 0.5);
        color: var(--color-primary);
        font-size: 0.8125rem;
        font-weight: 600;
        font-family: var(--font-display);
    }
    .waiting-request-copy {
        flex: 1;
        min-width: 0;
        display: flex;
        flex-direction: column;
        gap: 1px;
    }
    .waiting-request-name {
        font-size: 0.875rem;
        font-weight: 600;
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
    }
    .waiting-request-meta {
        font-size: 0.75rem;
        color: rgba(255, 255, 255, 0.65);
    }
    .waiting-request-elapsed {
        color: rgba(255, 255, 255, 0.4);
        font-variant-numeric: tabular-nums;
    }
    .waiting-request-actions {
        display: flex;
        flex-direction: column;
        gap: 4px;
    }
    .waiting-request-btn {
        border: none;
        border-radius: var(--radius-sm);
        padding: 5px 10px;
        font-size: 0.6875rem;
        font-weight: 600;
        cursor: pointer;
        transition: filter 0.15s ease;
    }
    .waiting-request-btn:hover {
        filter: brightness(1.1);
    }
    .waiting-request-btn.approve {
        background: var(--color-primary);
        color: #041014;
    }
    .waiting-request-btn.deny {
        background: rgba(255, 255, 255, 0.12);
        color: rgba(255, 255, 255, 0.85);
    }
    .waiting-request-btn.deny:hover {
        background: rgba(239, 68, 68, 0.75);
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
        .presence-row { display: none; }
        .video-wrapper.split-active { flex-direction: column; }
        .video-wrapper.split-active .video-container { flex: 1; }
        .split-screenshare { flex: 1; }
    }

    /* Icon-only controls on small phones so all buttons fit a 375px screen */
    @media (max-width: 480px) {
        .controls-overlay { padding-left: var(--space-sm); padding-right: var(--space-sm); }
        .stream-card { padding: var(--space-lg); }
        .connect-stage { padding: var(--space-lg); }
        .connect-copy > p { white-space: normal; }
        .end-state-card { padding: var(--space-lg); }
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
