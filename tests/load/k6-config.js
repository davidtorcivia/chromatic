/**
 * k6 Load Testing Configuration for Chromatic
 *
 * Install k6: https://k6.io/docs/getting-started/installation/
 *
 * Run tests:
 *   k6 run tests/load/scenarios/concurrent-viewers.js
 *   k6 run tests/load/scenarios/chat-load.js
 *
 * Run with HTML report:
 *   K6_WEB_DASHBOARD=true k6 run tests/load/scenarios/concurrent-viewers.js
 */

// Performance targets from design doc
export const PERFORMANCE_TARGETS = {
  // Max concurrent viewers per room
  maxViewers: 8,

  // Latency targets
  chatDeliveryMs: 100,     // Chat messages < 100ms
  cursorSyncMs: 100,       // Cursor sync < 100ms
  webrtcSetupMs: 2000,     // WebRTC setup < 2s

  // Resource limits
  maxMemoryMB: 512,        // Memory < 512MB
  maxCpuPercent: 25,       // CPU < 25% average

  // API response times
  apiResponseP95Ms: 200,   // 95th percentile < 200ms
  apiResponseP99Ms: 500,   // 99th percentile < 500ms
};

// Common test options
export const DEFAULT_OPTIONS = {
  // Thresholds for pass/fail
  thresholds: {
    http_req_duration: ['p(95)<200', 'p(99)<500'],
    http_req_failed: ['rate<0.01'],  // <1% errors
    ws_connecting: ['p(95)<1000'],
    ws_session_duration: ['p(95)>30000'],
  },

  // Custom metrics summary
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
};

// Smoke test - quick validation
export const SMOKE_TEST = {
  vus: 1,
  duration: '30s',
  ...DEFAULT_OPTIONS,
};

// Load test - normal load
export const LOAD_TEST = {
  stages: [
    { duration: '1m', target: 4 },   // Ramp up to 4 users
    { duration: '3m', target: 8 },   // Stay at 8 users
    { duration: '1m', target: 0 },   // Ramp down
  ],
  ...DEFAULT_OPTIONS,
};

// Stress test - beyond normal
export const STRESS_TEST = {
  stages: [
    { duration: '1m', target: 4 },
    { duration: '2m', target: 8 },
    { duration: '2m', target: 16 },  // 2x target capacity
    { duration: '2m', target: 8 },
    { duration: '1m', target: 0 },
  ],
  thresholds: {
    ...DEFAULT_OPTIONS.thresholds,
    http_req_failed: ['rate<0.05'],  // Allow up to 5% errors under stress
  },
};

// Spike test - sudden traffic
export const SPIKE_TEST = {
  stages: [
    { duration: '30s', target: 2 },
    { duration: '10s', target: 16 }, // Sudden spike
    { duration: '1m', target: 16 },
    { duration: '10s', target: 2 },  // Sudden drop
    { duration: '30s', target: 0 },
  ],
  thresholds: {
    ...DEFAULT_OPTIONS.thresholds,
    http_req_failed: ['rate<0.10'],  // Allow up to 10% errors during spike
  },
};

// Soak test - extended duration
export const SOAK_TEST = {
  stages: [
    { duration: '2m', target: 8 },
    { duration: '30m', target: 8 },  // Hold for 30 minutes
    { duration: '2m', target: 0 },
  ],
  ...DEFAULT_OPTIONS,
};

export default {
  PERFORMANCE_TARGETS,
  DEFAULT_OPTIONS,
  SMOKE_TEST,
  LOAD_TEST,
  STRESS_TEST,
  SPIKE_TEST,
  SOAK_TEST,
};
