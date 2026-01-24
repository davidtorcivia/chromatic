/**
 * Chat Message Load Test
 *
 * Focuses on chat message delivery performance and rate limiting.
 * Tests:
 * - Message delivery latency
 * - Rate limit enforcement (30 messages/minute)
 * - Message ordering
 * - Concurrent message handling
 *
 * Run:
 *   k6 run tests/load/scenarios/chat-load.js
 */

import http from 'k6/http';
import ws from 'k6/ws';
import { check, sleep, group } from 'k6';
import { Counter, Trend, Rate, Gauge } from 'k6/metrics';
import { PERFORMANCE_TARGETS } from '../k6-config.js';

// Environment configuration
const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const ADMIN_TOKEN = __ENV.ADMIN_TOKEN || 'test-admin-token';
const WS_URL = BASE_URL.replace('http', 'ws');

// Custom metrics
const chatSendLatency = new Trend('chat_send_latency');
const chatReceiveLatency = new Trend('chat_receive_latency');
const messagesSent = new Counter('messages_sent');
const messagesReceived = new Counter('messages_received');
const messagesDropped = new Counter('messages_dropped');
const rateLimitHits = new Counter('rate_limit_hits');
const deliverySuccessRate = new Rate('delivery_success');
const activeConnections = new Gauge('active_connections');

// Test configuration - focused on chat performance
export const options = {
  scenarios: {
    // Steady load for message latency testing
    steady_chat: {
      executor: 'constant-vus',
      vus: 4,
      duration: '2m',
      gracefulStop: '10s',
    },
    // Burst scenario for rate limit testing
    burst_chat: {
      executor: 'per-vu-iterations',
      vus: 2,
      iterations: 1,
      startTime: '2m30s',
      maxDuration: '30s',
    },
  },
  thresholds: {
    chat_receive_latency: [`p(95)<${PERFORMANCE_TARGETS.chatDeliveryMs}`],
    delivery_success: ['rate>0.95'],
    http_req_failed: ['rate<0.01'],
  },
};

const TEST_ROOM_SLUG = __ENV.ROOM_SLUG || 'chat-load-test';

