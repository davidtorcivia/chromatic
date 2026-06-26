// Audio ducking manager - reduces stream volume when voice chat is active

import { getAudioContext } from './context';

export interface DuckingConfig {
    duckLevel: number;      // 0.2 = 20% volume when ducked
    attackTime: number;     // 50ms ramp down
    releaseTime: number;    // 200ms ramp up
    holdTime: number;       // 800ms before release
    // Voice-activity threshold expressed as a mean byte level over the analyser's
    // getByteFrequencyData output (0-255, where 0 = minDecibels and 255 =
    // maxDecibels of the AnalyserNode). Speech concentrates energy in a handful
    // of bins, so the average across all bins stays low until someone actually
    // speaks; ~10 is a calibrated "speaking" floor that ignores room noise/hum
    // but catches normal speech. (The previous -50dB value fed the byte average
    // through a second 20·log10 conversion, which is meaningless after the
    // analyser's own dB→byte mapping and triggered on near-silence.)
    vadByteThreshold: number;
}

const DEFAULT_CONFIG: DuckingConfig = {
    duckLevel: 0.2,
    attackTime: 50,
    releaseTime: 200,
    holdTime: 800,
    vadByteThreshold: 10
};

export class AudioDuckingManager {
    private streamElement: HTMLMediaElement;
    private config: DuckingConfig;
    private isAdmin: boolean;
    private voiceAnalysers: Map<string, AnalyserNode> = new Map();
    private voiceGainNodes: Map<string, GainNode> = new Map();
    private voiceSources: Map<string, MediaStreamAudioSourceNode> = new Map();
    // Muted media-element sinks that force Chromium to decode each remote
    // voice track (see the comment in addVoiceTrack).
    private voiceSinks: Map<string, HTMLAudioElement> = new Map();
    private isDucked: boolean = false;
    private releaseTimer: ReturnType<typeof setTimeout> | null = null;
    private monitorFrame: number | null = null;
    private volumeAnimationFrame: number | null = null;
    private baseStreamVolume: number = 1.0;
    private voiceVolume: number = 1.0;
    private vadBuffer: Uint8Array<ArrayBuffer> | null = null;
    // True once destroy() has torn the manager down. addVoiceTrack bails at
    // entry and re-checks after its await so a late-resolving setup can't
    // resurrect a zombie manager (orphaned sinks/sources/RAF loop).
    private destroyed: boolean = false;
    // Participant IDs whose addVoiceTrack setup is past the await. removeVoiceTrack
    // / destroy record cancellation here so the post-await code knows to abort
    // instead of building a graph for a departed participant.
    private addCancelled: Set<string> = new Set();

    constructor(streamElement: HTMLMediaElement, isAdmin: boolean, config?: Partial<DuckingConfig>) {
        this.streamElement = streamElement;
        this.isAdmin = isAdmin;
        this.config = { ...DEFAULT_CONFIG, ...config };
        this.baseStreamVolume = streamElement.volume;
    }

    // Volume control methods
    setStreamVolume(volume: number): void {
        this.baseStreamVolume = Math.max(0, Math.min(1, volume));
        // Apply immediately if not ducked, otherwise apply with duck level
        if (!this.isDucked) {
            this.streamElement.volume = this.baseStreamVolume;
        } else {
            this.streamElement.volume = this.baseStreamVolume * this.config.duckLevel;
        }
    }

    getStreamVolume(): number {
        return this.baseStreamVolume;
    }

    setVoiceVolume(volume: number): void {
        this.voiceVolume = Math.max(0, Math.min(1, volume));
        // Apply to all voice gain nodes
        for (const gainNode of this.voiceGainNodes.values()) {
            gainNode.gain.value = this.voiceVolume;
        }
    }

