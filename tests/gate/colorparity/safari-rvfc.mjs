// Detached-element program audio on REAL Safari, via safaridriver.
import { createServer } from 'node:http';
import { readFileSync } from 'node:fs';
import { spawn } from 'node:child_process';
import path from 'node:path';

const DIR = path.dirname(new URL(import.meta.url).pathname);
const PORT = 8731;
const DRIVER_PORT = 4457;

const MIME = { '.html': 'text/html', '.mjs': 'text/javascript' };
const server = createServer((req, res) => {
    const file = path.join(DIR, decodeURIComponent(req.url.split('?')[0]).replace(/^\/+/, ''));
    let body;
    try { body = readFileSync(file); } catch { res.writeHead(404).end('nope'); return; }
    res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream' });
    res.end(body);
});
await new Promise((r) => server.listen(PORT, '127.0.0.1', r));

const driver = spawn('safaridriver', ['-p', String(DRIVER_PORT)], { stdio: 'ignore' });
await new Promise((r) => setTimeout(r, 2000));

const base = `http://127.0.0.1:${DRIVER_PORT}`;
async function wd(method, route, body) {
    const res = await fetch(base + route, {
        method, headers: { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body)
    });
    const json = await res.json();
    if (json.value && json.value.error) throw new Error(`${json.value.error}: ${json.value.message}`);
    return json.value;
}

let session;
try {
    session = await wd('POST', '/session', {
        capabilities: { alwaysMatch: {
            browserName: 'safari',
            'webkit:alwaysAllowAutoplay': true,
            'webkit:WebRTC': { DisableICECandidateFiltering: true }
        } }
    });
    const sid = session.sessionId;
    const exec = (script) => wd('POST', `/session/${sid}/execute/sync`, { script: `return ${script}`, args: [] });
    await wd('POST', `/session/${sid}/url`, { url: `http://127.0.0.1:${PORT}/rvfc.html` });

    // Safari throttles requestVideoFrameCallback to nothing in an occluded
    // window, so a background Safari measures zero frames and no blit cost.
    // Bring it to the front for the duration of the measurement.
    spawn('osascript', ['-e', 'tell application "Safari" to activate'], { stdio: 'ignore' });
    await new Promise((r) => setTimeout(r, 1500));

    const deadline = Date.now() + 180000;
    let ready = false;
    while (Date.now() < deadline) {
        if (await exec('window.ready === true')) { ready = true; break; }
        await new Promise((r) => setTimeout(r, 1000));
    }
    if (!ready) throw new Error('page never reported ready');
    const result = JSON.parse(await exec('JSON.stringify(window.result)'));
    console.log('\n=== safari / detached audio ===');
    console.log(JSON.stringify(result, null, 1));
    await wd('DELETE', `/session/${sid}`);
} catch (e) {
    console.log('\n=== safari / detached audio === FAILED: ' + e.message.split('\n')[0]);
    if (session) await wd('DELETE', `/session/${session.sessionId}`).catch(() => {});
}
driver.kill();
server.close();
process.exit(0);
