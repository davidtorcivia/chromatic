// Chat store for managing messages

export interface ChatMessage {
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

export function createChatStore() {
    let messages = $state<ChatMessage[]>([]);
    let unreadCount = $state(0);
    let isVisible = $state(false);

    function addMessage(msg: Omit<ChatMessage, 'id' | 'timestamp'> & { id?: string; timestamp?: number }) {
        const message: ChatMessage = {
            ...msg,
            id: msg.id || crypto.randomUUID(),
            timestamp: msg.timestamp || Date.now()
        };

        messages = [...messages, message];

        if (!isVisible) {
            unreadCount++;
        }
    }

    function loadHistory(msgs: ChatMessage[]) {
        // Server history is authoritative: replace local state with it so
        // messages that arrived during a WS outage aren't lost on reconnect.
        // Count messages we haven't seen before as unread (but not on the
        // initial load, where everything is "new" but already read history).
        if (messages.length > 0 && !isVisible) {
            const knownIds = new Set(messages.map(m => m.id));
            const newCount = msgs.reduce(
                (count, m) => count + (knownIds.has(m.id) ? 0 : 1),
                0
            );
            unreadCount += newCount;
        }
        messages = msgs;
    }

    function removeMessage(id: string) {
        messages = messages.filter((m) => m.id !== id);
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
        loadHistory,
        removeMessage,
        setVisible,
        clear
    };
}

export const chatStore = createChatStore();
