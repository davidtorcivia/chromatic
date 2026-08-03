# Compositor validation gate

The harness behind the gate in `docs/WATERMARKING_PLAN.md`. It answers three
questions the watermark compositor rests on, against a **live WebRTC stream**
rather than a file:

| Page | Question |
|---|---|
| `live.html` | Does a canvas render program pixels identically to the `<video>`? |
| `audio.html` | Does program audio survive on a detached element? |
| `rvfc.html` | Does `requestVideoFrameCallback` fire on a detached element, and what does the 4K blit cost? |

Results are recorded in `docs/COLOR_PARITY.md` and `docs/WATERMARKING.md`.

## Why a peer-connection loopback

Each page builds a real remote track — canvas -> `captureStream` ->
`RTCPeerConnection` pair -> `pc2.ontrack` — with the codec pinned by
`setCodecPreferences` to what the SFU negotiates (H.264 Constrained Baseline for
program video, VP8 for screen share). A file-backed `<video>` takes a different
decode path, which is the limitation this harness exists to close.

It is not the SFU: transport is loopback and the encoder is the browser's rather
than OBS's. Codec and profile are production values, so the colour-conversion
path under test is the right one.

## Running

Serve this directory and point a runner at it.

```sh
python3 -m http.server 8731    # from this directory
node runlive.mjs               # Chromium + Firefox, colour parity
node runaudio.mjs              # Chromium + Firefox, detached audio
node runrvfc.mjs               # Chromium + Firefox, rVFC + blit cost
```

Real Safari (macOS) — Playwright's WebKit is a bundled build and is not Safari,
so these drive `safaridriver` directly. Copy the pages plus `png.mjs` and the
matching runner to the Mac; each runner serves the directory itself:

```sh
node safari.mjs                # colour parity
node safari-audio.mjs          # detached audio
node safari-rvfc.mjs           # rVFC + blit cost
```

Real Firefox (Windows) — driven over WebDriver BiDi so nothing has to be
installed on the host:

```sh
FIREFOX="C:\Program Files\Firefox Nightly\firefox.exe" \
GMP_SOURCE="<profile dir holding gmp-gmpopenh264>" \
node firefox-bidi.mjs          # colour parity
node firefox-audio.mjs         # detached audio
```

`GMP_SOURCE` matters: Firefox does WebRTC H.264 through the OpenH264 plugin,
which only exists in a profile that has downloaded it. The runner copies just
that directory into a throwaway profile rather than using the real one.

## Traps that will cost you an hour

- **Playwright's Firefox on Linux has no H.264 at all** — not for files, not for
  WebRTC send or receive. Firefox H.264 rows have to come from a real Firefox.
- **Headless browsers render in software.** Blit timings from a headless run are
  worst-case, not what a reviewer's GPU does. Resolution-dependent claims need a
  headed session.
- **This Linux box has no audio device**, so its `AudioContext` never leaves
  `suspended` and audio energy reads zero. Measure audio on the Mac or Windows.
- **Headless Firefox needs autoplay prefs** (`media.autoplay.default=0`,
  `media.autoplay.blocking_policy=0`, `media.autoplay.block-webaudio=false`) or
  `play()` throws and the run reads as "detached audio is broken".
- **`safaridriver` and Firefox's BiDi endpoint refuse a second session** while a
  previous browser process survives. Kill leftovers before re-running.
- **A Safari driven over SSH reports the page `hidden`**, which stops `rAF` and
  `rVFC`. Frame-rate and blit numbers cannot be taken that way; wait on timers,
  not `rAF`, anywhere the harness must make progress.
- **`inbound-rtp` does not exist until packets flow.** Sampling before that reads
  null and looks like silence.

`png.mjs` is a dependency-free PNG reader: the Mac has no ffmpeg and installing a
decoder on three machines was worse than 60 lines.
