package audioengine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ProbeResult is the probed audio metadata plus a silence verdict.
type ProbeResult struct {
	DurationMs int64
	Channels   int
	SampleRate int
	Container  string
	Codec      string
	Silent     bool // silent-throughout or zero-length
}

// Probe reads the audio at sourceURL and returns its metadata plus a
// silent/zero-length verdict, without transcoding. It holds one worker slot and
// is bounded by the engine's probe timeout. ffprobe range-reads the URL, so the
// whole (possibly hour-long) file is never buffered.
func (e *Engine) Probe(ctx context.Context, sourceURL string) (*ProbeResult, error) {
	if strings.TrimSpace(sourceURL) == "" {
		return nil, fmt.Errorf("%w: empty source url", ErrInvalidAudio)
	}

	if err := e.acquire(ctx); err != nil {
		return nil, err
	}
	defer e.releaseWorker()

	ctx, cancel := context.WithTimeout(ctx, e.probeTimeout)
	defer cancel()

	meta, err := e.ffprobeMeta(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	res, err := meta.toResult()
	if err != nil {
		return nil, err
	}

	// Zero-length is silent by definition; skip the silencedetect pass.
	if res.DurationMs <= 0 {
		res.Silent = true
		return res, nil
	}
	// Otherwise run a silencedetect pass to catch a file that decodes but carries
	// no audible signal end-to-end. A silencedetect failure is non-fatal — a
	// probe that already read metadata still returns (Silent stays false).
	silent, err := e.detectSilentThroughout(ctx, sourceURL, res.DurationMs)
	if err == nil {
		res.Silent = silent
	}
	return res, nil
}

// ffprobeMeta runs ffprobe -show_format -show_streams and decodes the JSON.
func (e *Engine) ffprobeMeta(ctx context.Context, url string) (*ffprobeOutput, error) {
	stdout := &cappedBuffer{limit: maxProbeOutputBytes}
	stderr := &cappedBuffer{limit: maxProbeOutputBytes}
	cmd := exec.CommandContext(ctx, e.ffprobe,
		"-v", "error",
		"-hide_banner",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		"-i", url,
	)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("audioengine: ffprobe: %w", ctx.Err())
		}
		return nil, classifyProbeErr(stderr.String(), err)
	}
	if stdout.overflow {
		// Metadata past the cap can't be parsed; treat as transient (plain
		// error -> Internal) rather than reject a possibly-valid upload.
		return nil, fmt.Errorf("audioengine: ffprobe output exceeded %d bytes", maxProbeOutputBytes)
	}
	var out ffprobeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		// ffprobe exited 0 but its output isn't decodable — an anomaly in the
		// tool's own output, not evidence of content corruption. Treat it as
		// transient so a valid upload isn't wrongly rejected; terminal
		// ErrInvalidAudio is reserved for recognized markers from classifyProbeErr.
		return nil, fmt.Errorf("audioengine: ffprobe output not decodable: %w", err)
	}
	return &out, nil
}

// silenceDurationRe extracts the "silence_duration: <seconds>" values ffmpeg's
// silencedetect filter logs to stderr for each detected silent run.
var silenceDurationRe = regexp.MustCompile(`silence_duration:\s*([0-9.]+)`)

// detectSilentThroughout runs an ffmpeg silencedetect pass and reports whether
// the summed detected-silence duration covers effectively the whole file. It
// reads the URL directly (decode-only, -f null) so nothing is written or
// buffered. A run error is returned to the caller, which treats it as
// non-fatal (keeps Silent=false).
func (e *Engine) detectSilentThroughout(ctx context.Context, url string, durationMs int64) (bool, error) {
	stderr := &cappedBuffer{limit: maxProbeOutputBytes}
	cmd := exec.CommandContext(ctx, e.ffmpeg,
		"-nostdin",
		"-hide_banner",
		"-loglevel", "info", // silencedetect logs at info level
		"-i", url,
		// -50 dB noise floor, and only count runs of >=0.5s as silence so brief
		// gaps aren't summed into a false "silent throughout".
		"-af", "silencedetect=noise=-50dB:d=0.5",
		"-f", "null",
		"-",
	)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("audioengine: silencedetect: %w", ctx.Err())
		}
		return false, fmt.Errorf("audioengine: silencedetect failed: %s", firstLine(stderr.String(), err))
	}

	var silentMs float64
	for _, m := range silenceDurationRe.FindAllStringSubmatch(stderr.String(), -1) {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil && f > 0 {
			silentMs += f * 1000
		}
	}
	// Consider it silent-throughout when detected silence covers >=99% of the
	// duration — a hair of headroom for filter/rounding boundary effects.
	return silentMs >= 0.99*float64(durationMs), nil
}

// ffprobeOutput is the subset of ffprobe -show_format/-show_streams JSON we use.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeStream struct {
	CodecType  string `json:"codec_type"`
	CodecName  string `json:"codec_name"`
	Channels   int    `json:"channels"`
	SampleRate string `json:"sample_rate"`
	Duration   string `json:"duration"`
}

type ffprobeFormat struct {
	FormatName string `json:"format_name"`
	Duration   string `json:"duration"`
}

// toResult projects ffprobe output into a ProbeResult, or ErrInvalidAudio when
// the input carries no decodable audio stream.
func (o *ffprobeOutput) toResult() (*ProbeResult, error) {
	var as *ffprobeStream
	for i := range o.Streams {
		if o.Streams[i].CodecType == "audio" {
			as = &o.Streams[i]
			break
		}
	}
	if as == nil {
		return nil, fmt.Errorf("%w: no audio stream", ErrInvalidAudio)
	}

	res := &ProbeResult{
		Channels:   as.Channels,
		SampleRate: parseIntField(as.SampleRate),
		Container:  o.Format.FormatName,
		Codec:      as.CodecName,
	}
	// Prefer the container duration; fall back to the stream's.
	if ms := parseSecondsToMs(o.Format.Duration); ms > 0 {
		res.DurationMs = ms
	} else {
		res.DurationMs = parseSecondsToMs(as.Duration)
	}
	return res, nil
}

func parseSecondsToMs(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0
	}
	return int64(f * 1000)
}

func parseIntField(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
