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

if [ ! -f "$COMPOSE_FILE" ]; then
    log_error "Compose file not found: $COMPOSE_FILE"
    exit 1
fi

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
for table in stream_keys rooms participants messages files config sessions; do
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
    if tar -tzf "$FILES_BACKUP" >/dev/null 2>&1; then
        log_pass "Files archive valid: $(basename "$FILES_BACKUP")"
    else
        log_fail "Files archive is corrupted: $(basename "$FILES_BACKUP")"
        ERRORS=$((ERRORS + 1))
    fi
else
    log_warn "Files archive not found for timestamp (may be expected)"
fi

if [ -f "$LOGOS_BACKUP" ]; then
    if tar -tzf "$LOGOS_BACKUP" >/dev/null 2>&1; then
        log_pass "Logos archive valid: $(basename "$LOGOS_BACKUP")"
    else
        log_fail "Logos archive is corrupted: $(basename "$LOGOS_BACKUP")"
        ERRORS=$((ERRORS + 1))
    fi
else
    log_warn "Logos archive not found for timestamp (may be expected)"
fi

echo ""
echo "========================================"
if [ "$ERRORS" -eq 0 ]; then
    log_info "Backup verification passed"
    exit 0
fi

log_error "Backup verification failed with $ERRORS error(s)"
exit 1
