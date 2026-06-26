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
import { applyOpusPreferences } from './sdp';

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

export interface MicConstraintOptions {
    deviceId?: string | null;
    exact?: boolean;
    mode: AudioMode;
    studioHeadphones: boolean;
    // True when an in-app denoiser will actually run; we then disable the
    // browser's native noiseSuppression so the two don't stack (double NS
    // compounds the "musical noise" artifacts).
    inAppDenoise: boolean;
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
        // Studio: never suppress/AGC. Talkback: suppress natively only when our
        // own denoiser isn't carrying that load; AGC stays native for level
        // consistency (we add no gain stage in talkback, so it can't pump).
        noiseSuppression: studio ? false : !opts.inAppDenoise,
        autoGainControl: studio ? false : true
    };
    if (echoCancellation) {
        // Prefer the OS echo canceller where available; falls back to the
        // browser one otherwise.
        constraints.echoCancellationType = { ideal: 'system' };
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
    sendSignal: (type: string, payload: unknown) => boolean | void;
    onConnectionStateChange?: (state: RTCPeerConnectionState) => void;
    onIceRestart?: () => void;
    onIceRestartFailed?: () => void;
    onRenegotiation?: () => void;
    onScreenShareEnded?: () => void;
    /** Local renegotiation failed repeatedly — the page should rebuild the
     *  subscription (local tracks re-attach on the fresh offer). */
    onNegotiationWedged?: () => void;
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
    private subscriberOfferId: string | null = null;
    private subscriberCandidateOfferId: string | null = null;
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

            this.subscriberOfferId = offerId ?? null;
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
                await pc.setLocalDescription(answer);
                debugLog('Created and set local description (answer)');

                const payload: { sdp: string | undefined; offerId?: string } = { sdp: answer.sdp };
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
        this.subscriberOfferId = null;
        this.subscriberCandidateOfferId = null;
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

        // Handle incoming tracks
        this.pc.ontrack = (event) => {
            const streamId = event.streams[0]?.id ?? '';
            const trackId = event.track.id;
            debugLog('Received track:', event.track.kind, 'trackId:', trackId, 'streamId:', streamId, 'streams:', event.streams.length);
            this.tuneReceiverForLowLatency(event.receiver);

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

        for (let attempt = 0; attempt < 2; attempt++) {
            const inAppDenoise = this.wantsInAppDenoise() && attempt === 0;
            let raw: MediaStream;
            try {
                raw = await navigator.mediaDevices.getUserMedia({
                    audio: micConstraints({
                        deviceId,
                        exact,
                        mode: this.audioMode,
                        studioHeadphones: this.studioHeadphones,
                        inAppDenoise
                    }),
                    video: false
                });
            } catch (err) {
                console.error('Failed to get microphone access:', err);
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

            await this.installMicStream(raw);
            if (this.isClosed()) {
                return false;
            }

            // Talkback wanted an in-app denoiser but it didn't engage — retry
            // once with native noise suppression rather than shipping raw noisy
            // speech. (rawMicStream now points at this attempt's capture.)
            if (inAppDenoise && !(this.micChain?.denoiserActive ?? false)) {
                console.warn('In-app denoiser inactive; re-acquiring mic with native noise suppression');
                this.disposeMicChain();
                this.stopStream(raw);
                this.rawMicStream = null;
                this.localStream = null;
                continue;
            }

            const newTrack = this.localStream?.getAudioTracks()[0] ?? null;
            if (this.audioSender && newTrack) {
                await this.audioSender.replaceTrack(newTrack);
                if (this.isClosed()) return false;
            }

            // Release the previous capture only after the swap succeeded so a
            // failed switch leaves the working mic untouched.
            if (previousRaw && previousRaw !== raw) {
                this.stopStream(previousRaw);
            }
            return true;
        }
        return false;
    }

    // Routes a fresh mic capture through the mode-aware cleanup chain (high-pass
    // + optional denoiser + soft gate in talkback; bypassed entirely in studio),
    // falls back to the raw capture on any failure, and applies the current mute
    // state to whichever stream will be sent.
    private async installMicStream(raw: MediaStream): Promise<void> {
        this.disposeMicChain();
        this.rawMicStream = raw;
        this.micChain = await createMicChain(raw, {
            mode: this.audioMode,
            denoiser: this.denoiserEngine
        });
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
        await this.acquireMic(deviceId, false);
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
                }
                return false;
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
        this.publisherNeedsRenegotiation = false;
        this.audioSender = null;
        this.screenShareSender = null;
        try {
            old?.close();
        } catch {
            // already closed
        }

        const audioTrack = this.localStream?.getAudioTracks()[0];
        const shareTrack = this.screenShareStream?.getVideoTracks()[0];
        if (!audioTrack && !(shareTrack && shareTrack.readyState === 'live')) {
            return; // nothing to publish; next mic/share enable recreates it
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

            try {
                if (this.pc.signalingState === 'have-local-offer') {
                    debugLog('Rolling back local subscriber offer before server renegotiation');
                    await this.pc.setLocalDescription({ type: 'rollback' });
                    this.clearIceRestartAttempt();
                }

                this.subscriberCandidateOfferId = offerId ?? null;
                await this.pc.setRemoteDescription({ type: 'offer', sdp });
                const answer = await this.pc.createAnswer();
                await this.pc.setLocalDescription(answer);

                const payload: { sdp: string | undefined; offerId?: string } = { sdp: answer.sdp };
                if (offerId) {
                    payload.offerId = offerId;
                }

                if (!this.sendSignal('signal:renegotiate-answer', payload)) {
                    this.failSubscriberNegotiation('Failed to send renegotiation answer; resetting subscriber connection');
                    return;
                }

                debugLog('Sent renegotiation answer');
            } catch (err) {
                this.subscriberCandidateOfferId = previousCandidateOfferId;
                this.failSubscriberNegotiation('Failed to handle renegotiation; resetting subscriber connection', err);
            }
        });
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
            this.publisherNeedsRenegotiation = false;
        }
    }
}
