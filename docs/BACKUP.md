# Chromatic Backup Guide

This guide covers backup and restore procedures for Chromatic installations.

## What to Backup

Chromatic stores data in three locations:

| Data Type | Location | Priority |
|-----------|----------|----------|
| SQLite Database | `./data/chromatic.db` | **Critical** |
| Uploaded Files | `./data/files/` | High |
| Watermark Logos | `./data/logos/` | Medium |
| SSL Certificates | `./data/caddy/` | Low (auto-regenerated) |

## Quick Backup

### Manual Backup

```bash
cd deployments

# Stop the service for consistency (recommended for database)
docker-compose stop chromatic

# Create timestamped backup
tar -czvf chromatic-backup-$(date +%Y%m%d-%H%M%S).tar.gz data/

# Restart service
docker-compose start chromatic
```

### Hot Backup (No Downtime)

SQLite with WAL mode supports hot backups:

```bash
cd deployments

# Backup database while running (SQLite will handle consistency)
sqlite3 data/chromatic.db ".backup 'backup/chromatic-$(date +%Y%m%d).db'"

# Backup files (may miss files being written)
rsync -av data/files/ backup/files/
rsync -av data/logos/ backup/logos/
```

## Database Backup

### SQLite Backup Methods

**Method 1: .backup command (recommended)**
```bash
sqlite3 data/chromatic.db ".backup 'chromatic-backup.db'"
```

**Method 2: File copy (requires stopping service)**
```bash
docker-compose stop chromatic
cp data/chromatic.db backup/chromatic-$(date +%Y%m%d).db
cp data/chromatic.db-wal backup/chromatic-$(date +%Y%m%d).db-wal 2>/dev/null
cp data/chromatic.db-shm backup/chromatic-$(date +%Y%m%d).db-shm 2>/dev/null
docker-compose start chromatic
```

**Method 3: SQL dump (portable)**
```bash
sqlite3 data/chromatic.db ".dump" > backup/chromatic-$(date +%Y%m%d).sql
```

### Verifying Database Backup

```bash
# Check integrity
sqlite3 backup/chromatic-backup.db "PRAGMA integrity_check;"

# Expected output: ok

# Quick content check
sqlite3 backup/chromatic-backup.db "SELECT COUNT(*) FROM rooms;"
```

## Automated Backups

### Using Cron

Create `/etc/cron.d/chromatic-backup`:

```cron
# Daily database backup at 2 AM
0 2 * * * root /opt/chromatic/scripts/backup.sh

# Weekly full backup on Sunday at 3 AM
0 3 * * 0 root /opt/chromatic/scripts/full-backup.sh
```

### Backup Script

Create `scripts/backup.sh`:

```bash
#!/bin/bash
set -e

# Configuration
CHROMATIC_DIR="/opt/chromatic/deployments"
BACKUP_DIR="/backups/chromatic"
RETENTION_DAYS=30

# Create backup directory
mkdir -p "$BACKUP_DIR"

# Timestamp
TIMESTAMP=$(date +%Y%m%d-%H%M%S)

# Database backup (hot)
sqlite3 "$CHROMATIC_DIR/data/chromatic.db" ".backup '$BACKUP_DIR/chromatic-$TIMESTAMP.db'"

# Compress
gzip "$BACKUP_DIR/chromatic-$TIMESTAMP.db"

# Clean old backups
find "$BACKUP_DIR" -name "chromatic-*.db.gz" -mtime +$RETENTION_DAYS -delete

# Log
echo "$(date): Backup completed: chromatic-$TIMESTAMP.db.gz" >> "$BACKUP_DIR/backup.log"
```

Make executable:
```bash
chmod +x scripts/backup.sh
```

### Full Backup Script

Create `scripts/full-backup.sh`:

