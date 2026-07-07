# Chromatic API Documentation

## Authentication

Chromatic uses httpOnly session cookies for secure authentication (not vulnerable to XSS).

### Login

```
POST /api/auth/login
```

**Request Body:**
```json
{
  "token": "your-admin-token"
}
```

**Response:**
```json
{
  "success": true,
  "message": "Login successful"
}
```

The response sets an httpOnly cookie `chromatic_session` that authenticates subsequent requests.

### Logout

```
POST /api/auth/logout
```

**Response:**
```json
{
  "success": true,
  "message": "Logged out"
}
```

---

## Rooms

### List Rooms

```
GET /api/rooms
GET /api/rooms?status=live
```

**Query Parameters:**
- `status` (optional): Filter by status (`pending`, `live`, `ended`)

**Response:**
```json
[
  {
    "id": "uuid",
    "slug": "my-session",
    "name": "Color Review Session",
    "status": "pending",
    "hasPassword": true,
    "waitingRoomEnabled": true,
    "watermarkMode": "text",
    "createdAt": "2025-01-24T10:00:00Z"
  }
]
```

### Create Room

```
POST /api/rooms
```

**Request Body:**
```json
{
  "slug": "my-session",
  "name": "Color Review Session",
  "password": "optional-password",
  "waitingRoomEnabled": true,
  "streamKeyId": "uuid-of-stream-key",
  "watermarkMode": "text",
  "watermarkText": "{{ name }} - {{ date }}",
  "watermarkLogoPosition": "bottom-right",
  "watermarkOpacity": 0.3
}
```

**Watermark Modes:**
- `none`: No watermark
- `text`: Text watermark only
- `logo`: Logo watermark only
- `both`: Text and logo watermark

**Watermark Text Variables:**
- `{{ name }}`: Viewer's display name
- `{{ room }}`: Room name
- `{{ date }}`: Current date
- `{{ time }}`: Current time

**Response:** Created room object with `201 Created`

### Get Room

```
GET /api/rooms/{slug}
```

**Response:**
```json
{
  "id": "uuid",
  "slug": "my-session",
  "name": "Color Review Session",
  "status": "live",
  "hasPassword": true,
  "waitingRoomEnabled": true,
  "streamKeyId": "uuid",
  "watermarkMode": "text",
  "watermarkText": "{{ name }} - {{ date }}",
  "createdAt": "2025-01-24T10:00:00Z",
  "startedAt": "2025-01-24T11:00:00Z"
}
```

### Update Room

```
PATCH /api/rooms/{slug}
```

**Request Body (all fields optional):**
```json
{
  "name": "Updated Name",
  "waitingRoomEnabled": false,
  "watermarkMode": "logo",
  "watermarkLogoPosition": "top-left"
}
```

### Delete Room

```
DELETE /api/rooms/{slug}?deleteFiles=true
```

**Query parameters:**
- `deleteFiles` (optional) — when `true`, also removes the room's uploaded
  files and thumbnails from disk. Database rows cascade either way.

**Response:** `204 No Content`

### End Session

```
POST /api/rooms/{slug}/end
```

Ends the current streaming session, disconnecting all viewers.

**Response:** `204 No Content`

---

## Public Room Endpoints

These endpoints don't require admin authentication.

### Get Room Info

```
GET /api/rooms/{slug}/info
```

Returns public information about a room (for join page).

**Response:**
```json
{
  "name": "Color Review Session",
  "hasPassword": true,
  "waitingRoomEnabled": true,
  "status": "live"
}
```

### Join Room

```
POST /api/rooms/{slug}/join
```

**Request Body:**
```json
{
  "name": "Viewer Name",
  "password": "room-password-if-required"
}
```

**Response:**
```json
{
  "participantId": "uuid",
  "isAdmitted": false,
  "waitingRoom": true,
  "color": "#4F46E5",
  "name": "Viewer Name",
  "role": "viewer",
  "serverTime": "2026-07-01T12:00:00Z"
}
```

The response also sets a room-scoped HttpOnly cookie
`chromatic_join_{slug}`. In production mode the signed join token is not
returned in the JSON body, so browser clients should authenticate subsequent
status, upload, file, SSE, and WebSocket requests with that cookie. Development
mode may include a `token` field for local tooling compatibility.

### Check Participant Status

```
GET /api/rooms/{slug}/status/{participantId}
```

Used to poll for admission status in waiting room.
Browser clients authenticate with the room join cookie. Non-browser clients may
send `X-Join-Token: <signed-token>` when they were issued a token out of band.

**Response:**
```json
{
  "isAdmitted": true,
  "roomStatus": "live"
}
```

---

## Waiting Room Management

### List Waiting Participants

```
GET /api/rooms/{slug}/waiting
```

