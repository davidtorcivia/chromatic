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
global.ResizeObserver = class ResizeObserver {
    observe() {}
    unobserve() {}
    disconnect() {}
};

// Mock IntersectionObserver
global.IntersectionObserver = class IntersectionObserver {
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
global.AudioContext = class AudioContext {
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
    close() { return Promise.resolve(); }
} as unknown as typeof AudioContext;
