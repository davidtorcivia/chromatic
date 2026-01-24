import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

// Test cursor store logic directly without Svelte runtime
interface Cursor {
    participantId: string;
    participantName: string;
    color: string;
    x: number;
    y: number;
    active: boolean;
    lastUpdate: number;
}

function createTestCursorStore() {
    let cursors = new Map<string, Cursor>();
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

describe('CursorStore', () => {
    let store: ReturnType<typeof createTestCursorStore>;

    beforeEach(() => {
        vi.useFakeTimers();
        store = createTestCursorStore();
    });

    afterEach(() => {
        store.stopCleanup();
        vi.useRealTimers();
    });

    describe('update', () => {
        it('adds a new cursor', () => {
            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.5,
                y: 0.5,
                active: true
            });

            expect(store.cursors.size).toBe(1);
            const cursor = store.cursors.get('user-1');
            expect(cursor?.participantName).toBe('Alice');
            expect(cursor?.color).toBe('#ff0000');
            expect(cursor?.x).toBe(0.5);
            expect(cursor?.y).toBe(0.5);
            expect(cursor?.active).toBe(true);
        });

        it('updates existing cursor', () => {
            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.2,
                y: 0.2,
                active: true
            });

            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.8,
                y: 0.8,
                active: true
            });

            expect(store.cursors.size).toBe(1);
            const cursor = store.cursors.get('user-1');
            expect(cursor?.x).toBe(0.8);
            expect(cursor?.y).toBe(0.8);
        });

        it('sets lastUpdate timestamp', () => {
            const now = Date.now();
            vi.setSystemTime(now);

            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.5,
                y: 0.5,
                active: true
            });

            const cursor = store.cursors.get('user-1');
            expect(cursor?.lastUpdate).toBe(now);
        });

        it('handles multiple cursors', () => {
            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.2,
                y: 0.2,
                active: true
            });

            store.update({
                participantId: 'user-2',
                participantName: 'Bob',
                color: '#00ff00',
                x: 0.8,
                y: 0.8,
                active: true
            });

            expect(store.cursors.size).toBe(2);
        });
    });

    describe('remove', () => {
        it('removes cursor by participant id', () => {
            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.5,
                y: 0.5,
                active: true
            });

            store.remove('user-1');

            expect(store.cursors.size).toBe(0);
        });

        it('does nothing for non-existent cursor', () => {
            store.remove('non-existent');
            expect(store.cursors.size).toBe(0);
        });
    });

    describe('getAll', () => {
        it('returns empty array when no cursors', () => {
            expect(store.getAll()).toEqual([]);
        });

        it('returns all cursors as array', () => {
            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.2,
                y: 0.2,
                active: true
            });

            store.update({
                participantId: 'user-2',
                participantName: 'Bob',
                color: '#00ff00',
                x: 0.8,
                y: 0.8,
                active: false
            });

            const all = store.getAll();
            expect(all).toHaveLength(2);
            expect(all.find(c => c.participantId === 'user-1')).toBeDefined();
            expect(all.find(c => c.participantId === 'user-2')).toBeDefined();
        });
    });

    describe('cleanup', () => {
        it('removes stale cursors after 500ms', () => {
            const now = Date.now();
            vi.setSystemTime(now);

            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.5,
                y: 0.5,
                active: true
            });

            store.startCleanup();

            // Advance time by 600ms (past 500ms threshold)
            vi.advanceTimersByTime(600);

            expect(store.cursors.size).toBe(0);
        });

        it('keeps fresh cursors', () => {
            const now = Date.now();
            vi.setSystemTime(now);

            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.5,
                y: 0.5,
                active: true
            });

            store.startCleanup();

            // Advance time by 200ms
            vi.advanceTimersByTime(200);

            // Update the cursor to refresh it
            vi.setSystemTime(now + 200);
            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.6,
                y: 0.6,
                active: true
            });

            // Advance another 400ms (total 600ms since start, but only 400ms since last update)
            vi.advanceTimersByTime(400);

            expect(store.cursors.size).toBe(1);
        });

        it('runs cleanup at 100ms intervals', () => {
            store.startCleanup();

            const now = Date.now();
            vi.setSystemTime(now);

            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.5,
                y: 0.5,
                active: true
            });

            // Advance time by 100ms - cleanup runs but cursor is fresh
            vi.advanceTimersByTime(100);
            expect(store.cursors.size).toBe(1);

            // Advance time by 100ms more
            vi.advanceTimersByTime(100);
            expect(store.cursors.size).toBe(1);
        });

        it('stopCleanup stops the interval', () => {
            const now = Date.now();
            vi.setSystemTime(now);

            store.update({
                participantId: 'user-1',
                participantName: 'Alice',
                color: '#ff0000',
                x: 0.5,
                y: 0.5,
                active: true
            });

            store.startCleanup();
            store.stopCleanup();

            // Advance time past cleanup threshold
            vi.advanceTimersByTime(1000);

            // Cursor should still exist because cleanup was stopped
            expect(store.cursors.size).toBe(1);
        });
    });
});
