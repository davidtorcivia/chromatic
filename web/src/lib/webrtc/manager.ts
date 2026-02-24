// WebRTC Manager - handles peer connection for receiving stream and sending voice

export interface WebRTCManagerOptions {
    iceServers: RTCIceServer[];
    onTrack: (event: RTCTrackEvent) => void;
    onVoiceTrack?: (participantId: string, track: MediaStreamTrack) => void;
    sendSignal: (type: string, payload: unknown) => void;
    onConnectionStateChange?: (state: RTCPeerConnectionState) => void;
    onIceRestart?: () => void;
    onIceRestartFailed?: () => void;
    onRenegotiation?: () => void;
}

export class WebRTCManager {
    private pc: RTCPeerConnection | null = null;
    private options: WebRTCManagerOptions;
    private localStream: MediaStream | null = null;
    private audioSender: RTCRtpSender | null = null;
    private isMicMuted: boolean = true;
    private iceRestartPending: boolean = false;
    private connectionLostTimeout: ReturnType<typeof setTimeout> | null = null;

    constructor(options: WebRTCManagerOptions) {
        this.options = options;
    }

    // Handle incoming SDP offer from server
    async handleOffer(sdp: string): Promise<void> {
        console.log('Handling WebRTC offer');

        // Create peer connection if not exists
        if (!this.pc) {
            this.createPeerConnection();
        }

        const pc = this.pc!;

        // Set remote description (the offer from server)
        const offer: RTCSessionDescriptionInit = {
            type: 'offer',
            sdp: sdp
        };

        await pc.setRemoteDescription(offer);
        console.log('Set remote description');

        // Create answer
        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        console.log('Created and set local description (answer)');

        // Send answer back to server
        this.options.sendSignal('signal:answer', {
            sdp: answer.sdp
        });

        // If we have a pending mic stream and mic is enabled, add it now
        if (this.localStream && !this.audioSender && !this.isMicMuted) {
            console.log('Adding pending audio track after offer');
            await this.addLocalAudioTrack();
        }
    }

    // Handle ICE candidate from server (if server sends any)
    async handleCandidate(candidate: RTCIceCandidateInit): Promise<void> {
        if (!this.pc) {
            console.warn('Received ICE candidate but no peer connection');
            return;
        }

        await this.pc.addIceCandidate(candidate);
        console.log('Added ICE candidate from server');
    }

