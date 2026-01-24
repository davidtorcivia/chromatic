import { describe, it, expect, vi, beforeEach } from 'vitest';
import { getVideoContentRect, clientToVideoCoords, videoToElementCoords } from './coordinates';

// Mock video element factory
function createMockVideo(
    elementWidth: number,
    elementHeight: number,
    videoWidth: number,
    videoHeight: number,
    left = 0,
    top = 0
): HTMLVideoElement {
    return {
        getBoundingClientRect: () => ({
            left,
            top,
            width: elementWidth,
            height: elementHeight,
            right: left + elementWidth,
            bottom: top + elementHeight,
            x: left,
            y: top,
            toJSON: () => {}
        }),
        videoWidth,
        videoHeight
    } as unknown as HTMLVideoElement;
}

describe('getVideoContentRect', () => {
    it('returns full element when video fills element exactly', () => {
        const video = createMockVideo(1920, 1080, 1920, 1080);
        const rect = getVideoContentRect(video);

        expect(rect.x).toBe(0);
        expect(rect.y).toBe(0);
        expect(rect.width).toBe(1920);
        expect(rect.height).toBe(1080);
    });

    it('calculates letterbox for wide video in narrow container', () => {
        // 16:9 video in 4:3 container (800x600)
        const video = createMockVideo(800, 600, 1920, 1080);
        const rect = getVideoContentRect(video);

        // 16:9 = 1.777, 4:3 = 1.333
        // Video is wider, so letterbox top/bottom
        // Width = 800 (full width)
        // Height = 800 / (16/9) = 450
        expect(rect.width).toBe(800);
        expect(rect.height).toBeCloseTo(450);
        expect(rect.x).toBe(0);
        expect(rect.y).toBeCloseTo(75); // (600 - 450) / 2
    });

    it('calculates pillarbox for tall video in wide container', () => {
        // 4:3 video in 16:9 container (1920x1080)
        const video = createMockVideo(1920, 1080, 800, 600);
        const rect = getVideoContentRect(video);

        // 4:3 = 1.333, 16:9 = 1.777
        // Video is taller, so pillarbox left/right
        // Height = 1080 (full height)
        // Width = 1080 * (4/3) = 1440
        expect(rect.height).toBe(1080);
        expect(rect.width).toBeCloseTo(1440);
        expect(rect.y).toBe(0);
        expect(rect.x).toBeCloseTo(240); // (1920 - 1440) / 2
    });

    it('returns fallback when video dimensions not available', () => {
        const video = createMockVideo(1920, 1080, 0, 0);
        const rect = getVideoContentRect(video);

        expect(rect.x).toBe(0);
        expect(rect.y).toBe(0);
        expect(rect.width).toBe(1920);
        expect(rect.height).toBe(1080);
    });
});

describe('clientToVideoCoords', () => {
    it('converts center click to (0.5, 0.5)', () => {
        const video = createMockVideo(1920, 1080, 1920, 1080, 0, 0);
        const result = clientToVideoCoords(960, 540, video);

        expect(result.x).toBeCloseTo(0.5);
        expect(result.y).toBeCloseTo(0.5);
        expect(result.valid).toBe(true);
    });

    it('converts top-left click to (0, 0)', () => {
        const video = createMockVideo(1920, 1080, 1920, 1080, 0, 0);
        const result = clientToVideoCoords(0, 0, video);

        expect(result.x).toBeCloseTo(0);
        expect(result.y).toBeCloseTo(0);
        expect(result.valid).toBe(true);
    });

    it('converts bottom-right click to (1, 1)', () => {
        const video = createMockVideo(1920, 1080, 1920, 1080, 0, 0);
        const result = clientToVideoCoords(1920, 1080, video);

        expect(result.x).toBeCloseTo(1);
        expect(result.y).toBeCloseTo(1);
        expect(result.valid).toBe(true);
    });

    it('handles element offset', () => {
        const video = createMockVideo(1920, 1080, 1920, 1080, 100, 50);
        const result = clientToVideoCoords(1060, 590, video); // Center = 100+960, 50+540

        expect(result.x).toBeCloseTo(0.5);
        expect(result.y).toBeCloseTo(0.5);
        expect(result.valid).toBe(true);
    });

    it('marks letterbox clicks as invalid', () => {
        // 16:9 video in 800x600 container (has letterbox at top/bottom)
        const video = createMockVideo(800, 600, 1920, 1080, 0, 0);

        // Click in top letterbox area (y < 75)
        const result = clientToVideoCoords(400, 10, video);
        expect(result.valid).toBe(false);
    });

    it('clamps coordinates to 0-1 range', () => {
        const video = createMockVideo(1920, 1080, 1920, 1080, 0, 0);
        const result = clientToVideoCoords(-100, 2000, video);

        expect(result.x).toBe(0);
        expect(result.y).toBe(1);
        expect(result.valid).toBe(false);
    });
});

describe('videoToElementCoords', () => {
    it('converts (0.5, 0.5) to center of element', () => {
        const video = createMockVideo(1920, 1080, 1920, 1080);
        const result = videoToElementCoords(0.5, 0.5, video);

        expect(result.x).toBeCloseTo(960);
        expect(result.y).toBeCloseTo(540);
    });

    it('converts (0, 0) to top-left of video content', () => {
        const video = createMockVideo(1920, 1080, 1920, 1080);
        const result = videoToElementCoords(0, 0, video);

        expect(result.x).toBe(0);
        expect(result.y).toBe(0);
    });

    it('accounts for letterboxing offset', () => {
        // 16:9 video in 800x600 container
        const video = createMockVideo(800, 600, 1920, 1080);
        const result = videoToElementCoords(0, 0, video);

        // Video content starts at (0, 75) due to letterboxing
        expect(result.x).toBe(0);
        expect(result.y).toBeCloseTo(75);
    });

    it('accounts for pillarboxing offset', () => {
        // 4:3 video in 1920x1080 container
        const video = createMockVideo(1920, 1080, 800, 600);
        const result = videoToElementCoords(0, 0, video);

        // Video content starts at (240, 0) due to pillarboxing
        expect(result.x).toBeCloseTo(240);
        expect(result.y).toBe(0);
    });
});
