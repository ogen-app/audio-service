# syntax=docker/dockerfile:1
# audio-service: static Go build (CON-282). No CGO — the service shells out to
# ffmpeg/ffprobe, which the image builds from source (audio-only, see below). linux/amd64.

# ─── build ───────────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS build
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /audio-service ./cmd/audio-service

# ─── ffmpeg ──────────────────────────────────────────────────────────────────
# Minimal audio-only ffmpeg/ffprobe. Debian's ffmpeg package dynamically loads
# ~210 shared libraries (~236 MB: x265, AV1, codec2, rsvg, ICU, flite, …) on
# every exec. Their pages land in the kernel page cache charged to the
# container's cgroup and stay there — no memory pressure ever evicts them — so
# the first ffmpeg run left a permanent ~210 MB `file` plateau in the billed
# memory metric. This build carries only what the service uses: common audio
# demuxers/decoders, the libopus encoder + WAV muxer, the silencedetect /
# resample filters, and HTTPS via OpenSSL. Its only shared deps are libopus,
# libssl/libcrypto, zlib and glibc.
FROM debian:bookworm-slim AS ffmpeg
ARG FFMPEG_VERSION=8.1.3
ARG FFMPEG_SHA256=7138d28c96d9d3e3af4ee3d8cad72741f8ffb40da90c1112235dea3ecd3178a3
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates curl xz-utils build-essential nasm pkg-config \
      libopus-dev libssl-dev zlib1g-dev; \
    rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN set -eux; \
    curl -fsSL -o ffmpeg.tar.xz "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz"; \
    echo "${FFMPEG_SHA256}  ffmpeg.tar.xz" | sha256sum -c -; \
    tar -xJf ffmpeg.tar.xz --strip-components=1; \
    rm ffmpeg.tar.xz
# Source formats are whatever users upload (voice memos, podcasts, screen
# recordings), so the demuxer/decoder set covers the common audio codecs and
# containers, plus video-stream parsers so ffprobe can describe (and -vn skip)
# a video track without a video decoder.
RUN set -eux; \
    ./configure \
      --prefix=/opt/ffmpeg \
      --disable-everything --disable-autodetect \
      --disable-doc --disable-debug --disable-ffplay \
      --disable-avdevice --disable-swscale \
      --enable-pthreads --enable-libopus --enable-openssl --enable-zlib \
      --enable-protocol=file,pipe,http,https,tcp,tls,udp \
      --enable-demuxer=aac,ac3,aiff,amr,ape,asf,caf,dts,eac3,flac,matroska,mov,mp3,mpegts,ogg,w64,wav,wv \
      --enable-decoder='aac,aac_latm,ac3,eac3,alac,amrnb,amrwb,ape,dca,flac,mp1,mp1float,mp2,mp2float,mp3,mp3float,opus,vorbis,wavpack,wmav1,wmav2,wmapro,wmalossless,pcm_*,adpcm_*' \
      --enable-parser=aac,aac_latm,ac3,dca,flac,mpegaudio,opus,vorbis,h264,hevc,mjpeg \
      --enable-encoder=libopus,pcm_s16le \
      --enable-muxer=ogg,wav,null \
      --enable-filter=abuffer,abuffersink,aformat,anull,aresample,atrim,silencedetect; \
    make -j"$(nproc)"; \
    make install; \
    strip /opt/ffmpeg/bin/ffmpeg /opt/ffmpeg/bin/ffprobe

# ─── runtime ─────────────────────────────────────────────────────────────────
# Debian slim + the minimal ffmpeg/ffprobe above (built with HTTPS support, so
# they can read the presigned GET URLs the API hands over and upload the
# normalized derivative to a presigned PUT).
FROM debian:bookworm-slim
ARG GRPC_HEALTH_PROBE_VERSION=v0.4.34
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates libopus0 libssl3 zlib1g wget; \
    wget -qO /usr/local/bin/grpc_health_probe \
      "https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/${GRPC_HEALTH_PROBE_VERSION}/grpc_health_probe-linux-amd64"; \
    chmod +x /usr/local/bin/grpc_health_probe; \
    apt-get purge -y wget; apt-get autoremove -y; rm -rf /var/lib/apt/lists/*; \
    useradd -r -u 10001 app

COPY --from=ffmpeg /opt/ffmpeg/bin/ffmpeg /opt/ffmpeg/bin/ffprobe /usr/local/bin/
COPY --from=build /audio-service /usr/local/bin/audio-service
USER app

ENV AUDIO_SERVICE_LISTEN=":50051"
EXPOSE 50051

# Private-network only — orchestrators probe gRPC health via grpc_health_probe.
HEALTHCHECK --interval=10s --timeout=3s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/grpc_health_probe", "-addr=:50051"]

ENTRYPOINT ["/usr/local/bin/audio-service"]
