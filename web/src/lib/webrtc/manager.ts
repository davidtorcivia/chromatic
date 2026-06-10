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
    sendSignal: (type: string, payload: unknown) => void;
    onConnectionStateChange?: (state: RTCPeerConnectionState) => void;
    onIceRestart?: () => void;
    onIceRestartFailed?: () => void;
    onRenegotiation?: () => void;
    onScreenShareEnded?: () => void;
}

export class WebRTCManager {
    private pc: RTCPeerConnection | null = null;
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
    // Watchdog: retries voice renegotiation if the offer goes unanswered
    private voiceOfferTimer: ReturnType<typeof setTimeout> | null = null;
    private voiceOfferRetries: number = 0;
    private static readonly VOICE_OFFER_TIMEOUT_MS = 8000;
    private static readonly VOICE_OFFER_MAX_RETRIES = 3;
    // Serialize all SDP operations to prevent concurrent modifications
    // to the PeerConnection's signaling state (e.g. handleOffer + handleRenegotiation
    // firing from separate WebSocket messages while awaiting).
    private signalingQueue: Promise<void> = Promise.resolve();

    constructor(options: WebRTCManagerOptions) {
        this.options = options;
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
    async handleOffer(sdp: string): Promise<void> {
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

            const pc = this.pc!;

            // If we have a pending local offer (voice renegotiation, etc.),
            // rollback to accept the server's offer. Clear watchdog since
            // the voice offer is being abandoned.
            if (pc.signalingState === 'have-local-offer') {
                console.log('Rolling back local offer to accept server offer');
                this.clearVoiceOfferWatchdog();
                await pc.setLocalDescription({ type: 'rollback' });
            }

            // Set remote description (the offer from server)
            const offer: RTCSessionDescriptionInit = { type: 'offer', sdp };
            await pc.setRemoteDescription(offer);
            console.log('Set remote description');

            const answer = await pc.createAnswer();
            await pc.setLocalDescription(answer);
            console.log('Created and set local description (answer)');

            this.options.sendSignal('signal:answer', { sdp: answer.sdp });

            // Re-attach local senders that died with a replaced peer
            // connection (fresh offer after reconnect/resubscribe), then
            // fire-and-forget ONE renegotiation. We must NOT await
            // renegotiate() here because we're already inside
            // enqueueSignaling — awaiting a nested enqueue would deadlock
            // the signaling queue.
            let needsLocalRenegotiation = false;

            if (this.localStream && !this.audioSender && !this.isMicMuted) {
                const audioTrack = this.localStream.getAudioTracks()[0];
                if (audioTrack) {
                    this.audioSender = pc.addTrack(audioTrack, this.localStream);
                    console.log('Added pending mic track after offer');
                    needsLocalRenegotiation = true;
                }
            }

            // An active screen capture survives a PC rebuild (the browser
            // keeps capturing and the UI shows "you're sharing"), but its
            // sender belonged to the old PC — without re-adding it here the
            // share silently stops reaching everyone else.
            if (this.screenShareStream && !this.screenShareSender) {
                const shareTrack = this.screenShareStream.getVideoTracks()[0];
                if (shareTrack && shareTrack.readyState === 'live') {
                    this.screenShareSender = pc.addTrack(shareTrack, this.screenShareStream);
                    console.log('Re-added screen share track after peer connection rebuild');
                    needsLocalRenegotiation = true;
                }
            }

            if (needsLocalRenegotiation) {
                this.renegotiate();
            }
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
        this.clearVoiceOfferWatchdog();
        this.voiceOfferRetries = 0;
        this.iceRestartPending = false;
        this.iceRestartAttempted = false;
        this.audioSender = null;
        // The screen-share sender belonged to the old PC; the capture stream
        // (if any) is left running so the caller can decide whether to re-share.
        this.screenShareSender = null;
        if (this.pc) {
            try {
                this.pc.close();
            } catch {
                // already closed
            }
            this.pc = null;
        }
    }

    // Handle ICE candidate from server (if server sends any)
    async handleCandidate(candidate: RTCIceCandidateInit): Promise<void> {
        return this.enqueueSignaling(async () => {
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
                this.options.sendSignal('signal:candidate', {
                    candidate: event.candidate.candidate,
                    sdpMid: event.candidate.sdpMid,
                    sdpMLineIndex: event.candidate.sdpMLineIndex
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
                }, 5000);
            } else if (state === 'failed') {
                // Don't let the 5s 'disconnected' timer trigger a second
                // restart on top of the one handled here.
                if (this.connectionLostTimeout) {
                    clearTimeout(this.connectionLostTimeout);
                    this.connectionLostTimeout = null;
                }
                if (this.iceRestartPending) {
                    if (this.iceRestartAttempted) {
                        // A restart offer was actually sent and the connection
                        // failed again - genuinely unrecoverable.
                        console.log('ICE restart failed, connection unrecoverable');
                        this.iceRestartPending = false;
                        this.iceRestartAttempted = false;
                        this.options.onIceRestartFailed?.();
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
        // Clear voice watchdog — ICE restart takes priority
        this.clearVoiceOfferWatchdog();
        console.log('Performing ICE restart...');
        this.options.onIceRestart?.();

        if (this.iceRestartTimeout) {
            clearTimeout(this.iceRestartTimeout);
        }
        this.iceRestartTimeout = setTimeout(() => {
            this.iceRestartTimeout = null;
            if (this.iceRestartPending) {
                console.warn('ICE restart answer not received within 15s; clearing pending flag to allow retry');
                this.iceRestartPending = false;
            }
        }, 15000);

        return this.enqueueSignaling(async () => {
            if (!this.pc) return;

            try {
                const offer = await this.pc.createOffer({ iceRestart: true });
                await this.pc.setLocalDescription(offer);

                // Send offer to server for ICE restart
                this.options.sendSignal('signal:ice-restart', {
                    sdp: offer.sdp
                });

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

    // Request a stream resync (forces keyframe from publisher)
    requestResync(): void {
        console.log('Requesting stream resync (keyframe)');
        this.options.sendSignal('signal:resync', {});
    }

    // Apply a fresh set of ICE servers to the live peer connection AND to the
    // stored options (so a subsequent resetPeerConnection rebuilds with the
    // fresh credentials, not the originals).
    //
    // Cloudflare TURN credentials have a default 1 h TTL; long grading
    // sessions outlive that. The active ICE allocation keeps working on
    // its already-authenticated session, but any future ICE restart needs
    // valid creds — without this, an ICE restart on a >1 h session would
    // gather with expired credentials and fail.
    //
    // pc.setConfiguration only affects subsequent gathering; it does not
    // disrupt the running media relay.
    updateICEServers(iceServers: RTCIceServer[]): void {
        this.options = { ...this.options, iceServers };
        if (this.pc) {
            try {
                this.pc.setConfiguration({ iceServers });
                console.log('Refreshed ICE servers on live peer connection');
            } catch (err) {
                console.warn('setConfiguration with fresh ICE servers failed:', err);
            }
        }
    }

    // Get current connection state
    getConnectionState(): RTCPeerConnectionState | null {
        return this.pc?.connectionState ?? null;
    }

    // Get stats for latency display
    async getStats(): Promise<{ rtt?: number }> {
        if (!this.pc) {
            return {};
        }

        const stats = await this.pc.getStats();
        // Prefer the nominated (actively used) candidate pair so the latency
        // display is stable; fall back to any succeeded pair if none is
        // marked nominated.
        let nominatedRtt: number | undefined;
        let fallbackRtt: number | undefined;

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
        });

        return { rtt: nominatedRtt ?? fallbackRtt };
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

        // If we have a peer connection and local stream, add/update track
        if (enabled && this.pc && this.localStream) {
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

    // Add local audio track to peer connection
    private async addLocalAudioTrack(): Promise<void> {
        if (!this.pc || !this.localStream) return;

        const audioTrack = this.localStream.getAudioTracks()[0];
        if (!audioTrack) return;

        // Check if we already have a sender
        if (this.audioSender) {
            // Replace track
            await this.audioSender.replaceTrack(audioTrack);
        } else {
            // Add new track
            this.audioSender = this.pc.addTrack(audioTrack, this.localStream);

            // Need to renegotiate - create and send offer
            await this.renegotiate();
        }
    }

    // Renegotiate the connection after adding tracks.
    // Starts a watchdog timer that retries if the offer goes unanswered.
    private async renegotiate(): Promise<void> {
        return this.enqueueSignaling(async () => {
            if (!this.pc) return;

            try {
                const offer = await this.pc.createOffer();
                await this.pc.setLocalDescription(offer);

                // Send offer to server
                this.options.sendSignal('signal:offer', {
                    sdp: offer.sdp
                });

                // Start watchdog: if we're still in have-local-offer after
                // the timeout, the server never answered — retry.
                this.startVoiceOfferWatchdog();

                console.log('Sent renegotiation offer for voice');
            } catch (err) {
                console.error('Failed to renegotiate:', err);
            }
        });
    }

    // Start (or restart) the watchdog timer for unanswered voice offers
    private startVoiceOfferWatchdog(): void {
        this.clearVoiceOfferWatchdog();
        this.voiceOfferTimer = setTimeout(() => {
            if (!this.pc) return;
            if (this.pc.signalingState !== 'have-local-offer') return;
            // Don't interfere with ICE restart offers
            if (this.iceRestartPending) return;
            if (this.voiceOfferRetries >= WebRTCManager.VOICE_OFFER_MAX_RETRIES) {
                console.error('Voice offer unanswered after max retries, rolling back stale offer');
                this.voiceOfferRetries = 0;
                // Rollback so PC isn't stuck in have-local-offer
                this.enqueueSignaling(async () => {
                    if (this.pc?.signalingState === 'have-local-offer' && !this.iceRestartPending) {
                        await this.pc.setLocalDescription({ type: 'rollback' });
                    }
                });
                return;
            }
            this.voiceOfferRetries++;
            console.warn(`Voice offer unanswered after ${WebRTCManager.VOICE_OFFER_TIMEOUT_MS}ms, retrying (${this.voiceOfferRetries}/${WebRTCManager.VOICE_OFFER_MAX_RETRIES})`);
            // Rollback the stale offer and re-send
            this.enqueueSignaling(async () => {
                if (!this.pc || this.pc.signalingState !== 'have-local-offer') return;
                if (this.iceRestartPending) return;
                await this.pc.setLocalDescription({ type: 'rollback' });
                const offer = await this.pc.createOffer();
                await this.pc.setLocalDescription(offer);
                this.options.sendSignal('signal:offer', { sdp: offer.sdp });
                this.startVoiceOfferWatchdog();
                console.log('Re-sent voice offer');
            });
        }, WebRTCManager.VOICE_OFFER_TIMEOUT_MS);
    }

    private clearVoiceOfferWatchdog(): void {
        if (this.voiceOfferTimer) {
            clearTimeout(this.voiceOfferTimer);
            this.voiceOfferTimer = null;
        }
    }

    // Handle answer for voice renegotiation
    async handleVoiceAnswer(sdp: string): Promise<void> {
        return this.enqueueSignaling(async () => {
            if (!this.pc) return;

            const answer: RTCSessionDescriptionInit = {
                type: 'answer',
                sdp: sdp
            };

            await this.pc.setRemoteDescription(answer);
            // Answer received — clear the watchdog and reset retries
            this.clearVoiceOfferWatchdog();
            this.voiceOfferRetries = 0;
            console.log('Set remote description for voice answer');
        });
    }

    // Handle server-initiated renegotiation (e.g., when voice tracks are added)
    async handleRenegotiation(sdp: string, participantId?: string): Promise<void> {
        return this.enqueueSignaling(async () => {
            if (!this.pc) {
                console.warn('Received renegotiation but no peer connection');
                return;
            }

            console.log('Handling server-initiated renegotiation', participantId ? `for voice from ${participantId}` : '');
            this.options.onRenegotiation?.();

            let rolledBack = false;

            try {
                // If we have a pending local offer (client-initiated renegotiation),
                // rollback to accept the server's offer instead. The server is the
                // "impolite" peer in our signaling model.
                if (this.pc.signalingState === 'have-local-offer') {
                    console.log('Rolling back local offer to accept server renegotiation');
                    this.clearVoiceOfferWatchdog();
                    await this.pc.setLocalDescription({ type: 'rollback' });
                    rolledBack = true;
                }

                // Set the new offer from server
                const offer: RTCSessionDescriptionInit = {
                    type: 'offer',
                    sdp: sdp
                };
                await this.pc.setRemoteDescription(offer);

                // If we have a pending mic stream, add the track before creating
                // the answer so the answer includes the mic media section.
                if (this.localStream && !this.audioSender && !this.isMicMuted) {
                    const audioTrack = this.localStream.getAudioTracks()[0];
                    if (audioTrack) {
                        this.audioSender = this.pc.addTrack(audioTrack, this.localStream);
                        console.log('Added mic track during renegotiation answer');
                    }
                }

                // Create answer
                const answer = await this.pc.createAnswer();
                await this.pc.setLocalDescription(answer);

                // Send answer back to server
                this.options.sendSignal('signal:renegotiate-answer', {
                    sdp: answer.sdp
                });

                console.log('Sent renegotiation answer');

                // If we rolled back a client-initiated offer, any locally-added
                // tracks (mic, screen share) still exist on the PC but were NOT
                // included in the answer (createAnswer can only match the server's
                // offer m-lines). Re-trigger a client offer so those tracks get
                // properly negotiated.
                if (rolledBack && (this.audioSender || this.screenShareSender)) {
                    console.log('Re-offering after rollback to negotiate local tracks');
                    this.renegotiate();
                }
            } catch (err) {
                console.error('Failed to handle renegotiation:', err);
            }
        });
    }

    // Start screen sharing — captures display and adds video track to peer connection
    async startScreenShare(): Promise<boolean> {
        if (!this.pc) return false;

        try {
            this.screenShareStream = await navigator.mediaDevices.getDisplayMedia({
                video: true,
                audio: false
            });

            const videoTrack = this.screenShareStream.getVideoTracks()[0];
            if (!videoTrack) {
                this.screenShareStream = null;
                return false;
            }

            // Listen for browser "Stop sharing" button
            videoTrack.onended = () => {
                console.log('Screen share ended by user (browser chrome)');
                this.stopScreenShare();
                this.options.onScreenShareEnded?.();
            };

            // Add track to peer connection and renegotiate
            this.screenShareSender = this.pc.addTrack(videoTrack, this.screenShareStream);
            await this.renegotiate();

            console.log('Screen share started');
            return true;
        } catch (err) {
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
        const needsRenegotiation = this.screenShareSender != null && this.pc != null;

        if (this.screenShareSender && this.pc) {
            try {
                this.pc.removeTrack(this.screenShareSender);
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
            this.renegotiate();
        }

        console.log('Screen share stopped');
    }

    // Clean up
    close(): void {
        this.clearVoiceOfferWatchdog();

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
    }
}
