# Stream-embedded watermarking

Status: **Phases 0-1 implemented** (server-side frame-grab marking; the mark
engine). **The compositor gate is measured and passes on desktop** — see
"Validation gate results" below; iOS is deferred. Phases 2-5 pending.
Implementation plan in
[`WATERMARKING_PLAN.md`](./WATERMARKING_PLAN.md). **Phase numbers in this document
refer to the plan's Phase 0–5**; the section headers below name the plan phase
they correspond to. Supersedes the current DOM overlay
(`web/src/lib/components/WatermarkOverlay.svelte`), which is a `<canvas>`
sibling of the program `<video>` and is removed by deleting one DOM node.

Goal: a per-viewer mark that is part of the picture the viewer sees, that a
reviewer cannot switch off from devtools, and that survives the leak vector
that actually happens — someone pointing a phone at the monitor, or screen
recording the tab.

---

## 1. Threat model, stated before the design

Two unrelated products get called "watermarking". They do not share an
implementation and conflating them is how this feature goes wrong.

| | What it is | What it defeats | Cost |
|---|---|---|---|
| **Visible per-viewer burn-in** | Reviewer identity composited into displayed pixels | Screen recording, phone capture, casual removal | Low — client GPU only |
| **Invisible forensic mark** | Payload recovered from a leaked *file* after re-encode/crop | Redistribution of a downloaded copy | High — needs dual encoded ingest |

The ask — "embeds in the stream instead of one that can be removed via
inspect element" — is the first one. That is what the plan's Phases 0–5 build.
Forensic A/B marking is future work (§7) with its honest prerequisites stated;
it is not in the plan's phases, it is not the headline, and it should not be
sold as shipping sooner.

### What is achievable, and what is not

Chromatic is a browser client receiving a WebRTC MediaStream. **There is no
way to prevent a determined engineer with devtools from recording clean
pixels.** `pc.getReceivers()[0].track` and `video.captureStream()` hand out
the decoded stream, and nothing short of EME/Widevine L1 (a different product,
incompatible with WHIP ingest and with the color pipeline) changes that.

So state the security claim precisely, and put it in the sales material in
these words:

- **Removal defeated:** deleting the overlay node, `opacity: 0`, blocking the
  request, Picture-in-Picture, native fullscreen, extensions that hide
  elements — none of these yields unmarked pixels once the canvas *is* the
  picture.
- **Capture attributed, not prevented:** screen recording, phone-camera
  capture, and screenshots still leak the picture — but the leaked copy
  carries the viewer's mark, which is the property being sold. Do not phrase
  these as "defeated" to a buyer.
- **Not defeated:** a person who writes custom JavaScript against the page.
- **Mitigation for that person:** they had to authenticate to receive the
  stream at all, and §6's audit trail records that they did. Attribution, not
  prevention, is the control that covers them — and attribution is what
  corporate legal actually asks for.

Overclaiming here is the failure mode. A buyer who is told "unremovable" and
then sees a devtools bypass loses trust in the whole product.

---

## 2. Rejected: server-side burn-in

The SFU is a pure RTP forwarder. `internal/webrtc/sfu.go` fans one shared
`TrackLocalStaticRTP` out to every subscriber; nothing is ever decoded. Burning
a per-viewer mark server-side means decode + composite + **re-encode per
subscriber**:

- ~1–2 cores per 1080p60 H.264 subscriber (x264 veryfast); 4K is several times
  worse. Ten reviewers on a 4K session is not a machine you want to rent.
- Adds a full encode + GOP of latency to a product whose axiom is latency.
- A generational re-encode of program video, which is a direct violation of the
  quality axiom. Colorists will see it.
- Introduces an ffmpeg/NVENC dependency the repo currently does not have.

Hardware NVENC removes the CPU argument but not the latency or the generation
loss. Rejected on all three.

The corollary is the design's best property: **the server does zero per-frame
work.** The SFU hot path is untouched.

---

## 3. Render path (plan Phases 1–3)

**The program `<video>` element stops being the thing on screen.** It becomes a
hidden source feeding a `<canvas>` that composites frame + mark. Delete the
canvas and you have deleted the picture.

This is not a new mechanism in this codebase. `bindCanvasStream` in
`web/src/lib/video/streamBinding.ts:108` already does exactly this for webcam
pills — hidden `<video>`, `requestVideoFrameCallback`-driven `drawImage`,
`ResizeObserver`-cached backing-store size, full cleanup path. The watermark
compositor is that action generalized to the program stream plus a mark layer.

