import { chromium, firefox } from '@playwright/test';
const BASE = process.env.BASE || 'http://127.0.0.1:8731';
const W = process.env.W || '3840';
for (const [name, launcher] of [['chromium', chromium], ['firefox', firefox]]) {
    let browser;
    try {
        browser = await launcher.launch();
        const page = await browser.newPage({ viewport: { width: 1000, height: 700 } });
        page.on('pageerror', (e) => console.log('  [pageerror]', e.message));
        await page.goto(`${BASE}/rvfc.html?w=${W}`);
        await page.waitForFunction('window.ready === true', null, { timeout: 120000 });
        const r = await page.evaluate('JSON.parse(JSON.stringify(window.result))');
        console.log(`\n=== ${name} ===`);
        console.log(JSON.stringify(r, null, 1));
    } catch (e) {
        console.log(`\n=== ${name} === FAILED: ${e.message.split('\n')[0]}`);
    } finally {
        await browser?.close();
    }
}
