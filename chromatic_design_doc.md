# Chromatic — Self-Hosted Client Streaming Platform
## Design Document v2.3

> **Implementation Status:** Planning Complete — Ready for Phase 1  
> **Last Updated:** 2024-12-27

---

## 1. Overview

Chromatic is a self-hosted, low-latency streaming platform designed for professional colorists to conduct remote grading sessions with advertising creatives and directors. It replaces services like Louper.io with a high-fidelity, fully owned solution.

### 1.1 Core Value Proposition
- **Color Fidelity:** High-bitrate streaming optimized for MacBook Pro XDR displays.
- **Sub-second Latency:** Real-time WebRTC streaming from DaVinci Resolve via OBS.
- **Interactive Review:** Laser pointer allows any participant to point at the screen, visible to all.
- **Client Experience:** No-install, browser-based viewing optimized for non-technical stakeholders.

### 1.2 Bit Depth Reality (v1)

**Current State:** OBS + WebRTC + H.264 delivers 8-bit 4:2:0 color. This is the practical ceiling for v1.

**Why It's Acceptable:** A well-encoded 8-10 Mbps 8-bit stream on an XDR display looks excellent and satisfies 95% of client review scenarios. Clients are approving creative direction, not performing final QC.

**Path to 10-bit (v2+):**
| Encoder | Codec | Bit Depth | Browser Support |
|---------|-------|-----------|-----------------|
| x264 | H.264 | 8-bit | Universal |
| hevc_videotoolbox (Mac) | HEVC/H.265 | 10-bit | Safari, Chrome 107+ |
| hevc_nvenc (NVIDIA) | HEVC/H.265 | 10-bit | Safari, Chrome 107+ |
| libvpx-vp9 | VP9 Profile 2 | 10-bit | Chrome, Firefox |

**Recommendation:** Ship v1 with high-bitrate 8-bit H.264. Evaluate HEVC for v2 once core functionality is stable.

### 1.2 User Constraints & Environment
- **Target Client Hardware:** 95% MacBook Pro (Liquid Retina XDR).
- **Target Client Software:** Google Chrome (Primary), Safari (Secondary).
- **Deployment:** Single-user (Colorist hosted), Ubuntu Server + Docker.
- **Network:** Gigabit symmetric fiber. At maximum load (10Mbps × 8 clients = 80Mbps), approximately 8% upstream utilization.

---

## 2. System Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              HOST SERVER                                    │
│  ┌─────────────────────────────────────────────────────────────────────────┐│
│  │                         DOCKER COMPOSE STACK                            ││
│  │                                                                         ││
│  │  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐                  ││
│  │  │   Caddy     │    │  Chromatic  │    │   Coturn    │                  ││
│  │  │  (Reverse   │───▶│   Server    │    │   (TURN)    │                  ││
│  │  │   Proxy)    │    │   (Go)      │    │             │                  ││
│  │  └─────────────┘    └──────┬──────┘    └─────────────┘                  ││
│  │                            │                                            ││
│  │                     ┌──────┴──────┐                                     ││
│  │                     │   SQLite    │                                     ││
│  │                     │ (WAL Mode)  │                                     ││
│  │                     └─────────────┘                                     ││
│  └─────────────────────────────────────────────────────────────────────────┘│
└─────────────────────────────────────────────────────────────────────────────┘
         ▲                        ▲                         ▲
         │ HTTPS/WSS              │ WHIP (WebRTC)           │ WebRTC (UDP/TCP)
         │ (CF Tunnel OK)         │                         │ (Direct/TURN)
    ┌────┴────┐              ┌────┴────┐              ┌─────┴─────┐
    │ Browser │              │   OBS   │              │  Clients  │
    │ (Admin) │              │ Studio  │              │ (2-8)     │
    └─────────┘              └─────────┘              └───────────┘
