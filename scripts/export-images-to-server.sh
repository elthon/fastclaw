#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

REMOTE_HOST=${REMOTE_HOST:-lhdata}
REMOTE_DIR=${REMOTE_DIR:-/opt/data/aegis/fastclaw}
OUT_DIR=${OUT_DIR:-"$REPO_ROOT/dist/docker-images"}
BUNDLE_PREFIX=${BUNDLE_PREFIX:-fastclaw-images}

SANDBOX_IMAGE=${SANDBOX_IMAGE:-thinkany/fastclaw-sandbox:latest}
BACKEND_IMAGE=${BACKEND_IMAGE:-fastclaw/fastclaw:latest}
FRONTEND_IMAGE=${FRONTEND_IMAGE:-}

BUILD=0

usage() {
  cat <<'EOF'
Usage:
  scripts/export-images-to-server.sh [--build]

Exports Docker images into one tar.gz bundle and uploads it with scp.
The sandbox is not a long-running compose service; fastclaw starts it
automatically when agent tools need it. Exporting SANDBOX_IMAGE only makes
that runtime image available on the server for docker-load deployments.

Defaults:
  REMOTE_HOST=lhdata
  REMOTE_DIR=/opt/data/aegis/fastclaw
  SANDBOX_IMAGE=thinkany/fastclaw-sandbox:latest
  BACKEND_IMAGE=fastclaw/fastclaw:latest
  FRONTEND_IMAGE=<empty>

Examples:
  scripts/export-images-to-server.sh
  scripts/export-images-to-server.sh --build
  FRONTEND_IMAGE=docker-frontend:latest scripts/export-images-to-server.sh
  BACKEND_IMAGE=docker-backend:latest FRONTEND_IMAGE=docker-frontend:latest scripts/export-images-to-server.sh
  IMAGES="thinkany/fastclaw-sandbox:latest docker-backend:latest docker-frontend:latest" scripts/export-images-to-server.sh

On the server:
  cd /opt/data/aegis/fastclaw
  sha256sum -c fastclaw-images-*.tar.gz.sha256
  docker load -i fastclaw-images-*.tar.gz
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --build)
      BUILD=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown arg: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -n "${IMAGES:-}" ]]; then
  read -r -a images <<< "$IMAGES"
else
  images=("$SANDBOX_IMAGE" "$BACKEND_IMAGE")
  if [[ -n "$FRONTEND_IMAGE" ]]; then
    images+=("$FRONTEND_IMAGE")
  fi
fi

if [[ ${#images[@]} -eq 0 ]]; then
  echo "no images configured" >&2
  exit 2
fi

mkdir -p "$OUT_DIR"

if [[ $BUILD -eq 1 ]]; then
  echo "==> building backend image: $BACKEND_IMAGE"
  docker build -t "$BACKEND_IMAGE" "$REPO_ROOT"

  echo "==> building sandbox image: $SANDBOX_IMAGE"
  docker build -t "$SANDBOX_IMAGE" "$REPO_ROOT/deploy/docker/sandbox"
fi

echo "==> checking local images"
missing=()
for image in "${images[@]}"; do
  if docker image inspect "$image" >/dev/null 2>&1; then
    echo "    ok: $image"
  else
    missing+=("$image")
  fi
done

if [[ ${#missing[@]} -gt 0 ]]; then
  echo "missing local docker images:" >&2
  printf '  %s\n' "${missing[@]}" >&2
  echo "build them first, pass --build, or override BACKEND_IMAGE/FRONTEND_IMAGE/SANDBOX_IMAGE/IMAGES" >&2
  exit 1
fi

timestamp=$(date +%Y%m%d-%H%M%S)
bundle="$OUT_DIR/$BUNDLE_PREFIX-$timestamp.tar.gz"
checksum="$bundle.sha256"

echo "==> saving images to $bundle"
printf '    %s\n' "${images[@]}"
docker save "${images[@]}" | gzip -1 > "$bundle"

echo "==> writing checksum"
(cd "$OUT_DIR" && sha256sum "$(basename "$bundle")" > "$(basename "$checksum")")

remote_dir_quoted=$(printf '%q' "$REMOTE_DIR")
echo "==> ensuring remote directory: $REMOTE_HOST:$REMOTE_DIR"
ssh "$REMOTE_HOST" "mkdir -p $remote_dir_quoted"

echo "==> uploading bundle"
scp "$bundle" "$checksum" "$REMOTE_HOST:$REMOTE_DIR/"

echo "==> done"
echo "Uploaded:"
echo "  $REMOTE_HOST:$REMOTE_DIR/$(basename "$bundle")"
echo "  $REMOTE_HOST:$REMOTE_DIR/$(basename "$checksum")"
echo
echo "Load on the server with:"
echo "  cd $REMOTE_DIR"
echo "  sha256sum -c $(basename "$checksum")"
echo "  docker load -i $(basename "$bundle")"
