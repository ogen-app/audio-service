package audioengine

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// maxSegmentBytes caps the WAV a single SegmentCut may produce. Segments are
// bounded (~5 min) upstream; a 16 kHz mono s16le WAV of 5 min is ~9.6 MB, so
// 64 MiB is generous headroom while still bounding memory against a pathological
// or mis-bounded request. An overflow is a transient error.
const maxSegmentBytes = 64 << 20 // 64 MiB

// SegmentAudio is a cut of the normalized derivative, ready to hand to the
// transcriber as inline bytes.
type SegmentAudio struct {
	WAV      []byte // 16 kHz mono s16le WAV (RIFF)
	MIMEType string // always "audio/wav"
}

// SegmentCut extracts [startMs, endMs) from the normalized derivative at
// normalizedURL as a self-contained 16 kHz mono WAV. ffmpeg seeks to startMs and
// reads only (endMs-startMs) of the stream, so it range-reads the URL rather
// than pulling the whole derivative. The segment is bounded (~5 min) so the WAV
// is buffered in memory (capped) for the transcriber's inline request. Holds one
// worker slot; bounded by the engine's transcribe timeout.
func (e *Engine) SegmentCut(ctx context.Context, normalizedURL string, startMs, endMs int64) (*SegmentAudio, error) {
	if strings.TrimSpace(normalizedURL) == "" {
		return nil, fmt.Errorf("%w: empty normalized url", ErrInvalidAudio)
	}
	if endMs <= startMs {
		return nil, fmt.Errorf("%w: empty segment window [%d,%d)", ErrInvalidAudio, startMs, endMs)
	}
	if startMs < 0 {
		startMs = 0
	}

	if err := e.acquire(ctx); err != nil {
		return nil, err
	}
	defer e.releaseWorker()

	ctx, cancel := context.WithTimeout(ctx, e.transcribeTimeout)
	defer cancel()

	stdout := &cappedBuffer{limit: maxSegmentBytes}
	stderr := &cappedBuffer{limit: maxProbeOutputBytes}

	ss := formatMs(startMs)
	t := formatMs(endMs - startMs)
	cmd := exec.CommandContext(ctx, e.ffmpeg,
		"-nostdin",
		"-hide_banner",
		"-loglevel", "error",
		// Input seek (-ss before -i) is fast and range-friendly; -t bounds the
		// read to the window length so only the segment is decoded.
		"-ss", ss,
		"-i", normalizedURL,
		"-t", t,
		"-vn",
		"-ac", "1",
		"-ar", strconv.Itoa(e.targetSampleRate),
		"-c:a", "pcm_s16le",
		"-f", "wav",
		"pipe:1",
	)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("audioengine: segment cut: %w", ctx.Err())
		}
		return nil, classifyProbeErr(stderr.String(), err)
	}
	if stdout.overflow {
		return nil, fmt.Errorf("audioengine: segment exceeded %d bytes", maxSegmentBytes)
	}
	b := stdout.Bytes()
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: segment produced no audio", ErrInvalidAudio)
	}
	// Copy out of the cappedBuffer's backing array so the returned slice doesn't
	// pin the whole buffer.
	out := make([]byte, len(b))
	copy(out, b)
	return &SegmentAudio{WAV: out, MIMEType: "audio/wav"}, nil
}

// formatMs renders a millisecond offset as an ffmpeg-friendly seconds string
// with millisecond precision (e.g. 1500 -> "1.500").
func formatMs(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	d := time.Duration(ms) * time.Millisecond
	return strconv.FormatFloat(d.Seconds(), 'f', 3, 64)
}
