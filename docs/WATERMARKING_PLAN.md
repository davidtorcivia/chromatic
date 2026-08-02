# Watermarking — implementation plan

Companion to `WATERMARKING.md` (the what and why). This is the how: phases,
files, signatures, perf budget, and the test gate.

Design rule running through all of it: **one mark definition, four renderers.**
The display compositor, the loupe, the Go frame-grab path and the admin preview
must all produce the same mark from the same inputs, or forensic attribution
falls apart the moment two of them disagree. The rule takes effect when Phase 1
lands — Phase 0 ships earlier with a deliberately standalone stamp (see below)
and is re-based onto the shared engine afterwards.

---

## Phase 0 — Close the frame-grab hole (ships first, independent)

`grabFrame()` at `session/+page.svelte:2263` does:

```js
canvas.width = videoElement.videoWidth;      // native, e.g. 3840
canvas.getContext("2d").drawImage(videoElement, 0, 0);
canvas.toBlob(resolve, "image/jpeg", 0.92);  // → uploadFile → chat → everyone
```

That is a **clean, full-resolution frame in persistent storage, reachable by
clicking a button.** No devtools required. It is a worse hole than the DOM
overlay this project is being built to fix, and it is independent of the Safari
gate that blocks everything else — so it ships first.

### Watermark on *serve*, per requester — not only at upload

Marking only at upload misattributes: Alice grabs, Bob downloads, Bob leaks,
the frame says Alice. Since the whole point of server-signed marks (`§4` of the
spec) is preventing exactly that, the grab path must not reintroduce it.

- **At upload:** burn `captured by <grabber>` into the stored JPEG. Provenance.
- **At serve:** burn `downloaded by <requester> · <timestamp>` per request, in
  `Download` (`files.go:301`). `authorizeParticipant` (`files.go:123`) already
  resolves the requester.
- **Thumbnails stay unmarked.** `Thumbnail` (`files.go:495`) caps at 200×200
  (`files.go:31`) and is pre-generated to disk at upload (`:269`). Multi-line
  identity text at 200px would obliterate the preview — a "same level or
  better" violation — and per-requester marking would mean regenerating on
  every request. A 200px thumbnail is not a meaningful leak surface for a 4K
  frame. Explicit decision, not an oversight.

Both are affordable: `files.go` already imports `image/jpeg`, `image/png` and
`golang.org/x/image/draw` for thumbnails, so decode/composite/encode is an
addition to an existing pipeline, not a new dependency. Text needs a font —
`golang.org/x/image/font/opentype` + `golang.org/x/image/font/gofont/goregular`,
both inside the `golang.org/x/image` module already in `go.mod:32` (its
`// indirect` marker is stale — `files.go` imports it directly; `go mod tidy`
will fix the marker).

**Phase 0 does not wait for Phase 1's mark engine.** The upload/serve stamp is
a plain tiled text composite implemented locally in the files pipeline: the
requester's name + participant short-ID + timestamp, tiled diagonally at low
opacity. No drift, no seed — a stamped still has nothing temporal to
reproduce. When Phase 1 lands, re-base this composite onto
`internal/watermark/mark.go` so the grab path becomes one of the four shared
renderers; until then the standalone stamp is correct and complete for this
phase.

### Distinguish grabbed frames from ordinary uploads

A reference PDF or a client's brand deck must not get stamped. The
`frame-${slug}-${stamp}.jpg` filename is too flimsy to key security behavior on.

Migration `010_add_file_origin.sql`:

```sql
ALTER TABLE files ADD COLUMN origin TEXT NOT NULL DEFAULT 'upload';
-- 'upload' | 'frame-grab'
```

Set `origin` from a **distinct route** — `POST /api/rooms/{slug}/grab` — rather
than a flag on the generic upload, so security behavior never keys off a
client-supplied field. Serve-time marking keys off `origin` plus the room's
watermark mode. Existing rows default to `'upload'` and are untouched.

