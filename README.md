# Chromatic

**Self-hosted, low-latency streaming platform for professional colorists to conduct remote grading sessions with advertising creatives and directors.**

Chromatic enables real-time color-critical streaming from DaVinci Resolve via OBS, with interactive review tools, voice chat, and client-friendly browser-based viewing. Built for small teams (2-8 viewers) who need sub-second latency without compromising on color accuracy.

## Key Features

### Streaming
- **Sub-second latency**: WebRTC streaming optimized for real-time collaboration
- **High fidelity**: Up to 10 Mbps streaming for color-critical work
- **OBS integration**: Native WHIP protocol support (OBS 30+)
- **SFU architecture**: Efficient one-to-many broadcasting via Pion WebRTC

### Collaboration
- **Laser pointer**: Interactive pointing visible to all participants
- **Voice chat**: Built-in audio with intelligent ducking when speaking
- **Chat messaging**: Text communication during sessions
- **File sharing**: Share images, audio references, and PDFs

### Security & Access Control
- **Waiting room**: Approve participants before they join
- **Password protection**: Secure rooms with passwords
- **Watermarking**: Overlay text and logo watermarks on the client (visual deterrent — a determined viewer with DevTools can hide overlays; burned-in watermarking would require server-side re-encode)
- **Session management**: Persistent sessions, kick/mute controls

### Operations
- **Self-hosted**: Full control over your data
- **Docker deployment**: Pull prebuilt images from GHCR (no local build required)
- **Cloudflare TURN default**: Recommended non-self-hosted TURN path
- **Flexible TURN modes**: External-only, hybrid, or fully self-hosted Coturn
- **Prometheus metrics**: Built-in monitoring endpoint

## Quick Start

### Prerequisites

- Production deployment: Docker Engine + Docker Compose plugin
- Development only: Go 1.24+ and Node.js 20+
- OBS Studio 30+ (for streaming)

### Development

```bash
# Clone and enter directory
git clone https://github.com/davidtorcivia/chromatic.git
cd chromatic

# Install dependencies
make deps

# Configure environment
cp deployments/.env.example .env
# Edit .env with your settings

# Run backend (terminal 1)
make dev

# Run frontend (terminal 2)
make dev-frontend
```

Access at `http://localhost:5173`

### Production Deployment (Pull-Based)

```bash
# Clone once to get Docker Compose/Caddy config (no local build needed)
git clone https://github.com/davidtorcivia/chromatic.git
cd chromatic/deployments

# Configure environment
cp .env.example .env
nano .env  # Set ADMIN_TOKEN, PUBLIC_URL, DOMAIN, CHROMATIC_IMAGE, TURN_MODE, and TURN provider vars

# Pull prebuilt image and start
docker compose pull
docker compose up -d

# Verify
curl -fsS https://stream.yourdomain.com/health
```

Coturn is optional when `TURN_MODE=external` (Cloudflare TURN recommended).

Set `CHROMATIC_IMAGE` to an immutable release tag (for example `sha-<commit>`) or
digest (`@sha256:...`) for reproducible deploys and safe rollback.

Recommended TURN path for most deployments:
- `TURN_MODE=external`
- `TURN_CLOUDFLARE_KEY_ID=<key id>`
- `TURN_CLOUDFLARE_API_TOKEN=<api token>`

See [DEPLOYMENT.md](DEPLOYMENT.md) for complete instructions.
After login, open Admin → Setup Wizard to finish TURN, branding, stream keys, and
first-room setup. The wizard does not edit your `.env` file.

### Published Images

Production images are automatically published to GitHub Container Registry (GHCR):

- `ghcr.io/davidtorcivia/chromatic`
- Tags include branch, semver (`vX.Y.Z`), and immutable `sha-<commit>`
- Default branch also publishes `latest` (use only for non-critical environments)

For production, deploy using a pinned immutable tag or digest.
If anonymous pulls fail, set the GHCR package visibility to public.

## OBS Configuration

1. Open **OBS Settings > Stream**
2. Set Service to **WHIP**
3. Enter Server URL: `https://stream.yourdomain.com/whip/{stream-key}`

### Critical Encoder Settings

| Setting | Value | Why |
|---------|-------|-----|
| Profile | **Baseline** | Main/High can use B-frames |
| B-Frames | **0** | Non-zero causes 2+ second latency |
| Keyframe Interval | 2 seconds | Balance latency vs recovery |
| Tune | zerolatency | Optimizes for real-time |
| Bitrate | 6000-10000 Kbps | Adjust for your bandwidth |

> **Warning**: B-frames MUST be disabled. This is the #1 cause of high latency issues.

## Architecture

```
                           Internet
                              │
                    ┌─────────┴─────────┐
                    │      Caddy        │
                    │  (SSL + Proxy)    │
                    └─────────┬─────────┘
                              │
              ┌───────────────┼───────────────┐
              │               │               │
     ┌────────┴────────┐     │      ┌────────┴────────┐
     │    Chromatic    │     │      │     Coturn      │
     │   (Go Server)   │     │      │   (TURN Relay)  │
     │                 │     │      │                 │
     │ ├ HTTP API      │  SQLite    │ ├ NAT Traversal │
     │ ├ WebSocket     │  (WAL)     │ └ Media Relay   │
     │ └ SFU (Pion)    │     │      │                 │
     └─────────────────┘     │      └─────────────────┘
              ▲               │               ▲
              │               │               │
     ┌────────┴────────┐     │      ┌────────┴────────┐
     │   OBS Studio    │─────┘      │    Viewers      │
     │     (WHIP)      │            │   (Browser)     │
     └─────────────────┘            └─────────────────┘
```

