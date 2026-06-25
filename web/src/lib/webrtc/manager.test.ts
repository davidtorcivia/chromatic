import { afterEach, describe, expect, it, vi } from 'vitest';

import { WebRTCManager, type WebRTCManagerOptions } from './manager';

class FakePublisherPeerConnection {
    onicecandidate: ((event: { candidate: RTCIceCandidateInit | null }) => void) | null = null;
    onconnectionstatechange: (() => void) | null = null;
    connectionState: RTCPeerConnectionState = 'new';
    signalingState: RTCSignalingState = 'stable';
    configurationHistory: RTCConfiguration[] = [];

    async createOffer(): Promise<RTCSessionDescriptionInit> {
        return { type: 'offer', sdp: 'publisher-offer' };
    }

    async setLocalDescription(): Promise<void> {
        this.onicecandidate?.({
            candidate: {
                candidate: 'candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host',
                sdpMid: '0',
                sdpMLineIndex: 0
            }
        });
    }

    setConfiguration(configuration: RTCConfiguration): void {
        this.configurationHistory.push(configuration);
    }

    close() {}
}

class FakeSubscriberPeerConnection {
    ontrack: ((event: RTCTrackEvent) => void) | null = null;
    onicecandidate: ((event: { candidate: RTCIceCandidateInit | null }) => void) | null = null;
    onconnectionstatechange: (() => void) | null = null;
    oniceconnectionstatechange: (() => void) | null = null;
    onicegatheringstatechange: (() => void) | null = null;
    connectionState: RTCPeerConnectionState = 'disconnected';
    signalingState: RTCSignalingState = 'stable';
    closed = false;
    addedCandidates: RTCIceCandidateInit[] = [];
    configurationHistory: RTCConfiguration[] = [];
    statsReport = new Map<string, unknown>();

    async createOffer(options?: RTCOfferOptions): Promise<RTCSessionDescriptionInit> {
        return { type: 'offer', sdp: options?.iceRestart ? 'ice-restart-offer' : 'offer' };
    }

    async createAnswer(): Promise<RTCSessionDescriptionInit> {
        return { type: 'answer', sdp: 'subscriber-answer' };
    }

    async setRemoteDescription(description: RTCSessionDescriptionInit): Promise<void> {
        if (description.type === 'offer') {
            this.signalingState = 'have-remote-offer';
            return;
        }
        if (description.type === 'answer') {
            if (this.signalingState !== 'have-local-offer') {
                throw new Error(`cannot apply answer in ${this.signalingState}`);
            }
            this.signalingState = 'stable';
        }
    }

    async setLocalDescription(description?: RTCSessionDescriptionInit): Promise<void> {
        if (!description) return;
        if (description.type === 'offer') {
            this.signalingState = 'have-local-offer';
            return;
        }
        if (description.type === 'answer') {
            this.signalingState = 'stable';
            return;
        }
        if (description.type === 'rollback') {
            this.signalingState = 'stable';
        }
    }

    async addIceCandidate(candidate: RTCIceCandidateInit): Promise<void> {
        this.addedCandidates.push(candidate);
    }

    async getStats(): Promise<Map<string, unknown>> {
        return this.statsReport;
    }

    setConfiguration(configuration: RTCConfiguration): void {
        this.configurationHistory.push(configuration);
    }

    close() {
        this.closed = true;
        this.connectionState = 'closed';
    }
}

function newManager(sendSignal: WebRTCManagerOptions['sendSignal']) {
    return new WebRTCManager({
        iceServers: [],
        onTrack: () => {},
        sendSignal
    });
}

describe('WebRTCManager publisher signaling', () => {
    afterEach(() => {
        vi.useRealTimers();
        vi.unstubAllGlobals();
    });

    it('sends publish:offer before publisher ICE candidates generated during setLocalDescription', async () => {
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = newManager((type, payload) => {
            sent.push({ type, payload });
        });

        const pc = (manager as unknown as { ensurePublisher(): RTCPeerConnection }).ensurePublisher();
        expect(pc).toBeTruthy();

        const negotiated = await (manager as unknown as { negotiatePublisher(): Promise<boolean> }).negotiatePublisher();

        expect(negotiated).toBe(true);
        expect(sent.map((msg) => msg.type)).toEqual(['publish:offer', 'publish:candidate']);
    });

    it('does not flush publisher ICE candidates when the offer send fails', async () => {
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = newManager((type, payload) => {
            sent.push({ type, payload });
            return type !== 'publish:offer';
        });

        const managerInternals = manager as unknown as {
            ensurePublisher(): RTCPeerConnection;
            negotiatePublisher(): Promise<boolean>;
        };
        const pc = managerInternals.ensurePublisher();
        expect(pc).toBeTruthy();

        const negotiated = await managerInternals.negotiatePublisher();

        expect(negotiated).toBe(false);
        expect(sent.map((msg) => msg.type)).toContain('publish:offer');
        expect(sent.map((msg) => msg.type)).not.toContain('publish:candidate');
        expect(managerInternals.ensurePublisher()).not.toBe(pc);
    });
});

