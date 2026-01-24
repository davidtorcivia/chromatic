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
    private isDucked: boolean = false;
    private releaseTimer: ReturnType<typeof setTimeout> | null = null;
    private monitorFrame: number | null = null;

    constructor(streamElement: HTMLMediaElement, isAdmin: boolean, config?: Partial<DuckingConfig>) {
        this.streamElement = streamElement;
        this.isAdmin = isAdmin;
        this.config = { ...DEFAULT_CONFIG, ...config };
    }

    async addVoiceTrack(participantId: string, track: MediaStreamTrack): Promise<void> {
        const ctx = await getAudioContext();

        const stream = new MediaStream([track]);
        const source = ctx.createMediaStreamSource(stream);
        const analyser = ctx.createAnalyser();
        analyser.fftSize = 256;

        source.connect(analyser);
        this.voiceAnalysers.set(participantId, analyser);

        // Start monitoring if not already
        if (!this.monitorFrame) {
            this.startMonitoring();
        }
    }

    removeVoiceTrack(participantId: string): void {
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
            const data = new Uint8Array(analyser.frequencyBinCount);
            analyser.getByteFrequencyData(data);

            // Calculate average volume in dB
            const avg = data.reduce((sum, val) => sum + val, 0) / data.length;
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

    private animateVolume(target: number, duration: number): void {
        const start = this.streamElement.volume;
        const startTime = performance.now();

        const animate = (now: number) => {
            const elapsed = now - startTime;
            const progress = Math.min(1, elapsed / duration);
            const eased = this.easeOutQuad(progress);

            this.streamElement.volume = start + (target - start) * eased;

            if (progress < 1) {
                requestAnimationFrame(animate);
            }
        };

        requestAnimationFrame(animate);
    }

    private easeOutQuad(t: number): number {
        return t * (2 - t);
    }

    destroy(): void {
        if (this.monitorFrame) {
            cancelAnimationFrame(this.monitorFrame);
        }
        if (this.releaseTimer) {
            clearTimeout(this.releaseTimer);
        }
        this.voiceAnalysers.clear();
    }
}
