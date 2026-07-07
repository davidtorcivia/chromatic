# Deployment Checklist

Use this checklist for a simple production rollout from GHCR images.

Repository: `https://github.com/davidtorcivia/chromatic`

## 1. Host and Network

- [ ] Docker Engine installed
- [ ] `docker compose version` works
- [ ] Domain configured (for example `stream.yourdomain.com`)
- [ ] DNS points domain to your server
- [ ] Ports `80/tcp` and `443/tcp` are open
- [ ] If self-hosting Coturn, TURN ports are open (`3478`, `5349`, `49152-65535/udp`)

## 2. Files

- [ ] Cloned repo to server: `/opt/chromatic`
- [ ] Working in `/opt/chromatic/deployments`
- [ ] Copied `deployments/.env.example` to `deployments/.env`

## 3. Required Env Values

- [ ] `ADMIN_TOKEN` set to a strong random value
- [ ] `PUBLIC_URL` set to your HTTPS URL
- [ ] `DOMAIN` set to host name used by Caddy
- [ ] `CHROMATIC_IMAGE` pinned to immutable image tag/digest

## 4. TURN Mode

Recommended:

- [ ] `TURN_MODE=external`
- [ ] `TURN_CLOUDFLARE_KEY_ID` set
- [ ] `TURN_CLOUDFLARE_API_TOKEN` set

If using self-hosted/hybrid:

- [ ] `TURN_SECRET` set
- [ ] `TURN_REALM` set
- [ ] `PUBLIC_IP` set
- [ ] Deploy command includes `--profile self-hosted-turn`

## 5. Security Settings

- [ ] `PRODUCTION_MODE=true`
- [ ] `ALLOWED_ORIGINS` matches your public domain
- [ ] `TRUSTED_PROXIES` reviewed for your environment

## 6. Deploy

- [ ] `docker compose pull`
- [ ] `docker compose up -d`
- [ ] If self-hosted/hybrid: `docker compose --profile self-hosted-turn up -d`

## 7. Verify

- [ ] `curl -fsS https://stream.yourdomain.com/health`
- [ ] Admin login works at `/admin`
- [ ] Setup wizard shows Ready and the TURN server reachability test passes
- [ ] OBS stream starts with WHIP URL
- [ ] Viewer receives low-latency stream

## 8. Operational Readiness

- [ ] Backup path and cadence documented (`docs/BACKUP.md`)
- [ ] Monitoring checked (`/metrics`)
- [ ] Rollback plan documented (`CHROMATIC_IMAGE` previous pin)

