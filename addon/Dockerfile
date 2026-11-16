# BombeCam add-on image (gateway + MediaMTX).
#
# One file for both builders:
#   - Home Assistant's add-on builder copies this addon/ folder into the
#     context and runs this Dockerfile from its root, so run.sh and the
#     MediaMTX configs sit next to it;
#   - .github/workflows/addon.yml builds from the top of the repository and
#     copies the same three files next to the sources;
# either way they end up in the root of the context it builds. That is why the
# COPY lines at the bottom name them without a folder.
#
# One image holds both processes the gateway expects to find on localhost:
# MediaMTX (RTSP/HLS/WebRTC) and the BombeCam gateway that publishes the
# cameras into it. They must share the host's network namespace: the gateway
# talks to MediaMTX on 127.0.0.1 and cameras push WebRTC over random UDP
# ports, so `host_network: true` in config.yaml is not optional.
#
# Set BOMBECAM_BASE to a build image with the toolchain for other platforms;
# this is what Home Assistant's add-on builder does when it cross-builds.
# ==============================================================================

ARG BOMBECAM_BASE=alpine:3.22

FROM --platform=$BUILDPLATFORM bluenviron/mediamtx:1.9.3 AS mediamtx

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS gateway
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
# Baked in the way deploy/Dockerfile.gateway does it: tools/setup reads these
# from the environment and prints the linker flags. Without them the build
# asks for the Osaio server key on the web page instead.
ARG BOMBECAM_SERVER_KEY=""
ARG BOMBECAM_APP_ID=""
ENV BOMBECAM_SERVER_KEY="${BOMBECAM_SERVER_KEY}" \
    BOMBECAM_APP_ID="${BOMBECAM_APP_ID}"
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN keyflag="$(go run ./tools/setup -ldflags)" && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" GOARM="${TARGETVARIANT#v}" \
    go build -buildvcs=false -trimpath -ldflags="-s -w ${keyflag}" \
    -o /out/bombecam-gateway ./cmd/bombecam-gateway

FROM ${BOMBECAM_BASE}
LABEL \
    org.opencontainers.image.title="BombeCam add-on" \
    org.opencontainers.image.description="BombeCam gateway add-on (gateway + MediaMTX)" \
    org.opencontainers.image.vendor="BombeCam" \
    org.opencontainers.image.licenses="MIT"
# bash runs run.sh; ffmpeg gives the gateway its publisher and audio for the
# browser view, so the same set the compose image installs.
RUN apk add --no-cache bash ffmpeg ca-certificates tzdata \
    && mkdir -p /opt/bombecam /recordings /var/lib/bombecam
COPY --from=mediamtx /mediamtx /usr/local/bin/mediamtx
COPY --from=gateway /out/bombecam-gateway /usr/local/bin/bombecam-gateway
COPY mediamtx.yml /opt/bombecam/mediamtx.yml
COPY mediamtx.recording.yml /opt/bombecam/mediamtx.recording.yml
COPY run.sh /usr/local/bin/run.sh
RUN chmod +x /usr/local/bin/run.sh /usr/local/bin/mediamtx /usr/local/bin/bombecam-gateway
EXPOSE 8654 8554 8888 8889 9997 8189
ENTRYPOINT ["/usr/local/bin/run.sh"]
