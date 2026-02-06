/**
 * Concurrent Viewers Load Test
 *
 * Simulates multiple viewers joining a room simultaneously.
 * Tests:
 * - Room join API performance
 * - WebSocket connection establishment
 * - Concurrent user handling
 *
 * Run:
 *   k6 run tests/load/scenarios/concurrent-viewers.js
 *
 * With environment variables:
 *   k6 run -e BASE_URL=http://localhost:3000 -e ADMIN_TOKEN=secret tests/load/scenarios/concurrent-viewers.js
 */

import http from 'k6/http';
import ws from 'k6/ws';
import { check, sleep, group } from 'k6';
import { Counter, Trend, Rate } from 'k6/metrics';
import { LOAD_TEST, PERFORMANCE_TARGETS } from '../k6-config.js';

// Environment configuration
const BASE_URL = __ENV.BASE_URL || 'http://localhost:3000';
const ADMIN_TOKEN = __ENV.ADMIN_TOKEN || 'test-admin-token';
const WS_URL = BASE_URL.replace('http', 'ws');

// Custom metrics
const joinLatency = new Trend('room_join_latency');
const wsConnectLatency = new Trend('ws_connect_latency');
const wsMessageLatency = new Trend('ws_message_latency');
const roomJoinErrors = new Counter('room_join_errors');
const wsErrors = new Counter('ws_errors');
const messageDeliveryRate = new Rate('message_delivery_success');

// Test configuration
export const options = LOAD_TEST;

// Test room that should be created before running tests
const TEST_ROOM_SLUG = __ENV.ROOM_SLUG || 'load-test-room';

// Setup - create test room
export function setup() {
  console.log(`Setting up load test against ${BASE_URL}`);

  // Check if room exists, create if not
  const checkRes = http.get(`${BASE_URL}/api/rooms/${TEST_ROOM_SLUG}/info`);

  if (checkRes.status === 404) {
    console.log('Creating test room...');
    const createRes = http.post(
      `${BASE_URL}/api/rooms`,
      JSON.stringify({
        name: 'Load Test Room',
        slug: TEST_ROOM_SLUG,
        watermarkMode: 'none',
      }),
      {
        headers: {
          'Content-Type': 'application/json',
          'Authorization': `Bearer ${ADMIN_TOKEN}`,
        },
      }
    );

    check(createRes, {
      'room created': (r) => r.status === 201,
    });

    if (createRes.status !== 201) {
      console.error('Failed to create test room:', createRes.body);
      return { error: true };
    }
  } else {
    console.log('Test room already exists');
  }

  return { roomSlug: TEST_ROOM_SLUG };
}