A dishonest client could still capture a frame itself and upload it as an
ordinary file, dodging the stamp. That is already inside the conceded threat
model (spec §1: anyone writing custom JS reaches clean pixels), and it does not
weaken the point of Phase 0 — which is closing a hole reachable by *clicking a
button*, no devtools involved.

Cost: one decode + composite + encode per download of a grabbed frame. Cache
per `(file_id, participant_id)` on disk beside the thumbnail if it shows up in
profiling; do not pre-optimize.

### Phase 0 also fixes the response headers

Serve marked grabs with `Cache-Control: private, no-store` — a shared cache
handing Bob a copy marked for Alice would undo the whole thing.

---

## Phase 1 — The mark engine (one definition, two languages)

`web/src/lib/video/mark.ts` and `internal/watermark/mark.go`.

The mark is a **pure function**, not a cached bitmap. Four consumers need it at
four resolutions and transforms; a single cached layer cannot serve them, and a
cached layer fights drift (which would force constant re-rasterization).

```ts
export interface MarkSpec {
  token: string;        // server-signed, opaque; rendered verbatim
  lines: string[];      // name · participant short-ID · room · UTC clock ·
                        // server-expanded watermarkText template. NOTE: no
                        // email — models.Participant has none.
  seed: string;         // session ID — drives drift deterministically
  opacity: number;      // room config
  scale: number;        // room config
}

/** Rasterize ONE tile at the given device scale. Cheap; called on text
 *  change (clock ticks ~1Hz) or DPR change, never per frame. */
export function renderTile(spec: MarkSpec, dpr: number): HTMLCanvasElement;

/** Drift offset at server time t. Deterministic from seed — the same
 *  session at the same server second produces the same offset in JS and in
 *  Go, which is what makes a leaked still forensically locatable.
 *  MUST be server time, not Date.now(): a few seconds of client clock skew
 *  against a 30-60s drift period puts the forensic position in the wrong
 *  place. The mark token carries a server timestamp; the client keeps the
 *  offset and feeds corrected time in here. */
export function driftOffset(seed: string, serverTMs: number): { dx: number; dy: number };

/** Paint the mark over `ctx` covering `rect`, at `sourceScale`
 *  (1 = native, <1 = display-res composite, >1 = loupe magnification). */
export function paintMark(
  ctx: CanvasRenderingContext2D,
  spec: MarkSpec, rect: DOMRect, sourceScale: number, tMs: number,
): void;
```

`paintMark` is `createPattern(tile, "repeat")` + one `fillRect`.
**Tiling and drift become a transform, not a rasterization** — that is the
whole acceleration story for the mark layer, and it is what lets the same tile
serve the display path and the loupe at different zooms.

Put the rotation/scale/drift on **`pattern.setTransform(new DOMMatrix())`, and
leave the context transform at identity.** Rotating the *context* and then
filling means filling a rotated region, which leaves uncovered corners and
tempts a bounding-box enlargement that wastes fill area. The pattern-transform
form fills the true rect with a rotated pattern. Non-obvious API; noted so no
one loses a day to it.

Go mirrors `driftOffset` and the tile layout bit-for-bit. A shared JSON fixture
(`testdata/mark_vectors.json`) is asserted by both `mark.test.ts` and
`mark_test.go` so the two implementations cannot drift apart silently.

**What "bit-for-bit" covers — and a trap to avoid.** The determinism contract
is *geometry*: tile pitch, rotation, and `driftOffset(seed, t)`. It is not
glyph rasterization — the browser's font and `gofont/goregular` will never
match pixel-wise, and don't need to; forensics locates the mark by geometry
and reads it by eye. The trap: **tile pitch and layout must be computed from
`MarkSpec` fields and `dpr` only — never from `measureText`.** Text metrics
vary by platform and font fallback; a `measureText`-derived pitch is
irreproducible in Go and silently breaks the shared vectors. Fix the tile box
size from `scale`, and ellipsize text to fit it.

### Attack resistance (the "resistant to simple attacks" requirement)

