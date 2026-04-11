#!/bin/sh
set -e

uvicorn sidecar.app:app \
  --host 0.0.0.0 --port 5002 --workers 2 \
  --app-dir /app &

exec /usr/local/bin/router
