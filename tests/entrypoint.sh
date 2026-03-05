#!/bin/sh
set -e

APPIUM_HOST="${APPIUM_HOST:-localhost}"
APPIUM_PORT="${APPIUM_PORT:-4723}"
MAX_WAIT="${APPIUM_WAIT_SECONDS:-60}"

echo "[entrypoint] Waiting for Appium at ${APPIUM_HOST}:${APPIUM_PORT} (timeout ${MAX_WAIT}s)..."

elapsed=0
until curl -sf "http://${APPIUM_HOST}:${APPIUM_PORT}/status" > /dev/null 2>&1; do
  if [ "$elapsed" -ge "$MAX_WAIT" ]; then
    echo "[entrypoint] Timed out waiting for Appium."
    exit 1
  fi
  sleep 2
  elapsed=$((elapsed + 2))
done

echo "[entrypoint] Appium is ready. Running tests..."
exec npx wdio run wdio.conf.js