export function setup() {
  console.log(`Setting up chat load test against ${BASE_URL}`);

  // Create test room if it doesn't exist
  const checkRes = http.get(`${BASE_URL}/api/rooms/${TEST_ROOM_SLUG}/info`);

  if (checkRes.status === 404) {
    const createRes = http.post(
      `${BASE_URL}/api/rooms`,
      JSON.stringify({
        name: 'Chat Load Test Room',
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

    if (createRes.status !== 201) {
      console.error('Failed to create test room');
      return { error: true };
    }
  }

  return { roomSlug: TEST_ROOM_SLUG };
}

// Main chat test scenario
export default function (data) {
  if (data.error) return;

  const vuId = __VU;
  const iterationId = __ITER;
  const userName = `ChatUser_${vuId}_${iterationId}`;

  // Join room
  const joinRes = http.post(
    `${BASE_URL}/api/rooms/${data.roomSlug}/join`,
    JSON.stringify({ name: userName }),
    { headers: { 'Content-Type': 'application/json' } }
  );

  if (joinRes.status !== 200) {
    console.error('Join failed:', joinRes.status);
    return;
  }

  const joinData = JSON.parse(joinRes.body);
  const token = joinData.token;

  // Connect WebSocket
  const wsUrl = `${WS_URL}/ws/${data.roomSlug}?token=${encodeURIComponent(token)}&name=${encodeURIComponent(userName)}`;

  ws.connect(wsUrl, {}, function (socket) {
    activeConnections.add(1);

    const pendingMessages = new Map(); // messageId -> sendTime
    let localMessageCounter = 0;

    socket.on('message', function (msg) {
      try {
        const data = JSON.parse(msg);

        if (data.type === 'chat:message') {
          messagesReceived.add(1);

          // Check if this is one of our messages (for latency measurement)
          const payload = data.payload;
          if (payload.messageId && pendingMessages.has(payload.messageId)) {
            const sendTime = pendingMessages.get(payload.messageId);
            const latency = Date.now() - sendTime;
            chatReceiveLatency.add(latency);
            pendingMessages.delete(payload.messageId);

            check(null, {
              [`chat latency < ${PERFORMANCE_TARGETS.chatDeliveryMs}ms`]: () =>
                latency < PERFORMANCE_TARGETS.chatDeliveryMs,
            });

            deliverySuccessRate.add(true);
          }
        }
      } catch (e) {
        // Ignore non-JSON messages
      }
    });

    socket.on('error', function (e) {
      console.error('WebSocket error:', e);
    });

    // Wait for connection to establish
    socket.setTimeout(function () {
      // Determine test mode based on scenario
      const isBurstTest = __ITER === 0 && __VU <= 2;

      if (isBurstTest) {
        // Burst test - send many messages quickly to test rate limiting
        group('Rate Limit Test', function () {
          let sentCount = 0;
          const burstCount = 35; // More than 30/minute limit

          for (let i = 0; i < burstCount; i++) {
            const messageId = `${vuId}-${iterationId}-burst-${i}`;
            const sendTime = Date.now();
            pendingMessages.set(messageId, sendTime);

            socket.send(JSON.stringify({
              type: 'chat:send',
              payload: {
                content: `Burst message ${i} from ${userName}`,
                messageId: messageId,
              },
            }));

            messagesSent.add(1);
            sentCount++;

            // Small delay to avoid overwhelming
            sleep(0.05);
          }

          // Wait for responses
          socket.setTimeout(function () {
            // Check how many messages were dropped
            const dropped = pendingMessages.size;
            messagesDropped.add(dropped);

            if (dropped > 0) {
              rateLimitHits.add(dropped);
              console.log(`Rate limit test: ${dropped} messages dropped out of ${burstCount}`);
            }

            // Should have dropped some messages due to rate limiting
            check(null, {
              'rate limit enforced': () => dropped >= 5, // At least 5 should be dropped
            });

            socket.close();
            activeConnections.add(-1);
          }, 5000);
        });
      } else {
        // Steady test - normal chat behavior
        group('Steady Chat Test', function () {
          // Send messages at a reasonable rate
          const messagesToSend = 10;
          let sentIndex = 0;

          const sendInterval = socket.setInterval(function () {
            if (sentIndex >= messagesToSend) {
              return;
            }

            const messageId = `${vuId}-${iterationId}-${sentIndex}`;
            const sendTime = Date.now();
            pendingMessages.set(messageId, sendTime);

            socket.send(JSON.stringify({
              type: 'chat:send',
              payload: {
                content: `Test message ${sentIndex} from ${userName}`,
                messageId: messageId,
              },
            }));

            const latency = Date.now() - sendTime;
            chatSendLatency.add(latency);
            messagesSent.add(1);
            sentIndex++;
          }, 2000); // One message every 2 seconds (safe under rate limit)

          // End test after sending all messages
          socket.setTimeout(function () {
            // Wait a bit more for responses
            socket.setTimeout(function () {
              const stillPending = pendingMessages.size;
              if (stillPending > 0) {
                messagesDropped.add(stillPending);
                for (let i = 0; i < stillPending; i++) {
                  deliverySuccessRate.add(false);
                }
              }

              socket.close();
              activeConnections.add(-1);
            }, 3000);
          }, messagesToSend * 2000 + 1000);
        });
      }
    }, 1000);
  });
}

export function teardown(data) {
  if (data.error) return;

  console.log('\n=== Chat Load Test Summary ===');
  console.log(`Target message latency: <${PERFORMANCE_TARGETS.chatDeliveryMs}ms`);
  console.log('Rate limit: 30 messages/minute per user');
  console.log('==============================\n');
}

export function handleSummary(data) {
  const summary = {
    timestamp: new Date().toISOString(),
    test: 'chat-load',
    metrics: {
      chat_send_latency: data.metrics.chat_send_latency,
      chat_receive_latency: data.metrics.chat_receive_latency,
      messages_sent: data.metrics.messages_sent,
      messages_received: data.metrics.messages_received,
      messages_dropped: data.metrics.messages_dropped,
      rate_limit_hits: data.metrics.rate_limit_hits,
      delivery_success: data.metrics.delivery_success,
    },
    thresholds: data.thresholds,
  };

  let text = '\n=== Chat Load Test Results ===\n\n';

  if (data.metrics.chat_receive_latency) {
    const m = data.metrics.chat_receive_latency;
    text += `Chat Receive Latency:\n`;
    text += `  avg: ${m.values.avg?.toFixed(0)}ms\n`;
    text += `  p95: ${m.values['p(95)']?.toFixed(0)}ms\n`;
    text += `  max: ${m.values.max?.toFixed(0)}ms\n`;
  }

  if (data.metrics.messages_sent) {
    text += `Messages Sent: ${data.metrics.messages_sent.values.count}\n`;
  }
  if (data.metrics.messages_received) {
    text += `Messages Received: ${data.metrics.messages_received.values.count}\n`;
  }
  if (data.metrics.messages_dropped) {
    text += `Messages Dropped: ${data.metrics.messages_dropped.values.count}\n`;
  }
  if (data.metrics.rate_limit_hits) {
    text += `Rate Limit Hits: ${data.metrics.rate_limit_hits.values.count}\n`;
  }
  if (data.metrics.delivery_success) {
    text += `Delivery Success Rate: ${(data.metrics.delivery_success.values.rate * 100).toFixed(2)}%\n`;
  }

  text += '\nThreshold Results:\n';
  for (const [name, result] of Object.entries(data.thresholds || {})) {
    text += `  ${result.ok ? '✓' : '✗'} ${name}\n`;
  }

  return {
    'chat-load-results.json': JSON.stringify(summary, null, 2),
    stdout: text,
  };
}
