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
    // Hand the voice track to the voice-playback manager (own gain path)
  }
};
```

### Server-Enforced Mute

`admin:mute` flips a server-side gate on the speaker's relay: incoming RTP
is dropped at the SFU, so muting works even if the muted client ignores it.

## Program vs Voice Playback

Chromatic is a color-critical review tool, so the program stream (the OBS/WHIP
ingest carrying the Resolve playback — dialogue, music, the full mix) is
relayed as an untouched Opus stream and played back exactly as provided. It is
**never** automatically ducked, gated, or remixed in response to voice.

### What the voice-playback manager does

`VoicePlaybackManager` (`web/src/lib/audio/voice-playback.ts`) plays each remote
participant's voice through its own WebAudio gain node — independent of the
program element — so the listener's voice-volume slider does not affect the
program, and vice versa. It owns the explicit user program-volume slider too.
The ONLY things that change the program element's level are the user's explicit
volume control and the browser's own autoplay mute.

```javascript
// Voice track handling (in session/+page.svelte)
function handleVoiceTrack(participantId: string, track: MediaStreamTrack) {
  voicePlaybackManager?.addVoiceTrack(participantId, track);
}
```

### What it does NOT do

There is no voice-activity-driven ducking, no attack/hold/release ramps, and no
admin exemption. Earlier versions automatically reduced the program to 20% under
detected speech; that fought the reviewer during critical listening and was
removed. The speaking indicator still runs (a separate VAD analyser feeds the
UI tile only — it never touches program volume).

### Chromium decoder sink

A remote WebRTC audio track consumed only through WebAudio decodes to silence in
Chromium unless it also has a media-element sink. Each voice track is therefore
attached to a muted `<audio>` element that exists solely to drive the decoder;
the audible path is the WebAudio gain graph.

## Troubleshooting

### Voice Not Being Received

1. **Check microphone permissions**: Browser must have microphone access
2. **Check peer connection state**: Ensure WebRTC connection is established
3. **Check console for errors**: Look for `Failed to negotiate publisher` messages (client) or `Failed to handle publish offer` (server logs)

### Voice Playback Not Working

1. **Check voice track detection**: Track ID should start with `voice-`
2. **Check VoicePlaybackManager initialization**: Ensure the manager is created
3. **Check audio context state**: Context must be "running" (user interaction required)

### Program Audio Sounds Mono / Wrong Level

Program audio is relayed untouched as stereo Opus. If it sounds mono or at the
wrong level, check the user volume slider and the browser autoplay mute — those
are the only things that affect program playback. Verify OBS is exporting stereo
(Chromatic cannot undo an upstream OBS mixdown).

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

- Voice (talkback) tracks use Opus (48 kHz, mono, DTX/FEC) at a voice bitrate; studio mic mode requests stereo Opus at a high bitrate for reference music/instruments
- Typical talkback bitrate: 32-64 kbps per voice stream
- Program audio is uncapped stereo Opus, relayed without server gain/ducking
- The speaking-indicator VAD uses minimal CPU (< 1%); it never affects program volume

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
