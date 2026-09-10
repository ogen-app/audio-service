package audioengine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
)

// NormalizeResult is the normalized derivative's metadata.
type NormalizeResult struct {
	DurationMs   int64
	SampleRate   int
	Channels     int
	BytesWritten int64
}

// Normalize transcodes the source at sourceURL to mono @ targetSampleRate Opus
// (ogg) and streams the result to destPutURL (a presigned PUT). The whole file
// is never buffered: ffmpeg's stdout is piped straight into the HTTP request
// body, so a multi-hundred-MB source flows through in bounded memory. It holds
// one worker slot for the duration and is bounded by the engine's normalize
// timeout.
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx, timeoutCancel := context.WithTimeout(ctx, e.normalizeTimeout)
	defer timeoutCancel()

	// Read the source duration cheaply before transcoding (the source URL is
	// still valid here) so the response can report it; Opus transcoding preserves
	// duration. Best-effort: a probe failure leaves duration 0, not a hard error.
	var durationMs int64
	if meta, mErr := e.ffprobeMeta(ctx, sourceURL); mErr == nil {
		if res, rErr := meta.toResult(); rErr == nil {
			durationMs = res.DurationMs
		}
	}

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
	cmd.Stderr = stderr

	// ffmpeg's stdout becomes the PUT request body via an os.Pipe: the HTTP
	// client reads from one end as ffmpeg writes the other, so bytes stream
	// through without a full-file buffer. A counting reader tallies the payload.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: stdout pipe: %v", ErrNormalize, err)
	}
	counter := &countingReader{r: stdout}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: start ffmpeg: %v", ErrNormalize, redactErr(err))
	}

	// The PUT body reads from the pipe until ffmpeg closes stdout (EOF). Content
	// length is unknown up front (transcode is one-pass), so the request is
	// chunked — the -1 length signals that to net/http.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, destPutURL, counter)
	if err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("%w: build PUT: %v", ErrNormalize, redactErr(err))
	}
	req.Header.Set("Content-Type", "audio/ogg")
	req.ContentLength = -1 // unknown length → chunked transfer

	resp, putErr := http.DefaultClient.Do(req)
	if putErr != nil {
		// The PUT failed, possibly before draining the request body. Cancel the
		// context so ffmpeg's write to the abandoned stdout pipe fails and it
		// exits, then reap it — otherwise cmd.Wait() could block on a full pipe.
		cancel()
		_ = cmd.Wait()
		return nil, fmt.Errorf("%w: PUT upload: %s", ErrNormalize, firstLine(stderr.String(), redactErr(putErr)))
	}
	// Always reap ffmpeg so no zombie/worker leak.
	waitErr := cmd.Wait()
	// Drain and close so the connection can be reused, then check the status.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: PUT returned %s", ErrNormalize, resp.Status)
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %v", ErrNormalize, ctx.Err())
		}
		return nil, fmt.Errorf("%w: ffmpeg: %s", ErrNormalize, firstLine(stderr.String(), waitErr))
	}

	written := counter.n.Load()
	if written == 0 {
		return nil, fmt.Errorf("%w: ffmpeg produced no output", ErrNormalize)
	}

	return &NormalizeResult{
		DurationMs:   durationMs,
		SampleRate:   rate,
		Channels:     1,
		BytesWritten: written,
	}, nil
}

// countingReader wraps an io.Reader and tallies bytes read through it, used to
// report how many bytes were streamed into the PUT without buffering them.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}

// redactErr strips presigned query strings from an error's message (net/http
// errors embed the request URL).
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", redactURLCredentials(err.Error()))
}