| Attack | Countermeasure |
|---|---|
| Crop to the unmarked region | Tiled across the whole frame, diagonal |
| Temporal averaging over motion | Seeded drift — never a fixed pixel |
| Single-direction level correction | Tile draws a light stroke *and* a dark shadow, so it modulates luminance both ways; no gamma/contrast move removes both |
| Color-keying a flat mark | Alpha-composited over content, so mark pixels are content-dependent; no constant color to key |
| Inpainting / generative removal | Coverage is frame-wide, so removal destroys real picture content |
| Screenshot at a single instant | Every tile carries the full identity — one tile is enough to attribute |

Drift period ~30–60s, amplitude ≥ one tile pitch. Long enough not to distract
during a review, far enough to defeat averaging.

---

## Phase 2 — The display compositor

`web/src/lib/video/markedCanvas.ts`, generalized from `bindCanvasStream`
(`streamBinding.ts:108`), which already proves this pattern in production:
detached `<video>`, `requestVideoFrameCallback`, `ResizeObserver`-cached
backing-store size, full cleanup.

**Detached, never inserted.** `document.createElement("video")` with no parent
node — not `display: none`. A hidden-but-present element still offers the
native context menu and PiP an affordance, and some engines deprioritize
decode for hidden elements. Detaching moots both. (The program `<video>` at
`session/+page.svelte:3082` carries no `disablepictureinpicture` today, so PiP
on clean pixels is live right now; detaching closes it.)

Per displayed frame, two operations (three when a logo is configured):

1. `drawImage(hiddenVideo, …)` — the frame, downscaled on the GPU.
2. `paintMark(ctx, …)` — one `fillRect` with the cached pattern.
3. `drawImage(logoLayer, …)` — only for rooms with `watermarkMode`
   `logo`/`both`. The logo is branding, not identity: static position from
   room config (`WatermarkLogoPath`/`Position`/size), no drift, cached as an
   offscreen layer. Today's `WatermarkOverlay.svelte` renders it; retiring
   that overlay without this draw silently strips a client's brand bug from
   every `logo`/`both` room. The identity mark (op 2) renders regardless of
   which mode the room chose, whenever watermarking is enabled.

**Composite at display resolution**, CSS px × `min(devicePixelRatio, 2)`, never
at the video's intrinsic 4K. Screen capture happens at display resolution
anyway, so nothing is lost, and a 4K session composites at ~1080p.

**Reproduce `object-fit: contain` in the compositor.** Today the CSS rule
`.video-container video { object-fit: contain; background: #000 }`
(`+page.svelte:~4173`) does the letterboxing, and every geometry consumer
(`coordinates.ts`) models exactly that fit. A canvas has no `object-fit`; the
compositor must compute the contain-fit destination rect itself (aspect-fit
the source into the canvas, black bars around it) and **expose that content
rect** to consumers. This is why the coordinates change in Phase 3 simplifies:
the compositor owns the fit instead of re-deriving it from CSS behavior.

**A `CompositeSurface` handle, not loose element refs.** Consumers currently
take an `HTMLVideoElement` and read both its layout box and its intrinsic
size. After the split those live on two objects. Export one handle from
`markedCanvas.ts`:

```ts
export interface CompositeSurface {
  canvas: HTMLCanvasElement;      // on-screen; layout geometry, hit-testing
  video: HTMLVideoElement;        // detached; intrinsic size, rVFC, audio sink
  contentRect(): DOMRect;         // contain-fit rect within the canvas, CSS px
}
```

Phase 3 re-wires consumers to take this instead of the raw video element.
Without it, every consumer invents its own pairing and the silent-breakage
modes below multiply.

**Colour:** measured pixel-exact for Canvas 2D `drawImage` (spec §3). Do not
introduce a WebGL compositor — it measured 1–5/255 off.

**Latency budget:** rVFC fires on the decoded frame, so the composite lands in
the same or next compositor frame — ≤1 frame (~16ms). Measure and publish the
number; this product sells on latency.

