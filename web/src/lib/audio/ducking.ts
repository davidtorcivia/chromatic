// Audio ducking manager - reduces stream volume when voice chat is active

import { getAudioContext } from './context';

export interface DuckingConfig {
    duckLevel: number;      // 0.2 = 20% volume when ducked
    attackTime: number;     // 50ms ramp down
    releaseTime: number;    // 200ms ramp up
    holdTime: number;       // 800ms before release
    vadThreshold: number;   // -50dB activation threshold
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
    private config: DuckingConfig;
    private isAdmin: boolean;
    private voiceAnalysers: Map<string, AnalyserNode> = new Map();
    private voiceGainNodes: Map<string, GainNode> = new Map();
    private voiceSources: Map<string, MediaStreamAudioSourceNode> = new Map();
    private isDucked: boolean = false;
    private releaseTimer: ReturnType<typeof setTimeout> | null = null;
    private monitorFrame: number | null = null;
    private volumeAnimationFrame: number | null = null;
    private baseStreamVolume: number = 1.0;
    private voiceVolume: number = 1.0;
    private vadBuffer: Uint8Array<ArrayBuffer> | null = null;

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

        // If the participant already has a track registered (e.g. reconnect /
        // renegotiation), tear down the old nodes first so we don't leak audio
        // graph refs that keep the previous MediaStream alive.
        if (this.voiceGainNodes.has(participantId) || this.voiceAnalysers.has(participantId)) {
            this.removeVoiceTrack(participantId);
        }

        const ctx = await getAudioContext();

        const stream = new MediaStreamCtor([track]);
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

        // Start monitoring if not already
        if (!this.monitorFrame) {
            this.startMonitoring();
        }
    }

    removeVoiceTrack(participantId: string): void {
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

            // Calculate average volume in dB
            let sum = 0;
            for (let i = 0; i < this.vadBuffer.length; i++) sum += this.vadBuffer[i];
            const avg = sum / this.vadBuffer.length;
            const db = 20 * Math.log10(avg / 255);

            if (db > this.config.vadThreshold) {
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