```

### 2.1 Technology Stack

| Component | Technology | Purpose |
|-----------|------------|---------|
| Chromatic Server | Go + Pion WebRTC | Core application, SFU |
| Frontend | SvelteKit | Admin dashboard, client viewer |
| Database | SQLite (WAL mode) | Rooms, scheduling, config, chat |
| Reverse Proxy | Caddy | TLS termination, routing |
| TURN Server | Coturn | NAT traversal fallback |
| Ingest | WHIP | OBS to server WebRTC |

### 2.2 Network Architecture

**Signaling vs. Media Split:**
- **Signaling (HTTP/WebSocket):** Can traverse Cloudflare Tunnel.
- **Media (WebRTC/UDP):** Requires direct connection or TURN relay. Cannot use CF Tunnel.

**Bandwidth Planning:**

| Bitrate | Clients | Upstream Required | % of Gigabit |
|---------|---------|-------------------|--------------|
| 6 Mbps | 8 | 48 Mbps | 4.8% |
| 8 Mbps | 8 | 64 Mbps | 6.4% |
| 10 Mbps | 8 | 80 Mbps | 8.0% |

Comfortable headroom at all quality levels.

---

## 3. WebRTC & Media Architecture

### 3.1 SFU Design Decisions

**Main Stream (Resolve via OBS):**
- **No Simulcast.** Single high-quality stream forwarded to all clients.
- **Rationale:** Color-critical work requires maximum fidelity. We assume clients are agency creatives on good connections. Buffering is preferable to quality degradation for this use case.
- **Tradeoff:** Clients with poor connections will experience buffering rather than automatic quality reduction.

**Participant Webcams:**
- **Simulcast enabled.** Three layers: 180p, 360p, 720p.
- **Adaptive forwarding** based on receiver bandwidth estimates.

**Priority Hierarchy (bandwidth constrained):**

| Priority | Track | Degradation Strategy |
|----------|-------|---------------------|
| 1 (Highest) | Resolve Stream Video | Never degraded |
| 2 | Resolve Stream Audio | Never degraded |
| 3 | Voice Chat | Reduce bitrate (Opus DTX) |
| 4 (Lowest) | Client Webcam Video | Drop layers, then disable |

### 3.2 Audio Architecture

Since we are not mixing audio server-side, we manage echo prevention client-side.

**Audio Ducking Strategy:**

| Track | Description |
|-------|-------------|
| Stream Audio | High-quality audio from Resolve (dialogue, music, mix) |
| Voice Audio | Voice chat from all participants |

**Client Behavior:**
1. When voice activity detected (VAD threshold exceeded):
   - Stream audio volume ramps to 20% over 50ms.
2. When voice activity ceases for 800ms:
   - Stream audio ramps back to 100% over 200ms.
3. Prevents feedback loop where client mic picks up stream audio from speakers.

**Admin Exemption:**
The colorist (admin) is exempt from audio ducking. Assumption: colorist monitors on separate reference speakers/headphones and controls their own environment. Admin hears stream at full volume always; voice chat is mixed at a fixed level they can adjust.

### 3.3 Browser Color Management

**Detection & Guidance:**
1. Detect browser via User-Agent.
2. Chrome on macOS: Display non-intrusive toast on join:
   > "For color-critical review, Safari provides more accurate color management."
3. No forced calibration patterns. Trust Apple XDR display ecosystem baseline.

**Color Pipeline (OBS → Browser):**
- OBS Color Space: sRGB (field-tested 2026-06: Rec. 709 renders washed out on
  macOS, where ColorSync displays 709-tagged video with a 1.96 gamma; sRGB
  tagging is interpreted consistently across platforms)
- OBS Color Range: Limited/Partial (prevents crushed blacks)
- Browser: Standard video element rendering

---

## 4. Data Models

### 4.1 Database Schema

```sql
PRAGMA journal_mode=WAL;

-- Stream keys for OBS authentication (persistent)
CREATE TABLE stream_keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,                 -- "Main Suite", "Laptop"
    key_token TEXT UNIQUE NOT NULL,     -- Secret used in OBS WHIP URL
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Rooms
CREATE TABLE rooms (
    id TEXT PRIMARY KEY,
    slug TEXT UNIQUE NOT NULL,
    name TEXT NOT NULL,
    
    -- Scheduling
    scheduled_at DATETIME,              -- NULL = instant room
    duration_minutes INTEGER,           -- Expected duration (informational)
    
    -- Access Control
    password_hash TEXT,                 -- NULL = no password required
    waiting_room_enabled BOOLEAN DEFAULT FALSE,
    
    -- Stream Key Binding
    stream_key_id TEXT REFERENCES stream_keys(id),
    
    -- Watermark Config
    watermark_mode TEXT DEFAULT 'none', -- 'none', 'text', 'logo', 'both'
    watermark_text TEXT,                -- Template: "{{name}} - {{date}}"
    watermark_logo_path TEXT,           -- Path to uploaded logo file
    watermark_logo_position TEXT DEFAULT 'bottom-right',
    watermark_opacity REAL DEFAULT 0.3,
    
    -- State
    status TEXT DEFAULT 'pending',      -- 'pending', 'live', 'ended'
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    started_at DATETIME,
    ended_at DATETIME
);

-- Participants (ephemeral, cleared on room end)
CREATE TABLE participants (
    id TEXT PRIMARY KEY,
    room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    role TEXT NOT NULL,                 -- 'admin', 'viewer'
    
    is_admitted BOOLEAN DEFAULT FALSE,  -- For waiting room
    joined_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    
    audio_enabled BOOLEAN DEFAULT TRUE,
    video_enabled BOOLEAN DEFAULT TRUE
);

-- Chat Messages
CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    participant_id TEXT NOT NULL REFERENCES participants(id),
    
    type TEXT NOT NULL,                 -- 'text', 'file'
    content TEXT,                       -- Message text or file ref JSON
    
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- File Uploads
CREATE TABLE files (
    id TEXT PRIMARY KEY,
    room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    uploader_id TEXT NOT NULL REFERENCES participants(id),
    
    original_name TEXT NOT NULL,
    stored_path TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Admin Configuration (single row)
CREATE TABLE config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    default_watermark_text TEXT,
    default_watermark_logo_path TEXT,
    turn_external_url TEXT,
    turn_external_username TEXT,
    turn_external_credential TEXT
);

