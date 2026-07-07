import { test, expect } from './fixtures';

// This spec drives the real Svelte setup route but route-mocks the setup-related
// APIs so it does not depend on shared live DB state. Auth still uses the live
// admin session cookie; only the setup/config/keys/rooms/health/TURN responses
// are intercepted.

type CheckStatus = 'ready' | 'needs-action' | 'warning' | 'optional';

function makeCheck(id: string, status: CheckStatus, required: boolean, summary: string) {
    return { id, title: id, status, required, summary };
}

// Build a setup status payload. When `ready` is false the install is missing
// TURN reachability (the check the wizard exercises); when true every required
// check passes.
function statusPayload(ready: boolean) {
    const turnConn: CheckStatus = ready ? 'ready' : 'needs-action';
    const checks = [
        makeCheck('public-url', 'ready', true, 'Local development URL'),
        makeCheck('security', 'ready', true, 'Local development'),
        makeCheck('turn-config', 'ready', true, 'Self-hosted TURN configured'),
        makeCheck('turn-connectivity', turnConn, true, ready ? 'Server reachability test passed' : 'Run the TURN reachability test'),
        makeCheck('stream-key', 'ready', true, '1 stream key(s) configured'),
        makeCheck('room', 'ready', true, '1 room(s) configured'),
        makeCheck('branding', 'optional', false, 'Default branding can be set later.'),
    ];
    const requiredReady = checks.filter((c) => c.required && c.status === 'ready').length;
    return {
        readyToComplete: ready,
        firstRun: false,
        requiresAttention: false,
        progress: { ready: requiredReady, required: 6, total: checks.length },
        checks,
        facts: {
            publicUrl: 'http://localhost:3000',
            productionMode: false,
            allowedOrigins: [],
            turnMode: 'hybrid',
            turnCloudflareConfigured: false,
            turnStaticConfigured: false,
            hasTurnCredential: false,
            turnLastTestSuccess: ready,
            turnLastTestValidForCurrentConfig: ready,
            streamKeyCount: 1,
            firstStreamKeyId: 'k1',
            roomCount: 1,
            firstRoomSlug: 'studio',
        },
    };
}

test.describe('Setup wizard', () => {
    test('guided flow blocks completion until TURN reachability passes, then completes', async ({ page, adminPage }) => {
        // Authenticate against the live app via the shared fixture so the admin
        // session cookie is set on the browser context.
        await adminPage.authenticate();


        let setupReady = false;

        await page.route('**/api/setup/status', async (route) => {
            await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(statusPayload(setupReady)) });
        });
        await page.route('**/api/setup/complete', async (route) => {
            // Flipping to ready is what the UI does before calling complete; the
            // server stamps completion and returns ready status.
            await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(statusPayload(true)) });
        });
        await page.route('**/api/setup/dismiss', async (route) => {
            await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(statusPayload(false)) });
        });
        await page.route('**/api/config', async (route) => {
            await route.fulfill({
                status: 200,
                contentType: 'application/json',
                body: JSON.stringify({
                    hasTurnCredential: false,
                    turnMode: 'hybrid',
                    turnCloudflareConfigured: false,
                    publicUrl: 'http://localhost:3000',
                    whipFormat: 'http://localhost:3000/whip/{stream_key_token}',
                }),
            });
        });
        await page.route('**/api/stream-keys', async (route) => {
            await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify([{ id: 'k1', name: 'Main', keyToken: 'tok1', createdAt: '2026-01-01T00:00:00Z' }]) });
        });
        await page.route('**/api/rooms', async (route) => {
            await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify([{ id: 'r1', slug: 'studio', name: 'Studio', status: 'pending', createdAt: '2026-01-02T00:00:00Z', hasPassword: false, waitingRoomEnabled: true, watermarkMode: 'none' }]) });
        });
        await page.route('**/health', async (route) => {
            await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ status: 'ok' }) });
        });
        await page.route('**/api/config/test-turn', async (route) => {
            // A successful reachability test flips the mocked status to ready so
            // the subsequent refreshSetupStatus() call observes readiness.
            setupReady = true;
            await route.fulfill({
                status: 200,
                contentType: 'application/json',
                body: JSON.stringify({ success: true, results: [{ server: 'turn.local:3478', reachable: true, protocol: 'udp', testType: 'self-hosted' }], message: 'At least one TURN endpoint is reachable from this server' }),
            });
        });

        await page.goto('/admin/setup');

        // The guided flow renders the hero and step sidebar — no self-attested
        // checklist checkboxes exist anywhere on the page now.
        await expect(page.getByRole('button', { name: 'Skip for now' })).toBeVisible();
        await expect(page.getByRole('checkbox', { name: /validated for this deployment/i })).toHaveCount(0);
        await expect(page.getByRole('checkbox', { name: /running locally/i })).toHaveCount(0);
        await expect(page.getByRole('checkbox', { name: /OBS configured/i })).toHaveCount(0);

        // Jump to the finish step via the sidebar. With a missing TURN check,
        // Finish setup is disabled and the required-check list is shown.
        const sidebarStep = (title: string) =>
            page.locator('.setup-steps li', { hasText: title }).getByRole('button');
        await sidebarStep('Finish').click();
        await expect(page.getByText(/required checks still need action/i)).toBeVisible();
        const finishButton = page.getByRole('button', { name: 'Finish setup' });
        await expect(finishButton).toBeDisabled();

        // Go to the connectivity step and run the TURN reachability test, which
        // flips the mocked status to ready.
        await sidebarStep('Connectivity').click();
        const [testTurnResponse] = await Promise.all([
            page.waitForResponse((r) => r.url().includes('/api/config/test-turn') && r.request().method() === 'POST'),
            page.getByRole('button', { name: 'Test TURN reachability' }).click(),
        ]);
        expect(testTurnResponse.ok()).toBeTruthy();
        // A successful reachability test flips the mocked status to ready, and
        // refreshSetupStatus auto-advances the wizard to the finish step. Assert
        // that stable finish-step ready state (not the TURN results panel, which
        // unmounts on the auto-advance) so the check does not race a transient.
        await expect(page.getByText(/Chromatic is ready for the first stream/i)).toBeVisible();
        await expect(finishButton).toBeEnabled();

        // Completing posts to /api/setup/complete and lands on the dashboard.
        const [completeResponse] = await Promise.all([
            page.waitForResponse((r) => r.url().includes('/api/setup/complete') && r.request().method() === 'POST'),
            finishButton.click(),
        ]);
        expect(completeResponse.ok()).toBeTruthy();
        await expect(page).toHaveURL(/\/admin$/);
    });
});
