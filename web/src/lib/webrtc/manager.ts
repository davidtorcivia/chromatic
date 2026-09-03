// WebRTC Manager - handles peer connection for receiving stream and sending voice

import { createMicChain, type MicChain } from '$lib/audio/mic-chain';
import {
    type AudioMode,
    type DenoiserEngine,
    loadAudioModeState,
    saveAudioModeState,
    opusPreferencesFor
} from '$lib/audio/audio-mode';
import { isDenoiserImplemented } from '$lib/audio/denoiser';
import { isPermissionError as isCameraPermissionError } from './gum-error';
import { applyOpusPreferences, findProgramAudioMid, tuneSubscriberAnswerOpus } from './sdp';

const DEBUG = import.meta.env.DEV;
const debugLog = (...args: unknown[]) => {
    if (DEBUG) console.log(...args);
};

// localStorage key for the preferred microphone input device. Read on every
// getUserMedia call so the user's choice survives reloads and reconnects.
export const MIC_DEVICE_STORAGE_KEY = 'chromatic_mic_device';

export function getStoredMicDeviceId(): string | null {
    try {
        return localStorage.getItem(MIC_DEVICE_STORAGE_KEY);
    } catch {
        return null;
    }
}

export function storeMicDeviceId(deviceId: string | null): void {
    try {
        if (deviceId) {
            localStorage.setItem(MIC_DEVICE_STORAGE_KEY, deviceId);
        } else {
            localStorage.removeItem(MIC_DEVICE_STORAGE_KEY);
        }
    } catch {
        // Storage unavailable (private mode) — the in-session choice still applies.
    }
}

// Preferred camera input device. Persisted so the user's choice survives
// reloads; the camera itself stays OFF on join (privacy) — only the device
// preference is remembered.
export const CAMERA_DEVICE_STORAGE_KEY = 'chromatic_camera_device';

export function getStoredCameraDeviceId(): string | null {
    try {
        return localStorage.getItem(CAMERA_DEVICE_STORAGE_KEY);
    } catch {
        return null;
    }
}

export function storeCameraDeviceId(deviceId: string | null): void {
    try {
        if (deviceId) {
            localStorage.setItem(CAMERA_DEVICE_STORAGE_KEY, deviceId);
        } else {
            localStorage.removeItem(CAMERA_DEVICE_STORAGE_KEY);
        }
    } catch {
        // Storage unavailable (private mode) — the in-session choice still applies.
    }
}

// Presence-cam capture constraints: deliberately tiny (small circular tiles) so
// encode/bandwidth stay cheap. The display is ~96px; 320x240 is plenty.
function camConstraints(deviceId?: string | null, exact = false): MediaTrackConstraints {
    const constraints: MediaTrackConstraints = {
        width: { ideal: 320 },
        height: { ideal: 240 },
        frameRate: { ideal: 24, max: 30 },
        facingMode: 'user'
    };
    if (deviceId) {
        constraints.deviceId = exact ? { exact: deviceId } : { ideal: deviceId };
    }
    return constraints;
}

const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms));

export interface MicConstraintOptions {
    deviceId?: string | null;
    exact?: boolean;
    mode: AudioMode;
    studioHeadphones: boolean;
    // Explicit native-noise-suppression decision made by the caller. In talkback
    // this is OFF when our own denoiser will run (no double NS) and also OFF when
    // the user explicitly chose "no noise reduction"; it is only ON as a fallback
    // when an intended in-app denoiser failed to engage. Always OFF in studio.
    noiseSuppression: boolean;
}

// Per-mode capture constraints. Talkback keeps native echo cancellation
// (system-preferred — OS AEC beats Chrome's software AEC3, notably on macOS)
// and lets our own denoiser handle noise. Studio sends a pristine signal: no
// noise suppression or AGC, and echo cancellation off only when the user has
// confirmed headphones (otherwise EC stays on as a laptop-speaker safety net).
// Non-standard keys (echoCancellationType) are ignored by browsers that don't
// support them, so this degrades gracefully.
export function micConstraints(opts: MicConstraintOptions): MediaTrackConstraints {
    const studio = opts.mode === 'studio';
    const echoCancellation = studio ? !opts.studioHeadphones : true;

    const constraints: MediaTrackConstraints & Record<string, unknown> = {
        echoCancellation,
        // Studio: never suppress/AGC. Talkback: caller decides NS (see opts).
        // AGC stays native for level consistency (we add no gain stage in
        // talkback, so it can't pump).
        noiseSuppression: studio ? false : opts.noiseSuppression,
        autoGainControl: studio ? false : true
    };
    if (echoCancellation) {
        // Prefer the OS echo canceller where available; falls back to the
        // browser one otherwise.
        constraints.echoCancellationType = { ideal: 'system' };
    }
    if (studio) {
        // Studio carries reference music/instruments, so ask for a stereo
        // capture (non-mandatory `ideal` — a mono-only source still satisfies
        // the constraint). Talkback stays mono and is never given a channel
        // count hint, so it doesn't fight the mono Opus relay.
        constraints.channelCount = { ideal: 2 };
    }
    if (opts.deviceId) {
        // `ideal` lets getUserMedia fall back to the default mic when the
        // remembered device was unplugged; `exact` is used for explicit
        // user selection where silently picking another mic would be wrong.
        constraints.deviceId = opts.exact ? { exact: opts.deviceId } : { ideal: opts.deviceId };
    }
    return constraints;
}

export interface WebRTCManagerOptions {
    iceServers: RTCIceServer[];
    onTrack: (event: RTCTrackEvent) => void;
    onVoiceTrack?: (participantId: string, track: MediaStreamTrack) => void;
    onScreenShareTrack?: (participantId: string, track: MediaStreamTrack) => void;
    onWebcamTrack?: (participantId: string, track: MediaStreamTrack) => void;
    /** Local cam capture ended on its own (device unplugged / OS revoked), so
     *  the page can reset its camera UI state. */
    onWebcamEnded?: () => void;
    sendSignal: (type: string, payload: unknown) => boolean | void;
    onConnectionStateChange?: (state: RTCPeerConnectionState) => void;
    onIceRestart?: () => void;
    onIceRestartFailed?: () => void;
    onRenegotiation?: () => void;
    onScreenShareEnded?: () => void;
    /** Local renegotiation failed repeatedly — the page should rebuild the
     *  subscription (local tracks re-attach on the fresh offer). */
    onNegotiationWedged?: () => void;
    /** The browser refused our stereo-tuned answer, so program audio is
     *  decoding mono until a later renegotiation re-applies it (true), or that
     *  has now happened (false). Stability wins over rebuilding the connection
     *  to fix it, so the page surfaces this instead of reconnecting. */
    onProgramAudioDegraded?: (degraded: boolean) => void;
}

// Compact, log-safe rendering of a thrown value. DOMException.name is the part
// that identifies a munge rejection (InvalidModificationError) versus a state
// error (InvalidStateError), so it must survive into the server log.
function describeError(err: unknown): string {
    const e = err as { name?: string; message?: string } | null;
    if (e && (e.name || e.message)) {
        return `${e.name ?? 'Error'}: ${e.message ?? ''}`.trim();
    }
    return String(err);
}

export interface WebRTCStats {
    rtt?: number;
    videoJitterBufferDelay?: number;
    videoFramesDropped?: number;
    receiverJitterBufferTarget?: number | null;
    receiverPlayoutDelayHint?: number;
}

interface JitterBufferSample {
    delay: number;
    emittedCount: number;
}

export class WebRTCManager {
    private pc: RTCPeerConnection | null = null;
    private subscriberCandidateOfferId: string | null = null;
    // Set when the browser rejected our stereo-tuned answer and we fell back to
    // its own (mono) answer to keep the connection alive. Cleared by the next
    // renegotiation that applies the munge cleanly.
    private programStereoDegraded = false;
    // Rate-limits the program-mid-unknown breadcrumb to once per connection.
    private programMidWarned = false;
    private options: WebRTCManagerOptions;
    // localStream holds the stream whose audio track is SENT — the processed
    // output of the mic cleanup chain when available, the raw capture
    // otherwise. rawMicStream always holds the actual device capture (the
    // thing that must be stopped to release the mic / read deviceId from).
    private localStream: MediaStream | null = null;
    private rawMicStream: MediaStream | null = null;
    private micChain: MicChain | null = null;
    private audioSender: RTCRtpSender | null = null;
    // Talkback vs studio/critical-listening, plus the chosen denoiser engine and
    // the studio "I'm on headphones" flag. Persisted across sessions.
    private audioMode: AudioMode = 'talkback';
    private denoiserEngine: DenoiserEngine = 'rnnoise';
    private studioHeadphones: boolean = false;
    private screenShareStream: MediaStream | null = null;
    private screenShareSender: RTCRtpSender | null = null;
    // Presence webcam (small cam tile). Opt-in: openCameraPreview captures it,
    // enableCamera broadcasts it. One capture serves preview + broadcast.
    private cameraStream: MediaStream | null = null;
    private cameraSender: RTCRtpSender | null = null;
    // Single-flight guard: one in-flight camera getUserMedia at a time, so a
    // preview-open racing an Enable (or a device-switch) can't fire a SECOND
    // concurrent capture — that double-acquire is exactly the Firefox "device in
    // use" failure (and leaks a stream whose track is never stopped).
    private cameraOpenPromise: Promise<MediaStream | null> | null = null;
    // Last camera capture rejection, kept so the UI can say WHY the camera
    // failed (busy device / gone device / actually blocked) instead of always
    // blaming permissions. Cleared on a successful capture.
    private lastCameraError: unknown = null;
    private isMicMuted: boolean = true;
    private iceRestartPending: boolean = false;
    private iceRestartOfferCounter = 0;
    private iceRestartOfferId: string | null = null;
    private iceRestartOfferSent: boolean = false;
    private pendingIceRestartCandidates: RTCIceCandidateInit[] = [];
    // True once an ICE restart offer has actually been sent to the server,
    // so a second 'failed' state can be attributed to a genuine restart
    // failure rather than firing onIceRestartFailed prematurely.
    private iceRestartAttempted: boolean = false;
    private connectionLostTimeout: ReturnType<typeof setTimeout> | null = null;
    // Dedicated send-only peer connection (mic + screen share). The client is
    // always the offerer here; the subscriber PC (this.pc) is receive-only
    // with the server as sole offerer. See ensurePublisher().
    private publisherPc: RTCPeerConnection | null = null;
    private publisherOfferSent: boolean = false;
    private publisherOfferCounter = 0;
    private publisherOfferId: string | null = null;
    private publisherCandidateOfferId: string | null = null;
    private pendingPublisherCandidates: RTCIceCandidateInit[] = [];
    // Inbound counterpart: the SFU's own candidates for the publisher PC, held
    // until its answer is applied (addIceCandidate before setRemoteDescription
    // throws). Distinct from pendingPublisherCandidates above, which is OUR
    // candidates waiting to go out.
    private pendingRemotePublisherCandidates: RTCIceCandidateInit[] = [];
    private publisherNeedsRenegotiation: boolean = false;
    private closed: boolean = false;
    // Watchdog: rebuilds the publisher if an offer goes unanswered
    private voiceOfferTimer: ReturnType<typeof setTimeout> | null = null;
    private publisherDisconnectedTimeout: ReturnType<typeof setTimeout> | null = null;
    private static readonly VOICE_OFFER_TIMEOUT_MS = 8000;
    private static readonly RESYNC_MIN_INTERVAL_MS = 250;
    private static readonly DISCONNECTED_ICE_RESTART_MS = 2000;
    private static readonly ICE_RESTART_ANSWER_TIMEOUT_MS = 8000;
    private static readonly PUBLISHER_DISCONNECTED_REBUILD_MS = 2000;
    // Keep browser playout buffers tight for color-review A/B work. Modern
    // browsers expose jitterBufferTarget in ms; Chromium also has the older
    // seconds-based playoutDelayHint. Unsupported browsers ignore either hint.
    private static readonly LOW_LATENCY_JITTER_BUFFER_TARGET_MS = 20;
    private static readonly LOW_LATENCY_PLAYOUT_DELAY_SECONDS =
        WebRTCManager.LOW_LATENCY_JITTER_BUFFER_TARGET_MS / 1000;
    private lastResyncAt = 0;
    // Serialize all SDP operations to prevent concurrent modifications
    // to the PeerConnection's signaling state (e.g. handleOffer + handleRenegotiation
    // firing from separate WebSocket messages while awaiting).
    private signalingQueue: Promise<void> = Promise.resolve();
    private videoJitterBufferSamples = new Map<string, JitterBufferSample>();

