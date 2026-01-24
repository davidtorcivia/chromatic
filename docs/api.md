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
DELETE /api/rooms/{slug}
```

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
  "token": "jwt-token-for-websocket",
  "isAdmitted": false,
  "waitingRoom": true,
  "color": "#4F46E5"
}
```

### Check Participant Status

```
GET /api/rooms/{slug}/status/{participantId}
```

Used to poll for admission status in waiting room.

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
  "publicUrl": "https://stream.example.com",
  "whipFormat": "https://stream.example.com/whip/{stream_key_token}"
}
```

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
- `audio/mpeg`, `audio/wav`, `audio/ogg`
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
ws://host/ws/room/{slug}?token={jwt}&name={viewerName}
```

### Client → Server Messages

#### Cursor Update (Laser Pointer)
```json
{
  "type": "cursor:update",
  "payload": {
    "x": 0.5,
    "y": 0.3,
    "active": true
  }
}
```

#### Chat Message
```json
{
  "type": "chat:message",
  "payload": {
    "content": "Hello everyone!"
  }
}
```

#### Chat File
```json
{
  "type": "chat:file",
  "payload": {
    "fileId": "uuid",
    "fileName": "screenshot.png",
    "mimeType": "image/png",
    "url": "/api/files/uuid",
    "thumbnailUrl": "/api/files/uuid/thumbnail"
  }
}
```

#### Request WebRTC Subscription
```json
{
  "type": "webrtc:subscribe",
  "payload": {}
}
```

#### WebRTC Answer
```json
{
  "type": "webrtc:answer",
  "payload": {
    "sdp": "v=0..."
  }
}
```

#### ICE Candidate
```json
{
  "type": "webrtc:ice",
  "payload": {
    "candidate": "candidate:...",
    "sdpMid": "0",
    "sdpMLineIndex": 0
  }
}
```

### Server → Client Messages

#### Room State (on connect)
```json
{
  "type": "room:state",
  "payload": {
    "room": {
      "slug": "my-session",
      "name": "Color Review Session"
    },
    "participants": [...],
    "isLive": true,
    "iceServers": [
      {"urls": ["stun:stun.l.google.com:19302"]},
      {"urls": ["turn:server:3478"], "username": "u", "credential": "p"}
    ]
  }
}
```

#### Room Live
```json
{
  "type": "room:live",
  "payload": {}
}
```

#### Room Ended
```json
{
  "type": "room:ended",
  "payload": {}
}
```

#### Participant Joined
```json
{
  "type": "participant:joined",
  "payload": {
    "participant": {
      "id": "uuid",
      "name": "John",
      "color": "#4F46E5"
    }
  }
}
```

#### Participant Left
```json
{
  "type": "participant:left",
  "payload": {
    "participantId": "uuid"
  }
}
```

#### Cursor Update (from others)
```json
{
  "type": "cursor:update",
  "payload": {
    "participantId": "uuid",
    "participantName": "John",
    "color": "#4F46E5",
    "x": 0.5,
    "y": 0.3,
    "active": true
  }
}
```

#### Chat Message (from others)
```json
{
  "type": "chat:message",
  "payload": {
    "participantId": "uuid",
    "participantName": "John",
    "content": "Hello everyone!"
  }
}
```

#### WebRTC Offer
```json
{
  "type": "webrtc:offer",
  "payload": {
    "sdp": "v=0..."
  }
}
```

#### ICE Candidate
```json
{
  "type": "webrtc:ice",
  "payload": {
    "candidate": "candidate:...",
    "sdpMid": "0",
    "sdpMLineIndex": 0
  }
}
```

---

## Rate Limits

| Endpoint | Limit |
|----------|-------|
| General | 20 req/sec per IP |
| File Upload | 5MB max file size |
| Logo Upload | 1MB max file size |

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
