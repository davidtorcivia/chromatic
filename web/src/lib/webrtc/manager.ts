// WebRTC Manager - handles peer connection for receiving stream and sending voice

import { createMicChain, type MicChain } from '$lib/audio/mic-chain';

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

function micConstraints(deviceId?: string | null, exact = false): MediaTrackConstraints {
    const constraints: MediaTrackConstraints = {
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true
    };
    if (deviceId) {
        // `ideal` lets getUserMedia fall back to the default mic when the
        // remembered device was unplugged; `exact` is used for explicit
        // user selection where silently picking another mic would be wrong.
        constraints.deviceId = exact ? { exact: deviceId } : { ideal: deviceId };
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
}

export class WebRTCManager {
    private pc: RTCPeerConnection | null = null;
    private subscriberOfferId: string | null = null;
    private options: WebRTCManagerOptions;
    // localStream holds the stream whose audio track is SENT — the processed
    // output of the mic cleanup chain when available, the raw capture
    // otherwise. rawMicStream always holds the actual device capture (the
    // thing that must be stopped to release the mic / read deviceId from).
    private localStream: MediaStream | null = null;
    private rawMicStream: MediaStream | null = null;
    private micChain: MicChain | null = null;
    private audioSender: RTCRtpSender | null = null;
    private screenShareStream: MediaStream | null = null;
    private screenShareSender: RTCRtpSender | null = null;
    private isMicMuted: boolean = true;
    private iceRestartPending: boolean = false;
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
    // Watchdog: rebuilds the publisher if an offer goes unanswered
    private voiceOfferTimer: ReturnType<typeof setTimeout> | null = null;
    private publisherDisconnectedTimeout: ReturnType<typeof setTimeout> | null = null;
    private static readonly VOICE_OFFER_TIMEOUT_MS = 8000;
    private static readonly RESYNC_MIN_INTERVAL_MS = 250;
    private static readonly DISCONNECTED_ICE_RESTART_MS = 2000;
    private static readonly ICE_RESTART_ANSWER_TIMEOUT_MS = 8000;
    private static readonly PUBLISHER_DISCONNECTED_REBUILD_MS = 2000;
    // Keep browser playout buffers tight for color-review A/B work. This is
    // a best-effort Chrome hint; unsupported browsers ignore it.
    private static readonly LOW_LATENCY_PLAYOUT_DELAY_SECONDS = 0.05;
    private lastResyncAt = 0;
    // Serialize all SDP operations to prevent concurrent modifications
    // to the PeerConnection's signaling state (e.g. handleOffer + handleRenegotiation
    // firing from separate WebSocket messages while awaiting).
    private signalingQueue: Promise<void> = Promise.resolve();

    constructor(options: WebRTCManagerOptions) {
        this.options = options;
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
            console.log('Handling WebRTC offer');

            if (this.pc) {
                const state = this.pc.connectionState;
                const sig = this.pc.signalingState;
                // If we're partway through a fresh handshake (no local description
                // yet), reuse the pc. Otherwise assume this is a reconnect-initiated
                // fresh session and rebuild.
                if (state !== 'new' || sig !== 'stable') {
                    console.log('Resetting stale peer connection before fresh offer', { state, sig });
                    this.resetPeerConnection();
                }
            }

            if (!this.pc) {
                this.createPeerConnection();
            }

            this.subscriberOfferId = offerId ?? null;
            const pc = this.pc!;

            // Set remote description (the offer from server). The subscriber
            // PC is receive-only and the client never offers on it, so no
            // pending-local-offer handling is needed here.
            const offer: RTCSessionDescriptionInit = { type: 'offer', sdp };
            await pc.setRemoteDescription(offer);
            console.log('Set remote description');

            const answer = await pc.createAnswer();
            await pc.setLocalDescription(answer);
            console.log('Created and set local description (answer)');

            const payload: { sdp: string | undefined; offerId?: string } = { sdp: answer.sdp };
            if (offerId) {
                payload.offerId = offerId;
            }
            if (!this.sendSignal('signal:answer', payload)) {
                console.warn('Failed to send WebRTC answer; resetting subscriber connection');
                this.resetPeerConnection();
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
    }

    // Handle ICE candidate from server (if server sends any)
    async handleCandidate(candidate: RTCIceCandidateInit, offerId?: string): Promise<void> {
        return this.enqueueSignaling(async () => {
            if (offerId && this.subscriberOfferId && offerId !== this.subscriberOfferId) {
                console.warn('Ignoring stale ICE candidate for replaced subscriber connection', { offerId });
                return;
            }
            if (!this.pc) {
                console.warn('Received ICE candidate but no peer connection');
                return;
            }

            await this.pc.addIceCandidate(candidate);
            console.log('Added ICE candidate from server');
        });
    }

    private createPeerConnection(): void {
        console.log('Creating peer connection with ICE servers:', this.options.iceServers);

        this.pc = new RTCPeerConnection({
            iceServers: this.options.iceServers
        });

        // Handle incoming tracks
        this.pc.ontrack = (event) => {
            const streamId = event.streams[0]?.id ?? '';
            const trackId = event.track.id;
            console.log('Received track:', event.track.kind, 'trackId:', trackId, 'streamId:', streamId, 'streams:', event.streams.length);
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
                console.log('Identified screen share track from participant:', screenShareParticipantId);
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
                console.log('Identified voice track from participant:', voiceParticipantId);
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
            if (event.candidate) {
                console.log('Sending ICE candidate to server');
                this.sendSignal('signal:candidate', {
                    candidate: event.candidate.candidate,
                    sdpMid: event.candidate.sdpMid,
                    sdpMLineIndex: event.candidate.sdpMLineIndex,
                    offerId: this.subscriberOfferId ?? undefined
                });
            }
        };

        // Handle connection state changes
        this.pc.onconnectionstatechange = () => {
            const state = this.pc?.connectionState;
            console.log('Connection state:', state);

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
                    if (this.pc?.connectionState === 'disconnected') {
                        console.log('Connection still disconnected, attempting ICE restart');
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
                        console.log('Connection failed while ICE restart is being prepared, waiting');
                    }
                } else {
                    // First failure - attempt ICE restart
                    console.log('Connection failed, attempting ICE restart');
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
            }
        };

        this.pc.oniceconnectionstatechange = () => {
            console.log('ICE connection state:', this.pc?.iceConnectionState);
        };

        this.pc.onicegatheringstatechange = () => {
            console.log('ICE gathering state:', this.pc?.iceGatheringState);
        };
    }

    private tuneReceiverForLowLatency(receiver: RTCRtpReceiver): void {
        type LowLatencyReceiver = RTCRtpReceiver & { playoutDelayHint?: number };
        const lowLatencyReceiver = receiver as LowLatencyReceiver;

        if (!('playoutDelayHint' in lowLatencyReceiver)) {
            return;
        }

        try {
            lowLatencyReceiver.playoutDelayHint = WebRTCManager.LOW_LATENCY_PLAYOUT_DELAY_SECONDS;
        } catch (err) {
            console.warn('Could not set low-latency receiver playout hint:', err);
        }
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
        console.log('Performing ICE restart...');
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

            try {
                const offer = await this.pc.createOffer({ iceRestart: true });
                await this.pc.setLocalDescription(offer);

                // Send offer to server for ICE restart
                if (!this.sendSignal('signal:ice-restart', {
                    sdp: offer.sdp
                })) {
                    console.warn('ICE restart offer was not sent; clearing pending restart state');
                    this.resetPeerConnection();
                    return;
                }

                // The restart has genuinely been attempted; a subsequent
                // 'failed' state now means the restart itself failed.
                this.iceRestartAttempted = true;

                console.log('Sent ICE restart offer');
            } catch (err) {
                console.error('Failed to perform ICE restart:', err);
                this.iceRestartPending = false;
                this.iceRestartAttempted = false;
                if (this.iceRestartTimeout) {
                    clearTimeout(this.iceRestartTimeout);
                    this.iceRestartTimeout = null;
                }
            }
        });
    }

    private failIceRestart(message: string): void {
        console.warn(message);
        this.resetPeerConnection();
        this.options.onIceRestartFailed?.();
    }

    private clearIceRestartAttempt(): void {
        if (this.iceRestartTimeout) {
            clearTimeout(this.iceRestartTimeout);
            this.iceRestartTimeout = null;
        }
        this.iceRestartPending = false;
        this.iceRestartAttempted = false;
    }

    // Request a stream resync (forces keyframe from publisher)
    requestResync(): void {
        const now = Date.now();
        if (this.lastResyncAt !== 0 && now - this.lastResyncAt < WebRTCManager.RESYNC_MIN_INTERVAL_MS) {
            return;
        }
        this.lastResyncAt = now;
        console.log('Requesting stream resync (keyframe)');
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
                console.log(`Refreshed ICE servers on live ${label} peer connection`);
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

        const stats = await this.pc.getStats();
        // Prefer the nominated (actively used) candidate pair so the latency
        // display is stable; fall back to any succeeded pair if none is
        // marked nominated.
        let nominatedRtt: number | undefined;
        let fallbackRtt: number | undefined;
        let videoJitterBufferDelay: number | undefined;
        let videoFramesDropped: number | undefined;

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
                if (
                    typeof report.jitterBufferDelay === 'number' &&
                    typeof report.jitterBufferEmittedCount === 'number' &&
                    report.jitterBufferEmittedCount > 0
                ) {
                    videoJitterBufferDelay = (report.jitterBufferDelay / report.jitterBufferEmittedCount) * 1000;
                }
                if (typeof report.framesDropped === 'number') {
                    videoFramesDropped = report.framesDropped;
                }
            }
        });

        return {
            rtt: nominatedRtt ?? fallbackRtt,
            videoJitterBufferDelay,
            videoFramesDropped
        };
    }

    // Request microphone access and prepare for sending. Honors the persisted
    // device preference (chromatic_mic_device) unless an explicit deviceId is
    // passed.
    async requestMicrophone(deviceId?: string | null): Promise<boolean> {
        try {
            const preferred = deviceId ?? getStoredMicDeviceId();
            const raw = await navigator.mediaDevices.getUserMedia({
                audio: micConstraints(preferred),
                video: false
            });

            await this.installMicStream(raw);

            console.log('Microphone access granted');
            return true;
        } catch (err) {
            console.error('Failed to get microphone access:', err);
            return false;
        }
    }

    // Routes a fresh mic capture through the light cleanup chain (high-pass +
    // soft gate) when available, falls back to the raw capture otherwise, and
    // applies the current mute state to whichever stream will be sent.
    private async installMicStream(raw: MediaStream): Promise<void> {
        this.disposeMicChain();
        this.rawMicStream = raw;
        this.micChain = await createMicChain(raw);
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

    // Switch the microphone input device: re-acquire the mic with the given
    // deviceId and swap the new track into the existing RTCRtpSender via
    // replaceTrack — same kind, same m-line, so NO renegotiation is needed.
    async setMicDevice(deviceId: string): Promise<boolean> {
        try {
            const newRaw = await navigator.mediaDevices.getUserMedia({
                audio: micConstraints(deviceId, true),
                video: false
            });

            if (newRaw.getAudioTracks().length === 0) {
                newRaw.getTracks().forEach(t => t.stop());
                return false;
            }

            const oldRaw = this.rawMicStream;
            await this.installMicStream(newRaw);

            const newTrack = this.localStream?.getAudioTracks()[0] ?? null;
            if (this.audioSender && newTrack) {
                await this.audioSender.replaceTrack(newTrack);
            }

            // Release the previous capture only after the swap succeeded so a
            // failed switch leaves the working mic untouched.
            if (oldRaw && oldRaw !== newRaw) {
                oldRaw.getTracks().forEach(t => t.stop());
            }

            console.log('Switched microphone device:', deviceId);
            return true;
        } catch (err) {
            console.error('Failed to switch microphone device:', err);
            return false;
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

        // Publishing rides its own PC — no subscriber connection needed
        if (enabled && this.localStream) {
            this.addLocalAudioTrack().catch(err => {
                console.error('Failed to add local audio track, reverting mic state:', err);
                this.isMicMuted = true;
                if (this.localStream) {
                    this.localStream.getAudioTracks().forEach(t => { t.enabled = false; });
                }
            });
        }

        console.log('Microphone', enabled ? 'enabled' : 'muted');
    }

    // Check if mic is currently enabled
    isMicEnabled(): boolean {
        return !this.isMicMuted;
    }

    // Add local audio track to the publisher connection
    private async addLocalAudioTrack(): Promise<void> {
        if (!this.localStream) return;

        const audioTrack = this.localStream.getAudioTracks()[0];
        if (!audioTrack) return;

        // Check if we already have a sender
        if (this.audioSender) {
            // Replace track
            await this.audioSender.replaceTrack(audioTrack);
        } else {
            // Add new track on the publisher PC and negotiate
            const pub = this.ensurePublisher();
            this.audioSender = pub.addTrack(audioTrack, this.localStream);
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
            console.log('Publisher connection state:', pc.connectionState);
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
                const offerId = `publish-${++this.publisherOfferCounter}`;
                this.publisherOfferId = offerId;
                this.publisherCandidateOfferId = offerId;
                const offer = await pc.createOffer();
                await pc.setLocalDescription(offer);
                if (!this.sendSignal('publish:offer', { sdp: offer.sdp, offerId })) {
                    throw new Error('publisher offer send failed');
                }
                this.publisherOfferSent = true;
                this.flushPendingPublisherCandidates();
                this.startPublishAnswerWatchdog();
                console.log('Sent publisher offer');
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
            console.log('Publisher answer applied');
        });
    }

    // Tear down and recreate the publisher with the current local tracks.
    // Safe at any time: the publisher has no inbound state to lose.
    private rebuildPublisher(): void {
        console.warn('Rebuilding publisher peer connection');
        this.clearPublishAnswerWatchdog();
        this.clearPublisherDisconnectWatchdog();
        const old = this.publisherPc;
        this.publisherPc = null;
        this.publisherOfferSent = false;
        this.publisherOfferId = null;
        this.publisherCandidateOfferId = null;
        this.pendingPublisherCandidates = [];
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
    async handleVoiceAnswer(sdp: string): Promise<void> {
        return this.enqueueSignaling(async () => {
            if (!this.pc) return;

            if (this.pc.signalingState !== 'have-local-offer') {
                console.warn('Ignoring stale answer with no local offer pending', { signalingState: this.pc.signalingState });
                return;
            }

            const answer: RTCSessionDescriptionInit = {
                type: 'answer',
                sdp: sdp
            };

            await this.pc.setRemoteDescription(answer);
            console.log('Set remote description for answer');
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

            console.log('Handling server-initiated renegotiation', participantId ? `for voice from ${participantId}` : '');
            this.options.onRenegotiation?.();

            try {
                if (this.pc.signalingState === 'have-local-offer') {
                    console.log('Rolling back local subscriber offer before server renegotiation');
                    await this.pc.setLocalDescription({ type: 'rollback' });
                    this.clearIceRestartAttempt();
                }

                await this.pc.setRemoteDescription({ type: 'offer', sdp });
                const answer = await this.pc.createAnswer();
                await this.pc.setLocalDescription(answer);

                const payload: { sdp: string | undefined; offerId?: string } = { sdp: answer.sdp };
                if (offerId) {
                    payload.offerId = offerId;
                }

                if (!this.sendSignal('signal:renegotiate-answer', payload)) {
                    console.warn('Failed to send renegotiation answer; resetting subscriber connection');
                    this.resetPeerConnection();
                    return;
                }

                console.log('Sent renegotiation answer');
            } catch (err) {
                console.error('Failed to handle renegotiation:', err);
            }
        });
    }

    // Start screen sharing — captures display and adds video track to peer connection
    // Breadcrumb reporting for the share flow: mirrored to the server log so
    // failures on remote testers' machines are diagnosable without console
    // access (multiple field reports of shares silently not arriving).
    private shareDebug(event: string, detail = ''): void {
        console.log(`[share] ${event}`, detail);
        try {
            this.sendSignal('client:debug', { event: `share:${event}`, detail });
        } catch {
            // never let diagnostics break the share flow
        }
    }

    async startScreenShare(): Promise<boolean> {
        try {
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
                console.log('Screen share ended by user (browser chrome)');
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

            console.log('Screen share started');
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

        console.log('Screen share stopped');
    }

    // Clean up
    close(): void {
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
            this.localStream.getTracks().forEach(track => track.stop());
            this.localStream = null;
        }
        // The processed stream above doesn't hold the device — stop the raw
        // capture too so the browser's mic indicator goes away.
        if (this.rawMicStream) {
            this.rawMicStream.getTracks().forEach(track => track.stop());
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
            this.publisherOfferId = null;
            this.publisherCandidateOfferId = null;
        }
    }
}