    constructor(options: WebRTCManagerOptions) {
        this.options = options;
        const saved = loadAudioModeState();
        this.audioMode = saved.mode;
        this.denoiserEngine = saved.denoiser;
        this.studioHeadphones = saved.studioHeadphones;
    }

    // True when the talkback path intends to run an in-app denoiser (talkback
    // mode + a chosen, implemented engine). Drives whether native NS is left on.
    private wantsInAppDenoise(): boolean {
        return (
            this.audioMode === 'talkback' &&
            this.denoiserEngine !== 'off' &&
            isDenoiserImplemented(this.denoiserEngine)
        );
    }

    private persistAudioModeState(): void {
        saveAudioModeState({
            mode: this.audioMode,
            denoiser: this.denoiserEngine,
            studioHeadphones: this.studioHeadphones
        });
    }

    private isClosed(): boolean {
        return this.closed;
    }

    private stopStream(stream: MediaStream | null): void {
        stream?.getTracks().forEach(track => track.stop());
    }

    private sendSignal(type: string, payload: unknown): boolean {
        return this.options.sendSignal(type, payload) !== false;
    }

    // Enqueue an async signaling operation so SDP changes are serialized.
    private enqueueSignaling<T>(fn: () => Promise<T>): Promise<T> {
        const task = this.signalingQueue.then(fn, fn);
        // Keep the queue moving regardless of success/failure. Log rejections
        // so they're not swallowed silently — previously a thrown op would
        // disappear and the next op would run on possibly-stale PC state with
        // no trace in the console.
        this.signalingQueue = task.then(
            () => {},
            (err) => {
                console.error('Signaling queue task failed:', err);
            }
        );
        return task;
    }

    // Handle incoming SDP offer from server. `signal:offer` is semantically a
    // FRESH subscription (server just spun up a new SFU subscriber for us),
    // distinct from `signal:renegotiate` which modifies an existing session.
    // So if we already have a peer connection here, it's stale — tear it down
    // before accepting the new one. This is what unblocks viewers hanging on
    // reconnect: the old WS closed, the server replaced our subscriber, and
    // we must mirror that by abandoning the old PC.
    async handleOffer(sdp: string, offerId?: string): Promise<void> {
        return this.enqueueSignaling(async () => {
            debugLog('Handling WebRTC offer');

            if (this.pc) {
                const state = this.pc.connectionState;
                const sig = this.pc.signalingState;
                // If we're partway through a fresh handshake (no local description
                // yet), reuse the pc. Otherwise assume this is a reconnect-initiated
                // fresh session and rebuild.
                if (state !== 'new' || sig !== 'stable') {
                    debugLog('Resetting stale peer connection before fresh offer', { state, sig });
                    this.resetPeerConnection();
                }
            }

            if (!this.pc) {
                this.createPeerConnection();
            }

            this.subscriberCandidateOfferId = offerId ?? null;
            const pc = this.pc!;

            try {
                // Set remote description (the offer from server). The subscriber
                // PC is receive-only and the client never offers on it, so no
                // pending-local-offer handling is needed here.
                const offer: RTCSessionDescriptionInit = { type: 'offer', sdp };
                await pc.setRemoteDescription(offer);
                debugLog('Set remote description');

                const answer = await pc.createAnswer();
                // Munge the Opus fmtp to advertise full-stereo program decode
                // before committing the local answer — the browser's default
                // answer follows RFC 7587 (mono) and would downmix the program
                // stream. Receive-side params only; no DTX/bitrate cap. The same
                // tuned SDP is what we signal, so both peers agree. Scoped to
                // the program m-line so voice m-lines (mono) keep the browser's
                // own fmtp.
                const tunedAnswer: RTCSessionDescriptionInit = {
                    type: answer.type,
                    sdp: tuneSubscriberAnswerOpus(answer.sdp ?? '', this.programAudioMid(sdp))
                };
                await pc.setLocalDescription(tunedAnswer);
                debugLog('Created and set local description (answer)');
                // A fresh subscription re-applies the munge from scratch, so any
                // mono fallback from the previous PC is over. Without this the
                // "playing in mono" notice would stick for the rest of the
                // session even though stereo came back.
                this.markProgramStereoRestored();

                const payload: { sdp: string | undefined; offerId?: string } = { sdp: tunedAnswer.sdp };
                if (offerId) {
                    payload.offerId = offerId;
                }
                if (!this.sendSignal('signal:answer', payload)) {
                    this.failSubscriberNegotiation('Failed to send WebRTC answer; resetting subscriber connection');
                    return;
                }
            } catch (err) {
                this.failSubscriberNegotiation('Failed to handle WebRTC offer; resetting subscriber connection', err);
                return;
            }

            // Outgoing media (mic, screen share) lives on the publisher PC,
            // which is independent of subscriber rebuilds — nothing to
            // re-attach here.
        });
    }

    // resetPeerConnection tears down the existing RTCPeerConnection without
    // touching localStream (the mic may still be granted) or the signaling
    // queue. Used on fresh-offer arrival when the old PC is stale.
    private resetPeerConnection(): void {
        if (this.connectionLostTimeout) {
            clearTimeout(this.connectionLostTimeout);
            this.connectionLostTimeout = null;
        }
        if (this.iceRestartTimeout) {
            clearTimeout(this.iceRestartTimeout);
            this.iceRestartTimeout = null;
        }
        this.iceRestartPending = false;
        this.iceRestartAttempted = false;
        this.iceRestartOfferId = null;
        this.iceRestartOfferSent = false;
        this.pendingIceRestartCandidates = [];
        // NOTE: audioSender/screenShareSender belong to the publisher PC,
        // which is independent of the subscriber PC reset here.
        if (this.pc) {
            try {
                this.pc.close();
            } catch {
                // already closed
            }
            this.pc = null;
        }
        this.subscriberCandidateOfferId = null;
        // The replacement PC negotiates its own answer from scratch, so a mono
        // fallback recorded against the old one no longer describes reality.
        this.markProgramStereoRestored();
        this.videoJitterBufferSamples.clear();
    }

    // Handle ICE candidate from server (if server sends any)
    async handleCandidate(candidate: RTCIceCandidateInit, offerId?: string): Promise<void> {
        return this.enqueueSignaling(async () => {
            if (offerId && this.subscriberCandidateOfferId && offerId !== this.subscriberCandidateOfferId) {
                console.warn('Ignoring stale ICE candidate for replaced subscriber connection', { offerId });
                return;
            }
            if (!this.pc) {
                console.warn('Received ICE candidate but no peer connection');
                return;
            }

            await this.pc.addIceCandidate(candidate);
            debugLog('Added ICE candidate from server');
        });
    }

