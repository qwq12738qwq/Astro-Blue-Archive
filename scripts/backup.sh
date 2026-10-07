#!/usr/bin/env bash
#
# Back up the blog: content + media + database.
#
# ARCHITECTURE.md §28: a personal blog's real data is all three. Backing up only
# SQLite, or only content, is not a backup.
#
# The database is captured with SQLite's online backup primitive rather than a
# file copy, so the snapshot is consistent even while the blog is running.
#
# Usage:
#   scripts/backup.sh                       # use the configured paths
#   scripts/backup.sh /mnt/backups          # explicit output directory
#
# Environment overrides:
#   DATA_ROOT, CONTENT_ROOT, MEDIA_ROOT, BACKUP_DIR, BACKUP_KEEP

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

DATA_ROOT="${DATA_ROOT:-$REPO_ROOT/data}"
CONTENT_ROOT="${CONTENT_ROOT:-$REPO_ROOT/content}"
MEDIA_ROOT="${MEDIA_ROOT:-$REPO_ROOT/media}"
BACKUP_DIR="${1:-${BACKUP_DIR:-$DATA_ROOT/backups}}"
BACKUP_KEEP="${BACKUP_KEEP:-14}"

BINARY="$REPO_ROOT/backend/bin/blogcms-backup"

if [[ ! -x "$BINARY" ]]; then
  echo "Building the backup binary..." >&2
  (cd "$REPO_ROOT/backend" && go build -o "$BINARY" ./cmd/backup)
fi

for dir in "$DATA_ROOT" "$CONTENT_ROOT" "$MEDIA_ROOT"; do
  if [[ ! -d "$dir" ]]; then
    echo "error: $dir does not exist" >&2
    exit 1
  fi
done

mkdir -p "$BACKUP_DIR"

echo "content: $CONTENT_ROOT"
echo "media:   $MEDIA_ROOT"
echo "data:    $DATA_ROOT/blog.db"
echo "output:  $BACKUP_DIR"

"$BINARY" \
  -data "$DATA_ROOT" \
  -content "$CONTENT_ROOT" \
  -media "$MEDIA_ROOT" \
  -out "$BACKUP_DIR" \
  -keep "$BACKUP_KEEP"