// Main test scenario
export default function (data) {
  if (data.error) {
    console.error('Setup failed, skipping test');
    return;
  }

  const vuId = __VU;
  const iterationId = __ITER;
  const userName = `LoadUser_${vuId}_${iterationId}`;

  group('Join Room', function () {
    // Step 1: Join room via API
    const joinStart = Date.now();
    const joinRes = http.post(
      `${BASE_URL}/api/rooms/${data.roomSlug}/join`,
      JSON.stringify({ name: userName }),
      {
        headers: { 'Content-Type': 'application/json' },
        tags: { name: 'RoomJoin' },
      }
    );

    const joinDuration = Date.now() - joinStart;
    joinLatency.add(joinDuration);

    const joinOk = check(joinRes, {
      'join status is 200': (r) => r.status === 200,
      'join returns token': (r) => {
        try {
          const body = JSON.parse(r.body);
          return !!body.token;
        } catch {
          return false;
        }
      },
      [`join latency < ${PERFORMANCE_TARGETS.apiResponseP95Ms}ms`]: () =>
        joinDuration < PERFORMANCE_TARGETS.apiResponseP95Ms,
    });

    if (!joinOk) {
      roomJoinErrors.add(1);
      console.error(`Join failed: ${joinRes.status} - ${joinRes.body}`);
      return;
    }

    const joinData = JSON.parse(joinRes.body);
    const token = joinData.token;
    const participantId = joinData.participantId;

    // Step 2: Connect to WebSocket
    group('WebSocket Connection', function () {
      const wsUrl = `${WS_URL}/ws/${data.roomSlug}?token=${encodeURIComponent(token)}&name=${encodeURIComponent(userName)}`;

      const wsStart = Date.now();

      const wsRes = ws.connect(wsUrl, {}, function (socket) {
        const connectDuration = Date.now() - wsStart;
        wsConnectLatency.add(connectDuration);

        let roomStateReceived = false;
        let messagesSent = 0;
        let messagesReceived = 0;

        socket.on('open', function () {
          check(null, {
            [`WS connect < ${PERFORMANCE_TARGETS.webrtcSetupMs}ms`]: () =>
              connectDuration < PERFORMANCE_TARGETS.webrtcSetupMs,
          });
        });

        socket.on('message', function (msg) {
          try {
            const data = JSON.parse(msg);

            if (data.type === 'room:state') {
              roomStateReceived = true;
              check(null, {
                'received room state': () => true,
              });
            }

            if (data.type === 'chat:message') {
              messagesReceived++;
              const latency = Date.now() - parseInt(data.payload?.timestamp || '0');
              if (latency > 0 && latency < 10000) {
                wsMessageLatency.add(latency);
              }
            }
          } catch (e) {
            // Ignore parse errors for non-JSON messages
          }
        });

        socket.on('error', function (e) {
          wsErrors.add(1);
          console.error('WebSocket error:', e);
        });

        // Wait for room state
        socket.setTimeout(function () {
          check(null, {
            'room state received within 2s': () => roomStateReceived,
          });
        }, 2000);

        // Simulate user activity - send chat messages
        socket.setInterval(function () {
          const timestamp = Date.now();
          socket.send(JSON.stringify({
            type: 'chat:send',
            payload: {
              content: `Load test message from ${userName}`,
              timestamp: timestamp.toString(),
            },
          }));
          messagesSent++;
        }, 3000); // Send message every 3 seconds

        // Simulate cursor movement
        socket.setInterval(function () {
          socket.send(JSON.stringify({
            type: 'cursor',
            payload: {
              x: Math.random(),
              y: Math.random(),
              active: true,
            },
          }));
        }, 100); // Update cursor every 100ms (10Hz, client-side throttled)

        // Keep connection open for test duration
        socket.setTimeout(function () {
          const deliveryRate = messagesSent > 0 ? messagesReceived / messagesSent : 0;
          messageDeliveryRate.add(deliveryRate > 0.5);

          socket.close();
        }, 30000); // 30 seconds per session
      });

      check(wsRes, {
        'WebSocket connected': (r) => r && r.status === 101,
      });
    });
  });

  // Small delay between iterations
  sleep(1);
}

// Teardown
export function teardown(data) {
  if (data.error) return;

  console.log('\n=== Load Test Summary ===');
  console.log(`Room: ${data.roomSlug}`);
  console.log('Performance targets:');
  console.log(`  - Max viewers: ${PERFORMANCE_TARGETS.maxViewers}`);
  console.log(`  - Chat delivery: <${PERFORMANCE_TARGETS.chatDeliveryMs}ms`);
  console.log(`  - Cursor sync: <${PERFORMANCE_TARGETS.cursorSyncMs}ms`);
  console.log('========================\n');
}

// Handle summary
export function handleSummary(data) {
  const summary = {
    timestamp: new Date().toISOString(),
    metrics: {
      room_join_latency: data.metrics.room_join_latency,
      ws_connect_latency: data.metrics.ws_connect_latency,
      ws_message_latency: data.metrics.ws_message_latency,
      http_req_duration: data.metrics.http_req_duration,
      http_req_failed: data.metrics.http_req_failed,
    },
    thresholds: data.thresholds,
  };

  return {
    'load-test-results.json': JSON.stringify(summary, null, 2),
    stdout: textSummary(data),
  };
}

function textSummary(data) {
  let summary = '\n=== Chromatic Load Test Results ===\n\n';

  summary += 'Key Metrics:\n';
  if (data.metrics.room_join_latency) {
    const m = data.metrics.room_join_latency;
    summary += `  Room Join Latency: avg=${m.values.avg?.toFixed(0)}ms p95=${m.values['p(95)']?.toFixed(0)}ms\n`;
  }
  if (data.metrics.ws_connect_latency) {
    const m = data.metrics.ws_connect_latency;
    summary += `  WS Connect Latency: avg=${m.values.avg?.toFixed(0)}ms p95=${m.values['p(95)']?.toFixed(0)}ms\n`;
  }
  if (data.metrics.ws_message_latency) {
    const m = data.metrics.ws_message_latency;
    summary += `  Message Latency: avg=${m.values.avg?.toFixed(0)}ms p95=${m.values['p(95)']?.toFixed(0)}ms\n`;
  }
  if (data.metrics.http_req_failed) {
    summary += `  Request Failure Rate: ${(data.metrics.http_req_failed.values.rate * 100).toFixed(2)}%\n`;
  }

  summary += '\nThreshold Results:\n';
  for (const [name, result] of Object.entries(data.thresholds || {})) {
    summary += `  ${result.ok ? '✓' : '✗'} ${name}\n`;
  }

  return summary;
}
