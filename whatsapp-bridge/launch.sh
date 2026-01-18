#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STORE_DIR="$SCRIPT_DIR/store"
CERT_PATH="$STORE_DIR/server.crt"
KEY_PATH="$STORE_DIR/server.key"

mkdir -p "$STORE_DIR"

if [[ ! -f "$CERT_PATH" || ! -f "$KEY_PATH" ]]; then
  openssl req -x509 -newkey rsa:4096 \
    -keyout "$KEY_PATH" \
    -out "$CERT_PATH" \
    -days 365 \
    -nodes \
    -subj "/CN=localhost"
fi

exec go run "$SCRIPT_DIR/main.go"