```bash
#!/bin/bash
set -e

# Configuration
CHROMATIC_DIR="/opt/chromatic/deployments"
BACKUP_DIR="/backups/chromatic"
RETENTION_WEEKS=8

# Create backup directory
mkdir -p "$BACKUP_DIR"

# Timestamp
TIMESTAMP=$(date +%Y%m%d)

# Stop service for consistent backup
cd "$CHROMATIC_DIR"
docker-compose stop chromatic

# Full backup
tar -czvf "$BACKUP_DIR/chromatic-full-$TIMESTAMP.tar.gz" data/

# Restart service
docker-compose start chromatic

# Clean old backups
find "$BACKUP_DIR" -name "chromatic-full-*.tar.gz" -mtime +$((RETENTION_WEEKS * 7)) -delete

# Log
echo "$(date): Full backup completed: chromatic-full-$TIMESTAMP.tar.gz" >> "$BACKUP_DIR/backup.log"
```

## Cloud Backup

### AWS S3

```bash
#!/bin/bash
# Requires: aws-cli configured

BACKUP_FILE="chromatic-$(date +%Y%m%d).db"
S3_BUCKET="your-backup-bucket"

# Create backup
sqlite3 data/chromatic.db ".backup '$BACKUP_FILE'"
gzip "$BACKUP_FILE"

# Upload to S3
aws s3 cp "${BACKUP_FILE}.gz" "s3://${S3_BUCKET}/chromatic/"

# Clean local
rm "${BACKUP_FILE}.gz"
```

### Google Cloud Storage

```bash
#!/bin/bash
# Requires: gcloud configured

BACKUP_FILE="chromatic-$(date +%Y%m%d).db"
GCS_BUCKET="your-backup-bucket"

sqlite3 data/chromatic.db ".backup '$BACKUP_FILE'"
gzip "$BACKUP_FILE"
gsutil cp "${BACKUP_FILE}.gz" "gs://${GCS_BUCKET}/chromatic/"
rm "${BACKUP_FILE}.gz"
```

### Backblaze B2

```bash
#!/bin/bash
# Requires: b2 CLI configured

BACKUP_FILE="chromatic-$(date +%Y%m%d).db"
B2_BUCKET="your-backup-bucket"

sqlite3 data/chromatic.db ".backup '$BACKUP_FILE'"
gzip "$BACKUP_FILE"
b2 upload-file "$B2_BUCKET" "${BACKUP_FILE}.gz" "chromatic/${BACKUP_FILE}.gz"
rm "${BACKUP_FILE}.gz"
```

## Restore Procedures

### Full Restore

```bash
cd deployments

# Stop service
docker-compose down

# Clear existing data (be careful!)
rm -rf data/*

# Extract backup
tar -xzvf chromatic-full-YYYYMMDD.tar.gz

# Start service
docker-compose up -d
```

### Database Only Restore

```bash
cd deployments

# Stop service
docker-compose stop chromatic

# Backup current (just in case)
mv data/chromatic.db data/chromatic.db.old
rm -f data/chromatic.db-wal data/chromatic.db-shm

# Restore from backup
cp /backups/chromatic/chromatic-YYYYMMDD.db data/chromatic.db

# Or from gzip:
gunzip -c /backups/chromatic/chromatic-YYYYMMDD.db.gz > data/chromatic.db

# Start service
docker-compose start chromatic

# Verify
docker-compose logs -f chromatic
```

### Restore from SQL Dump

```bash
cd deployments
docker-compose stop chromatic

# Remove old database
rm -f data/chromatic.db*

# Restore from SQL
sqlite3 data/chromatic.db < /backups/chromatic/chromatic-YYYYMMDD.sql

docker-compose start chromatic
```

### Selective File Restore

```bash
# Restore specific room's files
tar -xzvf chromatic-full-YYYYMMDD.tar.gz data/files/room-id/
```

## Backup Verification

### Automated Verification Script

