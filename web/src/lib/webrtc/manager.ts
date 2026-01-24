// WebRTC Manager - handles peer connection for receiving stream and sending voice

export interface WebRTCManagerOptions {
    iceServers: RTCIceServer[];
    onTrack: (event: RTCTrackEvent) => void;
    onVoiceTrack?: (participantId: string, track: MediaStreamTrack) => void;
    sendSignal: (type: string, payload: unknown) => void;
}

export class WebRTCManager {
    private pc: RTCPeerConnection | null = null;
    private options: WebRTCManagerOptions;
    private localStream: MediaStream | null = null;
    private audioSender: RTCRtpSender | null = null;
    private isMicMuted: boolean = true;

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
            console.log('Received track:', event.track.kind);
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
            console.log('Connection state:', this.pc?.connectionState);
        };

        this.pc.oniceconnectionstatechange = () => {
            console.log('ICE connection state:', this.pc?.iceConnectionState);
        };

        this.pc.onicegatheringstatechange = () => {
            console.log('ICE gathering state:', this.pc?.iceGatheringState);
        };
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

            // Start muted by default
            this.localStream.getAudioTracks().forEach(track => {
                track.enabled = false;
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

    // Clean up
    close(): void {
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
