#!/bin/bash
#
# Chromatic Backup Script
# Creates timestamped backups from the Docker volume used by Compose.
#
# Usage: ./scripts/backup.sh [backup_dir]
#
# Defaults:
#   backup_dir: ./backups
#   compose file: ./deployments/docker-compose.yml

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$REPO_ROOT/deployments/docker-compose.yml}"
BACKUP_DIR="${1:-$REPO_ROOT/backups}"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-7}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        log_error "Required command not found: $1"
        exit 1
    fi
}

require_cmd docker

if [ ! -f "$COMPOSE_FILE" ]; then
    log_error "Compose file not found: $COMPOSE_FILE"
    exit 1
fi

mkdir -p "$BACKUP_DIR"
BACKUP_DIR_ABS="$(cd "$BACKUP_DIR" && pwd)"

COMPOSE_CMD=(docker compose -f "$COMPOSE_FILE")

log_info "Starting Chromatic backup"
log_info "Timestamp: $TIMESTAMP"
log_info "Backup directory: $BACKUP_DIR_ABS"

# Database hot backup using sqlite CLI inside the Chromatic image.
"${COMPOSE_CMD[@]}" run --rm --no-deps \
    -v "$BACKUP_DIR_ABS:/backup" \
    chromatic sh -ec "sqlite3 /data/chromatic.db \".backup '/backup/chromatic-$TIMESTAMP.db'\""

log_info "Database backup created: chromatic-$TIMESTAMP.db"

# Backup uploaded files. Always emit an archive, even when empty, so restore can
# faithfully remove stale files that were created after this backup.
"${COMPOSE_CMD[@]}" run --rm --no-deps \
    -v "$BACKUP_DIR_ABS:/backup" \
    chromatic sh -ec "mkdir -p /data/files && tar -czf '/backup/files-$TIMESTAMP.tar.gz' -C /data files"

if [ -f "$BACKUP_DIR_ABS/files-$TIMESTAMP.tar.gz" ]; then
    log_info "Files backup created: files-$TIMESTAMP.tar.gz"
else
    log_error "Files backup was not created"
    exit 1
fi

# Backup logos. Always emit an archive, even when empty, for exact restores.
"${COMPOSE_CMD[@]}" run --rm --no-deps \
    -v "$BACKUP_DIR_ABS:/backup" \
    chromatic sh -ec "mkdir -p /data/logos && tar -czf '/backup/logos-$TIMESTAMP.tar.gz' -C /data logos"

if [ -f "$BACKUP_DIR_ABS/logos-$TIMESTAMP.tar.gz" ]; then
    log_info "Logos backup created: logos-$TIMESTAMP.tar.gz"
else
    log_error "Logos backup was not created"
    exit 1
fi

MANIFEST="$BACKUP_DIR_ABS/manifest-$TIMESTAMP.txt"
{
    echo "Chromatic Backup Manifest"
    echo "========================="
    echo "Timestamp: $TIMESTAMP"
    echo "Date: $(date -u +'%Y-%m-%dT%H:%M:%SZ')"
    echo "Compose file: $COMPOSE_FILE"
    echo ""
    echo "Artifacts:"
    ls -1 "$BACKUP_DIR_ABS"/*"$TIMESTAMP"* 2>/dev/null | xargs -n1 basename || true
} > "$MANIFEST"

log_info "Manifest created: $(basename "$MANIFEST")"

log_info "Cleaning up backups older than $RETENTION_DAYS day(s)"
find "$BACKUP_DIR_ABS" -name "chromatic-*.db" -mtime +"$RETENTION_DAYS" -delete 2>/dev/null || true
find "$BACKUP_DIR_ABS" -name "files-*.tar.gz" -mtime +"$RETENTION_DAYS" -delete 2>/dev/null || true
find "$BACKUP_DIR_ABS" -name "logos-*.tar.gz" -mtime +"$RETENTION_DAYS" -delete 2>/dev/null || true
find "$BACKUP_DIR_ABS" -name "manifest-*.txt" -mtime +"$RETENTION_DAYS" -delete 2>/dev/null || true

log_info "Backup completed successfully"
log_info "Generated files:"
ls -lh "$BACKUP_DIR_ABS"/*"$TIMESTAMP"* 2>/dev/null || true
