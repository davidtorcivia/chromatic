<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { fade, fly, scale } from "svelte/transition";
    import { quintOut } from "svelte/easing";
    import { goto } from "$app/navigation";
    import { page } from "$app/stores";
    import { session } from "$lib/stores/session.svelte";
    import { rooms, uploadFile } from "$lib/api/client";
    import { chatStore, type ChatMessage } from "$lib/stores/chat.svelte";
    import { unlockAudio, getAudioContext, closeAudioContext } from "$lib/audio/context";
    import {
        WebRTCManager,
        getStoredMicDeviceId,
        storeMicDeviceId,
        storeCameraDeviceId,
    } from "$lib/webrtc/manager";
    import { loadAudioModeState, getJoinWithCamera, type AudioMode, type DenoiserEngine } from "$lib/audio/audio-mode";
    import { deriveStreamOverlayState } from "$lib/video/stream-overlay";
    import { AudioDuckingManager } from "$lib/audio/ducking";
    import { playShareRequestChime, playWaitingRoomChime, playJoinChime, playLeaveChime, playChatReceiveChime, getUiSoundsEnabled, setUiSoundsEnabled } from "$lib/audio/chimes";
    import LaserPointerOverlay from "$lib/components/LaserPointerOverlay.svelte";
    import LoupeOverlay from "$lib/components/LoupeOverlay.svelte";
    import ScopesPanel from "$lib/components/ScopesPanel.svelte";
    import ChatPanel from "$lib/components/ChatPanel.svelte";
    import ConfirmDialog from "$lib/components/ConfirmDialog.svelte";
    import WatermarkOverlay from "$lib/components/WatermarkOverlay.svelte";
    import { liquidLens } from "$lib/glass/lens";
    import { videoGlassGroup } from "$lib/glass/videoGlass";
    import {
        loadSnapshot,
        setReviewQualityMode,
        startLoadMonitor,
        stopLoadMonitor,
        type ReviewQualityMode,
    } from "$lib/perf/loadMonitor";
    import { releaseFrames } from "$lib/glass/frameSource";
    import { tooltip } from "$lib/ui/tooltip";
    import { parseStoredSession, type StoredSessionData } from "$lib/session/storedSession";
    import { getStorageItem, removeStorageItem, setStorageItem } from "$lib/storage/safeStorage";

    const DEBUG = import.meta.env.DEV;
    const debugLog = (...args: unknown[]) => {
        if (DEBUG) console.log(...args);
    };

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
    let micPromptTimer: ReturnType<typeof setTimeout> | null = null;
    let kickTarget = $state<{ id: string; name: string } | null>(null);
    let endState = $state<{ title: string; body: string } | null>(null);
    let isFullscreen = $state(false);
    // iPhone has no element Fullscreen API (only native <video>, which drops our
    // overlays + watermark) — hide the button there. iPad/desktop keep it.
    let supportsFullscreen = $state(true);
    // Mobile: phones collapse secondary tools behind a "More" sheet.
    let showMoreSheet = $state(false);
    // Touch device (coarse pointer) — gates the More sheet, touch tools, the
    // rotate hint, and hides controls that can't work on iPhone (Fullscreen).
    let isCoarsePointer = $state(false);
    // Orientation + the program stream's intrinsic aspect, for the rotate nudge.
    let isPortrait = $state(false);
    let videoAspect = $state(0); // videoWidth / videoHeight, 0 until known
    let rotateHintDismissed = $state(false);
    // Glass surfaces rendered by the shared WebGL bar renderers
    let controlBarEl = $state<HTMLDivElement | null>(null);
    let roomNameEl = $state<HTMLDivElement | null>(null);
    let presenceRowEl = $state<HTMLDivElement | null>(null);
    let participantCountEl = $state<HTMLButtonElement | null>(null);
    let livePillEl = $state<HTMLElement | null>(null);
    let topBarEl = $state<HTMLDivElement | null>(null);
    let bottomBarEl = $state<HTMLDivElement | null>(null);
    let signalEl = $state<HTMLElement | null>(null);
    let latencyEl = $state<HTMLElement | null>(null);
    // Popover optics, owned in one place (three surfaces share it)
    const POPOVER_LENS = { blur: 12, radius: 20, scale: 36, zoom: 0.04, rim: 18 };
    const glassItems = (els: (HTMLElement | null)[]) =>
        els.filter((el): el is HTMLElement => el !== null);
    const topGlassItems = () =>
        glassItems([roomNameEl, presenceRowEl, participantCountEl]);
    const bottomGlassItems = () =>
        glassItems([controlBarEl, livePillEl, signalEl, latencyEl]);
    const glassEnabled = () =>
        hasStream &&
        isVideoPlaying &&
        !screenShareStream &&
        !selfShareStream &&
        // The WebGL glass samples the video every frame on the main thread; while
        // a review tool is in hand that competes with the laser's pointer input
        // and makes strokes coarse. Sleep it during laser/loupe — the bars fall
        // back to the (GPU-composited) CSS glass, so they stay glassy and the
        // laser stays smooth.
        !isLaserEnabled &&
        !isLoupeEnabled;
    // Specular sweep requests: stream connect and new arrivals. The glass
    // renderer only honors fresh requests, so a join while the controls
    // are hidden passes without a stale sweep on the next reveal.
    let shimmerRequestedAt = $state(0);
    $effect(() => {
        if (isVideoPlaying) shimmerRequestedAt = Date.now();
    });
    let statsPopoverEl = $state<HTMLDivElement | null>(null);
    let participantListEl = $state<HTMLDivElement | null>(null);
    let currentRtt = $state<number | null>(null);
    let currentVideoBufferDelay = $state<number | null>(null);
    let currentReceiverJitterTarget = $state<number | null>(null);
    let currentReceiverPlayoutHint = $state<number | null>(null);
    let loadUnderPressure = $state(false);
    let activeReviewToolCount = $state(0);
    let longFrameCount = $state(0);
    let worstLongFrameMs = $state<number | null>(null);
    let reconnectEvents = $state(0);
    let resubscribeEvents = $state(0);
    let statsInterval: ReturnType<typeof setInterval> | null = null;
    let statsPollGeneration = 0;
    // Whether stats polling should be running while the tab is visible. The
    // visibility handler pauses polling when backgrounded (a backgrounded
    // viewer's 1s getStats poll does nothing useful and re-tunes nothing now,
    // but still wakes the CPU) and resumes it only if this was true.
    let statsPollingWanted = false;
    // Cloudflare TURN credentials default to a 1 h TTL; long color-grading
    // sessions (4–8 h) outlive that. Refresh every 30 min over the existing
    // WebSocket so any ICE restart later always has fresh creds to gather
    // with. The live media allocation is unaffected by the refresh.
    let iceServerRefreshInterval: ReturnType<typeof setInterval> | null = null;
    let streamVolume = $state(1.0);
    let voiceVolume = $state(1.0);
    // Review tools + new overlays
    let isLoupeEnabled = $state(false);
    let showScopes = $state(false);
    let showShortcuts = $state(false);
    let showStats = $state(false);
    let reviewQualityMode = $state<ReviewQualityMode>("balanced");
    let displayFps = $state<number | null>(null);
    let frameCaptureToDisplayDelay = $state<number | null>(null);
    let frameReceiveToDisplayDelay = $state<number | null>(null);
    let frameProcessingDelay = $state<number | null>(null);
    let grabBusy = $state(false);
    let grabFlash = $state(false);
    let grabToast = $state<string | null>(null);
    let grabFlashTimer: ReturnType<typeof setTimeout> | null = null;
    let grabToastTimer: ReturnType<typeof setTimeout> | null = null;
    let grabAbortController: AbortController | null = null;
    let destroyed = false;
    // Preferences (persisted)
    let uiSounds = $state(getUiSoundsEnabled());
    let reduceTransparency = $state(false);
    let streamError = $state<string | null>(null);
    let needsPlayClick = $state(false); // Autoplay fallback
    let streamPaused = $state(false); // Stream temporarily disconnected
    // True once the video element fires 'playing' — the only reliable signal
    // that frames are rendering. play()'s promise can stay pending forever on
    // a stream waiting for a keyframe (BUG 1), so we never gate UI on it.
    let isVideoPlaying = $state(false);
    // True while the subscriber RTCPeerConnection is in a degraded/terminal
    // state (failed/disconnected) that the manager is recovering from. Driven
    // by onConnectionStateChange so a failed PC with a HEALTHY WebSocket —
    // where the WS-driven connectionLost flag never flips — still drops the
    // frozen last frame out of the 'playing' overlay and into 'connecting' while
    // ICE restart / resubscribe runs. Without this the viewer sits on a frozen
    // frame with no recovery indicator.
    let peerConnectionDegraded = $state(false);
    // Keyframe nudge: if tracks are bound but the video hasn't started
    // playing shortly after, request a resync (PLI). The server's single PLI
    // at subscriber-creation can be lost (sent before ICE finished), leaving
    // a reloading viewer stuck waiting for a decodable keyframe.
    let playNudgeTimer: ReturnType<typeof setTimeout> | null = null;
    let playNudgeAttempts = 0;
    const PLAY_NUDGE_MAX_ATTEMPTS = 3;
    const PLAY_NUDGE_INTERVAL_MS = 350;
    const MEDIA_STALL_GRACE_MS = 750;
    const STATS_POLL_INTERVAL_MS = 1000;
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
    // Talkback vs studio/critical-listening (persisted). Initialized from the
    // saved preference so the UI is correct even before the manager exists.
    const _savedAudioMode = loadAudioModeState();
    let audioMode = $state<AudioMode>(_savedAudioMode.mode);
    let denoiserEngine = $state<DenoiserEngine>(_savedAudioMode.denoiser);
    let studioHeadphones = $state(_savedAudioMode.studioHeadphones);
    let audioModePending = $state(false);
    // Presence webcams (small circular tiles, top strip). Off by default; only
    // the device preference persists. Remote cam streams keyed by participant id
    // (reassigned for reactivity, mirroring speakingParticipants).
    let isCameraOn = $state(false);
    let cameraPending = $state(false);
    let selfCamStream = $state<MediaStream | null>(null);
    let activeCameraId = $state<string | null>(null);
    let videoInputs = $state<MediaDeviceInfo[]>([]);
    let remoteCamStreams = $state<Map<string, MediaStream>>(new Map());
    // One-time "turn on your camera?" nudge after the mic is enabled. Dismissed
    // permanently (for the session) once the user acts on it or turns the cam on.
    let camNudgeDismissed = $state(false);
    const SPEAKER_DEVICE_STORAGE_KEY = "chromatic_speaker_device";
    const supportsSinkSelection =
        typeof HTMLMediaElement !== "undefined" &&
        "setSinkId" in HTMLMediaElement.prototype;
    // Chat notification cues (ITEM 5)
    let chatPulseActive = $state(false);
    let chatPulseTimer: ReturnType<typeof setTimeout> | null = null;
    let chatToast = $state<{ id: string; name: string; text: string } | null>(null);
    let chatToastTimer: ReturnType<typeof setTimeout> | null = null;
    // Who's typing in chat (id -> name + expiry); pruned by a 1s sweep so
    // the indicator dies on its own when signals stop arriving.
    let typingUsers = $state<Record<string, { name: string; until: number }>>({});
    let typingPruneInterval: ReturnType<typeof setInterval> | null = null;
    let typingList = $derived(
        Object.entries(typingUsers).map(([id, v]) => ({ id, name: v.name })),
    );
    const prefersReducedMotion =
        typeof window !== "undefined" &&
        window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    let isLaserEnabled = $state(false);
    let showParticipantList = $state(false);
    let speakingParticipants = $state<Set<string>>(new Set());
    // Participants currently muted BY AN ADMIN (distinct from self-muted:
    // the admin toggle must only offer Unmute where it was the admin who
    // muted — force-enabling a self-muted mic would be a hot-mic surprise).
    let adminMutedIds = $state<Set<string>>(new Set());
    // Participants whose camera an admin has gated off (server-enforced).
    let camDisabledIds = $state<Set<string>>(new Set());
    // Hot-mic guard: an admin unmute may only RESUME a mic that was live
    // when the admin muted it. Anything else (we were self-muted, or a
    // stray/forged unmute for someone never admin-muted) must not touch
    // the microphone.
    let resumeMicOnAdminUnmute = false;
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
    // False from "someone is starting a share" until their frames render
    let shareVideoReady = $state(false);
    // Any change of the displayed share stream restarts the loading state;
    // the video's onplaying flips it back. Structural, so paths that swap
    // the stream without the start/stop handlers can't leave it stale.
    $effect(() => {
        void screenShareStream;
        void selfShareStream;
        shareVideoReady = false;
    });
    let pendingScreenShareRequest = $state<{participantId: string, name: string} | null>(null);
    // Share was approved; waiting for the user's click to open the OS picker
    // (getDisplayMedia must be called from a user gesture).
    let shareApprovedPrompt = $state(false);
    // Local self-preview of the sharer's own capture (BUG 4)
    let selfShareStream = $state<MediaStream | null>(null);

    // Get session data from storage
    let sessionData = $state<StoredSessionData | null>(null);

    onMount(async () => {
        startLoadMonitor();
        document.addEventListener("visibilitychange", handleVisibilityChange);
        try {
            if (getStorageItem("local", "chromatic_reduce_transparency") === "on") {
                reduceTransparency = true;
                document.documentElement.classList.add("reduce-transparency");
            }
            const savedReviewMode = getStorageItem("local", "chromatic_review_quality_mode");
            if (
                savedReviewMode === "performance" ||
                savedReviewMode === "balanced" ||
                savedReviewMode === "fidelity"
            ) {
                reviewQualityMode = savedReviewMode;
            }
        } catch {
            // Storage unavailable; default applies.
        }
        setReviewQualityMode(reviewQualityMode);
        // Get session data
        const stored = getStorageItem("session", `chromatic_session_${slug}`);
        if (!stored) {
            goto(`/room/${slug}`);
            return;
        }

        const parsedSession = parseStoredSession(stored);
        if (!parsedSession) {
            removeStorageItem("session", `chromatic_session_${slug}`);
            goto(`/room/${slug}`);
            return;
        }
        sessionData = parsedSession;

        const storedName = getStorageItem("local", "chromatic_name");
        if (storedName) {
            participantName = storedName;
        }
        // Prefer the name stored with the session payload over the global
        // localStorage name, which a later join (other room/tab) may have
        // overwritten.
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
            debugLog('Received ICE servers:', iceServers);
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
            debugLog('Received WebRTC offer');

            if (!webrtcManager) {
                initializeWebRTC();
            }

            if (webrtcManager) {
                try {
                    await webrtcManager.handleOffer(data.sdp, data.offerId);
                    clearSubscriptionRetryTimer();
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
            debugLog('Room is now live');
        });

        session.onMessage("stream:paused", (payload: unknown) => {
            const data = payload as { message?: string };
            streamError = null;
            streamPaused = true;
            if (data?.message) debugLog(data.message);
        });

        session.onMessage("stream:resumed", () => {
            streamError = null;
            streamPaused = false;
        });

        session.onMessage("room:ended", () => {
            clearStoredSession();
            cleanupWebRTC();
            session.disconnect();
            endState = {
                title: "Session ended",
                body: "The host has ended this review session. Thanks for joining.",
            };
        });

        session.onMessage("kicked", (payload: unknown) => {
            const data = payload as { reason?: string };
            clearStoredSession();
            cleanupWebRTC();
            session.disconnect();
            endState = {
                title: "You were removed from the session",
                body: data.reason || "An admin removed you from this session.",
            };
        });

        session.onMessage("admin:muted", (payload: unknown) => {
            const data = payload as { participantId: string; muted?: boolean };
            const muted = data.muted !== false;
            // Read the prior state BEFORE updating the set: resume is only
            // legitimate when this unmute reverses an admin mute we saw.
            const selfWasAdminMuted = adminMutedIds.has(data.participantId);
            const next = new Set(adminMutedIds);
            if (muted) next.add(data.participantId);
            else next.delete(data.participantId);
            adminMutedIds = next;

            if (data.participantId === sessionData?.participantId) {
                if (muted) {
                    resumeMicOnAdminUnmute = isMicEnabled;
                    isMicEnabled = false;
                    micAutoEnablePending = false;
                    webrtcManager?.setMicEnabled(false);
                    setSelfAudio(false);
                } else if (
                    selfWasAdminMuted &&
                    resumeMicOnAdminUnmute &&
                    hasMicPermission &&
                    webrtcManager
                ) {
                    // Admin restored a mic that was live when they muted it
                    resumeMicOnAdminUnmute = false;
                    isMicEnabled = true;
                    webrtcManager.setMicEnabled(true);
                    setSelfAudio(true);
                } else {
                    resumeMicOnAdminUnmute = false;
                }
            }
        });

        // Admin camera gate. The server stops relaying the target's cam; the
        // target also stops capturing locally so their light goes off. We do NOT
        // auto-re-enable on un-gate (cam is opt-in/privacy) — the button just
        // becomes usable again.
        session.onMessage("webcam:disabled", (payload: unknown) => {
            const data = payload as { participantId?: string; disabled?: boolean };
            if (!data?.participantId) return;
            const disabled = data.disabled !== false;
            const next = new Set(camDisabledIds);
            if (disabled) next.add(data.participantId);
            else next.delete(data.participantId);
            camDisabledIds = next;

            if (disabled) {
                // The relay teardown already drops the tile elsewhere; ensure
                // ours goes too.
                removeRemoteCam(data.participantId);
                if (data.participantId === sessionData?.participantId) {
                    // Stop a live broadcast AND release a preview-only capture
                    // (modal open but not yet enabled) so the admin gate also
                    // turns off the camera light. Close the modal too.
                    if (isCameraOn) {
                        webrtcManager?.stopWebcam();
                        isCameraOn = false;
                    } else {
                        webrtcManager?.cancelCameraPreview();
                    }
                    showCameraModal = false;
                    selfCamStream = null;
                }
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
            debugLog('Screen share denied:', data.reason || 'Request denied by admin');
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

        // A participant turned their cam off — drop their tile promptly (the
        // relayed track also ends, but this is the explicit, immediate signal).
        session.onMessage("webcam:stopped", (payload: unknown) => {
            const data = payload as { participantId?: string };
            if (data?.participantId) removeRemoteCam(data.participantId);
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
            if (msg.participantId !== sessionData?.participantId) {
                playChatReceiveChime();
            }
            // Their message arrived; stop showing them as typing.
            if (msg.participantId && typingUsers[msg.participantId]) {
                const { [msg.participantId]: _, ...rest } = typingUsers;
                typingUsers = rest;
            }
            notifyChatMessage(msg);
        });

        session.onMessage("chat:typing", (payload: unknown) => {
            const data = payload as { participantId?: string; participantName?: string };
            if (!data?.participantId || data.participantId === sessionData?.participantId) return;
            typingUsers = {
                ...typingUsers,
                [data.participantId]: {
                    name: data.participantName || "Someone",
                    until: Date.now() + 3500,
                },
            };
        });
        typingPruneInterval = setInterval(() => {
            const now = Date.now();
            const live = Object.entries(typingUsers).filter(([, v]) => v.until > now);
            if (live.length !== Object.keys(typingUsers).length) {
                typingUsers = Object.fromEntries(live);
            }
        }, 1000);

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
            reconnectEvents++;
            debugLog("WebSocket reconnected, resetting WebRTC state");
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
            // The new manager has no camera capture (the old one was closed), so
            // reset cam UI to off and drop stale remote tiles. Active remote cams
            // re-arrive in the fresh subscriber offer; the user re-enables their
            // own cam if they want it back (parity with screen share resetting).
            isCameraOn = false;
            selfCamStream = null;
            remoteCamStreams = new Map();
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
        // Pass our participant id so session.state.participantId is populated
        // (the laser's local cursor, self-identification, etc. depend on it).
        session.connect(slug, participantName, sessionData!.participantId);

        window.addEventListener("chromatic:tampering", handleTampering);

        // Audio device plumbing (ITEM 4): restore the persisted speaker choice
        // and keep the device lists fresh when hardware is (un)plugged.
        try {
            selectedSpeakerId = getStorageItem("local", SPEAKER_DEVICE_STORAGE_KEY);
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
        destroyed = true;
        grabAbortController?.abort();
        grabAbortController = null;
        stopLoadMonitor();
        releaseFrames();
        cleanupWebRTC();
        if (audioDuckingManager) {
            audioDuckingManager.destroy();
            audioDuckingManager = null;
        }
        session.disconnect();
        closeAudioContext();
        window.removeEventListener("chromatic:tampering", handleTampering);
        navigator.mediaDevices?.removeEventListener?.("devicechange", refreshAudioDevices);
        document.removeEventListener("visibilitychange", handleVisibilityChange);
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
        if (typingPruneInterval) {
            clearInterval(typingPruneInterval);
            typingPruneInterval = null;
        }
        clearGrabTimers();
    });

    function clearGrabTimers() {
        if (grabFlashTimer) {
            clearTimeout(grabFlashTimer);
            grabFlashTimer = null;
        }
        if (grabToastTimer) {
            clearTimeout(grabToastTimer);
            grabToastTimer = null;
        }
    }

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
            onWebcamTrack: handleWebcamTrack,
            onWebcamEnded: () => {
                // Local cam capture died (device unplugged / OS revoked) — reset
                // the UI so it doesn't show "Cam On" over a frozen self-view.
                isCameraOn = false;
                selfCamStream = null;
            },
            sendSignal: (type, payload) => session.send(type, payload),
            onConnectionStateChange: (state) => {
                // The WebSocket can stay healthy while the media path dies (e.g.
                // a TURN allocation expires mid-session). Surface the PC's own
                // state so a frozen frame drops into the 'connecting' overlay
                // while the manager's ICE-restart / resubscribe recovery runs,
                // instead of masquerading as 'playing'. We do NOT trigger a
                // competing resubscribe here — the manager's failed→ICE-restart
                // →onIceRestartFailed chain (already wired below) owns that.
                peerConnectionDegraded = state === 'failed' || state === 'disconnected';
                if (peerConnectionDegraded) {
                    isVideoPlaying = false;
                }
            },
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

        debugLog('WebRTC manager initialized');
        setSelfAudio(false);
        void startAutoMicConnection();
        void maybeAutoStartCamera();
    }

    // Honor the waiting-room "join with camera on" choice: turn the presence cam
    // on once after join (permission was already granted in the green room, so
    // no prompt). Runs at most once; respects an admin camera gate.
    let camAutoStartTried = false;
    async function maybeAutoStartCamera() {
        if (camAutoStartTried) return;
        camAutoStartTried = true;
        if (!getJoinWithCamera() || !webrtcManager || isCameraOn || myCamDisabled) return;
        camNudgeDismissed = true; // they already opted in; no nudge needed
        await toggleCamera();
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
        const manager = webrtcManager;
        micAutoRequestStarted = true;
        micPromptState = "requesting";

        const granted = await manager.requestMicrophone();
        if (destroyed || webrtcManager !== manager) return;
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
        // Deliberately NOT gated on the subscriber offer: mic publishing
        // rides its own publisher PC (see manager.setMicEnabled), so the
        // mic connects immediately on join even when no stream is live
        // yet. The old offer gate left "Connecting your microphone"
        // spinning until the host started streaming.
        if (!webrtcManager || !hasMicPermission || !micAutoEnablePending) return;

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
        debugLog('Received track:', event.track.kind, event.streams);
        streamError = null;

        if (!videoElement) {
            streamError = "The video player didn't load correctly.";
            return;
        }

        if (!event.streams || event.streams.length === 0 || !event.streams[0]) {
            streamError = "The stream isn't available right now. The host may need to restart it.";
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

            ensureAudioDuckingManager();
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
        clearSubscriptionRetryTimer();
        // Media is flowing again — future failures get a fresh retry budget.
        resubscribeAttempts = 0;
        captureVideoAspect();
    }

    // The intrinsic aspect of the program stream drives the rotate hint (so we
    // only nudge when the phone orientation actually fights the content).
    function captureVideoAspect() {
        const w = videoElement?.videoWidth ?? 0;
        const h = videoElement?.videoHeight ?? 0;
        if (w > 0 && h > 0) videoAspect = w / h;
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
        resubscribeEvents++;
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
            debugLog(`Video not rendering yet, requesting keyframe (attempt ${playNudgeAttempts}/${PLAY_NUDGE_MAX_ATTEMPTS})`);
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
        statsPollingWanted = true;
        // If the tab is hidden, defer until it becomes visible again — the
        // visibility handler will resume polling.
        if (typeof document !== "undefined" && document.hidden) {
            return;
        }
        if (statsInterval) return;
        const generation = ++statsPollGeneration;
        let inFlight = false;
        const poll = async () => {
            const manager = webrtcManager;
            if (!manager || inFlight || destroyed || generation !== statsPollGeneration) return;
            inFlight = true;
            try {
                const stats = await manager.getStats();
                if (destroyed || webrtcManager !== manager || generation !== statsPollGeneration) {
                    return;
                }
                currentRtt = stats.rtt ?? null;
                currentVideoBufferDelay = stats.videoJitterBufferDelay ?? null;
                currentReceiverJitterTarget = stats.receiverJitterBufferTarget ?? null;
                currentReceiverPlayoutHint =
                    stats.receiverPlayoutDelayHint === undefined
                        ? null
                        : stats.receiverPlayoutDelayHint * 1000;
                const load = loadSnapshot();
                loadUnderPressure = load.underPressure;
                activeReviewToolCount = load.activeReviewToolCount;
                longFrameCount = load.longFrameCount;
                worstLongFrameMs = load.worstLongFrameMs;
            } catch (err) {
                if (!destroyed && webrtcManager === manager && generation === statsPollGeneration) {
                    console.warn("Failed to poll WebRTC stats", err);
                    currentRtt = null;
                    currentVideoBufferDelay = null;
                    currentReceiverJitterTarget = null;
                    currentReceiverPlayoutHint = null;
                }
            } finally {
                if (!destroyed && generation === statsPollGeneration) {
                    inFlight = false;
                }
            }
        };
        void poll();
        statsInterval = setInterval(() => {
            void poll();
        }, STATS_POLL_INTERVAL_MS);
    }

    function stopStatsPolling() {
        statsPollingWanted = false;
        statsPollGeneration++;
        if (statsInterval) {
            clearInterval(statsInterval);
            statsInterval = null;
        }
    }

    // Pause stats polling while the tab is hidden (nothing useful happens for a
    // backgrounded viewer, and skipping the 1s getStats poll saves CPU/power),
    // resuming automatically when the tab is visible again — but only if
    // polling was wanted (i.e. a stream is attached).
    function handleVisibilityChange() {
        if (destroyed) return;
        if (document.hidden) {
            // Drop the interval but remember the intent to poll.
            statsPollGeneration++;
            if (statsInterval) {
                clearInterval(statsInterval);
                statsInterval = null;
            }
        } else if (statsPollingWanted && !statsInterval) {
            startStatsPolling();
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

    // The audio graph that PLAYS voice must not wait for the program
    // stream: pre-stream lobby voice arrives long before handleTrack ever
    // fires, and buffering it until stream start meant participants could
    // not hear each other while waiting for the host (voice tracks flowed,
    // playback never started).
    function ensureAudioDuckingManager() {
        if (audioDuckingManager || !videoElement) return;
        audioDuckingManager = new AudioDuckingManager(videoElement, isAdmin);
        audioDuckingManager.setStreamVolume(streamVolume);
        audioDuckingManager.setVoiceVolume(voiceVolume);
        // Flush any voice tracks that arrived before we were created
        for (const [pid, vTrack] of pendingVoiceTracks) {
            audioDuckingManager.addVoiceTrack(pid, vTrack);
        }
        pendingVoiceTracks.clear();
    }

    function handleVoiceTrack(participantId: string, track: MediaStreamTrack) {
        ensureAudioDuckingManager();
        if (audioDuckingManager) {
            audioDuckingManager.addVoiceTrack(participantId, track);
        } else {
            // videoElement not bound yet (very early in mount): buffer for
            // the ensure call that runs on the next voice/main track.
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
        debugLog('Received screen share track from', participantId);
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

    function handleWebcamTrack(participantId: string, track: MediaStreamTrack) {
        debugLog("Received webcam track from", participantId);
        const stream = new MediaStream([track]);
        const next = new Map(remoteCamStreams);
        next.set(participantId, stream);
        remoteCamStreams = next;

        // Only remove on a real end (track stopped / relay torn down). A
        // transient `mute` event fires on a brief network stall and is followed
        // by `unmute` on recovery, so removing on mute would permanently drop a
        // tile after one hiccup. The explicit-off case is covered by the
        // webcam:stopped signal and participant:left.
        track.onended = () => removeRemoteCam(participantId, stream);
    }

    function removeRemoteCam(participantId: string, only?: MediaStream) {
        const current = remoteCamStreams.get(participantId);
        if (!current) return;
        // Guard against a stale end-event for a track that was already replaced.
        if (only && current !== only) return;
        const next = new Map(remoteCamStreams);
        next.delete(participantId);
        remoteCamStreams = next;
    }

    function cleanupParticipantVoice(participantId: string) {
        removeRemoteCam(participantId);
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

    // ---- Presence webcam -----------------------------------------------------
    // The camera is captured ONCE, in the manager, and the SAME MediaStream
    // serves both the modal preview and the live broadcast. openCameraModal
    // opens the preview (device light on); Enable only ADDS the already-live
    // track to the publisher. selfCamStream points at that one manager-owned
    // stream the whole way through, so going live never re-acquires the device
    // or detaches/re-attaches the <video> — the light can't drop and the tile
    // can't black on Enable (the old preview→handoff path did both).

    // Set true if the user dismisses the modal while an Enable is still in
    // flight, so the resolving enable tears down instead of silently going live.
    let camEnableAborted = false;

    // Open the centered camera modal: pick a device, preview, then Enable.
    function openCameraModal() {
        if (myCamDisabled) return; // admin gate: no preview, no enable
        camNudgeDismissed = true;
        showMoreSheet = false; // collapse the phone sheet cleanly into the modal
        showCameraModal = true;
        void refreshAudioDevices();
        void startCameraPreview(activeCameraId);
    }

    function closeCameraModal() {
        // If an Enable is mid-flight, mark it aborted so it doesn't turn the cam
        // on after the user dismissed.
        if (cameraPending) camEnableAborted = true;
        showCameraModal = false;
        // Dismissed without going live — release the preview capture.
        if (!isCameraOn) {
            webrtcManager?.cancelCameraPreview();
            selfCamStream = null;
        }
    }

    // Camera control-bar button: when off, open the picker; when on, turn off.
    function onCameraButton() {
        camNudgeDismissed = true;
        if (isCameraOn) {
            void toggleCamera();
        } else {
            openCameraModal();
        }
    }

    // Modal "Enable camera": go live on the device we're already previewing.
    // The manager keeps the same capture (no stop / re-acquire), so the camera
    // light stays on and the self-view never blacks.
    async function enableCameraFromModal() {
        if (!webrtcManager || cameraPending || myCamDisabled) return;
        const manager = webrtcManager;
        camNudgeDismissed = true;
        if (isCameraOn) {
            showCameraModal = false; // button reads "Done"
            return;
        }
        cameraPending = true;
        camEnableAborted = false;
        try {
            const ok = await manager.enableCamera(activeCameraId);
            if (destroyed || webrtcManager !== manager) {
                manager.stopWebcam();
                return;
            }
            if (ok && camEnableAborted) {
                // User dismissed the modal while we were going live — honor it.
                manager.stopWebcam();
                selfCamStream = null;
                return;
            }
            if (ok) {
                isCameraOn = true;
                selfCamStream = manager.getCameraStream();
                activeCameraId = manager.getCurrentCameraDeviceId();
                showCameraModal = false;
            } else {
                // Capture failed/denied — keep the modal up to show the reason.
                camCaptureDenied = true;
                selfCamStream = manager.getCameraStream();
            }
        } finally {
            if (!destroyed) cameraPending = false;
        }
    }

    async function toggleCamera() {
        camNudgeDismissed = true; // any camera interaction ends the nudge
        if (!webrtcManager || cameraPending) return;
        const manager = webrtcManager;
        cameraPending = true;
        try {
            if (isCameraOn) {
                manager.stopWebcam();
                isCameraOn = false;
                selfCamStream = null;
                return;
            }
            if (myCamDisabled) return; // admin gate: can't go live
            // No preview open (join-with-camera / direct toggle): enableCamera
            // acquires the capture itself.
            const ok = await manager.enableCamera(activeCameraId);
            if (destroyed || webrtcManager !== manager) {
                manager.stopWebcam();
                return;
            }
            if (ok) {
                isCameraOn = true;
                selfCamStream = manager.getCameraStream();
                activeCameraId = manager.getCurrentCameraDeviceId();
                // Labels become available after the first successful capture.
                await refreshAudioDevices();
            }
        } finally {
            if (!destroyed) cameraPending = false;
        }
    }

    async function selectCameraDevice(deviceId: string) {
        if (!webrtcManager || cameraPending || deviceId === activeCameraId) return;
        const manager = webrtcManager;
        cameraPending = true;
        try {
            storeCameraDeviceId(deviceId);
            activeCameraId = deviceId;
            // setCameraDevice handles BOTH states: replaceTrack while live,
            // in-place track swap while only previewing — one stream identity
            // either way, so the bound <video> never re-attaches.
            const ok = await manager.setCameraDevice(deviceId);
            if (destroyed || webrtcManager !== manager) return;
            if (ok) {
                selfCamStream = manager.getCameraStream();
                activeCameraId = manager.getCurrentCameraDeviceId() ?? deviceId;
            }
            await refreshAudioDevices();
        } finally {
            if (!destroyed) cameraPending = false;
        }
    }

    // Open the manager-owned preview capture and bind it to the self-view. The
    // manager reuses an already-live capture, so this never double-acquires the
    // device (which fails on Firefox).
    async function startCameraPreview(deviceId?: string | null) {
        if (!webrtcManager) return;
        const manager = webrtcManager;
        camCaptureDenied = false;
        const stream = await manager.openCameraPreview(deviceId ?? activeCameraId);
        if (destroyed || webrtcManager !== manager) return;
        if (!stream) {
            camCaptureDenied = true;
            return;
        }
        // The modal may have been dismissed during the await — if we're not
        // going to use the capture, release it.
        if (!showCameraModal && !isCameraOn) {
            manager.cancelCameraPreview();
            selfCamStream = null;
            return;
        }
        selfCamStream = stream;
        activeCameraId = manager.getCurrentCameraDeviceId() ?? activeCameraId;
        await refreshAudioDevices();
    }

    // Svelte action: attach a MediaStream to a <video> and keep it in sync.
    function bindStream(node: HTMLVideoElement, stream: MediaStream | null) {
        // Firefox can leave a freshly-attached getUserMedia stream painting a
        // black frame: a lone synchronous play() that rejects is never retried —
        // so the tile sits black over its #000 background even though the track
        // is live. Retry playback on the element's readiness events, on a track
        // flipping mute→unmute, AND when the device switch swaps a new track INTO
        // the same MediaStream (identity unchanged, so `update` never fires — we
        // catch it via the stream's 'addtrack' and re-kick + rebind).
        let current: MediaStream | null = null;
        let watched: MediaStream | null = null;
        const tryPlay = () => void node.play().catch(() => {});
        const onLoaded = () => tryPlay();
        const onUnmute = () => tryPlay();
        const trackListeners: MediaStreamTrack[] = [];
        const detachTracks = () => {
            for (const t of trackListeners) t.removeEventListener("unmute", onUnmute);
            trackListeners.length = 0;
        };
        const attachTracks = (s: MediaStream) => {
            for (const t of s.getVideoTracks()) {
                t.addEventListener("unmute", onUnmute);
                trackListeners.push(t);
            }
        };
        const onAddTrack = () => {
            if (!watched) return;
            detachTracks();
            attachTracks(watched);
            tryPlay();
        };
        const apply = (s: MediaStream | null) => {
            if (current === s) return;
            current = s;
            detachTracks();
            if (watched) {
                watched.removeEventListener("addtrack", onAddTrack);
                watched = null;
            }
            node.srcObject = s;
            if (s) {
                watched = s;
                s.addEventListener("addtrack", onAddTrack);
                attachTracks(s);
                tryPlay();
            }
        };
        node.addEventListener("loadeddata", onLoaded);
        apply(stream);
        return {
            update: apply,
            destroy() {
                node.removeEventListener("loadeddata", onLoaded);
                detachTracks();
                if (watched) watched.removeEventListener("addtrack", onAddTrack);
                watched = null;
                node.srcObject = null;
                current = null;
            },
        };
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
        const manager = webrtcManager;
        if (!manager) return;
        shareApprovedPrompt = false;
        const ok = await manager.startScreenShare();
        if (destroyed || webrtcManager !== manager) {
            manager.stopScreenShare();
            return;
        }
        if (ok) {
            screenShareActive = true;
            selfShareStream = manager.getScreenShareStream() ?? null;
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
        // Camera teardown: release a preview-only capture now (close() stops it
        // anyway when broadcasting); drop the UI state and any remote cam tiles
        // so nothing is left frozen/desynced.
        webrtcManager?.cancelCameraPreview();
        isCameraOn = false;
        selfCamStream = null;
        remoteCamStreams = new Map();
        if (webrtcManager) {
            webrtcManager.close();
            webrtcManager = null;
        }
        hasStream = false;
        isVideoPlaying = false;
        peerConnectionDegraded = false;
        needsPlayClick = false;
        clearMediaStallTimer();
        clearKeyframeNudge();
        clearSubscriptionRetryTimer();
        currentRtt = null;
        currentVideoBufferDelay = null;
        currentReceiverJitterTarget = null;
        currentReceiverPlayoutHint = null;
        loadUnderPressure = false;
        activeReviewToolCount = 0;
        micAutoEnablePending = false;
        clearMicPromptTimer();
        micPromptState = "hidden";
        if (controlsTimer) {
            clearTimeout(controlsTimer);
            controlsTimer = null;
        }
    }

    function handleTampering() {
        clearStoredSession();
        cleanupWebRTC();
        session.disconnect();
        endState = {
            title: "Session terminated",
            body: "This session was closed due to a content protection policy violation.",
        };
    }

    // Leave control + end-state exit: clear stored credentials for this room
    // and return to the join page.
    function clearStoredSession() {
        removeStorageItem("session", `chromatic_session_${slug}`);
    }

    function leaveToRoomPage() {
        clearStoredSession();
        goto(`/room/${slug}`);
    }

    // Controls auto-hide (ITEM 3): idle for CONTROLS_HIDE_DELAY_MS with the
    // cursor away from the bars fades them out fully; any pointer movement or
    // touch brings them back. They never hide while the cursor is over a bar,
    // a control has keyboard focus, or a popover (volume / participants / audio
    // settings) is open. Chat being open does NOT pin them — the chrome hides on
    // idle while the chat panel stays up (closed via its own X or the chat
    // button when the controls return).
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

    function handleMouseMove(e: MouseEvent) {
        // Tool mode: while a tool is ACTIVELY drawing (a mouse button is held),
        // moving is USING the tool, not asking for chrome — so don't reveal.
        // Plain movement (no button) always reveals, so the user is never locked
        // out of the controls (e.g. to turn the laser back off). Previously any
        // movement while the laser was merely SELECTED was suppressed except in
        // narrow edge zones, which hid the controls with no easy way back.
        if ((isLaserEnabled || isLoupeEnabled) && e.buttons !== 0) return;
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
    // Pin the controls open only for KEYBOARD focus: pointer clicks also
    // focus buttons, and treating that as "user is navigating the bar"
    // pinned the UI open after every click. :focus-visible is the
    // platform's own keyboard-vs-pointer distinction.
    function handleControlsFocusIn(e: FocusEvent) {
        const target = e.target as HTMLElement | null;
        controlsHaveFocus = !!target?.matches(":focus-visible");
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
            if (destroyed) return;
            audioInputs = devices.filter((d) => d.kind === "audioinput" && d.deviceId);
            audioOutputs = devices.filter((d) => d.kind === "audiooutput" && d.deviceId);
            videoInputs = devices.filter((d) => d.kind === "videoinput" && d.deviceId);
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
        const manager = webrtcManager;
        micSwitchPending = true;
        try {
            const ok = await manager.setMicDevice(deviceId);
            if (destroyed || webrtcManager !== manager) return;
            if (!ok) {
                console.warn("Could not switch to the selected microphone");
                return;
            }
            // setMicDevice acquired the mic, so permission is granted even if
            // the auto-request flow hadn't completed yet.
            hasMicPermission = true;
            activeMicId = manager.getCurrentMicDeviceId() ?? deviceId;
            storeMicDeviceId(deviceId);
            // Labels become available after the first successful capture.
            await refreshAudioDevices();
        } finally {
            if (!destroyed) {
                micSwitchPending = false;
            }
        }
    }

    // Switch talkback <-> studio. The manager re-acquires the mic with the new
    // constraints and renegotiates Opus (mono voice vs stereo hi-fi).
    async function selectAudioMode(mode: AudioMode) {
        if (!webrtcManager || audioModePending || mode === audioMode) return;
        const manager = webrtcManager;
        audioMode = mode; // optimistic; reconciled below
        audioModePending = true;
        try {
            await manager.setAudioMode(mode);
            if (destroyed || webrtcManager !== manager) return;
            audioMode = manager.getAudioMode();
        } finally {
            if (!destroyed) audioModePending = false;
        }
    }

    // Talkback noise-reduction engine. Rebuilds the mic chain; no renegotiation.
    async function selectDenoiser(engine: DenoiserEngine) {
        if (!webrtcManager || audioModePending || engine === denoiserEngine) return;
        const manager = webrtcManager;
        denoiserEngine = engine; // optimistic
        audioModePending = true;
        try {
            await manager.setDenoiserEngine(engine);
            if (destroyed || webrtcManager !== manager) return;
            denoiserEngine = manager.getDenoiserEngine();
        } finally {
            if (!destroyed) audioModePending = false;
        }
    }

    // Studio only: removing echo cancellation for headphone users (max fidelity).
    async function toggleStudioHeadphones() {
        if (!webrtcManager || audioModePending) return;
        const manager = webrtcManager;
        const next = !studioHeadphones;
        studioHeadphones = next; // optimistic
        audioModePending = true;
        try {
            await manager.setStudioHeadphones(next);
            if (destroyed || webrtcManager !== manager) return;
            studioHeadphones = manager.isStudioHeadphones();
        } finally {
            if (!destroyed) audioModePending = false;
        }
    }

    async function selectSpeakerDevice(deviceId: string) {
        selectedSpeakerId = deviceId;
        setStorageItem("local", SPEAKER_DEVICE_STORAGE_KEY, deviceId);
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
        if (isLaserEnabled) isLoupeEnabled = false;
    }

    // Safari shows native hover media controls over the share video and a
    // click (e.g. using the laser) pauses it — a paused live stream is never
    // meaningful, so resume immediately.
    function handleSharePause() {
        screenShareVideoEl?.play().catch(() => {});
    }

    function toggleFullscreen() {
        // The button is hidden on iPhone (no element Fullscreen API there, and
        // native <video> fullscreen would strip our overlays + watermark), but
        // feature-detect anyway so the call can never throw.
        if (typeof document.documentElement.requestFullscreen !== "function") return;
        if (document.fullscreenElement) {
            void document.exitFullscreen().catch(() => {});
        } else {
            void document.documentElement.requestFullscreen().catch(() => {});
        }
    }

    function toggleMute() {
        isMuted = !isMuted;
        if (videoElement) {
            videoElement.muted = isMuted;
        }
    }

    function toggleLoupe() {
        isLoupeEnabled = !isLoupeEnabled;
        if (isLoupeEnabled) isLaserEnabled = false;
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
        const manager = webrtcManager;

        if (!hasMicPermission) {
            const granted = await manager.requestMicrophone();
            if (destroyed || webrtcManager !== manager) return;
            if (granted) {
                hasMicPermission = true;
                micAutoEnablePending = false;
                isMicEnabled = true;
                manager.setMicEnabled(true);
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

    function muteAllParticipants() {
        for (const p of participants) {
            if (p.id !== sessionData?.participantId && p.audioEnabled) {
                session.send("admin:mute", { participantId: p.id, muted: true });
            }
        }
    }

    function approveAllWaiting() {
        for (const request of waitingRequests) {
            approveWaiting(request.participantId);
        }
    }

    // Frame grab: capture the current program frame, push it through the
    // normal chat file pipeline so it lands in history for everyone.
    async function grabFrame() {
        if (grabBusy || !videoElement || videoElement.readyState < 2 || !videoElement.videoWidth) {
            return;
        }
        const controller = new AbortController();
        grabAbortController = controller;
        grabBusy = true;
        grabFlash = true;
        if (grabFlashTimer) clearTimeout(grabFlashTimer);
        grabFlashTimer = setTimeout(() => {
            grabFlash = false;
            grabFlashTimer = null;
        }, 60);
        try {
            const canvas = document.createElement("canvas");
            canvas.width = videoElement.videoWidth;
            canvas.height = videoElement.videoHeight;
            canvas.getContext("2d")!.drawImage(videoElement, 0, 0);
            const blob = await new Promise<Blob | null>((resolve) =>
                canvas.toBlob(resolve, "image/jpeg", 0.92),
            );
            if (!blob) throw new Error("capture failed");
            const t = new Date();
            const stamp = [t.getHours(), t.getMinutes(), t.getSeconds()]
                .map((n) => n.toString().padStart(2, "0"))
                .join("");
            const file = new File([blob], `frame-${slug}-${stamp}.jpg`, { type: "image/jpeg" });
            const uploaded = await uploadFile(
                slug,
                file,
                undefined,
                controller.signal,
            );
            if (destroyed || grabAbortController !== controller) return;
            session.send("chat:file", {
                fileId: uploaded.id,
                name: uploaded.originalName,
                mimeType: uploaded.mimeType,
                url: uploaded.url,
                thumbnailUrl: uploaded.thumbnailUrl,
            });
            showGrabToast("Frame shared to chat");
        } catch {
            if (!destroyed && grabAbortController === controller) {
                showGrabToast("Could not capture the frame");
            }
        } finally {
            if (grabAbortController === controller) {
                grabAbortController = null;
            }
            if (!destroyed) {
                grabBusy = false;
            }
        }
    }

    function showGrabToast(text: string) {
        grabToast = text;
        if (grabToastTimer) clearTimeout(grabToastTimer);
        grabToastTimer = setTimeout(() => {
            grabToast = null;
            grabToastTimer = null;
        }, 2200);
    }

    function toggleUiSounds() {
        uiSounds = !uiSounds;
        setUiSoundsEnabled(uiSounds);
    }

    function toggleReduceTransparency() {
        reduceTransparency = !reduceTransparency;
        document.documentElement.classList.toggle("reduce-transparency", reduceTransparency);
        setStorageItem("local", "chromatic_reduce_transparency", reduceTransparency ? "on" : "off");
    }

    function setReviewMode(mode: ReviewQualityMode) {
        reviewQualityMode = mode;
        setReviewQualityMode(mode);
        setStorageItem("local", "chromatic_review_quality_mode", mode);
    }

    function smoothFrameDelay(current: number | null, value: number): number {
        return current === null ? value : current * 0.8 + value * 0.2;
    }

    function saneFrameDelay(value: number): number | null {
        return Number.isFinite(value) && value >= 0 && value < 60_000 ? value : null;
    }

    // Display fps and browser-provided WebRTC frame timing for the stats card,
    // measured only while it's open.
    $effect(() => {
        const video = videoElement;
        if (!showStats || !video || !("requestVideoFrameCallback" in video)) return;
        let frames = 0;
        let windowStart = performance.now();
        let handle = 0;
        const tick = (now: number, metadata: VideoFrameCallbackMetadata) => {
            frames++;
            if (now - windowStart >= 1000) {
                displayFps = Math.round((frames * 1000) / (now - windowStart));
                frames = 0;
                windowStart = now;
            }
            const displayTime = metadata.expectedDisplayTime;
            if (typeof metadata.captureTime === "number") {
                const delay = saneFrameDelay(displayTime - metadata.captureTime);
                if (delay !== null) {
                    frameCaptureToDisplayDelay = smoothFrameDelay(frameCaptureToDisplayDelay, delay);
                }
            }
            if (typeof metadata.receiveTime === "number") {
                const delay = saneFrameDelay(displayTime - metadata.receiveTime);
                if (delay !== null) {
                    frameReceiveToDisplayDelay = smoothFrameDelay(frameReceiveToDisplayDelay, delay);
                }
            }
            if (typeof metadata.processingDuration === "number") {
                frameProcessingDelay = smoothFrameDelay(
                    frameProcessingDelay,
                    metadata.processingDuration * 1000,
                );
            }
            handle = (video as any).requestVideoFrameCallback(tick);
        };
        handle = (video as any).requestVideoFrameCallback(tick);
        return () => {
            (video as any).cancelVideoFrameCallback?.(handle);
            displayFps = null;
            frameCaptureToDisplayDelay = null;
            frameReceiveToDisplayDelay = null;
            frameProcessingDelay = null;
        };
    });

    function toggleParticipantMute(participantId: string) {
        const muted = !adminMutedIds.has(participantId);
        session.send("admin:mute", { participantId, muted });
    }

    function toggleParticipantCam(participantId: string) {
        const disabled = !camDisabledIds.has(participantId);
        session.send("admin:disable-cam", { participantId, disabled });
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
            case "g":
                void grabFrame();
                break;
            case "z":
                toggleLoupe();
                break;
            case "s":
                showScopes = !showScopes;
                break;
            case "?":
                showShortcuts = !showShortcuts;
                break;
            case "escape":
                if (showShortcuts) {
                    showShortcuts = false;
                } else if (showMoreSheet) {
                    showMoreSheet = false;
                } else if (showStats) {
                    showStats = false;
                } else if (showAudioSettings) {
                    showAudioSettings = false;
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
        if (showStats) {
            if (
                !statsPopoverEl?.contains(target) &&
                !signalEl?.contains(target) &&
                !latencyEl?.contains(target)
            ) {
                showStats = false;
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
        // A review tool owns the touch (laser draws, loupe moves/pinches), so a
        // tap on the picture must NOT also toggle the chrome then.
        if (e.pointerType !== "touch" || isLaserEnabled || isLoupeEnabled) return;
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
    // True when anyone (self or remote) has a cam on — gates the cam-strip
    // sizing and the hide toggle.
    let anyCamActive = $derived(isCameraOn || remoteCamStreams.size > 0);
    // True when an admin has gated this user's own camera off.
    let myCamDisabled = $derived(camDisabledIds.has(sessionData?.participantId ?? ""));
    // One-time nudge to turn the camera on, shown after the mic is live (cams are
    // opt-in/default-off). Hidden once dismissed, the cam is on, or it's gated.
    let showCamNudge = $derived(
        isMicEnabled && !isCameraOn && !camNudgeDismissed && !myCamDisabled,
    );
    // Centered camera setup modal: pick a device, see a live preview, then
    // Enable (broadcast) or Dismiss. This is the camera "selector".
    let showCameraModal = $state(false);
    let camCaptureDenied = $state(false);

    // Join/leave chimes for everyone: watch the roster for deltas rather
    // than a specific message type (joins arrive via roster broadcasts).
    // The first non-empty roster is the initial sync, not arrivals; own
    // join/leave never chimes; bursts collapse into one chime.
    let knownParticipantIds: Set<string> | null = null;
    let lastRosterChimeAt = 0;
    $effect(() => {
        const ids = new Set(participants.map((p: { id: string }) => p.id));
        if (knownParticipantIds === null) {
            if (ids.size > 0) knownParticipantIds = ids;
            return;
        }
        const prev = knownParticipantIds;
        knownParticipantIds = ids;
        const self = sessionData?.participantId;
        let joined = false;
        let left = false;
        for (const id of ids) if (!prev.has(id) && id !== self) joined = true;
        for (const id of prev) if (!ids.has(id) && id !== self) left = true;
        const now = Date.now();
        if (now - lastRosterChimeAt < 800) return;
        if (joined) {
            playJoinChime();
            shimmerRequestedAt = Date.now();
            lastRosterChimeAt = now;
        } else if (left) {
            playLeaveChime();
            lastRosterChimeAt = now;
        }
    });
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
        displayedLatency === null ? null : displayedLatency < 100 ? "good" : displayedLatency <= 200 ? "fair" : "poor"
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

    // Controls stay visible while any of these hold (ITEM 3). Chat being open is
    // deliberately NOT here: the chrome should auto-hide on idle even with chat
    // up (the chat panel persists on its own), so the picture isn't crowded.
    let controlsPinned = $derived(
        isPointerOverControls ||
            controlsHaveFocus ||
            showStats ||
            showParticipantList ||
            showAudioSettings ||
            showMoreSheet
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

    // Track touch-ness and orientation for the mobile chrome + rotate hint.
    $effect(() => {
        if (typeof window === "undefined") return;
        const coarse = window.matchMedia("(pointer: coarse)");
        const portrait = window.matchMedia("(orientation: portrait)");
        supportsFullscreen = typeof document.documentElement.requestFullscreen === "function";
        const sync = () => {
            isCoarsePointer = coarse.matches;
            isPortrait = portrait.matches;
            // Re-read the aspect on rotate: the element box changed, and a hint
            // dismissed for the previous orientation shouldn't stick forever.
            captureVideoAspect();
        };
        sync();
        coarse.addEventListener("change", sync);
        portrait.addEventListener("change", sync);
        return () => {
            coarse.removeEventListener("change", sync);
            portrait.removeEventListener("change", sync);
        };
    });

    // Show the "rotate your phone" nudge only on a touch device when the stream's
    // aspect genuinely fights the current orientation: landscape content (≳5:4)
    // on a portrait phone, or tall content (≲4:5) on a landscape phone. Near-
    // square content is left alone (rotating wouldn't help). Dismissed per
    // orientation so it can re-appear if the mismatch flips on rotate.
    let rotateHintMismatch = $derived(
        videoAspect > 0 &&
            ((isPortrait && videoAspect >= 1.25) || (!isPortrait && videoAspect <= 0.8)),
    );
    let showRotateHint = $derived(
        isCoarsePointer &&
            hasStream &&
            isVideoPlaying &&
            rotateHintMismatch &&
            !rotateHintDismissed,
    );
    // Reset the dismissal whenever the mismatch direction changes (the user
    // rotated, or the content aspect changed), so a fresh mismatch nudges again.
    $effect(() => {
        void isPortrait;
        void rotateHintMismatch;
        rotateHintDismissed = false;
    });
    // Auto-dismiss the rotate hint after a few seconds so it never nags.
    $effect(() => {
        if (!showRotateHint) return;
        const t = setTimeout(() => (rotateHintDismissed = true), 6000);
        return () => clearTimeout(t);
    });

    // Device labels are empty until the browser has granted a capture —
    // used to show the permission hint in the audio settings popover.
    let micLabelsAvailable = $derived(audioInputs.some((d) => d.label !== ""));

    // The <select> value MUST be a deviceId that exists in the option list, or
    // the native control silently shows the first option (misrepresenting the
    // active mic, and a click on that shown item fires no change event). Fall
    // back to the first enumerated device when activeMicId doesn't match.
    let micSelectValue = $derived(
        audioInputs.some((d) => d.deviceId === activeMicId)
            ? activeMicId
            : (audioInputs[0]?.deviceId ?? ""),
    );

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

<main
    class="session-page"
    class:controls-hidden={!isControlsVisible}
    class:chat-open={isChatOpen}
    class:loupe-on={isLoupeEnabled}
    onmousemove={handleMouseMove}
    onpointerdown={handlePagePointerDown}
>
    <!-- Settings panel content, declared at top level so it can render in BOTH
         the desktop popover (anchored above the control bar) and the phone
         "More" bottom sheet (a top-level overlay). -->
    {#snippet settingsContent()}
        <div class="audio-settings-section">
            <span class="audio-settings-title">Microphone</span>
            {#if audioInputs.length > 0 && micLabelsAvailable}
                <!-- Collapsed to the selected device + caret; the native dropdown
                     scrolls cleanly even with many devices (vs. a long inline list). -->
                <select
                    class="device-select"
                    value={micSelectValue}
                    disabled={micSwitchPending}
                    aria-label="Microphone device"
                    onchange={(e) => selectMicDevice((e.currentTarget as HTMLSelectElement).value)}
                >
                    {#each audioInputs as device (device.deviceId)}
                        <option value={device.deviceId}>{device.label || "Microphone"}</option>
                    {/each}
                </select>
            {:else if hasMicPermission}
                <p class="audio-settings-hint">No microphones found.</p>
            {:else}
                <p class="audio-settings-hint">
                    Allow microphone access to see and choose your input devices.
                </p>
                <button class="audio-settings-grant" onclick={requestMicForLabels}>
                    Enable microphone
                </button>
            {/if}
        </div>
        <div class="audio-settings-section">
            <span class="audio-settings-title">Camera</span>
            <button class="audio-settings-grant" onclick={openCameraModal}>
                {isCameraOn ? "Camera settings" : "Set up camera"}
            </button>
        </div>
        {#if supportsSinkSelection && audioOutputs.length > 0}
            <div class="audio-settings-section">
                <span class="audio-settings-title">Speaker</span>
                <select
                    class="device-select"
                    value={selectedSpeakerId ?? "default"}
                    aria-label="Speaker device"
                    onchange={(e) => selectSpeakerDevice((e.currentTarget as HTMLSelectElement).value)}
                >
                    {#each audioOutputs as device (device.deviceId)}
                        <option value={device.deviceId}>{device.label || "Speaker"}</option>
                    {/each}
                </select>
            </div>
        {/if}
        <div class="audio-settings-section">
            <span class="audio-settings-title">Volume</span>
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
        <div class="audio-settings-section">
            <span class="audio-settings-title">Audio mode</span>
            <div class="audio-mode-toggle" role="group" aria-label="Audio mode">
                <button
                    class="audio-mode-option"
                    class:selected={audioMode === "talkback"}
                    disabled={audioModePending}
                    onclick={() => selectAudioMode("talkback")}
                    aria-pressed={audioMode === "talkback"}
                >
                    <span class="audio-mode-name">Talkback</span>
                    <span class="audio-mode-desc">Clean voice, ultra-low latency</span>
                </button>
                <button
                    class="audio-mode-option"
                    class:selected={audioMode === "studio"}
                    disabled={audioModePending}
                    onclick={() => selectAudioMode("studio")}
                    aria-pressed={audioMode === "studio"}
                >
                    <span class="audio-mode-name">Studio</span>
                    <span class="audio-mode-desc">Pristine stereo for music &amp; instruments</span>
                </button>
            </div>
            {#if audioMode === "talkback"}
                <label class="pref-row">
                    <span>Noise reduction</span>
                    <span class="seg" role="group" aria-label="Noise reduction">
                        <button
                            class="seg-btn"
                            class:selected={denoiserEngine !== "off"}
                            disabled={audioModePending}
                            onclick={() => selectDenoiser("rnnoise")}
                            aria-pressed={denoiserEngine !== "off"}
                        >On</button>
                        <button
                            class="seg-btn"
                            class:selected={denoiserEngine === "off"}
                            disabled={audioModePending}
                            onclick={() => selectDenoiser("off")}
                            aria-pressed={denoiserEngine === "off"}
                        >Off</button>
                    </span>
                </label>
            {/if}
            {#if audioMode === "studio"}
                <label class="pref-row">
                    <span>On headphones (disable echo cancellation)</span>
                    <input
                        type="checkbox"
                        class="switch"
                        checked={studioHeadphones}
                        disabled={audioModePending}
                        onchange={toggleStudioHeadphones}
                    />
                </label>
                <p class="audio-settings-hint">
                    Studio sends full-bandwidth stereo with no noise processing. Use headphones to avoid echo.
                </p>
            {/if}
        </div>
        <div class="audio-settings-section">
            <span class="audio-settings-title">Preferences</span>
            <label class="pref-row">
                <span>UI sounds</span>
                <input
                    type="checkbox"
                    class="switch"
                    checked={uiSounds}
                    onchange={toggleUiSounds}
                />
            </label>
            <label class="pref-row">
                <span>Reduce transparency</span>
                <input
                    type="checkbox"
                    class="switch"
                    checked={reduceTransparency}
                    onchange={toggleReduceTransparency}
                />
            </label>
        </div>
    {/snippet}

    <!-- Phone "More" bottom sheet: secondary tools + the full settings panel,
         collapsed off the cramped control bar. Top-level overlay (z-index honored
         against the other overlays); only reachable on phones (More button hidden
         elsewhere). -->
    {#if showMoreSheet}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
            class="more-sheet-backdrop"
            transition:fade={{ duration: 150 }}
            role="presentation"
            onclick={() => (showMoreSheet = false)}
            onkeydown={(e) => {
                if (e.key === "Escape") showMoreSheet = false;
            }}
        >
            <div
                class="more-sheet"
                role="dialog"
                aria-label="More controls"
                tabindex="-1"
                transition:fly={{ y: 28, duration: 240, easing: quintOut }}
                onclick={(e) => e.stopPropagation()}
                onkeydown={(e) => e.stopPropagation()}
            >
                <span class="more-sheet-handle" aria-hidden="true"></span>
                <div class="more-tools" role="group" aria-label="Tools">
                    <button
                        class="more-tool"
                        disabled={grabBusy || !hasStream}
                        onclick={() => { void grabFrame(); showMoreSheet = false; }}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22"><path d="M14.5 4h-5L7 7H4a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2V9a2 2 0 0 0-2-2h-3l-2.5-3z"/><circle cx="12" cy="13" r="3"/></svg>
                        <span>Grab</span>
                    </button>
                    <button
                        class="more-tool"
                        class:active={isLoupeEnabled}
                        onclick={() => { toggleLoupe(); showMoreSheet = false; }}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22"><circle cx="11" cy="11" r="8"/><line x1="21" x2="16.65" y1="21" y2="16.65"/><line x1="11" x2="11" y1="8" y2="14"/><line x1="8" x2="14" y1="11" y2="11"/></svg>
                        <span>Loupe</span>
                    </button>
                    <button
                        class="more-tool"
                        class:active={showScopes}
                        onclick={() => { showScopes = !showScopes; showMoreSheet = false; }}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22"><path d="M22 12h-2.48a2 2 0 0 0-1.93 1.46l-2.35 8.36a.25.25 0 0 1-.48 0L9.24 2.18a.25.25 0 0 0-.48 0l-2.35 8.36A2 2 0 0 1 4.49 12H2"/></svg>
                        <span>Scopes</span>
                    </button>
                    <button
                        class="more-tool"
                        class:active={screenShareActive}
                        disabled={isScreenShareDisabled}
                        onclick={() => { toggleScreenShare(); showMoreSheet = false; }}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22"><path d="m9 10 3-3 3 3"/><path d="M12 13V7"/><rect width="20" height="14" x="2" y="3" rx="2"/><path d="M12 17v4"/><path d="M8 21h8"/></svg>
                        <span>{screenShareActive ? "Stop" : "Share"}</span>
                    </button>
                </div>
                <div class="more-sheet-settings">
                    {@render settingsContent()}
                </div>
            </div>
        </div>
    {/if}

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
                    onplaying={() => (shareVideoReady = true)}
                ></video>

                {#if !shareVideoReady}
                    <div class="share-loading-pane" transition:fade={{ duration: 150 }}>
                        <span class="connect-dots" aria-hidden="true"><span></span><span></span><span></span></span>
                        Loading screen share
                    </div>
                {/if}

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
                            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="13" height="13"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>
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
                <LoupeOverlay {videoElement} shareElement={screenShareVideoEl} enabled={isLoupeEnabled} onExit={() => (isLoupeEnabled = false)} />
            {/if}

            {#if grabFlash}
                <div class="grab-flash" out:fade={{ duration: 220 }}></div>
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
                    <p class="stream-card-body">The stream is ready. Your browser just needs one tap to begin playback.</p>
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
                    <p class="stream-card-body">We couldn't reach the session after automatic recovery attempts.</p>
                    <button class="btn btn-primary" onclick={() => window.location.reload()}>
                        Refresh page
                    </button>
                </div>
            </div>
        {:else if overlayState === 'reconnecting'}
            <div class="stream-status-overlay" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <div class="connect-dots" aria-hidden="true"><span></span><span></span><span></span></div>
                    <h2 class="stream-card-title">{session.state.networkOffline ? "Network offline" : "Reconnecting"}</h2>
                    <p class="stream-card-body">
                        {session.state.networkOffline
                            ? "Waiting for this browser's network to return. The session will reconnect automatically."
                            : "Restoring signaling and rebuilding the media path..."}
                    </p>
                    <p class="stream-card-meta">
                        {session.state.networkOffline
                            ? `Standing by · media rebuilds ${resubscribeEvents}`
                            : `WebSocket attempt ${session.state.reconnectAttempt} · media rebuilds ${resubscribeEvents}`}
                    </p>
                </div>
            </div>
        {:else if overlayState === 'paused'}
            <div class="stream-status-overlay" transition:fade={{ duration: 150 }}>
                <div class="stream-card">
                    <div class="stream-card-icon" aria-hidden="true">
                        <svg viewBox="0 0 24 24" fill="currentColor" width="22" height="22"><path d="M6 19h4V5H6v14zm8-14v14h4V5h-4z"/></svg>
                    </div>
                    <h2 class="stream-card-title">Stream paused</h2>
                    <p class="stream-card-body">The host's connection was interrupted. The stream will resume automatically.</p>
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
                    <span class="screenshare-approval-text">Share approved. Choose what to share</span>
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
                            <p class="mic-prompt-text">You can watch and listen. Enable your mic anytime to speak.</p>
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
                {#if waitingRequests.length > 1}
                    <button class="waiting-approve-all" onclick={approveAllWaiting}>
                        Approve all ({waitingRequests.length})
                    </button>
                {/if}
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
        <!-- Centered camera setup modal: device picker + live preview + Enable. -->
        {#if showCameraModal}
            <div
                class="cam-modal-backdrop"
                transition:fade={{ duration: 150 }}
                role="presentation"
                onclick={closeCameraModal}
                onkeydown={(e) => {
                    if (e.key === "Escape") closeCameraModal();
                }}
            >
                <div
                    class="cam-modal"
                    role="dialog"
                    aria-modal="true"
                    aria-label="Camera setup"
                    tabindex="-1"
                    onclick={(e) => e.stopPropagation()}
                    onkeydown={(e) => e.stopPropagation()}
                >
                    <h2 class="cam-modal-title">Camera</h2>
                    <div class="cam-modal-preview">
                        {#if selfCamStream}
                            <!-- svelte-ignore a11y_media_has_caption -->
                            <video
                                class="cam-modal-video"
                                use:bindStream={selfCamStream}
                                muted
                                autoplay
                                playsinline
                            ></video>
                        {:else if camCaptureDenied}
                            <span class="cam-modal-empty">Camera blocked — allow access in your browser.</span>
                        {:else}
                            <span class="cam-modal-empty">Starting preview…</span>
                        {/if}
                    </div>
                    {#if videoInputs.length > 1}
                        <select
                            class="device-select"
                            value={activeCameraId}
                            aria-label="Camera device"
                            onchange={(e) => selectCameraDevice((e.currentTarget as HTMLSelectElement).value)}
                        >
                            {#each videoInputs as d (d.deviceId)}
                                <option value={d.deviceId}>{d.label || "Camera"}</option>
                            {/each}
                        </select>
                    {/if}
                    <div class="cam-modal-actions">
                        <button class="btn btn-secondary" onclick={closeCameraModal}>Dismiss</button>
                        <button class="btn btn-primary" onclick={enableCameraFromModal} disabled={cameraPending || myCamDisabled}>
                            {isCameraOn ? "Done" : "Enable camera"}
                        </button>
                    </div>
                </div>
            </div>
        {/if}

        <!-- Rotate hint: only on a phone whose orientation fights the stream's
             aspect (e.g. 16:9 on a portrait phone). iOS can't lock orientation,
             so we nudge rather than force; tap to dismiss, auto-hides, and it
             re-evaluates on rotate. -->
        {#if showRotateHint}
            <button
                class="rotate-hint"
                transition:fade={{ duration: 200 }}
                onclick={() => (rotateHintDismissed = true)}
                aria-label="Dismiss rotate hint"
            >
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="18" height="18"><path d="M2 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M22 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M3 21v-5h5"/></svg>
                <span>{isPortrait ? "Rotate for a bigger view" : "Rotate upright for a bigger view"}</span>
            </button>
        {/if}

        <!-- One-time prompt after the mic goes live: cameras are opt-in, so
             invite the user to turn theirs on. Centered on screen (a deliberate
             first-run prompt, distinct from the on-click camera setup) and it
             stays put when the controls auto-hide. "Set up camera" opens the
             centered picker/preview modal. -->
        {#if showCamNudge}
            <div class="cam-nudge" transition:fade={{ duration: 150 }}>
                <button
                    class="cam-nudge-dismiss"
                    onclick={() => (camNudgeDismissed = true)}
                    aria-label="Not now"
                    title="Not now"
                >
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="14" height="14"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>
                </button>
                <span class="cam-nudge-icon" aria-hidden="true">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22"><path d="M23 7l-7 5 7 5V7z"/><rect x="1" y="5" width="15" height="14" rx="2" ry="2"/></svg>
                </span>
                <span class="cam-nudge-text">Turn your camera on?</span>
                <span class="cam-nudge-sub">Let everyone see you during the review.</span>
                <button class="cam-nudge-enable" onclick={openCameraModal}>Set up camera</button>
            </div>
        {/if}

        <!-- Persistent floating cam strip: shown whenever cameras are on and not
             hidden. Unlike the top-bar dots it lives OUTSIDE .controls-overlay,
             so it stays up when the UI auto-hides — sliding flush into the
             corner then, and back to its padded spot when the UI returns. When
             chat is open it shifts left by the chat-panel width so it sits over
             the video beside chat (chat also stacks above it). pointer-events:
             none so it never blocks the laser/loupe on the video beneath; only
             the hide button is live. -->
        {#if anyCamActive}
            <div class="cam-float" class:flush={!isControlsVisible} class:chat-open={isChatOpen}>
                {#each participants.slice(0, 12) as p (p.id)}
                    {@const isSelf = p.id === sessionData?.participantId}
                    {@const camStream = isSelf
                        ? (isCameraOn ? selfCamStream : null)
                        : (remoteCamStreams.get(p.id) ?? null)}
                    <div class="cam-float-tile">
                        <div
                            class="cam-float-circle"
                            class:speaking={speakingParticipants.has(p.id)}
                            class:muted={!p.audioEnabled}
                            class:has-cam={!!camStream}
                            style="--participant-color: {p.color}"
                            title="{p.name}{isSelf ? ' (you)' : ''}"
                        >
                            {#if camStream}
                                <!-- svelte-ignore a11y_media_has_caption -->
                                <video
                                    class="cam-video"
                                    class:mirror={isSelf}
                                    use:bindStream={camStream}
                                    muted
                                    autoplay
                                    playsinline
                                ></video>
                            {:else}
                                {p.name.charAt(0).toUpperCase()}
                            {/if}
                        </div>
                        <span class="cam-float-name">{isSelf ? "You" : p.name}</span>
                    </div>
                {/each}
                {#if participants.length > 12}
                    <span class="cam-float-overflow">+{participants.length - 12}</span>
                {/if}
                <!-- Fast self-cam toggle: flips YOUR webcam on/off for everyone
                     using the already-selected device — no picker dialog (that's
                     the control-bar Camera button's job). -->
                <button
                    class="cam-hide-btn cam-float-hide"
                    onclick={() => toggleCamera()}
                    disabled={cameraPending || myCamDisabled}
                    aria-label={isCameraOn ? "Turn my camera off" : "Turn my camera on"}
                    title={myCamDisabled ? "Camera disabled by host" : isCameraOn ? "Turn my camera off" : "Turn my camera on"}
                >
                    {#if isCameraOn}
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="14" height="14"><path d="M23 7l-7 5 7 5V7z"/><rect x="1" y="5" width="15" height="14" rx="2" ry="2"/></svg>
                    {:else}
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="14" height="14"><line x1="2" x2="22" y1="2" y2="22"/><path d="M10.66 5H14a2 2 0 0 1 2 2v3.34l1 1L23 7v10"/><path d="M16 16a2 2 0 0 1-2 2H3a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2h2"/></svg>
                    {/if}
                </button>
            </div>
        {/if}

        <div
            class="controls-overlay"
            class:visible={isControlsVisible}
            onfocusin={handleControlsFocusIn}
            onfocusout={handleControlsFocusOut}
        >
            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <div
                class="top-bar"
                bind:this={topBarEl}
                use:videoGlassGroup={{
                    getVideo: () => videoElement,
                    isEnabled: glassEnabled,
                    items: topGlassItems,
                    shimmerAt: () => shimmerRequestedAt,
                }}
                onpointerenter={handleBarsPointerEnter}
                onpointerleave={handleBarsPointerLeave}
            >
                <div class="room-name" bind:this={roomNameEl}>{roomState?.name || "Session"}</div>
                <div class="top-bar-right">
                    <!-- Compact presence dots in the top bar. Shown only when the
                         floating cam strip isn't active (i.e. no cams, or cams
                         hidden) — then it behaves like before and hides with the
                         UI. When cams are showing, the persistent .cam-float
                         below takes over. -->
                    {#if participants.length > 1 && !anyCamActive}
                        <div
                            class="presence-row"
                            bind:this={presenceRowEl}
                        >
                            {#each participants.slice(0, 8) as p (p.id)}
                                {@const isSelf = p.id === sessionData?.participantId}
                                <span
                                    class="presence-dot"
                                    class:speaking={speakingParticipants.has(p.id)}
                                    class:muted={!p.audioEnabled}
                                    style="--participant-color: {p.color}"
                                    title="{p.name}{isSelf ? ' (you)' : ''}{p.audioEnabled ? '' : ' (muted)'}"
                                >
                                    {p.name.charAt(0).toUpperCase()}
                                </span>
                            {/each}
                            {#if participants.length > 8}
                                <span class="presence-overflow">+{participants.length - 8}</span>
                            {/if}
                        </div>
                    {/if}
                    <button
                        class="participant-count"
                        bind:this={participantCountEl}
                        onclick={toggleParticipantList}
                        class:active={showParticipantList}
                        aria-label="Participants ({participants.length})"
                        aria-expanded={showParticipantList}
                        aria-haspopup="dialog"
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="15" height="15"><path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/></svg>
                        {participants.length}
                    </button>
                </div>
            </div>

            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <div
                class="bottom-bar"
                bind:this={bottomBarEl}
                use:videoGlassGroup={{
                    getVideo: () => videoElement,
                    isEnabled: glassEnabled,
                    items: bottomGlassItems,
                    shimmerAt: () => shimmerRequestedAt,
                }}
                onpointerenter={handleBarsPointerEnter}
                onpointerleave={handleBarsPointerLeave}
            >
                <div class="bottom-left" aria-hidden="true"></div>

                <!-- Main control bar - large, obvious buttons with labels -->
                <div class="control-bar-anchor">
                {#if showAudioSettings}
                    <div
                        class="audio-settings-popover"
                        use:liquidLens={POPOVER_LENS}
                        bind:this={audioSettingsPopoverEl}
                        transition:scale={{ start: 0.95, duration: 260, easing: quintOut }}
                        role="dialog"
                        aria-label="Settings"
                    >
                        {@render settingsContent()}
                    </div>
                {/if}

                <div class="control-bar" bind:this={controlBarEl}>
                    <button
                        class="control-btn"
                        style="--stagger: 0ms"
                        class:active={isMicEnabled}
                        class:off={!isMicEnabled}
                        onclick={toggleMic}
                        aria-pressed={isMicEnabled}
                        aria-label="Microphone (M)"
                        use:tooltip={"Microphone (M)"}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            {#if isMicEnabled}
                                <path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><line x1="12" x2="12" y1="19" y2="22"/>
                            {:else}
                                <line x1="2" x2="22" y1="2" y2="22"/><path d="M18.89 13.23A7.12 7.12 0 0 0 19 12v-2"/><path d="M5 10v2a7 7 0 0 0 12 5"/><path d="M15 9.34V5a3 3 0 0 0-5.68-1.33"/><path d="M9 9v3a3 3 0 0 0 5.12 2.12"/><line x1="12" x2="12" y1="19" y2="22"/>
                            {/if}
                        </svg>
                        <span class="control-label">{isMicEnabled ? "Mic On" : "Mic Off"}</span>
                    </button>

                    <button
                        class="control-btn"
                        style="--stagger: 9ms"
                        class:active={isCameraOn}
                        class:off={!isCameraOn}
                        onclick={onCameraButton}
                        disabled={cameraPending || myCamDisabled}
                        aria-pressed={isCameraOn}
                        aria-label="Camera"
                        use:tooltip={myCamDisabled
                            ? "Camera disabled by host"
                            : isCameraOn
                              ? "Turn camera off"
                              : "Turn camera on"}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            {#if isCameraOn}
                                <path d="M23 7l-7 5 7 5V7z"/><rect x="1" y="5" width="15" height="14" rx="2" ry="2"/>
                            {:else}
                                <line x1="2" x2="22" y1="2" y2="22"/><path d="M10.66 5H14a2 2 0 0 1 2 2v3.34l1 1L23 7v10"/><path d="M16 16a2 2 0 0 1-2 2H3a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2h2"/>
                            {/if}
                        </svg>
                        <span class="control-label">{isCameraOn ? "Cam On" : "Cam Off"}</span>
                    </button>

                    <!-- Program (stream) audio mute — a dedicated, visually
                         distinct control so clients can't mistake it for their
                         own mic or the settings gear. -->
                    <button
                        class="control-btn program-audio-btn"
                        style="--stagger: 14ms"
                        class:off={isMuted}
                        onclick={toggleMute}
                        aria-pressed={!isMuted}
                        aria-label="Stream audio (mute/unmute)"
                        use:tooltip={isMuted ? "Unmute stream audio" : "Mute stream audio"}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            {#if isMuted}
                                <polygon points="11 5 6 9 2 9 2 15 6 15 11 19 11 5"/><line x1="23" x2="17" y1="9" y2="15"/><line x1="17" x2="23" y1="9" y2="15"/>
                            {:else}
                                <polygon points="11 5 6 9 2 9 2 15 6 15 11 19 11 5"/><path d="M15.54 8.46a5 5 0 0 1 0 7.07"/><path d="M19.07 4.93a10 10 0 0 1 0 14.14"/>
                            {/if}
                        </svg>
                        <span class="control-label">{isMuted ? "Stream Muted" : "Stream Audio"}</span>
                    </button>

                    <button
                        class="control-btn secondary-tool"
                        style="--stagger: 18ms"
                        class:active={showAudioSettings}
                        onclick={toggleAudioSettings}
                        bind:this={audioSettingsBtnEl}
                        aria-label="Settings"
                        aria-expanded={showAudioSettings}
                        aria-haspopup="dialog"
                        use:tooltip={"Volume, devices and preferences"}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            <path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>
                        </svg>
                        <span class="control-label">Settings</span>
                    </button>

                    <button
                        class="control-btn chat-btn"
                        style="--stagger: 36ms"
                        class:active={isChatOpen}
                        class:pulse={chatPulseActive}
                        onclick={toggleChat}
                        aria-pressed={isChatOpen}
                        aria-label="Chat (C)"
                        use:tooltip={"Chat (C)"}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            <path d="M7.9 20A9 9 0 1 0 4 16.1L2 22Z"/>
                        </svg>
                        <span class="control-label">Chat</span>
                        {#if unreadCount > 0}
                            {#key unreadCount}
                                <span class="chat-badge">{unreadCount > 9 ? '9+' : unreadCount}</span>
                            {/key}
                        {/if}
                    </button>

                    <span class="bar-divider" aria-hidden="true"></span>

                    <button
                        class="control-btn secondary-tool"
                        style="--stagger: 54ms"
                        disabled={grabBusy || !hasStream}
                        onclick={() => void grabFrame()}
                        use:tooltip={"Grab the current frame to chat (G)"}
                        aria-label="Grab frame (G)"
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            <path d="M14.5 4h-5L7 7H4a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2V9a2 2 0 0 0-2-2h-3l-2.5-3z"/><circle cx="12" cy="13" r="3"/>
                        </svg>
                        <span class="control-label">Grab</span>
                    </button>

                    <button
                        class="control-btn laser-btn"
                        style="--stagger: 72ms"
                        class:active={isLaserEnabled}
                        onclick={toggleLaser}
                        use:tooltip={"Laser pointer (L)"}
                        aria-pressed={isLaserEnabled}
                        aria-label="Laser pointer (L)"
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            <circle cx="12" cy="12" r="10"/><line x1="22" x2="18" y1="12" y2="12"/><line x1="6" x2="2" y1="12" y2="12"/><line x1="12" x2="12" y1="6" y2="2"/><line x1="12" x2="12" y1="22" y2="18"/>
                        </svg>
                        <span class="control-label">Laser</span>
                    </button>

                    <button
                        class="control-btn secondary-tool"
                        style="--stagger: 90ms"
                        class:active={isLoupeEnabled}
                        onclick={toggleLoupe}
                        use:tooltip={"Pixel loupe, scroll to zoom (Z)"}
                        aria-pressed={isLoupeEnabled}
                        aria-label="Loupe (Z)"
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            <circle cx="11" cy="11" r="8"/><line x1="21" x2="16.65" y1="21" y2="16.65"/><line x1="11" x2="11" y1="8" y2="14"/><line x1="8" x2="14" y1="11" y2="11"/>
                        </svg>
                        <span class="control-label">Loupe</span>
                    </button>

                    <button
                        class="control-btn secondary-tool"
                        style="--stagger: 108ms"
                        class:active={showScopes}
                        onclick={() => (showScopes = !showScopes)}
                        use:tooltip={"Waveform and RGB parade (S)"}
                        aria-pressed={showScopes}
                        aria-label="Scopes (S)"
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            <path d="M22 12h-2.48a2 2 0 0 0-1.93 1.46l-2.35 8.36a.25.25 0 0 1-.48 0L9.24 2.18a.25.25 0 0 0-.48 0l-2.35 8.36A2 2 0 0 1 4.49 12H2"/>
                        </svg>
                        <span class="control-label">Scopes</span>
                    </button>

                    <span class="bar-divider" aria-hidden="true"></span>

                    <button
                        class="control-btn secondary-tool"
                        style="--stagger: 126ms"
                        class:active={screenShareActive}
                        class:requesting={screenShareRequested}
                        disabled={isScreenShareDisabled}
                        onclick={toggleScreenShare}
                        use:tooltip={isScreenShareDisabled ? "Screen share in progress" : screenShareActive ? "Stop sharing" : "Share screen"}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            <path d="m9 10 3-3 3 3"/><path d="M12 13V7"/><rect width="20" height="14" x="2" y="3" rx="2"/><path d="M12 17v4"/><path d="M8 21h8"/>
                        </svg>
                        <span class="control-label">{screenShareActive ? "Stop Share" : screenShareRequested ? "Pending..." : "Share"}</span>
                    </button>

                    {#if supportsFullscreen}
                        <button
                            class="control-btn desktop-only"
                            style="--stagger: 144ms"
                            onclick={toggleFullscreen}
                            aria-pressed={isFullscreen}
                            aria-label={isFullscreen ? "Exit fullscreen (F)" : "Fullscreen (F)"}
                            use:tooltip={"Fullscreen (F)"}
                        >
                            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                                {#if isFullscreen}
                                    <path d="M8 3v3a2 2 0 0 1-2 2H3"/><path d="M21 8h-3a2 2 0 0 1-2-2V3"/><path d="M3 16h3a2 2 0 0 1 2 2v3"/><path d="M16 21v-3a2 2 0 0 1 2-2h3"/>
                                {:else}
                                    <path d="M8 3H5a2 2 0 0 0-2 2v3"/><path d="M21 8V5a2 2 0 0 0-2-2h-3"/><path d="M3 16v3a2 2 0 0 0 2 2h3"/><path d="M16 21h3a2 2 0 0 0 2-2v-3"/>
                                {/if}
                            </svg>
                            <span class="control-label">{isFullscreen ? "Exit Full" : "Fullscreen"}</span>
                        </button>
                    {/if}

                    <!-- Phone-only: collapse the secondary tools + all settings
                         into a labeled bottom sheet so the bar fits a 390px bar. -->
                    <button
                        class="control-btn more-btn"
                        style="--stagger: 144ms"
                        class:active={showMoreSheet}
                        onclick={() => { showMoreSheet = !showMoreSheet; if (showMoreSheet) showAudioSettings = false; }}
                        aria-label="More controls"
                        aria-expanded={showMoreSheet}
                        aria-haspopup="dialog"
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            <circle cx="5" cy="12" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="19" cy="12" r="1.6"/>
                        </svg>
                        <span class="control-label">More</span>
                    </button>

                    <button
                        class="control-btn leave-btn"
                        style="--stagger: 162ms"
                        onclick={leaveToRoomPage}
                        aria-label="Leave session"
                        use:tooltip={"Leave the session"}
                    >
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                            <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" x2="9" y1="12" y2="12"/>
                        </svg>
                        <span class="control-label">Leave</span>
                    </button>
                </div>
                </div>

                <div class="bottom-right">
                    {#if showStats}
                        <div
                            class="stats-popover"
                            use:liquidLens={POPOVER_LENS}
                            bind:this={statsPopoverEl}
                            transition:scale={{ start: 0.95, duration: 260, easing: quintOut }}
                            role="dialog"
                            aria-label="Stream statistics"
                        >
                            <div class="stats-row">
                                <span>Resolution</span>
                                <span>{videoElement?.videoWidth || 0}×{videoElement?.videoHeight || 0}</span>
                            </div>
                            <div class="stats-row">
                                <span>Frame rate</span>
                                <span>{displayFps !== null ? `${displayFps} fps` : "measuring"}</span>
                            </div>
                            <div class="stats-row">
                                <span>{displayedLatencySource}</span>
                                <span>{displayedLatency !== null ? `~${Math.round(displayedLatency)} ms` : "n/a"}</span>
                            </div>
                            <div class="stats-row">
                                <span>Network RTT</span>
                                <span>{currentRtt !== null ? `~${Math.round(currentRtt)} ms` : "n/a"}</span>
                            </div>
                            <div class="stats-row">
                                <span>Frame delay</span>
                                <span>{frameCaptureToDisplayDelay !== null ? `~${Math.round(frameCaptureToDisplayDelay)} ms` : "n/a"}</span>
                            </div>
                            <div class="stats-row">
                                <span>Post-receive</span>
                                <span>{frameReceiveToDisplayDelay !== null ? `~${Math.round(frameReceiveToDisplayDelay)} ms` : "n/a"}</span>
                            </div>
                            <div class="stats-row">
                                <span>Buffer target</span>
                                <span>{currentReceiverJitterTarget !== null ? `${Math.round(currentReceiverJitterTarget)} ms` : currentReceiverPlayoutHint !== null ? `${Math.round(currentReceiverPlayoutHint)} ms` : "n/a"}</span>
                            </div>
                            <div class="stats-row">
                                <span>Decode</span>
                                <span>{frameProcessingDelay !== null ? `~${Math.round(frameProcessingDelay)} ms` : "n/a"}</span>
                            </div>
                            <div class="stats-row">
                                <span>Connection</span>
                                <span class="stats-quality {connectionQuality ?? ''}">{connectionQuality ?? "n/a"}</span>
                            </div>
                            <div class="stats-row">
                                <span>Load</span>
                                <span>{loadUnderPressure ? "pressure" : "normal"} · {activeReviewToolCount} tools</span>
                            </div>
                            <div class="stats-row">
                                <span>Long frames</span>
                                <span>{longFrameCount}{worstLongFrameMs !== null ? ` · worst ${Math.round(worstLongFrameMs)} ms` : ""}</span>
                            </div>
                            <div class="stats-row">
                                <span>Recovery</span>
                                <span>{reconnectEvents} ws · {resubscribeEvents} media</span>
                            </div>
                            <label class="stats-mode">
                                <span>Review mode</span>
                                <select
                                    aria-label="Review tool performance mode"
                                    value={reviewQualityMode}
                                    onchange={(e) => setReviewMode((e.currentTarget as HTMLSelectElement).value as ReviewQualityMode)}
                                >
                                    <option value="performance">Performance</option>
                                    <option value="balanced">Balanced</option>
                                    <option value="fidelity">Max fidelity</option>
                                </select>
                            </label>
                            <button
                                class="stats-reload"
                                onclick={() => {
                                    handleResync();
                                    showStats = false;
                                }}
                            >
                                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="13" height="13" aria-hidden="true"><path d="M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/></svg>
                                Reload stream
                            </button>
                        </div>
                    {/if}
                    {#if isLive}
                        <span class="live-pill" bind:this={livePillEl}><span class="live-dot" aria-hidden="true"></span>Live</span>
                    {/if}
                    {#if hasStream}
                        <button
                            bind:this={signalEl}
                            class="signal-indicator {connectionQuality ?? ''}"
                            onclick={() => (showStats = !showStats)}
                            use:tooltip={"Stream statistics"}
                            aria-label={connectionQuality && currentRtt !== null
                                ? `Connection: ${connectionQuality} (${Math.round(currentRtt)}ms). Stream statistics`
                                : "Stream statistics"}
                            aria-expanded={showStats}
                            aria-haspopup="dialog"
                        >
                            <span class="signal-bar"></span>
                            <span class="signal-bar"></span>
                            <span class="signal-bar"></span>
                        </button>
                    {/if}
                    {#if isAdmin && displayedLatency !== null}
                        <button
                            bind:this={latencyEl}
                            class="latency-display"
                            class:good={displayedLatencyQuality === "good"}
                            class:warning={displayedLatencyQuality === "fair"}
                            class:bad={displayedLatencyQuality === "poor"}
                            onclick={() => (showStats = !showStats)}
                            use:tooltip={displayedLatencyTitle}
                            aria-label="{displayedLatencySource}: {Math.round(displayedLatency)}ms"
                            aria-expanded={showStats}
                            aria-haspopup="dialog"
                        >
                            ~{Math.round(displayedLatency)}ms
                        </button>
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
        {#if activeSpeakers.length > 0 && !showParticipantList}
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

        <ScopesPanel {videoElement} open={showScopes} onClose={() => (showScopes = false)} />

        {#if grabToast}
            <div class="mini-toast" transition:fade={{ duration: 150 }}>{grabToast}</div>
        {/if}

        {#if screenShareParticipantId && screenShareParticipantId !== sessionData?.participantId && !screenShareStream}
            <div class="mini-toast share-loading" transition:fade={{ duration: 150 }}>
                <span class="connect-dots" aria-hidden="true"><span></span><span></span><span></span></span>
                {screenShareParticipantName || "Someone"} is starting a screen share
            </div>
        {/if}

        {#if showShortcuts}
            <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
            <div
                class="shortcuts-overlay"
                role="dialog"
                aria-modal="true"
                aria-label="Keyboard shortcuts"
                tabindex="-1"
                transition:fade={{ duration: 150 }}
                onclick={(e) => {
                    if (e.target === e.currentTarget) showShortcuts = false;
                }}
            >
                <div class="shortcuts-card" transition:scale={{ start: 0.95, duration: 240, easing: quintOut }}>
                    <h3>Keyboard shortcuts</h3>
                    {#each [["M", "Toggle microphone"], ["C", "Toggle chat"], ["G", "Grab frame to chat"], ["L", "Laser pointer"], ["Z", "Pixel loupe"], ["S", "Scopes"], ["F", "Fullscreen"], ["Esc", "Close panels"], ["?", "These shortcuts"]] as [key, label] (key)}
                        <div class="shortcut-row">
                            <span>{label}</span>
                            <kbd>{key}</kbd>
                        </div>
                    {/each}
                </div>
            </div>
        {/if}

        <!-- Participant list (outside controls overlay so it doesn't auto-hide) -->
        {#if showParticipantList}
            <div
                class="participant-list"
                use:liquidLens={POPOVER_LENS}
                role="dialog"
                aria-label="Participants"
                tabindex="-1"
                bind:this={participantListEl}
                transition:scale={{ start: 0.95, duration: 260, easing: quintOut }}
            >
                <div class="participant-list-header">
                    <span>Participants</span>
                    {#if isAdmin && participants.length > 1}
                        <button class="participant-action" onclick={muteAllParticipants}>Mute all</button>
                    {/if}
                </div>
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
                                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="13" height="13"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><line x1="12" x2="12" y1="19" y2="22"/></svg>
                            </span>
                        {:else if p.audioEnabled}
                            <span class="mic-on-indicator" title="Mic on">
                                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="13" height="13"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><line x1="12" x2="12" y1="19" y2="22"/></svg>
                            </span>
                        {:else}
                            <span class="mic-muted-indicator" title="Mic muted">
                                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="13" height="13"><line x1="2" x2="22" y1="2" y2="22"/><path d="M18.89 13.23A7.12 7.12 0 0 0 19 12v-2"/><path d="M5 10v2a7 7 0 0 0 12 5"/><path d="M15 9.34V5a3 3 0 0 0-5.68-1.33"/><path d="M9 9v3a3 3 0 0 0 5.12 2.12"/><line x1="12" x2="12" y1="19" y2="22"/></svg>
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
                                    onclick={() => toggleParticipantMute(p.id)}
                                    title={adminMutedIds.has(p.id) ? `Unmute ${p.name}` : `Mute ${p.name}`}
                                >{adminMutedIds.has(p.id) ? "Unmute" : "Mute"}</button>
                                <button
                                    class="participant-action"
                                    onclick={() => toggleParticipantCam(p.id)}
                                    title={camDisabledIds.has(p.id) ? `Allow ${p.name}'s camera` : `Turn off ${p.name}'s camera`}
                                >{camDisabledIds.has(p.id) ? "Allow cam" : "Disable cam"}</button>
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
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="26" height="26">
                            <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" x2="9" y1="12" y2="12"/>
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
        typing={typingList}
        roomSlug={slug}
        {participantColors}
        selfId={sessionData?.participantId || ""}
        canModerate={isAdmin}
    />

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
        border: 1px solid var(--glass-edge);
        border-radius: var(--radius-lg);
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
        border: 1px solid var(--glass-edge);
        color: var(--color-text);
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
        /* Include left/right safe-area so the room name and the latency/signal
           pills clear the notch / Dynamic Island and the rounded corners when a
           phone is held in landscape. */
        padding:
            calc(var(--space-md) + env(safe-area-inset-top, 0px))
            calc(var(--space-lg) + env(safe-area-inset-right, 0px))
            calc(var(--space-md) + env(safe-area-inset-bottom, 0px))
            calc(var(--space-lg) + env(safe-area-inset-left, 0px));
        pointer-events: none;
        opacity: 0;
        visibility: hidden;
        /* Idle fade-out is gentle (~350ms)… */
        transition: opacity 350ms ease, visibility 350ms ease;
    }
    .controls-overlay.visible {
        opacity: 1;
        visibility: visible;
        /* …but reappearing on movement is quick (~200ms, eased). */
        transition: opacity 200ms var(--ease-glide), visibility 200ms var(--ease-glide);
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
        transition: transform 420ms var(--ease-spring);
    }
    .controls-overlay:not(.visible) .top-bar { transform: translateY(-8px) scale(0.99); }
    .controls-overlay:not(.visible) .bottom-bar { transform: translateY(12px) scale(0.98); }

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
        border: 1px solid var(--glass-edge);
        padding: 8px 16px;
        border-radius: var(--radius-md);
    }

    .latency-display {
        font-size: 0.75rem; font-family: monospace;
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
        padding: 8px 16px;
        border-radius: var(--radius-md);
        border: 1px solid var(--glass-edge);
        cursor: pointer;
        transition: border-color 0.15s ease, color 0.15s ease, background 0.15s ease;
    }
    .participant-count:hover { border-color: rgba(255,255,255,0.2); }
    .participant-count.active { border-color: var(--color-primary); color: var(--color-primary); }

    /* Participant list dropdown */
    .participant-list {
        position: absolute;
        top: calc(58px + env(safe-area-inset-top, 0px));
        right: var(--space-sm);
        border: 1px solid var(--glass-edge);
        border-radius: var(--radius-lg);
        transform-origin: top right;
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
    /* Segmented capsule, macOS-menu style: quiet text buttons inside one
       pill that floats over the row's right edge on hover/focus. */
    .participant-actions {
        position: absolute;
        right: var(--space-xs);
        top: 50%;
        transform: translateY(-50%);
        display: flex;
        align-items: center;
        gap: 2px;
        padding: 3px;
        background: rgba(28, 28, 33, 0.97);
        border: 1px solid rgba(255, 255, 255, 0.12);
        border-radius: var(--radius-full);
        box-shadow: 0 4px 12px rgba(0, 0, 0, 0.4);
        opacity: 0;
        pointer-events: none;
        transition: opacity 0.12s ease;
    }
    .participant-list-item:hover .participant-actions,
    .participant-list-item:focus-within .participant-actions {
        opacity: 1;
        pointer-events: auto;
    }
    /* Touch has no hover, so the per-row moderation actions would be unreachable.
       Show them in-flow (static) so a host on a phone can still moderate. */
    @media (hover: none) {
        .participant-actions {
            position: static;
            transform: none;
            opacity: 1;
            pointer-events: auto;
            margin-left: auto;
            background: transparent;
            border: none;
            box-shadow: none;
        }
    }
    .participant-action {
        background: transparent;
        border: none;
        border-radius: var(--radius-full);
        color: var(--color-text-muted);
        font-size: var(--text-min);
        font-weight: 500;
        padding: 4px 10px;
        white-space: nowrap;
        cursor: pointer;
        transition: background 0.12s ease, color 0.12s ease;
    }
    .participant-action:hover {
        background: rgba(255, 255, 255, 0.14);
        color: #fff;
    }
    .participant-action.danger {
        color: var(--color-error);
    }
    .participant-action.danger:hover {
        background: rgba(239, 68, 68, 0.18);
        color: #ff8a8a;
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
        border: 1px solid var(--glass-edge);
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
        /* Speaking ring is always green (a universal "talking" cue) rather than
           the participant's color. */
        box-shadow:
            0 0 0 2px var(--color-success),
            0 0 8px var(--color-success);
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

    .cam-video {
        width: 100%;
        height: 100%;
        object-fit: cover;
        border-radius: 50%;
        display: block;
    }
    .cam-video.mirror {
        transform: scaleX(-1);
    }
    .cam-hide-btn {
        display: inline-flex;
        align-items: center;
        justify-content: center;
        width: 1.75rem;
        height: 1.75rem;
        margin-left: 2px;
        border: none;
        border-radius: 50%;
        background: rgba(255, 255, 255, 0.08);
        color: var(--color-text-muted);
        cursor: pointer;
        transition: background 0.12s ease, color 0.12s ease;
    }
    .cam-hide-btn:hover {
        background: rgba(255, 255, 255, 0.16);
        color: #fff;
    }

    /* Persistent floating cam strip (top-right). Padded by default; slides flush
       into the corner when the UI hides (and chat is closed). pointer-events are
       off so it never blocks the laser/loupe on the video; only the hide button
       is interactive. */
    .cam-float {
        position: fixed;
        top: 60px;
        right: 16px;
        z-index: 8;
        display: flex;
        flex-wrap: wrap;
        justify-content: flex-end;
        /* Never run off the left edge with a crowded room — wrap into rows and
           cap to the viewport (minus its right offset + a margin). */
        max-width: calc(100vw - 32px);
        align-items: flex-start;
        gap: 10px;
        padding: 7px 9px;
        border-radius: var(--radius-md);
        /* Subtle glass: a whisper-light translucent strip so the cams read as
           floating over the picture, not a heavy box. Tints only its own
           footprint — the image elsewhere is untouched. */
        background: rgba(18, 18, 22, 0.34);
        backdrop-filter: var(--glass-backdrop);
        -webkit-backdrop-filter: var(--glass-backdrop);
        border: 1px solid rgba(255, 255, 255, 0.06);
        pointer-events: none;
        /* Match the chat drawer's open/close motion (duration + glide easing) so
           the strip's leftward shift stays geometry-locked to the panel and the
           cams never sweep across chat content mid-animation. */
        transition: top 0.32s var(--ease-glide), right 0.32s var(--ease-glide),
            background 0.3s ease;
    }
    .cam-float.flush {
        top: 0;
        right: 0;
    }
    /* Chat is an in-flow panel on the right that shrinks the video; the cam strip
       is viewport-fixed, so shift it left of the chat (by the panel width) to
       keep it over the picture, not on top of chat. The chat panel also stacks
       above the strip (z-index), so any transient animation overlap hides the
       strip behind chat rather than painting cams over it. */
    .cam-float.chat-open {
        right: calc(var(--chat-panel-width, 320px) + 16px);
    }
    .cam-float.flush.chat-open {
        top: 0;
        right: var(--chat-panel-width, 320px);
    }
    .cam-float-tile {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: 4px;
    }
    .cam-float-circle {
        width: 3.9rem;
        height: 3.9rem;
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 1.05rem;
        font-weight: 600;
        color: #fff;
        /* Glass-framed avatar: the participant color sits under a soft glass
           rim + inner highlight rather than a hard black ring, so it reads as
           part of the chrome. The video (when on) covers the fill. */
        background-color: color-mix(in srgb, var(--participant-color, #555) 80%, transparent);
        border: 1px solid var(--glass-edge);
        box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.12);
        overflow: hidden;
        transition: box-shadow 0.2s ease, opacity 0.2s ease;
    }
    .cam-float-circle.has-cam {
        /* Dim glass base behind the video so a momentary no-frame gap reads as
           soft glass, not a hard black disc. */
        background-color: rgba(8, 8, 11, 0.85);
    }
    .cam-float-circle.speaking {
        box-shadow:
            0 0 0 2px var(--color-success),
            0 0 12px var(--color-success);
    }
    .cam-float-circle.muted {
        opacity: 0.7;
    }
    .cam-float-name {
        max-width: 4.25rem;
        font-size: 0.6875rem;
        font-weight: 500;
        color: #fff;
        text-shadow: 0 1px 2px rgba(0, 0, 0, 0.85);
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
    }
    .cam-float-overflow {
        align-self: center;
        font-size: 0.75rem;
        font-weight: 600;
        color: var(--color-text-muted);
    }
    .cam-float-hide {
        pointer-events: auto;
        align-self: center;
    }

    /* Mic→camera prompt: a one-time glass card centered on screen (not pinned to
       the bar) so it reads as a deliberate first-run invite and survives the
       control-bar auto-hide. Sits below the .cam-modal (z 60) it opens. */

    /* Rotate hint: a small glass pill at the top-center, tap to dismiss. */
    .rotate-hint {
        position: fixed;
        top: calc(12px + env(safe-area-inset-top, 0px));
        left: 50%;
        transform: translateX(-50%);
        z-index: 30;
        display: inline-flex;
        align-items: center;
        gap: 8px;
        padding: 8px 14px;
        max-width: calc(100vw - 24px);
        border-radius: var(--radius-full);
        background: var(--glass-bg-deep);
        border: 1px solid var(--glass-edge);
        box-shadow: var(--glass-specular), 0 8px 28px rgba(0, 0, 0, 0.4);
        color: var(--color-text);
        font-size: 0.8125rem;
        font-weight: 500;
        white-space: nowrap;
        cursor: pointer;
    }
    .rotate-hint svg {
        flex-shrink: 0;
        color: var(--color-text-muted);
    }

    .cam-nudge {
        position: fixed;
        top: 50%;
        left: 50%;
        transform: translate(-50%, -50%);
        z-index: 40;
        display: flex;
        flex-direction: column;
        align-items: center;
        text-align: center;
        gap: 8px;
        padding: 22px 24px 20px;
        max-width: 300px;
        border-radius: var(--radius-lg, 16px);
        background:
            linear-gradient(to bottom, rgba(255, 255, 255, 0.04), rgba(255, 255, 255, 0) 56px),
            var(--glass-bg-deep);
        border: 1px solid var(--glass-edge);
        box-shadow: var(--glass-specular), 0 16px 48px rgba(0, 0, 0, 0.5);
        color: var(--color-text);
        /* Centered over the picture but NON-blocking: only the buttons take
           clicks, so the loupe/laser/grab on the picture center still work
           through the card. */
        pointer-events: none;
    }
    .cam-nudge-enable,
    .cam-nudge-dismiss {
        pointer-events: auto;
    }
    .cam-nudge-icon {
        display: inline-flex;
        align-items: center;
        justify-content: center;
        width: 46px;
        height: 46px;
        margin-bottom: 2px;
        border-radius: 50%;
        background: rgba(255, 255, 255, 0.08);
        border: 1px solid var(--glass-edge);
        color: #fff;
    }
    .cam-nudge-text {
        font-size: 1rem;
        font-weight: 600;
    }
    .cam-nudge-sub {
        font-size: 0.8125rem;
        color: var(--color-text-muted);
        line-height: 1.35;
    }
    .cam-nudge-enable {
        margin-top: 6px;
        background: var(--color-primary);
        border: none;
        border-radius: var(--radius-full);
        color: #041014;
        font-size: 0.875rem;
        font-weight: 600;
        padding: 9px 20px;
        cursor: pointer;
        transition: filter 0.15s ease;
    }
    .cam-nudge-enable:hover {
        filter: brightness(1.08);
    }
    .cam-nudge-dismiss {
        position: absolute;
        top: 8px;
        right: 8px;
        display: inline-flex;
        align-items: center;
        justify-content: center;
        width: 1.75rem;
        height: 1.75rem;
        border: none;
        border-radius: 50%;
        background: transparent;
        color: var(--color-text-muted);
        cursor: pointer;
        transition: background 0.12s ease, color 0.12s ease;
    }
    .cam-nudge-dismiss:hover {
        background: rgba(255, 255, 255, 0.1);
        color: #fff;
    }
    /* Centered camera setup modal */
    .cam-modal-backdrop {
        position: fixed;
        inset: 0;
        z-index: 60;
        display: flex;
        align-items: center;
        justify-content: center;
        padding: var(--space-lg);
        background: rgba(0, 0, 0, 0.55);
    }
    .cam-modal {
        width: 100%;
        max-width: 380px;
        /* Never exceed the screen (landscape phone is only ~390px tall) — scroll
           the modal body and keep the Dismiss/Enable buttons reachable. */
        max-height: calc(100dvh - 2 * var(--space-lg));
        overflow-y: auto;
        -webkit-overflow-scrolling: touch;
        display: flex;
        flex-direction: column;
        gap: var(--space-md);
        padding: var(--space-lg);
        border-radius: var(--radius-lg, 16px);
        /* Match the app's panels (popovers/cards) — glass, not solid. */
        background:
            linear-gradient(to bottom, rgba(255, 255, 255, 0.04), rgba(255, 255, 255, 0) 48px),
            var(--glass-bg-deep);
        backdrop-filter: var(--glass-backdrop-deep);
        -webkit-backdrop-filter: var(--glass-backdrop-deep);
        border: 1px solid var(--glass-edge);
        box-shadow: var(--glass-specular), var(--glass-shadow, 0 16px 48px rgba(0, 0, 0, 0.5));
    }
    .cam-modal-title {
        margin: 0;
        font-size: 1rem;
        font-weight: 600;
        color: var(--color-text);
    }
    .cam-modal-preview {
        width: 100%;
        aspect-ratio: 4 / 3;
        border-radius: var(--radius-sm);
        overflow: hidden;
        background: #000;
        border: 1px solid rgba(255, 255, 255, 0.08);
        display: flex;
        align-items: center;
        justify-content: center;
        text-align: center;
        padding: var(--space-md);
    }
    .cam-modal-video {
        width: 100%;
        height: 100%;
        object-fit: cover;
        transform: scaleX(-1); /* self-view mirror */
    }
    .cam-modal-empty {
        font-size: 0.8125rem;
        color: var(--color-text-subtle);
    }
    /* Shared device dropdown (mic / speaker / camera): collapses to the selected
       device with a caret; the native list opens on click and scrolls for many
       devices. appearance:none + an inline caret keeps it on-theme. */
    .device-select {
        width: 100%;
        appearance: none;
        -webkit-appearance: none;
        background-color: rgba(255, 255, 255, 0.05);
        background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='%23ffffff' stroke-width='2' stroke-linecap='round' stroke-linejoin='round'%3E%3Cpath d='M6 9l6 6 6-6'/%3E%3C/svg%3E");
        background-repeat: no-repeat;
        background-position: right 10px center;
        border: 1px solid rgba(255, 255, 255, 0.12);
        border-radius: var(--radius-sm);
        color: var(--color-text);
        font-size: 0.8125rem;
        padding: 8px 32px 8px 10px;
        cursor: pointer;
        transition: background-color 0.12s ease, border-color 0.12s ease;
    }
    /* Touch only: bump to 16px (+roomier hit area) so iOS Safari doesn't zoom
       the page on focus. Desktop keeps the compact size. */
    @media (pointer: coarse) {
        .device-select {
            font-size: 16px;
            padding: 10px 32px 10px 10px;
        }
    }
    .device-select:hover {
        background-color: rgba(255, 255, 255, 0.09);
        border-color: rgba(255, 255, 255, 0.2);
    }
    .device-select:focus-visible {
        outline: none;
        border-color: rgba(255, 255, 255, 0.45);
        box-shadow: 0 0 0 2px rgba(255, 255, 255, 0.35);
    }
    .device-select:disabled {
        opacity: 0.6;
        cursor: default;
    }
    /* Native option list is OS-rendered; force legible dark text on the
       light default menu background (Chromium/Firefox respect these). */
    .device-select option {
        color: #000;
    }
    .cam-modal-actions {
        display: flex;
        justify-content: flex-end;
        gap: var(--space-sm);
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
        /* minmax(0, …) side cells let the LIVE/signal cluster shrink instead of
           forcing the bar wider than the video column — the old `1fr auto 1fr`
           overflowed right and painted the control bar / status over the chat
           entry field (z 20 vs chat z 9) when chat was open on a small window.
           The control bar itself is kept compact (not wrapped) via the
           chat-open media queries below. */
        grid-template-columns: minmax(0, 1fr) auto minmax(0, 1fr);
        align-items: end;
        gap: var(--space-sm);
        min-width: 0;
    }

    .bottom-right {
        display: flex;
        align-items: center;
        justify-content: flex-end;
        gap: var(--space-sm);
        justify-self: end;
        /* Shrink/wrap the LIVE + signal + latency cluster rather than pushing it
           out of the video column and over the chat. */
        min-width: 0;
        flex-wrap: wrap;
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
        padding: 4px 10px;
        border-radius: var(--radius-full);
        border: 1px solid var(--glass-edge);
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
        border: 1px solid var(--glass-edge);
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
        /* Breathing room from the Voice-chat slider directly above it. */
        margin-top: 6px;
        background: rgba(255, 255, 255, 0.08);
        border: 1px solid rgba(255, 255, 255, 0.15);
        border-radius: var(--radius-sm);
        color: var(--color-text);
        font-size: var(--text-meta);
        padding: 8px 10px;
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
        border: 1px solid var(--glass-edge);
        border-radius: var(--radius-lg);
        transform-origin: 50% 100%;
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
    .audio-mode-toggle {
        display: flex;
        gap: var(--space-xs);
    }
    .audio-mode-option {
        flex: 1;
        display: flex;
        flex-direction: column;
        gap: 2px;
        text-align: left;
        background: rgba(255, 255, 255, 0.04);
        border: 1px solid transparent;
        border-radius: var(--radius-sm);
        color: var(--color-text);
        padding: 8px 10px;
        cursor: pointer;
        transition: background 0.12s ease, border-color 0.12s ease;
    }
    .audio-mode-option:hover { background: rgba(255, 255, 255, 0.1); }
    .audio-mode-option.selected {
        border-color: var(--color-primary);
        background: rgba(255, 255, 255, 0.12);
        color: #fff;
    }
    .audio-mode-option:disabled {
        opacity: 0.5;
        cursor: wait;
    }
    .audio-mode-name {
        font-size: 0.8125rem;
        font-weight: 600;
    }
    .audio-mode-desc {
        font-size: 0.6875rem;
        color: var(--color-text-subtle);
        line-height: 1.25;
    }
    .seg {
        display: inline-flex;
        border: 1px solid rgba(255, 255, 255, 0.12);
        border-radius: var(--radius-sm);
        overflow: hidden;
    }
    .seg-btn {
        background: transparent;
        border: none;
        color: var(--color-text-muted);
        font-size: 0.75rem;
        padding: 3px 10px;
        cursor: pointer;
        transition: background 0.12s ease, color 0.12s ease;
    }
    .seg-btn.selected {
        background: var(--color-primary);
        color: #041014;
        font-weight: 600;
    }
    .seg-btn:disabled { cursor: wait; opacity: 0.6; }
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
        border: 1px solid var(--glass-edge);
        padding: 8px;
        border-radius: 20px;
        /* WebKit can jank the first blur paint without a promoted layer */
        transform: translateZ(0);
    }

    .control-btn {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: 4px;
        padding: 10px 16px;
        background: rgba(255, 255, 255, 0.06);
        border: 1px solid rgba(255, 255, 255, 0.08);
        border-radius: 12px;
        color: rgba(255, 255, 255, 0.92);
        cursor: pointer;
        transition: background 0.15s ease, border-color 0.15s ease, color 0.15s ease,
            transform 0.2s var(--ease-spring),
            opacity 280ms var(--ease-glide) var(--stagger, 0ms);
        position: relative;
        min-width: 64px;
    }

    /* Top-lit sheen on hover; press squishes. Plain rgba fills only in
       here — a backdrop-filter on buttons inside the glass bar would
       nest backdrop roots and double the per-frame filter cost. */
    .control-btn:hover {
        background: radial-gradient(
            120% 100% at 50% 0%,
            rgba(255, 255, 255, 0.16),
            rgba(255, 255, 255, 0.06) 70%
        );
        border-color: rgba(255, 255, 255, 0.16);
    }

    .control-btn:active {
        transform: scale(0.95);
        transition-duration: 60ms;
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

    /* Program/stream audio: the one control that governs what EVERYONE hears,
       so it must never read as just-another-button. When live it sits brighter
       than the neutral chrome with a small "audio is playing" dot; when muted
       the .off rule above paints it solid red ("Stream Muted"). */
    .control-btn.program-audio-btn {
        position: relative;
    }
    .control-btn.program-audio-btn:not(.off) {
        background: rgba(255, 255, 255, 0.16);
        border-color: rgba(255, 255, 255, 0.34);
        color: #fff;
    }
    .control-btn.program-audio-btn:not(.off):hover {
        background: radial-gradient(
            120% 100% at 50% 0%,
            rgba(255, 255, 255, 0.26),
            rgba(255, 255, 255, 0.12) 70%
        );
        border-color: rgba(255, 255, 255, 0.42);
    }
    /* "Audio is playing" dot. Neutral white, NOT a saturated color — a green/teal
       accent next to the picture would contaminate color judgment (the one
       sanctioned colored accent is the laser button). */
    .control-btn.program-audio-btn:not(.off)::after {
        content: "";
        position: absolute;
        top: 6px;
        right: 6px;
        width: 6px;
        height: 6px;
        border-radius: 50%;
        background: rgba(255, 255, 255, 0.92);
        box-shadow: 0 0 5px rgba(255, 255, 255, 0.5);
    }

    /* Laser is the one deliberate exception to the neutral active state:
       it matches the green "live/speaking" semantic already on screen. */
    .control-btn.laser-btn.active {
        background: rgba(47, 191, 113, 0.16);
        border-color: rgba(47, 191, 113, 0.5);
        color: var(--color-success);
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

    /* Gel pill: inset top light + soft drop, pops on every new message */
    .chat-badge {
        position: absolute;
        top: -4px;
        right: -4px;
        background: linear-gradient(to bottom, #58c6b6, #3b9c8d);
        color: #04201c;
        font-size: 0.5625rem;
        font-weight: 700;
        font-variant-numeric: tabular-nums;
        min-width: 15px;
        height: 15px;
        border-radius: var(--radius-full);
        display: flex;
        align-items: center;
        justify-content: center;
        padding: 0 4px;
        line-height: 1;
        box-shadow:
            inset 0 1px 0 rgba(255, 255, 255, 0.32),
            0 1px 4px rgba(0, 0, 0, 0.45);
        animation: badge-pop 360ms var(--ease-spring);
    }
    @keyframes badge-pop {
        from { transform: scale(0.4); }
        to { transform: scale(1); }
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
        border: 1px solid rgba(72, 182, 166, 0.35);
        border-radius: var(--radius-full);
        color: var(--color-text);
        cursor: pointer;
        transition: border-color 0.15s ease, background 0.15s ease;
    }
    .chat-toast:hover {
        border-color: rgba(72, 182, 166, 0.7);
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
    /* Stacked top-right, directly beneath the participant badge */
    .active-speaker-indicator {
        position: absolute;
        top: calc(60px + env(safe-area-inset-top, 0px));
        right: var(--space-lg);
        z-index: 12;
        display: flex;
        flex-direction: column;
        align-items: flex-end;
        gap: var(--space-xs);
        pointer-events: none;
    }
    .active-speaker-chip {
        display: flex;
        align-items: center;
        gap: var(--space-xs);
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
        from { opacity: 0; transform: translateY(-6px); }
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
        border: 1px solid var(--glass-edge);
        color: #fff;
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
        border: 1px solid rgba(72, 182, 166, 0.4);
        color: #fff;
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
        border: 1px solid var(--glass-edge);
        color: #fff;
        flex-shrink: 0;
    }
    /* Chime-free attention: one brief teal pulse on arrival, then settle. */
    .waiting-request-card.pulse {
        animation: waiting-card-pulse 1.2s ease-out 1;
    }
    @keyframes waiting-card-pulse {
        0% { border-color: rgba(72, 182, 166, 0.85); box-shadow: 0 0 0 0 rgba(72, 182, 166, 0.35), var(--glass-specular), var(--glass-shadow); }
        60% { border-color: rgba(72, 182, 166, 0.45); box-shadow: 0 0 0 8px rgba(72, 182, 166, 0), var(--glass-specular), var(--glass-shadow); }
        100% { border-color: var(--glass-edge); box-shadow: var(--glass-specular), var(--glass-shadow); }
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

    /* ==================== LIQUID GLASS MATERIAL ====================
       One shared material for every floating surface (tokens live in
       app.css). Ambient: pills and bars sitting over the picture.
       Deep: panels that need text legibility (menus, banners, cards).
       On Chromium the liquidLens action upgrades key surfaces to true
       edge refraction; WebKit/Gecko keep this stylesheet material. */
    .control-bar,
    .room-name,
    .participant-count,
    .presence-row,
    .live-pill,
    .signal-indicator,
    .latency-display,
    .chat-toast,
    .active-speaker-chip {
        background:
            linear-gradient(to bottom, rgba(255, 255, 255, 0.05), rgba(255, 255, 255, 0) 55%),
            var(--glass-bg);
        backdrop-filter: var(--glass-backdrop);
        -webkit-backdrop-filter: var(--glass-backdrop);
        /* Specular only — no drop shadows on ambient pills: a row of soft
           shadows over the picture blends into a faint dark veil. */
        box-shadow: var(--glass-specular);
    }

    .stream-card,
    .mic-prompt,
    .screenshare-approval,
    .open-early-banner,
    .waiting-request-card,
    .stats-popover,
    .shortcuts-card,
    .audio-settings-popover,
    .participant-list {
        background:
            linear-gradient(to bottom, rgba(255, 255, 255, 0.04), rgba(255, 255, 255, 0) 48px),
            var(--glass-bg-deep);
        backdrop-filter: var(--glass-backdrop-deep);
        -webkit-backdrop-filter: var(--glass-backdrop-deep);
        box-shadow: var(--glass-specular), var(--glass-shadow);
    }

    /* Apple-like geometry: capsules for bars, pills and small action
       buttons; large continuous radii for panels and banners. */
    .control-btn,
    .participant-count,
    .latency-display,
    .signal-indicator {
        border-radius: var(--radius-full);
    }
    .stats-popover,
    .shortcuts-card,
    .audio-settings-popover,
    .participant-list,
    .stream-card {
        border-radius: 20px;
    }
    .mic-prompt,
    .screenshare-approval,
    .open-early-banner,
    .waiting-request-card {
        border-radius: 16px;
    }
    .mic-prompt-btn,
    .screenshare-approval-btn,
    .open-early-btn,
    .waiting-request-btn,
    .volume-mute-btn,
    .participant-action,
    .audio-settings-grant,
    .split-screenshare-label,
    .split-screenshare-stop {
        border-radius: var(--radius-full);
    }
    /* Bars host a shared glass canvas as their first child; real content
       must be positioned so it paints above the canvas. */
    .top-bar,
    .bottom-bar {
        position: relative;
    }
    .room-name,
    .top-bar-right,
    .bottom-left,
    .bottom-right {
        position: relative;
    }

    /* Entrance choreography: bar buttons cascade in left to right */
    .controls-overlay:not(.visible) .control-btn {
        opacity: 0;
    }

    /* Cinema mode: hide the cursor with the controls. The :global(*)
       leg covers children (video, canvases, overlays) that resolve their
       own cursor instead of inheriting the wrapper's. */
    .session-page.controls-hidden .video-wrapper,
    .session-page.controls-hidden .video-wrapper :global(*) {
        cursor: none;
    }

    /* Neutral glass focus ring instead of the default teal outline */
    .control-btn:focus-visible,
    .participant-count:focus-visible {
        outline: none;
        box-shadow: 0 0 0 2px rgba(255, 255, 255, 0.45);
    }

    /* Volume sliders: quiet glass track, white thumb */
    .audio-settings-popover .range-input,
    .more-sheet .range-input {
        height: 4px;
        background: rgba(255, 255, 255, 0.16);
    }
    .audio-settings-popover .range-input::-webkit-slider-thumb,
    .more-sheet .range-input::-webkit-slider-thumb {
        width: 14px;
        height: 14px;
        background: #fff;
        box-shadow: 0 1px 4px rgba(0, 0, 0, 0.45);
    }
    .audio-settings-popover .range-input::-moz-range-thumb,
    .more-sheet .range-input::-moz-range-thumb {
        width: 14px;
        height: 14px;
        background: #fff;
        box-shadow: 0 1px 4px rgba(0, 0, 0, 0.45);
    }

    /* Bar grouping: comms | review tools | session */
    .bar-divider {
        width: 1px;
        align-self: stretch;
        margin: 8px 3px;
        background: rgba(255, 255, 255, 0.09);
        flex-shrink: 0;
    }

    /* Phone "More" sheet (trigger hidden on desktop; shown via the phone media
       query below). The sheet holds the secondary tools + the full settings
       panel so the control bar can stay a short primary row on a phone. */
    .more-btn { display: none; }
    .more-sheet-backdrop {
        position: fixed;
        inset: 0;
        z-index: 55;
        display: flex;
        align-items: flex-end;
        justify-content: center;
        background: rgba(0, 0, 0, 0.45);
        /* It lives inside .controls-overlay (pointer-events:none); take taps. */
        pointer-events: auto;
    }
    .more-sheet {
        width: 100%;
        max-width: 540px;
        max-height: 85dvh;
        overflow-y: auto;
        -webkit-overflow-scrolling: touch;
        display: flex;
        flex-direction: column;
        gap: var(--space-md);
        padding: 10px var(--space-lg) calc(var(--space-lg) + env(safe-area-inset-bottom, 0px));
        border-radius: 20px 20px 0 0;
        background:
            linear-gradient(to bottom, rgba(255, 255, 255, 0.05), rgba(255, 255, 255, 0) 72px),
            var(--glass-bg-deep);
        border: 1px solid var(--glass-edge);
        border-bottom: none;
        box-shadow: var(--glass-specular), 0 -12px 48px rgba(0, 0, 0, 0.5);
        color: var(--color-text);
    }
    .more-sheet-handle {
        align-self: center;
        width: 40px;
        height: 4px;
        border-radius: 2px;
        background: rgba(255, 255, 255, 0.25);
        flex-shrink: 0;
    }
    .more-tools {
        display: grid;
        grid-template-columns: repeat(4, 1fr);
        gap: var(--space-sm);
    }
    .more-tool {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: 6px;
        padding: 12px 6px;
        min-height: 64px;
        background: rgba(255, 255, 255, 0.05);
        border: 1px solid rgba(255, 255, 255, 0.08);
        border-radius: 14px;
        color: var(--color-text);
        font-size: 0.75rem;
        font-weight: 500;
        cursor: pointer;
        transition: background 0.12s ease, border-color 0.12s ease;
    }
    .more-tool.active {
        border-color: rgba(255, 255, 255, 0.4);
        background: rgba(255, 255, 255, 0.14);
        color: #fff;
    }
    .more-tool:disabled { opacity: 0.45; cursor: default; }
    .more-sheet-settings {
        display: flex;
        flex-direction: column;
        gap: var(--space-md);
    }

    /* Preference switches in the settings popover */
    .pref-row {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: var(--space-sm);
        font-size: 0.8125rem;
        color: var(--color-text);
        padding: 2px 0;
        cursor: pointer;
    }
    .switch {
        appearance: none;
        width: 34px;
        height: 20px;
        border-radius: var(--radius-full);
        background: rgba(255, 255, 255, 0.14);
        position: relative;
        cursor: pointer;
        transition: background 0.18s ease;
        flex-shrink: 0;
        margin: 0;
    }
    .switch::after {
        content: "";
        position: absolute;
        top: 2px;
        left: 2px;
        width: 16px;
        height: 16px;
        border-radius: 50%;
        background: #fff;
        box-shadow: 0 1px 3px rgba(0, 0, 0, 0.4);
        transition: transform 0.18s var(--ease-spring);
    }
    .switch:checked {
        background: var(--color-primary);
    }
    .switch:checked::after {
        transform: translateX(14px);
    }

    /* Stream statistics popover (anchored above the status pills) */
    .stats-popover {
        position: absolute;
        bottom: calc(100% + 8px);
        right: 0;
        width: 270px;
        display: flex;
        flex-direction: column;
        gap: 7px;
        padding: var(--space-md);
        border: 1px solid var(--glass-edge);
        transform-origin: 100% 100%;
        z-index: 30;
        /* ~14 rows can exceed a landscape phone's height — scroll instead of
           clipping the top rows off-screen. */
        max-height: 70dvh;
        overflow-y: auto;
        -webkit-overflow-scrolling: touch;
    }
    .stats-row {
        display: flex;
        align-items: baseline;
        justify-content: space-between;
        font-size: 0.75rem;
        color: var(--color-text-muted);
    }
    .stats-row span:last-child {
        color: var(--color-text);
        font-variant-numeric: tabular-nums;
    }
    .stats-mode {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 10px;
        font-size: 0.75rem;
        color: var(--color-text-muted);
    }
    .stats-mode select {
        max-width: 132px;
        min-width: 0;
        padding: 4px 8px;
        border-radius: var(--radius-full);
        border: 1px solid rgba(255, 255, 255, 0.12);
        background: rgba(255, 255, 255, 0.08);
        color: var(--color-text);
        font-size: 0.75rem;
    }
    .stats-quality { text-transform: capitalize; }
    .stats-quality.good { color: var(--color-success) !important; }
    .stats-quality.fair { color: var(--color-warning) !important; }
    .stats-quality.poor { color: var(--color-error) !important; }
    .stats-reload {
        margin-top: 4px;
        display: flex;
        align-items: center;
        justify-content: center;
        gap: 6px;
        padding: 6px 10px;
        background: rgba(255, 255, 255, 0.08);
        border: 1px solid rgba(255, 255, 255, 0.12);
        border-radius: var(--radius-full);
        color: var(--color-text);
        font-size: 0.75rem;
        font-weight: 500;
        cursor: pointer;
        transition: background 0.12s ease;
    }
    .stats-reload:hover {
        background: rgba(255, 255, 255, 0.15);
    }
    .signal-indicator,
    .latency-display {
        cursor: pointer;
        font-family: inherit;
    }

    /* Shutter flash on frame grab */
    .grab-flash {
        position: absolute;
        inset: 0;
        z-index: 14;
        background: rgba(255, 255, 255, 0.45);
        pointer-events: none;
    }

    .mini-toast.share-loading {
        display: flex;
        align-items: center;
        gap: 10px;
    }
    .share-loading-pane {
        position: absolute;
        top: 50%;
        left: 50%;
        transform: translate(-50%, -50%);
        z-index: 4;
        display: flex;
        align-items: center;
        gap: 10px;
        padding: 8px 18px;
        font-size: 0.8125rem;
        color: var(--color-text);
        background: var(--glass-bg-deep);
        backdrop-filter: var(--glass-backdrop-deep);
        -webkit-backdrop-filter: var(--glass-backdrop-deep);
        border: 1px solid var(--glass-edge);
        border-radius: var(--radius-full);
        box-shadow: var(--glass-specular);
        pointer-events: none;
        white-space: nowrap;
    }

    /* Neutral confirmation toast (frame grabs etc.) */
    .mini-toast {
        position: absolute;
        bottom: 150px;
        left: 50%;
        transform: translateX(-50%);
        z-index: 22;
        padding: 7px 16px;
        font-size: 0.8125rem;
        color: var(--color-text);
        background: var(--glass-bg-deep);
        backdrop-filter: var(--glass-backdrop-deep);
        -webkit-backdrop-filter: var(--glass-backdrop-deep);
        border: 1px solid var(--glass-edge);
        border-radius: var(--radius-full);
        box-shadow: var(--glass-specular);
        pointer-events: none;
        white-space: nowrap;
    }

    /* Keyboard shortcuts overlay */
    .shortcuts-overlay {
        position: absolute;
        inset: 0;
        z-index: 55;
        display: flex;
        align-items: center;
        justify-content: center;
        background: rgba(0, 0, 0, 0.45);
        padding: var(--space-lg);
    }
    .shortcuts-card {
        width: min(92vw, 340px);
        display: flex;
        flex-direction: column;
        gap: 9px;
        padding: var(--space-lg);
        border: 1px solid var(--glass-edge);
    }
    .shortcuts-card h3 {
        margin: 0 0 4px;
        font-size: 0.8125rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.08em;
        color: var(--color-text-muted);
    }
    .shortcut-row {
        display: flex;
        align-items: center;
        justify-content: space-between;
        font-size: 0.8125rem;
        color: var(--color-text-muted);
    }
    .shortcut-row kbd {
        font-family: var(--font-mono);
        font-size: var(--text-min);
        color: var(--color-text);
        background: rgba(255, 255, 255, 0.08);
        border: 1px solid rgba(255, 255, 255, 0.12);
        border-radius: 6px;
        padding: 2px 8px;
        min-width: 30px;
        text-align: center;
    }

    /* Participant list header (label + admin batch action) */
    .participant-list-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding: var(--space-xs) var(--space-md) var(--space-sm);
        font-size: var(--text-min);
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.06em;
        color: var(--color-text-muted);
        border-bottom: 1px solid rgba(255, 255, 255, 0.06);
        margin-bottom: var(--space-xs);
    }

    /* Approve-all shortcut atop the waiting stack */
    .waiting-approve-all {
        align-self: flex-end;
        padding: 5px 12px;
        background: var(--color-primary);
        border: none;
        border-radius: var(--radius-full);
        color: #041014;
        font-size: var(--text-min);
        font-weight: 600;
        cursor: pointer;
        transition: filter 0.15s ease;
        flex-shrink: 0;
    }
    .waiting-approve-all:hover {
        filter: brightness(1.1);
    }

    /* Loupe: hide the system cursor over the video, lens replaces it */
    .session-page.loupe-on .video-wrapper,
    .session-page.loupe-on .video-wrapper :global(*) {
        cursor: none;
    }

    /* The scopes panel is interactive chrome inside the video wrapper:
       it must keep a visible cursor through cinema mode and tool modes
       (same specificity as the cursor-none rules; later in the file wins,
       and explicit values beat the inherited none). */
    .session-page .video-wrapper :global(.scopes-panel),
    .session-page .video-wrapper :global(.scopes-panel *) {
        cursor: default;
    }
    .session-page .video-wrapper :global(.scopes-panel .scopes-header) {
        cursor: grab;
    }
    .session-page .video-wrapper :global(.scopes-panel button) {
        cursor: pointer;
    }
    .session-page .video-wrapper :global(.scopes-panel .scopes-resize) {
        cursor: nwse-resize;
    }

    /* Phones, BOTH orientations. The width leg catches portrait (and small
       windows); the height+landscape+coarse leg catches landscape iPhones,
       which are ~844px wide and would otherwise fall through to desktop chrome
       in a 390px-tall window. */
    @media (max-width: 768px), (orientation: landscape) and (max-height: 480px) and (pointer: coarse) {
        .session-page { flex-direction: column; }
        .video-wrapper { flex: 1; min-height: 0; }
        /* Wrap as a safety net: on a very narrow phone (<=360px) the 7 primary
           buttons fall to a second centered row rather than clipping Mic/Leave
           off the edges. */
        .control-bar { gap: 4px; padding: var(--space-xs) var(--space-sm); flex-wrap: wrap; justify-content: center; }
        .control-btn { padding: 8px 10px; min-width: 52px; }
        .control-btn svg { width: 20px; height: 20px; }
        .control-label { font-size: 0.5625rem; }
        /* "Stream Audio"/"Stream Muted" is ~2x the other labels; drop it here so
           this one button doesn't widen the bar on tablets. The white live-dot,
           red muted state and tooltip still convey it. */
        .program-audio-btn .control-label { display: none; }
        /* Collapse the control bar to a primary row + a labeled "More" sheet:
           secondary tools, the bar dividers, and the (desktop-only) Fullscreen
           come out; the More trigger comes in. */
        .control-btn.secondary-tool,
        .control-btn.desktop-only,
        .bar-divider { display: none; }
        .more-btn { display: flex; }
        .active-speaker-indicator {
            top: calc(52px + env(safe-area-inset-top, 0px));
            right: var(--space-sm);
        }
        .presence-row { display: none; }
        /* Chat is a bottom sheet here (not a right panel), so the cam strip
           needs no horizontal offset — undo the desktop chat shift. */
        .cam-float.chat-open { right: 16px; }
        .cam-float.flush.chat-open { right: 0; }
        .video-wrapper.split-active { flex-direction: column; }
        .video-wrapper.split-active .video-container { flex: 1; }
        .split-screenshare { flex: 1; }
    }

    /* Smallest phones: the primary bar is only ~7 buttons now, so they keep
       their short labels (Mic/Cam/Laser/Chat/More/Leave) — far more usable for a
       non-technical client than blank icons. Just tighten spacing to fit ~360px. */
    @media (max-width: 480px) {
        .controls-overlay { padding-left: var(--space-sm); padding-right: var(--space-sm); }
        .stream-card { padding: var(--space-lg); }
        .connect-stage { padding: var(--space-lg); }
        .connect-copy > p { white-space: normal; }
        .end-state-card { padding: var(--space-lg); }
        .control-bar { gap: 2px; }
        .control-btn {
            min-width: 46px;
            min-height: 46px;
            padding: 7px 6px;
        }
        .bottom-bar {
            display: flex;
            flex-direction: column;
            align-items: center;
            gap: var(--space-xs);
        }
        .bottom-left { display: none; }
    }

    /* Chat open steals ~320px from the video column. On a constrained (but still
       desktop-layout, >768px) window that can push the control bar toward the
       chat. Rather than wrap to an ugly second row, SHRINK the bar so it stays a
       single row inside the video area and the chat entry field stays clear.
       Two tiers: compact buttons first, then icon-only when very tight. */
    @media (min-width: 769px) and (max-width: 1100px) {
        .session-page.chat-open .control-bar { gap: 5px; padding: 6px; }
        .session-page.chat-open .control-btn { min-width: 50px; padding: 7px 10px; }
        .session-page.chat-open .control-btn svg { width: 19px; height: 19px; }
        .session-page.chat-open .control-label { font-size: 0.5625rem; }
        /* "Stream Audio/Muted" is the widest label; drop it first (state still
           reads via the dot/colour + tooltip). */
        .session-page.chat-open .program-audio-btn .control-label { display: none; }
        .session-page.chat-open .bar-divider { display: none; }
    }
    @media (min-width: 769px) and (max-width: 920px) {
        /* Very tight: icon-only so ~9 buttons still fit one row beside chat. */
        .session-page.chat-open .control-label { display: none; }
        .session-page.chat-open .control-btn { min-width: 44px; padding: 6px 8px; }
        .session-page.chat-open .control-btn svg { width: 18px; height: 18px; }
    }
</style>
