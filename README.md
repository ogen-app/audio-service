# audio-service

gRPC audio transcode + transcribe microservice for Ogen (CON-282). It is the
audio sibling of [video-service](https://github.com/ogen-app/video-service): the
Ogen API hands it short-lived presigned URLs, and it probes, normalizes, and
transcribes audio into time-anchored transcript segments.

Stateless and **internal-only** — the API reaches it over the private network
(no public port). Real audio is long-running (hour-long, multi-hundred-MB), so
**ogen owns the resumable state machine**: this service exposes three narrow,
independently retryable compute RPCs and holds no state and no database.

## Contract

`audio.v1.AudioService` (from the shared `buf.build/ogen-app/proto` module,
CON-220 — see `proto/audio/v1/audio.proto` in that repo). Three unary RPCs, all
presigned-URL based:

| RPC | Does |
|-----|------|
| `Probe(source_url, filename)` | ffprobe range-reads the source; returns `duration_ms`, `channels`, `sample_rate`, `container`, `codec`, and a `silent` (silent-throughout / zero-length) verdict. Cheap; drives ogen's validation gate. |
| `Normalize(source_url, dest_put_url, target_sample_rate)` | `ffmpeg -ac 1 -ar 16000 -c:a libopus -f ogg` transcodes the source and **streams** the result to a presigned PUT. Returns the derivative's metadata. Never buffers the whole file. |
| `TranscribeSegment(normalized_url, start_ms, end_ms, language_hint, model)` | Cuts `[start_ms, end_ms)` from the normalized derivative (ffmpeg range-read), transcribes it via **Gemini multimodal**, and returns utterances whose offsets are **rebased to the original asset timeline**, plus Gemini token usage. |

All three read/write over HTTP directly (range-requesting only what they need),
so a multi-hundred-MB file never streams through either the service or the API
process.

### Transcription backend

`TranscribeSegment` runs on **Gemini multimodal via the Gemini Developer API**
(the same `GEMINI_API_KEY` the Ogen embedder uses). Per the v1 decision there is
no dedicated ASR: per-utterance offsets and confidence are model-reported and
**approximate** (confidence `-1` marks "not exposed"). When the model returns
no/garbled timestamps for a segment, the service falls back to the segment window
bounds and marks the result approximate.

**EU data residency is deferred.** v1 uses the Gemini Developer API for
simplicity; a later revision can switch the transcriber to Vertex AI in
`europe-west` without changing the RPC contract (the `genai` SDK already supports
both backends — swap `Backend` and add a location).

The model id is **never compiled in** — it comes from the request's `model`
field, falling back to `TRANSCRIBE_MODEL` only when the request leaves it empty.

### Error semantics

| Condition | gRPC code | API behaviour |
|-----------|-----------|---------------|
| Unsupported / corrupt container, zero-length, silent-throughout | `InvalidArgument` | reject upload (terminal, no transcode/transcribe spend) |
| Normalization ffmpeg/upload failure | `Internal` (`ErrNormalize`) | retry the whole normalize step |
| ffprobe/ffmpeg network / transient | `Internal` | keep upload, degrade |
| Gemini 5xx / unavailable | `Unavailable` | retry the segment |
| Gemini 429 rate-limited | `ResourceExhausted` | back off, retry |
| Gemini / ffmpeg deadline | `DeadlineExceeded` | retry the segment |
| No `GEMINI_API_KEY` configured | `Unavailable` | key can be set without a restart |
| Empty `source_url` / bad window | `InvalidArgument` | — |

The ffmpeg error classifier is deliberately conservative: only well-known
content-corruption markers are terminal, so a transient blip never wrongly
rejects a valid upload.

Also serves standard `grpc.health.v1.Health` (both the `""` overall key and
`audio.v1.AudioService`) for orchestrator probes.

## Configuration

Environment variables (no prefix, matching the Ogen API's style):

| Var | Default | Purpose |
|-----|---------|---------|
| `AUDIO_SERVICE_LISTEN` | `:50051` | gRPC listen address (bare port accepted) |
| `AUDIO_SERVICE_WORKERS` | `4` | max concurrent ffprobe/ffmpeg processes |
| `PROBE_TIMEOUT` | `90s` | per-probe upper bound |
| `NORMALIZE_TIMEOUT` | `30m` | per-normalize upper bound (audio can be hour-long) |
| `TRANSCRIBE_TIMEOUT` | `10m` | per-segment upper bound (cut + Gemini round-trip) |
| `FFPROBE_PATH` | `ffprobe` | ffprobe binary (name or path) |
| `FFMPEG_PATH` | `ffmpeg` | ffmpeg binary (name or path) |
| `TARGET_SAMPLE_RATE` | `16000` | mono sample rate of the normalized derivative (Hz) |
| `GEMINI_API_KEY` | (unset) | Gemini Developer API key; empty ⇒ `TranscribeSegment` returns `Unavailable` |
| `TRANSCRIBE_MODEL` | `gemini-2.5-flash` | **default** Gemini model id; the request's `model` field wins |
| `AUDIO_SERVICE_GC_PERCENT` | `50` | GC target (GOGC); lower = smaller heap, more CPU. `<=0` keeps the runtime default |
| `AUDIO_SERVICE_MEMORY_LIMIT_RATIO` | `0.9` | soft mem limit (GOMEMLIMIT) as a fraction of the cgroup limit; ignored if `GOMEMLIMIT` is set or no cgroup limit is found |
| `AUDIO_SERVICE_SCAVENGE_ON_IDLE` | `true` | return freed memory to the OS once the worker pool drains after a burst |
| `LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `LOG_FORMAT` | `json` | `json` (prod) or `text` (local) |

See [MEMORY_TUNING.md](MEMORY_TUNING.md) for the Railway memory-cost knobs.

## Build & run

The service shells out to **ffmpeg/ffprobe** (built with HTTPS support), so no
CGO is needed. The Docker runtime image installs ffmpeg from Debian.

```sh
make proto                # regenerate gen/ from the pinned proto module
go build ./cmd/audio-service
go test ./...             # end-to-end audioengine tests run when ffmpeg is present, else skip

docker build -t audio-service .
```

> **Proto note (CON-282):** the `audio.v1` addition is not yet published to the
> `buf.build/ogen-app/proto` BSR module. Until it is, the committed `gen/` was
> produced from a local checkout of the proto repo:
>
> ```sh
> buf generate . --path audio/v1/audio.proto \
>   --template ../audio-service/buf.gen.yaml -o ../audio-service   # run from proto/proto
> ```
>
> Once `audio.v1` ships in a tagged release, bump `PROTO_VERSION` in the Makefile
> (>= `v1.2.0`) and switch `make proto` back to the BSR module.

The API reaches it at `audio-service:50051` in compose (behind an `audio`
profile) / `audio-service.railway.internal:50051` in prod, via an
`AUDIO_SERVICE_ADDR` on the API side.
