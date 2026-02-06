import '@testing-library/jest-dom/vitest';

// Mock matchMedia for tests
Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: (query: string) => ({
        matches: false,
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false
    })
});

// Mock ResizeObserver
globalThis.ResizeObserver = class ResizeObserver {
    observe() {}
    unobserve() {}
    disconnect() {}
} as unknown as typeof ResizeObserver;

// Mock IntersectionObserver
globalThis.IntersectionObserver = class IntersectionObserver {
    root = null;
    rootMargin = '';
    thresholds = [];
    observe() {}
    unobserve() {}
    disconnect() {}
    takeRecords() { return []; }
} as unknown as typeof IntersectionObserver;

// Mock navigator.mediaDevices
Object.defineProperty(navigator, 'mediaDevices', {
    value: {
        getUserMedia: () => Promise.resolve(null),
        enumerateDevices: () => Promise.resolve([])
    }
});

// Mock AudioContext
globalThis.AudioContext = class AudioContext {
    state = 'running';
    createGain() {
        return {
            connect: () => {},
            gain: { value: 1 }
        };
    }
    createAnalyser() {
        return {
            connect: () => {},
            fftSize: 256,
            getByteFrequencyData: () => {}
        };
    }
    createMediaStreamSource() {
        return { connect: () => {} };
    }
    resume() { return Promise.resolve(); }
    createBuffer() {
        return {} as AudioBuffer;
    }
    createBufferSource() {
        return {
            buffer: null,
            connect: () => {},
            start: () => {}
        };
    }
    close() { return Promise.resolve(); }
} as unknown as typeof AudioContext;
