#!/bin/sh
set -e

# `make dev` serves the web UI from `next dev` (hot reload) and sets
# FASTCLAW_DEV_SKIP_WEB so Go rebuilds don't re-export the frontend.
if [ -z "$FASTCLAW_DEV_SKIP_WEB" ]; then
  # Build web
  (cd web && pnpm build)

  # Copy web output to embed dir
  rm -rf internal/setup/web
  cp -r web/out internal/setup/web
fi

# Build Go binary. Stamp the same version/commit/date that the release
# Makefile sets — so dev binaries report a real `git describe` tag (e.g.
# v0.32.0-3-gabc1234-dirty) instead of the hard-coded "dev" default.
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO=github.com/fastclaw-ai/fastclaw/internal/buildinfo
LDFLAGS="-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE \
 -X $BUILDINFO.Version=$VERSION -X $BUILDINFO.Commit=$COMMIT -X $BUILDINFO.Date=$DATE"
go build -ldflags "$LDFLAGS" -o tmp/fastclaw ./cmd/fastclaw
