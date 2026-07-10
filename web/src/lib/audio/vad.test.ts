import { describe, it, expect, vi, afterEach } from "vitest";
import { createVADMonitor } from "./vad";

function fakeAnalyser(level: number) {
    return {
        analyser: {
            frequencyBinCount: 4,
            getByteFrequencyData: (buf: Uint8Array) => buf.fill(level),
        } as unknown as AnalyserNode,
    };
}

describe("createVADMonitor", () => {
    afterEach(() => vi.useRealTimers());

    it("reports speakers above the threshold and stays silent when nothing changes", () => {
        const analysers = new Map([
            ["loud", fakeAnalyser(200)],
            ["quiet", fakeAnalyser(0)],
        ]);
        const onChange = vi.fn();
        const vad = createVADMonitor({ getAnalysers: () => analysers, onChange });

        vad.tick();
        expect(onChange).toHaveBeenCalledTimes(1);
        expect([...onChange.mock.calls[0][0]]).toEqual(["loud"]);

        // Same state again: no reactive churn.
        vad.tick();
        expect(onChange).toHaveBeenCalledTimes(1);
    });

    it("fires again when a speaker goes quiet", () => {
        let level = 200;
        const analysers = new Map([
            [
                "p1",
                {
                    analyser: {
                        frequencyBinCount: 4,
                        getByteFrequencyData: (buf: Uint8Array) => buf.fill(level),
                    } as unknown as AnalyserNode,
                },
            ],
        ]);
        const onChange = vi.fn();
        const vad = createVADMonitor({ getAnalysers: () => analysers, onChange });

        vad.tick();
        expect([...onChange.mock.calls[0][0]]).toEqual(["p1"]);

        level = 0;
        vad.tick();
        expect(onChange).toHaveBeenCalledTimes(2);
        expect(onChange.mock.calls[1][0].size).toBe(0);
    });

    it("start is idempotent and stop clears the timer and state", () => {
        vi.useFakeTimers();
        const analysers = new Map([["p1", fakeAnalyser(200)]]);
        const onChange = vi.fn();
        const vad = createVADMonitor({ getAnalysers: () => analysers, onChange, intervalMs: 10 });

        vad.start();
        vad.start();
        expect(vad.running).toBe(true);
        vi.advanceTimersByTime(25);
        expect(onChange).toHaveBeenCalledTimes(1); // set changed once, then stable

        vad.stop();
        expect(vad.running).toBe(false);
        vi.advanceTimersByTime(50);
        expect(onChange).toHaveBeenCalledTimes(1);
    });
});
