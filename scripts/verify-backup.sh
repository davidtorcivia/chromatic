#!/bin/bash
#
# Chromatic Backup Verification Script
# Verifies the integrity of a backup without restoring it
#
# Usage: ./scripts/verify-backup.sh <backup_timestamp> [backup_dir]

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

log_pass() {
    echo -e "${GREEN}[PASS]${NC} $1"
}

log_fail() {
    echo -e "${RED}[FAIL]${NC} $1"
}

# Define backup files
DB_BACKUP="$BACKUP_DIR/chromatic-$TIMESTAMP.db"
FILES_BACKUP="$BACKUP_DIR/files-$TIMESTAMP.tar.gz"
LOGOS_BACKUP="$BACKUP_DIR/logos-$TIMESTAMP.tar.gz"
MANIFEST="$BACKUP_DIR/manifest-$TIMESTAMP.txt"

ERRORS=0

log_info "Verifying Chromatic backup: $TIMESTAMP"
log_info "Backup directory: $BACKUP_DIR"
echo ""

# Check manifest
echo "Checking manifest..."
if [ -f "$MANIFEST" ]; then
    log_pass "Manifest found"
    echo "  Contents:"
    cat "$MANIFEST" | sed 's/^/    /'
    echo ""
else
    log_warn "Manifest not found (optional)"
fi

# Verify database backup
echo "Checking database backup..."
if [ -f "$DB_BACKUP" ]; then
    log_pass "Database backup exists: $(du -h $DB_BACKUP | cut -f1)"

    # Verify SQLite integrity
    echo "  Running SQLite integrity check..."
    INTEGRITY=$(sqlite3 "$DB_BACKUP" "PRAGMA integrity_check;" 2>&1)
    if [ "$INTEGRITY" == "ok" ]; then
        log_pass "Database integrity check passed"
    else
        log_fail "Database integrity check failed: $INTEGRITY"
        ERRORS=$((ERRORS + 1))
    fi

    # Check schema
    echo "  Verifying schema..."
    TABLES=$(sqlite3 "$DB_BACKUP" ".tables" 2>&1)
    if echo "$TABLES" | grep -q "rooms"; then
        log_pass "Schema verified (rooms table exists)"
    else
        log_fail "Schema verification failed (rooms table missing)"
        ERRORS=$((ERRORS + 1))
    fi

    # Show table counts
    echo "  Table record counts:"
    for table in stream_keys rooms participants messages files config sessions; do
        COUNT=$(sqlite3 "$DB_BACKUP" "SELECT COUNT(*) FROM $table;" 2>/dev/null || echo "N/A")
        echo "    $table: $COUNT"
    done
else
    log_fail "Database backup not found: $DB_BACKUP"
    ERRORS=$((ERRORS + 1))
fi

echo ""

# Verify files backup
echo "Checking files backup..."
if [ -f "$FILES_BACKUP" ]; then
    log_pass "Files backup exists: $(du -h $FILES_BACKUP | cut -f1)"

    # Verify tar integrity
    echo "  Verifying archive integrity..."
    if tar -tzf "$FILES_BACKUP" > /dev/null 2>&1; then
        FILE_COUNT=$(tar -tzf "$FILES_BACKUP" | wc -l)
        log_pass "Archive integrity check passed ($FILE_COUNT files)"
    else
        log_fail "Archive integrity check failed"
        ERRORS=$((ERRORS + 1))
    fi
else
    log_warn "Files backup not found (may be expected if no files uploaded)"
fi

echo ""

# Verify logos backup
echo "Checking logos backup..."
if [ -f "$LOGOS_BACKUP" ]; then
    log_pass "Logos backup exists: $(du -h $LOGOS_BACKUP | cut -f1)"

    # Verify tar integrity
    echo "  Verifying archive integrity..."
    if tar -tzf "$LOGOS_BACKUP" > /dev/null 2>&1; then
        LOGO_COUNT=$(tar -tzf "$LOGOS_BACKUP" | wc -l)
        log_pass "Archive integrity check passed ($LOGO_COUNT files)"
    else
        log_fail "Archive integrity check failed"
        ERRORS=$((ERRORS + 1))
    fi
else
    log_warn "Logos backup not found (may be expected if no logo uploaded)"
fi

echo ""
echo "========================================"
if [ $ERRORS -eq 0 ]; then
    log_info "Backup verification completed successfully!"
    exit 0
else
    log_error "Backup verification found $ERRORS error(s)"
    exit 1
fi