    private createPeerConnection(): void {
        debugLog('Creating peer connection with ICE servers:', this.options.iceServers);

        const pc = new RTCPeerConnection({
            iceServers: this.options.iceServers
        });
        this.pc = pc;
        // Belongs here, not only in resetPeerConnection: handleOffer builds a
        // PC directly when there is none to tear down, so resetting only there
        // would let the flag outlive the connection it describes and suppress
        // the warning for every later subscription.
        this.programMidWarned = false;

        // Handle incoming tracks
        this.pc.ontrack = (event) => {
            const streamId = event.streams[0]?.id ?? '';
            const trackId = event.track.id;
            debugLog('Received track:', event.track.kind, 'trackId:', trackId, 'streamId:', streamId, 'streams:', event.streams.length);
            // Low-latency receiver hints target the program VIDEO path (a
            // 20 ms jitter target / playout hint). Applying them to audio
            // receivers would push program/voice audio toward video-style
            // jitter targets and can shrink the audio jitter buffer below what smooth
            // Opus decode needs. Only video receivers get the hints.
            if (event.track.kind === 'video') {
                this.tuneReceiverForLowLatency(event.receiver);
            }

            // Identify screen share tracks by stream/track ID.
            // Server creates relay tracks with:
            //   track ID:  "screenshare-{participantId}"
            //   stream ID: "screenshare-stream-{participantId}"
            let screenShareParticipantId: string | null = null;

            if (streamId.startsWith('screenshare-stream-')) {
                screenShareParticipantId = streamId.substring('screenshare-stream-'.length);
            } else if (trackId.startsWith('screenshare-')) {
                screenShareParticipantId = trackId.substring('screenshare-'.length);
            }

            if (screenShareParticipantId) {
                debugLog('Identified screen share track from participant:', screenShareParticipantId);
                this.options.onScreenShareTrack?.(screenShareParticipantId, event.track);
                return;
            }

            // Identify presence webcam tracks by stream/track ID.
            //   track ID:  "webcam-{participantId}"
            //   stream ID: "webcam-stream-{participantId}"
            let webcamParticipantId: string | null = null;
            if (streamId.startsWith('webcam-stream-')) {
                webcamParticipantId = streamId.substring('webcam-stream-'.length);
            } else if (trackId.startsWith('webcam-')) {
                webcamParticipantId = trackId.substring('webcam-'.length);
            }

            if (webcamParticipantId) {
                debugLog('Identified webcam track from participant:', webcamParticipantId);
                this.options.onWebcamTrack?.(webcamParticipantId, event.track);
                return;
            }

            // Identify voice tracks by stream/track ID.
            // Server creates voice relay tracks with:
            //   track ID:  "voice-{participantId}"
            //   stream ID: "voice-stream-{participantId}"
            // Some browsers (Firefox) replace track.id with a generated UUID,
            // so we check stream ID first (preserved per WebRTC spec via a=msid).
            let voiceParticipantId: string | null = null;

            if (streamId.startsWith('voice-stream-')) {
                voiceParticipantId = streamId.substring('voice-stream-'.length);
            } else if (trackId.startsWith('voice-')) {
                voiceParticipantId = trackId.substring('voice-'.length);
            }

            if (voiceParticipantId) {
                debugLog('Identified voice track from participant:', voiceParticipantId);
                if (this.options.onVoiceTrack) {
                    this.options.onVoiceTrack(voiceParticipantId, event.track);
                }
                return;
            }

            // Not a voice track - it's the main stream
            this.options.onTrack(event);
        };

        // Handle ICE candidates
        this.pc.onicecandidate = (event) => {
            // Guard against the old PC's trailing events after a rebuild: an
            // ICE candidate from a superseded connection must not be sent for
            // the current one.
            if (this.pc !== pc) {
                return;
            }
            if (event.candidate) {
                debugLog('Sending ICE candidate to server');
                this.sendSubscriberCandidate({
                    candidate: event.candidate.candidate,
                    sdpMid: event.candidate.sdpMid,
                    sdpMLineIndex: event.candidate.sdpMLineIndex
                });
            }
        };

        // Handle connection state changes
        this.pc.onconnectionstatechange = () => {
            const state = pc.connectionState;
            debugLog('Connection state:', state);

            // Identity guard: resetPeerConnection (reconnect/fresh-offer/ICE
            // restart failure) closes this PC and may null this.pc or replace
            // it with a new one. The old PC can still emit a final
            // 'closed'/'failed'/'disconnected' event asynchronously; without
            // this check it would spuriously fire performIceRestart or clear the
            // new connection's recovery timers (mirrors the publisher's guard).
            if (this.pc !== pc) {
                return;
            }

            // Notify callback
            if (state) {
                this.options.onConnectionStateChange?.(state);
            }

            // Handle connection failures
            if (state === 'disconnected') {
                // Always clear any existing timer before starting a new one —
                // rapid state oscillation (dis/connected/dis) otherwise leaves
                // stale timers that trigger unnecessary ICE restarts after the
                // connection has already recovered.
                if (this.connectionLostTimeout) {
                    clearTimeout(this.connectionLostTimeout);
                }
                this.connectionLostTimeout = setTimeout(() => {
                    this.connectionLostTimeout = null;
                    if (this.pc === pc && this.pc?.connectionState === 'disconnected') {
                        debugLog('Connection still disconnected, attempting ICE restart');
                        this.performIceRestart();
                    }
                }, WebRTCManager.DISCONNECTED_ICE_RESTART_MS);
            } else if (state === 'failed') {
                // Don't let the 'disconnected' timer trigger a second
                // restart on top of the one handled here.
                if (this.connectionLostTimeout) {
                    clearTimeout(this.connectionLostTimeout);
                    this.connectionLostTimeout = null;
                }
                if (this.iceRestartPending) {
                    if (this.iceRestartAttempted) {
                        // A restart offer was actually sent and the connection
                        // failed again - genuinely unrecoverable.
                        this.failIceRestart('ICE restart failed, connection unrecoverable');
                    } else {
                        // Restart is still being prepared (offer not yet
                        // sent) - don't declare failure prematurely.
                        debugLog('Connection failed while ICE restart is being prepared, waiting');
                    }
                } else {
                    // First failure - attempt ICE restart
                    debugLog('Connection failed, attempting ICE restart');
                    this.performIceRestart();
                }
            } else if (state === 'connected') {
                // Clear timeout if reconnected
                if (this.connectionLostTimeout) {
                    clearTimeout(this.connectionLostTimeout);
                    this.connectionLostTimeout = null;
                }
                if (this.iceRestartTimeout) {
                    clearTimeout(this.iceRestartTimeout);
                    this.iceRestartTimeout = null;
                }
                this.iceRestartPending = false;
                this.iceRestartAttempted = false;
                this.iceRestartOfferId = null;
                this.iceRestartOfferSent = false;
                this.pendingIceRestartCandidates = [];
            }
        };

        this.pc.oniceconnectionstatechange = () => {
            debugLog('ICE connection state:', this.pc?.iceConnectionState);
        };

        this.pc.onicegatheringstatechange = () => {
            debugLog('ICE gathering state:', this.pc?.iceGatheringState);
        };
    }

    private sendSubscriberCandidate(candidate: RTCIceCandidateInit): void {
        if (this.iceRestartPending && !this.iceRestartOfferSent) {
            this.pendingIceRestartCandidates.push(candidate);
            return;
        }

        this.sendSignal('signal:candidate', {
            ...candidate,
            offerId: this.subscriberCandidateOfferId ?? undefined
        });
    }

    private flushPendingIceRestartCandidates(): void {
        const pending = this.pendingIceRestartCandidates;
        this.pendingIceRestartCandidates = [];
        for (const candidate of pending) {
            this.sendSubscriberCandidate(candidate);
        }
    }

    private tuneReceiverForLowLatency(receiver: RTCRtpReceiver): void {
        type LowLatencyReceiver = RTCRtpReceiver & { playoutDelayHint?: number };
        const lowLatencyReceiver = receiver as LowLatencyReceiver;

        if ('jitterBufferTarget' in receiver) {
            try {
                receiver.jitterBufferTarget = WebRTCManager.LOW_LATENCY_JITTER_BUFFER_TARGET_MS;
            } catch (err) {
                console.warn('Could not set low-latency receiver jitter buffer target:', err);
            }
        }

        if ('playoutDelayHint' in lowLatencyReceiver) {
            try {
                lowLatencyReceiver.playoutDelayHint = WebRTCManager.LOW_LATENCY_PLAYOUT_DELAY_SECONDS;
            } catch (err) {
                console.warn('Could not set low-latency receiver playout hint:', err);
            }
        }
    }

    private videoReceiverLatencyTuning(): Pick<WebRTCStats, 'receiverJitterBufferTarget' | 'receiverPlayoutDelayHint'> {
        type LowLatencyReceiver = RTCRtpReceiver & { playoutDelayHint?: number };
        for (const receiver of this.pc?.getReceivers() ?? []) {
            if (receiver.track?.kind !== 'video') continue;

            const lowLatencyReceiver = receiver as LowLatencyReceiver;
            return {
                receiverJitterBufferTarget: 'jitterBufferTarget' in receiver
                    ? receiver.jitterBufferTarget
                    : undefined,
                receiverPlayoutDelayHint: 'playoutDelayHint' in lowLatencyReceiver
                    ? lowLatencyReceiver.playoutDelayHint
                    : undefined
            };
        }
        return {};
    }

    // Perform ICE restart to recover from connection issues.
    //
    // If the answer never arrives (dropped WS, server restart mid-flight) the
    // connection-state machine won't clear iceRestartPending — without the
    // timeout below, the flag would stay true forever and every subsequent
    // restart attempt would be a no-op, leaving the viewer stranded.
    private iceRestartTimeout: ReturnType<typeof setTimeout> | null = null;
    async performIceRestart(): Promise<void> {
        if (!this.pc || this.iceRestartPending) {
            return;
        }

        this.iceRestartPending = true;
        this.iceRestartAttempted = false;
        debugLog('Performing ICE restart...');
        this.options.onIceRestart?.();

        if (this.iceRestartTimeout) {
            clearTimeout(this.iceRestartTimeout);
        }
        this.iceRestartTimeout = setTimeout(() => {
            this.iceRestartTimeout = null;
            if (this.iceRestartPending) {
                this.failIceRestart('ICE restart answer not received within 8s');
            }
        }, WebRTCManager.ICE_RESTART_ANSWER_TIMEOUT_MS);

        return this.enqueueSignaling(async () => {
            if (!this.pc) return;

            const offerId = `ice-restart-${++this.iceRestartOfferCounter}`;
            const previousCandidateOfferId = this.subscriberCandidateOfferId;
            this.iceRestartOfferId = offerId;
            this.iceRestartOfferSent = false;
            this.pendingIceRestartCandidates = [];
            this.subscriberCandidateOfferId = offerId;

            try {
                const offer = await this.pc.createOffer({ iceRestart: true });
                await this.pc.setLocalDescription(offer);

                // Send offer to server for ICE restart
                if (!this.sendSignal('signal:ice-restart', {
                    sdp: offer.sdp,
                    offerId
                })) {
                    this.failIceRestart('ICE restart offer was not sent; rebuilding subscriber connection');
                    return;
                }

                this.iceRestartOfferSent = true;
                this.flushPendingIceRestartCandidates();

                // The restart has genuinely been attempted; a subsequent
                // 'failed' state now means the restart itself failed.
                this.iceRestartAttempted = true;

                debugLog('Sent ICE restart offer');
            } catch (err) {
                console.error('Failed to perform ICE restart:', err);
                if (this.subscriberCandidateOfferId === offerId) {
                    this.subscriberCandidateOfferId = previousCandidateOfferId;
                }
                this.failIceRestart('ICE restart setup failed; rebuilding subscriber connection');
            }
        });
    }

    private failIceRestart(message: string): void {
        console.warn(message);
        this.resetPeerConnection();
        this.options.onIceRestartFailed?.();
    }

    private failSubscriberNegotiation(message: string, err?: unknown): void {
        if (err !== undefined) {
            console.error(message, err);
        } else {
            console.warn(message);
        }
        this.resetPeerConnection();
        this.options.onNegotiationWedged?.();
    }

    private clearIceRestartAttempt(): void {
        if (this.iceRestartTimeout) {
            clearTimeout(this.iceRestartTimeout);
            this.iceRestartTimeout = null;
        }
        this.iceRestartPending = false;
        this.iceRestartAttempted = false;
        this.iceRestartOfferId = null;
        this.iceRestartOfferSent = false;
        this.pendingIceRestartCandidates = [];
    }

    // Request a stream resync (forces keyframe from publisher)
    requestResync(): void {
        const now = Date.now();
        if (this.lastResyncAt !== 0 && now - this.lastResyncAt < WebRTCManager.RESYNC_MIN_INTERVAL_MS) {
            return;
        }
        this.lastResyncAt = now;
        debugLog('Requesting stream resync (keyframe)');
        this.sendSignal('signal:resync', {});
    }

    // Apply a fresh set of ICE servers to the live peer connections AND to
    // the stored options (so subsequent subscriber/publisher rebuilds use
    // the fresh credentials, not the originals).
    //
    // Cloudflare TURN credentials have a default 1 h TTL; long grading
    // sessions outlive that. The active ICE allocation keeps working on
    // its already-authenticated session, but any future ICE restart needs
    // valid creds — without this, an ICE restart on a >1 h session would
    // gather with expired credentials and fail.
    //
    // setConfiguration only affects subsequent gathering; it does not disrupt
    // the running media relay.
    updateICEServers(iceServers: RTCIceServer[]): void {
        this.options = { ...this.options, iceServers };
        const refresh = (pc: RTCPeerConnection, label: string) => {
            try {
                pc.setConfiguration({ iceServers });
                debugLog(`Refreshed ICE servers on live ${label} peer connection`);
            } catch (err) {
                console.warn(`setConfiguration with fresh ICE servers failed on ${label} peer connection:`, err);
            }
        };

        if (this.pc) {
            refresh(this.pc, 'subscriber');
        }
        if (this.publisherPc) {
            refresh(this.publisherPc, 'publisher');
        }
    }

    // Get current connection state
    getConnectionState(): RTCPeerConnectionState | null {
        return this.pc?.connectionState ?? null;
    }

