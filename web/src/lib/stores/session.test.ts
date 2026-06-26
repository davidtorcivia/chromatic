import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createSessionStore } from './session.svelte';

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
    beforeEach(() => {
        FakeWebSocket.instances = [];
        vi.stubGlobal('WebSocket', FakeWebSocket);
    });

    afterEach(() => {
        vi.useRealTimers();
        vi.unstubAllGlobals();
    });

    function connectedStore() {
        const store = createSessionStore();
        store.connect('review-room', 'signed-token', 'Viewer One');
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
});