## Technology Stack

| Component | Technology |
|-----------|------------|
| Backend | Go 1.24+, Pion WebRTC v4 |
| Frontend | SvelteKit 2, Svelte 5 |
| Database | SQLite with WAL mode |
| Reverse Proxy | Caddy (auto SSL) |
| TURN Server | Cloudflare TURN (default external) or Coturn |
| Container | Docker Compose |

## Project Structure

```
chromatic/
├── cmd/chromatic/          # Application entrypoint
├── internal/
│   ├── api/                # HTTP handlers and middleware
│   ├── config/             # Configuration management
│   ├── database/           # SQLite + migrations
│   ├── metrics/            # Prometheus metrics
│   ├── webrtc/             # Pion WebRTC, SFU, WHIP
│   └── websocket/          # Real-time messaging hub
├── web/                    # SvelteKit frontend
│   └── src/
│       ├── lib/            # Shared components and stores
│       └── routes/         # Pages and layouts
├── deployments/            # Docker configs
│   ├── Dockerfile
│   ├── docker-compose.yml
│   └── Caddyfile
└── docs/                   # Documentation
```

## Documentation

| Document | Description |
|----------|-------------|
| [DEPLOYMENT.md](DEPLOYMENT.md) | Production deployment guide |
| [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) | Common issues and solutions |
| [docs/BACKUP.md](docs/BACKUP.md) | Backup and restore procedures |
| [docs/DEPLOYMENT_CHECKLIST.md](docs/DEPLOYMENT_CHECKLIST.md) | Pre-flight checklist |
| [docs/api.md](docs/api.md) | API reference |
| [docs/VOICE_CHAT.md](docs/VOICE_CHAT.md) | Voice chat system details |
| [e2e/README.md](e2e/README.md) | End-to-end testing guide |
| [tests/load/README.md](tests/load/README.md) | Load testing guide |

## TURN Modes

Chromatic supports three TURN deployment modes:

- **`external` (recommended)**: Cloudflare TURN or another hosted TURN provider
- **`hybrid`**: Self-hosted Coturn plus external provider fallback
- **`self-hosted`**: Coturn only

See [DEPLOYMENT.md#turn-modes](DEPLOYMENT.md#turn-modes) for exact configuration.

## Monitoring

### Prometheus Metrics

Prometheus-compatible metrics at `/metrics`. **Authentication is required** —
scrape with `Authorization: Bearer $ADMIN_TOKEN`:

```bash
curl -H "Authorization: Bearer $ADMIN_TOKEN" https://stream.yourdomain.com/metrics
```

Key metrics:
- `chromatic_active_rooms` - Current active rooms
- `chromatic_websocket_connections` - Active WebSocket connections
- `chromatic_whip_ingests` - Active WHIP streams
- `chromatic_active_subscribers` - WebRTC subscribers
- `chromatic_messages_chat_total` - Total chat messages
- `chromatic_errors_total` - Total errors

### Grafana Dashboard

A pre-built Grafana dashboard is included:

```bash
# Start monitoring stack with Chromatic
cd deployments
docker compose -f docker-compose.yml -f docker-compose.monitoring.yml up -d
```

Access Grafana at `http://localhost:3001` (admin/admin)

The dashboard shows:
- Active rooms, viewers, and streams
- Connection history
- Message throughput
- Error rates

## Browser Compatibility

| Browser | Status | Notes |
|---------|--------|-------|
| Safari 15+ (macOS) | Reference | Best color management |
| Chrome 90+ (macOS) | Primary | Most users |
| Chrome 90+ (Windows) | Supported | Gamma shifts possible |
| Edge 90+ | Supported | Chromium-based |
| Firefox 90+ | Degraded | WebRTC quirks |
| Mobile Safari/Chrome | Supported | Voice only |

## Development

### Running Tests

```bash
# Run all backend tests
make test

# Run with race detector
go test -race ./...

# Run with coverage
make test-coverage

# Run specific package
go test ./internal/webrtc/...

# Run frontend tests
cd web && npm test

# Run E2E tests (requires server running)
cd e2e && npm test

# Run load tests (requires k6)
k6 run tests/load/scenarios/concurrent-viewers.js
```

### Building

```bash
# Build binary
make build

# Build Docker image
make docker-build  # Produces local image: chromatic:local

# Build frontend only
make build-frontend
```

### Code Style

```bash
# Format code
make fmt

# Run linter
make lint
```

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Commit changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

Please ensure:
- All tests pass (`make test`)
- Code follows existing style (`make lint`)
- Documentation is updated if needed

## Troubleshooting

### Stream has high latency
- Set B-frames to **0** in OBS
- Use **Baseline** profile (not Main or High)
- Set tune to **zerolatency**

### Viewers stuck on "Connecting..."
- Check TURN provider configuration (`TURN_MODE`, Cloudflare creds, or Coturn if self-hosted)
- Verify firewall allows UDP ports 49152-65535
- Try third-party TURN if behind corporate firewall

### OBS shows "Invalid stream key"
- Verify stream key exists in admin dashboard
- Check WHIP URL format (no trailing slash)
- Ensure SSL certificate is valid

See [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) for more solutions.

## License

MIT License - see [LICENSE](LICENSE) for details.

## Acknowledgments

- [Pion WebRTC](https://github.com/pion/webrtc) - Pure Go WebRTC implementation
- [SvelteKit](https://kit.svelte.dev/) - Web application framework
- [Coturn](https://github.com/coturn/coturn) - TURN server
- [Caddy](https://caddyserver.com/) - Automatic HTTPS reverse proxy