**Firefox 4K watch item:** `frameSource.ts:9` records that Gecko does
video→canvas `drawImage` on the main thread. Profile at 4K→1080p before ship.
Mitigation if needed is capping composite resolution on Gecko. Note the
OffscreenCanvas-in-a-worker path is *not* the answer: it needs
`MediaStreamTrackProcessor`, which is Chromium-only — available precisely where
the problem isn't. Do not build it.

### Program audio rides on this element — treat it as a gate, not a detail

The program `<video>` is **also the program audio sink**. `muted={isMuted}`,
`toggleMute()` writes `videoElement.muted`, and output-device selection calls
`setSinkId` directly on it (`session/+page.svelte:2126`). Program audio purity
is a CLAUDE.md non-negotiable, and this plan moves that element to a detached
node.

`bindCanvasStream` is *not* precedent: it sets `video.muted = true` because
webcam pills carry no audio, so the detached-element audio path is untested
here. `new Audio()` is a detached element that plays fine in every engine,
which is encouraging but does not cover `setSinkId` on a detached node, or iOS
Safari's autoplay/suspend behavior.

Verify in all three engines **before Phase 2 design is locked**, alongside the
colour check:

- Stereo program audio plays from a detached element, bit-clean, no resample.
- `muted` toggling and volume behave identically.
- `setSinkId` still routes output (Chromium; the section is hidden elsewhere).
- No added A/V sync drift against the composited canvas.
- iOS Safari does not suspend the detached element's audio.

If any engine misbehaves, the fallback is splitting audio onto its own element
or an `AudioContext` graph — a real design change, which is why it belongs in
the gate rather than in the middle of Phase 2.

### Screen share

`screenShareVideoEl` (`session/+page.svelte:3043`) goes through the same
compositor with the same mark, sized to the split pane. Screen shares in review
sessions carry cuts and client comps; leaving it a bare video reproduces the
hole one element over.

---

## Phase 3 — Keep every tool working (the hard requirement)

Each consumer gets the **`CompositeSurface` from Phase 2** — canvas for layout
geometry and hit-testing, hidden video for intrinsic dimensions, media state,
and audio. The video reference stays alive; it just stops being on screen.

Why the geometry moves below are mandatory, not stylistic: a detached
element's `getBoundingClientRect()` returns all zeros. Left pointed at the
video, `getVideoContentPageRect` returns `null` (glass bails permanently at
`videoGlass.ts:345`, loupe never matches a surface at `LoupeOverlay:189`),
`getVideoContentRect` returns a zero-size rect (laser normalization yields NaN
and every sample is dropped at `LaserPointerOverlay:457`), and the laser's
`ResizeObserver` on the video never fires. All of it fails **silently**.

