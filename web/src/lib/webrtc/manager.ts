// WebRTC Manager - handles peer connection for receiving stream

export interface WebRTCManagerOptions {
    iceServers: RTCIceServer[];
    onTrack: (event: RTCTrackEvent) => void;
    sendSignal: (type: string, payload: unknown) => void;
}

export class WebRTCManager {
    private pc: RTCPeerConnection | null = null;
    private options: WebRTCManagerOptions;

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

    // Clean up
    close(): void {
        if (this.pc) {
            this.pc.close();
            this.pc = null;
        }
    }
}
