#!/bin/bash
#
# Chromatic Restore Script
# Restores a backup into the Docker volume used by Compose.
#
# Usage: ./scripts/restore.sh <backup_timestamp> [backup_dir]
# Example: ./scripts/restore.sh 20250115_143000 ./backups

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$REPO_ROOT/deployments/docker-compose.yml}"

if [ -z "${1:-}" ]; then
    echo "Usage: $0 <backup_timestamp> [backup_dir]"
    echo "Example: $0 20250115_143000 ./backups"
    echo ""
    echo "Available backups:"
    ls -la "${2:-$REPO_ROOT/backups}"/chromatic-*.db 2>/dev/null | sed 's/.*chromatic-/  /' | sed 's/.db//' || echo "  No backups found"
    exit 1
fi

TIMESTAMP="$1"
BACKUP_DIR="${2:-$REPO_ROOT/backups}"
BACKUP_DIR_ABS="$(cd "$BACKUP_DIR" && pwd)"
DB_BACKUP="$BACKUP_DIR_ABS/chromatic-$TIMESTAMP.db"
FILES_BACKUP="$BACKUP_DIR_ABS/files-$TIMESTAMP.tar.gz"
LOGOS_BACKUP="$BACKUP_DIR_ABS/logos-$TIMESTAMP.tar.gz"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

if [ ! -f "$COMPOSE_FILE" ]; then
    log_error "Compose file not found: $COMPOSE_FILE"
    exit 1
fi

if [ ! -f "$DB_BACKUP" ]; then
    log_error "Database backup not found: $DB_BACKUP"
    exit 1
fi

COMPOSE_CMD=(docker compose -f "$COMPOSE_FILE")

log_info "Starting Chromatic restore"
log_info "Backup timestamp: $TIMESTAMP"
log_info "Backup directory: $BACKUP_DIR_ABS"

echo ""
log_warn "WARNING: This will overwrite the current database and media data."
log_warn "A pre-restore database snapshot will be created in $BACKUP_DIR_ABS."
read -r -p "Type 'yes' to continue: " CONFIRM
if [ "$CONFIRM" != "yes" ]; then
    log_info "Restore cancelled"
    exit 0
fi

PRE_RESTORE_TS="$(date +%Y%m%d_%H%M%S)"
"${COMPOSE_CMD[@]}" run --rm --no-deps \
    -v "$BACKUP_DIR_ABS:/backup" \
    chromatic sh -ec "if [ -f /data/chromatic.db ]; then sqlite3 /data/chromatic.db \".backup '/backup/chromatic-pre-restore-$PRE_RESTORE_TS.db'\"; fi"

log_info "Pre-restore snapshot created: chromatic-pre-restore-$PRE_RESTORE_TS.db"

log_info "Stopping chromatic service"
"${COMPOSE_CMD[@]}" stop chromatic >/dev/null

log_info "Restoring backup files into Docker volume"
"${COMPOSE_CMD[@]}" run --rm --no-deps \
    -v "$BACKUP_DIR_ABS:/backup:ro" \
    chromatic sh -ec "
cp '/backup/chromatic-$TIMESTAMP.db' /data/chromatic.db
rm -f /data/chromatic.db-wal /data/chromatic.db-shm
if [ -f '/backup/files-$TIMESTAMP.tar.gz' ]; then
  rm -rf /data/files
  mkdir -p /data
  tar -xzf '/backup/files-$TIMESTAMP.tar.gz' -C /data
fi
if [ -f '/backup/logos-$TIMESTAMP.tar.gz' ]; then
  rm -rf /data/logos
  mkdir -p /data
  tar -xzf '/backup/logos-$TIMESTAMP.tar.gz' -C /data
fi
"

log_info "Starting chromatic service"
"${COMPOSE_CMD[@]}" up -d chromatic >/dev/null

log_info "Restore completed successfully"
if [ -f "$FILES_BACKUP" ]; then
    log_info "Restored media archive: $(basename "$FILES_BACKUP")"
else
    log_warn "No files archive found for this timestamp; existing /data/files was left unchanged"
fi
if [ -f "$LOGOS_BACKUP" ]; then
    log_info "Restored logos archive: $(basename "$LOGOS_BACKUP")"
else
    log_warn "No logos archive found for this timestamp; existing /data/logos was left unchanged"
fi

log_info "Next steps:"
log_info "1. Verify app health: docker compose -f deployments/docker-compose.yml ps"
log_info "2. Tail logs: docker compose -f deployments/docker-compose.yml logs -f chromatic"
