import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getStorageItem, removeStorageItem, setStorageItem } from './safeStorage';

function makeStorage(): Storage {
    const entries = new Map<string, string>();
    return {
        get length() {
            return entries.size;
        },
        clear: vi.fn(() => entries.clear()),
        getItem: vi.fn((key: string) => entries.get(key) ?? null),
        key: vi.fn((index: number) => Array.from(entries.keys())[index] ?? null),
        removeItem: vi.fn((key: string) => {
            entries.delete(key);
        }),
        setItem: vi.fn((key: string, value: string) => {
            entries.set(key, value);
        }),
    };
}

describe('safeStorage', () => {
    beforeEach(() => {
        Object.defineProperty(window, 'localStorage', {
            configurable: true,
            value: makeStorage(),
        });
        Object.defineProperty(window, 'sessionStorage', {
            configurable: true,
            value: makeStorage(),
        });
    });

    afterEach(() => {
        vi.restoreAllMocks();
    });

    it('reads and writes the requested storage area', () => {
        expect(setStorageItem('local', 'name', 'Ada')).toBe(true);
        expect(setStorageItem('session', 'token', 'join-token')).toBe(true);

        expect(getStorageItem('local', 'name')).toBe('Ada');
        expect(getStorageItem('session', 'token')).toBe('join-token');
    });

    it('reports failed writes and treats failed reads as missing', () => {
        vi.spyOn(window.sessionStorage, 'setItem').mockImplementation(() => {
            throw new Error('storage denied');
        });
        vi.spyOn(window.sessionStorage, 'getItem').mockImplementation(() => {
            throw new Error('storage denied');
        });

        expect(setStorageItem('session', 'token', 'join-token')).toBe(false);
        expect(getStorageItem('session', 'token')).toBeNull();
    });

    it('does not throw when removal is blocked', () => {
        vi.spyOn(window.sessionStorage, 'removeItem').mockImplementation(() => {
            throw new Error('storage denied');
        });

        expect(() => removeStorageItem('session', 'token')).not.toThrow();
    });
});
