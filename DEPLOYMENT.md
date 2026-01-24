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

## Third-Party TURN Servers

Chromatic includes a self-hosted Coturn server by default, but you may want to use a third-party TURN service for:

- **Better global coverage**: CDN-like distributed TURN relays worldwide
- **Reduced infrastructure**: No need to manage TURN server ports/firewall
- **Reliability**: Enterprise-grade uptime and redundancy
- **Scalability**: Handle thousands of concurrent connections without server upgrades

### When to Use Third-Party TURN

| Scenario | Recommendation |
|----------|----------------|
| Small team, single region | Self-hosted Coturn is fine |
| Global audience | Use third-party TURN |
| Corporate firewalls blocking UDP | Third-party TURN with TCP fallback |
| High reliability requirements | Third-party TURN |
| Cost-sensitive deployment | Self-hosted Coturn |

### Supported Third-Party TURN Providers

Chromatic supports any standard TURN service. Here are setup instructions for popular providers:

---

### Twilio Network Traversal Service

**Pricing**: Pay-per-GB (~$0.40/GB for TURN relay)

**Setup**:

1. Create a Twilio account at https://www.twilio.com
2. Navigate to **Console > Network Traversal > Manage**
3. Note your **Account SID** and **Auth Token**
4. Generate TURN credentials (temporary tokens are more secure)

**Configuration**:

```bash
# In your .env file
TURN_EXTERNAL_URL=turn:global.turn.twilio.com:3478?transport=udp
TURN_EXTERNAL_USER=<Account_SID>
TURN_EXTERNAL_PASS=<Auth_Token>
```

**For temporary credentials** (recommended for production):

Twilio recommends generating short-lived credentials using their API. You'll need to integrate this into your server. Example credential format:
```
Username: <Account_SID>:<timestamp>
Password: Base64(HMAC-SHA1(<Auth_Token>, <username>))
```

**Multiple protocols for firewall traversal**:
```bash
# UDP (fastest)
TURN_EXTERNAL_URL=turn:global.turn.twilio.com:3478?transport=udp

# TCP (works through more firewalls)
TURN_EXTERNAL_URL=turn:global.turn.twilio.com:443?transport=tcp

# TLS (most compatible with corporate firewalls)
TURN_EXTERNAL_URL=turns:global.turn.twilio.com:443?transport=tcp
```

---

### Xirsys

**Pricing**: Free tier (500MB/month), paid plans start at $10/month

**Setup**:

1. Create account at https://xirsys.com
2. Create a channel in the dashboard
3. Get your credentials from **My Credentials**

**Configuration**:

```bash
# In your .env file
TURN_EXTERNAL_URL=turn:ws.xirsys.com:80?transport=udp
TURN_EXTERNAL_USER=<your-xirsys-username>
TURN_EXTERNAL_PASS=<your-xirsys-credential>
```

**Regional endpoints** (for lower latency):
```bash
# US East
TURN_EXTERNAL_URL=turn:us-east-1.xirsys.com:80?transport=udp

# EU West
TURN_EXTERNAL_URL=turn:eu-west-1.xirsys.com:80?transport=udp

# Asia Pacific
TURN_EXTERNAL_URL=turn:ap-southeast-1.xirsys.com:80?transport=udp
```

---

### Metered TURN

**Pricing**: Free tier (50GB/month), pay-as-you-go ~$0.05/GB

**Setup**:

1. Create account at https://www.metered.ca/stun-turn
2. Create an application
3. Get credentials from the dashboard

**Configuration**:

```bash
# In your .env file
TURN_EXTERNAL_URL=turn:a.relay.metered.ca:443?transport=tcp
TURN_EXTERNAL_USER=<api-key>
TURN_EXTERNAL_PASS=<api-secret>
```

**With TURN over TLS** (for restrictive firewalls):
```bash
TURN_EXTERNAL_URL=turns:a.relay.metered.ca:443?transport=tcp
```

---

### Cloudflare Calls (Beta)

**Pricing**: Part of Cloudflare Workers/Stream pricing

**Note**: Cloudflare Calls provides TURN as part of their WebRTC offering. Check their documentation for the latest integration details.

---

### Google Cloud STUN/TURN

**Pricing**: No free tier for TURN, charges for compute/bandwidth

Google provides STUN servers for free but requires custom infrastructure for TURN:
```bash
# Free STUN only (no relay capability)
stun:stun.l.google.com:19302
stun:stun1.l.google.com:19302
```

For full TURN support, deploy your own Coturn on Google Cloud or use a third-party service.

---

### Multiple TURN Servers

Chromatic supports using both self-hosted and third-party TURN servers simultaneously. The client will try servers in order and use the first working one.

```bash
# In your .env file - use both self-hosted and Twilio
TURN_EXTERNAL_URL=turn:global.turn.twilio.com:3478?transport=udp
TURN_EXTERNAL_USER=<twilio_sid>
TURN_EXTERNAL_PASS=<twilio_token>

# Self-hosted Coturn will also be included automatically
```

The ICE servers sent to clients will include:
1. Your self-hosted Coturn (if running)
2. The external TURN server (if configured)
3. Public STUN servers (for direct connectivity)

---

### Disabling Self-Hosted TURN

If you only want to use third-party TURN (e.g., to simplify firewall rules):

1. Comment out the `coturn` service in `docker-compose.yml`
2. Remove ports 3478, 5349, and 49152-65535 from firewall
3. Configure third-party TURN via environment variables

---

### Testing TURN Connectivity

**Using trickle-ice tool**:

1. Visit https://webrtc.github.io/samples/src/content/peerconnection/trickle-ice/
2. Add your TURN server with credentials
3. Click "Gather candidates"
4. Verify you get "relay" type candidates

**Using turnutils_uclient** (from Coturn):

```bash
turnutils_uclient -u <username> -w <password> <turn-server>:3478
```

**Expected output**: Should show successful allocation and data transmission.

---

### Monitoring TURN Usage

**Self-hosted Coturn**:
```bash
# View Coturn logs
docker-compose logs -f coturn

# Check active sessions (if redis enabled)
docker-compose exec coturn turnadmin -l
```

**Third-party providers**: Check their respective dashboards for:
- Bandwidth usage
- Active connections
- Geographic distribution
- Error rates

---

### TURN Security Best Practices

1. **Use time-limited credentials** when possible (Twilio, Xirsys support this)
2. **Rotate credentials** regularly if using static credentials
3. **Monitor bandwidth** to detect abuse
4. **Set up billing alerts** on third-party services
5. **Use TURN over TLS** (turns://) when clients are behind strict firewalls

## Prometheus Metrics

Chromatic exposes Prometheus-compatible metrics at `/metrics`:

```bash
curl https://stream.yourdomain.com/metrics
```

**Available metrics**:
- `chromatic_active_rooms` - Current active rooms
- `chromatic_websocket_connections` - Active WebSocket connections
- `chromatic_whip_ingests` - Active WHIP streams from OBS
- `chromatic_active_subscribers` - WebRTC subscribers receiving stream
- `chromatic_waiting_participants` - Participants in waiting rooms
- `chromatic_rooms_created_total` - Total rooms created
- `chromatic_messages_chat_total` - Total chat messages
- `chromatic_files_uploaded_total` - Total file uploads
- `chromatic_uptime_seconds` - Server uptime

**Grafana dashboard** example query:
```promql
rate(chromatic_messages_chat_total[5m])
```

## Support

For issues and feature requests, please open an issue on GitHub.
