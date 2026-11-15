#!/usr/bin/env bash
# BombeCam add-on entrypoint: starts MediaMTX and the BombeCam gateway in the
# host's network namespace, then keeps both alive until the add-on stops.
#
# Options come from /data/options.json, which the Supervisor writes on every
# start and every time the user saves the add-on configuration.
set -euo pipefail

OPTIONS=/data/options.json

# option NAME DEFAULT -> prints the option's value, or DEFAULT when the key is
# missing or empty. jq is not part of the base image, so this uses python3.
option() {
    python3 - "$OPTIONS" "$1" "$2" <<'PY'
import json, sys
path, name, default = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    with open(path, encoding="utf-8") as fh:
        value = json.load(fh).get(name)
except FileNotFoundError:
    value = None
if value is None:
    value = ""
if isinstance(value, bool):
    value = "true" if value else "false"
value = str(value)
print(value if value != "" else default)
PY
}

opt_bool() {
    case "$(option "$1" "$2")" in
        true|True|TRUE|1|yes|on) echo 1 ;;
        *) echo 0 ;;
    esac
}

ADMIN_NAME="$(option admin_name admin)"
ADMIN_PASSWORD="$(option admin_password "")"
OSAIO_EMAIL="$(option osaio_email "")"
OSAIO_PASSWORD="$(option osaio_password "")"
OSAIO_COUNTRY="$(option osaio_country "")"
OSAIO_ENROLLED="$(option osaio_enrolled "")"
CAMERA_IP="$(option camera_ip "")"
CAMERA_SUBNET="$(option camera_subnet "")"
HTTP_BIND="$(option http_bind 0.0.0.0)"
HTTP_PORT="$(option http_port 8654)"
CONTROL_TRANSPORT="$(option control_transport cloud)"
MQTT_URL="$(option mqtt_url tcp://127.0.0.1:1883)"
STRICT_MANUAL="$(option strict_manual false)"
VIDEO_ONLY_DEVICES="$(option video_only_devices "")"
TIMEZONE="$(option timezone "")"
TIMEZONE_OFFSET="$(option timezone_offset "")"
RETENTION_DAYS="$(option recording_retention_days 0)"

CONFIG=/opt/bombecam/mediamtx.yml
if [ "$(opt_bool recording_enabled true)" = "1" ]; then
    CONFIG=/opt/bombecam/mediamtx.recording.yml
fi

# MediaMTX writes its recordings here; the gateway serves them from the same
# folder (BOMBECAM_RECORDINGS_DIR) as the per-camera latest recording.
mkdir -p /recordings /var/lib/bombecam

echo "[addon] MediaMTX starting with $(basename "$CONFIG")"
/usr/local/bin/mediamtx "$CONFIG" &
MEDIAMTX_PID=$!

# The gateway publishes into MediaMTX and calls its API; give it a moment so
# the first camera start does not race the RTSP listener.
sleep 1

export OSAIO_EMAIL OSAIO_PASSWORD OSAIO_COUNTRY OSAIO_ENROLLED
export CAMERA_IP CAMERA_SUBNET
export OSAIO_HTTP="${HTTP_BIND}:${HTTP_PORT}"
export BOMBECAM_ADMIN_NAME="$ADMIN_NAME"
export BOMBECAM_ADMIN_PASSWORD="$ADMIN_PASSWORD"
export BOMBECAM_HEADLESS=true
export BOMBECAM_NO_MEDIAMTX_SUPERVISOR=true
export BOMBECAM_MEDIAMTX_API=http://127.0.0.1:9997
export BOMBECAM_RTSP_PUBLISH_BASE=rtsp://127.0.0.1:8554
export OSAIO_HLS_BASE=http://127.0.0.1:8888
export BOMBECAM_WEBRTC_BASE=http://127.0.0.1:8889
export CONTROL_TRANSPORT MOSQUITTO_URL
export MOSQUITTO_URL="$MQTT_URL"
export STRICT_MANUAL
export BOMBECAM_VIDEO_ONLY_DEVICES="$VIDEO_ONLY_DEVICES"
export OSAIO_TIMEZONE="$TIMEZONE"
export OSAIO_ZONE_OFFSET="$TIMEZONE_OFFSET"
export FFMPEG_PATH=/usr/bin/ffmpeg
export BOMBECAM_RECORDINGS_DIR=/recordings
export BOMBECAM_RECORDINGS_RETENTION_DAYS="$RETENTION_DAYS"

echo "[addon] gateway starting on ${OSAIO_HTTP}"
/usr/local/bin/bombecam-gateway &
GATEWAY_PID=$!

# Stop both together: if either exits, the add-on stops so the Supervisor can
# show the failure instead of leaving half a stack running.
stop() {
    trap - TERM INT
    kill -TERM "$MEDIAMTX_PID" "$GATEWAY_PID" 2>/dev/null || true
    wait "$MEDIAMTX_PID" "$GATEWAY_PID" 2>/dev/null || true
}
trap stop TERM INT

wait -n "$MEDIAMTX_PID" "$GATEWAY_PID"
STATUS=$?
stop
echo "[addon] a process exited (status ${STATUS}); stopping"
exit "$STATUS"
