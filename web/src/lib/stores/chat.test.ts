import { describe, it, expect, beforeEach, vi } from 'vitest';

// We need to test the store logic without Svelte runtime
// Extract the core logic for testing

interface ChatMessage {
    id: string;
    participantId: string;
    participantName: string;
    type: 'text' | 'file';
    content: string;
    file?: {
        id: string;
        name: string;
        mimeType: string;
        url: string;
        thumbnailUrl?: string;
    };
    timestamp: number;
}

// Test the store logic directly
function createTestChatStore() {
    let messages: ChatMessage[] = [];
    let unreadCount = 0;
    let isVisible = false;

    function addMessage(msg: Omit<ChatMessage, 'id' | 'timestamp'>) {
        const message: ChatMessage = {
            ...msg,
            id: crypto.randomUUID(),
            timestamp: Date.now()
        };

        messages = [...messages, message];

        if (!isVisible) {
            unreadCount++;
        }
    }

    function setVisible(visible: boolean) {
        isVisible = visible;
        if (visible) {
            unreadCount = 0;
        }
    }

    function clear() {
        messages = [];
        unreadCount = 0;
    }

    return {
        get messages() { return messages; },
        get unreadCount() { return unreadCount; },
        get isVisible() { return isVisible; },
        addMessage,
        setVisible,
        clear
    };
}

describe('ChatStore', () => {
    let store: ReturnType<typeof createTestChatStore>;

    beforeEach(() => {
        store = createTestChatStore();
        vi.useFakeTimers();
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
