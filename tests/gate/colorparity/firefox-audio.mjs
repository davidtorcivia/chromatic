// Live colour parity on real Firefox, driven over WebDriver BiDi.
//
// Firefox is launched directly with --remote-debugging-port, so nothing has to
// be installed on the host: no Playwright, no geckodriver. Node 22's global
// WebSocket speaks BiDi fine.
//
// Firefox does WebRTC H.264 through the OpenH264 GMP plugin, which only exists
// in a profile that has downloaded it. Rather than run in the user's real
// profile, this copies just that plugin directory into a throwaway profile.
import { createServer } from 'node:http';
import { readFileSync, writeFileSync, mkdtempSync, cpSync, existsSync, readdirSync } from 'node:fs';
import { spawn } from 'node:child_process';
import { tmpdir } from 'node:os';
import path from 'node:path';


const DIR = path.dirname(new URL(import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1'));
const PORT = 8731;
const DEBUG_PORT = 9222;
const W = 800, H = 600;
const XS = [80, 240, 400, 560];
const ROWS = { video: 180, webgl: 540, canvas2d: 900, webglNoConv: 1260, bitmap2d: 1620, webglBitmap: 1980, webglCanvas: 2340 };
const CODECS = (process.env.CODECS || 'H264,VP8').split(',');
const FIREFOX = process.env.FIREFOX;
const GMP_SOURCE = process.env.GMP_SOURCE; // profile dir holding gmp-gmpopenh264

if (!FIREFOX || !existsSync(FIREFOX)) throw new Error(`set FIREFOX to the firefox binary (got ${FIREFOX})`);

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

const profile = mkdtempSync(path.join(tmpdir(), 'ffparity-'));
let gmpVersion = null;
if (GMP_SOURCE && existsSync(path.join(GMP_SOURCE, 'gmp-gmpopenh264'))) {
    const src = path.join(GMP_SOURCE, 'gmp-gmpopenh264');
    cpSync(src, path.join(profile, 'gmp-gmpopenh264'), { recursive: true });
    gmpVersion = readdirSync(src).filter((d) => /^\d/.test(d)).sort().pop() || null;
}
writeFileSync(path.join(profile, 'user.js'), [
    'user_pref("media.peerconnection.video.h264_enabled", true);',
    'user_pref("media.gmp-gmpopenh264.enabled", true);',
    gmpVersion ? `user_pref("media.gmp-gmpopenh264.version", "${gmpVersion}");` : '',
    'user_pref("media.gmp-manager.updateEnabled", false);',
    // Without these the element never plays and the AudioContext stays
    // suspended, which reads as "detached audio is broken" when it is only the
    // autoplay policy.
    'user_pref("media.autoplay.default", 0);',
    'user_pref("media.autoplay.blocking_policy", 0);',
    'user_pref("media.autoplay.block-webaudio", false);',
    'user_pref("media.autoplay.block-event.enabled", false);',
    'user_pref("media.navigator.permission.disabled", true);',
    'user_pref("browser.shell.checkDefaultBrowser", false);',
    'user_pref("datareporting.policy.dataSubmissionEnabled", false);',
    'user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);'
].filter(Boolean).join('\n'));
console.log(`profile ${profile}  openh264=${gmpVersion || 'none'}`);

const ff = spawn(FIREFOX, [
    '-profile', profile,
    '--no-remote',
    '--headless',
    '--remote-debugging-port', String(DEBUG_PORT),
    'about:blank'
], { stdio: ['ignore', 'pipe', 'pipe'] });

const wsUrl = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('BiDi endpoint never announced')), 60000);
    const scan = (chunk) => {
        const text = chunk.toString();
        process.stdout.write(`[ff] ${text}`);
        const m = /(ws:\/\/[^\s]+)/.exec(text);
        if (m) { clearTimeout(timer); resolve(m[1]); }
    };
    ff.on('error', (e) => reject(new Error('spawn failed: ' + e.message)));
    ff.on('exit', (c) => reject(new Error('firefox exited early, code ' + c)));
    ff.stderr.on('data', scan);
    ff.stdout.on('data', scan);
});
// Firefox announces the bare origin (ws://127.0.0.1:9222); the BiDi session
// endpoint lives at /session and connecting to the root just closes.
const bidiUrl = new URL(wsUrl).pathname === '/' ? wsUrl.replace(/\/?$/, '/session') : wsUrl;
console.log(`bidi ${bidiUrl}`);

const ws = new WebSocket(bidiUrl);
await new Promise((r, j) => { ws.onopen = r; ws.onerror = (e) => j(new Error('ws failed: ' + e.message)); });

let nextId = 1;
const pending = new Map();
ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) {
        const { resolve, reject } = pending.get(msg.id);
        pending.delete(msg.id);
        msg.type === 'error' ? reject(new Error(`${msg.error}: ${msg.message}`)) : resolve(msg.result);
    }
};
function send(method, params = {}) {
    const id = nextId++;
    ws.send(JSON.stringify({ id, method, params }));
    return new Promise((resolve, reject) => pending.set(id, { resolve, reject }));
}

await send('session.new', { capabilities: { alwaysMatch: {} } });
const tree = await send('browsingContext.getTree', {});
const context = tree.contexts[0].context;
await send('browsingContext.setViewport', { context, viewport: { width: W, height: H } });


try {
    await send('browsingContext.navigate', {
        context, url: `http://127.0.0.1:${PORT}/audio.html`, wait: 'complete'
    });
    const evaluate = async (expression) => {
        const r = await send('script.evaluate', {
            expression, target: { context }, awaitPromise: true, resultOwnership: 'none'
        });
        return r.result?.value ?? null;
    };
    const deadline = Date.now() + 120000;
    let ready = false;
    while (Date.now() < deadline) {
        if (await evaluate('window.ready === true')) { ready = true; break; }
        await new Promise((r) => setTimeout(r, 1000));
    }
    if (!ready) throw new Error('page never reported ready');
    const result = JSON.parse(await evaluate('JSON.stringify(window.result)'));
    console.log('\n=== firefox / detached audio ===');
    console.log(JSON.stringify(result, null, 1));
} catch (e) {
    console.log('\n=== firefox / detached audio === FAILED: ' + e.message.split('\n')[0]);
}
ws.close();
ff.kill();
server.close();
process.exit(0);
