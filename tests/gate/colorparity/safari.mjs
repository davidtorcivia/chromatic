// Live colour parity on REAL Safari, driven through safaridriver (W3C
// WebDriver). Playwright's WebKit is a bundled build and is weak evidence for
// Safari, which is the engine the spec calls out as entirely unvalidated.
//
// Serves the harness itself on 127.0.0.1 so the page is a secure context
// (RTCPeerConnection needs one) with no network exposure and no python
// dependency on the Mac.
import { createServer } from 'node:http';
import { readFileSync } from 'node:fs';
import { spawn } from 'node:child_process';
import path from 'node:path';

import { decodePNG, maxDelta } from './png.mjs';

const DIR = path.dirname(new URL(import.meta.url).pathname);
const PORT = 8731;
const DRIVER_PORT = 4455;
const XS = [80, 240, 400, 560];
const SAMPLE_Y = 180;
const CODECS = (process.env.CODECS || 'H264,VP8').split(',');
const ELEMENT_KEY = 'element-6066-11e4-a52e-4f735466cecf';

const MIME = { '.html': 'text/html', '.mjs': 'text/javascript', '.js': 'text/javascript' };
const server = createServer((req, res) => {
    const file = path.join(DIR, decodeURIComponent(req.url.split('?')[0]).replace(/^\/+/, ''));
    let body;
    try {
        body = readFileSync(file);
    } catch {
        res.writeHead(404).end('nope');
        return;
    }
    res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream' });
    res.end(body);
});
await new Promise((r) => server.listen(PORT, '127.0.0.1', r));

const driver = spawn('safaridriver', ['-p', String(DRIVER_PORT)], { stdio: 'ignore' });
await new Promise((r) => setTimeout(r, 2000));

const base = `http://127.0.0.1:${DRIVER_PORT}`;
async function wd(method, route, body) {
    const res = await fetch(base + route, {
        method,
        headers: { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body)
    });
    const json = await res.json();
    if (json.value && json.value.error) throw new Error(`${json.value.error}: ${json.value.message}`);
    return json.value;
}

const results = [];

for (const codec of CODECS) {
    let session;
    try {
        session = await wd('POST', '/session', {
            capabilities: {
                alwaysMatch: {
                    browserName: 'safari',
                    // Without this the video never starts and window.ready never flips.
                    'webkit:alwaysAllowAutoplay': true,
                    // Safari filters host ICE candidates absent a getUserMedia
                    // grant, which stops the loopback connecting at all.
                    'webkit:WebRTC': { DisableICECandidateFiltering: true }
                }
            }
        });
        const sid = session.sessionId;
        const exec = (script) => wd('POST', `/session/${sid}/execute/sync`, { script: `return ${script}`, args: [] });

        await wd('POST', `/session/${sid}/url`, { url: `http://127.0.0.1:${PORT}/live.html?codec=${codec}` });

        const deadline = Date.now() + 60000;
        let ready = false;
        while (Date.now() < deadline) {
            if (await exec('window.ready === true')) { ready = true; break; }
            await new Promise((r) => setTimeout(r, 500));
        }
        if (!ready) throw new Error('page never reported ready');

        const info = await exec('window.info');
        const glErr = await exec('window.glErr');
        const glRead = await exec('window.glRead');
        const c2Read = await exec('window.c2Read');

        // Element screenshots rather than one viewport capture: the page is
        // 1080px tall and Safari's viewport screenshot is viewport-only, so a
        // short window would silently crop the rows being compared.
        const shots = {};
        for (const [name, sel] of [['video', '#v'], ['webgl', '#gl'], ['canvas2d', '#c2'], ['webglNoConv', '#glnc'], ['bitmap2d', '#bm'], ['webglBitmap', '#glbm'], ['webglCanvas', '#glcv']]) {
            const el = await wd('POST', `/session/${sid}/element`, { using: 'css selector', value: sel });
            const b64 = await wd('GET', `/session/${sid}/element/${el[ELEMENT_KEY]}/screenshot`);
            const png = decodePNG(Buffer.from(b64, 'base64'));
            const scale = png.width / 640;
            if (!Number.isInteger(scale) || scale < 1 || scale > 3) {
                throw new Error(`unexpected element screenshot scale ${scale} (${png.width}x${png.height})`);
            }
            shots[name] = XS.map((x) => {
                const i = (Math.round(SAMPLE_Y * scale) * png.width + Math.round(x * scale)) * png.channels;
                return [png.data[i], png.data[i + 1], png.data[i + 2]];
            });
            shots[name + 'Scale'] = scale;
        }

        console.log(`\n=== safari / ${codec} ===`);
        console.log(`  negotiated: ${info.codec || 'unknown'}   requested: ${info.requested}`);
        console.log(`  video: ${JSON.stringify(info.videoSize)} readyState=${info.readyState} t=${info.currentTime}`);
        if (info.fatal) console.log(`  FATAL: ${info.fatal}`);
        if (info.playError) console.log(`  play(): ${info.playError}`);
        if (glErr) console.log(`  WebGL error: ${glErr}`);
        console.log(`  element screenshot scale: ${shots.videoScale}x`);
        const worst = (name) => Math.max(...XS.map((_, i) => maxDelta(shots.video[i], shots[name][i])));
        for (let i = 0; i < XS.length; i++) {
            console.log(`  patch${i}: video=${shots.video[i].join(',').padEnd(12)} 2d=${shots.canvas2d[i].join(',').padEnd(12)} gl=${shots.webgl[i].join(',').padEnd(12)} glNoConv=${shots.webglNoConv[i].join(',').padEnd(12)} bitmap=${shots.bitmap2d[i].join(',').padEnd(12)} glFromBitmap=${shots.webglBitmap[i].join(',').padEnd(12)}`);
        }
        const maxGL = worst('webgl'), max2D = worst('canvas2d');
        const maxGLNC = worst('webglNoConv'), maxBM = worst('bitmap2d'), maxGLBM = worst('webglBitmap'), maxGLCV = worst('webglCanvas');
        console.log(`  MAX DELTA (0-255):  canvas2d=${max2D}  webgl=${maxGL}  webglNoConv=${maxGLNC}  bitmap2d=${maxBM}  webglFromBitmap=${maxGLBM}  webglFromCanvas=${maxGLCV}`);
        console.log(`  readback glRead=${JSON.stringify(glRead)}`);
        console.log(`  readback c2Read=${JSON.stringify(c2Read)}`);
        results.push({ engine: 'safari', codec, negotiated: info.codec, max2D, maxGL, maxGLNC, maxBM, maxGLBM, maxGLCV, scale: shots.videoScale });

        await wd('DELETE', `/session/${sid}`);
    } catch (e) {
        console.log(`\n=== safari / ${codec} === FAILED: ${e.message.split('\n')[0]}`);
        results.push({ engine: 'safari', codec, error: e.message.split('\n')[0] });
        if (session) await wd('DELETE', `/session/${session.sessionId}`).catch(() => {});
    }
}

console.log('\n' + JSON.stringify(results, null, 2));
driver.kill();
server.close();
process.exit(0);
