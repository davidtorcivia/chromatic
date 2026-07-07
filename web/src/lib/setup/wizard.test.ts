import { describe, it, expect } from 'vitest';
import {
    setupSteps,
    requiredMissingChecks,
    checkById,
    stepForCheck,
    nextSetupStep,
    setupProgressPercent,
    setupCheckTone
} from './wizard';
import type { SetupStatusResponse, SetupCheck } from '$lib/api/client';

function check(partial: Partial<SetupCheck>): SetupCheck {
    return {
        id: 'x',
        title: 'X',
        status: 'ready',
        required: true,
        summary: '',
        ...partial
    };
}

const readyCheck = (id: string) => check({ id, status: 'ready' });

function status(checks: SetupCheck[], progress?: { ready: number; required: number; total: number }): SetupStatusResponse {
    const required = checks.filter((c) => c.required).length;
    const ready = checks.filter((c) => c.required && c.status === 'ready').length;
    return {
        readyToComplete: required > 0 && ready === required,
        firstRun: false,
        requiresAttention: false,
        progress: progress ?? { ready, required, total: checks.length },
        checks,
        facts: {
            publicUrl: '',
            productionMode: false,
            allowedOrigins: [],
            turnMode: 'hybrid',
            turnCloudflareConfigured: false,
            turnStaticConfigured: false,
            hasTurnCredential: false,
            turnLastTestSuccess: false,
            turnLastTestValidForCurrentConfig: false,
            streamKeyCount: 0,
            roomCount: 0
        }
    };
}

describe('setupSteps', () => {
    it('includes the six guided steps ending in finish', () => {
        const ids = setupSteps.map((s) => s.id);
        expect(ids).toEqual(['preflight', 'turn', 'branding', 'stream', 'room', 'finish']);
    });
});

describe('stepForCheck', () => {
    it('maps each check id to its owning step', () => {
        expect(stepForCheck('public-url')).toBe('preflight');
        expect(stepForCheck('security')).toBe('preflight');
        expect(stepForCheck('turn-config')).toBe('turn');
        expect(stepForCheck('turn-connectivity')).toBe('turn');
        expect(stepForCheck('branding')).toBe('branding');
        expect(stepForCheck('stream-key')).toBe('stream');
        expect(stepForCheck('room')).toBe('room');
    });

    it('falls back to preflight for unknown ids', () => {
        expect(stepForCheck('nope')).toBe('preflight');
    });
});

describe('requiredMissingChecks', () => {
    it('returns required non-ready checks in backend order and excludes optional branding', () => {
        const s = status([
            check({ id: 'public-url', status: 'ready' }),
            check({ id: 'turn-config', status: 'needs-action', summary: 'missing' }),
            check({ id: 'stream-key', status: 'needs-action', summary: 'no key' }),
            check({ id: 'branding', status: 'optional', required: false })
        ]);
        const missing = requiredMissingChecks(s);
        expect(missing.map((c) => c.id)).toEqual(['turn-config', 'stream-key']);
    });

    it('returns empty for null status', () => {
        expect(requiredMissingChecks(null)).toEqual([]);
    });
});

describe('checkById', () => {
    it('finds a check by id', () => {
        const s = status([check({ id: 'room', status: 'ready' })]);
        expect(checkById(s, 'room')?.status).toBe('ready');
    });

    it('returns null when missing or status is null', () => {
        const s = status([check({ id: 'room' })]);
        expect(checkById(s, 'nope')).toBeNull();
        expect(checkById(null, 'room')).toBeNull();
    });
});

describe('nextSetupStep', () => {
    it('returns preflight when health is not OK', () => {
        const s = status([readyCheck('public-url'), readyCheck('room')]);
        expect(nextSetupStep(s, false)).toBe('preflight');
    });

    it('returns preflight for null status', () => {
        expect(nextSetupStep(null, true)).toBe('preflight');
    });

    it('returns the step for the first missing required check', () => {
        const s = status([
            readyCheck('public-url'),
            readyCheck('security'),
            check({ id: 'turn-connectivity', status: 'needs-action' }),
            check({ id: 'stream-key', status: 'needs-action' })
        ]);
        expect(nextSetupStep(s, true)).toBe('turn');
    });

    it('skips ahead to stream when only stream-key is missing', () => {
        const s = status([
            readyCheck('public-url'),
            readyCheck('security'),
            readyCheck('turn-config'),
            readyCheck('turn-connectivity'),
            check({ id: 'stream-key', status: 'needs-action' })
        ]);
        expect(nextSetupStep(s, true)).toBe('stream');
    });

    it('returns finish when every required check is ready', () => {
        const s = status([
            readyCheck('public-url'),
            readyCheck('security'),
            readyCheck('turn-config'),
            readyCheck('turn-connectivity'),
            readyCheck('stream-key'),
            readyCheck('room'),
            check({ id: 'branding', status: 'optional', required: false })
        ]);
        expect(nextSetupStep(s, true)).toBe('finish');
    });
});

describe('setupProgressPercent', () => {
    it('returns 0 for null status', () => {
        expect(setupProgressPercent(null)).toBe(0);
    });

    it('returns 0 when there are no required checks', () => {
        const s = status([check({ id: 'branding', required: false })], { ready: 0, required: 0, total: 1 });
        expect(setupProgressPercent(s)).toBe(0);
    });

    it('rounds ready/required to a percentage', () => {
        const s = status(
            [
                check({ id: 'a', status: 'ready' }),
                check({ id: 'b', status: 'needs-action' }),
                check({ id: 'c', status: 'needs-action' })
            ],
            { ready: 1, required: 3, total: 3 }
        );
        expect(setupProgressPercent(s)).toBe(33);
    });

    it('reaches 100 when all required are ready', () => {
        const s = status([check({ id: 'a', status: 'ready' }), check({ id: 'b', status: 'ready' })], { ready: 2, required: 2, total: 2 });
        expect(setupProgressPercent(s)).toBe(100);
    });
});

describe('setupCheckTone', () => {
    it('maps status to tone', () => {
        expect(setupCheckTone(check({ status: 'ready' }))).toBe('good');
        expect(setupCheckTone(check({ status: 'needs-action' }))).toBe('bad');
        expect(setupCheckTone(check({ status: 'warning' }))).toBe('warn');
        expect(setupCheckTone(check({ status: 'optional' }))).toBe('neutral');
    });

    it('returns neutral for a null check', () => {
        expect(setupCheckTone(null)).toBe('neutral');
    });
});
