#!/usr/bin/env sh
# BombeCam - build the gateway image from this source and run it with local
# recording enabled. Run from the repository root (../docker-compose.yml paths
# are relative to deploy/).
#
#   deploy/scripts/up-recording.sh [up|down|logs|restart]
#
set -eu

# Built from this source (has local recording); used by `up` so the deploy
# does not fall back to the published image from deploy/.env.
export BOMBECAM_GATEWAY_IMAGE="${BOMBECAM_GATEWAY_IMAGE:-bombecam-gateway:local}"

cd "$(dirname "$0")/../.."

action="${1:-up}"

case "$action" in
  up)
    echo "Building bombecam-gateway:local from this source..."
    docker build -f deploy/Dockerfile.gateway -t bombecam-gateway:local .
    echo "Starting compose with the recording override..."
    docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.recording.yml up -d
    ;;
  down)
    docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.recording.yml down
    ;;
  logs)
    docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.recording.yml logs -f --tail=100
    ;;
  restart)
    "$0" down
    "$0" up
    ;;
  *)
    echo "usage: $0 [up|down|logs|restart]" >&2
    exit 2
    ;;
esac