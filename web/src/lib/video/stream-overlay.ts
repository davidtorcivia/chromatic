// Stream overlay state machine (BUG 1).
//
// The session page used to decide between the waiting overlay and the video
// with a chain of independent booleans, and the critical one (`hasStream`)
// was only flipped inside videoElement.play()'s promise callbacks. On a page
// reload there is no user gesture and play() on a MediaStream-backed element
// does not settle until playback actually begins (which needs a decodable
// keyframe) — so a viewer whose negotiation completed perfectly could sit on
// "the host hasn't started streaming yet" forever.
//
// This pure function makes the overlay decision explicit and testable:
//  - "live" is derived from the room status (room:state / room:live) OR from
//    track arrival — a late joiner / reloader whose tracks arrive is
//    connecting to a live stream even if it never saw a live broadcast.
//  - autoplay rejection surfaces as 'needs-click' (the tap-to-play card),
//    never the waiting copy.

export type StreamOverlayState =
    | 'error' // unrecoverable stream/signaling error — show refresh card
    | 'connection-lost' // WS gone, reconnect attempts exhausted
    | 'reconnecting' // WS reconnect in progress
    | 'needs-click' // autoplay blocked — show tap-to-play card
    | 'paused' // host temporarily disconnected (stream:paused)
    | 'playing' // video is rendering — no overlay
    | 'connecting' // room is live (or tracks arrived) but video not playing yet
    | 'waiting'; // room not live — host hasn't started streaming

export interface StreamOverlayInputs {
    /** Unrecoverable error message, if any (signal:error, attach failure). */
    streamError: string | null;
    /** WebSocket lost with no further reconnect attempts. */
    connectionLost: boolean;
    /** WebSocket reconnect in progress. */
    reconnecting: boolean;
    /** videoElement.play() was rejected by the autoplay policy. */
    needsPlayClick: boolean;
    /** Host publisher temporarily gone (stream:paused / stream:resumed). */
    streamPaused: boolean;
    /** Room status is live per room:state / room:live. */
    roomLive: boolean;
    /** Media tracks have arrived and are bound to the video element. */
    hasStream: boolean;
    /** The video element fired 'playing' (frames are rendering). */
    isVideoPlaying: boolean;
}

export function deriveStreamOverlayState(i: StreamOverlayInputs): StreamOverlayState {
    if (i.streamError) return 'error';
    if (i.connectionLost) return 'connection-lost';
    if (i.reconnecting) return 'reconnecting';
    if (i.needsPlayClick) return 'needs-click';
    if (i.streamPaused) return 'paused';
    if (i.hasStream && i.isVideoPlaying) return 'playing';
    // Live is room status OR track arrival — never show "host hasn't started"
    // when media is demonstrably flowing to us.
    if (i.roomLive || i.hasStream) return 'connecting';
    return 'waiting';
}
