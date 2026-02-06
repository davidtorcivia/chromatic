# Chromatic Backup and Restore

This guide covers backup, verification, and restore for production deployments using:

- `deployments/docker-compose.yml`
- Docker named volume `chromatic_data`
- Scripts in `scripts/`

Run all commands from the repository root (for example `/opt/chromatic`).

## What Gets Backed Up

`./scripts/backup.sh` creates timestamped artifacts in your backup directory:

- `chromatic-YYYYMMDD_HHMMSS.db` - SQLite database backup
- `files-YYYYMMDD_HHMMSS.tar.gz` - Uploaded files (if present)
- `logos-YYYYMMDD_HHMMSS.tar.gz` - Watermark logos (if present)
- `manifest-YYYYMMDD_HHMMSS.txt` - Backup manifest

## Quick Backup

```bash
# Recommended location
mkdir -p /opt/chromatic/backups
chmod 700 /opt/chromatic/backups

# Create backup
./scripts/backup.sh /opt/chromatic/backups
```

By default, backups older than 7 days are removed.
Set retention with `BACKUP_RETENTION_DAYS`:

```bash
BACKUP_RETENTION_DAYS=30 ./scripts/backup.sh /opt/chromatic/backups
```

## Verify a Backup

After backup, run verification before updates or maintenance:

```bash
./scripts/verify-backup.sh 20260206_231500 /opt/chromatic/backups
```

Replace `20260206_231500` with your backup timestamp.

## Quick Restore

1. Pick the backup timestamp you want to restore.
2. Run restore:

```bash
./scripts/restore.sh 20260206_231500 /opt/chromatic/backups
```

Restore behavior:

- Creates a pre-restore DB snapshot (`chromatic-pre-restore-*.db`)
- Stops `chromatic`
- Restores DB and media artifacts from the selected timestamp
- Starts `chromatic` again

## Update Safety Pattern (Recommended)

Before upgrading to a new image tag:

```bash
./scripts/backup.sh /opt/chromatic/backups
TIMESTAMP=20260206_231500  # Replace with your backup timestamp
./scripts/verify-backup.sh "$TIMESTAMP" /opt/chromatic/backups

docker compose -f deployments/docker-compose.yml pull
docker compose -f deployments/docker-compose.yml up -d
```

If the upgrade fails, restore the last known-good backup:

```bash
TIMESTAMP=20260206_231500  # Replace with your backup timestamp
./scripts/restore.sh "$TIMESTAMP" /opt/chromatic/backups
```

## Automation with Cron

Run nightly backups at 2 AM:

```cron
0 2 * * * cd /opt/chromatic && /opt/chromatic/scripts/backup.sh /opt/chromatic/backups >> /var/log/chromatic-backup.log 2>&1
```

Run weekly verification at 2:15 AM on Sundays:

```cron
15 2 * * 0 cd /opt/chromatic && /opt/chromatic/scripts/verify-backup.sh $(ls -1 /opt/chromatic/backups/chromatic-*.db | sed 's/.*chromatic-//' | sed 's/.db$//' | tail -n1) /opt/chromatic/backups >> /var/log/chromatic-backup-verify.log 2>&1
```

## Manual Backup Commands (Without Scripts)

If needed, run manual commands against Compose:

```bash
mkdir -p /opt/chromatic/backups

# Database hot backup
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
docker compose -f deployments/docker-compose.yml run --rm --no-deps \
  -v /opt/chromatic/backups:/backup \
  chromatic sh -ec "sqlite3 /data/chromatic.db \".backup '/backup/chromatic-${TIMESTAMP}.db'\""

# Files and logos archives (optional)
docker compose -f deployments/docker-compose.yml run --rm --no-deps \
  -v /opt/chromatic/backups:/backup \
  chromatic sh -ec "[ -d /data/files ] && [ \"\$(ls -A /data/files 2>/dev/null)\" ] && tar -czf '/backup/files-${TIMESTAMP}.tar.gz' -C /data files || true"

docker compose -f deployments/docker-compose.yml run --rm --no-deps \
  -v /opt/chromatic/backups:/backup \
  chromatic sh -ec "[ -d /data/logos ] && [ \"\$(ls -A /data/logos 2>/dev/null)\" ] && tar -czf '/backup/logos-${TIMESTAMP}.tar.gz' -C /data logos || true"
```

## Restore Validation Checklist

After restore:

```bash
docker compose -f deployments/docker-compose.yml ps
docker compose -f deployments/docker-compose.yml logs -f chromatic
curl -fsS https://stream.yourdomain.com/health
```

Confirm in Admin UI:

- rooms exist
- stream keys exist
- logos/watermarks are present
- test room can stream and play back

## Best Practices

1. Keep backups on a separate disk or object storage.
2. Keep at least one off-server copy.
3. Verify backups regularly, not only when incidents happen.
4. Run a test restore monthly.
5. Keep immutable release tags (`sha-<commit>` or digest) in `.env` for safe rollback.
