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

### 1. Viewer Enables Microphone

When a viewer clicks the microphone button:

1. Browser requests microphone permission
2. Client creates a new RTCPeerConnection for voice
3. Client creates an SDP offer with the audio track
4. Client sends `signal:offer` to server

```json
{
  "type": "signal:offer",
  "payload": {
    "sdp": "v=0\r\no=- ..."
  }
}
```

### 2. Server Processes Voice Offer

1. SFU creates a peer connection to receive the voice audio
2. SFU sends back `signal:voice-answer` to the client

```json
{
  "type": "signal:voice-answer",
  "payload": {
    "sdp": "v=0\r\no=- ..."
  }
}
```

### 3. Voice Track Forwarding

When the SFU receives the voice audio track:

1. For each other participant in the room:
   - SFU adds the voice track to their subscriber connection
   - SFU generates a renegotiation offer
   - SFU sends `signal:renegotiate` to the participant

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

1. Client receives renegotiation offer
2. Client sets remote description
3. Client creates answer
4. Client sends `signal:renegotiate-answer`

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
3. **Check console for errors**: Look for `Failed to handle voice offer` messages

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
| `signal:offer` | Voice SDP offer (client mic enabled) |
| `signal:renegotiate-answer` | Answer to renegotiation offer |

### Server to Client

| Type | Description |
|------|-------------|
| `signal:voice-answer` | Answer to client's voice offer |
| `signal:renegotiate` | Offer with new voice track added |

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
