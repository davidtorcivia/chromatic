# Chromatic Load Testing

Load tests for Chromatic using [k6](https://k6.io/).

## Prerequisites

Install k6:

```bash
# macOS
brew install k6

# Windows (Chocolatey)
choco install k6

# Linux (Debian/Ubuntu)
sudo gpg -k
sudo gpg --no-default-keyring --keyring /usr/share/keyrings/k6-archive-keyring.gpg --keyserver hkp://keyserver.ubuntu.com:80 --recv-keys C5AD17C747E3415A3642D57D77C6C491D6AC1D69
echo "deb [signed-by=/usr/share/keyrings/k6-archive-keyring.gpg] https://dl.k6.io/deb stable main" | sudo tee /etc/apt/sources.list.d/k6.list
sudo apt-get update
sudo apt-get install k6
```

## Performance Targets

From the design document:

| Metric | Target |
|--------|--------|
| Max concurrent viewers | 8 per room |
| Chat message delivery | < 100ms |
| Cursor sync latency | < 100ms |
| WebRTC setup time | < 2s |
| Memory usage | < 512MB |
| CPU usage | < 25% average |
| API response (p95) | < 200ms |
| API response (p99) | < 500ms |

## Running Tests

### Quick Smoke Test

```bash
k6 run --vus 1 --duration 30s tests/load/scenarios/concurrent-viewers.js
```

### Standard Load Test

```bash
k6 run tests/load/scenarios/concurrent-viewers.js
```

### Chat-Specific Load Test

```bash
k6 run tests/load/scenarios/chat-load.js
```

### With Custom Configuration

```bash
k6 run \
  -e BASE_URL=http://localhost:8080 \
  -e ADMIN_TOKEN=your-admin-token \
  -e ROOM_SLUG=my-test-room \
  tests/load/scenarios/concurrent-viewers.js
```

### With HTML Dashboard

```bash
K6_WEB_DASHBOARD=true k6 run tests/load/scenarios/concurrent-viewers.js
```

## Test Scenarios

### concurrent-viewers.js

Simulates multiple users joining a room simultaneously:

- **Smoke test**: 1 VU, 30 seconds
- **Load test**: Ramps up to 8 VUs over 5 minutes
- **Stress test**: Goes up to 16 VUs (2x capacity)
- **Spike test**: Sudden traffic spikes
- **Soak test**: 8 VUs for 30 minutes

What it tests:
- Room join API performance
- WebSocket connection establishment
- Concurrent user handling
- Cursor update throughput
- Chat message delivery

### chat-load.js

Focused on chat functionality:

- **Steady state**: 4 VUs sending messages at normal rate
- **Burst test**: 2 VUs sending rapid messages to test rate limiting

What it tests:
- Message send latency
- Message delivery latency
- Rate limit enforcement (30 msg/min)
- Message ordering

## Test Profiles

Edit `k6-config.js` to use different profiles:

```javascript
// In your test file
import { SMOKE_TEST, LOAD_TEST, STRESS_TEST } from '../k6-config.js';

// Use desired profile
export const options = LOAD_TEST;
```

| Profile | VUs | Duration | Use Case |
|---------|-----|----------|----------|
| `SMOKE_TEST` | 1 | 30s | Quick validation |
| `LOAD_TEST` | 4→8→0 | 5m | Normal load |
| `STRESS_TEST` | 4→16→0 | 8m | Beyond capacity |
| `SPIKE_TEST` | 2→16→2 | 2.5m | Sudden traffic |
| `SOAK_TEST` | 8 | 34m | Extended duration |

## Output Formats

### JSON Results

Tests automatically output JSON results:

```bash
cat load-test-results.json
cat chat-load-results.json
```

### InfluxDB/Grafana

Send results to InfluxDB:

```bash
k6 run --out influxdb=http://localhost:8086/k6 tests/load/scenarios/concurrent-viewers.js
```

### Cloud (k6 Cloud)

```bash
k6 cloud tests/load/scenarios/concurrent-viewers.js
```

## Interpreting Results

### Key Metrics

| Metric | Description | Target |
|--------|-------------|--------|
| `room_join_latency` | Time to join a room | p95 < 200ms |
| `ws_connect_latency` | WebSocket connection time | p95 < 1000ms |
| `chat_receive_latency` | Chat message delivery time | p95 < 100ms |
| `http_req_duration` | API response time | p95 < 200ms |
| `http_req_failed` | Request failure rate | < 1% |

### Thresholds

Tests will fail if thresholds are not met:

```
✓ http_req_duration: p(95)<200
✗ chat_receive_latency: p(95)<100
```

### Example Output

```
=== Chromatic Load Test Results ===

Key Metrics:
  Room Join Latency: avg=45ms p95=98ms
  WS Connect Latency: avg=23ms p95=67ms
  Message Latency: avg=12ms p95=34ms
  Request Failure Rate: 0.00%

Threshold Results:
  ✓ http_req_duration: p(95)<200
  ✓ http_req_failed: rate<0.01
  ✓ chat_receive_latency: p(95)<100
```

## Troubleshooting

### Connection Refused

Ensure Chromatic is running:

```bash
make run
# or
go run ./cmd/chromatic
```

### Room Not Found

The test will auto-create a room, but it needs valid admin credentials:

```bash
k6 run -e ADMIN_TOKEN=your-actual-token tests/load/scenarios/concurrent-viewers.js
```

### WebSocket Errors

Check that WebSocket upgrade is working:

```bash
curl -i -N -H "Connection: Upgrade" -H "Upgrade: websocket" \
  http://localhost:8080/ws/test-room
```

### High Latency

- Check server resources (CPU, memory)
- Ensure no other processes are competing
- Try with fewer VUs to establish baseline
