// Detached-element program audio, Chromium and Firefox (Playwright).
import { chromium, firefox } from '@playwright/test';

const BASE = process.env.BASE || 'http://127.0.0.1:8731';

for (const [name, launcher, opts] of [
    ['chromium', chromium, { args: ['--autoplay-policy=no-user-gesture-required'] }],
    ['firefox', firefox, { firefoxUserPrefs: { 'media.autoplay.default': 0, 'media.autoplay.blocking_policy': 0, 'media.autoplay.block-webaudio': false, 'media.autoplay.block-event.enabled': false } }]
]) {
    let browser;
    try {
        browser = await launcher.launch(opts);
        const page = await browser.newPage();
        page.on('console', (m) => { if (m.type() === 'error') console.log(`  [console] ${m.text()}`); });
        await page.goto(`${BASE}/audio.html`);
        await page.waitForFunction('window.ready === true', null, { timeout: 90000 });
        const r = await page.evaluate('JSON.parse(JSON.stringify(window.result))');
        console.log(`\n=== ${name} ===`);
        console.log(JSON.stringify(r, null, 1));
    } catch (e) {
        console.log(`\n=== ${name} === FAILED: ${e.message.split('\n')[0]}`);
    } finally {
        await browser?.close();
    }
}
