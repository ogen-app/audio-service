package audioengine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// NormalizeResult is the normalized derivative's metadata.
type NormalizeResult struct {
	DurationMs   int64
	SampleRate   int
	Channels     int
	BytesWritten int64
}

// Normalize transcodes the source at sourceURL to mono @ targetSampleRate Opus
// (ogg) and uploads the result to destPutURL (a presigned PUT). ffmpeg writes
// the derivative to a temp file under the engine's tmp dir first, so the PUT can
// carry an explicit Content-Length: presigned single-request PUTs (S3/R2/GCS)
// reject chunked uploads with 411 Length Required. The derivative is small
// (mono Opus, ~30 MB/hour), so this costs a little ephemeral disk, not memory.
// It holds one worker slot for the duration and is bounded by the engine's
// normalize timeout. The temp file is removed on every path.
//
// targetSampleRate <= 0 uses the engine default. On any ffmpeg or upload
// failure it returns ErrNormalize (transient); a genuinely undecodable source is
// expected to have been rejected by the Probe gate as ErrInvalidAudio upstream.
func (e *Engine) Normalize(ctx context.Context, sourceURL, destPutURL string, targetSampleRate int) (*NormalizeResult, error) {
	if strings.TrimSpace(sourceURL) == "" {
		return nil, fmt.Errorf("%w: empty source url", ErrInvalidAudio)
	}
	if strings.TrimSpace(destPutURL) == "" {
		return nil, fmt.Errorf("%w: empty dest put url", ErrNormalize)
	}
	rate := targetSampleRate
	if rate <= 0 {
		rate = e.targetSampleRate
	}

	if err := e.acquire(ctx); err != nil {
		return nil, err
	}
	defer e.releaseWorker()

	ctx, cancel := context.WithTimeout(ctx, e.normalizeTimeout)
	defer cancel()

	// Read the source duration cheaply before transcoding (the source URL is
	// still valid here) so the response can report it; Opus transcoding preserves
	// duration. Best-effort: a probe failure leaves duration 0, not a hard error.
	var durationMs int64
	if meta, mErr := e.ffprobeMeta(ctx, sourceURL); mErr == nil {
		if res, rErr := meta.toResult(); rErr == nil {
			durationMs = res.DurationMs
		}
	}

	out, err := os.CreateTemp(e.tmpDir, "normalize-*.ogg")
	if err != nil {
		return nil, fmt.Errorf("%w: create temp file: %v", ErrNormalize, err)
	}
	defer func() {
		_ = out.Close()
		_ = os.Remove(out.Name())
	}()

	stderr := &cappedBuffer{limit: maxProbeOutputBytes}
	cmd := exec.CommandContext(ctx, e.ffmpeg,
		"-nostdin",
		"-hide_banner",
		"-loglevel", "error",
		"-i", sourceURL,
		"-vn",      // drop any cover-art / video stream
		"-ac", "1", // mono
		"-ar", strconv.Itoa(rate),
		"-c:a", "libopus",
		"-f", "ogg",
		"pipe:1",
	)
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %v", ErrNormalize, ctx.Err())
		}
		return nil, fmt.Errorf("%w: ffmpeg: %s", ErrNormalize, firstLine(stderr.String(), redactErr(err)))
	}

	info, err := out.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: stat temp file: %v", ErrNormalize, err)
	}
	size := info.Size()
	if size == 0 {
		return nil, fmt.Errorf("%w: ffmpeg produced no output", ErrNormalize)
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("%w: rewind temp file: %v", ErrNormalize, err)
	}

	// NopCloser keeps the transport from closing the file under the deferred
	// cleanup; an explicit ContentLength means no chunked transfer encoding.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, destPutURL, io.NopCloser(out))
	if err != nil {
		return nil, fmt.Errorf("%w: build PUT: %v", ErrNormalize, redactErr(err))
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "audio/ogg")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: PUT upload: %v", ErrNormalize, redactErr(err))
	}
	// Drain and close so the connection can be reused, then check the status.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: PUT returned %s", ErrNormalize, resp.Status)
	}

	return &NormalizeResult{
		DurationMs:   durationMs,
		SampleRate:   rate,
		Channels:     1,
		BytesWritten: size,
	}, nil
}

// redactErr strips presigned query strings from an error's message (net/http
// errors embed the request URL).
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", redactURLCredentials(err.Error()))
}
