# Chromatic Deployment Guide

This guide covers deploying Chromatic to a production server using Docker.

## Server Requirements

### Hardware
- **CPU**: 2+ cores recommended
- **RAM**: 2GB minimum, 4GB recommended
- **Storage**: 20GB+ for video files and uploads
- **Network**: Public IP address required for WebRTC

### Software
- Docker and Docker Compose
- Domain name with DNS configured
- SSL certificate (handled by Caddy)

### Network/Firewall
Open the following ports:

| Port | Protocol | Purpose |
|------|----------|---------|
| 80 | TCP | HTTP (redirects to HTTPS) |
| 443 | TCP | HTTPS |
| 3478 | TCP/UDP | TURN server (STUN/TURN) |
| 5349 | TCP | TURN over TLS |
| 49152-65535 | UDP | WebRTC media relay (TURN) |

## Quick Start

### 1. Clone and Configure

```bash
# Clone the repository
git clone https://github.com/yourorg/chromatic.git
cd chromatic/deployments

# Copy and edit environment variables
cp .env.example .env
nano .env
```

### 2. Generate Secrets

```bash
# Generate ADMIN_TOKEN (for admin login)
openssl rand -hex 32

# Generate TURN_SECRET (for TURN server auth)
openssl rand -hex 32
```

### 3. Configure Environment

Edit `.env` with your values:

```bash
# Required
ADMIN_TOKEN=your-generated-admin-token
PUBLIC_URL=https://stream.yourdomain.com
TURN_SECRET=your-generated-turn-secret
TURN_REALM=stream.yourdomain.com
PUBLIC_IP=your.server.ip.address

# Production mode (recommended)
PRODUCTION_MODE=true
ALLOWED_ORIGINS=https://stream.yourdomain.com
```

### 4. Configure Caddy

Edit `deployments/Caddyfile`:

```caddy
stream.yourdomain.com {
    reverse_proxy chromatic:3000
}
```

### 5. Deploy

```bash
cd deployments
docker-compose up -d
```

### 6. Verify

1. Visit `https://stream.yourdomain.com/admin`
2. Login with your ADMIN_TOKEN
3. Create a stream key
4. Create a room

## OBS Configuration

### Setting Up WHIP Streaming

1. Open OBS Studio (version 30.0+ required for WHIP support)
2. Go to **Settings > Stream**
3. Configure:
   - **Service**: WHIP
   - **Server**: Your WHIP URL (from admin dashboard)

   Example: `https://stream.yourdomain.com/whip/your-stream-key-token`

4. Click **Start Streaming**

### Recommended OBS Settings

**Video Settings**:
- Resolution: 1920x1080
- Frame Rate: 30 or 60 FPS
- Output Mode: Simple

**Output Settings**:
- Encoder: x264 or Hardware (NVENC/QuickSync)
- Bitrate: 4000-8000 Kbps
- Keyframe Interval: 2 seconds

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                        Internet                          │
└────────────────────────┬────────────────────────────────┘
                         │
              ┌──────────┴──────────┐
              │      Firewall       │
              │   (80, 443, 3478,   │
              │    5349, UDP range) │
              └──────────┬──────────┘
                         │
              ┌──────────┴──────────┐
              │        Caddy        │
              │   (Reverse Proxy)   │
              │   SSL Termination   │
              └──────────┬──────────┘
                         │
         ┌───────────────┼───────────────┐
         │               │               │
┌────────┴────────┐ ┌────┴────┐ ┌────────┴────────┐
│    Chromatic    │ │         │ │     Coturn      │
│   (Go Server)   │ │  SQLite │ │  (TURN Server)  │
│                 │ │   DB    │ │                 │
│ - HTTP API      │ │         │ │ - NAT Traversal │
│ - WebSocket     │ │         │ │ - Media Relay   │
│ - SFU (Pion)    │ │         │ │                 │
└─────────────────┘ └─────────┘ └─────────────────┘
```

## Docker Compose Services

| Service | Purpose | Port |
|---------|---------|------|
| `chromatic` | Main application server | 3000 (internal) |
| `caddy` | Reverse proxy with automatic SSL | 80, 443 |
| `coturn` | TURN server for NAT traversal | 3478, 5349, UDP range |

## Data Volumes

```bash
./data/
├── chromatic.db    # SQLite database
├── files/          # Uploaded files
├── logos/          # Watermark logos
└── caddy/          # SSL certificates
```

## Backup

### Database Backup

```bash
# Stop the service first for consistency
docker-compose stop chromatic

# Copy the database
cp data/chromatic.db backup/chromatic-$(date +%Y%m%d).db

# Restart
docker-compose start chromatic
```

### Full Backup

```bash
tar -czvf chromatic-backup-$(date +%Y%m%d).tar.gz data/
```

## Updating

```bash
cd deployments

# Pull latest images
docker-compose pull

# Restart with new images
docker-compose up -d

# Check logs
docker-compose logs -f chromatic
```

## Troubleshooting

### WebRTC Connection Issues

1. **Check TURN server**:
   ```bash
   docker-compose logs coturn
   ```

2. **Verify ports are open**:
   ```bash
   # Test TURN port
   nc -zv your.server.ip 3478
   ```

3. **Check ICE candidates**:
   - Open browser DevTools > Network > WS
   - Look for `room:state` message with `iceServers`

### Stream Not Starting

1. **Check OBS settings**:
   - Verify WHIP URL is correct
   - Check stream key is valid

2. **Check server logs**:
   ```bash
   docker-compose logs -f chromatic
   ```

3. **Common errors**:
   - `401 Unauthorized`: Invalid stream key
   - `Connection refused`: Check firewall rules

### High Latency

1. **Check TURN usage**:
   - TURN relay adds latency (~100-200ms)
   - Direct P2P is faster when possible

2. **Optimize encoder**:
   - Use hardware encoding if available
   - Reduce bitrate or resolution

### Database Locked Errors

SQLite may report "database locked" under heavy load:

1. Check for stuck processes:
   ```bash
   docker-compose exec chromatic ls -la /data/chromatic.db*
   ```

2. Restart the service:
   ```bash
   docker-compose restart chromatic
   ```

## Security Considerations

### Secrets Management

- Never commit `.env` to version control
- Use strong, randomly generated tokens
- Rotate ADMIN_TOKEN periodically

### Network Security

- Keep firewall rules minimal
- Use HTTPS only (Caddy handles this)
- Configure ALLOWED_ORIGINS in production

### Access Control

- Share room links selectively
- Use passwords for sensitive sessions
- Enable waiting room for verification

## Monitoring

### Health Check

```bash
curl -s https://stream.yourdomain.com/health | jq
```

### Container Status

```bash
docker-compose ps
docker stats
```

### Logs

```bash
# All services
docker-compose logs -f

# Specific service
docker-compose logs -f chromatic
```

## External TURN Server (Optional)

For better global connectivity, you can add an external TURN service like Twilio:

1. Sign up for Twilio (or similar service)
2. Get TURN credentials
3. Configure in admin Settings page or via environment:

```bash
TURN_EXTERNAL_URL=turn:global.turn.twilio.com:3478?transport=udp
TURN_EXTERNAL_USER=your_twilio_username
TURN_EXTERNAL_PASS=your_twilio_credential
```

## Support

For issues and feature requests, please open an issue on GitHub.
