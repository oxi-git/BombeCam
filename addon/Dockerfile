# ==============================================================================
# BombeCam - Home Assistant add-on.
#
# One image holds both processes the gateway expects to find on localhost:
# MediaMTX (RTSP/HLS/WebRTC) and the BombeCam gateway that publishes the
# cameras into it. They must share the host's network namespace: the gateway
# talks to MediaMTX on 127.0.0.1 and cameras push WebRTC over random UDP
# ports, so `host_network: true` in config.yaml is not optional.
#
# The add-on builder copies addon/ into the build context, so run.sh and the
# MediaMTX configs are at the top of it. .github/workflows/addon.yml builds
# the same image from the top of the repository: addon/ is copied there too
# (see the "Prepare the add-on context" step), so both paths build one file.
#
# Set BOMBECAM_BASE to a build image with the toolchain for other platforms;
# this is what Home Assistant's add-on builder does when it cross-builds.
# ==============================================================================

ARG BOMBECAM_BASE=alpine:3.22

# MediaMTX v1.9.3, the same release deploy/mediamtx.yml is pinned to. Only the
# binary is taken from it: the add-on runs its own config.
FROM --platform=$BUILDPLATFORM bluenviron/mediamtx:1.9.3 AS mediamtx

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS gateway
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
# Osaio server key and app ID, handed over as build arguments by CI and the
# add-on builder and visible in the image history: the published image has
# them built in anyway. Leave them empty to use osaio-setup.txt at the top of
# the tree, or to make an image whose gateway asks for them on its own page.
ARG BOMBECAM_SERVER_KEY=""
ARG BOMBECAM_APP_ID=""
ENV BOMBECAM_SERVER_KEY="${BOMBECAM_SERVER_KEY}" \
    BOMBECAM_APP_ID="${BOMBECAM_APP_ID}"
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# tools/setup reads the two environment variables above or osaio-setup.txt and
# prints the linker flags the build embeds.
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
RUN apk add --no-cache ffmpeg ca-certificates tzdata \
    && mkdir -p /opt/bombecam /recordings /var/lib/bombecam
COPY --from=mediamtx /mediamtx /usr/local/bin/mediamtx
COPY --from=gateway /out/bombecam-gateway /usr/local/bin/bombecam-gateway
COPY mediamtx.yml /opt/bombecam/mediamtx.yml
COPY mediamtx.recording.yml /opt/bombecam/mediamtx.recording.yml
COPY run.sh /usr/local/bin/run.sh
RUN chmod +x /usr/local/bin/run.sh /usr/local/bin/mediamtx /usr/local/bin/bombecam-gateway
EXPOSE 8654 8554 8888 8889 9997 8189
ENTRYPOINT ["/usr/local/bin/run.sh"]