-- Indexes
CREATE INDEX idx_rooms_scheduled ON rooms(scheduled_at) WHERE status = 'pending';
CREATE INDEX idx_rooms_status ON rooms(status);
CREATE INDEX idx_rooms_stream_key ON rooms(stream_key_id);
CREATE INDEX idx_participants_room ON participants(room_id);
CREATE INDEX idx_messages_room ON messages(room_id);
```

### 4.2 Stream Key → Room Binding

**Binding Mechanism:**
1. Each room can optionally reference a `stream_key_id`.
2. When OBS connects via WHIP with a stream key token:
   - Server looks up the stream key.
   - Finds rooms where `stream_key_id` matches AND `status = 'pending'` or `status = 'live'`.
   - If exactly one room matches: stream routes there.
   - If multiple rooms match: uses the one with earliest `scheduled_at` (or `created_at` for instant rooms).
   - If no rooms match: stream is held in "standby" (no viewers, but connection maintained).
3. **Admin UI:** When creating/editing a room, colorist selects which stream key to use from dropdown.

**Workflow:**
- Colorist configures OBS once with stream key URL.
- Creates rooms in admin UI, binding to that stream key.
- OBS stays connected; rooms come and go.

---

## 5. Interactive Features

### 5.1 Laser Pointer

Allows any participant to point at the stream, visible to all viewers including admin.

**Client Side:**
1. Transparent overlay div on video player.
2. Captures pointer events (mouse/touch).
3. On pointer down + move (drag):
   - Calculate normalized coordinates: `x = clientX / videoWidth`, `y = clientY / videoHeight`
   - Clamp to 0.0–1.0 range.
   - Send WebSocket message every 50ms (throttled).
4. On pointer up: send `active: false`.

**Server Side:**
1. Receives cursor message from participant.
2. Broadcasts to ALL participants in room (including sender, for latency feedback).
3. Each cursor tagged with participant ID and assigned color.

**Rendering (All Clients):**
1. Each active cursor rendered as colored dot + participant name label.
2. Position mapped to local video element dimensions.
3. Cursors from different participants get different colors (assigned server-side from preset palette).
4. Cursor fades out 500ms after last update if no `active: false` received (handles disconnect).

**WebSocket Message:**
```json
{
  "type": "cursor",
  "payload": {
    "participantId": "abc123",
    "participantName": "Sarah",
    "color": "#e63946",
    "x": 0.4532,
    "y": 0.3321,
    "active": true
  }
}
```

### 5.2 Latency Visualization

**Implementation:**
1. Frontend extracts RTT from WebRTC stats API (`RTCStatsReport`).
2. Admin dashboard displays estimated latency per participant.
3. Format: "Sarah: ~150ms"

**Usage:** Helps colorist time verbal cues ("stop... NOW") accounting for delay.

---

## 6. Room Scheduling & Access Control

### 6.1 Scheduling

**Room States:**
- `pending`: Created but not yet live.
- `live`: OBS connected and streaming.
- `ended`: Session concluded.

**Early Access Rule:**
- Clients can access room URL starting 10 minutes before `scheduled_at`.
- Before early access window: "This session opens at {time}."
- During early access but before OBS connects: Waiting/device setup screen.

**Instant Rooms:**
- `scheduled_at = NULL` means room is available immediately.
- Transitions to `live` when OBS connects.

### 6.2 Waiting Room

When `waiting_room_enabled = true`:

1. Client completes join flow (name, password if required).
2. Client enters waiting room:
   - Can set up camera/microphone.
   - Sees "Waiting for host to admit you."
3. Admin sees list of waiting participants.
4. Admin clicks "Admit" → participant joins session.
5. Admin can "Admit All" for convenience.

### 6.3 Password Protection

- Optional per-room.
- Client prompted before entering waiting room or session.
- Passwords stored as bcrypt hash.
- Minimum 4 characters.

---

## 7. File Sharing

### 7.1 Supported Files

| Category | MIME Types | Max Size |
|----------|------------|----------|
| Images | image/jpeg, image/png, image/gif, image/webp | 5 MB |
| Audio | audio/mpeg, audio/wav, audio/ogg | 5 MB |
| Documents | application/pdf | 5 MB |

### 7.2 Upload Flow

1. Client selects file via chat input.
2. Client-side validation (size, type).
3. `POST /api/rooms/{slug}/files` (multipart).
4. Server validates, stores in `/data/files/{room_id}/{file_id}.{ext}`.
5. For images: generate thumbnail (`{file_id}_thumb.webp`).
6. Server responds with file metadata.
7. Client sends chat message of type `file` with metadata.
8. All participants see file in chat:
   - Images: inline thumbnail, click to expand.
   - Audio: inline player.
   - PDF: download link with preview icon.

### 7.3 Cleanup

Files deleted when room is deleted or 7 days after room ends (configurable).

---

## 8. Watermarking

### 8.1 Modes

| Mode | Description |
|------|-------------|
| `none` | No watermark |
| `text` | Dynamic text overlay |
| `logo` | Static logo image |
| `both` | Text and logo combined |

### 8.2 Text Watermark

**Template Variables:**
- `{{name}}` — Participant display name
- `{{room}}` — Room name
- `{{date}}` — Current date (YYYY-MM-DD)
- `{{time}}` — Current time (HH:MM)

**Rendering:**
- Semi-transparent text.
- Positioned center or corner (configurable).
- Each client sees their own name (personalized).

### 8.3 Logo Watermark

**Upload:**
- Admin uploads logo via settings or per-room config.
- Stored in `/data/logos/{filename}`.
- Recommended: PNG with transparency, max 500x500px.

**Positioning:**
- `top-left`, `top-right`, `bottom-left`, `bottom-right`
- Configurable per-room.

### 8.4 Anti-Tampering

**MutationObserver Protection:**
1. Watermark rendered as canvas overlay.
2. `MutationObserver` watches for:
   - Overlay element removal.
   - Style changes (opacity, visibility, display).
   - Z-index manipulation.
3. If tampering detected:
   - Immediately disconnect WebRTC.
   - Show "Session terminated due to policy violation."
   - Log event server-side.

**Limitations:**
This is a deterrent, not cryptographic protection. Determined users with screen capture software can bypass. Appropriate for "keeping honest people honest."

---

## 9. Frontend Architecture

### 9.1 Route Structure

```
/                               Landing / Admin login
/admin                          Dashboard
/admin/rooms                    Room list
/admin/rooms/new                Create room
/admin/rooms/{slug}             Room settings
/admin/rooms/{slug}/live        Live session control
/admin/stream-keys              Manage OBS stream keys
/admin/settings                 Global settings, default watermarks