| Consumer | Change | Rationale |
|---|---|---|
| **ScopesPanel** (`:177`) | **Unchanged — keeps sampling the clean video** | A waveform/vectorscope reading mark pixels is a broken instrument. Measurement must see program pixels. This is "same level or better" in its strictest form. |
| **LoupeOverlay** (`:321`, `:358`) | Keeps `texImage2D(video)` at **native** res; adds `paintMark` at the lens's zoom/offset. Hit-testing (`:189`) and the `touchAction` writes (`:400-409`) move to the canvases. Covers **both surfaces**: it iterates `[shareElement, videoElement]` (`:187`), so it needs both hidden videos for pixels and both canvases for geometry, and marks each surface with that surface's spec. | Sampling the display-res composite would *lose* 4K detail — a regression. Native sampling + own mark keeps quality and closes the clean-window hole. Applies to both the WebGL path and the 2D fallback at `:358`. |
| **LaserPointerOverlay** — **two instances**: program (`:3095`) and share (`:3062`, `surface="share"`) | `getBoundingClientRect`, cursor, `touchAction`, the `pointerdown`/`loadedmetadata` listeners (`:232`, `:283`) and the `ResizeObserver` (`:239`) move to each instance's canvas | Pure geometry/interaction swap; overlay stays a DOM sibling (it's annotation, not content). Both instances, or the share pane's laser dies silently. |
| **videoGlass** (`:395`) | **Decision: stays clean** — including its direct `texImage2D(video)` fallback at `:415`. Its `getVideoContentPageRect` call (`:343`) moves to the canvas/`contentRect()`. | Refraction is frosted, distorted, and confined to the control-bar strip. Marking it would force a second `frameSource` capture, defeating the module's single-capture purpose, for a surface no one can read footage from. Accepted risk, recorded here rather than left silent. |
| **coordinates.ts** | `getVideoContentRect` / `getVideoContentPageRect` **change signature** to take the `CompositeSurface` (or `(canvas, video)`): element rect from the canvas, `videoWidth/Height` from the video. One element can no longer serve both. Callers to update: `LaserPointerOverlay:158/:446-447`, `LoupeOverlay:189`, `videoGlass:343`, plus `coordinates.test.ts`. (`clientToVideoCoords` / `videoToElementCoords` have zero production callers — update their tests or drop them.) | The old signatures assumed one element carried both layout and intrinsic size; after the split that is false by construction. The compositor owning contain-fit is what keeps the math simple. |
| **`grabFrame()`** (`:2262`; control bar `:3692`, More sheet, key **G**) | Unchanged — still guards on `readyState`/`videoWidth` and `drawImage`s the **clean hidden video** at native res | Deliberate: grabs are marked server-side per requester (Phase 0), which beats baking one viewer's mark into a stored frame. A detached video is a valid `drawImage` source, so this survives as-is. |
| **Stats loop + resolution readout** (`:2358-2409`, `:3832`) | Unchanged — rVFC metadata (`captureTime`/`receiveTime`/`processingDuration`) and `videoWidth/Height` keep coming from the detached element | Must report the *source* resolution and true stream timing, not the composite's. Verify rVFC metadata still populates on a detached element during the Phase 2 gate — same mechanism the compositor itself depends on. |
| **`captureVideoAspect` / rotate hint** (`:1168`, `:2722`) | Unchanged — reads intrinsic dimensions from the detached element | Pure property reads; no layout involved. |
| **Fullscreen** (`:2183`) | Unchanged (`documentElement`); **unhide the button on iPhone** | No native video element to promote, so nothing strips the mark. Gated on the iOS validation in Phase 2. |
| **`frameSource.ts`** | Stays clean and single-capture | Serves scopes and glass, both of which want clean pixels. |
| **Video readiness events** (`:3087-3089`) | `onplaying` / `onwaiting` / `onstalled` are bound in *markup* today; re-attach programmatically to the detached element. Same for the share element's `onpause` (WebKit hover-pause workaround, `:2176`) and `onplaying` (`:3051`). | `stream-overlay.ts` derives `isVideoPlaying` from these. Its header documents a bug where a viewer "could sit on 'the host hasn't started streaming yet' forever" — lose these listeners and that bug is back. |
| **`stream-overlay.ts` state machine** | Inputs unchanged; `hasStream` / `isVideoPlaying` now sourced from the detached element | Pure re-sourcing. Its existing tests must still pass untouched — that is the regression guard. |
| **Overlay mount gates** (`:3094`) | `{#if videoElement && hasStream && !needsPlayClick}` re-keys on the canvas being mounted | The detached element is created in script at init, so `videoElement` becomes truthy earlier than today; gating on it alone would mount overlays before there is a surface to attach to. |
| **Tap-to-play (`needsPlayClick`)** | `play()` targets the detached element (`:1129/:1137/:1294`, muted-fallback dance included); the click target becomes the canvas | Autoplay rejection still surfaces as `needs-click`. Room starts muted (`isMuted = true`) for autoplay compliance, so the unmute gesture path must be re-verified. |
| **Audio: controls + voice ducking** | `muted` (`:1136`, `:2194`), `setSinkId` (`:2126`) keep addressing the detached element. **`VoicePlaybackManager`** (`:1409`) also takes the element and is the sole writer of program `volume` (`voice-playback.ts:57`, driven by the stream-volume slider `:2203`) — it keeps the detached element. | See the Phase 2 audio gate above; if that gate fails, all of these move together to whatever the separate sink is. |
| **WatermarkOverlay.svelte** | Removed from the session page; retained for the admin preview | The preview legitimately wants a DOM overlay over a still. Its logo rendering moves into the compositor's logo layer (Phase 2, op 3). |

