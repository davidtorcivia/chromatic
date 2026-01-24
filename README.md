# Chromatic

Self-hosted, low-latency streaming platform for professional colorists to conduct remote grading sessions with advertising creatives and directors.

## Features

- **High-Fidelity Streaming**: 8-10 Mbps WebRTC streaming optimized for color-critical work on MacBook Pro XDR displays
- **Sub-second Latency**: Real-time WebRTC streaming from DaVinci Resolve via OBS
- **Interactive Review**: Laser pointer visible to all participants for precise feedback
- **Client-Friendly**: No-install, browser-based viewing optimized for non-technical stakeholders
- **Voice Chat**: Built-in audio with intelligent ducking when participants speak
- **File Sharing**: Share images, audio references, and PDFs during sessions
- **Watermarking**: Text and logo watermarks with anti-tampering protection
- **Waiting Room**: Control when participants can enter sessions
- **Password Protection**: Secure rooms with password access

## Quick Start

### Prerequisites

- Go 1.22+
- Node.js 20+
- Docker (optional, for deployment)
- OBS Studio 30+ (for streaming)

### Development

1. Clone the repository:
   ```bash
   git clone https://github.com/yourusername/chromatic.git
   cd chromatic
   ```

2. Install dependencies:
   ```bash
   make deps
   ```

3. Set up environment variables:
   ```bash
   cp deployments/.env.example .env
   # Edit .env with your configuration
   ```

4. Run the backend:
   ```bash
   make dev
   ```

5. Run the frontend (in a separate terminal):
   ```bash
   make dev-frontend
   ```

### Docker Deployment

1. Configure environment:
   ```bash
   cd deployments
   cp .env.example .env
   # Generate secrets:
   # ADMIN_TOKEN=$(openssl rand -hex 32)
   # TURN_SECRET=$(openssl rand -hex 32)
   ```

2. Build and run:
   ```bash
   make docker-build
   make docker-up
   ```

## OBS Configuration

Configure OBS for WHIP streaming:

1. Open OBS Settings → Stream
2. Service: **WHIP**
3. Server: `https://your-domain.com/whip/{stream-key}`

### Required Encoder Settings

| Setting | Value |
|---------|-------|
| Encoder | x264 / NVENC / QSV |
| Rate Control | CBR |
| Bitrate | 6000-10000 Kbps |
| Keyframe Interval | 2 seconds |
| Profile | High |
| Tune | zerolatency |
| **B-Frames** | **0** (CRITICAL) |

> ⚠️ **Important**: B-frames MUST be set to 0. Non-zero values cause 2+ second latency due to browser reordering issues.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         DOCKER HOST                              │
│  ┌────────────────────────────────────────────────────────────┐ │
│  │                    CHROMATIC SERVER                         │ │
│  │                                                             │ │
│  │  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐     │ │
│  │  │   Caddy     │    │  Chromatic  │    │   Coturn    │     │ │
│  │  │  (Proxy)    │───▶│   (Go/SFU)  │    │   (TURN)    │     │ │
│  │  └─────────────┘    └──────┬──────┘    └─────────────┘     │ │
│  │                            │                                │ │
│  │                     ┌──────┴──────┐                        │ │
│  │                     │   SQLite    │                        │ │
│  │                     │  (WAL Mode) │                        │ │
│  │                     └─────────────┘                        │ │
│  └────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
         ▲                        ▲                         ▲
         │ HTTPS/WSS              │ WHIP                    │ WebRTC
    ┌────┴────┐              ┌────┴────┐              ┌─────┴─────┐
    │ Browser │              │   OBS   │              │  Clients  │
    │ (Admin) │              │ Studio  │              │  (2-8)    │
    └─────────┘              └─────────┘              └───────────┘
```

## Technology Stack

- **Backend**: Go 1.22+, Pion WebRTC v4
- **Frontend**: SvelteKit 2, Svelte 5
- **Database**: SQLite with WAL mode
- **Reverse Proxy**: Caddy
- **TURN Server**: Coturn
- **Containerization**: Docker Compose

## Project Structure

```
chromatic/
├── cmd/chromatic/              # Application entrypoint
├── internal/
│   ├── api/                    # HTTP handlers
│   ├── config/                 # Configuration
│   ├── database/               # SQLite + migrations
│   ├── models/                 # Data models
│   ├── webrtc/                 # Pion WebRTC, SFU, WHIP
│   └── websocket/              # Real-time messaging
├── web/                        # SvelteKit frontend
├── deployments/                # Docker configs
└── docs/                       # Documentation
```

## API Reference

See [API Documentation](docs/api.md) for detailed endpoint documentation.

## Browser Support

| Browser | Support | Notes |
|---------|---------|-------|
| Safari 15+ (macOS) | ✅ Reference | Best color management |
| Chrome 90+ (macOS) | ✅ Primary | Most users |
| Chrome 90+ (Windows) | ✅ Supported | Gamma shifts possible |
| Edge 90+ | ✅ Supported | Chromium-based |
| Firefox 90+ | ⚠️ Degraded | WebRTC quirks |
| Mobile Safari | ✅ Supported | Voice only, no camera |
| Mobile Chrome | ✅ Supported | Voice only, no camera |

## License

MIT License - see [LICENSE](LICENSE) for details.

## Acknowledgments

- [Pion WebRTC](https://github.com/pion/webrtc) - Pure Go WebRTC implementation
- [SvelteKit](https://kit.svelte.dev/) - Web application framework
- [Coturn](https://github.com/coturn/coturn) - TURN server
