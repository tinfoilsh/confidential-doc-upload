#!/bin/sh
set -e

cleanup() {
    kill "$SIDECAR_PID" 2>/dev/null
    wait "$SIDECAR_PID" 2>/dev/null
}
trap cleanup EXIT INT TERM

uvicorn sidecar.app:app \
  --host 127.0.0.1 --port 5002 --workers 2 \
  --app-dir /app &
SIDECAR_PID=$!

/usr/local/bin/router &
ROUTER_PID=$!

wait -n "$SIDECAR_PID" "$ROUTER_PID"
EXIT_CODE=$?
cleanup
exit $EXIT_CODE
