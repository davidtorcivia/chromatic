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

export const chatStore = createChatStore();