/room/{slug}                    Client join flow
/room/{slug}/waiting            Waiting room (device setup)
/room/{slug}/session            Active session
```

### 9.2 Mobile Considerations

**Viewer Experience:**
- Stream fills viewport (16:9 letterboxed if needed).
- Controls overlay on tap, auto-hide after 3s.
- Swipe up for chat drawer.
- Participants shown as small avatars, expand on tap.

**Capabilities:**
- View stream: Yes
- Voice chat: Yes (device permitting)
- Camera: No (mobile participants are voice-only)
- Laser pointer: Yes (touch drag)
- File viewing: Yes
- File upload: Yes

**Layout (Mobile < 768px):**
```
┌─────────────────────────────┐
│                             │
│      Stream Viewer          │
│      (letterboxed 16:9)     │
│                             │
├─────────────────────────────┤
│  [Participants: avatars]    │
├─────────────────────────────┤
│   🎤   🔊   💬   ⛶   ⋮     │
└─────────────────────────────┘
```

### 9.3 Audio Manager Implementation

**Mobile AudioContext Unlock:**

Mobile Safari and Chrome mute `AudioContext` until the user performs a physical interaction. The audio manager must be initialized lazily.

```typescript
// lib/audio/context.ts

let audioContext: AudioContext | null = null;

export async function getAudioContext(): Promise<AudioContext> {
  if (!audioContext) {
    audioContext = new AudioContext();
  }
  
  // Required for mobile browsers
  if (audioContext.state === 'suspended') {
    await audioContext.resume();
  }
  
  return audioContext;
}

// Must be called from a click/tap handler
export async function unlockAudio(): Promise<void> {
  const ctx = await getAudioContext();
  // Play a silent buffer to fully unlock on iOS
  const buffer = ctx.createBuffer(1, 1, 22050);
  const source = ctx.createBufferSource();
  source.buffer = buffer;
  source.connect(ctx.destination);
  source.start();
}
```

**Integration Point:** The "Join Session" button handler must call `unlockAudio()` before initializing the ducking manager.

```typescript
// routes/room/[slug]/session/+page.svelte

async function handleJoinClick() {
  await unlockAudio();  // MUST be in click handler
  await initializeSession();
}
```

**Ducking Implementation:**

```typescript
// lib/audio/ducking.ts

interface DuckingConfig {
  duckLevel: number;        // 0.2 = 20% volume when ducked
  attackTime: number;       // 50ms ramp down
  releaseTime: number;      // 200ms ramp up
  holdTime: number;         // 800ms before release
  vadThreshold: number;     // -50dB activation threshold
}

const DEFAULT_CONFIG: DuckingConfig = {
  duckLevel: 0.2,
  attackTime: 50,
  releaseTime: 200,
  holdTime: 800,
  vadThreshold: -50
};

export class AudioDuckingManager {
  private streamElement: HTMLMediaElement;
  private voiceTracks: Map<string, MediaStreamTrack>;
  private audioContext: AudioContext;
  private isAdmin: boolean;
  
  constructor(streamElement: HTMLMediaElement, isAdmin: boolean) {
    this.streamElement = streamElement;
    this.isAdmin = isAdmin;
    this.voiceTracks = new Map();
    this.audioContext = new AudioContext();
  }
  
  addVoiceTrack(participantId: string, track: MediaStreamTrack) {
    // Create analyser for this track
    // Monitor for voice activity
  }
  
  private onVoiceActivity(active: boolean) {
    // Admin exempt from ducking
    if (this.isAdmin) return;
    
    if (active) {
      this.duckStream();
    } else {
      this.scheduleRelease();
    }
  }
  
  private duckStream() {
    // Ramp streamElement.volume to duckLevel over attackTime
  }
  
  private scheduleRelease() {
    // After holdTime, ramp volume back to 1.0 over releaseTime
  }
}
```

### 9.4 Laser Pointer Rendering

**Letterbox-Aware Coordinate Calculation:**

When using `object-fit: contain`, the video may have black bars. Pointer coordinates must map to the actual video content, not the element bounds.

```typescript
// lib/video/coordinates.ts

interface VideoRect {
  x: number;      // Left edge of video content within element
  y: number;      // Top edge of video content within element
  width: number;  // Rendered width of video content
  height: number; // Rendered height of video content
}

