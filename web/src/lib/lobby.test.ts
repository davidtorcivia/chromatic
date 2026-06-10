import { describe, it, expect } from 'vitest';
import {
    countdownParts,
    formatOpensIn,
    formatScheduleLabel,
    serverClockOffset
} from './lobby';

const MIN = 60 * 1000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

describe('serverClockOffset', () => {
    it('computes the offset from the server timestamp', () => {
        const clientNow = Date.parse('2026-06-09T12:00:00Z');
        expect(serverClockOffset('2026-06-09T12:00:30Z', clientNow)).toBe(30_000);
        expect(serverClockOffset('2026-06-09T11:59:00Z', clientNow)).toBe(-60_000);
    });

    it('falls back to 0 for missing or invalid input', () => {
        expect(serverClockOffset(undefined, Date.now())).toBe(0);
        expect(serverClockOffset(null, Date.now())).toBe(0);
        expect(serverClockOffset('not-a-date', Date.now())).toBe(0);
    });
});

describe('countdownParts', () => {
    const now = Date.parse('2026-06-09T12:00:00Z');

    it('splits the remaining time into D/H/M/S', () => {
        const target = now + 2 * DAY + 3 * HOUR + 4 * MIN + 5000;
        const parts = countdownParts(target, now);
        expect(parts).toMatchObject({ days: 2, hours: 3, minutes: 4, seconds: 5 });
        expect(parts.totalMs).toBe(2 * DAY + 3 * HOUR + 4 * MIN + 5000);
    });

    it('clamps to zero once the target has passed', () => {
        const parts = countdownParts(now - 5000, now);
        expect(parts).toMatchObject({ days: 0, hours: 0, minutes: 0, seconds: 0, totalMs: 0 });
    });
});

describe('formatOpensIn', () => {
    const now = Date.parse('2026-06-09T12:00:00Z');
    const at = (ms: number) => formatOpensIn(countdownParts(now + ms, now));

    it('picks the two most significant units', () => {
        expect(at(2 * DAY + 5 * HOUR)).toBe('2d 5h');
        expect(at(HOUR + 23 * MIN)).toBe('1h 23m');
        expect(at(12 * MIN + 5000)).toBe('12m 05s');
        expect(at(42_000)).toBe('42s');
    });
});

describe('formatScheduleLabel', () => {
    const tz = 'America/New_York';
    // 2026-06-09 is a Tuesday; 19:00 EDT = 23:00 UTC
    const scheduled = new Date('2026-06-09T23:00:00Z');

    it('uses "Today" for same calendar day in the viewer timezone', () => {
        const now = new Date('2026-06-09T14:00:00Z'); // 10:00 EDT same day
        const label = formatScheduleLabel(scheduled, now, tz);
        expect(label).toMatch(/^Today at /);
        expect(label).toContain('7:00');
        expect(label).toContain('EDT');
    });

    it('uses "Tomorrow" for the next calendar day', () => {
        const now = new Date('2026-06-08T14:00:00Z');
        expect(formatScheduleLabel(scheduled, now, tz)).toMatch(/^Tomorrow at /);
    });

    it('falls back to a weekday/date label further out', () => {
        const now = new Date('2026-06-01T14:00:00Z');
        const label = formatScheduleLabel(scheduled, now, tz);
        expect(label).toMatch(/^Tue, Jun 9 at /);
        expect(label).toContain('EDT');
    });

    it('respects the calendar day boundary, not 24h windows', () => {
        // 23:30 local on the 8th vs an event at 00:30 local on the 9th —
        // less than 24h away but still "Tomorrow".
        const lateNow = new Date('2026-06-09T03:30:00Z'); // 23:30 EDT Jun 8
        const earlyEvent = new Date('2026-06-09T04:30:00Z'); // 00:30 EDT Jun 9
        expect(formatScheduleLabel(earlyEvent, lateNow, tz)).toMatch(/^Tomorrow at /);
    });
});