describe('WebRTCManager ICE restart recovery', () => {
    afterEach(() => {
        vi.useRealTimers();
        vi.unstubAllGlobals();
    });

    it('escalates when an ICE restart offer receives no answer', async () => {
        vi.useFakeTimers();
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const onIceRestartFailed = vi.fn();
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: (type, payload) => {
                sent.push({ type, payload });
            },
            onIceRestartFailed
        });

        const managerInternals = manager as unknown as {
            createPeerConnection(): void;
            performIceRestart(): Promise<void>;
        };
        managerInternals.createPeerConnection();

        await managerInternals.performIceRestart();

        expect(sent).toEqual([{ type: 'signal:ice-restart', payload: { sdp: 'ice-restart-offer' } }]);
        expect(onIceRestartFailed).not.toHaveBeenCalled();

        vi.advanceTimersByTime(7999);
        expect(onIceRestartFailed).not.toHaveBeenCalled();

        vi.advanceTimersByTime(1);

        expect(onIceRestartFailed).toHaveBeenCalledTimes(1);
    });

    it('starts an ICE restart after a short persistent disconnect', async () => {
        vi.useFakeTimers();
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: (type, payload) => {
                sent.push({ type, payload });
            }
        });

        const managerInternals = manager as unknown as {
            createPeerConnection(): void;
            pc: FakeSubscriberPeerConnection | null;
        };
        managerInternals.createPeerConnection();

        managerInternals.pc?.onconnectionstatechange?.();
        await vi.advanceTimersByTimeAsync(1999);
        expect(sent).toEqual([]);

        await vi.advanceTimersByTimeAsync(1);
        expect(sent).toEqual([{ type: 'signal:ice-restart', payload: { sdp: 'ice-restart-offer' } }]);
    });

    it('rolls back a pending ICE restart when server renegotiation arrives', async () => {
        vi.useFakeTimers();
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const onIceRestartFailed = vi.fn();
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: (type, payload) => {
                sent.push({ type, payload });
            },
            onIceRestartFailed
        });

        const managerInternals = manager as unknown as {
            createPeerConnection(): void;
            performIceRestart(): Promise<void>;
            pc: FakeSubscriberPeerConnection | null;
        };
        managerInternals.createPeerConnection();

        await managerInternals.performIceRestart();
        expect(managerInternals.pc?.signalingState).toBe('have-local-offer');

        await manager.handleRenegotiation('renegotiation-offer', 'speaker-1', 'renegotiate-456');

        expect(managerInternals.pc?.signalingState).toBe('stable');
        expect(sent).toEqual([
            { type: 'signal:ice-restart', payload: { sdp: 'ice-restart-offer' } },
            { type: 'signal:renegotiate-answer', payload: { sdp: 'subscriber-answer', offerId: 'renegotiate-456' } }
        ]);

        vi.advanceTimersByTime(8000);
        expect(onIceRestartFailed).not.toHaveBeenCalled();
    });
});

