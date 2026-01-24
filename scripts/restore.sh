#!/bin/bash
#
# Chromatic Restore Script
# Restores a backup of the database and uploaded files
#
# Usage: ./scripts/restore.sh <backup_timestamp> [backup_dir]
#
# Example: ./scripts/restore.sh 20250115_143000 ./backups

set -e

# Check arguments
if [ -z "$1" ]; then
    echo "Usage: $0 <backup_timestamp> [backup_dir]"
    echo "Example: $0 20250115_143000 ./backups"
    echo ""
    echo "Available backups:"
    ls -la "${2:-./backups}"/chromatic-*.db 2>/dev/null | sed 's/.*chromatic-/  /' | sed 's/.db//' || echo "  No backups found"
    exit 1
fi

TIMESTAMP="$1"
BACKUP_DIR="${2:-./backups}"
DATA_DIR="${DATA_DIR:-./data}"
DATABASE_PATH="${DATABASE_PATH:-$DATA_DIR/chromatic.db}"
UPLOAD_PATH="${UPLOAD_PATH:-$DATA_DIR/files}"
LOGO_PATH="${LOGO_PATH:-$DATA_DIR/logos}"

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

# Verify backup files exist
DB_BACKUP="$BACKUP_DIR/chromatic-$TIMESTAMP.db"
FILES_BACKUP="$BACKUP_DIR/files-$TIMESTAMP.tar.gz"
LOGOS_BACKUP="$BACKUP_DIR/logos-$TIMESTAMP.tar.gz"

if [ ! -f "$DB_BACKUP" ]; then
    log_error "Database backup not found: $DB_BACKUP"
    exit 1
fi

log_info "Starting Chromatic restore..."
log_info "Timestamp: $TIMESTAMP"
log_info "Backup directory: $BACKUP_DIR"

# Confirmation prompt
echo ""
log_warn "WARNING: This will overwrite the current database and files!"
log_warn "Make sure Chromatic is stopped before proceeding."
echo ""
read -p "Are you sure you want to continue? (yes/no): " CONFIRM
if [ "$CONFIRM" != "yes" ]; then
    log_info "Restore cancelled"
    exit 0
fi

# Create data directory if it doesn't exist
mkdir -p "$DATA_DIR"

# Backup current state before restore (just in case)
if [ -f "$DATABASE_PATH" ]; then
    CURRENT_BACKUP="$DATABASE_PATH.pre-restore-$(date +%Y%m%d_%H%M%S)"
    log_info "Backing up current database to $CURRENT_BACKUP"
    cp "$DATABASE_PATH" "$CURRENT_BACKUP"
fi

# Restore database
log_info "Restoring database from $DB_BACKUP..."
cp "$DB_BACKUP" "$DATABASE_PATH"

if [ $? -eq 0 ]; then
    log_info "Database restored successfully"
else
    log_error "Database restore failed"
    exit 1
fi

# Verify database integrity
log_info "Verifying database integrity..."
sqlite3 "$DATABASE_PATH" "PRAGMA integrity_check;" | grep -q "ok"
if [ $? -eq 0 ]; then
    log_info "Database integrity check passed"
else
    log_error "Database integrity check failed!"
    log_warn "Restoring previous database..."
    [ -f "$CURRENT_BACKUP" ] && cp "$CURRENT_BACKUP" "$DATABASE_PATH"
    exit 1
fi

# Restore uploaded files if backup exists
if [ -f "$FILES_BACKUP" ]; then
    log_info "Restoring uploaded files from $FILES_BACKUP..."
    mkdir -p "$UPLOAD_PATH"
    tar -xzf "$FILES_BACKUP" -C "$(dirname $UPLOAD_PATH)"
    log_info "Files restored successfully"
else
    log_warn "No files backup found, skipping"
fi

# Restore logos if backup exists
if [ -f "$LOGOS_BACKUP" ]; then
    log_info "Restoring logos from $LOGOS_BACKUP..."
    mkdir -p "$LOGO_PATH"
    tar -xzf "$LOGOS_BACKUP" -C "$(dirname $LOGO_PATH)"
    log_info "Logos restored successfully"
else
    log_warn "No logos backup found, skipping"
fi

log_info ""
log_info "Restore completed successfully!"
log_info ""
log_info "Next steps:"
log_info "1. Start Chromatic: docker-compose up -d"
log_info "2. Verify the application is working correctly"
log_info "3. Remove pre-restore backup if everything is OK:"
[ -f "$CURRENT_BACKUP" ] && log_info "   rm $CURRENT_BACKUP"
