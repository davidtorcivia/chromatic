#!/bin/bash
#
# Chromatic Backup Script
# Creates a timestamped backup of the database and uploaded files
#
# Usage: ./scripts/backup.sh [backup_dir]
#
# Default backup directory: ./backups

set -e

# Configuration
BACKUP_DIR="${1:-./backups}"
DATA_DIR="${DATA_DIR:-./data}"
DATABASE_PATH="${DATABASE_PATH:-$DATA_DIR/chromatic.db}"
UPLOAD_PATH="${UPLOAD_PATH:-$DATA_DIR/files}"
LOGO_PATH="${LOGO_PATH:-$DATA_DIR/logos}"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Create backup directory if it doesn't exist
mkdir -p "$BACKUP_DIR"

log_info "Starting Chromatic backup..."
log_info "Timestamp: $TIMESTAMP"
log_info "Backup directory: $BACKUP_DIR"

# Check if database exists
if [ ! -f "$DATABASE_PATH" ]; then
    log_error "Database not found at $DATABASE_PATH"
    exit 1
fi

# Create database backup using SQLite's .backup command (hot backup)
DB_BACKUP="$BACKUP_DIR/chromatic-$TIMESTAMP.db"
log_info "Backing up database to $DB_BACKUP..."

# Use SQLite's online backup for consistency
sqlite3 "$DATABASE_PATH" ".backup '$DB_BACKUP'"

if [ $? -eq 0 ]; then
    log_info "Database backup completed successfully"
else
    log_error "Database backup failed"
    exit 1
fi

# Backup uploaded files if they exist
if [ -d "$UPLOAD_PATH" ] && [ "$(ls -A $UPLOAD_PATH 2>/dev/null)" ]; then
    FILES_BACKUP="$BACKUP_DIR/files-$TIMESTAMP.tar.gz"
    log_info "Backing up uploaded files to $FILES_BACKUP..."
    tar -czf "$FILES_BACKUP" -C "$(dirname $UPLOAD_PATH)" "$(basename $UPLOAD_PATH)"
    log_info "Files backup completed"
else
    log_warn "No uploaded files found to backup"
fi

# Backup logos if they exist
if [ -d "$LOGO_PATH" ] && [ "$(ls -A $LOGO_PATH 2>/dev/null)" ]; then
    LOGOS_BACKUP="$BACKUP_DIR/logos-$TIMESTAMP.tar.gz"
    log_info "Backing up logos to $LOGOS_BACKUP..."
    tar -czf "$LOGOS_BACKUP" -C "$(dirname $LOGO_PATH)" "$(basename $LOGO_PATH)"
    log_info "Logos backup completed"
else
    log_warn "No logos found to backup"
fi

# Create a manifest file
MANIFEST="$BACKUP_DIR/manifest-$TIMESTAMP.txt"
echo "Chromatic Backup Manifest" > "$MANIFEST"
echo "=========================" >> "$MANIFEST"
echo "Timestamp: $TIMESTAMP" >> "$MANIFEST"
echo "Date: $(date)" >> "$MANIFEST"
echo "" >> "$MANIFEST"
echo "Files:" >> "$MANIFEST"
echo "  Database: $(basename $DB_BACKUP)" >> "$MANIFEST"
[ -f "$FILES_BACKUP" ] && echo "  Files: $(basename $FILES_BACKUP)" >> "$MANIFEST"
[ -f "$LOGOS_BACKUP" ] && echo "  Logos: $(basename $LOGOS_BACKUP)" >> "$MANIFEST"
echo "" >> "$MANIFEST"
echo "Database size: $(du -h $DB_BACKUP | cut -f1)" >> "$MANIFEST"
[ -f "$FILES_BACKUP" ] && echo "Files archive size: $(du -h $FILES_BACKUP | cut -f1)" >> "$MANIFEST"
[ -f "$LOGOS_BACKUP" ] && echo "Logos archive size: $(du -h $LOGOS_BACKUP | cut -f1)" >> "$MANIFEST"

log_info "Manifest created: $MANIFEST"

# Cleanup old backups (keep last 7 days by default)
RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-7}"
log_info "Cleaning up backups older than $RETENTION_DAYS days..."
find "$BACKUP_DIR" -name "chromatic-*.db" -mtime +$RETENTION_DAYS -delete 2>/dev/null || true
find "$BACKUP_DIR" -name "files-*.tar.gz" -mtime +$RETENTION_DAYS -delete 2>/dev/null || true
find "$BACKUP_DIR" -name "logos-*.tar.gz" -mtime +$RETENTION_DAYS -delete 2>/dev/null || true
find "$BACKUP_DIR" -name "manifest-*.txt" -mtime +$RETENTION_DAYS -delete 2>/dev/null || true

log_info "Backup completed successfully!"
log_info ""
log_info "Backup files:"
ls -lh "$BACKUP_DIR"/*-$TIMESTAMP* 2>/dev/null || true