    // Get stats for latency/quality display. RTT is transport-only; inbound
    // video jitter-buffer delay is the browser-side media buffering we can
    // actively tune against for low-latency review.
    async getStats(): Promise<WebRTCStats> {
        if (!this.pc) {
            return {};
        }

        // Read-only: report the receiver latency hints the browser is actually
        // using. We do NOT re-stomp jitterBufferTarget/playoutDelayHint here:
        // those are applied once in ontrack, and rewriting them every poll
        // (this runs each second) fights the browser's adaptive jitter buffer
        // and adds avoidable per-poll work — both bad for a low-latency stream.
        const receiverLatencyTuning = this.videoReceiverLatencyTuning();
        const stats = await this.pc.getStats();
        // Prefer the nominated (actively used) candidate pair so the latency
        // display is stable; fall back to any succeeded pair if none is
        // marked nominated.
        let nominatedRtt: number | undefined;
        let fallbackRtt: number | undefined;
        let videoJitterBufferDelay: number | undefined;
        let videoFramesDropped: number | undefined;
        const seenVideoReports = new Set<string>();

        stats.forEach(report => {
            if (
                report.type === 'candidate-pair' &&
                report.state === 'succeeded' &&
                typeof report.currentRoundTripTime === 'number'
            ) {
                const rttMs = report.currentRoundTripTime * 1000; // Convert to ms
                if (report.nominated) {
                    nominatedRtt = rttMs;
                } else if (fallbackRtt === undefined) {
                    fallbackRtt = rttMs;
                }
            }

            if (
                report.type === 'inbound-rtp' &&
                (report.kind === 'video' || report.mediaType === 'video')
            ) {
                seenVideoReports.add(report.id);
                if (
                    typeof report.jitterBufferDelay === 'number' &&
                    typeof report.jitterBufferEmittedCount === 'number' &&
                    report.jitterBufferEmittedCount > 0
                ) {
                    const sample = this.videoJitterBufferDelayForReport(
                        report.id,
                        report.jitterBufferDelay,
                        report.jitterBufferEmittedCount
                    );
                    videoJitterBufferDelay = videoJitterBufferDelay === undefined
                        ? sample
                        : Math.max(videoJitterBufferDelay, sample);
                }
                if (typeof report.framesDropped === 'number') {
                    videoFramesDropped = report.framesDropped;
                }
            }
        });
        for (const id of this.videoJitterBufferSamples.keys()) {
            if (!seenVideoReports.has(id)) {
                this.videoJitterBufferSamples.delete(id);
            }
        }

        return {
            rtt: nominatedRtt ?? fallbackRtt,
            videoJitterBufferDelay,
            videoFramesDropped,
            ...receiverLatencyTuning
        };
    }

    private videoJitterBufferDelayForReport(id: string, delay: number, emittedCount: number): number {
        const previous = this.videoJitterBufferSamples.get(id);
        this.videoJitterBufferSamples.set(id, { delay, emittedCount });

        if (!previous) {
            // First sample for this report id: the cumulative average IS the
            // current average (fresh receiver), not a smoothed-over-fore value.
            return (delay / emittedCount) * 1000;
        }

        const countDelta = emittedCount - previous.emittedCount;
        if (countDelta <= 0) {
            // No new packets, or the counter went backwards (receiver recycled
            // under the same report id). With no new interval to measure, the
            // cumulative average reflects only what's accumulated so far — for a
            // recycled receiver the denominator is small, so this is the current
            // average, not a lifetime smoothing. Report it rather than the stale
            // previous value.
            return (delay / emittedCount) * 1000;
        }

        if (delay >= previous.delay) {
            // Monotonic cumulative counters: the per-packet delay over the last
            // interval is the real-time latency signal we want to surface.
            return ((delay - previous.delay) / countDelta) * 1000;
        }

        // delay decreased while emittedCount increased: a per-packet delta would
        // be negative (meaningless for a delay). Previously this fell back to the
        // cumulative average, which over a long session converges toward the
        // all-time mean and hides real-time latency spikes. Clamp the interval
        // contribution to zero instead — "no additional delay this interval" —
        // so a spike on the next monotonic sample still surfaces cleanly.
        return 0;
    }

    // Request microphone access and prepare for sending. Honors the persisted
    // device preference (chromatic_mic_device) unless an explicit deviceId is
    // passed.
    async requestMicrophone(deviceId?: string | null): Promise<boolean> {
        const preferred = deviceId ?? getStoredMicDeviceId();
        const ok = await this.acquireMic(preferred, false);
        if (ok) debugLog('Microphone access granted');
        return ok;
    }

    // Unified mic acquisition used by the initial request, device switching, and
    // audio mode/engine/headphone changes. Acquires with the current mode's
    // constraints, builds the mode-aware cleanup chain, swaps the new track into
    // any live sender via replaceTrack (same kind/m-line — no renegotiation for
    // a pure device swap), and releases the previous capture only on success.
    //
    // If talkback intends an in-app denoiser but it doesn't actually engage
    // (engine unimplemented or failed to load), it re-acquires ONCE with native
    // noise suppression so speech is never shipped un-denoised.
    private async acquireMic(deviceId: string | null, exact: boolean): Promise<boolean> {
        if (this.isClosed()) return false;
        const previousRaw = this.rawMicStream;
        // The previous cleanup chain stays alive until the new track is in the
        // sender: if replaceTrack fails, the sender is still playing the old
        // chain's output and disposing it early would kill the working mic.
        // installMicStream overwrites this.micChain; this handle is what we
        // dispose (on success) or reinstall (on failure).
        const previousChain = this.micChain;

        // Whether talkback intends to run an in-app denoiser at all (mode +
        // chosen, implemented engine). When false (studio, or engine "off"), we
        // do NOT turn native NS on — "off" means off.
        const wantInApp = this.wantsInAppDenoise();

        for (let attempt = 0; attempt < 2; attempt++) {
            // Build the denoiser on attempt 0 only; attempt 1 is the fallback.
            const useDenoiser = wantInApp && attempt === 0;
            // Native NS is enabled ONLY as the fallback (attempt 1) when an
            // intended in-app denoiser failed to engage — never alongside a
            // working denoiser, and never when the user chose "off".
            const noiseSuppression = wantInApp && attempt === 1;
            let raw: MediaStream;
            try {
                raw = await navigator.mediaDevices.getUserMedia({
                    audio: micConstraints({
                        deviceId,
                        exact,
                        mode: this.audioMode,
                        studioHeadphones: this.studioHeadphones,
                        noiseSuppression
                    }),
                    video: false
                });
            } catch (err) {
                console.error('Failed to get microphone access:', err);
                const name = (err as { name?: string })?.name ?? '';
                // A saved/selected mic that has vanished (hot-plugged capture or
                // Blackmagic audio churn) fails NotFound/Overconstrained. Forget
                // it and retry ONCE with the OS default before surfacing "blocked"
                // — otherwise a stale device id locks the user out of their mic.
                if ((name === 'OverconstrainedError' || name === 'NotFoundError') && deviceId) {
                    storeMicDeviceId(null);
                    return this.acquireMic(null, false);
                }
                return false;
            }

            if (this.isClosed()) {
                this.stopStream(raw);
                return false;
            }
            if (raw.getAudioTracks().length === 0) {
                this.stopStream(raw);
                return false;
            }

            await this.installMicStream(raw, useDenoiser);
            if (this.isClosed()) {
                // close() disposed the new chain; the previous one is only
                // referenced here now.
                previousChain?.dispose();
                if (previousRaw && previousRaw !== raw) this.stopStream(previousRaw);
                return false;
            }

            // Talkback wanted an in-app denoiser but it didn't engage — retry
            // once with native noise suppression (and no in-app denoiser) rather
            // than shipping raw noisy speech. (rawMicStream now points at this
            // attempt's capture.)
            if (useDenoiser && !(this.micChain?.denoiserActive ?? false)) {
                console.warn('In-app denoiser inactive; re-acquiring mic with native noise suppression');
                this.disposeMicChain();
                this.stopStream(raw);
                // Put the previous mic back while the fallback capture runs: if
                // it fails too, the sender is still transmitting that chain
                // and state must keep saying so.
                this.micChain = previousChain;
                this.rawMicStream = previousRaw;
                this.localStream = previousChain?.stream ?? previousRaw;
                continue;
            }

            const newTrack = this.localStream?.getAudioTracks()[0] ?? null;
            if (this.audioSender && newTrack) {
                try {
                    await this.audioSender.replaceTrack(newTrack);
                } catch (err) {
                    // Rare (same-kind replace). The sender is still on the
                    // previous track, so drop the new capture and restore the
                    // previous chain instead of tearing down the working mic.
                    console.error('Failed to replace mic track:', err);
                    this.disposeMicChain();
                    this.stopStream(raw);
                    if (this.isClosed()) {
                        // close() only stopped the new capture; the previous
                        // one is referenced nowhere else and would keep the
                        // device open.
                        previousChain?.dispose();
                        if (previousRaw && previousRaw !== raw) this.stopStream(previousRaw);
                        return false;
                    }
                    this.micChain = previousChain;
                    this.rawMicStream = previousRaw;
                    this.localStream = previousChain?.stream ?? previousRaw;
                    return false;
                }
            }

            // Release the previous capture only after the swap succeeded so a
            // failed switch leaves the working mic untouched. This runs even if
            // close() landed during replaceTrack: close() only knows about the
            // new capture, so the previous one would otherwise keep the device open.
            previousChain?.dispose();
            if (previousRaw && previousRaw !== raw) {
                this.stopStream(previousRaw);
            }
            return !this.isClosed();
        }
        // Both attempts failed to capture; the previous mic is still installed.
        return false;
    }

    // Routes a fresh mic capture through the RNNoise cleanup chain only when
    // that engine is active. "Noise reduction off" and the native-NS fallback
    // send the capture directly, avoiding the in-app gate on quiet Mac mics.
    // Does not dispose the chain it replaces: acquireMic keeps that alive
    // until the new track is confirmed in the sender.
    private async installMicStream(raw: MediaStream, useDenoiser: boolean): Promise<void> {
        this.rawMicStream = raw;
        // Detach the previous chain first so a close() racing createMicChain
        // cannot dispose it out from under acquireMic's handle.
        this.micChain = null;
        this.micChain = useDenoiser
            ? await createMicChain(raw, {
                    mode: this.audioMode,
                    denoiser: this.denoiserEngine
                })
            : null;
        if (this.isClosed()) {
            this.disposeMicChain();
            this.stopStream(raw);
            this.rawMicStream = null;
            this.localStream = null;
            return;
        }
        this.localStream = this.micChain ? this.micChain.stream : raw;

        // Respect current mic mute state so permission can be requested
        // before we begin sending audio.
        this.localStream.getAudioTracks().forEach(track => {
            track.enabled = !this.isMicMuted;
        });
    }

    private disposeMicChain(): void {
        if (this.micChain) {
            this.micChain.dispose();
            this.micChain = null;
        }
    }

    // Switch the microphone input device: re-acquire with the given deviceId and
    // swap the new track into the existing sender. No renegotiation needed.
    async setMicDevice(deviceId: string): Promise<boolean> {
        const ok = await this.acquireMic(deviceId, true);
        if (ok) debugLog('Switched microphone device:', deviceId);
        return ok;
    }

    // ---- Audio mode (talkback vs studio / critical-listening) ---------------
    getAudioMode(): AudioMode {
        return this.audioMode;
    }
    getDenoiserEngine(): DenoiserEngine {
        return this.denoiserEngine;
    }
    isStudioHeadphones(): boolean {
        return this.studioHeadphones;
    }