### Compositing API: Canvas 2D, not WebGL

Measured, not assumed. A standalone harness rendered the same H.264 (BT.709)
and VP9 clip three ways on one page — native `<video>`, WebGL2 passthrough
blit, Canvas 2D `drawImage` — screenshotted the page and compared patch pixels:

| Engine | Codec | `drawImage` Δ vs `<video>` | WebGL Δ vs `<video>` |
|---|---|---|---|
| Chromium 1228 | H.264 | **0 / 255** | 1 / 255 |
| Chromium 1228 | VP9 | **0 / 255** | 5 / 255 |
| Firefox (headless) | VP9 | **0 / 255** | no WebGL2 in harness |

Canvas 2D `drawImage` is **pixel-exact**. WebGL passthrough is not — up to
5/255 on a saturated channel, which is a color-conversion difference, not
filtering. In a color-critical tool that is a regression, and it rules WebGL
out as the compositor even though `gl.ts` is right there.

Headless Playwright Firefox exposes no WebGL2, so the Firefox WebGL row is
untaken rather than failing — but that is itself informative: the compositor
must not depend on WebGL being available, because the existing glass renderer
already has a silent no-WebGL fallback path.

That limitation is now closed: see "Validation gate results" below. Canvas 2D
`drawImage` measures pixel-exact against a live WebRTC stream on real Safari and
real Firefox too, and the WebGL gap turns out to be the video-element upload
rather than WebGL itself (`docs/COLOR_PARITY.md`).

### If Safari fails, that is a product decision, not a fallback

Film and agency reviewers are heavily on Macs, so this branch has revenue
consequences and needs an owner's answer before step 1 of §8 is run, not after:

- **(a) Safari composites correctly** → ship as written.
- **(b) Safari fails, and marked rooms go Chromium/Firefox-desktop-only** →
  §5's gate means Safari users get *no video* in a marked room. Defensible for
  a security feature, but it must be stated to buyers up front and surfaced in
  the room UI at join time, not discovered mid-review.
- **(c) Safari fails and (b) is unacceptable** → Phase 1 needs a different
  design, and this spec does not cover it. Do not ship the DOM overlay to
  Safari as a silent substitute: a room where the mark is enforced for some
  viewers and cosmetic for others is worse than no feature, because the audit
  trail then implies a guarantee it did not deliver.

The same three-way applies to iOS independently of macOS.

### Composite at display resolution, not source resolution

Size the canvas backing store to CSS pixels × `min(devicePixelRatio, 2)`,
exactly as `bindCanvasStream` does — never to the video's intrinsic 4K.
`drawImage` downscales on the GPU during the blit, so a 4K session composites
at ~1920×1080, not 3840×2160. Since any screen capture is at display
resolution anyway, marking at display resolution loses nothing.

### Per-frame cost

Two `drawImage` calls:

1. `drawImage(hiddenVideo, …)` — the frame.
2. one `fillRect` through a cached `createPattern` tile — drift and rotation
   live on the *pattern's* transform (plan Phase 1), so the tile re-rasterizes
   only on a text change (~1 Hz clock tick) or DPR change, never per frame. A
   third `drawImage(logoLayer, …)` runs only when the room configures a logo
   watermark (see §4).

Driven by `requestVideoFrameCallback`, so it fires once per *produced* frame,
never on the display's 120Hz cadence — per the frontend loop convention in
CLAUDE.md, and matching `streamBinding.ts:213`. rAF fallback gated to ~30fps
for engines without rVFC.

Latency budget: rVFC fires when the decoded frame is ready, so the composite
lands in the same or the next compositor frame — **≤1 frame (~16ms)** added
versus the native video path. Measure it; if it consistently costs a frame,
that number goes in the docs, because this product sells on latency.

Known cost to watch: `frameSource.ts:9` records that Firefox does
video→canvas `drawImage` on the main thread. At 4K→1080p that blit is the one
thing that could make this expensive on Gecko. Profile it before shipping and
be prepared to cap the composite resolution further there.

### Knock-on: the loupe must mark its own output

`web/src/lib/glass/frameSource.ts` samples the *video element*. Left alone,
the loupe becomes a clean-pixel magnifier — an unmarked window onto the
footage, screenshot-able. The fix is **not** repointing everything at the
composited canvas — the plan's Phase 3 table is authoritative and deliberately
keeps scopes (a measurement instrument must see program pixels) and the glass
refraction (frosted, unreadable, accepted risk) on the clean source. The loupe
keeps sampling the video at native resolution for quality, and **applies
`paintMark` itself at the lens's zoom/offset** so its window is marked. Missing
that one assignment is the hole; the rest are recorded decisions.

