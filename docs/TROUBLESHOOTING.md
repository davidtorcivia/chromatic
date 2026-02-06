# Chromatic Troubleshooting Guide

This guide covers common issues and their solutions when running Chromatic.

## Table of Contents

- [OBS/Streaming Issues](#obsstreaming-issues)
- [WebRTC Connection Problems](#webrtc-connection-problems)
- [TURN Server Issues](#turn-server-issues)
- [Audio/Video Quality](#audiovideo-quality)
- [Watermark Problems](#watermark-problems)
- [High Latency](#high-latency)
- [Database Issues](#database-issues)
- [Authentication Problems](#authentication-problems)
- [Debug Logging](#debug-logging)

---

## OBS/Streaming Issues

### "Invalid stream key" error

**Symptoms**: OBS shows 401 Unauthorized error when starting stream

**Solutions**:

1. **Verify stream key is active**:
   - Go to Admin > Stream Keys
   - Ensure the key is not expired or deleted

2. **Check WHIP URL format**:
   ```
   Correct: https://stream.yourdomain.com/whip/your-stream-key-token
   Wrong:   https://stream.yourdomain.com/whip/
   ```

3. **Check for trailing slashes**: Remove any trailing slashes from the URL

4. **Server logs**:
   ```bash
   docker compose logs chromatic | grep -i "stream key"
   ```

### OBS connects but no video appears

**Symptoms**: OBS shows "Live" but viewers see nothing

**Solutions**:

1. **Check OBS encoder settings**:
   - Profile: **Baseline** (not Main or High)
   - B-frames: **0** (disabled)

   Main/High profiles with B-frames cause 2+ second latency and may not work.

2. **Verify room is set to "Live"**:
   ```bash
   curl -s https://stream.yourdomain.com/api/rooms/your-slug | jq '.status'
   ```

3. **Check server logs for track info**:
   ```bash
   docker compose logs chromatic | grep "Received track"
   ```

### OBS shows "Could not connect to server"

**Solutions**:

1. **Check SSL certificate**: Visit the WHIP URL in a browser - should not show certificate errors

2. **Verify ports are open**:
   ```bash
   curl -I https://stream.yourdomain.com/health
   ```

3. **Check Caddy status**:
   ```bash
   docker compose logs caddy
   ```

### Stream key already in use

**Symptoms**: Error 409 Conflict when starting stream

**Solutions**:

1. **Previous stream didn't close cleanly**:
   ```bash
   docker compose restart chromatic
   ```

2. **Check for active ingests**:
   ```bash
   curl -s https://stream.yourdomain.com/metrics | grep whip_ingests
   ```

---

## WebRTC Connection Problems

### Viewers stuck on "Connecting..."

**Symptoms**: Video element shows loading spinner indefinitely

**Solutions**:

1. **Check ICE server configuration**:
   - Open browser DevTools > Network > WS
   - Find the `room:state` message
   - Verify `iceServers` array is present and populated

2. **Verify TURN provider is working** (see [TURN Server Issues](#turn-server-issues))

3. **Check WebSocket connection**:
   ```javascript
   // In browser console
   // Should show "connected" state
   ```

4. **Firewall blocking UDP**:
   - Try TURN over TCP (port 443)
   - Check if corporate firewall blocks WebRTC

### Connection drops frequently

**Solutions**:

1. **Check network stability**: Run ping test to server

2. **Enable ICE restart** (automatic in Chromatic 1.0+):
   - Check browser console for "ICE restart" messages

3. **Check TURN provider health**:
   - `TURN_MODE=external`: verify Cloudflare/static provider credentials in `.env`
   - `TURN_MODE=hybrid` or `TURN_MODE=self-hosted`: check Coturn logs:
     ```bash
     docker compose --profile self-hosted-turn logs coturn | tail -100
     ```

4. **Increase reconnection timeout**:
   - Default: 10 attempts with exponential backoff
   - Check browser console for reconnection attempts

### "Connection failed" error

**Solutions**:

1. **All ICE candidates failed**: Usually a TURN server issue

2. **Check firewall rules**:
   - Always: 80/443 for HTTPS
   - Self-hosted TURN only: 3478, 5349, and 49152-65535/udp

3. **Try external TURN mode**: See [TURN Modes](../DEPLOYMENT.md#turn-modes)

---

## TURN Server Issues

### Coturn not starting (self-hosted/hybrid only)

**Symptoms**: Container exits immediately or shows errors

**Solutions**:

1. **Check logs**:
   ```bash
   docker compose --profile self-hosted-turn logs coturn
   ```

2. **Common errors**:
   - "Cannot bind to port": Another service using port 3478
   - "Invalid realm": Check TURN_REALM environment variable

3. **Verify configuration**:
   ```bash
   docker compose --profile self-hosted-turn exec coturn cat /etc/coturn/turnserver.conf
   ```

### TURN relay not working

**Test TURN connectivity**:

1. **Using trickle-ice** (web-based):
   - Visit https://webrtc.github.io/samples/src/content/peerconnection/trickle-ice/
   - Enter your TURN server: `turn:your.server:3478`
   - Enter credentials
   - Click "Gather candidates"
   - Look for "relay" type candidates

2. **Using turnutils_uclient**:
   ```bash
   turnutils_uclient -u <username> -w <password> your.server:3478
   ```

3. **Check for "relay" candidates in browser**:
   - Open DevTools > Console
   - Look for "ICE candidate" logs with type "relay"

### TURN authentication failing

**Solutions**:

1. **Verify TURN_SECRET matches**:
   ```bash
   # Check Chromatic config
   docker compose exec chromatic env | grep TURN

   # Check Coturn config (self-hosted/hybrid only)
   docker compose --profile self-hosted-turn exec coturn cat /etc/coturn/turnserver.conf | grep static-auth-secret
   ```

2. **Check credential format**:
   - Username: `timestamp:uniqueID`
   - Password: HMAC-SHA1 of username with secret

3. **Time synchronization**: Server and client time must be within 24 hours

---

## Audio/Video Quality

### Video is pixelated/blocky

**Solutions**:

1. **Increase OBS bitrate**: Recommended 4000-8000 Kbps for 1080p

2. **Check network bandwidth**:
   ```bash
   # On server
   speedtest-cli
   ```

3. **Reduce resolution if bandwidth is limited**: 720p works well at 2500 Kbps

### Audio is choppy or distorted

**Solutions**:

1. **Check OBS audio settings**:
   - Sample rate: 48000 Hz
   - Bitrate: 128-256 Kbps

2. **Network issues**: Audio is more sensitive to packet loss than video

3. **Check for audio track**:
   ```bash
   docker compose logs chromatic | grep "audio track"
   ```

### Video stuttering/freezing

**Solutions**:

1. **Check if using TURN relay**: Relay adds latency (~100-200ms)

2. **OBS settings**:
   - Keyframe interval: 2 seconds
   - Tune: zerolatency

3. **Server resource check**:
   ```bash
   docker stats
   ```

---

## Watermark Problems

### Watermark not appearing

**Solutions**:

1. **Check room configuration**:
   ```bash
   curl -s https://stream.yourdomain.com/api/rooms/your-slug | jq '.watermarkMode'
   ```
   Should return "text", "logo", or "both"

2. **For logo watermarks**:
   - Verify logo was uploaded in Admin > Settings
   - Check logo URL: `https://stream.yourdomain.com/api/config/logo`

3. **Client-side check**: Look for `watermarkMode` in `room:state` WebSocket message

### Logo not displaying

**Solutions**:

1. **Check file permissions**:
   ```bash
   docker compose exec chromatic ls -la /data/logos/
   ```

2. **Supported formats**: PNG, JPEG, WebP

3. **File size**: Keep under 1MB for best performance

### Watermark text variables not working

**Template syntax**:
```
{{ name }}  - Viewer's name
{{ date }}  - Current date
{{ time }}  - Current time
```

**Note**: Variables must have spaces around them: `{{ name }}` not `{{name}}`

---

## High Latency

### Stream has 2+ second delay

**Common causes and solutions**:

1. **B-frames enabled in OBS**:
   - Set Profile to Baseline
   - Set B-frames to 0

2. **TURN relay in use**:
   - Check ICE candidates for "relay" type
   - Try to establish direct connection (requires open UDP ports)

3. **High server load**:
   ```bash
   docker stats
   top
   ```

### Voice chat has delay

**Solutions**:

1. **Check if using relay**: Voice goes through TURN if direct fails

2. **Audio ducking**: When voice is active, stream is ducked (intentional)

3. **Network latency**: Run ping test between participants

### Measuring latency

1. **WebRTC stats** (in browser console):
   ```javascript
   // Get RTT from peer connection stats
   pc.getStats().then(stats => {
     stats.forEach(report => {
       if (report.type === 'candidate-pair' && report.state === 'succeeded') {
         console.log('RTT:', report.currentRoundTripTime * 1000, 'ms');
       }
     });
   });
   ```

2. **Server metrics**:
   ```bash
   curl -s https://stream.yourdomain.com/metrics
   ```

---

## Database Issues

### "Database locked" errors

**Symptoms**: Operations fail with SQLite locked error

**Solutions**:

1. **Check for stuck processes**:
   ```bash
   docker compose exec chromatic ls -la /data/chromatic.db*
   ```

2. **Restart the service**:
   ```bash
   docker compose restart chromatic
   ```

3. **Enable WAL mode** (should be on by default):
   ```bash
   docker compose exec chromatic sqlite3 /data/chromatic.db "PRAGMA journal_mode;"
   # Should return: wal
   ```

### Database corruption

**Symptoms**: Queries fail, missing data

**Solutions**:

1. **Check integrity**:
   ```bash
   docker compose exec chromatic sqlite3 /data/chromatic.db "PRAGMA integrity_check;"
   ```

2. **Restore from backup**:
   ```bash
   TIMESTAMP=20260206_231500  # Replace with your backup timestamp
   ./scripts/restore.sh "$TIMESTAMP" /opt/chromatic/backups
   ```

3. **Attempt repair**:
   ```bash
   docker compose run --rm --no-deps chromatic sh -ec "sqlite3 /data/chromatic.db '.recover' | sqlite3 /data/chromatic-recovered.db"
   ```

---

## Authentication Problems

### Can't login to admin

**Solutions**:

1. **Verify ADMIN_TOKEN**:
   ```bash
   docker compose exec chromatic env | grep ADMIN_TOKEN
   ```

2. **Check cookies**: Clear browser cookies and try again

3. **Session expired**: Sessions last 24 hours by default

### "Not admitted to room" error

**Solutions**:

1. **Waiting room enabled**: Admin must admit participant

2. **Session expired**: Token is only valid for 24 hours

3. **Participant was kicked**: Check admin logs

### Token validation failing

**Solutions**:

1. **Check server time**: Token timestamps must be valid

2. **Verify token secret hasn't changed**: Restarting with different ADMIN_TOKEN invalidates old tokens

---

## Debug Logging

### Enable verbose logging

1. **Set log level** (in .env):
   ```bash
   LOG_LEVEL=debug
   ```

2. **Restart service**:
   ```bash
   docker compose restart chromatic
   ```

3. **View logs**:
   ```bash
   docker compose logs -f chromatic
   ```

### Specific log areas

**WebSocket messages**:
```bash
docker compose logs chromatic | grep "WS message"
```

**WebRTC signaling**:
```bash
docker compose logs chromatic | grep -E "(offer|answer|candidate)"
```

**WHIP/Stream**:
```bash
docker compose logs chromatic | grep -i whip
```

**TURN server**:
```bash
docker compose --profile self-hosted-turn logs coturn
```

### Client-side debugging

1. **Browser console**: Enable verbose logging
2. **WebRTC internals**: `chrome://webrtc-internals` (Chrome only)
3. **Network tab**: Check WebSocket frames

### Collecting debug info for bug reports

```bash
# System info
docker compose version
docker version

# Container logs
docker compose logs --tail=500 > debug-logs.txt

# Metrics
curl -s https://stream.yourdomain.com/metrics >> debug-logs.txt

# Configuration (sanitize secrets!)
docker compose exec chromatic env | grep -v TOKEN | grep -v SECRET >> debug-logs.txt
```

---

## Getting Help

If these solutions don't resolve your issue:

1. **Search existing issues**: Check GitHub issues for similar problems
2. **Collect debug info**: See [Collecting debug info](#collecting-debug-info-for-bug-reports)
3. **Open new issue**: Include logs, configuration, and steps to reproduce
