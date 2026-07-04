import { afterEach, describe, expect, it, vi } from 'vitest';

import { WebRTCManager, type WebRTCManagerOptions } from './manager';

class FakePublisherPeerConnection {
    onicecandidate: ((event: { candidate: RTCIceCandidateInit | null }) => void) | null = null;
    onconnectionstatechange: (() => void) | null = null;
    connectionState: RTCPeerConnectionState = 'new';
    signalingState: RTCSignalingState = 'stable';
    configurationHistory: RTCConfiguration[] = [];
    addedTracks: MediaStreamTrack[] = [];
    closed = false;

    async createOffer(): Promise<RTCSessionDescriptionInit> {
        return { type: 'offer', sdp: 'publisher-offer' };
    }

    async setLocalDescription(): Promise<void> {
        this.signalingState = 'have-local-offer';
        this.onicecandidate?.({
            candidate: {
                candidate: 'candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host',
                sdpMid: '0',
                sdpMLineIndex: 0
            }
        });
    }

    async setRemoteDescription(description: RTCSessionDescriptionInit): Promise<void> {
        if (description.type === 'answer') {
            if (this.signalingState !== 'have-local-offer') {
                throw new Error(`cannot apply answer in ${this.signalingState}`);
            }
            this.signalingState = 'stable';
        }
    }

    setConfiguration(configuration: RTCConfiguration): void {
        this.configurationHistory.push(configuration);
    }

    addTrack(track: MediaStreamTrack): RTCRtpSender {
        this.addedTracks.push(track);
        return {} as RTCRtpSender;
    }

    getTransceivers(): RTCRtpTransceiver[] {
        return [];
    }

    close() {
        this.closed = true;
        this.connectionState = 'closed';
    }
}

class FakeShareSender {
    parameters: RTCRtpSendParameters = {
        transactionId: 'fake',
        codecs: [],
        headerExtensions: [],
        rtcp: {},
        encodings: [{}]
    };
    setParametersCalls: RTCRtpSendParameters[] = [];

    getParameters(): RTCRtpSendParameters {
        return this.parameters;
    }

    async setParameters(parameters: RTCRtpSendParameters): Promise<void> {
        this.parameters = parameters;
        this.setParametersCalls.push(structuredClone(parameters));
    }
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
    receivers: RTCRtpReceiver[] = [];
    rejectRemoteOffers = false;

    async createOffer(options?: RTCOfferOptions): Promise<RTCSessionDescriptionInit> {
        return { type: 'offer', sdp: options?.iceRestart ? 'ice-restart-offer' : 'offer' };
    }

    async createAnswer(): Promise<RTCSessionDescriptionInit> {
        return { type: 'answer', sdp: 'subscriber-answer' };
    }

