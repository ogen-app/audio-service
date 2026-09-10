# syntax=docker/dockerfile:1
# audio-service: static Go build (CON-282). No CGO — the service shells out to
# ffmpeg/ffprobe, which the runtime image provides. linux/amd64.

# ─── build ───────────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS build
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /audio-service ./cmd/audio-service

# ─── runtime ─────────────────────────────────────────────────────────────────
# Debian slim + ffmpeg (brings ffprobe). ffmpeg here is built with HTTPS
# support, so ffprobe/ffmpeg can read the presigned GET URLs the API hands over
# and stream the normalized derivative to a presigned PUT.
FROM debian:bookworm-slim
ARG GRPC_HEALTH_PROBE_VERSION=v0.4.34
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates ffmpeg wget; \
    wget -qO /usr/local/bin/grpc_health_probe \
      "https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/${GRPC_HEALTH_PROBE_VERSION}/grpc_health_probe-linux-amd64"; \
    chmod +x /usr/local/bin/grpc_health_probe; \
    apt-get purge -y wget; apt-get autoremove -y; rm -rf /var/lib/apt/lists/*; \
    useradd -r -u 10001 app

COPY --from=build /audio-service /usr/local/bin/audio-service
USER app

ENV AUDIO_SERVICE_LISTEN=":50051"
EXPOSE 50051

# Private-network only — orchestrators probe gRPC health via grpc_health_probe.
HEALTHCHECK --interval=10s --timeout=3s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/grpc_health_probe", "-addr=:50051"]

ENTRYPOINT ["/usr/local/bin/audio-service"]