**Response:**
```json
[
  {
    "id": "uuid",
    "name": "John Doe",
    "joinedAt": "2025-01-24T11:05:00Z"
  }
]
```

### Admit Participant

```
POST /api/rooms/{slug}/admit/{participantId}
```

**Response:** `204 No Content`

### Admit All

```
POST /api/rooms/{slug}/admit-all
```

**Response:** `204 No Content`

---

## Stream Keys

### List Stream Keys

```
GET /api/stream-keys
```

**Response:**
```json
[
  {
    "id": "uuid",
    "name": "Main Camera",
    "keyToken": "abc123...",
    "createdAt": "2025-01-24T09:00:00Z"
  }
]
```

### Create Stream Key

```
POST /api/stream-keys
```

**Request Body:**
```json
{
  "name": "Main Camera"
}
```

**Response:** Created stream key with `201 Created`

### Delete Stream Key

```
DELETE /api/stream-keys/{id}
```

**Response:** `204 No Content`

---

## Configuration

### Get Configuration

```
GET /api/config
```

**Response:**
```json
{
  "defaultWatermarkText": "{{ name }} - {{ date }}",
  "defaultWatermarkLogoUrl": "/api/config/logo",
  "turnExternalUrl": "turn:global.turn.twilio.com:3478",
  "turnExternalUsername": "username",
  "hasTurnCredential": true,
  "turnMode": "hybrid",
  "turnCloudflareConfigured": false,
  "publicUrl": "https://stream.example.com",
  "whipFormat": "https://stream.example.com/whip/{stream_key_token}"
}
```

`turnMode` is one of `self-hosted`, `external`, or `hybrid`.
`turnCloudflareConfigured` reports whether Cloudflare TURN credentials are
present in the environment. `turnExternalUrl`/`turnExternalUsername` reflect the
effective static TURN settings (DB override with per-field environment
fallback), so the displayed config matches the tested and running config.

### Update Configuration

```
PATCH /api/config
```

**Request Body:**
```json
{
  "defaultWatermarkText": "{{ name }} - {{ room }}",
  "turnExternalUrl": "turn:server.com:3478",
  "turnExternalUsername": "user",
  "turnExternalCredential": "secret"
}
```

### Upload Default Logo

```
POST /api/config/logo
```

**Content-Type:** `multipart/form-data`

**Form Field:** `logo` (PNG, JPEG, or WebP, max 1MB)

### Get Default Logo

```
GET /api/config/logo
```

**Response:** Image file

### Delete Default Logo

```
DELETE /api/config/logo
```

**Response:** `204 No Content`

### Test TURN Reachability

```
POST /api/config/test-turn
```

Runs a server-side socket reachability test against the effective TURN servers
(self-hosted Coturn realm plus the effective external/static URLs). This is a
reachability check, not an authenticated TURN allocation or a browser NAT proof.
The result is persisted in `config` with a signature of the effective TURN
settings so the setup status can tell whether a stored test is still valid;
saving any TURN field clears that stored test.

**Response:**
```json
{
  "success": true,
  "results": [
    {
      "server": "turn.example.com:3478",
      "reachable": true,
      "latency": 42,
      "protocol": "udp",
      "testType": "external"
    }
  ],
  "message": "At least one TURN endpoint is reachable from this server"
}
```

---

## Setup

The setup wizard status is owned by the backend and persisted in the `config`
table, not browser localStorage.

### Get Setup Status

```
GET /api/setup/status
```

Returns the server-computed setup status: completion/dismissal timestamps, the
rollup progress, the per-check results, and the install facts Chromatic can
derive from its own config and database.

**Response:**
```json
{
  "readyToComplete": false,
  "firstRun": true,
  "requiresAttention": false,
  "progress": { "ready": 3, "required": 6, "total": 7 },
  "checks": [
    {
      "id": "public-url",
      "title": "Public URL",
      "status": "ready",
      "required": true,
      "summary": "Local development URL"
    }
  ],
  "facts": {
    "publicUrl": "http://localhost:3000",
    "productionMode": false,
    "allowedOrigins": [],
    "turnMode": "hybrid",
    "turnCloudflareConfigured": false,
    "turnStaticConfigured": false,
    "hasTurnCredential": false,
    "turnLastTestSuccess": false,
    "turnLastTestValidForCurrentConfig": false,
    "streamKeyCount": 0,
    "roomCount": 0
  }
}
```

Check `status` values: `ready`, `needs-action`, `warning`, `optional`. Required
checks block `readyToComplete` until every one is `ready`; the optional
`branding` check never blocks completion. `firstRun` is true only when setup is
neither completed nor dismissed and no stream keys or rooms exist yet.
`requiresAttention` is true when setup was completed but a required check is no
longer ready.

### Complete Setup

```
POST /api/setup/complete
```