### Screen share goes through the same compositor

`session/+page.svelte:3043` renders a **second** raw `<video>` —
`screenShareVideoEl` — in split mode. A screen share in a review session
carries cuts, timelines and client comps; leaving it as a bare video element
reproduces the exact hole this feature exists to close, one element over.

It goes through the same compositor, with the same mark, sized to the split
pane. `LaserPointerOverlay` stays on top as a DOM sibling — it is an
annotation, not content, and nothing is lost if a viewer hides it.

If screen share is deliberately scoped out of a milestone, say so in the room
UI ("screen shares are not watermarked") rather than leaving it silent. The
`disablepictureinpicture` and `controlslist` attributes already on that element
show the intent was there; this finishes it.

### Bonus: this fixes the iOS fullscreen hole

`session/+page.svelte:71` already documents it: iPhone has no element
Fullscreen API, only native `<video>` fullscreen, which strips the overlays and
the watermark — so the fullscreen button is hidden on iPhone today. With a
canvas as the display surface there is no video element for iOS to promote,
and the mark is in the pixels regardless. Validate that iOS Safari permits
`drawImage` from a hidden, `playsinline` MediaStream-backed video — historically
iOS has restricted video→canvas, and if it refuses, iOS falls back to §5
enforcement or is blocked from marked rooms by policy.

---

## 3b. Validation gate results

Measured 2026-08-02 on Chromium, real Firefox (Windows) and real Safari 26.5.2
(macOS). Harness in `tests/gate/colorparity/`, full colour numbers in
`docs/COLOR_PARITY.md`. Every measurement uses a live WebRTC track with the
codec pinned to what the SFU negotiates.

**Colour parity — passes.** Canvas 2D `drawImage` is 0/255 against the native
`<video>` on every engine, both codecs. This is branch **(a)** above for macOS
Safari: marked rooms need not go Chromium/Firefox-only on colour grounds. WebGL
is 1-39/255 off (worst on Safari with VP8), and
`UNPACK_COLORSPACE_CONVERSION_WEBGL` does not change that — but an `ImageBitmap`
through the same pipeline is exact, which is a fix rather than a fallback.

**Program audio on a detached element — passes, and constrains the design.**
`play()` resolves, `currentTime` advances, and sample counts match an attached
element within 0.5% on all three engines. Stereo survives with the production
Opus fmtp (`stereo=1;sprop-stereo=1`, 2 channels, 48 kHz) and zero concealment.
The redesign this gate existed to catch — splitting audio onto its own element
or an `AudioContext` graph — is not needed.

The constraint: **"detached" must mean never-inserted, not removed.** The HTML
spec pauses a media element when it is *removed* from a document, and that is
observable — an element attached and then removed froze its `currentTime` while
the receiver kept playing out. So `createElement` and never append. If a later
refactor appends that element, `remove()` will silently kill program audio.

Not established: `muted`/`volume` on Firefox and Safari, where
`totalAudioEnergy` did not move with them (most likely sampled before the
element's volume rather than the controls failing — this test cannot tell those
apart). Nor audibility at the DAC, nor A/V sync drift. Those need a human
listening or a loopback audio device.

**rVFC on a detached element — passes.** `requestVideoFrameCallback` fires on a
never-inserted element at the same rate as attached on Chromium and Firefox, and
`receiveTime`, `processingDuration`, `presentationTime`, `expectedDisplayTime`,
`width`, `height` and `mediaTime` all populate. Detaching changes nothing about
the metadata. Safari delivers the same metadata on a detached element.

One field to know about: **`captureTime` is absent on every engine**, attached
and detached alike. It rides on the `abs-capture-time` RTP header extension,
which this SFU does not relay, so the stats readout should not promise it. That
is a pre-existing property, not a consequence of detaching.

**Firefox 4K blit cost — not settled.** Headless runs render in software, so the
numbers (Chromium 25 ms median at 3840x2160 -> 1920x1080; Firefox 12 ms but only
reaching 2364x1330) are worst-case, not what a reviewer's GPU does. This needs a
headed session on real hardware before Phase 2 ships.

**iOS — deferred by owner.** Both iOS questions remain open: whether it permits
`drawImage` from a hidden `playsinline` MediaStream video, and whether it
suspends a detached element's audio.

---

## 4. What the mark looks like (and why)