```bash
#!/bin/bash
# verify-backup.sh

BACKUP_FILE=$1

if [ -z "$BACKUP_FILE" ]; then
    echo "Usage: verify-backup.sh <backup-file>"
    exit 1
fi

# Decompress if needed
if [[ "$BACKUP_FILE" == *.gz ]]; then
    gunzip -c "$BACKUP_FILE" > /tmp/verify-backup.db
    DB_FILE="/tmp/verify-backup.db"
else
    DB_FILE="$BACKUP_FILE"
fi

# Check integrity
INTEGRITY=$(sqlite3 "$DB_FILE" "PRAGMA integrity_check;")
if [ "$INTEGRITY" != "ok" ]; then
    echo "FAILED: Integrity check"
    exit 1
fi

# Check tables exist
TABLES=$(sqlite3 "$DB_FILE" ".tables")
REQUIRED_TABLES="rooms participants files sessions stream_keys"
for table in $REQUIRED_TABLES; do
    if [[ ! "$TABLES" == *"$table"* ]]; then
        echo "FAILED: Missing table $table"
        exit 1
    fi
done

# Check record counts
ROOMS=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM rooms;")
echo "Rooms: $ROOMS"

PARTICIPANTS=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM participants;")
echo "Participants: $PARTICIPANTS"

# Clean up
rm -f /tmp/verify-backup.db

echo "PASSED: Backup verification successful"
```

## Disaster Recovery

### Recovery Time Objectives

| Scenario | RTO | Procedure |
|----------|-----|-----------|
| Database corruption | 15 min | Restore from latest backup |
| Server failure | 30 min | Deploy new server, restore backup |
| Accidental deletion | 5 min | Restore specific files |

### Recovery Checklist

1. **Assess the situation**
   - What data is affected?
   - When did the issue start?
   - Is there a recent backup?

2. **Stop the service**
   ```bash
   docker-compose down
   ```

3. **Identify the correct backup**
   ```bash
   ls -la /backups/chromatic/
   ```

4. **Test backup before restoring** (on different machine if possible)
   ```bash
   ./scripts/verify-backup.sh /backups/chromatic/chromatic-YYYYMMDD.db.gz
   ```

5. **Perform restore**
   - Follow appropriate restore procedure above

6. **Verify restoration**
   ```bash
   docker-compose up -d
   curl https://stream.yourdomain.com/health
   ```

7. **Document the incident**
   - What happened
   - What was restored
   - Any data loss

## Backup Monitoring

### Health Check Script

```bash
#!/bin/bash
# backup-health.sh

BACKUP_DIR="/backups/chromatic"
MAX_AGE_HOURS=25  # Alert if no backup in 25 hours

# Check latest backup age
LATEST=$(ls -t "$BACKUP_DIR"/chromatic-*.db.gz 2>/dev/null | head -1)

if [ -z "$LATEST" ]; then
    echo "CRITICAL: No backups found!"
    exit 2
fi

# Get age in hours
AGE_SECONDS=$(($(date +%s) - $(stat -c %Y "$LATEST")))
AGE_HOURS=$((AGE_SECONDS / 3600))

if [ $AGE_HOURS -gt $MAX_AGE_HOURS ]; then
    echo "WARNING: Latest backup is $AGE_HOURS hours old"
    exit 1
fi

# Check backup size (should be > 0)
SIZE=$(stat -c %s "$LATEST")
if [ $SIZE -lt 1000 ]; then
    echo "WARNING: Backup file suspiciously small: $SIZE bytes"
    exit 1
fi

echo "OK: Latest backup is $AGE_HOURS hours old, size: $SIZE bytes"
exit 0
```

### Prometheus Metrics

Add to your monitoring:

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'chromatic-backup'
    static_configs:
      - targets: ['localhost:9100']
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: 'node_textfile_scrape_error'
        action: keep
```

Create metric file at `/var/lib/node_exporter/chromatic_backup.prom`:
```
# HELP chromatic_backup_age_seconds Age of latest backup in seconds
# TYPE chromatic_backup_age_seconds gauge
chromatic_backup_age_seconds 3600
```

## Best Practices

1. **Test restores regularly**: Monthly test restores to verify backup integrity
2. **Multiple backup locations**: Keep backups in at least 2 physical locations
3. **Encrypt sensitive backups**: Database may contain session tokens
4. **Monitor backup jobs**: Alert on failure
5. **Document procedures**: Keep this guide updated
6. **Retention policy**: Balance storage costs vs recovery needs
7. **Version backups**: Don't overwrite; keep historical versions