Stamps `setup_completed_at` (clearing any prior dismissal) and returns the
updated status. Returns `409 Conflict` with the current status if any required
check is not ready.

### Dismiss Setup

```
POST /api/setup/dismiss
```

Stamps `setup_dismissed_at` and returns the updated status.

---

## File Upload

### Upload File

```
POST /api/rooms/{slug}/files
```

**Content-Type:** `multipart/form-data`

**Headers:**
- `X-Participant-ID`: Uploader's participant ID

**Form Field:** `file` (max 5MB)

**Allowed Types:**
- `image/jpeg`, `image/png`, `image/gif`, `image/webp`
- `audio/mpeg`, `audio/wav`/`audio/wave`, `audio/ogg`/`application/ogg`
- `application/pdf`

**Response:**
```json
{
  "id": "uuid",
  "originalName": "screenshot.png",
  "mimeType": "image/png",
  "sizeBytes": 102400,
  "url": "/api/files/uuid",
  "thumbnailUrl": "/api/files/uuid/thumbnail"
}
```

### Download File

```
GET /api/files/{id}
```

**Response:** File content with appropriate Content-Type

### Get Thumbnail

```
GET /api/files/{id}/thumbnail
```

**Response:** JPEG thumbnail (images only)

### List Room Files (Admin)

```
GET /api/rooms/{slug}/files
```

Requires an admin session. Returns every file uploaded to the room, newest
first — used by the room-settings Files review section.

**Response:**
```json
{
  "files": [
    {
      "id": "uuid",
      "originalName": "still.png",
      "mimeType": "image/png",
      "sizeBytes": 123456,
      "uploaderName": "Jane",
      "createdAt": 1765400000000,
      "url": "/api/files/uuid",
      "thumbnailUrl": "/api/files/uuid/thumbnail"
    }
  ]
}
```

### Delete File (Admin)

```
DELETE /api/files/{id}
```

Requires an admin session. Removes the file and its thumbnail from disk,
deletes the database row, and removes chat messages that referenced it.

**Response:** `204 No Content`

---

## WHIP (WebRTC-HTTP Ingestion Protocol)

### Start Stream

```
POST /whip/{streamKeyToken}
```

**Content-Type:** `application/sdp`

**Request Body:** SDP Offer from OBS

**Response:** SDP Answer

This endpoint is used by OBS for WHIP streaming.

---

## WebSocket API

Connect to the WebSocket endpoint:

```
ws://host/ws/room/{slug}?name={viewerName}
```

Browser WebSocket upgrades authenticate with the room-scoped HttpOnly join
cookie set by `POST /api/rooms/{slug}/join`. Production mode ignores join
tokens in WebSocket query strings to avoid leaking credentials into logs,
history, and referrers.

### Client → Server Messages

All messages are JSON: `{"type": "...", "payload": {...}}`.

#### Media signaling (subscriber connection — server is the only offerer)

| Type | Payload | Purpose |
|------|---------|---------|
| `signal:answer` | `{sdp}` | Answer to the server's subscription offer |
| `signal:candidate` | `{candidate, sdpMid, sdpMLineIndex}` | Trickled ICE candidate |
| `signal:renegotiate-answer` | `{sdp}` | Answer to a server renegotiation offer (`signal:renegotiate`) |
| `signal:ice-restart` | `{sdp}` | Client-initiated ICE restart offer (connection recovery) |
| `signal:resync` | `{}` | Request a keyframe from the publisher (video not rendering) |
| `signal:resubscribe` | `{}` | Tear down and rebuild the server-side subscriber (unrecoverable media path) |
| `signal:ice-servers-request` | `{}` | Request fresh ICE servers/TURN credentials (long sessions) |
| `signal:offer` | `{sdp}` | Legacy: client offer on the subscriber PC (superseded by `publish:offer`) |

#### Publishing (dedicated publisher connection — client is the only offerer)

Microphone audio and screen-share video are sent over a separate peer
connection negotiated with these messages. The server only answers, so the
publisher path has no signaling glare.

| Type | Payload | Purpose |
|------|---------|---------|
| `publish:offer` | `{sdp}` | Offer for the publisher PC (initial or renegotiation, e.g. adding a share track) |
| `publish:candidate` | `{candidate, sdpMid, sdpMLineIndex}` | Trickled ICE candidate for the publisher PC |

#### Collaboration

| Type | Payload | Purpose |
|------|---------|---------|
| `cursor` | `{points: [{x, y}...], active, release?, surface?}` | Laser pointer batch; coordinates normalized 0–1 to the video content. `surface` is `"video"` (default) or `"share"` |
| `chat:send` | `{content}` | Text chat message (max 2000 chars) |
| `chat:file` | `{fileId}` | Share a previously uploaded file in chat |
| `media:toggle` | `{audio}` | Broadcast local mic on/off state |
| `screenshare:request` | `{}` | Ask to share the screen (admins auto-approved) |
| `screenshare:stop` | `{}` | Stop the active screen share (sharer or admin) |
| `client:debug` | `{event, detail}` | Client-side diagnostic breadcrumb, mirrored to the server log |

