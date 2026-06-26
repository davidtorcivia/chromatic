import { describe, expect, it } from 'vitest';

import { parseStoredSession } from './storedSession';

const validSession = {
    participantId: 'participant-1',
    token: 'join-token',
    color: '#48b6a6',
    name: 'Viewer',
    role: 'viewer',
};

describe('parseStoredSession', () => {
    it('rejects missing and malformed stored session data', () => {
        expect(parseStoredSession(null)).toBeNull();
        expect(parseStoredSession('not-json')).toBeNull();
        expect(parseStoredSession(JSON.stringify({ token: 'join-token' }))).toBeNull();
    });

    it('accepts a valid room session payload', () => {
        expect(parseStoredSession(JSON.stringify(validSession))).toMatchObject(validSession);
    });

    it('rejects invalid roles and incomplete lobby payloads', () => {
        expect(
            parseStoredSession(JSON.stringify({ ...validSession, role: 'owner' }))
        ).toBeNull();
        expect(
            parseStoredSession(JSON.stringify({ ...validSession, lobby: { scheduledAt: '2026-06-26T12:00:00Z' } }))
        ).toBeNull();
    });

    it('accepts scheduled lobby metadata used by the waiting room', () => {
        const lobbySession = {
            ...validSession,
            serverTime: '2026-06-26T11:00:00Z',
            lobby: {
                scheduledAt: '2026-06-26T12:00:00Z',
                opensAt: '2026-06-26T11:50:00Z',
                waitingRoomEnabled: true,
            },
        };

        expect(parseStoredSession(JSON.stringify(lobbySession))).toMatchObject(lobbySession);
    });
});