**Scopes see clean pixels by design.** That is not a new hole: the spec already
concedes the decoded stream is reachable from JS by anyone writing custom code.
It changes nothing about the threat model and it keeps the instrument correct.

---

## Phase 4 — Server authority and audit

- **Mark tokens** over the existing WebSocket: HMAC-signed, short TTL, re-issued
  periodically. Signing prevents a leaker rendering a colleague's name — the
  one cryptographically real property here. Concretely, so nothing is left to
  invent during implementation:
  - Payload: `{participantId, name, roomSlug, seed, issuedAtMs, ttlMs}` —
    JSON, HMAC-SHA256 over the serialized payload, sent as
    `payload.signature`. The hub's `Client` (`hub.go:62`) already carries
    ID/Name/RoomSlug, so issuance needs no new plumbing.
  - The issue message also carries `serverNowMs`; the client stores
    `serverNowMs - Date.now()` and feeds corrected time into `driftOffset` —
    this is the clock-offset channel the mark engine's comment requires.
  - Key: random 32 bytes generated on first boot and persisted (settings
    table or config file). It must survive restarts — a forensic analyst
    verifying a recorded token against the audit trail needs the key that
    signed it.
  - Re-issue at TTL/2 (e.g. 5-minute TTL, reissue ~2.5 min). Re-issue is also
    what advances the UTC clock line at the coarse level; the client renders
    the live clock locally but from server-corrected time.
- **Migration `011_add_view_audit.sql`**: participant, room, mark token, issued
  and revoked timestamps, IP, user agent, attestation failure count.
- Exportable per room: *who saw this cut, when, under which mark.*
- Conventions: `database.WithTimeout(r.Context())` + the `*Context` method on
  every call, `rows.Err()` after every `rows.Next()`, one transaction for
  writes that must hold together.

---

## Phase 5 — Attestation and the gate

Report-only first. Client heartbeats, per composited surface (program, and
screen share when up): canvas presence, dimensions, rendered token, mark
coordinates, a frame counter proving rVFC is advancing, and
`document.visibilityState`. The server cannot observe visibility on its own —
the client must report it, and the server trusts it only in the lenient
direction (hidden ⇒ frame-freeze is excused; it never excuses a missing
canvas or wrong token).

- **Visibility-aware:** `visibilityState === "hidden"` suspends the
  frame-advance requirement. A backgrounded tab stops rVFC and would otherwise
  fail attestation honestly — reviewers losing video on alt-tab would be worse
  than the bug being prevented.
- **Budgeted:** 3 consecutive misses on a ~2s heartbeat (~6s grace), tuned
  against real data from the report-only period. The grace must also cover
  stream-swap transients — `bindCanvasStream` grew play-retry machinery
  because Firefox drops frames on attach, and renegotiation/repair pauses
  rVFC briefly on an honest client.
- **Cheap server-side check:** the server knows `seed` and server time, so it
  can recompute `driftOffset(seed, t)` and compare against the reported mark
  coordinates (within a tolerance for heartbeat latency). That turns the
  coordinate field from decoration into a real consistency check.
- **Lever:** `sub.VideoSender.ReplaceTrack(nil)` — **and the screen-share
  sender when one is up**; a gated viewer must not keep receiving the share.
  Verified against pion v4.2.1 (`rtpsender.go:233`): unbinds the track from
  that sender and returns — no renegotiation, no `negotiationneeded`, no
  teardown. Other subscribers on the shared `TrackLocalStaticRTP` are
  unaffected.