    async setRemoteDescription(description: RTCSessionDescriptionInit): Promise<void> {
        if (description.type === 'offer') {
            if (this.rejectRemoteOffers) {
                throw new Error('remote offer rejected');
            }
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
            this.onicecandidate?.({
                candidate: {
                    candidate: 'candidate:restart 1 udp 2122260223 192.0.2.3 54321 typ host',
                    sdpMid: '0',
                    sdpMLineIndex: 0
                }
            });
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

    getReceivers(): RTCRtpReceiver[] {
        return this.receivers;
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

function deferred<T>() {
    let resolve!: (value: T) => void;
    let reject!: (reason?: unknown) => void;
    const promise = new Promise<T>((res, rej) => {
        resolve = res;
        reject = rej;
    });
    return { promise, resolve, reject };
}

function fakeMediaStream(kind: 'audio' | 'video') {
    const track = {
        kind,
        enabled: true,
        readyState: 'live',
        stop: vi.fn(),
        getSettings: () => ({ deviceId: `${kind}-device` })
    } as unknown as MediaStreamTrack;
    const stream = {
        getTracks: () => [track],
        getAudioTracks: () => (kind === 'audio' ? [track] : []),
        getVideoTracks: () => (kind === 'video' ? [track] : [])
    } as unknown as MediaStream;
    return { stream, track };
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
        expect(sent[0].payload).toEqual({ sdp: 'publisher-offer', offerId: 'publish-1' });
        expect(sent[1].payload).toEqual({
            candidate: 'candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host',
            sdpMid: '0',
            sdpMLineIndex: 0,
            offerId: 'publish-1'
        });
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

    it('clears cameraSender when publisher negotiation fails so the cam is not stuck on', async () => {
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);

        const manager = newManager((type) => type !== 'publish:offer');
        const managerInternals = manager as unknown as {
            ensurePublisher(): RTCPeerConnection;
            negotiatePublisher(): Promise<boolean>;
            cameraSender: RTCRtpSender | null;
        };

        const pc = managerInternals.ensurePublisher();
        expect(pc).toBeTruthy();
        // Simulate a live webcam sender on the publisher PC.
        managerInternals.cameraSender = {} as RTCRtpSender;
        expect(manager.isCameraOn()).toBe(true);

        const negotiated = await managerInternals.negotiatePublisher();

        // The failed negotiation closes the publisher PC; cameraSender must be
        // cleared too, otherwise isCameraOn() reports a dead cam and
        // enableCamera() refuses to restore it (regression guard).
        expect(negotiated).toBe(false);
        expect(manager.isCameraOn()).toBe(false);
        expect(managerInternals.cameraSender).toBeNull();
    });

    it('keeps tagging late publisher ICE candidates after the answer is applied', async () => {
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = newManager((type, payload) => {
            sent.push({ type, payload });
        });

        const pc = (manager as unknown as { ensurePublisher(): FakePublisherPeerConnection }).ensurePublisher();
        await (manager as unknown as { negotiatePublisher(): Promise<boolean> }).negotiatePublisher();
        await manager.handlePublishAnswer('publisher-answer', 'publish-1');
        sent.length = 0;

        pc.onicecandidate?.({
            candidate: {
                candidate: 'candidate:late 1 udp 2122260223 192.0.2.2 54321 typ host',
                sdpMid: '0',
                sdpMLineIndex: 0
            }
        });

        expect(sent).toEqual([
            {
                type: 'publish:candidate',
                payload: {
                    candidate: 'candidate:late 1 udp 2122260223 192.0.2.2 54321 typ host',
                    sdpMid: '0',
                    sdpMLineIndex: 0,
                    offerId: 'publish-1'
                }
            }
        ]);
    });

    it('defers publisher renegotiation while an offer is in flight', async () => {
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = newManager((type, payload) => {
            sent.push({ type, payload });
        });

        const managerInternals = manager as unknown as {
            ensurePublisher(): FakePublisherPeerConnection;
            negotiatePublisher(): Promise<boolean>;
        };
        managerInternals.ensurePublisher();

        await managerInternals.negotiatePublisher();
        expect(sent.map((msg) => msg.type)).toEqual(['publish:offer', 'publish:candidate']);
        expect(sent[0].payload).toEqual({ sdp: 'publisher-offer', offerId: 'publish-1' });

        const deferred = await managerInternals.negotiatePublisher();
        expect(deferred).toBe(true);
        expect(sent).toHaveLength(2);

        await manager.handlePublishAnswer('publisher-answer', 'publish-1');
        await new Promise<void>((resolve) => setTimeout(resolve, 0));

        expect(sent.map((msg) => msg.type)).toEqual([
            'publish:offer',
            'publish:candidate',
            'publish:offer',
            'publish:candidate'
        ]);
        expect(sent[2].payload).toEqual({ sdp: 'publisher-offer', offerId: 'publish-2' });
        expect(sent[3].payload).toEqual({
            candidate: 'candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host',
            sdpMid: '0',
            sdpMLineIndex: 0,
            offerId: 'publish-2'
        });
    });

    it('rebuilds a live publisher after a short persistent disconnect', async () => {
        vi.useFakeTimers();
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = newManager((type, payload) => {
            sent.push({ type, payload });
        });

        const audioTrack = { kind: 'audio', readyState: 'live' } as MediaStreamTrack;
        const stream = {
            getAudioTracks: () => [audioTrack],
            getVideoTracks: () => [],
            getTracks: () => [audioTrack]
        } as unknown as MediaStream;

        const managerInternals = manager as unknown as {
            ensurePublisher(): FakePublisherPeerConnection;
            negotiatePublisher(): Promise<boolean>;
            localStream: MediaStream | null;
            publisherPc: FakePublisherPeerConnection | null;
        };

        managerInternals.localStream = stream;
        const oldPc = managerInternals.ensurePublisher();
        oldPc.addTrack(audioTrack);

        await managerInternals.negotiatePublisher();
        sent.length = 0;

        oldPc.connectionState = 'disconnected';
        oldPc.onconnectionstatechange?.();
        await vi.advanceTimersByTimeAsync(1999);

        expect(sent).toEqual([]);
        expect(managerInternals.publisherPc).toBe(oldPc);

        await vi.advanceTimersByTimeAsync(1);

        expect(oldPc.closed).toBe(true);
        expect(managerInternals.publisherPc).not.toBe(oldPc);
        expect(managerInternals.publisherPc?.addedTracks).toEqual([audioTrack]);
        expect(sent.map((msg) => msg.type)).toEqual(['publish:offer', 'publish:candidate']);
        expect(sent[0].payload).toEqual({ sdp: 'publisher-offer', offerId: 'publish-2' });
        expect(sent[1].payload).toEqual({
            candidate: 'candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host',
            sdpMid: '0',
            sdpMLineIndex: 0,
            offerId: 'publish-2'
        });

        oldPc.onicecandidate?.({
            candidate: {
                candidate: 'candidate:old 1 udp 2122260223 192.0.2.99 54321 typ host',
                sdpMid: '0',
                sdpMLineIndex: 0
            }
        });
        expect(sent).toHaveLength(2);
    });

    it('ignores stale publisher answers from a replaced offer', async () => {
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = newManager((type, payload) => {
            sent.push({ type, payload });
        });

        const managerInternals = manager as unknown as {
            ensurePublisher(): FakePublisherPeerConnection;
            negotiatePublisher(): Promise<boolean>;
            rebuildPublisher(): void;
            localStream: MediaStream | null;
            publisherPc: FakePublisherPeerConnection | null;
        };

        const audioTrack = { kind: 'audio', readyState: 'live' } as MediaStreamTrack;
        managerInternals.localStream = {
            getAudioTracks: () => [audioTrack],
            getVideoTracks: () => [],
            getTracks: () => [audioTrack]
        } as unknown as MediaStream;

        const oldPc = managerInternals.ensurePublisher();
        oldPc.addTrack(audioTrack);
        await managerInternals.negotiatePublisher();
        expect(sent[0].payload).toEqual({ sdp: 'publisher-offer', offerId: 'publish-1' });

        managerInternals.rebuildPublisher();
        await new Promise<void>((resolve) => setTimeout(resolve, 0));
        const replacementPc = managerInternals.publisherPc;
        expect(replacementPc).not.toBe(oldPc);
        expect(sent[sent.length - 2]?.payload).toEqual({ sdp: 'publisher-offer', offerId: 'publish-2' });

        await manager.handlePublishAnswer('stale-answer', 'publish-1');

        expect(replacementPc?.signalingState).toBe('have-local-offer');

        await manager.handlePublishAnswer('fresh-answer', 'publish-2');

        expect(replacementPc?.signalingState).toBe('stable');
    });

    it('retries screen share sender tuning after the publisher answer is applied', async () => {
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);

        const manager = newManager(() => {});
        const shareSender = new FakeShareSender();
        const managerInternals = manager as unknown as {
            ensurePublisher(): FakePublisherPeerConnection;
            negotiatePublisher(): Promise<boolean>;
            screenShareSender: RTCRtpSender | null;
        };

        managerInternals.ensurePublisher();
        managerInternals.screenShareSender = shareSender as unknown as RTCRtpSender;
        await managerInternals.negotiatePublisher();

        await manager.handlePublishAnswer('publisher-answer', 'publish-1');

        expect(shareSender.setParametersCalls).toHaveLength(1);
        expect(shareSender.parameters.degradationPreference).toBe('maintain-resolution');
        expect(shareSender.parameters.encodings[0]?.maxBitrate).toBe(8_000_000);
    });
});

describe('WebRTCManager media capture lifetime', () => {
    afterEach(() => {
        vi.useRealTimers();
        vi.restoreAllMocks();
        vi.unstubAllGlobals();
    });

    it('stops a microphone capture that resolves after the manager is closed', async () => {
        const capture = deferred<MediaStream>();
        const { stream, track } = fakeMediaStream('audio');
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);
        vi.spyOn(navigator.mediaDevices, 'getUserMedia').mockImplementation(() => capture.promise);

        const manager = newManager(() => {});
        const request = manager.requestMicrophone();
        manager.close();
        capture.resolve(stream);

        await expect(request).resolves.toBe(false);
        expect(track.stop).toHaveBeenCalledTimes(1);
        expect(manager.getCurrentMicDeviceId()).toBeNull();
    });

    it('stops a screen-share capture that resolves after the manager is closed', async () => {
        const capture = deferred<MediaStream>();
        const { stream, track } = fakeMediaStream('video');
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);
        Object.defineProperty(navigator.mediaDevices, 'getDisplayMedia', {
            configurable: true,
            value: vi.fn(() => capture.promise)
        });

        const manager = newManager(() => {});
        const request = manager.startScreenShare();
        manager.close();
        capture.resolve(stream);

        await expect(request).resolves.toBe(false);
        expect(track.stop).toHaveBeenCalledTimes(1);
        expect(manager.getScreenShareStream()).toBeNull();
    });

