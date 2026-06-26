type StorageKind = "local" | "session";

function getStorage(kind: StorageKind): Storage | null {
    if (typeof window === "undefined") return null;
    return kind === "local" ? window.localStorage : window.sessionStorage;
}

export function getStorageItem(kind: StorageKind, key: string): string | null {
    try {
        return getStorage(kind)?.getItem(key) ?? null;
    } catch {
        return null;
    }
}

export function setStorageItem(kind: StorageKind, key: string, value: string): boolean {
    try {
        getStorage(kind)?.setItem(key, value);
        return true;
    } catch {
        return false;
    }
}

export function removeStorageItem(kind: StorageKind, key: string): void {
    try {
        getStorage(kind)?.removeItem(key);
    } catch {
        // Nothing to remove when storage is unavailable.
    }
}
