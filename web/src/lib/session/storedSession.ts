import type { LobbyInfo } from "$lib/api/client";

export interface StoredSessionData {
    participantId: string;
    color: string;
    name?: string;
    role?: "admin" | "viewer";
    serverTime?: string;
    lobby?: LobbyInfo;
}

function isString(value: unknown): value is string {
    return typeof value === "string" && value.length > 0;
}

function isLobbyInfo(value: unknown): value is LobbyInfo {
    if (!value || typeof value !== "object") return false;
    const lobby = value as Record<string, unknown>;
    return (
        isString(lobby.scheduledAt) &&
        isString(lobby.opensAt) &&
        typeof lobby.waitingRoomEnabled === "boolean"
    );
}

export function parseStoredSession(raw: string | null): StoredSessionData | null {
    if (!raw) return null;
    try {
        const parsed = JSON.parse(raw) as unknown;
        if (!parsed || typeof parsed !== "object") return null;
        const data = parsed as Record<string, unknown>;
        if (!isString(data.participantId) || !isString(data.color)) {
            return null;
        }
        if (
            data.role !== undefined &&
            data.role !== "admin" &&
            data.role !== "viewer"
        ) {
            return null;
        }
        if (data.name !== undefined && !isString(data.name)) return null;
        if (data.serverTime !== undefined && !isString(data.serverTime)) return null;
        if (data.lobby !== undefined && !isLobbyInfo(data.lobby)) return null;
        return {
            participantId: data.participantId,
            color: data.color,
            name: data.name,
            role: data.role,
            serverTime: data.serverTime,
            lobby: data.lobby,
        } as StoredSessionData;
    } catch {
        return null;
    }
}