    // Switch talkback <-> studio. Re-acquires the mic with the new constraints,
    // rebuilds the chain, retunes the send bitrate, and renegotiates the
    // publisher so the new per-mode Opus preferences take effect.
    async setAudioMode(mode: AudioMode): Promise<void> {
        if (this.audioMode === mode) return;
        this.audioMode = mode;
        this.persistAudioModeState();
        await this.reapplyAudioSettings();
    }

    // Change the talkback denoiser engine. Rebuilds the mic chain (different
    // worklet); no renegotiation (codec unchanged). No effect in studio mode.
    async setDenoiserEngine(engine: DenoiserEngine): Promise<void> {
        if (this.denoiserEngine === engine) return;
        this.denoiserEngine = engine;
        this.persistAudioModeState();
        if (this.audioMode === 'talkback') {
            await this.reapplyAudioSettings();
        }
    }

    // Studio only: toggle whether echo cancellation is removed (headphones).
    // Changes a capture constraint, so it re-acquires the mic.
    async setStudioHeadphones(on: boolean): Promise<void> {
        if (this.studioHeadphones === on) return;
        this.studioHeadphones = on;
        this.persistAudioModeState();
        if (this.audioMode === 'studio') {
            await this.reapplyAudioSettings();
        }
    }

    // Re-acquire the current mic with up-to-date constraints, retune the sender
    // bitrate, and renegotiate the publisher so Opus fmtp reflects the mode.
    // No-op when there is no active capture yet — the next mic-enable picks up
    // the new settings.
    private async reapplyAudioSettings(): Promise<void> {
        if (this.isClosed() || !this.rawMicStream) return;
        const deviceId = this.getCurrentMicDeviceId();
        if (!(await this.acquireMic(deviceId, false))) {
            console.warn('Audio settings not applied: mic re-acquire failed; previous mic kept');
            return;
        }
        if (this.isClosed()) return;
        await this.tuneAudioSender();
        if (this.audioSender && this.publisherPc) {
            await this.negotiatePublisher();
        }
    }

    // Apply the current mode's send bitrate cap to the audio sender. Mirrors
    // tuneShareSender. Opus fmtp (stereo/dtx/fec) is set separately on the
    // publisher offer via applyOpusPreferences.
    private async tuneAudioSender(): Promise<void> {
        const sender = this.audioSender;
        if (!sender) return;
        try {
            const params = sender.getParameters();
            if (!params.encodings || params.encodings.length === 0) {
                params.encodings = [{}];
            }
            params.encodings[0].maxBitrate = opusPreferencesFor(this.audioMode).maxBitrate;
            await sender.setParameters(params);
        } catch (err) {
            console.warn('Could not tune audio sender parameters:', err);
        }
    }

    // deviceId of the currently captured microphone, if any.
    getCurrentMicDeviceId(): string | null {
        const track = (this.rawMicStream ?? this.localStream)?.getAudioTracks()[0];
        if (!track) return null;
        try {
            return track.getSettings().deviceId ?? null;
        } catch {
            return null;
        }
    }

    // Enable/disable microphone
    setMicEnabled(enabled: boolean): void {
        this.isMicMuted = !enabled;

        if (this.localStream) {
            this.localStream.getAudioTracks().forEach(track => {
                track.enabled = enabled;
            });
        }

        // Publishing rides its own PC — no subscriber connection needed.
        // Guard against teardown: if close() ran between the user toggling the
        // mic and this async path, localStream is already null and we must not
        // spin up a publisher that would leak (and send publish:offer over a
        // dead socket). The isClosed() check inside addLocalAudioTrack covers
        // the await window too.
        if (enabled && this.localStream && !this.isClosed()) {
            this.addLocalAudioTrack().catch(err => {
                console.error('Failed to add local audio track, reverting mic state:', err);
                this.isMicMuted = true;
                if (this.localStream) {
                    this.localStream.getAudioTracks().forEach(t => { t.enabled = false; });
                }
            });
        }

        debugLog('Microphone', enabled ? 'enabled' : 'muted');
    }

    // Check if mic is currently enabled
    isMicEnabled(): boolean {
        return !this.isMicMuted;
    }

    // Add local audio track to the publisher connection
    private async addLocalAudioTrack(): Promise<void> {
        // Bail immediately if teardown already happened (the caller's synchronous
        // isClosed() check doesn't cover this async function's body).
        if (this.isClosed() || !this.localStream) return;

        const audioTrack = this.localStream.getAudioTracks()[0];
        if (!audioTrack) return;

        // Check if we already have a sender
        if (this.audioSender) {
            // Replace track
            await this.audioSender.replaceTrack(audioTrack);
            // close() may have run during the await; don't touch publisher state.
            if (this.isClosed()) return;
        } else {
            // Add new track on the publisher PC and negotiate. ensurePublisher
            // refuses to create a PC after close(), so a late mic-enable can't
            // leak an orphaned publisher or send publish:offer on a dead socket.
            const pub = this.ensurePublisher();
            this.audioSender = pub.addTrack(audioTrack, this.localStream);
            void this.tuneAudioSender();
            if (!(await this.negotiatePublisher())) {
                throw new Error('publisher offer was not sent');
            }
        }
    }

    // ---- Publisher peer connection -----------------------------------------
    // All outgoing media (mic, screen share) flows over a dedicated PC where
    // the CLIENT is always the offerer and the server only answers. The
    // subscriber PC (this.pc) is server-offer-only. Mixing offer directions
    // on one PC is what caused every signaling wedge in the field: Chrome's
    // "Failed to apply demuxer criteria" re-offer crashes and Safari's
    // unrecoverable "Failed to set SSL role" rollback failures. A wedged
    // publisher carries no inbound state, so the recovery story is trivial:
    // tear it down and rebuild it with the current local tracks.

    private ensurePublisher(): RTCPeerConnection {
        if (this.publisherPc) return this.publisherPc;

        // Never (re)create a publisher after teardown. close() nulls publisherPc,
        // so without this guard a late async caller (e.g. setMicEnabled →
        // addLocalAudioTrack, or a screen-share start racing leave-session)
        // would spin up an orphaned PC that is never closed, leaks its tracks,
        // and tries to negotiate over a dead WebSocket.
        if (this.isClosed()) {
            throw new Error('cannot create publisher peer connection after close');
        }

        const pc = new RTCPeerConnection({ iceServers: this.options.iceServers });
        pc.onicecandidate = (event) => {
            if (event.candidate) {
                this.sendPublisherCandidate({
                    candidate: event.candidate.candidate,
                    sdpMid: event.candidate.sdpMid,
                    sdpMLineIndex: event.candidate.sdpMLineIndex
                }, pc);
            }
        };
        pc.onconnectionstatechange = () => {
            debugLog('Publisher connection state:', pc.connectionState);
            if (this.publisherPc !== pc) {
                return;
            }

            if (pc.connectionState === 'disconnected') {
                this.clearPublisherDisconnectWatchdog();
                this.publisherDisconnectedTimeout = setTimeout(() => {
                    this.publisherDisconnectedTimeout = null;
                    if (this.publisherPc === pc && pc.connectionState === 'disconnected') {
                        console.warn('Publisher remained disconnected; rebuilding publisher');
                        this.rebuildPublisher();
                    }
                }, WebRTCManager.PUBLISHER_DISCONNECTED_REBUILD_MS);
            } else if (pc.connectionState === 'failed') {
                this.clearPublisherDisconnectWatchdog();
                this.rebuildPublisher();
            } else if (pc.connectionState === 'connected' || pc.connectionState === 'closed') {
                this.clearPublisherDisconnectWatchdog();
            }
        };
        this.publisherPc = pc;
        return pc;
    }

    private sendPublisherCandidate(candidate: RTCIceCandidateInit, pc: RTCPeerConnection): void {
        if (this.publisherPc !== pc) {
            console.warn('Ignoring ICE candidate from replaced publisher connection');
            return;
        }
        if (!this.publisherOfferSent) {
            this.pendingPublisherCandidates.push(candidate);
            return;
        }
        this.sendPublisherCandidateForCurrentOffer(candidate);
    }

    private flushPendingPublisherCandidates(): void {
        const pending = this.pendingPublisherCandidates;
        this.pendingPublisherCandidates = [];
        for (const candidate of pending) {
            this.sendPublisherCandidateForCurrentOffer(candidate);
        }
    }

    private sendPublisherCandidateForCurrentOffer(candidate: RTCIceCandidateInit): void {
        this.sendSignal('publish:candidate', {
            ...candidate,
            offerId: this.publisherCandidateOfferId ?? undefined
        });
    }

    // Create and send an offer on the publisher PC. The server answers
    // immediately (no offer can ever be in flight from the server on this
    // PC), so an unanswered offer means a lost message or wedged PC — the
    // watchdog rebuilds the publisher from scratch in that case.
    private async negotiatePublisher(): Promise<boolean> {
        return this.enqueueSignaling(async () => {
            const pc = this.publisherPc;
            if (!pc) return false;

            try {
                this.publisherOfferSent = false;
                this.pendingPublisherCandidates = [];
                if (pc.signalingState !== 'stable') {
                    this.publisherNeedsRenegotiation = true;
                    debugLog('Publisher offer already in flight; deferring renegotiation', { signalingState: pc.signalingState });
                    return true;
                }
                const offerId = `publish-${++this.publisherOfferCounter}`;
                this.publisherOfferId = offerId;
                this.publisherCandidateOfferId = offerId;
                const offer = await pc.createOffer();
                // Steer the browser's Opus encoder per mode (mono voice with DTX
                // vs stereo hi-fi) by editing only Opus fmtp params — never
                // m-lines/payloads, which would wedge renegotiation. The same
                // munged SDP is set locally and signaled so both sides agree.
                const tunedSdp = applyOpusPreferences(
                    offer.sdp ?? '',
                    opusPreferencesFor(this.audioMode)
                );
                await pc.setLocalDescription({ type: offer.type, sdp: tunedSdp });
                // Re-announce the webcam by its now-assigned transceiver mid
                // (set by setLocalDescription) BEFORE the offer reaches the
                // server, so its OnTrack can route the cam by mid. track.id is
                // announced too but Firefox/Safari rewrite it in the msid; the
                // mid is the reliable anchor. Idempotent, so running on every
                // negotiation also re-maps the cam after a publisher rebuild.
                this.announceWebcamMid(pc);
                if (!this.sendSignal('publish:offer', { sdp: tunedSdp, offerId })) {
                    throw new Error('publisher offer send failed');
                }
                this.publisherOfferSent = true;
                this.flushPendingPublisherCandidates();
                this.startPublishAnswerWatchdog();
                debugLog('Sent publisher offer');
                return true;
            } catch (err) {
                const e = err as Error;
                console.error('Failed to negotiate publisher:', err);
                try {
                    this.sendSignal('client:debug', {
                        event: 'publish-negotiate-failed',
                        detail: `${e?.name ?? 'Error'}: ${e?.message ?? String(err)} (state=${pc.signalingState})`
                    });
                } catch {
                    // diagnostics must never throw
                }
                this.publisherOfferSent = false;
                this.publisherOfferId = null;
                this.publisherCandidateOfferId = null;
                this.pendingPublisherCandidates = [];
                this.pendingRemotePublisherCandidates = [];
                this.publisherNeedsRenegotiation = false;
                if (this.publisherPc === pc) {
                    this.clearPublisherDisconnectWatchdog();
                    try {
                        pc.close();
                    } catch {
                        // already closed
                    }
                    this.publisherPc = null;
                    this.audioSender = null;
                    this.screenShareSender = null;
                    // Also drop cameraSender — it references a sender on the PC
                    // we just closed. Leaving it set makes isCameraOn() report a
                    // live cam that no peer receives, and enableCamera() early-
                    // returns "already broadcasting", so the user can never
                    // restore it. Every other publisher-teardown site clears it.
                    this.cameraSender = null;
                }
                return false;
            }
        });
    }

