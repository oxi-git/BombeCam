#!/usr/bin/env bash
# BombeCam add-on entrypoint: starts MediaMTX and the BombeCam gateway in the
# host's network namespace, then keeps both alive until the add-on stops.
#
# Options come from /data/options.json, which the Supervisor writes on every
# start and every time the user saves the add-on configuration. It is a flat
# JSON object of strings, numbers and booleans; the base image has no jq and
# no python3, so the option is read out of the text with a pattern.

set -euo pipefail

OPTIONS=/data/options.json

# json_option NAME DEFAULT -> prints the top-level value of NAME, or DEFAULT
# when the key is missing or empty. An option never contains a quote, so the
# value is either quoted or runs to the next comma or closing brace.
json_option() {
    local name="$1" fallback="$2" match value
    if [ -r "$OPTIONS" ]; then
        match="$(tr -d '\n' < "$OPTIONS" \
            | sed -n "s/.*\"${name}\"[[:space:]]*:[[:space:]]*\(\"[^\"]*\"\|[^,}]*\).*/\1/p")"
        value="${match#\"}"
        value="${value%\"}"
        if [ -n "$value" ]; then
            printf '%s\n' "$value"
            return
        fi
    fi
    printf '%s\n' "$fallback"
}

json_bool() {
    case "$(json_option "$1" "$2")" in
        true|True|TRUE|1|yes|on) echo 1 ;;
        *) echo 0 ;;
    esac
}

ADMIN_NAME="$(json_option admin_name admin)"
ADMIN_PASSWORD="$(json_option admin_password "")"
OSAIO_EMAIL="$(json_option osaio_email "")"
OSAIO_PASSWORD="$(json_option osaio_password "")"
OSAIO_COUNTRY="$(json_option osaio_country "")"
OSAIO_ENROLLED="$(json_option osaio_enrolled "")"
CAMERA_IP="$(json_option camera_ip "")"
CAMERA_SUBNET="$(json_option camera_subnet "")"
HTTP_BIND="$(json_option http_bind 0.0.0.0)"
HTTP_PORT="$(json_option http_port 8654)"
CONTROL_TRANSPORT="$(json_option control_transport cloud)"
MQTT_URL="$(json_option mqtt_url tcp://127.0.0.1:1883)"
STRICT_MANUAL="$(json_option strict_manual false)"
VIDEO_ONLY_DEVICES="$(json_option video_only_devices "")"
TIMEZONE="$(json_option timezone "")"
TIMEZONE_OFFSET="$(json_option timezone_offset "")"
RETENTION_DAYS="$(json_option recording_retention_days 0)"

CONFIG=/opt/bombecam/mediamtx.yml
if [ "$(json_bool recording_enabled true)" = "1" ]; then
    CONFIG=/opt/bombecam/mediamtx.recording.yml
fi

# MediaMTX writes its recordings here; the gateway serves them from the same
# folder (BOMBECAM_RECORDINGS_DIR) as the per-camera latest recording. The
# profile, the operator tokens and any saved keys live in /var/lib/bombecam.
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
export CONTROL_TRANSPORT="$CONTROL_TRANSPORT"
export MOSQUITTO_URL="$MQTT_URL"
export STRICT_MANUAL="$STRICT_MANUAL"
export BOMBECAM_VIDEO_ONLY_DEVICES="$VIDEO_ONLY_DEVICES"
export OSAIO_TIMEZONE="$TIMEZONE"
export OSAIO_ZONE_OFFSET="$TIMEZONE_OFFSET"
export FFMPEG_PATH=/usr/bin/ffmpeg
export BOMBECAM_RECORDINGS_DIR=/recordings
export BOMBECAM_RECORDINGS_RETENTION_DAYS="$RETENTION_DAYS"
# Everything the gateway remembers between restarts, inside the add-on's
# persistent /data folder instead of the home directory.
export BOMBECAM_PROFILE_PATH=/data/bombecam/profile.enc
export BOMBECAM_PROFILE_KEY_FILE=/data/bombecam/profile.key

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