    private createPeerConnection(): void {
        console.log('Creating peer connection with ICE servers:', this.options.iceServers);

        this.pc = new RTCPeerConnection({
            iceServers: this.options.iceServers
        });

        // Handle incoming tracks
        this.pc.ontrack = (event) => {
            console.log('Received track:', event.track.kind, 'id:', event.track.id, 'streams:', event.streams.length);

            // Check if this is a voice track from another participant
            // Server creates voice tracks with ID format: "voice-{participantId}"
            const trackId = event.track.id;
            if (trackId.startsWith('voice-')) {
                const participantId = trackId.substring(6); // Remove "voice-" prefix
                console.log('Identified voice track from participant:', participantId);

                // Call the voice track callback if provided
                if (this.options.onVoiceTrack) {
                    this.options.onVoiceTrack(participantId, event.track);
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
                // Give it some time to recover before triggering ICE restart
                this.connectionLostTimeout = setTimeout(() => {
                    if (this.pc?.connectionState === 'disconnected') {
                        console.log('Connection still disconnected, attempting ICE restart');
                        this.performIceRestart();
                    }
                }, 5000);
            } else if (state === 'failed') {
                if (this.iceRestartPending) {
                    // ICE restart was already attempted but failed
                    console.log('ICE restart failed, connection unrecoverable');
                    this.iceRestartPending = false;
                    this.options.onIceRestartFailed?.();
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
                this.iceRestartPending = false;
            }
        };

        this.pc.oniceconnectionstatechange = () => {
            console.log('ICE connection state:', this.pc?.iceConnectionState);
        };

        this.pc.onicegatheringstatechange = () => {
            console.log('ICE gathering state:', this.pc?.iceGatheringState);
        };
    }

    // Perform ICE restart to recover from connection issues
    async performIceRestart(): Promise<void> {
        if (!this.pc || this.iceRestartPending) {
            return;
        }

        this.iceRestartPending = true;
        console.log('Performing ICE restart...');
        this.options.onIceRestart?.();

        try {
            const offer = await this.pc.createOffer({ iceRestart: true });
            await this.pc.setLocalDescription(offer);

            // Send offer to server for ICE restart
            this.options.sendSignal('signal:ice-restart', {
                sdp: offer.sdp
            });

            console.log('Sent ICE restart offer');
        } catch (err) {
            console.error('Failed to perform ICE restart:', err);
            this.iceRestartPending = false;
        }
    }

    // Request a stream resync (forces keyframe from publisher)
    requestResync(): void {
        console.log('Requesting stream resync (keyframe)');
        this.options.sendSignal('signal:resync', {});
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
        let rtt: number | undefined;

        stats.forEach(report => {
            if (report.type === 'candidate-pair' && report.state === 'succeeded') {
                rtt = report.currentRoundTripTime * 1000; // Convert to ms
            }
        });

        return { rtt };
    }

    // Request microphone access and prepare for sending
    async requestMicrophone(): Promise<boolean> {
        try {
            this.localStream = await navigator.mediaDevices.getUserMedia({
                audio: {
                    echoCancellation: true,
                    noiseSuppression: true,
                    autoGainControl: true
                },
                video: false
            });

            // Respect current mic mute state so permission can be requested
            // before we begin sending audio.
            this.localStream.getAudioTracks().forEach(track => {
                track.enabled = !this.isMicMuted;
            });

            console.log('Microphone access granted');
            return true;
        } catch (err) {
            console.error('Failed to get microphone access:', err);
            return false;
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
            this.addLocalAudioTrack();
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

    // Renegotiate the connection after adding tracks
    private async renegotiate(): Promise<void> {
        if (!this.pc) return;

        try {
            const offer = await this.pc.createOffer();
            await this.pc.setLocalDescription(offer);

            // Send offer to server
            this.options.sendSignal('signal:offer', {
                sdp: offer.sdp
            });

            console.log('Sent renegotiation offer for voice');
        } catch (err) {
            console.error('Failed to renegotiate:', err);
        }
    }

    // Handle answer for voice renegotiation
    async handleVoiceAnswer(sdp: string): Promise<void> {
        if (!this.pc) return;

        const answer: RTCSessionDescriptionInit = {
            type: 'answer',
            sdp: sdp
        };

        await this.pc.setRemoteDescription(answer);
        console.log('Set remote description for voice answer');
    }

    // Handle server-initiated renegotiation (e.g., when voice tracks are added)
    async handleRenegotiation(sdp: string, participantId?: string): Promise<void> {
        if (!this.pc) {
            console.warn('Received renegotiation but no peer connection');
            return;
        }

        console.log('Handling server-initiated renegotiation', participantId ? `for voice from ${participantId}` : '');
        this.options.onRenegotiation?.();

        try {
            // Set the new offer from server
            const offer: RTCSessionDescriptionInit = {
                type: 'offer',
                sdp: sdp
            };
            await this.pc.setRemoteDescription(offer);

            // Create answer
            const answer = await this.pc.createAnswer();
            await this.pc.setLocalDescription(answer);

            // Send answer back to server
            this.options.sendSignal('signal:renegotiate-answer', {
                sdp: answer.sdp
            });

            console.log('Sent renegotiation answer');
        } catch (err) {
            console.error('Failed to handle renegotiation:', err);
        }
    }

    // Clean up
    close(): void {
        if (this.connectionLostTimeout) {
            clearTimeout(this.connectionLostTimeout);
            this.connectionLostTimeout = null;
        }

        if (this.localStream) {
            this.localStream.getTracks().forEach(track => track.stop());
            this.localStream = null;
        }
        this.audioSender = null;

        if (this.pc) {
            this.pc.close();
            this.pc = null;
        }
    }
}