export function getVideoContentRect(video: HTMLVideoElement): VideoRect {
  const elementRect = video.getBoundingClientRect();
  const elementWidth = elementRect.width;
  const elementHeight = elementRect.height;
  
  const videoWidth = video.videoWidth;
  const videoHeight = video.videoHeight;
  
  if (!videoWidth || !videoHeight) {
    // Fallback if video dimensions not yet available
    return { x: 0, y: 0, width: elementWidth, height: elementHeight };
  }
  
  const elementAspect = elementWidth / elementHeight;
  const videoAspect = videoWidth / videoHeight;
  
  let renderedWidth: number;
  let renderedHeight: number;
  
  if (videoAspect > elementAspect) {
    // Video is wider than element: letterbox top/bottom
    renderedWidth = elementWidth;
    renderedHeight = elementWidth / videoAspect;
  } else {
    // Video is taller than element: pillarbox left/right
    renderedHeight = elementHeight;
    renderedWidth = elementHeight * videoAspect;
  }
  
  const x = (elementWidth - renderedWidth) / 2;
  const y = (elementHeight - renderedHeight) / 2;
  
  return { x, y, width: renderedWidth, height: renderedHeight };
}

export function clientToVideoCoords(
  clientX: number,
  clientY: number,
  video: HTMLVideoElement
): { x: number; y: number; valid: boolean } {
  const elementRect = video.getBoundingClientRect();
  const videoRect = getVideoContentRect(video);
  
  // Convert client coords to element-relative
  const elementX = clientX - elementRect.left;
  const elementY = clientY - elementRect.top;
  
  // Convert to video-content-relative
  const videoX = elementX - videoRect.x;
  const videoY = elementY - videoRect.y;
  
  // Normalize to 0-1 range
  const normalizedX = videoX / videoRect.width;
  const normalizedY = videoY / videoRect.height;
  
  // Check if click was in letterbox/pillarbox area
  const valid = normalizedX >= 0 && normalizedX <= 1 && 
                normalizedY >= 0 && normalizedY <= 1;
  
  return {
    x: Math.max(0, Math.min(1, normalizedX)),
    y: Math.max(0, Math.min(1, normalizedY)),
    valid
  };
}
```

**Svelte Component:**

```typescript
// lib/components/LaserPointerOverlay.svelte

<script lang="ts">
  import { onMount } from 'svelte';
  import { cursors } from '$lib/stores/session';
  import { clientToVideoCoords, getVideoContentRect } from '$lib/video/coordinates';
  
  export let videoElement: HTMLVideoElement;
  
  let overlayEl: HTMLDivElement;
  let isPointing = false;
  let videoRect = { x: 0, y: 0, width: 0, height: 0 };
  
  // Recalculate on resize or video metadata load
  function updateVideoRect() {
    videoRect = getVideoContentRect(videoElement);
  }
  
  onMount(() => {
    updateVideoRect();
    videoElement.addEventListener('loadedmetadata', updateVideoRect);
    window.addEventListener('resize', updateVideoRect);
    
    return () => {
      videoElement.removeEventListener('loadedmetadata', updateVideoRect);
      window.removeEventListener('resize', updateVideoRect);
    };
  });
  
  function handlePointerDown(e: PointerEvent) {
    isPointing = true;
    sendCursor(e);
  }
  
  function handlePointerMove(e: PointerEvent) {
    if (!isPointing) return;
    sendCursor(e);
  }
  
  function handlePointerUp() {
    isPointing = false;
    sendCursorEnd();
  }
  
  function sendCursor(e: PointerEvent) {
    const coords = clientToVideoCoords(e.clientX, e.clientY, videoElement);
    
    // Optionally ignore clicks in letterbox area
    // if (!coords.valid) return;
    
    // Throttled WebSocket send
    ws.send({ type: 'cursor', payload: { x: coords.x, y: coords.y, active: true } });
  }
</script>

<!-- Overlay positioned to match video content area, not full element -->
<div 
  bind:this={overlayEl}
  class="absolute cursor-crosshair"
  style="
    left: {videoRect.x}px;
    top: {videoRect.y}px;
    width: {videoRect.width}px;
    height: {videoRect.height}px;
  "
  on:pointerdown={handlePointerDown}
  on:pointermove={handlePointerMove}
  on:pointerup={handlePointerUp}
  on:pointerleave={handlePointerUp}
