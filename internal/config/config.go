// Package config loads audio-service settings from the environment via envconfig.
package config

import (
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// Config holds the service settings. Read from the environment with no global
// prefix (each field names its own var), matching the Ogen API's config style.
type Config struct {
	// Listen is the gRPC listen address. Accepts a full "host:port" or a bare
	// port (e.g. "50051", as Railway's PORT provides) — Load normalises a bare
	// port to ":port" so net.Listen gets the leading colon it requires.
	Listen string `envconfig:"AUDIO_SERVICE_LISTEN" default:":50051"`
	// Workers bounds how many ffprobe/ffmpeg processes run concurrently. ffmpeg
	// is process-based so real parallelism is possible; this caps it so a burst
	// can't exhaust the box. Transcode/segment-cut both hold a worker slot.
	Workers int `envconfig:"AUDIO_SERVICE_WORKERS" default:"4"`
	// ProbeTimeout bounds a single ffprobe (+ optional silencedetect) run. A
	// remote URL that stalls (slow origin) is aborted rather than pinning a
	// worker slot. The API also applies its own per-call deadline.
	ProbeTimeout time.Duration `envconfig:"PROBE_TIMEOUT" default:"90s"`
	// NormalizeTimeout bounds a single transcode-and-stream-to-PUT run. Audio can
	// be hour-long, so this is generous; the API also applies its own deadline.
	NormalizeTimeout time.Duration `envconfig:"NORMALIZE_TIMEOUT" default:"30m"`
	// TranscribeTimeout bounds a single TranscribeSegment run (segment cut +
	// Gemini round-trip). Segments are bounded (~5 min) so this is per-segment.
	TranscribeTimeout time.Duration `envconfig:"TRANSCRIBE_TIMEOUT" default:"10m"`
	// FFprobePath / FFmpegPath are the binary names or absolute paths. Defaults
	// resolve on PATH; override only for non-standard installs.
	FFprobePath string `envconfig:"FFPROBE_PATH" default:"ffprobe"`
	FFmpegPath  string `envconfig:"FFMPEG_PATH"  default:"ffmpeg"`
	// TargetSampleRate is the mono sample rate of the normalized derivative in
	// Hz. 16 kHz is the standard ASR rate and what TranscribeSegment expects; a
	// Normalize request may override per-call (0 -> this default).
	TargetSampleRate int `envconfig:"TARGET_SAMPLE_RATE" default:"16000"`
	// GeminiAPIKey authenticates TranscribeSegment against the Gemini Developer
	// API (the same key the Ogen embedder uses). Empty is tolerated at boot so
	// Probe/Normalize still serve; TranscribeSegment then returns Unavailable.
	GeminiAPIKey string `envconfig:"GEMINI_API_KEY" default:""`
	// TranscribeModel is the DEFAULT Gemini transcription model id. It is only a
	// fallback: the model is a per-request field (TranscribeSegmentRequest.model)
	// and is never compiled in. Used when the request leaves model empty.
	TranscribeModel string `envconfig:"TRANSCRIBE_MODEL" default:"gemini-2.5-flash"`
	// GCPercent sets the GC target (Go's GOGC) via debug.SetGCPercent at boot. A
	// lower value collects more often, trading CPU for a smaller heap high-water
	// mark — worthwhile here since the service sits CPU-idle between bursts. <=0
	// leaves the runtime default (100) in place.
	GCPercent int `envconfig:"AUDIO_SERVICE_GC_PERCENT" default:"50"`
	// MemoryLimitRatio sets Go's soft memory limit (GOMEMLIMIT) to this fraction
	// of the container's cgroup memory limit, read at boot. It makes the GC lean
	// harder as the heap nears the cap, keeping RSS down and guarding against OOM
	// under a burst. Ignored when GOMEMLIMIT is set explicitly, when <=0, or when
	// no finite cgroup limit is found (e.g. local dev). Values >1 are allowed but
	// unusual — leave headroom for the ffmpeg/ffprobe child processes' own RSS.
	MemoryLimitRatio float64 `envconfig:"AUDIO_SERVICE_MEMORY_LIMIT_RATIO" default:"0.9"`
	// ScavengeOnIdle returns freed memory to the OS (debug.FreeOSMemory) once the
	// worker pool drains to idle after a burst, so container RSS tracks real
	// usage instead of holding a high-water mark. Runs off the request path;
	// disable if the extra GC churn is unwanted.
	ScavengeOnIdle bool `envconfig:"AUDIO_SERVICE_SCAVENGE_ON_IDLE" default:"true"`
	// LogLevel is the minimum slog level: debug|info|warn|error. Unknown/empty
	// falls back to info. Bare LOG_LEVEL (not prefixed) matches the Ogen API's
	// knob so operators use identical settings across services (CON-107).
	LogLevel string `envconfig:"LOG_LEVEL" default:"info"`
	// LogFormat selects the slog handler: json (default, prod) or text (local).
	LogFormat string `envconfig:"LOG_FORMAT" default:"json"`
}

// Load reads and validates the configuration from the environment.
func Load() (*Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return nil, err
	}
	// A bare port like "50051" is a valid env value (Railway's PORT) but net.Listen
	// needs "host:port" — without a colon it errors "missing port in address".
	// Prefix it so the service binds all interfaces on that port.
	if c.Listen != "" && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	return &c, nil
}