    it('does not create a publisher peer connection when the mic is enabled after close()', () => {
        // Regression: setMicEnabled(true) fires addLocalAudioTrack() without
        // awaiting it. If close() ran in between, ensurePublisher() used to see
        // publisherPc === null and spin up a brand-new orphaned PC — leaking its
        // tracks and sending publish:offer over a dead socket. Now the mic-enable
        // path and ensurePublisher refuse to run after teardown.
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);

        const { stream } = fakeMediaStream('audio');

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = newManager((type, payload) => {
            sent.push({ type, payload });
        });

        const managerInternals = manager as unknown as {
            localStream: MediaStream | null;
            publisherPc: FakePublisherPeerConnection | null;
            ensurePublisher(): FakePublisherPeerConnection;
        };
        managerInternals.localStream = stream;

        // Teardown first — simulates a user toggling the mic on right as they
        // leave the session.
        manager.close();

        // setMicEnabled must be a no-op for publishing (no publisher created,
        // no publish:offer sent). It may still flip the mute flag.
        expect(() => manager.setMicEnabled(true)).not.toThrow();

        // Flush any microtasks the fire-and-forget addLocalAudioTrack scheduled.
        return Promise.resolve().then(() => {
            expect(managerInternals.publisherPc).toBeNull();
            expect(sent.some((m) => m.type === 'publish:offer')).toBe(false);
        });
    });

    it('ensurePublisher throws after close() so late callers cannot leak a PC', () => {
        vi.stubGlobal('RTCPeerConnection', FakePublisherPeerConnection);
        const manager = newManager(() => {});
        manager.close();

        const managerInternals = manager as unknown as {
            ensurePublisher(): FakePublisherPeerConnection;
        };
        expect(() => managerInternals.ensurePublisher()).toThrow(/after close/);
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

        expect(sent).toEqual([
            { type: 'signal:ice-restart', payload: { sdp: 'ice-restart-offer', offerId: 'ice-restart-1' } },
            {
                type: 'signal:candidate',
                payload: {
                    candidate: 'candidate:restart 1 udp 2122260223 192.0.2.3 54321 typ host',
                    sdpMid: '0',
                    sdpMLineIndex: 0,
                    offerId: 'ice-restart-1'
                }
            }
        ]);
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
        expect(sent).toEqual([
            { type: 'signal:ice-restart', payload: { sdp: 'ice-restart-offer', offerId: 'ice-restart-1' } },
            {
                type: 'signal:candidate',
                payload: {
                    candidate: 'candidate:restart 1 udp 2122260223 192.0.2.3 54321 typ host',
                    sdpMid: '0',
                    sdpMLineIndex: 0,
                    offerId: 'ice-restart-1'
                }
            }
        ]);
    });

    it('ignores connection-state events from a superseded subscriber peer connection', async () => {
        // Regression: resetPeerConnection (reconnect / fresh offer / ICE-restart
        // failure) closes the old PC, which can still emit a trailing
        // 'disconnected'/'failed' event asynchronously. Without an identity
        // guard that stale event would spuriously fire an ICE restart — or clear
        // the NEW connection's recovery timers — on the replacement PC.
        vi.useFakeTimers();
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: (type, payload) => {
                sent.push({ type, payload });
                return true;
            }
        });

        const managerInternals = manager as unknown as {
            createPeerConnection(): void;
            resetPeerConnection(): void;
            pc: FakeSubscriberPeerConnection | null;
        };
        managerInternals.createPeerConnection();
        const oldPc = managerInternals.pc!;

        // Simulate a rebuild: the old PC is torn down and a new one takes over.
        managerInternals.resetPeerConnection();
        managerInternals.createPeerConnection();
        const newPc = managerInternals.pc!;

        expect(newPc).not.toBe(oldPc);

        // The stale old PC reports a 'disconnected' state and fires its handler.
        oldPc.connectionState = 'disconnected';
        oldPc.onconnectionstatechange?.();

        // Let the disconnect watchdog window elapse fully.
        await vi.advanceTimersByTimeAsync(3000);

        // No ICE restart must have been requested for the stale event, and the
        // new connection's state must be untouched.
        expect(sent.some((m) => m.type === 'signal:ice-restart')).toBe(false);
        expect(newPc.connectionState).toBe('disconnected');

        // Sanity: the new PC's own event still drives a restart (guard doesn't
        // suppress legitimate recovery).
        sent.length = 0;
        newPc.onconnectionstatechange?.();
        await vi.advanceTimersByTimeAsync(2000);
        expect(sent.some((m) => m.type === 'signal:ice-restart')).toBe(true);
    });

    it('escalates immediately when the ICE restart offer cannot be sent', async () => {
        vi.useFakeTimers();
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const sent: Array<{ type: string; payload: unknown }> = [];
        const onIceRestartFailed = vi.fn();
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: (type, payload) => {
                sent.push({ type, payload });
                return type !== 'signal:ice-restart';
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

        expect(sent).toEqual([
            { type: 'signal:ice-restart', payload: { sdp: 'ice-restart-offer', offerId: 'ice-restart-1' } }
        ]);
        expect(onIceRestartFailed).toHaveBeenCalledTimes(1);
        expect(managerInternals.pc).toBeNull();

        vi.advanceTimersByTime(8000);
        expect(onIceRestartFailed).toHaveBeenCalledTimes(1);
    });

    it('ignores stale ICE restart answers from a replaced restart offer', async () => {
        vi.useFakeTimers();
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: () => {}
        });

        const managerInternals = manager as unknown as {
            createPeerConnection(): void;
            performIceRestart(): Promise<void>;
            pc: FakeSubscriberPeerConnection | null;
        };
        managerInternals.createPeerConnection();

        await managerInternals.performIceRestart();
        expect(managerInternals.pc?.signalingState).toBe('have-local-offer');

        await manager.handleVoiceAnswer('stale-answer', 'ice-restart-0');
        expect(managerInternals.pc?.signalingState).toBe('have-local-offer');

        await manager.handleVoiceAnswer('fresh-answer', 'ice-restart-1');
        expect(managerInternals.pc?.signalingState).toBe('stable');
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
            { type: 'signal:ice-restart', payload: { sdp: 'ice-restart-offer', offerId: 'ice-restart-1' } },
            {
                type: 'signal:candidate',
                payload: {
                    candidate: 'candidate:restart 1 udp 2122260223 192.0.2.3 54321 typ host',
                    sdpMid: '0',
                    sdpMLineIndex: 0,
                    offerId: 'ice-restart-1'
                }
            },
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

    it('resubscribes when a fresh subscriber offer cannot be applied', async () => {
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const onNegotiationWedged = vi.fn();
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: () => {},
            onNegotiationWedged
        });

        const managerInternals = manager as unknown as {
            createPeerConnection(): void;
            pc: FakeSubscriberPeerConnection | null;
        };
        managerInternals.createPeerConnection();
        const pc = managerInternals.pc;
        pc!.connectionState = 'new';
        pc!.rejectRemoteOffers = true;

        await manager.handleOffer('bad-subscriber-offer', 'offer-123');

        expect(pc?.closed).toBe(true);
        expect(managerInternals.pc).toBeNull();
        expect(onNegotiationWedged).toHaveBeenCalledTimes(1);
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

    it('accepts server ICE candidates tagged with the current renegotiation offer ID', async () => {
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: () => {}
        });

        await manager.handleOffer('subscriber-offer', 'offer-123');
        const pc = (manager as unknown as { pc: FakeSubscriberPeerConnection | null }).pc;

        await manager.handleRenegotiation('renegotiation-offer', 'speaker-1', 'renegotiate-456');
        await manager.handleCandidate({
            candidate: 'candidate:old 1 udp 2122260223 192.0.2.1 54321 typ host',
            sdpMid: '0',
            sdpMLineIndex: 0
        }, 'offer-123');
        await manager.handleCandidate({
            candidate: 'candidate:new 1 udp 2122260223 192.0.2.2 54321 typ host',
            sdpMid: '0',
            sdpMLineIndex: 0
        }, 'renegotiate-456');

        expect(pc?.addedCandidates).toEqual([
            {
                candidate: 'candidate:new 1 udp 2122260223 192.0.2.2 54321 typ host',
                sdpMid: '0',
                sdpMLineIndex: 0
            }
        ]);
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

    it('resubscribes when a server renegotiation offer cannot be applied', async () => {
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const onNegotiationWedged = vi.fn();
        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: () => {},
            onNegotiationWedged
        });

        await manager.handleOffer('subscriber-offer', 'offer-123');
        const managerInternals = manager as unknown as { pc: FakeSubscriberPeerConnection | null };
        const pc = managerInternals.pc;
        pc!.rejectRemoteOffers = true;

        await manager.handleRenegotiation('bad-renegotiation-offer', 'speaker-1', 'renegotiate-456');

        expect(pc?.closed).toBe(true);
        expect(managerInternals.pc).toBeNull();
        expect(onNegotiationWedged).toHaveBeenCalledTimes(1);
    });

    it('sets low-latency jitter buffer targets on inbound receivers when supported', () => {
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

        const receiver = { jitterBufferTarget: null, playoutDelayHint: 1 };
        managerInternals.pc?.ontrack?.({
            track: { kind: 'video', id: 'main-video' },
            streams: [{ id: 'main-stream' }],
            receiver
        } as unknown as RTCTrackEvent);

        expect(receiver.jitterBufferTarget).toBe(20);
        expect(receiver.playoutDelayHint).toBe(0.02);
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
        const receiver = {
            track: { kind: 'video' },
            jitterBufferTarget: null,
            playoutDelayHint: 1
        };
        // Receivers are tuned once via the ontrack path (the only place the
        // manager writes latency hints); getStats() must report — not rewrite —
        // them. Drive the receiver through ontrack so the test mirrors reality,
        // and register it with the (fake) PC's receiver list so getReceivers()
        // returns it the way a real browser would after ontrack.
        pc?.receivers.push(receiver as unknown as RTCRtpReceiver);
        pc?.ontrack?.({
            track: { kind: 'video', id: 'main-video' },
            streams: [{ id: 'main-stream' }],
            receiver
        } as unknown as RTCTrackEvent);
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
            videoFramesDropped: 2,
            receiverJitterBufferTarget: 20,
            receiverPlayoutDelayHint: 0.02
        });
        expect(receiver.jitterBufferTarget).toBe(20);
        expect(receiver.playoutDelayHint).toBe(0.02);

        pc?.statsReport.set('inbound-video-1', {
            type: 'inbound-rtp',
            kind: 'video',
            jitterBufferDelay: 0.56,
            jitterBufferEmittedCount: 22,
            framesDropped: 3
        });

        const refreshedStats = await manager.getStats();
        expect(refreshedStats.rtt).toBe(32);
        expect(refreshedStats.videoJitterBufferDelay).toBeCloseTo(20);
        expect(refreshedStats.videoFramesDropped).toBe(3);
        expect(refreshedStats.receiverJitterBufferTarget).toBe(20);
        expect(refreshedStats.receiverPlayoutDelayHint).toBe(0.02);
    });

    it('surfaces a real-time jitter-buffer spike instead of smoothing it to the cumulative average', async () => {
        // Regression: when the cumulative jitterBufferDelay decreased between
        // polls (a browser reporting quirk, or a receiver recycled under the
        // same report id), videoJitterBufferDelayForReport used to fall back to
        // delay/emittedCount — the all-time cumulative average. Over a long
        // session that converges toward the lifetime mean and hides the spike a
        // low-latency review client needs to surface. The fix clamps the
        // negative interval contribution to 0 so the NEXT monotonic sample
        // reports the real per-packet delay.
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: () => {}
        });

        await manager.handleOffer('subscriber-offer', 'offer-123');
        const pc = (manager as unknown as { pc: FakeSubscriberPeerConnection | null }).pc;
        pc?.statsReport.set('inbound-video-1', {
            type: 'inbound-rtp',
            kind: 'video',
            jitterBufferDelay: 5.0,     // long-running session: large cumulative
            jitterBufferEmittedCount: 200
        });
        // First sample: cumulative average is the current average (fresh) = 25ms.
        let stats = await manager.getStats();
        expect(stats.videoJitterBufferDelay).toBeCloseTo(25);

        // Cumulative delay DECREASES while count increases (the quirk). The old
        // code returned 5.0/210*1000 ≈ 23.8ms (smoothed mean, hiding any spike).
        // The fix returns 0 (no positive interval contribution), so a later
        // spike is not averaged away.
        pc?.statsReport.set('inbound-video-1', {
            type: 'inbound-rtp',
            kind: 'video',
            jitterBufferDelay: 4.8,     // decreased
            jitterBufferEmittedCount: 210
        });
        stats = await manager.getStats();
        expect(stats.videoJitterBufferDelay).toBe(0);

        // A subsequent genuine spike must surface at full real-time value, not
        // be dragged back toward the lifetime mean.
        pc?.statsReport.set('inbound-video-1', {
            type: 'inbound-rtp',
            kind: 'video',
            jitterBufferDelay: 5.5,     // monotonic again
            jitterBufferEmittedCount: 215
        });
        stats = await manager.getStats();
        // ((5.5 - 4.8) / (215 - 210)) * 1000 = 140ms — the spike shows.
        expect(stats.videoJitterBufferDelay).toBeCloseTo(140);
    });

    it('reseeds the jitter-buffer estimate when a receiver resets its counters', async () => {
        // When a receiver is recycled under the same report id, emittedCount
        // jumps backwards. The estimate must reseed to the fresh cumulative
        // average (small denominator = current average), not report a stale or
        // nonsensical value.
        vi.stubGlobal('RTCPeerConnection', FakeSubscriberPeerConnection);

        const manager = new WebRTCManager({
            iceServers: [],
            onTrack: () => {},
            sendSignal: () => {}
        });

        await manager.handleOffer('subscriber-offer', 'offer-123');
        const pc = (manager as unknown as { pc: FakeSubscriberPeerConnection | null }).pc;
        pc?.statsReport.set('inbound-video-1', {
            type: 'inbound-rtp',
            kind: 'video',
            jitterBufferDelay: 8.0,
            jitterBufferEmittedCount: 400
        });
        await manager.getStats(); // seed previous = {8.0, 400}

        // Receiver recycled: counters drop to small fresh values.
        pc?.statsReport.set('inbound-video-1', {
            type: 'inbound-rtp',
            kind: 'video',
            jitterBufferDelay: 0.3,
            jitterBufferEmittedCount: 10
        });
        const stats = await manager.getStats();
        // Reseeds to the fresh cumulative average: 0.3/10*1000 = 30ms.
        expect(stats.videoJitterBufferDelay).toBeCloseTo(30);
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
