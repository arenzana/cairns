#!/usr/bin/env bash
# Dump the cairns database to ./backups on the host.
#
# This is the backup, not the data directory. Copying a live Postgres data
# directory gives you a torn snapshot that will not restore, and on macOS the
# data directory cannot be bind-mounted at all (VirtioFS ownership mapping).
# A dump is restorable anywhere, including onto a different Postgres major.
#
# Restore:  gunzip -c backups/cairns-<stamp>.sql.gz | docker exec -i cairns-postgres psql -U cairns -d cairns
set -euo pipefail

cd "$(dirname "$0")/.."
STAMP=$(date +%Y%m%d-%H%M%S)
OUT="backups/cairns-${STAMP}.sql.gz"

docker exec cairns-postgres pg_dump -U cairns -d cairns --clean --if-exists \
  | gzip > "$OUT"

# Validate rather than trust the exit code. A dump truncated by a dying
# container still exits 0 and still produces a plausible-looking file; gzip -t
# catches the truncation and the table count catches a schema-only dump.
gzip -t "$OUT"
TABLES=$(gzcat "$OUT" | grep -c '^CREATE TABLE' || true)
if [ "$TABLES" -lt 3 ]; then
  echo "FAIL: only $TABLES CREATE TABLE statements in $OUT, expected at least 3" >&2
  exit 1
fi

# Keep 14, drop the rest.
ls -1t backups/cairns-*.sql.gz 2>/dev/null | tail -n +15 | xargs -r rm --

echo "ok: $OUT  ($(du -h "$OUT" | cut -f1), ${TABLES} tables)"
