// Voice playback + program-volume manager.
//
// Responsibilities:
//   - Play each remote participant's voice track through a per-participant
//     WebAudio gain node, so the listener's voice-volume slider is independent
//     of the program (WHIP/OBS) stream.
//   - Own the explicit user volume of the program media element.
//
// It NEVER mutates the program element's volume in response to voice activity.
// The program stream is a pristine Opus relay carried end-to-end for critical
// review; only the user's explicit volume slider (setStreamVolume) or the
// browser's own autoplay mute may change its level. This manager replaces the
// former AudioDuckingManager, which automatically ducked the program under
// detected speech — that behavior fought the reviewer and is gone.
//
// Chromium quirk (crbug.com/121673 lineage, still live): a remote WebRTC audio
// track consumed only through WebAudio decodes to silence unless it also has a
// media-element sink. Each voice track therefore gets a muted <audio> sink that
// exists solely to drive Chromium's decoder; the audible path is the WebAudio
// graph below (source -> gain -> destination).

import { getAudioContext } from './context';

export class VoicePlaybackManager {
    private streamElement: HTMLMediaElement;
    private voiceGainNodes: Map<string, GainNode> = new Map();
    private voiceSources: Map<string, MediaStreamAudioSourceNode> = new Map();
    // Muted media-element sinks that force Chromium to decode each remote voice
    // track (see the class doc and addVoiceTrack).
    private voiceSinks: Map<string, HTMLAudioElement> = new Map();
    private baseStreamVolume: number = 1.0;
    private voiceVolume: number = 1.0;
    // True once destroy() has torn the manager down. addVoiceTrack bails at
    // entry and re-checks after its await so a late-resolving setup can't
    // resurrect a zombie manager (orphaned sinks/sources on a dead object).
    private destroyed: boolean = false;
    // Participant IDs whose addVoiceTrack setup is past the await. removeVoiceTrack
    // / destroy record cancellation here so the post-await code knows to abort
    // instead of building a graph for a departed participant.
    private addCancelled: Set<string> = new Set();
    // The MediaStreamTrack currently registered per participant, plus its
    // 'ended' cleanup handler. A remote voice track can end without a
    // participant:left (publisher PC tear-down, ICE failure) — without an
    // 'ended' handler the sink/source/gain would leak until explicit removal.
    private voiceTracks: Map<string, MediaStreamTrack> = new Map();
    private voiceEndHandlers: Map<string, () => void> = new Map();

    constructor(streamElement: HTMLMediaElement) {
        this.streamElement = streamElement;
        this.baseStreamVolume = streamElement.volume;
    }

    // Explicit user program volume (0..1). This is the ONLY path that sets the
    // program element's volume — never voice activity, never a ducking ramp.
    setStreamVolume(volume: number): void {
        this.baseStreamVolume = Math.max(0, Math.min(1, volume));
        this.streamElement.volume = this.baseStreamVolume;
    }

    setVoiceVolume(volume: number): void {
        this.voiceVolume = Math.max(0, Math.min(1, volume));
        // Apply to all voice gain nodes.
        for (const gainNode of this.voiceGainNodes.values()) {
            gainNode.gain.value = this.voiceVolume;
        }
    }

