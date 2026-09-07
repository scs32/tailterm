#!/bin/sh
# Run the tailterm stack in apple/container on this Mac.
#
#   scripts/container-dev.sh up       build (if needed) and start hub + static site
#   scripts/container-dev.sh down     stop and remove both containers
#   scripts/container-dev.sh rebuild  rebuild both images, then restart
#   scripts/container-dev.sh logs     follow hub logs
#   scripts/container-dev.sh status   list the stack's containers
#
# TS_AUTHKEY (a tagged, reusable Tailscale auth key) makes the hub join the
# tailnet as TS_HOSTNAME (default tailterm-hub). Without it the hub runs in dev
# mode on http://127.0.0.1:8765 with a fixed identity, which is enough for
# local testing but not reachable by agent hosts.
set -eu
cd "$(dirname "$0")/.."
HUB_IMAGE=tailterm-hub:dev
WEB_IMAGE=tailterm-static:dev
HUB=tailterm-hub
WEB=tailterm-static
VOLUME=tailterm-hub-state
WEB_PORT=${TAILTERM_WEB_PORT:-4318}
HUB_DEV_PORT=${TAILTERM_HUB_DEV_PORT:-8765}

have_image() { container image ls 2>/dev/null | awk '{print $1":"$2}' | grep -qx "$1"; }
running() { container ls 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "$1"; }
exists() { container ls -a 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "$1"; }

build_hub() { container build -t "$HUB_IMAGE" hub; }
build_web() {
  commit=$(git rev-parse HEAD 2>/dev/null || echo unknown)
  if [ -n "$(git status --porcelain 2>/dev/null)" ]; then commit="$commit-dirty"; fi
  container build -t "$WEB_IMAGE" -f Dockerfile.static --build-arg TAILTERM_SOURCE_COMMIT="$commit" .
}

start_hub() {
  container volume ls 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "$VOLUME" || container volume create "$VOLUME" >/dev/null
  if [ -n "${TS_AUTHKEY:-}" ]; then
    container run -d --name "$HUB" -v "$VOLUME:/state" \
      -e TS_AUTHKEY="$TS_AUTHKEY" -e TS_HOSTNAME="${TS_HOSTNAME:-tailterm-hub}" "$HUB_IMAGE" >/dev/null
    echo "hub: joining the tailnet as ${TS_HOSTNAME:-tailterm-hub}"
  else
    container run -d --name "$HUB" -v "$VOLUME:/state" \
      -e TAILTERM_DEV_LISTEN=0.0.0.0:8765 -p "127.0.0.1:$HUB_DEV_PORT:8765" "$HUB_IMAGE" >/dev/null
    echo "hub: dev mode on http://127.0.0.1:$HUB_DEV_PORT (set TS_AUTHKEY to join the tailnet)"
  fi
}

start_web() {
  container run -d --name "$WEB" -p "127.0.0.1:$WEB_PORT:80" "$WEB_IMAGE" >/dev/null
  echo "tailterm: http://127.0.0.1:$WEB_PORT"
}

stop_one() {
  if exists "$1"; then
    container stop "$1" >/dev/null 2>&1 || true
    container rm "$1" >/dev/null 2>&1 || true
  fi
}

case "${1:-up}" in
  up)
    have_image "$HUB_IMAGE" || build_hub
    have_image "$WEB_IMAGE" || build_web
    running "$HUB" || { stop_one "$HUB"; start_hub; }
    running "$WEB" || { stop_one "$WEB"; start_web; }
    ;;
  down)
    stop_one "$HUB"; stop_one "$WEB"
    echo "stopped (state volume $VOLUME kept)"
    ;;
  rebuild)
    stop_one "$HUB"; stop_one "$WEB"
    build_hub; build_web
    start_hub; start_web
    ;;
  logs)
    container logs -f "$HUB"
    ;;
  status)
    container ls -a | awk -v h="$HUB" -v w="$WEB" 'NR==1 || $1==h || $1==w'
    ;;
  *)
    sed -n '2,15p' "$0"; exit 2
    ;;
esac
