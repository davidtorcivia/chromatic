// Countdown-lobby time helpers.
//
// All countdowns are anchored to the SERVER clock: the API returns a
// serverTime alongside schedule data, we compute the offset to the local
// clock once, and every tick applies that offset. A viewer whose machine is
// minutes off must still see the room open exactly when the server opens it.

export interface CountdownParts {
    days: number;
    hours: number;
    minutes: number;
    seconds: number;
    /** Raw remaining milliseconds (clamped to 0) */
    totalMs: number;
}

/**
 * Offset (ms) to add to Date.now() to approximate the server clock.
 * Returns 0 when no server time is available (falls back to the local clock).
 */
export function serverClockOffset(serverTimeIso: string | undefined | null, clientNowMs: number): number {
    if (!serverTimeIso) return 0;
    const serverMs = Date.parse(serverTimeIso);
    if (Number.isNaN(serverMs)) return 0;
    return serverMs - clientNowMs;
}

/** Split the time remaining until target into D/H/M/S display parts. */
export function countdownParts(targetMs: number, nowMs: number): CountdownParts {
    const totalMs = Math.max(0, targetMs - nowMs);
    const totalSeconds = Math.floor(totalMs / 1000);
    return {
        days: Math.floor(totalSeconds / 86400),
        hours: Math.floor((totalSeconds % 86400) / 3600),
        minutes: Math.floor((totalSeconds % 3600) / 60),
        seconds: totalSeconds % 60,
        totalMs
    };
}

/** Compact "opens in" phrasing: "2d 4h", "1h 23m", "12m 05s", "42s". */
export function formatOpensIn(parts: CountdownParts): string {
    const pad = (n: number) => String(n).padStart(2, '0');
    if (parts.days > 0) return `${parts.days}d ${parts.hours}h`;
    if (parts.hours > 0) return `${parts.hours}h ${pad(parts.minutes)}m`;
    if (parts.minutes > 0) return `${parts.minutes}m ${pad(parts.seconds)}s`;
    return `${parts.seconds}s`;
}

/** Calendar day key (yyyy-mm-dd) for a date in a specific time zone. */
function dayKeyInZone(date: Date, timeZone?: string): string {
    const fmt = new Intl.DateTimeFormat('en-CA', {
        ...(timeZone ? { timeZone } : {}),
        year: 'numeric',
        month: '2-digit',
        day: '2-digit'
    });
    return fmt.format(date);
}

/**
 * Timezone-aware schedule label in the viewer's local time with relative day
 * phrasing: "Today at 7:00 PM EDT", "Tomorrow at 7:00 PM EDT", or
 * "Mon, Jun 15 at 7:00 PM EDT". `timeZone` is injectable for tests.
 */
export function formatScheduleLabel(scheduled: Date, now: Date = new Date(), timeZone?: string): string {
    const timeLabel = new Intl.DateTimeFormat(undefined, {
        ...(timeZone ? { timeZone } : {}),
        hour: 'numeric',
        minute: '2-digit',
        timeZoneName: 'short'
    }).format(scheduled);

    const scheduledDay = dayKeyInZone(scheduled, timeZone);
    const todayDay = dayKeyInZone(now, timeZone);
    const tomorrowDay = dayKeyInZone(new Date(now.getTime() + 24 * 60 * 60 * 1000), timeZone);

    if (scheduledDay === todayDay) return `Today at ${timeLabel}`;
    if (scheduledDay === tomorrowDay) return `Tomorrow at ${timeLabel}`;

    const dayLabel = new Intl.DateTimeFormat(undefined, {
        ...(timeZone ? { timeZone } : {}),
        weekday: 'short',
        month: 'short',
        day: 'numeric'
    }).format(scheduled);
    return `${dayLabel} at ${timeLabel}`;
}