**Content** comes from the server, never from anything the client can edit:
display name, participant short-ID, room, and a live UTC clock. The clock
matters — it converts a screen recording into a timestamped one. **There is no
email in the data model** — `models.Participant` is ID/name/role/color — so the
mark does not promise one; the short-ID is what ties a leaked frame back to
the §6 audit row. The room's admin-configured `watermarkText` template (today:
`{{ name }} - {{ date }}`) is expanded *server-side* and carried as an extra
line, so custom room text survives without the client controlling the mark.

**Signed and short-TTL.** The server issues an HMAC-signed mark token over the
existing WebSocket, re-issued periodically. This does not stop a determined
client from rendering something else; it stops a leaker from rendering a
*colleague's* name. Preventing misattribution is the property corporate legal
needs, and it is the one that is cryptographically real here.

**Tiled, diagonal, drifting.** Three separate reasons:

- A single corner mark is removed by cropping. Tile it across the frame.
- A *static* low-opacity mark is removed by temporal averaging over
  motion-rich footage — average enough frames and the constant layer separates
  out. The mark must drift, so it never sits at a fixed pixel.
- Drift must be **deterministic from the session ID**, seeded server-side, so
  the exact position at a given timestamp is reproducible during forensic
  analysis of a leaked recording.

A slow Lissajous path over ~30–60s, with slight opacity modulation, satisfies
all three. Default opacity stays low enough not to fight the grade; the
existing per-room opacity/scale/position controls carry over as bounds.

**Logo watermarks carry forward.** Today's `watermarkMode` is
`none | text | logo | both` with a room logo image
(`WatermarkLogoPath`/`Position`, `models.go`). The compositor must keep
rendering the logo layer (static, no drift — it is branding, not identity) or
rooms configured `logo`/`both` silently lose their client's brand bug the day
the DOM overlay is retired. The identity mark renders *in addition to*
whatever mode the room configured, whenever the room enables watermarking.

**Program-video purity.** `drawImage` measured pixel-exact, so the picture is
untouched everywhere the mark is not drawn. Lock that with a test in the same
spirit as the Opus fmtp tests: composite a known frame, assert unmarked regions
are bit-identical to the source.

---

## 5. Enforcement (plan Phase 5)

Rendering is client-side, so the server must have a lever when the client stops
cooperating.

**Attestation.** The client heartbeats over the WebSocket: canvas present, its
dimensions, the mark token it is rendering, mark coordinates, and a
frame counter proving rVFC is advancing. Be honest internally about what this
is — the SFU has no decoded pixels, so it *cannot* cryptographically verify
that the mark is on screen. Attestation raises the bar from "delete a node in
devtools" to "write custom JavaScript." That is precisely the line between
casual and determined, and it is worth having; it is not a proof.

**Failure budget — get this wrong and reviewers lose video when they alt-tab.**
An OS-backgrounded tab stops `requestVideoFrameCallback` entirely, so the frame
counter freezes and attestation legitimately fails on a perfectly honest
client. The gate must be:

- **Visibility-aware.** `document.visibilityState === "hidden"` suspends the
  frame-advance requirement; a hidden tab is not rendering anything to leak.
  Canvas presence and token checks continue.
- **Budgeted.** N consecutive missed heartbeats before the gate trips, not one.
  Start at N=3 on a ~2s heartbeat (~6s grace) and tune. This codebase's history
  is transient-failure handling — `bindCanvasStream`'s play-retry machinery
  exists because Firefox drops frames on attach — and a watermark gate that
  trips on the same transients will be worse than the bug it prevents.
- **Logged before enforced.** Ship the gate in report-only mode first, watch
  the attestation-failure counts in §6's audit table across real sessions, and
  only then let it cut video. The false-positive rate is an empirical question.

**The lever: stop sending video to that subscriber.** Call
`sub.VideoSender.ReplaceTrack(nil)`. Verified against pion v4.2.1
(`rtpsender.go:233`): with a nil track it unbinds the current track from that
sender and clears the encoding, then returns — **no renegotiation, no
`negotiationneeded`, no peer-connection teardown**, which is what the
stability-over-purity rule requires. The shared `TrackLocalStaticRTP` keeps
fanning out to every other subscriber untouched; only this sender's binding is
gone. **The gate must nil the screen-share sender too** when one is up — a
gated viewer must not keep receiving the screen share, which carries the same
cuts and comps the program feed does.

Restore is `ReplaceTrack(videoTrack)`, which re-binds against the preserved
context — same SSRC, same write stream (`rtpsender.go:273-295`). Note
`sfu.go:1220` proves ReplaceTrack-with-a-track; the nil round-trip is verified
from source here but should get its own test alongside the existing SFU tests.

