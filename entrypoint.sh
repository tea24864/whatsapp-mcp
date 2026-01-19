#!/usr/bin/env sh
set -eu

STORE_DIR="/app/store"

mkdir -p "$STORE_DIR" "$STORE_DIR/tmp-media"

chown -R 10001:0 "$STORE_DIR" 2>/dev/null || true

exec /usr/sbin/runuser -u app -- /app/whatsapp-mcp
