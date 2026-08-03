# Colour parity: what each render path does to the picture

Measured 2026-08-02 against a **live WebRTC stream**, on Chromium, real Firefox
and real Safari. This is the reference for any code that puts program pixels
somewhere other than the `<video>` element: the compositor, the loupe, the
scopes, the glass.

## Method

A real remote track, not a file: colour-bar canvas -> `captureStream(30)` ->
`RTCPeerConnection` pair -> `pc2.ontrack`. The codec is pinned with
`setCodecPreferences` to what the SFU actually negotiates, because measuring a
codec production never uses proves nothing:

- **H.264 Constrained Baseline** (`42001f` / `42e01f`) - program video
  (`internal/webrtc/whip.go:123`, `internal/webrtc/sfu.go:358`)
- **VP8** - screen share (`internal/webrtc/sfu.go:386`)

The page renders one frame several ways, is screenshotted, and patch pixels are
compared against the native `<video>`. Real Safari is driven through
`safaridriver` and real Firefox through WebDriver BiDi, because Playwright's
WebKit is a bundled build and is not Safari.

## Results (max channel delta vs native `<video>`, 0-255)

| Engine | Codec | Canvas 2D `drawImage` | WebGL from video | WebGL, `UNPACK_COLORSPACE_CONVERSION=NONE` | ImageBitmap -> 2D | WebGL from ImageBitmap | WebGL from 2D canvas |
|---|---|---|---|---|---|---|---|
| Chromium 1228 | H.264 | **0** | 4 | 4 | **0** | **0** | **0** |
| Chromium 1228 | VP8 | **0** | 2 | 2 | **0** | **0** | **0** |
| Firefox Nightly (Win) | H.264 | **0** | 0 | 0 | **0** | **0** | **0** |
| Firefox Nightly (Win) | VP8 | **0** | 0 | 0 | **0** | **0** | **0** |
| Safari 26.5.2 (macOS) | H.264 | **0** | 1 | 1 | **0** | **0** | **0** |
| Safari 26.5.2 (macOS) | VP8 | **0** | **39** | **39** | **0** | **0** | **0** |

## What this means for the code

**Canvas 2D `drawImage` is pixel-exact.** Every engine, both codecs, live
stream. Compositing program video through a 2D canvas does not change the
picture.

**The WebGL error is in the video-element upload, not in WebGL.** Feeding the
identical pipeline an `ImageBitmap` or a 2D canvas - same shader, same texture
format, same readback - is exact everywhere. Only the upload source matters.

**`UNPACK_COLORSPACE_CONVERSION_WEBGL = NONE` is not the fix.** It does not move
the number at all, on any engine. Do not reach for it.

So: **never `texImage2D` a video element** if the result is meant to represent
the footage. `LoupeOverlay.svelte` did, and on Safari magnified a screen share
39/255 off - a colour-inspection instrument lying about the picture. It now
uploads an `ImageBitmap`.

`videoGlass.ts` uploads a bitmap on its preferred path and falls back to the
video element, so its fallback is not colour-equivalent to its main path. Glass
is frosted and decorative, so that is cosmetic, but it is the reason the two
paths are not interchangeable.

The scopes are fine: `frameSource.ts` hands them an `ImageBitmap` which
`ScopesPanel.svelte` draws into a 2D canvas before `getImageData`.

## Limits of this measurement

- The source is a same-page peer-connection loopback, not the SFU.
  `pc2.ontrack` yields a genuine remote track through the real encoder and
  decoder, which is the property under test, but the transport is loopback and
  the encoder is the browser's rather than OBS's.
- **iOS is unmeasured** and deferred.
- Firefox on Linux (Playwright build) has no H.264 at all, not for files nor for
  WebRTC send or receive, which is why the Firefox H.264 rows come from real
  Firefox on Windows where the OpenH264 plugin exists.

## Two traps that cost debugging cycles

- **Safari throttles `requestAnimationFrame` to nothing in an occluded window.**
  Anything driving Safari headlessly or over SSH must wait on timers instead.
  Attestation-style frame counters rest on the same mechanism.
- `safaridriver` and Firefox's BiDi endpoint both refuse a second session while
  a previous browser process survives. Kill leftovers before re-running.

Harness and raw numbers: `/tmp/keep-colortest/` (`live.html`, `runlive.mjs`,
`safari.mjs`, `firefox-bidi.mjs`, `RESULTS-live.md`).