- **Restore:** `ReplaceTrack(videoTrack)` re-binds against the preserved context
  (`rtpsender.go:273-295`), then request a keyframe through the existing
  coalesced PLI path (`sfu.go:93`, `:195`) or the viewer sits black until the
  next natural IDR. Pion quirk to know before writing the test: if the
  restore's internal `Bind` fails, pion's error path calls
  `replacedTrack.Bind` on the track it replaced — which is nil after a
  nil-replace — and panics (`rtpsender.go:283-286`). Bind of the same track
  that was bound before should not fail, but do not add retry logic that
  assumes `ReplaceTrack`'s error path is benign here.
- Lock ordering if the gate touches SFU state: `SignalingMu` → `room.mu`.

---

## Perf budget

| | Cost |
|---|---|
| SFU per packet | **zero** — hot path untouched |
| Client per displayed frame | 1 `drawImage` (GPU downscale) + 1 patterned `fillRect` (+1 `drawImage` of the cached logo layer for `logo`/`both` rooms) |
| Mark rasterization | ~1/s (clock tick), not per frame |
| Added latency | ≤1 frame (~16ms), to be measured |
| Server per grab download | 1 JPEG decode + composite + encode |
| Scopes / glass | unchanged — still one shared `frameSource` capture |

---

## Test gate

Per CLAUDE.md: `go build ./...`, `golangci-lint run ./...`, `go test ./...`
(cgo present, so `handlers`/`database` run locally), and
`cd web && npx svelte-check && npx vitest run && npm run build`.

New tests:

1. **Mark determinism across languages** — `mark.test.ts` and `mark_test.go`
   both assert `testdata/mark_vectors.json`. Guards forensic reproducibility.
2. **Program-video purity** — composite a known frame; assert unmarked regions
   are bit-identical to source. Same spirit as the Opus fmtp locks.
3. **Go JPEG watermark golden test** — fixed input + fixed spec → stable output.
4. **pion `ReplaceTrack(nil)` round-trip** — nil stops RTP, restore rebinds and
   resumes, no renegotiation fired. The whole enforcement tier rests on this.
   Build it alongside the existing SFU tests with `ICELoopbackOnly: true`,
   like every other SFU test — no firewall prompts.
5. **File-origin authorization** — `origin='upload'` files are never stamped;
   `origin='frame-grab'` always are, and are stamped for the *requester*.
6. **Coordinate regressions** — existing `coordinates.test.ts`,
   `laser.test.ts` repointed at canvas geometry.
7. **Colour parity, live** — the harness (kept at `/tmp/keep-colortest`)
   re-run against a live WebRTC stream on Chromium, Firefox, and real Safari.
8. **Program audio on a detached element** — stereo bit-clean, `muted`/volume,
   `setSinkId` routing, A/V sync, iOS non-suspension. Gate item, all engines.
9. **Overlay state machine unchanged** — existing `stream-overlay.test.ts`
   passes untouched after the events move off markup, guarding the
   "waiting forever" regression its header documents.

E2E: `TEST_BASE_URL` must match the instance's `ALLOWED_ORIGINS`, and it creates
real rooms — delete any `test-*` rooms left behind.

---

## Sequencing and gates

```
Phase 0  grab hole (server-side, per-requester)   ← independent, ships now
   │
   ├─ Phase 1  mark engine (JS + Go, shared vectors)
   │      │
   │      ▼
   │   [GATE] on a live stream, all three engines:
   │      │     · colour parity (Canvas 2D vs <video>)
   │      │     · program audio on a detached element  ← can force a redesign
   │      │     · iOS drawImage from a hidden MediaStream video
   │      │   fail → pick (a)/(b)/(c) from spec §3 before any more code
   │      ▼
   │   Phase 2  compositor (flagged off)
   │      ▼
   │   Phase 3  tool re-wiring — no phase ships until every tool is re-verified
   │      ▼
   ├─ Phase 4  tokens + audit
   │      ▼
   └─ Phase 5  attestation report-only → gate on measured false-positive rate
```

Flags: `watermark.compositor` (Phase 2/3) and `watermark.enforce` (Phase 5),
independently switchable, so a compositor regression can be rolled back without
losing the audit trail or the Phase 0 fix.
