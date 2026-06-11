# Voice Chat System

Chromatic supports two-way voice communication between the host and viewers.

## Overview

The voice chat system uses WebRTC for peer-to-peer audio communication. When a viewer enables their microphone, their audio is forwarded to all other participants in the room through the SFU (Selective Forwarding Unit).

## Architecture

```
┌─────────────┐     Voice Offer      ┌─────────────┐
│   Viewer    │ ──────────────────>  │     SFU     │
│ (Microphone)│ <──────────────────  │   Server    │
└─────────────┘     Voice Answer     └──────┬──────┘
                                            │
                         Renegotiation      │
                         Offers             │
                    ┌───────────────────────┼───────────────────────┐
                    │                       │                       │
                    v                       v                       v
             ┌──────────┐            ┌──────────┐            ┌──────────┐
             │ Viewer A │            │ Viewer B │            │   Host   │
             └──────────┘            └──────────┘            └──────────┘
```

## Message Flow

Outgoing media (microphone, screen share) rides a dedicated **publisher peer
connection** where the client is the only offerer and the server only
answers. The subscriber connection (stream + received voice) is the mirror
image: the server is the only offerer. Keeping each connection's offer
direction fixed eliminates signaling glare entirely (mixed-direction offers
caused unrecoverable wedges in both Chrome and Safari).

### 1. Viewer Enables Microphone

When a viewer's microphone is enabled:

1. Browser requests microphone permission (audio is routed through a light
   cleanup chain: high-pass filter + soft noise gate, on top of the
   browser's `noiseSuppression`/`echoCancellation` constraints)
2. Client lazily creates the publisher RTCPeerConnection
3. Client adds the audio track and sends `publish:offer`

```json
{
  "type": "publish:offer",
  "payload": {
    "sdp": "v=0\r\no=- ..."
  }
}
```

ICE candidates for the publisher trickle via `publish:candidate`.

### 2. Server Answers

The SFU creates (or renegotiates, if one already exists) the participant's
publisher peer connection and answers immediately:

```json
{
  "type": "publish:answer",
  "payload": {
    "sdp": "v=0\r\no=- ..."
  }
}
```

If the answer never arrives (lost message, wedged connection), the client
rebuilds the publisher from scratch with its current tracks — the publisher
carries no inbound state, so this is always safe.

### 3. Voice Track Forwarding

When the SFU receives the voice audio track:

1. It creates one shared relay track per speaker and fans it out
2. For each other participant, the track is added to their subscriber
   connection and a renegotiation offer is sent (`signal:renegotiate`)
3. If a subscriber's signaling is busy, the track is attached anyway and the
   offer is deferred until the in-flight exchange settles — voice tracks are
   never dropped

```json
{
  "type": "signal:renegotiate",
  "payload": {
    "sdp": "v=0\r\no=- ...",
    "participantId": "voice-sender-id"
  }
}
```

### 4. Client Receives Voice Track

1. Client receives the renegotiation offer on the subscriber connection
2. Client sets remote description, creates an answer
3. Client sends `signal:renegotiate-answer`

The voice track is received in the `ontrack` handler with an ID prefixed with `voice-`:

```javascript
pc.ontrack = (event) => {
  const trackId = event.track.id;
  if (trackId.startsWith('voice-')) {
    const participantId = trackId.substring(6);
    // Handle voice track (e.g., for audio ducking)
  }
};
```

### Server-Enforced Mute

`admin:mute` flips a server-side gate on the speaker's relay: incoming RTP
is dropped at the SFU, so muting works even if the muted client ignores it.

## Audio Ducking

Audio ducking automatically reduces the stream volume when someone is speaking.

### How It Works

1. `AudioDuckingManager` monitors all voice audio tracks
2. Uses Voice Activity Detection (VAD) to detect speech
3. When speech is detected, main stream volume is reduced to 20%
4. When speech stops, volume smoothly returns to 100%

### Implementation Details

```javascript
// Voice track detection (in WebRTCManager)
if (trackId.startsWith('voice-')) {
  const participantId = trackId.substring(6);
  onVoiceTrack(participantId, event.track);
}

// Audio ducking (in session/+page.svelte)
function handleVoiceTrack(participantId: string, track: MediaStreamTrack) {
  audioDuckingManager?.addVoiceTrack(participantId, track);
}
```

### Parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| Duck level | 20% | Volume when someone is speaking |
| Ramp time | 100ms | Fade duration for volume changes |
| Release time | 300ms | Time after speech stops before returning |
| VAD threshold | -50dB | Sound level to detect speech |

## Troubleshooting

### Voice Not Being Received

1. **Check microphone permissions**: Browser must have microphone access
2. **Check peer connection state**: Ensure WebRTC connection is established
3. **Check console for errors**: Look for `Failed to negotiate publisher` messages (client) or `Failed to handle publish offer` (server logs)

### Audio Ducking Not Working

1. **Check voice track detection**: Track ID should start with `voice-`
2. **Check AudioDuckingManager initialization**: Ensure manager is created
3. **Check audio context state**: Context must be "running" (user interaction required)

### High Latency

1. **Check TURN server**: Direct connection should be preferred
2. **Check network quality**: WebRTC handles varying network conditions
3. **Check audio processing**: Disable unnecessary audio processing

### Echo or Feedback

1. **Enable echo cancellation**: Browser's built-in AEC
2. **Use headphones**: Prevents speaker-to-mic feedback
3. **Reduce microphone sensitivity**: In browser/OS settings

## WebSocket Message Reference

### Client to Server

| Type | Description |
|------|-------------|
| `publish:offer` | Publisher SDP offer (mic enabled / share added) |
| `publish:candidate` | Trickled ICE candidate for the publisher |
| `signal:renegotiate-answer` | Answer to a subscriber renegotiation offer |
| `media:toggle` | Broadcast local mic on/off state |

### Server to Client

| Type | Description |
|------|-------------|
| `publish:answer` | Answer to the client's publisher offer |
| `publish:error` | Publisher negotiation failed |
| `signal:renegotiate` | Subscriber offer with a new voice track added |
| `admin:muted` | A participant was muted by an admin |

## Performance Considerations

- Voice tracks use Opus codec (48kHz, mono)
- Typical bitrate: 32-64 kbps per voice stream
- VAD processing uses minimal CPU (< 1%)
- Audio ducking adds negligible latency (< 10ms)

## Browser Support

Voice chat requires:
- WebRTC support
- getUserMedia API
- AudioContext API (for VAD)

Supported browsers:
- Chrome 70+
- Firefox 65+
- Safari 14+
- Edge 79+