#### Admin commands

| Type | Payload | Purpose |
|------|---------|---------|
| `admin:mute` | `{participantId}` | Server-enforced mute of a participant's voice |
| `admin:kick` | `{participantId}` | Remove a participant from the session |
| `admin:end-session` | `{}` | End the session for everyone |
| `admin:delete-message` | `{messageId}` | Delete a chat message for everyone (moderation) |
| `admin:waiting-approve` / `admin:waiting-deny` | `{participantId}` | Resolve a waiting-room request |
| `admin:screenshare-approve` / `admin:screenshare-deny` / `admin:screenshare-revoke` | `{participantId}` | Manage screen-share permission |

### Server → Client Messages

| Type | Payload | Purpose |
|------|---------|---------|
| `room:state` | `{room, participants, isLive, iceServers, ...}` | Full state on connect/reconnect |
| `iceServers` | `[{urls, username?, credential?}...]` | ICE servers for the WebRTC connections |
| `signal:ice-servers` | `{iceServers}` | Refreshed TURN credentials (periodic) |
| `signal:offer` | `{sdp}` | Fresh subscription offer (new server-side subscriber) |
| `signal:renegotiate` | `{sdp, participantId?}` | Renegotiation offer (voice/share track added or removed) |
| `signal:answer` | `{sdp}` | Answer to a client ICE-restart offer |
| `signal:candidate` | `{candidate, sdpMid, sdpMLineIndex}` | Trickled server ICE candidate |
| `signal:error` | `{code, message}` | Subscription setup failed (client retries/resubscribes) |
| `signal:voice-answer` | `{sdp}` | Legacy answer to a client `signal:offer` |
| `publish:answer` | `{sdp}` | Answer to a `publish:offer` |
| `publish:error` | `{message}` | Publisher negotiation failed |
| `cursor` | `{participantId, participantName, color, points, x, y, active, release, surface}` | Laser pointer update from another participant |
| `chat:history` | `{messages: [...]}` | Last 50 messages on join/reconnect |
| `chat:message` | `{id, participantId, participantName, type, content, file?, timestamp}` | New chat message |
| `chat:message-deleted` | `{id}` | A message was removed by an admin |
| `participant:joined` / `participant:left` / `participant:updated` | `{participant}` / `{participantId}` / `{participant}` | Presence updates |
| `room:live` / `room:ended` | `{}` | Stream lifecycle |
| `stream:paused` / `stream:resumed` | `{message?}` | OBS disconnected / reconnected |
| `admin:muted` | `{participantId}` | A participant was muted by an admin |
| `kicked` | `{reason?}` | You were removed from the session |
| `screenshare:pending` | `{participantId, name}` | (Admins) someone requests to share |
| `screenshare:approved` / `screenshare:denied` | `{}` / `{reason?}` | Your share request was resolved |
| `screenshare:started` / `screenshare:stopped` | `{participantId, name}` / `{}` | Share lifecycle |
| `waiting:joined` / `waiting:list` / `waiting:resolved` | `{participantId, name}` / `{participants}` / `{participantId, action}` | (Admins) waiting-room queue |
| `lobby:count` | `{count}` | (Admins) countdown-lobby headcount for scheduled rooms |

---

## Rate Limits

Chromatic implements granular rate limiting to protect against abuse:

### HTTP Endpoints

| Endpoint | Limit | Description |
|----------|-------|-------------|
| General API | 20 req/sec/IP | Token bucket with burst of 20 |
| Room Creation | 10/hour/IP | Prevents room spam |
| Room Join (Password) | 5/min/IP/room | Prevents password brute-force |
| File Upload | 10/min/IP | Prevents upload spam |

### WebSocket Messages

| Message Type | Limit | Description |
|--------------|-------|-------------|
| Chat Messages | 30/min/user | Prevents chat spam |
| Cursor Updates | 50/sec | Client-side throttled to 20Hz |

### File Size Limits

| File Type | Limit |
|-----------|-------|
| File Upload | 5MB max |
| Logo Upload | 1MB max |

When rate limited, the server returns `429 Too Many Requests` with a `Retry-After` header indicating when to retry.

## Error Codes

| Status | Description |
|--------|-------------|
| 400 | Bad Request - Invalid input |
| 401 | Unauthorized - Missing or invalid authentication |
| 403 | Forbidden - Not allowed to perform action |
| 404 | Not Found - Resource doesn't exist |
| 429 | Too Many Requests - Rate limit exceeded |
| 500 | Internal Server Error |

## Error Response Format

```json
{
  "error": "Description of the error"
}
```
