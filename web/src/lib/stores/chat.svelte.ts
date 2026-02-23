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
        // Only load if we have no messages yet (avoid duplicates on reconnect)
        if (messages.length > 0) return;
        messages = msgs;
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
        setVisible,
        clear
    };
}

export const chatStore = createChatStore();