    // Announce the live webcam by its transceiver mid so the SFU can tell it
    // apart from a screen share on the shared publisher PC. The mid (assigned by
    // setLocalDescription) is stable across browsers; the local track id is sent
    // too for the id-matching path. No-op when the cam isn't broadcasting.
    private announceWebcamMid(pc: RTCPeerConnection): void {
        if (!this.cameraSender) return;
        const tr = pc.getTransceivers().find((t) => t.sender === this.cameraSender);
        const mid = tr?.mid ?? '';
        const trackId = this.cameraSender.track?.id ?? '';
        if (!mid && !trackId) return;
        this.sendSignal('webcam:start', { trackId, mid });
    }

    // Apply an ICE candidate the SFU gathered for the publisher PC.
    //
    // The publisher used to be vanilla-ICE in this direction: the SFU had no way
    // to send candidates, so every one it had to offer needed to be inside the
    // answer, which is why the answer waits on gathering at all. Anything
    // gathered after that deadline was simply lost. Trickle makes those late
    // candidates deliverable, so a slow-gathering path degrades into a slightly
    // later connection instead of a missing one.
    async handlePublisherCandidate(candidate: RTCIceCandidateInit, offerId?: string): Promise<void> {
        return this.enqueueSignaling(async () => {
            // Generation guard: a candidate from a superseded publisher must
            // never be applied to its replacement. publisherCandidateOfferId is
            // set when we send an offer and cleared on rebuild/close, so it
            // identifies the negotiation this PC belongs to.
            if (offerId && this.publisherCandidateOfferId && offerId !== this.publisherCandidateOfferId) {
                debugLog('Ignoring publisher candidate from a superseded negotiation', offerId);
                return;
            }
            const pc = this.publisherPc;
            if (!pc) return;

            // Before the answer lands there is no remote description to attach
            // a candidate to. Buffer rather than drop.
            if (!pc.remoteDescription) {
                this.pendingRemotePublisherCandidates.push(candidate);
                return;
            }
            try {
                await pc.addIceCandidate(candidate);
            } catch (err) {
                // A rejected candidate costs one path, not the connection.
                console.warn('Failed to add publisher ICE candidate', err);
            }
        });
    }

    // Apply the server's answer to the publisher offer.
    async handlePublishAnswer(sdp: string, offerId?: string): Promise<void> {
        return this.enqueueSignaling(async () => {
            const pc = this.publisherPc;
            if (!pc) return;
            if (offerId && this.publisherOfferId && offerId !== this.publisherOfferId) {
                console.warn('Ignoring stale publisher answer', { offerId });
                return;
            }
            if (pc.signalingState !== 'have-local-offer') {
                console.warn('Ignoring publisher answer with no local offer pending', { signalingState: pc.signalingState });
                return;
            }
            await pc.setRemoteDescription({ type: 'answer', sdp });
            this.clearPublishAnswerWatchdog();
            this.publisherOfferId = null;
            // The PC can take candidates now — drain anything that arrived
            // ahead of the answer.
            const buffered = this.pendingRemotePublisherCandidates;
            this.pendingRemotePublisherCandidates = [];
            for (const candidate of buffered) {
                try {
                    await pc.addIceCandidate(candidate);
                } catch (err) {
                    console.warn('Failed to add buffered publisher ICE candidate', err);
                }
            }
            if (this.screenShareSender) {
                await this.tuneShareSender(this.screenShareSender);
            }
            if (this.publisherNeedsRenegotiation) {
                this.publisherNeedsRenegotiation = false;
                void this.negotiatePublisher();
            }
            debugLog('Publisher answer applied');
        });
    }

    // Tear down and recreate the publisher with the current local tracks.
    // Safe at any time: the publisher has no inbound state to lose.
    private rebuildPublisher(): void {
        // Teardown wins: never rebuild a publisher after close(). The watchdog
        // already guards on identity (this.publisherPc !== pc), but a callback
        // racing close() could still reach here; bail instead of spinning up an
        // orphaned PC.
        if (this.isClosed()) return;
        console.warn('Rebuilding publisher peer connection');
        this.clearPublishAnswerWatchdog();
        this.clearPublisherDisconnectWatchdog();
        const old = this.publisherPc;
        this.publisherPc = null;
        this.publisherOfferSent = false;
        this.publisherOfferId = null;
        this.publisherCandidateOfferId = null;
        this.pendingPublisherCandidates = [];
        this.pendingRemotePublisherCandidates = [];
        this.publisherNeedsRenegotiation = false;
        this.audioSender = null;
        this.screenShareSender = null;
        this.cameraSender = null;
        try {
            old?.close();
        } catch {
            // already closed
        }

        const audioTrack = this.localStream?.getAudioTracks()[0];
        const shareTrack = this.screenShareStream?.getVideoTracks()[0];
        const camTrack = this.cameraStream?.getVideoTracks()[0];
        if (
            !audioTrack &&
            !(shareTrack && shareTrack.readyState === 'live') &&
            !(camTrack && camTrack.readyState === 'live')
        ) {
            return; // nothing to publish; next mic/share/cam enable recreates it
        }

        const pc = this.ensurePublisher();
        if (audioTrack && this.localStream) {
            this.audioSender = pc.addTrack(audioTrack, this.localStream);
            void this.tuneAudioSender();
        }
        if (shareTrack && shareTrack.readyState === 'live' && this.screenShareStream) {
            this.screenShareSender = pc.addTrack(shareTrack, this.screenShareStream);
            void this.tuneShareSender(this.screenShareSender);
        }
        if (camTrack && camTrack.readyState === 'live' && this.cameraStream) {
            // The cam track id is unchanged across the rebuild, so the server's
            // existing webcam mapping still matches when OnTrack re-fires.
            this.cameraSender = pc.addTrack(camTrack, this.cameraStream);
            void this.tuneWebcamSender(this.cameraSender);
        }
        void this.negotiatePublisher();
    }

    // Raise the share encoder's quality ceiling: more bitrate headroom and
    // resolution-first degradation. Defaults cap screen shares around
    // 2.5 Mbps, which reads soft for color-review content.
    private async tuneShareSender(sender: RTCRtpSender): Promise<void> {
        try {
            const params = sender.getParameters();
            params.degradationPreference = 'maintain-resolution';
            if (!params.encodings || params.encodings.length === 0) {
                params.encodings = [{}];
            }
            params.encodings[0].maxBitrate = 8_000_000;
            await sender.setParameters(params);
        } catch (err) {
            // Browser-dependent (Safari is picky pre-negotiation) — defaults
            // still work, just softer.
            console.warn('Could not tune share sender parameters:', err);
        }
    }

    private startPublishAnswerWatchdog(): void {
        this.clearPublishAnswerWatchdog();
        this.voiceOfferTimer = setTimeout(() => {
            this.voiceOfferTimer = null;
            if (this.publisherPc?.signalingState === 'have-local-offer') {
                console.warn('Publisher offer unanswered; rebuilding publisher');
                this.rebuildPublisher();
            }
        }, WebRTCManager.VOICE_OFFER_TIMEOUT_MS);
    }

    private clearPublishAnswerWatchdog(): void {
        if (this.voiceOfferTimer) {
            clearTimeout(this.voiceOfferTimer);
            this.voiceOfferTimer = null;
        }
    }

    private clearPublisherDisconnectWatchdog(): void {
        if (this.publisherDisconnectedTimeout) {
            clearTimeout(this.publisherDisconnectedTimeout);
            this.publisherDisconnectedTimeout = null;
        }
    }

    // Handle answer for voice renegotiation
    async handleVoiceAnswer(sdp: string, offerId?: string): Promise<void> {
        return this.enqueueSignaling(async () => {
            if (!this.pc) return;

            if (offerId && this.iceRestartOfferId && offerId !== this.iceRestartOfferId) {
                console.warn('Ignoring stale ICE restart answer', { offerId });
                return;
            }

            if (this.pc.signalingState !== 'have-local-offer') {
                console.warn('Ignoring stale answer with no local offer pending', { signalingState: this.pc.signalingState });
                return;
            }

            const answer: RTCSessionDescriptionInit = {
                type: 'answer',
                sdp: sdp
            };

            await this.pc.setRemoteDescription(answer);
            if (offerId) {
                this.subscriberCandidateOfferId = offerId;
            }
            this.clearIceRestartAttempt();
            debugLog('Set remote description for answer');
        });
    }