**Request a keyframe on restore.** The subscriber's decoder has a gap and will
sit black until the next natural IDR. Reuse the existing coalesced PLI path
(`keyframeRequestMinInterval`, `lastKeyframeRequest` at `sfu.go:93`/`:195`)
rather than adding a second mechanism.

Audio and signaling stay up throughout, so the viewer sees a deliberate
"watermark verification failed" state rather than a broken room.

This also covers any engine where §3's validation fails: no verified compositor
means no video, by policy, rather than silently degrading to a removable
overlay.

Lock ordering when the gate touches SFU state: `SignalingMu` → `room.mu`,
never the reverse.

---

## 6. Audit trail (plan Phase 4 — ships before enforcement turns on)

For corporate adoption this is half the feature, and it is the control that
covers the threat §1 says is not preventable. The compositor flag does not turn
on for real rooms until this exists.

New migration (`011_add_view_audit.sql` — `010` is taken by Phase 0's
file-origin migration; the sequence currently ends at
`009_drop_redundant_slug_index.sql`):
who joined, which room, which mark token was issued, issued/revoked
timestamps, IP, user agent, attestation failure count. Exportable per room and
per cut: *"here is everyone who saw this, when, and the mark they carried."*

Standard conventions apply: `database.WithTimeout(r.Context())` + the
`*Context` method on every call, `rows.Err()` after every `rows.Next()` loop,
one transaction for writes that must hold together.

---

## 7. Forensic A/B marking (future work — not in the plan's phases)

The real invisible-forensic technique is A/B variant switching: encode two
versions differing imperceptibly, and give each viewer a per-GOP sequence of
A/B choices that spells their ID. It survives re-encode, crop, and camera
capture, and it is what NexGuard-class systems do.

Its prerequisites are the honest part:

- **Two encoded ingests.** OBS must publish twice, or the server must encode —
  which reintroduces everything §2 rejected. Publisher-side dual encode is the
  only version compatible with this architecture.
- Frequent, aligned IDRs on both variants, and GOP-aligned switching in the
  fan-out (which means per-subscriber tracks, not today's single shared track —
  a real change to `RoomTracks`).
- Tens of seconds of leaked footage to recover an ID.

Worth speccing when a customer asks for it by name. Not worth building first:
for live review sessions the dominant leak vector is a phone pointed at a
monitor, and **the visible per-viewer mark already attributes that** — which is
why Phases 1–2 deliver most of the value for a fraction of the work.

---

## 8. Build sequence

The plan (`WATERMARKING_PLAN.md`) is authoritative on ordering; this is the
same sequence with its gates called out.

1. **Close the frame-grab hole** (plan Phase 0) — server-side, per-requester
   marking of grabbed frames. Independent of everything below; ships first.
2. **Mark engine** (plan Phase 1) — pure functions in JS + Go with shared test
   vectors. Can proceed in parallel with step 3; nothing user-visible yet.
3. **Close the validation gap** (the plan's gate). Re-run the color-parity
   harness against a live WebRTC stream, and on real Safari (macOS + iOS).
   Verify program audio from a detached element. Confirm iOS allows
   `drawImage` from a hidden MediaStream video. *This gates all compositor
   work* — if it fails, pick (a)/(b)/(c) from §3 before writing any more code.
4. **Compositor** (plan Phase 2): extract from `bindCanvasStream` into
   `web/src/lib/video/markedCanvas.ts`; render program video through it behind
   a feature flag, no mark yet. Verify color, latency, and Firefox 4K cost.
5. **Tool re-wiring** (plan Phase 3): repoint `frameSource.ts` consumers
   (glass, loupe, scopes) per the plan's table, and route `screenShareVideoEl`
   through the compositor too. Then enable the mark layer: tiling, seeded
   drift, opacity modulation; purity test for unmarked regions.
6. **Server-issued signed mark tokens** over the WebSocket + the audit
   migration (plan Phase 4).
7. **Attestation heartbeat, report-only**, logging to the audit table (plan
   Phase 5).
8. Flip the `ReplaceTrack(nil)` gate on once step 7's false-positive rate is
   known; add the nil round-trip SFU test and the keyframe-on-restore path.
9. Retire `WatermarkOverlay.svelte` from the session page; keep it for the
   admin preview, which legitimately wants a DOM overlay.

Existing room config (`watermarkMode`, `opacity`, `posX/posY`, `scale` in
`internal/models/models.go:33`) carries forward as authoring bounds; the
per-viewer identity fields are new and server-owned.
