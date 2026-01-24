// Cursor store for laser pointer functionality

export interface Cursor {
    participantId: string;
    participantName: string;
    color: string;
    x: number;
    y: number;
    active: boolean;
    lastUpdate: number;
}

export function createCursorStore() {
    let cursors = $state<Map<string, Cursor>>(new Map());
    let cleanupTimer: ReturnType<typeof setInterval> | null = null;

    function update(data: {
        participantId: string;
        participantName: string;
        color: string;
        x: number;
        y: number;
        active: boolean;
    }) {
        cursors.set(data.participantId, {
            ...data,
            lastUpdate: Date.now()
        });
    }

    function remove(participantId: string) {
        cursors.delete(participantId);
    }

    function startCleanup() {
        // Fade out inactive cursors after 500ms
        cleanupTimer = setInterval(() => {
            const now = Date.now();
            for (const [id, cursor] of cursors) {
                if (now - cursor.lastUpdate > 500) {
                    cursors.delete(id);
                }
            }
        }, 100);
    }

    function stopCleanup() {
        if (cleanupTimer) {
            clearInterval(cleanupTimer);
            cleanupTimer = null;
        }
    }

    function getAll(): Cursor[] {
        return Array.from(cursors.values());
    }

    return {
        get cursors() { return cursors; },
        getAll,
        update,
        remove,
        startCleanup,
        stopCleanup
    };
}

export const cursorStore = createCursorStore();
