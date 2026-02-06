# Chromatic Deployment Guide

This guide is production-focused and image-pull based.
For production, you do **not** build locally. You pull prebuilt images from:

- `ghcr.io/davidtorcivia/chromatic`

Repository source is always:

- `https://github.com/davidtorcivia/chromatic`

## Recommended Architecture

Default recommendation for most teams:

- `TURN_MODE=external`
- Cloudflare TURN credentials in `.env`
- Caddy for HTTPS + reverse proxy
- Chromatic container from GHCR
- No self-hosted Coturn required

Use self-hosted Coturn only if you specifically want to run TURN yourself.

## Prerequisites

- Docker Engine + Docker Compose plugin (`docker compose`)
- A domain name pointed to your server
- Ports open:
  - `80/tcp` and `443/tcp` for Caddy
  - For self-hosted Coturn only: `3478/tcp+udp`, `5349/tcp`, and `49152-65535/udp`

## Quick Deploy (Production)

```bash
# Clone once to get deployment files
git clone https://github.com/davidtorcivia/chromatic.git /opt/chromatic
cd /opt/chromatic/deployments

# Create env file
cp .env.example .env
```

Edit `.env` with at least:

```bash
ADMIN_TOKEN=<strong-random-token>
PUBLIC_URL=https://stream.yourdomain.com
DOMAIN=stream.yourdomain.com
CHROMATIC_IMAGE=ghcr.io/davidtorcivia/chromatic:sha-<commit>

TURN_MODE=external
TURN_CLOUDFLARE_KEY_ID=<cloudflare-turn-key-id>
TURN_CLOUDFLARE_API_TOKEN=<cloudflare-api-token>

PRODUCTION_MODE=true
ALLOWED_ORIGINS=https://stream.yourdomain.com
```

Then deploy:

```bash
docker compose pull
docker compose up -d
curl -fsS https://stream.yourdomain.com/health
```

Open `https://stream.yourdomain.com/admin`, log in with `ADMIN_TOKEN`, then run the Setup Wizard.

## Image Pinning Best Practices

Use immutable references in `CHROMATIC_IMAGE`:

- Tag pin: `ghcr.io/davidtorcivia/chromatic:sha-<commit>`
- Digest pin: `ghcr.io/davidtorcivia/chromatic@sha256:<digest>`

Avoid `latest` in production.

If the GHCR package is private:

```bash
echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GHCR_USER" --password-stdin
```

## TURN Modes

### 1) `external` (Recommended)

Best for low ops overhead, dynamic/home IPs, and reliability.

```bash
TURN_MODE=external
TURN_CLOUDFLARE_KEY_ID=<key-id>
TURN_CLOUDFLARE_API_TOKEN=<token>
```

Optional static provider fallback:

```bash
TURN_EXTERNAL_URLS=turn:provider.example.com:3478?transport=udp,turns:provider.example.com:443?transport=tcp
TURN_EXTERNAL_USER=<user>
TURN_EXTERNAL_PASS=<pass>
```

### 2) `hybrid` (Self-hosted Coturn + External)

Use when you want local TURN plus hosted fallback:

```bash
TURN_MODE=hybrid
TURN_SECRET=<random-hex>
TURN_REALM=stream.yourdomain.com
PUBLIC_IP=<server-public-ip>
TURN_CLOUDFLARE_KEY_ID=<key-id>
TURN_CLOUDFLARE_API_TOKEN=<token>
```

Start Coturn profile:

```bash
docker compose --profile self-hosted-turn up -d
```

### 3) `self-hosted`

Use only your own Coturn:

```bash
TURN_MODE=self-hosted
TURN_SECRET=<random-hex>
TURN_REALM=stream.yourdomain.com
PUBLIC_IP=<server-public-ip>
```

Start Coturn profile:

```bash
docker compose --profile self-hosted-turn up -d
```

## Dynamic DNS / Home Server Notes

If your public IP changes, self-hosted TURN (`PUBLIC_IP`) can break until updated.
For home servers and dynamic DNS, `TURN_MODE=external` with Cloudflare TURN is usually the most robust option.

You can still host TURN on a VPS if you want full control:

- Run Coturn on the VPS with static IP
- Set `TURN_MODE=external`
- Point `TURN_EXTERNAL_URLS` to that VPS
- Set `TURN_EXTERNAL_USER` / `TURN_EXTERNAL_PASS`

## Updates and Rollback

```bash
cd /opt/chromatic/deployments
docker compose pull
docker compose up -d
```

Rollback is just changing `CHROMATIC_IMAGE` back to an older pinned tag/digest and redeploying.

## Verification Checklist

- `curl -fsS https://stream.yourdomain.com/health` returns success
- Admin login works
- Setup Wizard TURN test passes
- OBS can stream to `/whip/<stream_key_token>`
- Viewer joins and receives video/audio

## Monitoring and Backups

- Metrics endpoint: `https://stream.yourdomain.com/metrics`
- Backup/restore docs: `docs/BACKUP.md`
- Troubleshooting: `docs/TROUBLESHOOTING.md`

