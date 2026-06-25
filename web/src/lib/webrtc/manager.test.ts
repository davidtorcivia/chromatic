import { afterEach, describe, expect, it, vi } from 'vitest';

import { WebRTCManager, type WebRTCManagerOptions } from './manager';

class FakePublisherPeerConnection {
    onicecandidate: ((event: { candidate: RTCIceCandidateInit | null }) => void) | null = null;
    onconnectionstatechange: (() => void) | null = null;
    connectionState: RTCPeerConnectionState = 'new';
    signalingState: RTCSignalingState = 'stable';

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

    async createOffer(options?: RTCOfferOptions): Promise<RTCSessionDescriptionInit> {
        return { type: 'offer', sdp: options?.iceRestart ? 'ice-restart-offer' : 'offer' };
    }

    async setLocalDescription(): Promise<void> {}

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

        vi.advanceTimersByTime(15000);

        expect(onIceRestartFailed).toHaveBeenCalledTimes(1);
    });
});
