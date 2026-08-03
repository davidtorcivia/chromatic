// Live colour-parity runner for Chromium and Firefox (Playwright).
// Real Safari is driven separately through safaridriver (safari.mjs), because
// Playwright's WebKit is a bundled build, not Safari.
import { chromium, firefox } from '@playwright/test';
import { readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';

import { decodePNG, samplePatches, maxDelta } from './png.mjs';

const DIR = path.dirname(new URL(import.meta.url).pathname);
const W = 640, H = 2520;
const XS = [80, 240, 400, 560];
const ROWS = { video: 180, webgl: 540, canvas2d: 900, webglNoConv: 1260, bitmap2d: 1620, webglBitmap: 1980, webglCanvas: 2340 };
const BASE = process.env.BASE || 'http://127.0.0.1:8731';
const CODECS = (process.env.CODECS || 'H264,VP8').split(',');

const results = [];

for (const codec of CODECS) {
    for (const [name, launcher] of [['chromium', chromium], ['firefox', firefox]]) {
        let browser;
        try {
            browser = await launcher.launch();
            const page = await browser.newPage({ viewport: { width: W, height: H } });
            page.on('console', (m) => { if (m.type() === 'error') console.log(`  [console] ${m.text()}`); });
            await page.goto(`${BASE}/live.html?codec=${codec}`);
            await page.waitForFunction('window.ready === true', null, { timeout: 45000 });

            const info = await page.evaluate('window.info');
            const glErr = await page.evaluate('window.glErr');
            const shot = path.join(DIR, `live-${name}-${codec}.png`);
            await page.screenshot({ path: shot, clip: { x: 0, y: 0, width: W, height: H } });
            const px = samplePatches(decodePNG(readFileSync(shot)), W, XS, ROWS);

            report(name, codec, info, glErr, px, await page.evaluate('window.glRead'), await page.evaluate('window.c2Read'));
        } catch (e) {
            console.log(`\n=== ${name} / ${codec} === FAILED: ${e.message.split('\n')[0]}`);
            results.push({ engine: name, codec, error: e.message.split('\n')[0] });
        } finally {
            await browser?.close();
        }
    }
}

function report(engine, codec, info, glErr, px, glRead, c2Read) {
    console.log(`\n=== ${engine} / ${codec} ===`);
    console.log(`  negotiated: ${info.codec || 'unknown'}   requested: ${info.requested}`);
    console.log(`  video: ${JSON.stringify(info.videoSize)} readyState=${info.readyState} t=${info.currentTime?.toFixed?.(2)}`);
    if (info.fatal) console.log(`  FATAL: ${info.fatal}`);
    if (info.playError) console.log(`  play(): ${info.playError}`);
    if (glErr) console.log(`  WebGL error: ${glErr}`);
    console.log(`  screenshot scale: ${px.scale}x`);
    const worst = (name) => Math.max(...XS.map((_, i) => maxDelta(px.video[i], px[name][i])));
    for (let i = 0; i < XS.length; i++) {
        console.log(`  patch${i}: video=${px.video[i].join(',').padEnd(12)} 2d=${px.canvas2d[i].join(',').padEnd(12)} gl=${px.webgl[i].join(',').padEnd(12)} glNoConv=${px.webglNoConv[i].join(',').padEnd(12)} bitmap=${px.bitmap2d[i].join(',').padEnd(12)} glFromBitmap=${px.webglBitmap[i].join(',').padEnd(12)}`);
    }
    const maxGL = worst('webgl'), max2D = worst('canvas2d');
    const maxGLNC = worst('webglNoConv'), maxBM = worst('bitmap2d'), maxGLBM = worst('webglBitmap'), maxGLCV = worst('webglCanvas');
    console.log(`  MAX DELTA (0-255):  canvas2d=${max2D}  webgl=${maxGL}  webglNoConv=${maxGLNC}  bitmap2d=${maxBM}  webglFromBitmap=${maxGLBM}  webglFromCanvas=${maxGLCV}`);
    console.log(`  readback glRead=${JSON.stringify(glRead)}`);
    console.log(`  readback c2Read=${JSON.stringify(c2Read)}`);
    results.push({ engine, codec, negotiated: info.codec, max2D, maxGL, maxGLNC, maxBM, maxGLBM, maxGLCV, scale: px.scale });
}

writeFileSync(path.join(DIR, 'live-results.json'), JSON.stringify(results, null, 2));
console.log('\n' + JSON.stringify(results, null, 2));
