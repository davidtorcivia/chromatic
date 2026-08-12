// Keyframe-nudge escalation decision.
//
// When tracks bind but no frames render, the usual cause is a keyframe lost in
// flight, so the session page sends a few PLIs before escalating to a full
// re-subscription. The escalation used to fire on a precondition it never
// checked: `ontrack` runs during setRemoteDescription, so the nudge cycle is
// armed BEFORE ICE completes, and its three attempts expire 2450ms later. A
// viewer on a TURN relay needs about that long just to connect, so the cycle
// tore down a peer connection that was still coming up and would have
// succeeded. On 2026-08-11 that destroyed one subscription 2.45s after it was
// offered; its replacement took 2.43s to connect, and the initial join in the
// same session cleared the same fuse by roughly 140ms.
//
// A PLI cannot produce a frame while the transport is still coming up, so
// while the connection is 'new' or 'connecting' the lost-keyframe hypothesis
// is not yet testable: wait rather than spend attempts or escalate. A
// transport that never arrives is the connecting watchdog's job, on a budget
// that suits ICE rather than decoding.

export type KeyframeNudgeAction =
    | 'stop' // frames are rendering — the cycle is done
    | 'wait' // transport still coming up — re-poll without nudging
    | 'nudge' // connected but no frames — ask for a keyframe
    | 'escalate'; // nudges exhausted against a live transport — rebuild

export interface KeyframeNudgeInputs {
    /** The video element fired 'playing' (frames are rendering). */
    isVideoPlaying: boolean;
    /** Subscriber peer connection state; null when there is no peer connection. */
    connectionState: RTCPeerConnectionState | null;
    /** Nudges already sent in this cycle. */
    attempts: number;
    /** Nudges to send before escalating. */
    maxAttempts: number;
}

export function decideKeyframeNudge(i: KeyframeNudgeInputs): KeyframeNudgeAction {
    if (i.isVideoPlaying) return 'stop';
    // 'failed' and 'closed' deliberately fall through: they have their own
    // owner (the manager's ICE-restart chain) and must not be parked here.
    // A null state means there is no peer connection left to wait for.
    if (i.connectionState === 'new' || i.connectionState === 'connecting') return 'wait';
    if (i.attempts >= i.maxAttempts) return 'escalate';
    return 'nudge';
}
