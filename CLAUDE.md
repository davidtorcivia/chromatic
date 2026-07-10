# Chromatic

Self-hosted, low-latency video review for film/agency work: OBS publishes via
WHIP into a Go SFU (pion); reviewers join rooms in the browser (SvelteKit).
Premium product — latency, program-audio purity, and polish are the axioms.

## Layout

- `internal/webrtc/` — SFU: WHIP ingest, subscriber fan-out, voice/screen/cam
  relays. The hot path; treat every per-packet allocation as a regression.
- `internal/api/` — HTTP handlers, middleware, router. SQLite via `internal/database`.
- `internal/websocket/` — per-room signaling hub.
- `web/` — SvelteKit (Svelte 5 runes) frontend. The session room lives in
  `web/src/routes/room/[slug]/session/+page.svelte`; extract cohesive logic to
  `web/src/lib/` rather than growing it.

## Non-negotiable invariants

- **Program audio purity**: OBS's stereo program audio is relayed bit-clean —
  no ducking, no downmix, no processing. Voice chat is a separate track/chain.
  Tests lock the negotiated Opus fmtp; don't weaken them.
- **Never block the ingest read loop**: fan-out goes through `asyncForwarder`
  (drop-oldest). Anything on the RTP path must be allocation-free per packet.
- **Lock ordering in the SFU**: `SignalingMu` → `room.mu`, never the reverse.
  Fields read under both locks must be atomic (see `VoiceSession.publisherOfferID`).
- **No CSS `backdrop-filter` over the video**: washes the whole frame on
  Firefox. The glass/loupe effects are WebGL for this reason.

## Conventions (the lint gate enforces most of these)

- Every handler DB call: `ctx, cancel := database.WithTimeout(r.Context())` +
  the `*Context` method. Background work uses `WithTimeout(context.Background())`.
  The pool is 4 connections; an undeadlined query pins 25% of it.
- Every `rows.Next()` loop ends with `rows.Err()` — `Next()` returns false on
  both end-of-data and mid-iteration error; without the check a timeout serves
  a silently truncated result as a 200.
- Multi-statement writes that must hold together go in one transaction
  (`BeginTx` + `defer tx.Rollback()`).
- Timers/intervals: clear-before-set, and every `set*` has a cleanup path.
- Frontend loops: drive canvas redraws from `requestVideoFrameCallback` (or a
  throttled rAF fallback), never a bare 60fps rAF loop; cache layout reads via
  ResizeObserver instead of `getBoundingClientRect` per frame; non-display
  work (VAD etc.) uses `setInterval`, not rAF.
- Comments explain *why* (constraints, failure modes), not what.

## Build & test

- `go build ./...`, `golangci-lint run ./...` (config in `.golangci.yml`),
  `gofmt` (LF endings enforced via .gitattributes).
- `go test ./...` — the SQLite-backed packages (`handlers`, `database`) need
  cgo; on a no-gcc box they fail with the go-sqlite3 stub error and CI is the
  signal for them. webrtc/websocket/api/middleware tests run anywhere.
- Tests create SFUs with `ICELoopbackOnly: true` (loopback-only ICE) so test
  binaries never trigger OS firewall prompts. Keep it that way.
- Frontend: `cd web && npx svelte-check && npx vitest run && npm run build`.