    getVoiceVolume(): number {
        return this.voiceVolume;
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
        if (this.voiceGainNodes.has(participantId) || this.voiceAnalysers.has(participantId)) {
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

        // Chromium quirk (crbug.com/121673 lineage, still live in 2026,
        // confirmed empirically: analyser energy is ZERO without this): a
        // remote WebRTC audio track consumed only through WebAudio decodes
        // to silence — the track must also have a media-element sink. The
        // element stays muted; the WebAudio graph below (with ducking and
        // volume control) is the audible path.
        const sink = new Audio();
        sink.srcObject = stream;
        sink.muted = true;
        void sink.play().catch(() => {
            // Playback of a muted element may still be policy-blocked in
            // odd states; attaching srcObject alone starts the decoder.
        });
        this.voiceSinks.set(participantId, sink);

        const source = ctx.createMediaStreamSource(stream);
        const analyser = ctx.createAnalyser();
        analyser.fftSize = 256;

        // Create gain node for volume control
        const gainNode = ctx.createGain();
        gainNode.gain.value = this.voiceVolume;

        // Connect: source -> analyser -> gainNode -> destination
        source.connect(analyser);
        analyser.connect(gainNode);
        gainNode.connect(ctx.destination);

        this.voiceAnalysers.set(participantId, analyser);
        this.voiceGainNodes.set(participantId, gainNode);
        this.voiceSources.set(participantId, source);

        // Start monitoring if not already. Guard against a concurrent
        // addVoiceTrack that started its own loop during the await: by reading
        // this.monitorFrame here (after node creation), only the first caller
        // through actually arms the loop.
        if (!this.monitorFrame) {
            this.startMonitoring();
        }
    }

    removeVoiceTrack(participantId: string): void {
        // Signal any in-flight addVoiceTrack for this participant to abort once
        // its await resolves (it would otherwise build a graph for a track we
        // are now removing).
        this.addCancelled.add(participantId);

        const sink = this.voiceSinks.get(participantId);
        if (sink) {
            sink.srcObject = null;
            this.voiceSinks.delete(participantId);
        }

        // Disconnect the full chain (source -> analyser -> gain) so the
        // nodes and the underlying MediaStreamTrack can be released.
        const source = this.voiceSources.get(participantId);
        if (source) {
            source.disconnect();
            this.voiceSources.delete(participantId);
        }

        const analyser = this.voiceAnalysers.get(participantId);
        if (analyser) {
            analyser.disconnect();
        }

        const gainNode = this.voiceGainNodes.get(participantId);
        if (gainNode) {
            gainNode.disconnect();
            this.voiceGainNodes.delete(participantId);
        }

        this.voiceAnalysers.delete(participantId);

        if (this.voiceAnalysers.size === 0 && this.monitorFrame) {
            cancelAnimationFrame(this.monitorFrame);
            this.monitorFrame = null;
        }
    }

    private startMonitoring(): void {
        const checkVoice = () => {
            const hasVoice = this.detectVoiceActivity();

            if (hasVoice && !this.isDucked) {
                this.duck();
            } else if (!hasVoice && this.isDucked) {
                this.scheduleRelease();
            }

            this.monitorFrame = requestAnimationFrame(checkVoice);
        };

        this.monitorFrame = requestAnimationFrame(checkVoice);
    }

    private detectVoiceActivity(): boolean {
        for (const analyser of this.voiceAnalysers.values()) {
            // Reuse or resize the buffer to avoid per-frame allocation
            if (!this.vadBuffer || this.vadBuffer.length !== analyser.frequencyBinCount) {
                this.vadBuffer = new Uint8Array(analyser.frequencyBinCount) as Uint8Array<ArrayBuffer>;
            }
            analyser.getByteFrequencyData(this.vadBuffer);

            // Mean byte level across frequency bins. getByteFrequencyData already
            // maps the analyser's [minDecibels, maxDecibels] onto 0-255, so the
            // average is a direct energy proxy — compare it to vadByteThreshold
            // directly. (Re-deriving dB via 20·log10(avg/255) was meaningless
            // double conversion that fired on near-silence.)
            let sum = 0;
            for (let i = 0; i < this.vadBuffer.length; i++) sum += this.vadBuffer[i];
            const avg = sum / this.vadBuffer.length;

            if (avg > this.config.vadByteThreshold) {
                return true;
            }
        }
        return false;
    }

    private duck(): void {
        // Admin exempt from ducking
        if (this.isAdmin) return;

        // Cancel any pending release
        if (this.releaseTimer) {
            clearTimeout(this.releaseTimer);
            this.releaseTimer = null;
        }

        this.isDucked = true;
        this.animateVolume(this.config.duckLevel, this.config.attackTime);
    }

    private scheduleRelease(): void {
        if (this.releaseTimer) return;

        this.releaseTimer = setTimeout(() => {
            this.releaseTimer = null;
            this.isDucked = false;
            this.animateVolume(1.0, this.config.releaseTime);
        }, this.config.holdTime);
    }

    private animateVolume(targetMultiplier: number, duration: number): void {
        // Cancel any in-flight animation so rapid duck/release cycles don't
        // spawn parallel RAF loops fighting over the volume (audible jitter).
        if (this.volumeAnimationFrame !== null) {
            cancelAnimationFrame(this.volumeAnimationFrame);
            this.volumeAnimationFrame = null;
        }

        const targetVolume = this.baseStreamVolume * targetMultiplier;
        const start = this.streamElement.volume;
        const startTime = performance.now();

        const animate = (now: number) => {
            const elapsed = now - startTime;
            const progress = Math.min(1, elapsed / duration);
            const eased = this.easeOutQuad(progress);

            this.streamElement.volume = start + (targetVolume - start) * eased;

            if (progress < 1) {
                this.volumeAnimationFrame = requestAnimationFrame(animate);
            } else {
                this.volumeAnimationFrame = null;
            }
        };

        this.volumeAnimationFrame = requestAnimationFrame(animate);
    }

    private easeOutQuad(t: number): number {
        return t * (2 - t);
    }

    destroy(): void {
        // Mark the manager dead so any addVoiceTrack awaiting getAudioContext()
        // aborts on resolution instead of resurrecting an orphaned graph + RAF
        // loop on an object the caller believes is gone.
        this.destroyed = true;
        // Cancel every in-flight setup so the post-await guard fires for all.
        for (const pid of this.voiceSources.keys()) this.addCancelled.add(pid);
        for (const pid of this.voiceAnalysers.keys()) this.addCancelled.add(pid);

        for (const sink of this.voiceSinks.values()) {
            sink.srcObject = null;
        }
        this.voiceSinks.clear();
        if (this.monitorFrame) {
            cancelAnimationFrame(this.monitorFrame);
            this.monitorFrame = null;
        }
        if (this.volumeAnimationFrame !== null) {
            cancelAnimationFrame(this.volumeAnimationFrame);
            this.volumeAnimationFrame = null;
        }
        if (this.releaseTimer) {
            clearTimeout(this.releaseTimer);
            this.releaseTimer = null;
        }
        // Disconnect all audio graph nodes (source -> analyser -> gain)
        for (const source of this.voiceSources.values()) {
            source.disconnect();
        }
        this.voiceSources.clear();
        for (const analyser of this.voiceAnalysers.values()) {
            analyser.disconnect();
        }
        for (const gainNode of this.voiceGainNodes.values()) {
            gainNode.disconnect();
        }
        this.voiceGainNodes.clear();
        this.voiceAnalysers.clear();
        // Restore the un-ducked volume so a re-created manager (or the bare
        // video element) doesn't inherit a ducked volume.
        this.streamElement.volume = this.baseStreamVolume;
        this.isDucked = false;
    }
}
