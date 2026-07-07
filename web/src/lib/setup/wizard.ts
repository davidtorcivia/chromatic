import type { SetupCheck, SetupStatusResponse } from '$lib/api/client';

export type SetupStepId = 'preflight' | 'turn' | 'branding' | 'stream' | 'room' | 'finish';

// The six guided-flow steps. The setup route navigates by id (not array index)
// so reordering or inserting a step does not silently shift every goto.
export const setupSteps: { id: SetupStepId; title: string; description: string }[] = [
    { id: 'preflight', title: 'Preflight', description: 'Verify environment and security' },
    { id: 'turn', title: 'Connectivity', description: 'TURN and network checks' },
    { id: 'branding', title: 'Branding', description: 'Watermark defaults' },
    { id: 'stream', title: 'Stream setup', description: 'Create stream key + OBS' },
    { id: 'room', title: 'First room', description: 'Create and test a room' },
    { id: 'finish', title: 'Finish', description: 'Review and complete setup' },
];

// Map each backend check id to the wizard step that owns it. Unknown ids fall
// back to preflight so a future check never strands the user.
export function stepForCheck(checkId: string): SetupStepId {
    switch (checkId) {
        case 'public-url':
        case 'security':
            return 'preflight';
        case 'turn-config':
        case 'turn-connectivity':
            return 'turn';
        case 'branding':
            return 'branding';
        case 'stream-key':
            return 'stream';
        case 'room':
            return 'room';
        default:
            return 'preflight';
    }
}

// Required checks that are not yet ready, in the backend's check order. Optional
// checks (branding) are excluded so they never appear as blockers.
export function requiredMissingChecks(status: SetupStatusResponse | null): SetupCheck[] {
    if (!status) return [];
    return status.checks.filter((c) => c.required && c.status !== 'ready');
}

export function checkById(status: SetupStatusResponse | null, id: string): SetupCheck | null {
    if (!status) return null;
    return status.checks.find((c) => c.id === id) ?? null;
}

// The step the wizard should land on: preflight when health is down or status
// is unknown, otherwise the step owning the first missing required check,
// otherwise finish.
export function nextSetupStep(status: SetupStatusResponse | null, healthOK: boolean): SetupStepId {
    if (!healthOK || !status) return 'preflight';
    const missing = requiredMissingChecks(status);
    if (missing.length === 0) return 'finish';
    return stepForCheck(missing[0].id);
}

// Rounded completion percentage over required checks. 0 when there is nothing to
// measure (null status or no required checks).
export function setupProgressPercent(status: SetupStatusResponse | null): number {
    if (!status || status.progress.required === 0) return 0;
    return Math.round((status.progress.ready / status.progress.required) * 100);
}

// Tone mapping for StatusPill. ready -> good, needs-action -> bad, warning ->
// warn, optional -> neutral.
export function setupCheckTone(check: SetupCheck | null): 'good' | 'warn' | 'bad' | 'neutral' {
    if (!check) return 'neutral';
    switch (check.status) {
        case 'ready':
            return 'good';
        case 'needs-action':
            return 'bad';
        case 'warning':
            return 'warn';
        case 'optional':
            return 'neutral';
        default:
            return 'neutral';
    }
}
