// Package audioengine transcodes and slices audio by shelling out to
// ffprobe/ffmpeg (CON-282). It is the audio counterpart of video-service's
// videoengine: the process-based ffmpeg backend, a worker semaphore, capped
// output buffers, and an idle scavenge are all carried over verbatim; only the
// operations differ.
//
// Three operations back the three RPCs:
//
//   - Probe    — ffprobe reads the source URL's metadata (duration / channels /
//     sample-rate / container / codec) and a silencedetect pass classifies a
//     silent-throughout / zero-length input.
//   - Normalize — ffmpeg transcodes the source to mono @ target_sample_rate Opus
//     and streams the ogg result straight to a presigned PUT. The whole file is
//     never buffered (ffmpeg's stdout is piped to the HTTP request body).
//   - SegmentCut — ffmpeg seek-cuts [start,end) from the normalized derivative
//     into a bounded temp WAV for the transcriber (segments are short, ~5 min).
//
// All three read/write over HTTP directly (range-requesting only what they
// need), so the service never holds a multi-hundred-MB file in memory.
package audioengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"
)

// ErrInvalidAudio marks input ffprobe/ffmpeg could not decode as audio (corrupt,
// truncated, unsupported container, or not a media file). The gRPC layer maps it
// to InvalidArgument so the client treats it as terminal (no retry). Everything
// else (network, timeout, missing binary) is transient → Internal/Unavailable.
var ErrInvalidAudio = errors.New("audioengine: invalid or unreadable audio")

// ErrSilent marks a silent-throughout / zero-length input: ffprobe read it but
// there is no audio to transcribe. Terminal (InvalidArgument) — ogen rejects it
// upstream without any transcode/transcribe spend. Distinct from ErrInvalidAudio
// so the server can classify it separately while both land on InvalidArgument.
var ErrSilent = errors.New("audioengine: silent or zero-length audio")

// ErrNormalize marks a transcode/upload failure in Normalize: ffmpeg exited
// non-zero, or the streamed PUT was rejected. The gRPC layer maps it to Internal
// (transient) with a distinct sentinel so the caller can retry the whole
// normalize step. It is NOT a content verdict — a genuinely undecodable source
// surfaces as ErrInvalidAudio from the Probe gate instead.
var ErrNormalize = errors.New("audioengine: normalization failed")

// maxProbeOutputBytes bounds ffprobe's stdout (metadata JSON) and ffmpeg's
// stderr so a pathological input can't balloon memory. Legitimate output is far
// smaller; an overflow means the output is unusable and is treated as transient.
const maxProbeOutputBytes = 4 << 20 // 4 MiB

// Engine runs ffprobe/ffmpeg, bounded by a worker semaphore.
type Engine struct {
	ffprobe           string
	ffmpeg            string
	probeTimeout      time.Duration
	normalizeTimeout  time.Duration
	transcribeTimeout time.Duration
	targetSampleRate  int
	sem               chan struct{}

	// scavengeOnIdle returns freed memory to the OS once the pool drains after a
	// burst; active tracks in-flight work so the drain-to-idle edge can be
	// detected, and scavenging collapses overlapping scavenge triggers into one.
	// scavengeFn is the actual reclaim (debug.FreeOSMemory), injectable for tests.
	scavengeOnIdle bool
	scavengeFn     func()
	active         atomic.Int64
	scavenging     atomic.Bool
}

// Config tunes the engine.
type Config struct {
	FFprobePath       string
	FFmpegPath        string
	Workers           int
	ProbeTimeout      time.Duration
	NormalizeTimeout  time.Duration
	TranscribeTimeout time.Duration
	TargetSampleRate  int
	ScavengeOnIdle    bool
}

// defaultTargetSampleRate is the mono ASR rate used when none is configured.
const defaultTargetSampleRate = 16000

// New resolves the ffprobe/ffmpeg binaries and initialises the worker pool. It
// fails fast if either binary is missing so a misconfigured deploy surfaces at
// boot, not on the first request.
func New(cfg Config) (*Engine, error) {
	ffprobe := orDefault(cfg.FFprobePath, "ffprobe")
	ffmpeg := orDefault(cfg.FFmpegPath, "ffmpeg")
	if _, err := exec.LookPath(ffprobe); err != nil {
		return nil, fmt.Errorf("audioengine: ffprobe not found (%q): %w", ffprobe, err)
	}
	if _, err := exec.LookPath(ffmpeg); err != nil {
		return nil, fmt.Errorf("audioengine: ffmpeg not found (%q): %w", ffmpeg, err)
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = 4
	}
	return &Engine{
		ffprobe:           ffprobe,
		ffmpeg:            ffmpeg,
		probeTimeout:      orDuration(cfg.ProbeTimeout, 90*time.Second),
		normalizeTimeout:  orDuration(cfg.NormalizeTimeout, 30*time.Minute),
		transcribeTimeout: orDuration(cfg.TranscribeTimeout, 10*time.Minute),
		targetSampleRate:  orInt(cfg.TargetSampleRate, defaultTargetSampleRate),
		sem:               make(chan struct{}, workers),
		scavengeOnIdle:    cfg.ScavengeOnIdle,
		scavengeFn:        debug.FreeOSMemory,
	}, nil
}

