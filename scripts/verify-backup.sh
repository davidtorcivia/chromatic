#!/bin/bash
#
# Chromatic Backup Verification Script
# Verifies backup artifacts without modifying running services.
#
# Usage: ./scripts/verify-backup.sh <backup_timestamp> [backup_dir]

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
MANIFEST="$BACKUP_DIR_ABS/manifest-$TIMESTAMP.txt"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_pass() { echo -e "${GREEN}[PASS]${NC} $1"; }
log_fail() { echo -e "${RED}[FAIL]${NC} $1"; }

require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        log_error "Required command not found: $1"
        exit 1
    fi
}

validate_timestamp() {
    case "$1" in
        [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]_[0-9][0-9][0-9][0-9][0-9][0-9]) ;;
        *)
            log_error "Invalid backup timestamp format: $1"
            exit 1
            ;;
    esac
}

validate_archive_paths() {
    archive="$1"
    label="$2"

    if ! tar -tzf "$archive" >/dev/null 2>&1; then
        log_fail "$label archive is corrupted: $(basename "$archive")"
        ERRORS=$((ERRORS + 1))
        return
    fi
    if tar -tzf "$archive" | grep -E '(^/|(^|/)\.\.(/|$))' >/dev/null; then
        log_fail "$label archive contains unsafe absolute or parent-traversal paths: $(basename "$archive")"
        ERRORS=$((ERRORS + 1))
        return
    fi
    log_pass "$label archive valid: $(basename "$archive")"
}

if [ ! -f "$COMPOSE_FILE" ]; then
    log_error "Compose file not found: $COMPOSE_FILE"
    exit 1
fi

require_cmd docker
require_cmd tar
validate_timestamp "$TIMESTAMP"

if [ ! -f "$DB_BACKUP" ]; then
    log_error "Database backup not found: $DB_BACKUP"
    exit 1
fi

COMPOSE_CMD=(docker compose -f "$COMPOSE_FILE")
ERRORS=0

log_info "Verifying backup timestamp: $TIMESTAMP"
log_info "Backup directory: $BACKUP_DIR_ABS"
echo ""

echo "Checking manifest..."
if [ -f "$MANIFEST" ]; then
    log_pass "Manifest found"
else
    log_warn "Manifest not found"
fi

echo ""
echo "Checking database backup..."
DB_SIZE="$(du -h "$DB_BACKUP" | cut -f1)"
log_pass "Database artifact exists: $(basename "$DB_BACKUP") ($DB_SIZE)"

INTEGRITY="$("${COMPOSE_CMD[@]}" run --rm --no-deps -v "$BACKUP_DIR_ABS:/backup:ro" chromatic sh -ec "sqlite3 '/backup/chromatic-$TIMESTAMP.db' 'PRAGMA integrity_check;'" 2>&1 || true)"
if [ "$INTEGRITY" = "ok" ]; then
    log_pass "SQLite integrity check passed"
else
    log_fail "SQLite integrity check failed: $INTEGRITY"
    ERRORS=$((ERRORS + 1))
fi

TABLES="$("${COMPOSE_CMD[@]}" run --rm --no-deps -v "$BACKUP_DIR_ABS:/backup:ro" chromatic sh -ec "sqlite3 '/backup/chromatic-$TIMESTAMP.db' '.tables'" 2>&1 || true)"
for table in stream_keys rooms participants messages files config sessions admin_audit_logs; do
    if echo "$TABLES" | grep -q "\b$table\b"; then
        log_pass "Table exists: $table"
    else
        log_fail "Table missing: $table"
        ERRORS=$((ERRORS + 1))
    fi
done

echo ""
echo "Checking media archives..."
if [ -f "$FILES_BACKUP" ]; then
    validate_archive_paths "$FILES_BACKUP" "Files"
else
    log_warn "Files archive not found for timestamp (legacy backups only; current backups always include it)"
fi

if [ -f "$LOGOS_BACKUP" ]; then
    validate_archive_paths "$LOGOS_BACKUP" "Logos"
else
    log_warn "Logos archive not found for timestamp (legacy backups only; current backups always include it)"
fi

echo ""
echo "========================================"
if [ "$ERRORS" -eq 0 ]; then
    log_info "Backup verification passed"
    exit 0
fi

log_error "Backup verification failed with $ERRORS error(s)"
exit 1
