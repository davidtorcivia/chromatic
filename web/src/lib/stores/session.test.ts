import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createSessionStore, getReconnectDelay } from './session.svelte';

describe('getReconnectDelay backoff', () => {
    afterEach(() => vi.restoreAllMocks());

    it('grows exponentially from ~250ms and caps at 30s (jitter neutralized)', () => {
        // random()=0.5 makes the jitter factor exactly 1.0, exposing the raw
        // exponential curve: 250 * 2^attempt, capped at 30000.
        vi.spyOn(Math, 'random').mockReturnValue(0.5);
        expect(getReconnectDelay(0)).toBe(250);
        expect(getReconnectDelay(1)).toBe(500);
        expect(getReconnectDelay(2)).toBe(1000);
        expect(getReconnectDelay(6)).toBe(16000);
        // 250 * 2^7 = 32000 > 30000 → capped.
        expect(getReconnectDelay(7)).toBe(30000);
        expect(getReconnectDelay(20)).toBe(30000);
    });

    it('applies ±20% jitter around the capped delay', () => {
        const cap = 30000;
        vi.spyOn(Math, 'random').mockReturnValue(0); // factor 0.8 → lower bound
        expect(getReconnectDelay(50)).toBe(Math.round(cap * 0.8));
        vi.restoreAllMocks();
        vi.spyOn(Math, 'random').mockReturnValue(1); // factor 1.2 → upper bound
        expect(getReconnectDelay(50)).toBe(Math.round(cap * 1.2));
    });

    it('keeps every attempt within its ±20% jitter window across the real RNG', () => {
        for (let attempt = 0; attempt <= 12; attempt++) {
            const base = Math.min(250 * 2 ** attempt, 30000);
            for (let i = 0; i < 200; i++) {
                const d = getReconnectDelay(attempt);
                expect(d).toBeGreaterThanOrEqual(Math.round(base * 0.8) - 1);
                expect(d).toBeLessThanOrEqual(Math.round(base * 1.2) + 1);
            }
        }
    });
});

class FakeWebSocket {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSING = 2;
    static CLOSED = 3;

    static instances: FakeWebSocket[] = [];

    readyState = FakeWebSocket.CONNECTING;
    bufferedAmount = 0;
    sent: string[] = [];
    closeCode: number | undefined;
    closeReason: string | undefined;
    onopen: (() => void) | null = null;
    onclose: ((event: { code: number; reason: string }) => void) | null = null;
    onmessage: ((event: { data: string }) => void) | null = null;
    onerror: ((event: unknown) => void) | null = null;

    constructor(public url: string) {
        FakeWebSocket.instances.push(this);
    }

    open() {
        this.readyState = FakeWebSocket.OPEN;
        this.onopen?.();
    }

    send(data: string) {
        this.sent.push(data);
    }

    close(code = 1000, reason = '') {
        this.readyState = FakeWebSocket.CLOSED;
        this.closeCode = code;
        this.closeReason = reason;
        this.onclose?.({ code, reason });
    }
}

describe('SessionStore WebSocket sends', () => {
    let online = true;

    beforeEach(() => {
        online = true;
        FakeWebSocket.instances = [];
        vi.stubGlobal('WebSocket', FakeWebSocket);
        Object.defineProperty(window.navigator, 'onLine', {
            configurable: true,
            get: () => online
        });
    });

    afterEach(() => {
        vi.useRealTimers();
        vi.unstubAllGlobals();
    });

    function connectedStore() {
        const store = createSessionStore();
        store.connect('review-room', 'Viewer One');
        const socket = FakeWebSocket.instances.at(-1);
        if (!socket) throw new Error('WebSocket was not created');
        socket.open();
        return { store, socket };
    }

    it('drops disposable cursor messages when the browser send buffer is backed up', () => {
        const { store, socket } = connectedStore();
        socket.bufferedAmount = 20 * 1024;

        expect(store.send('cursor', { points: [{ x: 0.5, y: 0.5 }], active: true })).toBe(false);

        expect(socket.sent).toHaveLength(0);
        expect(socket.readyState).toBe(FakeWebSocket.OPEN);
        store.disconnect();
    });

    it('closes a congested socket instead of queueing critical signaling behind stale data', () => {
        const { store, socket } = connectedStore();
        socket.bufferedAmount = 2 * 1024 * 1024;

        expect(store.send('signal:answer', { sdp: 'answer-sdp' })).toBe(false);

        expect(socket.sent).toHaveLength(0);
        expect(socket.readyState).toBe(FakeWebSocket.CLOSED);
        expect(socket.closeCode).toBe(4001);
        store.disconnect();
    });

    it('sends critical messages immediately when the socket is healthy', () => {
        const { store, socket } = connectedStore();

        expect(store.send('signal:answer', { sdp: 'answer-sdp' })).toBe(true);

        expect(socket.sent).toHaveLength(1);
        expect(JSON.parse(socket.sent[0])).toEqual({
            type: 'signal:answer',
            payload: { sdp: 'answer-sdp' }
        });
        store.disconnect();
    });

    it('retries the first dropped connection in under a third of a second', () => {
        vi.useFakeTimers();
        const { store, socket } = connectedStore();

        socket.close(1006, 'abnormal close');

        expect(FakeWebSocket.instances).toHaveLength(1);
        vi.advanceTimersByTime(199);
        expect(FakeWebSocket.instances).toHaveLength(1);
        vi.advanceTimersByTime(101);
        expect(FakeWebSocket.instances).toHaveLength(2);
        store.disconnect();
    });

    it('pauses reconnect attempts while the browser is offline', () => {
        vi.useFakeTimers();
        const { store, socket } = connectedStore();

        online = false;
        window.dispatchEvent(new Event('offline'));

        expect(socket.readyState).toBe(FakeWebSocket.CLOSED);
        expect(socket.closeCode).toBe(4002);
        expect(store.state.connected).toBe(false);
        expect(store.state.reconnecting).toBe(true);
        expect(store.state.networkOffline).toBe(true);
        expect(store.state.reconnectAttempt).toBe(0);

        vi.advanceTimersByTime(60_000);

        expect(FakeWebSocket.instances).toHaveLength(1);
        expect(store.state.reconnectAttempt).toBe(0);

        online = true;
        window.dispatchEvent(new Event('online'));

        expect(FakeWebSocket.instances).toHaveLength(2);
        expect(store.state.networkOffline).toBe(false);
        expect(store.state.reconnectAttempt).toBe(0);
        store.disconnect();
    });

    it('waits for the browser to come online before opening the first socket', () => {
        online = false;
        const store = createSessionStore();

        store.connect('review-room', 'Viewer One');

        expect(FakeWebSocket.instances).toHaveLength(0);
        expect(store.state.reconnecting).toBe(true);
        expect(store.state.networkOffline).toBe(true);

        online = true;
        window.dispatchEvent(new Event('online'));

        expect(FakeWebSocket.instances).toHaveLength(1);
        expect(FakeWebSocket.instances[0].url).toContain('/ws/room/review-room');
        store.disconnect();
    });
});