>
  {#each $cursors as cursor (cursor.participantId)}
    <div
      class="absolute pointer-events-none transition-all duration-75"
      style="
        left: {cursor.x * 100}%;
        top: {cursor.y * 100}%;
        opacity: {cursor.active ? 1 : 0};
      "
    >
      <div 
        class="w-4 h-4 rounded-full -translate-x-1/2 -translate-y-1/2"
        style="background-color: {cursor.color}; box-shadow: 0 0 8px {cursor.color};"
      />
      <span 
        class="absolute left-4 top-0 text-xs whitespace-nowrap px-1 rounded"
        style="background-color: {cursor.color}; color: white;"
      >
        {cursor.participantName}
      </span>
    </div>
  {/each}
</div>
```

---

## 10. OBS Configuration

### 10.1 Required Settings

| Setting | Value | Reason |
|---------|-------|--------|
| Output Mode | Advanced | Access to all encoder settings |
| Encoder | x264 / NVENC / QSV | Hardware preferred if available |
| Rate Control | CBR | Consistent bandwidth, predictable quality |
| Bitrate | 6000–10000 Kbps | Balance fidelity vs. bandwidth |
| Keyframe Interval | 1 second | Fast join and recovery from packet loss |
| Profile | baseline | Required — the SFU rejects Main/High |
| Tune | zerolatency | Minimize encoder buffering |
| **B-Frames** | **0** | **CRITICAL: Browsers cannot reorder B-frames in live WebRTC. Non-zero causes 2+ second latency.** |
| Color Space | sRGB | Consistent rendering on all platforms (macOS shows Rec. 709 washed out via 1.96 gamma) |
| Color Range | Limited/Partial | Prevents crushed blacks in browser |

### 10.2 WHIP Configuration

OBS 30.0+:
1. Settings → Stream
2. Service: **WHIP**
3. Server: `https://{domain}/whip/{stream_key_token}`
4. No additional authentication needed (key is in URL)

### 10.3 Audio Settings

- Sample Rate: 48000 Hz
- Channels: Stereo
- Codec: Opus (handled by WebRTC, ~128kbps)

---

## 11. API Reference

### 11.1 REST Endpoints

**Authentication:** Admin endpoints require `Authorization: Bearer {admin_token}`.

**Stream Keys:**
```
GET    /api/stream-keys              List all stream keys
POST   /api/stream-keys              Create stream key
DELETE /api/stream-keys/{id}         Delete stream key
```

**Rooms:**
```
GET    /api/rooms                    List rooms (filterable by status)
POST   /api/rooms                    Create room
GET    /api/rooms/{slug}             Get room details
PATCH  /api/rooms/{slug}             Update room
DELETE /api/rooms/{slug}             Delete room
POST   /api/rooms/{slug}/end         End live session
```

**Waiting Room:**
```
GET    /api/rooms/{slug}/waiting     List waiting participants
POST   /api/rooms/{slug}/admit/{id}  Admit participant
POST   /api/rooms/{slug}/admit-all   Admit all waiting
```

**Files:**
```
POST   /api/rooms/{slug}/files       Upload file (multipart)
GET    /api/files/{id}               Download file
GET    /api/files/{id}/thumbnail     Get image thumbnail
```

**WHIP:**
```
POST   /whip/{stream_key_token}      WHIP offer/answer
PATCH  /whip/{stream_key_token}      ICE trickle
DELETE /whip/{stream_key_token}      End ingest
```

### 11.2 WebSocket Messages

**Connection:** `wss://{domain}/ws/room/{slug}?token={join_token}&name={name}`

**Client → Server:**
```typescript
// Join/leave handled by connection lifecycle

{ type: 'chat:send', payload: { content: string } }
{ type: 'chat:file', payload: { fileId: string } }
{ type: 'cursor', payload: { x: number, y: number, active: boolean } }
{ type: 'media:toggle', payload: { audio?: boolean, video?: boolean } }
```

**Server → Client:**
```typescript
{ type: 'room:state', payload: { room: Room, participants: Participant[], isLive: boolean } }
{ type: 'room:live', payload: {} }
{ type: 'room:ended', payload: {} }

{ type: 'participant:joined', payload: { participant: Participant } }
{ type: 'participant:left', payload: { participantId: string } }
{ type: 'participant:updated', payload: { participant: Participant } }
{ type: 'participant:waiting', payload: { participant: Participant } }  // Admin only
{ type: 'participant:admitted', payload: { participantId: string } }

{ type: 'chat:message', payload: { id, participantId, participantName, type, content, file?, timestamp } }

{ type: 'cursor', payload: { participantId, participantName, color, x, y, active } }

{ type: 'admin:muted', payload: { participantId } }
{ type: 'admin:video-disabled', payload: { participantId } }
{ type: 'kicked', payload: { reason?: string } }

{ type: 'signal:offer', payload: { sdp } }
{ type: 'signal:answer', payload: { sdp } }
{ type: 'signal:candidate', payload: { candidate } }

{ type: 'error', payload: { code, message } }
```

**Admin → Server (privileged):**
```typescript
{ type: 'admin:mute', payload: { participantId: string } }
{ type: 'admin:mute-all', payload: {} }
{ type: 'admin:disable-video', payload: { participantId: string } }
{ type: 'admin:kick', payload: { participantId: string, reason?: string } }
{ type: 'admin:admit', payload: { participantId: string } }
{ type: 'admin:admit-all', payload: {} }
{ type: 'admin:end-session', payload: {} }
```

---

## 12. Deployment

### 12.1 Docker Compose

```yaml
version: '3.8'

services:
  chromatic:
    build: .
    container_name: chromatic
    restart: unless-stopped
    network_mode: host
    environment:
      - DATABASE_PATH=/data/chromatic.db
      - UPLOAD_PATH=/data/files
      - LOGO_PATH=/data/logos
      - ADMIN_TOKEN=${ADMIN_TOKEN}
      - PUBLIC_URL=${PUBLIC_URL}
      - TURN_SECRET=${TURN_SECRET}
      - TURN_REALM=${TURN_REALM}
    volumes:
      - chromatic_data:/data

  coturn:
    image: coturn/coturn:latest
    container_name: chromatic_turn
    restart: unless-stopped
    network_mode: host
    volumes:
      - ./turnserver.conf:/etc/coturn/turnserver.conf:ro

volumes:
  chromatic_data:
```

**Note:** `network_mode: host` simplifies WebRTC NAT handling significantly.

### 12.2 Caddy (if not using CF Tunnel)

```caddyfile
stream.yourdomain.com {
    reverse_proxy localhost:3000
}
```

### 12.3 Cloudflare Tunnel

```yaml
tunnel: chromatic
credentials-file: /etc/cloudflared/creds.json

ingress:
  - hostname: stream.yourdomain.com
    service: http://localhost:3000
  - service: http_status:404
```

**Note:** CF Tunnel handles HTTP/WS only. WebRTC media uses direct UDP or TURN.

### 12.4 turnserver.conf

```conf
listening-port=3478
tls-listening-port=5349
fingerprint
lt-cred-mech
use-auth-secret
static-auth-secret=${TURN_SECRET}
realm=${TURN_REALM}

# CRITICAL for NAT traversal
external-ip=${PUBLIC_IP}

total-quota=100
bps-capacity=0
stale-nonce
no-multicast-peers

# Block private ranges
denied-peer-ip=10.0.0.0-10.255.255.255
denied-peer-ip=172.16.0.0-172.31.255.255
denied-peer-ip=192.168.0.0-192.168.255.255

log-file=/var/log/coturn.log
simple-log
```

### 12.5 Environment Variables

```bash
# .env

# Admin authentication
ADMIN_TOKEN=              # openssl rand -hex 32

# Public URL for WebSocket and signaling
PUBLIC_URL=https://stream.yourdomain.com

# TURN configuration
TURN_SECRET=              # openssl rand -hex 32
TURN_REALM=stream.yourdomain.com
PUBLIC_IP=                # Your server's public IP

# Optional: External TURN fallback
TURN_EXTERNAL_URL=
TURN_EXTERNAL_USER=
TURN_EXTERNAL_PASS=
```

---

## 13. Session Lifecycle & Timeouts

### 13.1 Session States

```
pending ──[OBS connects]──▶ live ──[admin ends OR timeout]──▶ ended
```

### 13.2 Timeout Policy

**OBS Disconnect Handling:**
1. OBS disconnects (WHIP session ends).
2. Server starts 5-minute reconnection timer.
3. Clients see "Stream offline. Waiting for host to reconnect..."
4. If OBS reconnects within 5 minutes: resume normally.
5. If timeout expires:
   - Server broadcasts `room:ended`.
   - All clients disconnect.
   - Room status set to `ended`.

**Idle Client Handling:**
1. If client WebSocket silent for 60 seconds: send ping.
2. If no pong within 10 seconds: consider disconnected.
3. Clean up participant record.

---

## 14. Security

### 14.1 Authentication Summary

| Endpoint | Method |
|----------|--------|
| Admin API | Bearer token (static, hashed in config) |
| WHIP ingest | Stream key in URL path |
| Room join | Optional password (bcrypt) |
| WebSocket | Short-lived join token |
| File access | Valid session required |

### 14.2 Rate Limits

| Action | Limit |
|--------|-------|
| Room creation | 10/hour |
| Join attempts (per IP) | 20/minute |
| Password attempts (per room, per IP) | 5/minute |
| File uploads (per session) | 10/minute |
| Chat messages | 30/minute |
| Cursor updates | 20/second (enforced client-side) |

### 14.3 Input Validation

- Room slugs: `^[a-z0-9-]{3,64}$`
- Display names: 1–50 chars, HTML sanitized
- Chat messages: 1–2000 chars, HTML sanitized
- Files: whitelist MIME types, max 5MB

---

## 15. Performance Targets

| Metric | Target |
|--------|--------|
| Glass-to-glass latency | < 500ms |
| Time to first frame | < 1s after WebRTC connected |
| Join to viewing | < 3s |
| Chat delivery | < 100ms |
| Cursor sync | < 100ms |
| File upload (1MB) | < 2s |
| Server memory (8 clients) | < 512MB |
| Server CPU (8 clients) | < 25% single core |

---

## 16. Browser Support

| Browser | Tier | Notes |
|---------|------|-------|
| Safari 15+ (macOS) | Reference | Best color management |
| Chrome 90+ (macOS) | Primary | Most users, color warning shown |
| Chrome 90+ (Windows) | Supported | Gamma shifts possible |
| Edge 90+ | Supported | Chromium-based |
| Firefox 90+ | Degraded | WebRTC quirks, discouraged |
| Mobile Safari | Supported | Voice only, no camera |
| Mobile Chrome | Supported | Voice only, no camera |

---

## 17. Development Phases

### Phase 1: Core Infrastructure (Week 1–2)
- [ ] Go server skeleton with Pion WebRTC
- [ ] WHIP ingest with B-frame validation (parse SDP)
- [ ] Basic SFU: OBS → subscribers
- [ ] SQLite schema with WAL mode
- [ ] Docker setup with host networking
- [ ] Verify color pipeline OBS → Chrome on Mac

### Phase 2: Room Management (Week 2–3)
- [ ] Stream key CRUD
- [ ] Room CRUD with stream key binding
- [ ] Scheduling logic (10-min early access)
- [ ] Password protection
- [ ] Waiting room

### Phase 3: Client Viewer (Week 3–4)
- [ ] SvelteKit project
- [ ] Join flow (password, name, device setup)
- [ ] Stream viewer with WebRTC
- [ ] Audio ducking implementation
- [ ] Volume controls (stream/voice separate)

### Phase 4: Communication (Week 4–5)
- [ ] Voice chat integration
- [ ] Video chat with simulcast
- [ ] Priority-based bandwidth management
- [ ] Text chat
- [ ] File upload and display

### Phase 5: Interactive Features (Week 5–6)
- [ ] Laser pointer (all clients)
- [ ] Latency display for admin
- [ ] Admin controls (mute, kick, etc.)
- [ ] Browser detection + Safari prompt

### Phase 6: Watermarking & Polish (Week 6–7)
- [ ] Text watermark rendering
- [ ] Logo watermark rendering
- [ ] MutationObserver protection
- [ ] Mobile responsive refinement

### Phase 7: Hardening (Week 7–8)
- [ ] Error handling and reconnection
- [ ] Rate limiting
- [ ] Timeout handling
- [ ] Documentation
- [ ] Deployment guide

---

## Appendix A: Glossary

| Term | Definition |
|------|------------|
| SFU | Selective Forwarding Unit — routes media without transcoding |
| WHIP | WebRTC HTTP Ingest Protocol — standard for WebRTC publishing |
| ICE | Interactive Connectivity Establishment — NAT traversal |
| STUN | Discovers public IP address |
| TURN | Relays media when direct connection fails |
| VAD | Voice Activity Detection |
| XDR | Extreme Dynamic Range (Apple display technology) |
| Glass-to-glass | Total latency from capture to display |

---

## Appendix B: Future Considerations

Out of scope for v1, architecturally possible later:

1. **Recording** — Server-side session recording
2. **Playback** — Review past sessions
3. **Multi-room** — Single OBS feeding multiple isolated rooms
4. **Multi-colorist** — Separate admin accounts
5. **Timecode overlay** — Display Resolve timecode in UI
6. **Annotations** — Draw on frames (requires canvas sync)
7. **Reactions** — Emoji overlays
8. **Native mobile app** — Better camera/mic handling

---

## Appendix C: Implementation Notes

### C.1 Project Structure

```
chromatic/
├── cmd/chromatic/              # Application entrypoint
├── internal/
│   ├── config/                 # Environment configuration
│   ├── database/               # SQLite + migrations
│   ├── models/                 # Data models
│   ├── api/                    # HTTP handlers
│   ├── webrtc/                 # Pion WebRTC, SFU, WHIP
│   ├── websocket/              # Real-time messaging hub
│   └── services/               # Business logic
├── web/                        # SvelteKit frontend
│   ├── src/routes/             # Pages
│   └── src/lib/                # Components, stores, utilities
├── deployments/                # Docker, Caddy, Coturn configs
└── docs/                       # User documentation
```

### C.2 Critical Path (Phase 1)

These items must work correctly before proceeding:

| Priority | Component | Validation |
|----------|-----------|------------|
| 1 | WHIP Endpoint | OBS connects, SDP negotiated |
| 2 | B-Frame Validation | Rejects misconfigured streams |
| 3 | SFU Track Forwarding | Video appears in test client |
| 4 | SQLite WAL Mode | Concurrent read/write works |
| 5 | Docker Host Networking | WebRTC UDP traversal works |

### C.3 Technology Versions

| Component | Version | Notes |
|-----------|---------|-------|
| Go | 1.22+ | Improved HTTP routing |
| Pion WebRTC | v4.x | Latest stable |
| SvelteKit | 2.x | With Svelte 5 runes |
| SQLite | 3.45+ | With WAL mode |
| Docker Compose | v2 | Compose v2 spec |

### C.4 Development Commands

```bash
# Backend
go run ./cmd/chromatic

# Frontend (development)
cd web && npm run dev

# Full stack (Docker)
docker compose -f deployments/docker-compose.dev.yml up

# Run tests
go test ./... -v -race
cd web && npm run test
```

### C.5 Key Design Decisions

1. **Go over Node.js**: Lower latency, better WebRTC integration via Pion, single binary deployment.

2. **SQLite over PostgreSQL**: Single-user deployment, no need for connection pooling complexity, WAL mode handles concurrent reads.

3. **SvelteKit SSG over SPA**: Static build embedded in Go binary, no Node.js runtime in production.

4. **Host Networking**: Simplifies WebRTC NAT traversal, acceptable for single-user colorist deployment.

5. **Canvas Watermarks**: Client-side rendering with MutationObserver protection is "good enough" deterrent without server-side processing overhead.

---

## Appendix D: Quick Reference

### OBS WHIP URL Format
```
https://{domain}/whip/{stream_key_token}
```

### WebSocket Connection URL
```
wss://{domain}/ws/room/{slug}?token={join_token}&name={display_name}
```

### Admin API Authentication
```
Authorization: Bearer {ADMIN_TOKEN}
```

### TURN Credential Generation
```go
// Time-limited TURN credentials using shared secret
username := fmt.Sprintf("%d:%s", time.Now().Add(12*time.Hour).Unix(), username)
credential := base64.StdEncoding.EncodeToString(
    hmac.New(sha1.New, []byte(turnSecret)).Sum([]byte(username)),
)
```