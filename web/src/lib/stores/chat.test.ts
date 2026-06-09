import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import { createChatStore, type ChatMessage } from './chat.svelte';

function makeMessage(id: string, content: string, overrides: Partial<ChatMessage> = {}): ChatMessage {
    return {
        id,
        participantId: 'user-1',
        participantName: 'Alice',
        type: 'text',
        content,
        timestamp: Date.now(),
        ...overrides
    };
}

describe('ChatStore', () => {
    let store: ReturnType<typeof createChatStore>;

    beforeEach(() => {
        store = createChatStore();
        vi.useFakeTimers();
    });

    afterEach(() => {
        vi.useRealTimers();
    });

    describe('addMessage', () => {
        it('adds a text message with generated id and timestamp', () => {
            const beforeTime = Date.now();

            store.addMessage({
                participantId: 'user-1',
                participantName: 'Alice',
                type: 'text',
                content: 'Hello, world!'
            });

            expect(store.messages).toHaveLength(1);
            expect(store.messages[0].participantId).toBe('user-1');
            expect(store.messages[0].participantName).toBe('Alice');
            expect(store.messages[0].type).toBe('text');
            expect(store.messages[0].content).toBe('Hello, world!');
            expect(store.messages[0].id).toBeDefined();
            expect(store.messages[0].timestamp).toBeGreaterThanOrEqual(beforeTime);
        });

        it('preserves a provided id and timestamp', () => {
            store.addMessage({
                id: 'server-id-1',
                participantId: 'user-1',
                participantName: 'Alice',
                type: 'text',
                content: 'Hello',
                timestamp: 12345
            });

            expect(store.messages[0].id).toBe('server-id-1');
            expect(store.messages[0].timestamp).toBe(12345);
        });

        it('adds a file message', () => {
            store.addMessage({
                participantId: 'user-2',
                participantName: 'Bob',
                type: 'file',
                content: '',
                file: {
                    id: 'file-123',
                    name: 'screenshot.png',
                    mimeType: 'image/png',
                    url: '/api/files/file-123',
                    thumbnailUrl: '/api/files/file-123/thumbnail'
                }
            });

            expect(store.messages).toHaveLength(1);
            expect(store.messages[0].type).toBe('file');
            expect(store.messages[0].file?.name).toBe('screenshot.png');
        });

        it('increments unread count when panel not visible', () => {
            expect(store.unreadCount).toBe(0);

            store.addMessage({
                participantId: 'user-1',
                participantName: 'Alice',
                type: 'text',
                content: 'Message 1'
            });

            expect(store.unreadCount).toBe(1);

            store.addMessage({
                participantId: 'user-2',
                participantName: 'Bob',
                type: 'text',
                content: 'Message 2'
            });

            expect(store.unreadCount).toBe(2);
        });

        it('does not increment unread count when panel is visible', () => {
            store.setVisible(true);

            store.addMessage({
                participantId: 'user-1',
                participantName: 'Alice',
                type: 'text',
                content: 'Message 1'
            });

            expect(store.unreadCount).toBe(0);
        });

        it('preserves message order', () => {
            store.addMessage({
                participantId: 'user-1',
                participantName: 'Alice',
                type: 'text',
                content: 'First'
            });

            vi.advanceTimersByTime(100);

            store.addMessage({
                participantId: 'user-2',
                participantName: 'Bob',
                type: 'text',
                content: 'Second'
            });

            expect(store.messages[0].content).toBe('First');
            expect(store.messages[1].content).toBe('Second');
            expect(store.messages[1].timestamp).toBeGreaterThan(store.messages[0].timestamp);
        });
    });

    describe('loadHistory', () => {
        it('loads history into an empty store without marking unread', () => {
            store.loadHistory([makeMessage('m1', 'one'), makeMessage('m2', 'two')]);

            expect(store.messages).toHaveLength(2);
            expect(store.messages[0].id).toBe('m1');
            expect(store.unreadCount).toBe(0);
        });

        it('replaces local state with server history on reconnect', () => {
            store.loadHistory([makeMessage('m1', 'one')]);
            store.addMessage(makeMessage('m2', 'two'));

            // Reconnect: server history now includes messages that arrived
            // during the WS outage.
            store.loadHistory([
                makeMessage('m1', 'one'),
                makeMessage('m2', 'two'),
                makeMessage('m3', 'missed during outage')
            ]);

            expect(store.messages).toHaveLength(3);
            expect(store.messages.map(m => m.id)).toEqual(['m1', 'm2', 'm3']);
        });

        it('is authoritative: drops local messages the server does not have', () => {
            store.loadHistory([makeMessage('m1', 'one')]);
            store.addMessage(makeMessage('local-only', 'ghost'));

            store.loadHistory([makeMessage('m1', 'one'), makeMessage('m2', 'two')]);

            expect(store.messages.map(m => m.id)).toEqual(['m1', 'm2']);
        });

        it('counts previously-unseen messages as unread on reconnect when hidden', () => {
            store.loadHistory([makeMessage('m1', 'one')]);
            expect(store.unreadCount).toBe(0);

            store.loadHistory([
                makeMessage('m1', 'one'),
                makeMessage('m2', 'two'),
                makeMessage('m3', 'three')
            ]);

            expect(store.unreadCount).toBe(2);
        });

        it('does not count reconnect messages as unread when panel is visible', () => {
            store.loadHistory([makeMessage('m1', 'one')]);
            store.setVisible(true);

            store.loadHistory([makeMessage('m1', 'one'), makeMessage('m2', 'two')]);

            expect(store.unreadCount).toBe(0);
            expect(store.messages).toHaveLength(2);
        });
    });

    describe('setVisible', () => {
        it('clears unread count when set to visible', () => {
            store.addMessage({
                participantId: 'user-1',
                participantName: 'Alice',
                type: 'text',
                content: 'Message 1'
            });

            store.addMessage({
                participantId: 'user-2',
                participantName: 'Bob',
                type: 'text',
                content: 'Message 2'
            });

            expect(store.unreadCount).toBe(2);

            store.setVisible(true);

            expect(store.unreadCount).toBe(0);
            expect(store.isVisible).toBe(true);
        });

        it('does not clear unread count when set to not visible', () => {
            store.addMessage({
                participantId: 'user-1',
                participantName: 'Alice',
                type: 'text',
                content: 'Message 1'
            });

            store.setVisible(false);

            expect(store.unreadCount).toBe(1);
            expect(store.isVisible).toBe(false);
        });
    });

    describe('clear', () => {
        it('removes all messages and resets unread count', () => {
            store.addMessage({
                participantId: 'user-1',
                participantName: 'Alice',
                type: 'text',
                content: 'Message 1'
            });

            store.addMessage({
                participantId: 'user-2',
                participantName: 'Bob',
                type: 'text',
                content: 'Message 2'
            });

            expect(store.messages).toHaveLength(2);
            expect(store.unreadCount).toBe(2);

            store.clear();

            expect(store.messages).toHaveLength(0);
            expect(store.unreadCount).toBe(0);
        });
    });
});