    async addVoiceTrack(participantId: string, track: MediaStreamTrack): Promise<void> {
        const MediaStreamCtor = globalThis.MediaStream;
        if (typeof MediaStreamCtor !== 'function') {
            // Non-browser test/runtime environments may not expose MediaStream.
            return;
        }
        // Never start work on a torn-down manager.
        if (this.destroyed) return;

        // If the participant already has a track registered (e.g. reconnect /
        // renegotiation), tear down the old nodes first so we don't leak audio
        // graph refs that keep the previous MediaStream alive.
        if (this.voiceGainNodes.has(participantId)) {
            this.removeVoiceTrack(participantId);
        }

        // Mark this setup as in-flight. removeVoiceTrack/destroy clear the flag
        // so a setup that resolves AFTER the participant left (or the manager
        // was destroyed) aborts instead of building an orphaned graph.
        this.addCancelled.delete(participantId);

        const ctx = await getAudioContext();

        // The await above can stay pending for a long time under the autoplay
        // policy (context resume needs a gesture). Re-check that we're still
        // wanted before creating any nodes.
        if (this.destroyed || this.addCancelled.has(participantId)) {
            this.addCancelled.delete(participantId);
            return;
        }

        const stream = new MediaStreamCtor([track]);

        // Chromium quirk: a remote WebRTC audio track consumed only through
        // WebAudio decodes to silence — the track must also have a media-element
        // sink. The element stays muted; the WebAudio graph below (with volume
        // control) is the audible path.
        const sink = new Audio();
        sink.srcObject = stream;
        sink.muted = true;
        void sink.play().catch(() => {
            // Playback of a muted element may still be policy-blocked in odd
            // states; attaching srcObject alone starts the decoder.
        });
        this.voiceSinks.set(participantId, sink);

        const source = ctx.createMediaStreamSource(stream);

        // Create gain node for volume control.
        const gainNode = ctx.createGain();
        gainNode.gain.value = this.voiceVolume;

        // Connect: source -> gainNode -> destination. No analyser — there is no
        // VAD/ducking loop anymore, so the voice graph is a straight gain chain.
        source.connect(gainNode);
        gainNode.connect(ctx.destination);

        this.voiceGainNodes.set(participantId, gainNode);
        this.voiceSources.set(participantId, source);

        // A remote track can end without a participant:left (publisher PC
        // tear-down, ICE failure). Tear the graph down when it does so the
        // sink/source/gain don't leak. The identity guard means a track that
        // was already replaced by a renegotiation can't evict its successor.
        // (A real MediaStreamTrack always exposes addEventListener; the guard
        // keeps minimal test/runtime mocks from throwing.)
        if (typeof track.addEventListener === 'function') {
            const onEnded = () => {
                if (!this.destroyed && this.voiceTracks.get(participantId) === track) {
                    this.removeVoiceTrack(participantId);
                }
            };
            track.addEventListener('ended', onEnded);
            this.voiceTracks.set(participantId, track);
            this.voiceEndHandlers.set(participantId, onEnded);
        }
    }

    removeVoiceTrack(participantId: string): void {
        // Signal any in-flight addVoiceTrack for this participant to abort once
        // its await resolves (it would otherwise build a graph for a track we
        // are now removing).
        this.addCancelled.add(participantId);

        // Drop the 'ended' listener for the registered track before tearing its
        // graph down, so the ending track can't re-enter removeVoiceTrack and a
        // later renegotiation's new track isn't evicted by a stale handler.
        const endHandler = this.voiceEndHandlers.get(participantId);
        const trackedTrack = this.voiceTracks.get(participantId);
        if (endHandler && trackedTrack) {
            trackedTrack.removeEventListener('ended', endHandler);
        }
        this.voiceEndHandlers.delete(participantId);
        this.voiceTracks.delete(participantId);

        const sink = this.voiceSinks.get(participantId);
        if (sink) {
            sink.srcObject = null;
            this.voiceSinks.delete(participantId);
        }

        // Disconnect the chain (source -> gain) so the nodes and the underlying
        // MediaStreamTrack can be released.
        const source = this.voiceSources.get(participantId);
        if (source) {
            source.disconnect();
            this.voiceSources.delete(participantId);
        }

        const gainNode = this.voiceGainNodes.get(participantId);
        if (gainNode) {
            gainNode.disconnect();
            this.voiceGainNodes.delete(participantId);
        }
    }

    destroy(): void {
        // Mark the manager dead so any addVoiceTrack awaiting getAudioContext()
        // aborts on resolution instead of resurrecting an orphaned graph on an
        // object the caller believes is gone.
        this.destroyed = true;
        // Cancel every in-flight setup so the post-await guard fires for all.
        for (const pid of this.voiceSources.keys()) this.addCancelled.add(pid);
        for (const pid of this.voiceGainNodes.keys()) this.addCancelled.add(pid);

        // Detach the per-participant 'ended' listeners before nulling sinks so
        // an ending track can't fire into a half-destroyed manager.
        for (const [pid, handler] of this.voiceEndHandlers) {
            const t = this.voiceTracks.get(pid);
            if (t) t.removeEventListener('ended', handler);
        }
        this.voiceEndHandlers.clear();
        this.voiceTracks.clear();

        for (const sink of this.voiceSinks.values()) {
            sink.srcObject = null;
        }
        this.voiceSinks.clear();
        // Disconnect all audio graph nodes (source -> gain).
        for (const source of this.voiceSources.values()) {
            source.disconnect();
        }
        this.voiceSources.clear();
        for (const gainNode of this.voiceGainNodes.values()) {
            gainNode.disconnect();
        }
        this.voiceGainNodes.clear();
        // The program element's volume reflects only the user's explicit choice;
        // restore the base (un-ducked) volume so a re-created manager (or the
        // bare video element) doesn't inherit a stale level.
        this.streamElement.volume = this.baseStreamVolume;
    }
}