// Close is a no-op; the process-based backend holds no long-lived resources.
// Present for symmetry with videoengine so cmd/ wiring is identical.
func (e *Engine) Close() error { return nil }

// TargetSampleRate reports the engine's default mono sample rate (Hz).
func (e *Engine) TargetSampleRate() int { return e.targetSampleRate }

// acquire takes a worker slot or returns the context error if the caller's
// deadline fires first. The matching releaseWorker frees it (and may scavenge).
func (e *Engine) acquire(ctx context.Context) error {
	select {
	case e.sem <- struct{}{}:
		e.active.Add(1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseWorker frees the worker slot and, when this was the last in-flight
// operation, kicks off an idle scavenge so the burst's freed memory returns to
// the OS instead of lingering as container RSS. Only the operation that drains
// the pool to zero triggers it, so a steady stream of work scavenges at most
// once per quiet gap rather than after every call.
func (e *Engine) releaseWorker() {
	idle := e.active.Add(-1) == 0
	<-e.sem
	if idle && e.scavengeOnIdle {
		e.scavenge()
	}
}

// scavenge returns freed memory to the OS off the request path. debug.
// FreeOSMemory runs a stop-the-world GC, so it must not block an operation; it
// also must not pile up if bursts drain to idle repeatedly, hence the
// single-flight guard. If work arrives mid-scavenge the extra GC is harmless —
// just a little CPU, of which this service has ample between bursts.
func (e *Engine) scavenge() {
	if !e.scavenging.CompareAndSwap(false, true) {
		return // a scavenge is already running
	}
	go func() {
		defer e.scavenging.Store(false)
		e.scavengeFn()
		slog.Debug("returned freed memory to OS after idle",
			"component", "audioengine.scavenge")
	}()
}

// cappedBuffer is an io.Writer that accumulates at most limit bytes and
// discards the rest, recording that an overflow happened. It bounds memory
// when collecting the stdout/stderr of an external process whose output size
// isn't trusted. Write never errors, so the process runs to completion (its
// excess output is dropped) instead of dying on a short write.
type cappedBuffer struct {
	limit    int
	buf      bytes.Buffer
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := c.limit - c.buf.Len(); remaining > 0 {
		if len(p) <= remaining {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:remaining])
			c.overflow = true
		}
	} else if len(p) > 0 {
		c.overflow = true
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte  { return c.buf.Bytes() }
func (c *cappedBuffer) String() string { return c.buf.String() }

// classifyProbeErr decides whether an ffprobe/ffmpeg failure is a terminal
// "not audio" verdict or a transient (network/timeout/binary) fault. It is
// deliberately conservative: only well-known content-corruption markers are
// terminal, so a transient blip never wrongly rejects a valid upload — the API
// then degrades gracefully.
func classifyProbeErr(stderr string, runErr error) error {
	s := strings.ToLower(stderr)
	transient := []string{
		"connection refused", "could not resolve", "failed to resolve",
		"connection reset", "connection timed out", "timed out", "no route to host",
		"network is unreachable", "server returned 4", "server returned 5",
		"http error", "tls", "i/o error", "protocol not found",
	}
	for _, m := range transient {
		if strings.Contains(s, m) {
			return fmt.Errorf("audioengine: ffprobe transient: %s", firstLine(stderr, runErr))
		}
	}
	// Only unambiguous content-corruption markers are terminal. Excluded on
	// purpose: "end of file" (a truncated transfer, not necessarily corrupt),
	// "invalid argument" (an ffmpeg option / EINVAL, not a content signal), and
	// "truncat" (also matches the recoverable "Truncating packet" warning).
	invalid := []string{
		"invalid data found", "moov atom not found", "could not find codec parameters",
		"unknown format", "header missing",
	}
	for _, m := range invalid {
		if strings.Contains(s, m) {
			return fmt.Errorf("%w: %s", ErrInvalidAudio, firstLine(stderr, runErr))
		}
	}
	// Unknown ffprobe failure: treat as transient so a valid upload is never
	// wrongly rejected.
	return fmt.Errorf("audioengine: ffprobe failed: %s", firstLine(stderr, runErr))
}

func firstLine(stderr string, runErr error) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		if runErr != nil {
			return redactURLCredentials(runErr.Error())
		}
		return "unknown error"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return redactURLCredentials(s)
}

// presignedQueryRe matches the query string of an http(s) URL — where presigned
// credentials (signatures, access keys, tokens) live. ffmpeg echoes the source
// URL in its error output, so the query is stripped before that message is
// wrapped into an error reaching a gRPC response. The full URL is only ever
// exposed via server-side logging, never to the client.
var presignedQueryRe = regexp.MustCompile(`(https?://[^\s?]+)\?\S+`)

// redactURLCredentials removes URL query strings from a diagnostic message,
// leaving the rest of the text intact.
func redactURLCredentials(s string) string {
	return presignedQueryRe.ReplaceAllString(s, "${1}?<redacted>")
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