    // Handle server-initiated renegotiation (e.g., when voice tracks are added).
    // If it collides with a client ICE-restart offer, the server offer wins:
    // rollback the local offer, cancel that restart attempt, and answer the
    // server immediately. The server already performs the matching rollback
    // when it receives an ICE restart during its own pending offer.
    async handleRenegotiation(sdp: string, participantId?: string, offerId?: string): Promise<void> {
        return this.enqueueSignaling(async () => {
            if (!this.pc) {
                console.warn('Received renegotiation but no peer connection');
                return;
            }

            debugLog('Handling server-initiated renegotiation', participantId ? `for voice from ${participantId}` : '');
            this.options.onRenegotiation?.();
            const previousCandidateOfferId = this.subscriberCandidateOfferId;

            const pc = this.pc;
            const stateBefore = pc.signalingState;

            try {
                if (pc.signalingState === 'have-local-offer') {
                    debugLog('Rolling back local subscriber offer before server renegotiation');
                    await pc.setLocalDescription({ type: 'rollback' });
                    this.clearIceRestartAttempt();
                }

                this.subscriberCandidateOfferId = offerId ?? null;
                await pc.setRemoteDescription({ type: 'offer', sdp });
                const answer = await pc.createAnswer();

                // Same stereo-decode munge as the initial answer: server
                // renegotiation (e.g. a new program/voice track) must not let
                // the browser fall back to a mono Opus answer.
                //
                // If the browser refuses the tuned SDP we must still answer.
                // The SFU cannot roll back its own offer (pion v4 rejects
                // SetLocalDescription(rollback) from HaveLocalOffer — see
                // sfu.go HandleIceRestart/HandleSubscriberOffer), so a
                // subscriber that goes quiet here leaves the server pinned in
                // have-local-offer, where every later track-add is dropped by
                // the stable-state guard and that viewer silently goes deaf to
                // everyone who joins afterwards. Answering untuned keeps the
                // session alive and the state machine moving; program audio
                // rides mono until the next renegotiation re-applies the munge,
                // and the UI says so rather than pretending it's clean.
                let answerSdp: string | undefined;
                try {
                    const tunedAnswer: RTCSessionDescriptionInit = {
                        type: answer.type,
                        sdp: tuneSubscriberAnswerOpus(answer.sdp ?? '', this.programAudioMid(sdp))
                    };
                    await pc.setLocalDescription(tunedAnswer);
                    answerSdp = tunedAnswer.sdp;
                    this.markProgramStereoRestored();
                } catch (mungeErr) {
                    // Let the browser author and apply its own answer — that
                    // form cannot be rejected as a modification.
                    await pc.setLocalDescription();
                    answerSdp = pc.localDescription?.sdp;
                    this.programStereoDegraded = true;
                    this.renegotiationDebug(
                        'munge-rejected',
                        `${describeError(mungeErr)} before=${stateBefore} after=${pc.signalingState}`
                    );
                    this.options.onProgramAudioDegraded?.(true);
                }

                const payload: { sdp: string | undefined; offerId?: string } = { sdp: answerSdp };
                if (offerId) {
                    payload.offerId = offerId;
                }

                if (!this.sendSignal('signal:renegotiate-answer', payload)) {
                    // The websocket is gone, so there is no answer to deliver
                    // and no way to ask for a replacement. Reconnect logic owns
                    // recovery from here.
                    this.renegotiationDebug('answer-undeliverable', `state=${pc.signalingState}`);
                    return;
                }

                debugLog('Sent renegotiation answer');
            } catch (err) {
                this.subscriberCandidateOfferId = previousCandidateOfferId;
                this.renegotiationDebug(
                    'failed',
                    `${describeError(err)} before=${stateBefore} after=${pc.signalingState} conn=${pc.connectionState}`
                );
                // A renegotiation that cannot be applied does not invalidate the
                // media already flowing. Destroying the connection here is what
                // turned one wedged offer into a room-wide reconnect cascade on
                // 2026-08-02: every rebuild re-published voice, which forced a
                // renegotiation on everyone else, which wedged the next viewer.
                // Only tear down a connection that is genuinely dead; otherwise
                // keep the stream up and let the next renegotiation resolve it.
                // Identity guard: an await above may have yielded long enough
                // for a fresh offer to replace this PC, and tearing down then
                // would kill the healthy replacement instead.
                if (this.pc !== pc) {
                    debugLog('Renegotiation failed on a superseded peer connection; ignoring');
                    return;
                }
                if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
                    this.failSubscriberNegotiation('Renegotiation failed on a dead connection; resetting subscriber', err);
                    return;
                }
                console.warn('Renegotiation could not be applied; keeping the existing connection', err);
            }
        });
    }

    // Breadcrumb reporting for subscriber renegotiation, mirrored to the server
    // log for the same reason as shareDebug: these failures only reproduce on a
    // participant's own machine mid-session, and the 2026-08-02 cascade had to
    // be diagnosed by inference because the browser-side exception never left
    // the client. Now the exception names itself in `docker logs chromatic`.
    // Locates the program-audio m-line in a server offer. A null result silently
    // reverts the munge to whole-SDP scope, which is the pre-fix behavior that
    // synthesized fmtp lines onto voice m-lines — so it must be visible in the
    // server log rather than looking identical to the fixed path.
    private programAudioMid(offerSdp: string): string | null {
        const mid = findProgramAudioMid(offerSdp);
        // Reported once per peer connection: if the offer shape ever changes
        // this is true of every renegotiation, and one line per voice track
        // would drown the log it exists to make readable.
        if (mid === null && !this.programMidWarned) {
            this.programMidWarned = true;
            this.renegotiationDebug('program-mid-unknown', 'tuning every Opus m-line');
        }
        return mid;
    }

    // Called wherever a tuned answer is applied successfully. Idempotent: only
    // reports when there was an outstanding degradation to clear.
    private markProgramStereoRestored(): void {
        if (!this.programStereoDegraded) return;
        this.programStereoDegraded = false;
        this.renegotiationDebug('stereo-restored');
        this.options.onProgramAudioDegraded?.(false);
    }

    private renegotiationDebug(event: string, detail = ''): void {
        debugLog(`[reneg] ${event}`, detail);
        try {
            this.sendSignal('client:debug', { event: `reneg:${event}`, detail });
        } catch {
            // never let diagnostics break renegotiation
        }
    }

    // Start screen sharing — captures display and adds video track to peer connection
    // Breadcrumb reporting for the share flow: mirrored to the server log so
    // failures on remote testers' machines are diagnosable without console
    // access (multiple field reports of shares silently not arriving).
    private shareDebug(event: string, detail = ''): void {
        debugLog(`[share] ${event}`, detail);
        try {
            this.sendSignal('client:debug', { event: `share:${event}`, detail });
        } catch {
            // never let diagnostics break the share flow
        }
    }

    async startScreenShare(): Promise<boolean> {
        try {
            if (this.isClosed()) return false;
            this.shareDebug('capture-requested');
            this.screenShareStream = await navigator.mediaDevices.getDisplayMedia({
                // Capture at native resolution up to 4K; review content is
                // detail-critical so resolution beats framerate.
                video: {
                    width: { ideal: 3840 },
                    height: { ideal: 2160 },
                    frameRate: { ideal: 30 }
                },
                // Screen-share AUDIO stays off: the SFU's publisher audio
                // branch classifies any audio track as voice, so tab/system
                // audio captured here would be relayed as a talkback voice
                // track and mixed at voice gain — wrong for shared program
                // audio. Enabling audio:true needs a dedicated screen-share
                // audio relay + a frontend track classifier BEFORE this offer
                // is negotiated. Keep it false until that path exists.
                audio: false
            });
            if (this.isClosed()) {
                this.stopStream(this.screenShareStream);
                this.screenShareStream = null;
                return false;
            }

            const videoTrack = this.screenShareStream.getVideoTracks()[0];
            if (!videoTrack) {
                this.shareDebug('failed', 'capture has no video track');
                this.screenShareStream = null;
                return false;
            }
            const s = videoTrack.getSettings();
            this.shareDebug('capture-acquired', `${s.width}x${s.height} state=${videoTrack.readyState}`);

            // Listen for browser "Stop sharing" button
            videoTrack.onended = () => {
                debugLog('Screen share ended by user (browser chrome)');
                this.stopScreenShare();
                this.options.onScreenShareEnded?.();
            };

            // Screen content is detail-critical: bias the encoder toward
            // sharpness (it will drop framerate before resolution).
            try {
                videoTrack.contentHint = 'detail';
            } catch {
                // older browsers — fine without the hint
            }

            // Add track to the publisher PC and negotiate
            const pub = this.ensurePublisher();
            this.screenShareSender = pub.addTrack(videoTrack, this.screenShareStream);
            void this.tuneShareSender(this.screenShareSender);
            this.shareDebug('track-added', `publisher=${pub.connectionState}/${pub.signalingState}`);
            const negotiated = await this.negotiatePublisher();
            if (!negotiated) {
                // Publisher signaling failed — rebuild it from scratch (it
                // carries no inbound state, so this is always safe). The
                // rebuild re-adds the live capture and negotiates again.
                this.shareDebug('negotiation-wedged', 'rebuilding publisher');
                this.rebuildPublisher();
            } else {
                this.shareDebug('offer-sent', `publisher=${this.publisherPc?.signalingState ?? 'gone'}`);
            }

            debugLog('Screen share started');
            return true;
        } catch (err) {
            const e = err as Error;
            this.shareDebug('failed', `${e?.name ?? 'Error'}: ${e?.message ?? String(err)}`);
            console.error('Failed to start screen share:', err);
            this.screenShareStream = null;
            return false;
        }
    }

    // Local screen capture stream (for the sharer's self-preview panel)
    getScreenShareStream(): MediaStream | null {
        return this.screenShareStream;
    }

    // Stop screen sharing
    stopScreenShare(): void {
        const needsRenegotiation = this.screenShareSender != null && this.publisherPc != null;

        if (this.screenShareSender && this.publisherPc) {
            try {
                this.publisherPc.removeTrack(this.screenShareSender);
            } catch (err) {
                console.error('Failed to remove screen share track:', err);
            }
            this.screenShareSender = null;
        }

        if (this.screenShareStream) {
            this.screenShareStream.getTracks().forEach(track => track.stop());
            this.screenShareStream = null;
        }

        // Renegotiate so the server knows the track was removed
        if (needsRenegotiation) {
            void this.negotiatePublisher();
        }

        debugLog('Screen share stopped');
    }

    // ---- Presence webcam ----------------------------------------------------
    // A small low-res cam published on the publisher PC. Both this and screen
    // share are VP8 video, so we announce the cam track id via `webcam:start`
    // BEFORE negotiating — the server routes by that id (see websocket.go).

    isCameraOn(): boolean {
        return this.cameraSender != null;
    }

    // Local capture for the user's own self-view tile.
    getCameraStream(): MediaStream | null {
        return this.cameraStream;
    }

    setWebcamVisible(visible: boolean): void {
        const track = this.cameraStream?.getVideoTracks()[0];
        if (!track || track.readyState !== 'live') return;
        track.enabled = visible;
        this.sendSignal('webcam:visibility', { visible });
    }

    // Why the last camera capture failed, for the UI's error copy. Null when the
    // camera has never failed (or has since succeeded).
    getLastCameraError(): unknown {
        return this.lastCameraError;
    }

    getCurrentCameraDeviceId(): string | null {
        const track = this.cameraStream?.getVideoTracks()[0];
        try {
            return track?.getSettings().deviceId ?? null;
        } catch {
            return null;
        }
    }

    // The camera is captured ONCE: the self-view preview and the broadcast share
    // a single getUserMedia stream and a single MediaStream identity. The modal
    // calls openCameraPreview() (device light on, self-view binds the stream);
    // Enable calls enableCamera(), which only ADDS the already-live track to the
    // publisher and negotiates. Nothing here ever stops a live capture except
    // stopWebcam()/cancelCameraPreview() — so clicking Enable can never turn the
    // camera light off (the old handoff path stopped the stream in a guard
    // branch and returned a stale one, which is what blacked the tile).

    private hasLiveVideo(stream: MediaStream | null): boolean {
        const t = stream?.getVideoTracks()[0];
        return !!t && t.readyState === 'live';
    }

    private async getCameraCapture(preferred: string | null, exactPreferred = false): Promise<MediaStream> {
        const attempts: MediaStreamConstraints[] = [];
        const pushAttempt = (constraints: MediaStreamConstraints) => {
            const key = JSON.stringify(constraints);
            if (!attempts.some((existing) => JSON.stringify(existing) === key)) {
                attempts.push(constraints);
            }
        };

        if (preferred) {
            pushAttempt({ video: camConstraints(preferred, exactPreferred), audio: false });
        }
        pushAttempt({ video: camConstraints(null), audio: false });
        // Firefox on Windows can reject a specific resolution/facingMode combo
        // with "Failed to allocate videosource" even when the camera is usable.
        pushAttempt({
            video: {
                width: { ideal: 320 },
                height: { ideal: 240 },
                frameRate: { ideal: 15, max: 24 }
            },
            audio: false
        });

        try {
            const devices = await navigator.mediaDevices.enumerateDevices();
            for (const device of devices) {
                if (device.kind === 'videoinput' && device.deviceId && device.deviceId !== preferred) {
                    pushAttempt({ video: { deviceId: { exact: device.deviceId } }, audio: false });
                }
            }
        } catch {
            // enumerateDevices can fail before permission in some browsers; the
            // generic attempts below are enough to continue.
        }

        pushAttempt({ video: true, audio: false });

        let lastError: unknown = null;
        // Every rung's failure is recorded and logged as one line if the whole
        // ladder fails: "which constraint set failed, and with what" is the only
        // thing that distinguishes a real block from a busy/absent device, and
        // it is otherwise invisible to anyone debugging from a user's console.
        const failures: string[] = [];
        for (let i = 0; i < attempts.length; i += 1) {
            try {
                const stream = await navigator.mediaDevices.getUserMedia(attempts[i]);
                this.lastCameraError = null;
                return stream;
            } catch (err) {
                lastError = err;
                failures.push(`${JSON.stringify(attempts[i].video)} -> ${(err as { name?: string })?.name ?? 'Error'}: ${(err as { message?: string })?.message ?? ''}`);
                if (preferred) storeCameraDeviceId(null);
                if (isCameraPermissionError(err)) {
                    console.error('Camera capture blocked:', failures.join(' | '));
                    throw err;
                }
                // Camera drivers on Windows/Firefox often need a short beat
                // after a failed allocation before the next constraint set.
                await sleep(i === 0 ? 300 : 150);
            }
        }

        // One last unconstrained try after a longer beat. Firefox does not
        // release a video source synchronously on track.stop(), so a capture
        // opened immediately after another was torn down (green room preview →
        // session auto-start, same tick) fails to allocate on every rung above
        // purely on timing. This does NOT help cross-tab contention — a camera
        // held by another tab stays held.
        await sleep(800);
        try {
            const stream = await navigator.mediaDevices.getUserMedia({ video: true, audio: false });
            this.lastCameraError = null;
            return stream;
        } catch (err) {
            lastError = err;
            failures.push(`retry {video:true} -> ${(err as { name?: string })?.name ?? 'Error'}: ${(err as { message?: string })?.message ?? ''}`);
        }

        console.error('Camera capture failed after all attempts:', failures.join(' | '));
        throw lastError ?? new Error('Camera capture failed');
    }

    // Adopt a freshly-acquired capture as the current camera stream: motion
    // content hint + an onended that tears the cam down if the device vanishes.
    private adoptCameraStream(stream: MediaStream): void {
        this.cameraStream = stream;
        const track = stream.getVideoTracks()[0];
        if (!track) return;
        try {
            track.contentHint = 'motion';
        } catch {
            // older browsers — fine without the hint
        }
        track.onended = () => {
            debugLog('Camera ended (device removed)');
            this.stopWebcam();
            this.options.onWebcamEnded?.();
        };
    }

    // Open (or reuse) the local capture for the self-view preview WITHOUT
    // broadcasting. Returns the stream to bind to the preview <video>, or null
    // if capture failed/was denied. Reuses an already-live capture so the device
    // is never double-acquired (which fails on Firefox: "device in use").
    async openCameraPreview(deviceId?: string | null): Promise<MediaStream | null> {
        if (this.isClosed()) return null;
        if (this.hasLiveVideo(this.cameraStream)) return this.cameraStream;
        if (this.cameraStream) {
            this.stopStream(this.cameraStream);
            this.cameraStream = null;
        }
        // Coalesce concurrent opens (preview + Enable, or a racing switch) onto
        // ONE getUserMedia so the device is never acquired twice at once.
        if (this.cameraOpenPromise) return this.cameraOpenPromise;
        this.cameraOpenPromise = (async () => {
            try {
                const preferred = deviceId ?? getStoredCameraDeviceId();
                const stream = await this.getCameraCapture(preferred);
                if (this.isClosed()) {
                    this.stopStream(stream);
                    return null;
                }
                this.adoptCameraStream(stream);
                return this.cameraStream;
            } catch (err) {
                console.error('Failed to open camera preview:', err);
                this.lastCameraError = err;
                return null;
            } finally {
                this.cameraOpenPromise = null;
            }
        })();
        return this.cameraOpenPromise;
    }

    // Release the preview capture — ONLY when not broadcasting (stopWebcam owns
    // teardown once live). Safe to call on modal dismiss / cleanup.
    cancelCameraPreview(): void {
        if (this.cameraSender) return;
        if (this.cameraStream) {
            this.stopStream(this.cameraStream);
            this.cameraStream = null;
        }
    }

    // Go live: add the already-captured cam track to the publisher and
    // negotiate. Acquires a capture first if none is open (the control-bar /
    // join-with-camera path, which has no preview). Idempotent while
    // broadcasting. NEVER stops a live capture, so it can't kill the self-view.
    async enableCamera(deviceId?: string | null): Promise<boolean> {
        try {
            if (this.isClosed()) return false;
            if (this.cameraSender) return true; // already broadcasting
            if (!this.hasLiveVideo(this.cameraStream)) {
                const opened = await this.openCameraPreview(deviceId);
                if (!opened || this.isClosed() || !this.hasLiveVideo(this.cameraStream)) {
                    return false;
                }
            }
            const videoTrack = this.cameraStream!.getVideoTracks()[0];
            videoTrack.enabled = true;
            // Announce the cam track id BEFORE negotiating so the server maps
            // the incoming video track to the webcam path (not screen share).
            this.sendSignal('webcam:start', { trackId: videoTrack.id });

            const pub = this.ensurePublisher();
            this.cameraSender = pub.addTrack(videoTrack, this.cameraStream!);
            void this.tuneWebcamSender(this.cameraSender);

            const negotiated = await this.negotiatePublisher();
            if (!negotiated) {
                this.rebuildPublisher();
            }
            debugLog('Webcam started');
            return true;
        } catch (err) {
            console.error('Failed to enable camera:', err);
            this.lastCameraError = err;
            // Leave nothing half-live: drop the sender AND the capture so the
            // returned false matches a clean "off" state in both the manager and
            // the UI (no broadcasting-with-Cam-Off desync, no device left lit).
            if (this.cameraSender && this.publisherPc) {
                try {
                    this.publisherPc.removeTrack(this.cameraSender);
                } catch {
                    // already gone
                }
            }
            this.cameraSender = null;
            if (this.cameraStream) {
                this.stopStream(this.cameraStream);
                this.cameraStream = null;
            }
            return false;
        }
    }

    stopWebcam(): void {
        const needsRenegotiation = this.cameraSender != null && this.publisherPc != null;

        if (this.cameraSender && this.publisherPc) {
            try {
                this.publisherPc.removeTrack(this.cameraSender);
            } catch (err) {
                console.error('Failed to remove webcam track:', err);
            }
        }
        this.cameraSender = null;

        if (this.cameraStream) {
            this.stopStream(this.cameraStream);
            this.cameraStream = null;
        }

        // Tell the server to tear down the relay + remove our tile everywhere.
        this.sendSignal('webcam:stop', {});

        if (needsRenegotiation) {
            void this.negotiatePublisher();
        }
        debugLog('Webcam stopped');
    }

    // Switch the camera device. While broadcasting, replaceTrack swaps the
    // sender's track with NO renegotiation — the SDP msid (and the server's
    // track-id mapping) is preserved, so the relay keeps flowing. While only
    // previewing, there's no sender yet. EITHER way the new track is moved into
    // the SAME this.cameraStream object so the bound self-view <video> keeps its
    // srcObject and shows the new device with no black re-attach flash. When no
    // capture is open yet, this just opens a preview on the chosen device.
    async setCameraDevice(deviceId: string): Promise<boolean> {
        try {
            if (this.isClosed()) return false;
            // Not capturing yet: open a preview on the chosen device.
            if (!this.hasLiveVideo(this.cameraStream)) {
                return (await this.openCameraPreview(deviceId)) != null;
            }
            const newStream = await this.getCameraCapture(deviceId, true);
            if (this.isClosed()) {
                this.stopStream(newStream);
                return false;
            }
            const newTrack = newStream.getVideoTracks()[0];
            if (!newTrack) {
                this.stopStream(newStream);
                return false;
            }
            try {
                newTrack.contentHint = 'motion';
            } catch {
                // fine without
            }
            newTrack.onended = () => {
                debugLog('Camera ended (device removed)');
                this.stopWebcam();
                this.options.onWebcamEnded?.();
            };

            const stream = this.cameraStream!;
            const oldTrack = stream.getVideoTracks()[0];

            if (this.cameraSender) {
                // Broadcasting: swap on the sender (no renegotiation).
                try {
                    await this.cameraSender.replaceTrack(newTrack);
                } catch (err) {
                    // Don't leak the freshly-opened device if the swap fails;
                    // keep the previous capture running.
                    console.error('Failed to replace camera track:', err);
                    this.stopStream(newStream);
                    return false;
                }
                if (this.isClosed()) {
                    this.stopStream(newStream);
                    return false;
                }
            }
            // Keep this.cameraStream identity stable: move the new track into it
            // and retire the old one, so the self-view <video> bound to this
            // stream shows the new device without a re-attach.
            stream.addTrack(newTrack);
            if (oldTrack) {
                stream.removeTrack(oldTrack);
                oldTrack.stop();
            }
            debugLog('Switched camera device:', deviceId);
            return true;
        } catch (err) {
            console.error('Failed to switch camera device:', err);
            return false;
        }
    }

    // Tiny presence cam: cap bitrate hard and keep framerate over resolution
    // (faces should stay fluid; the tile is small so detail barely matters).
    private async tuneWebcamSender(sender: RTCRtpSender): Promise<void> {
        try {
            const params = sender.getParameters();
            params.degradationPreference = 'maintain-framerate';
            if (!params.encodings || params.encodings.length === 0) {
                params.encodings = [{}];
            }
            params.encodings[0].maxBitrate = 120_000;
            await sender.setParameters(params);
        } catch (err) {
            console.warn('Could not tune webcam sender parameters:', err);
        }
    }

    // Clean up
    close(): void {
        this.closed = true;
        this.clearPublishAnswerWatchdog();
        this.clearPublisherDisconnectWatchdog();

        if (this.connectionLostTimeout) {
            clearTimeout(this.connectionLostTimeout);
            this.connectionLostTimeout = null;
        }
        if (this.iceRestartTimeout) {
            clearTimeout(this.iceRestartTimeout);
            this.iceRestartTimeout = null;
        }

        if (this.localStream) {
            this.stopStream(this.localStream);
            this.localStream = null;
        }
        // The processed stream above doesn't hold the device — stop the raw
        // capture too so the browser's mic indicator goes away.
        if (this.rawMicStream) {
            this.stopStream(this.rawMicStream);
            this.rawMicStream = null;
        }
        this.disposeMicChain();
        this.audioSender = null;

        if (this.screenShareStream) {
            this.screenShareStream.getTracks().forEach(track => track.stop());
            this.screenShareStream = null;
        }
        this.screenShareSender = null;

        if (this.cameraStream) {
            this.stopStream(this.cameraStream);
            this.cameraStream = null;
        }
        this.cameraSender = null;

        if (this.pc) {
            this.pc.close();
            this.pc = null;
        }

        if (this.publisherPc) {
            try {
                this.publisherPc.close();
            } catch {
                // already closed
            }
            this.publisherPc = null;
            this.publisherOfferSent = false;
            this.publisherOfferId = null;
            this.publisherCandidateOfferId = null;
            this.pendingPublisherCandidates = [];
            this.pendingRemotePublisherCandidates = [];
            this.publisherNeedsRenegotiation = false;
        }
    }
}