describe('WebRTCManager subscriber signaling', () => {
    afterEach(() => {
        vi.useRealTimers();
        vi.unstubAllGlobals();
    });

    it('echoes the server offer ID with the subscriber answer', async () => {
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: (type, payload) => {
                sent.push({ type, payload });
            }
        });

        await manager.handleOffer('subscriber-offer', 'offer-123');

        expect(sent).toEqual([{ type: 'signal:answer', payload: { sdp: 'subscriber-answer', offerId: 'offer-123' } }]);
    });

    it('tags subscriber ICE candidates with the current server offer ID', async () => {
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: (type, payload) => {
                sent.push({ type, payload });
            }
        });

        await manager.handleOffer('subscriber-offer', 'offer-123');
        sent.length = 0;

        const pc = (manager as unknown as { pc: FakeSubscriberPeerConnection | null }).pc;
        pc?.onicecandidate?.({
            candidate: {
                candidate: 'candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host',
                sdpMid: '0',
                sdpMLineIndex: 0
            }
        });

        expect(sent).toEqual([
            {
                type: 'signal:candidate',
                payload: {
                    candidate: 'candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host',
                    sdpMid: '0',
                    sdpMLineIndex: 0,
                    offerId: 'offer-123'
                }
            }
        ]);
    });

    it('ignores server ICE candidates from a replaced subscriber offer', async () => {
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: () => {}
        });

        await manager.handleOffer('subscriber-offer', 'offer-123');
        const pc = (manager as unknown as { pc: FakeSubscriberPeerConnection | null }).pc;

        await manager.handleCandidate({
            candidate: 'candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host',
            sdpMid: '0',
            sdpMLineIndex: 0
        }, 'old-offer');

        expect(pc?.addedCandidates).toEqual([]);
    });

    it('echoes the server offer ID with the renegotiation answer', async () => {
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: (type, payload) => {
                sent.push({ type, payload });
            }
        });

        await manager.handleOffer('subscriber-offer', 'offer-123');
        sent.length = 0;

        await manager.handleRenegotiation('renegotiation-offer', 'speaker-1', 'renegotiate-456');

        expect(sent).toEqual([
            { type: 'signal:renegotiate-answer', payload: { sdp: 'subscriber-answer', offerId: 'renegotiate-456' } }
        ]);
    });

    it('sets a low playout delay hint on inbound receivers when supported', () => {
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: () => {}
        });

        const managerInternals = manager as unknown as {
            createPeerConnection(): void;
            pc: FakeSubscriberPeerConnection | null;
        };
        managerInternals.createPeerConnection();

        const receiver = { playoutDelayHint: 1 };
        managerInternals.pc?.ontrack?.({
            track: { kind: 'video', id: 'main-video' },
            streams: [{ id: 'main-stream' }],
            receiver
        } as unknown as RTCTrackEvent);

        expect(receiver.playoutDelayHint).toBe(0.05);
    });

    it('refreshes ICE servers on both subscriber and publisher peer connections', async () => {
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: () => {}
        });

        const managerInternals = manager as unknown as {
            ensurePublisher(): RTCPeerConnection;
            pc: FakeSubscriberPeerConnection | null;
            publisherPc: FakeSubscriberPeerConnection | null;
        };

        await manager.handleOffer('subscriber-offer', 'offer-123');
        managerInternals.ensurePublisher();

        const refreshed = [{ urls: 'turn:turn.example.test', username: 'fresh', credential: 'secret' }];
        manager.updateICEServers(refreshed);

        expect(managerInternals.pc?.configurationHistory).toEqual([{ iceServers: refreshed }]);
        expect(managerInternals.publisherPc?.configurationHistory).toEqual([{ iceServers: refreshed }]);
    });

    it('reports transport RTT and inbound video buffer delay from WebRTC stats', async () => {
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: () => {}
        });

        await manager.handleOffer('subscriber-offer', 'offer-123');
        const pc = (manager as unknown as { pc: FakeSubscriberPeerConnection | null }).pc;
        pc?.statsReport.set('candidate-pair-1', {
            type: 'candidate-pair',
            state: 'succeeded',
            nominated: true,
            currentRoundTripTime: 0.032
        });
        pc?.statsReport.set('inbound-video-1', {
            type: 'inbound-rtp',
            kind: 'video',
            jitterBufferDelay: 0.36,
            jitterBufferEmittedCount: 12,
            framesDropped: 2
        });

        await expect(manager.getStats()).resolves.toEqual({
            rtt: 32,
            videoJitterBufferDelay: 30,
            videoFramesDropped: 2
        });
    });
});

describe('WebRTCManager resync signaling', () => {
    afterEach(() => {
        vi.useRealTimers();
        vi.unstubAllGlobals();
    });

    it('throttles duplicate keyframe requests without delaying the first one', () => {
        vi.useFakeTimers();
        vi.setSystemTime(1_700_000_000_000);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: (type, payload) => {
                sent.push({ type, payload });
            }
        });

        manager.requestResync();
        manager.requestResync();
        vi.advanceTimersByTime(249);
        manager.requestResync();
        vi.advanceTimersByTime(1);
        manager.requestResync();

        expect(sent).toEqual([
            { type: 'signal:resync', payload: {} },
            { type: 'signal:resync', payload: {} }
        ]);
    });
});
