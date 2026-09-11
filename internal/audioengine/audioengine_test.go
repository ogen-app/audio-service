package audioengine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestToResult_NoAudioStreamIsInvalid(t *testing.T) {
	// Video-only ffprobe output → terminal invalid (no audio to work with).
	out := &ffprobeOutput{
		Streams: []ffprobeStream{{CodecType: "video", CodecName: "h264"}},
		Format:  ffprobeFormat{FormatName: "mp4", Duration: "12.5"},
	}
	if _, err := out.toResult(); !errors.Is(err, ErrInvalidAudio) {
		t.Fatalf("video-only must be ErrInvalidAudio, got %v", err)
	}
}

func TestToResult_PicksAudioStream(t *testing.T) {
	out := &ffprobeOutput{
		Streams: []ffprobeStream{
			{CodecType: "video", CodecName: "h264"},
			{CodecType: "audio", CodecName: "mp3", Channels: 2, SampleRate: "44100", Duration: "10.0"},
		},
		Format: ffprobeFormat{FormatName: "mp3", Duration: "10.5"},
	}
	res, err := out.toResult()
	if err != nil {
		t.Fatalf("toResult: %v", err)
	}
	if res.Codec != "mp3" || res.Channels != 2 || res.SampleRate != 44100 {
		t.Errorf("stream fields wrong: %+v", res)
	}
	if res.Container != "mp3" {
		t.Errorf("container = %q", res.Container)
	}
	if res.DurationMs != 10500 { // format duration wins
		t.Errorf("duration = %d, want 10500", res.DurationMs)
	}
}

func TestClassifyProbeErr(t *testing.T) {
	if err := classifyProbeErr("Invalid data found when processing input", nil); !errors.Is(err, ErrInvalidAudio) {
		t.Errorf("corrupt content must be terminal, got %v", err)
	}
	if err := classifyProbeErr("moov atom not found", nil); !errors.Is(err, ErrInvalidAudio) {
		t.Errorf("moov atom missing must be terminal, got %v", err)
	}
	// Network faults are transient (NOT ErrInvalidAudio) so the API degrades.
	if err := classifyProbeErr("Connection refused", nil); errors.Is(err, ErrInvalidAudio) {
		t.Errorf("network fault must be transient, got %v", err)
	}
	if err := classifyProbeErr("Server returned 503 Service Unavailable", nil); errors.Is(err, ErrInvalidAudio) {
		t.Errorf("5xx must be transient, got %v", err)
	}
	if err := classifyProbeErr("some unrecognised ffprobe complaint", nil); errors.Is(err, ErrInvalidAudio) {
		t.Errorf("unknown must default transient, got %v", err)
	}
	// Ambiguous markers must NOT be terminal.
	for _, msg := range []string{"End of file", "Invalid argument", "Truncating packet of size 100 to 40"} {
		if err := classifyProbeErr(msg, nil); errors.Is(err, ErrInvalidAudio) {
			t.Errorf("%q must be transient, got %v", msg, err)
		}
	}
}

func TestParseHelpers(t *testing.T) {
	if got := parseSecondsToMs("10.5"); got != 10500 {
		t.Errorf("parseSecondsToMs(10.5) = %d", got)
	}
	if got := parseSecondsToMs("N/A"); got != 0 {
		t.Errorf("parseSecondsToMs(N/A) = %d, want 0", got)
	}
	if got := parseIntField("44100"); got != 44100 {
		t.Errorf("parseIntField = %d", got)
	}
	if got := parseIntField("N/A"); got != 0 {
		t.Errorf("parseIntField(N/A) = %d, want 0", got)
	}
}

func TestFormatMs(t *testing.T) {
	cases := map[int64]string{0: "0.000", 1500: "1.500", 250: "0.250", 61000: "61.000"}
	for in, want := range cases {
		if got := formatMs(in); got != want {
			t.Errorf("formatMs(%d) = %q, want %q", in, got, want)
		}
	}
	if got := formatMs(-5); got != "0.000" {
		t.Errorf("formatMs(-5) = %q, want 0.000", got)
	}
}

func TestCappedBuffer(t *testing.T) {
	c := &cappedBuffer{limit: 4}

	if n, _ := c.Write([]byte("ab")); n != 2 || c.overflow || string(c.Bytes()) != "ab" {
		t.Fatalf("under-limit: n=%d overflow=%v bytes=%q", n, c.overflow, c.Bytes())
	}
	if n, _ := c.Write([]byte("cdef")); n != 4 {
		t.Fatalf("Write must report full length, got %d", n)
	}
	if !c.overflow {
		t.Fatal("expected overflow after crossing the limit")
	}
	if string(c.Bytes()) != "abcd" {
		t.Fatalf("buffer must cap at limit, got %q", c.Bytes())
	}
	if n, _ := c.Write([]byte("ghi")); n != 3 || len(c.Bytes()) != 4 {
		t.Fatalf("past-limit write should discard: n=%d len=%d", n, len(c.Bytes()))
	}
	if c.String() != string(c.Bytes()) {
		t.Errorf("String %q != Bytes %q", c.String(), c.Bytes())
	}
}

func TestRedactURLCredentials(t *testing.T) {
	in := "https://bucket.s3.amazonaws.com/t/abc/key.mp3?X-Amz-Signature=deadbeefcafe&X-Amz-Credential=AKIAEXAMPLE: Invalid data found"
	got := firstLine(in, nil)
	for _, secret := range []string{"X-Amz-Signature", "deadbeefcafe", "AKIAEXAMPLE"} {
		if strings.Contains(got, secret) {
			t.Fatalf("credential %q leaked into %q", secret, got)
		}
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("expected a redaction marker, got %q", got)
	}
	if !strings.Contains(got, "Invalid data found") {
		t.Errorf("message dropped: %q", got)
	}
	if got := firstLine("moov atom not found", nil); got != "moov atom not found" {
		t.Errorf("non-URL message must pass through, got %q", got)
	}
}

func TestReleaseWorker_ScavengesOnDrainToIdle(t *testing.T) {
	scavenged := make(chan struct{}, 4)
	e := &Engine{
		sem:            make(chan struct{}, 4),
		scavengeOnIdle: true,
		scavengeFn:     func() { scavenged <- struct{}{} },
	}
	e.sem <- struct{}{}
	e.active.Add(1)
	e.sem <- struct{}{}
	e.active.Add(1)

	e.releaseWorker() // 2 -> 1: still busy, must not scavenge
	select {
	case <-scavenged:
		t.Fatal("scavenged while a worker was still active")
	case <-time.After(50 * time.Millisecond):
	}

	e.releaseWorker() // 1 -> 0: drained to idle, must scavenge
	select {
	case <-scavenged:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a scavenge after draining to idle")
	}
}

func TestScavenge_SingleFlight(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	e := &Engine{
		scavengeOnIdle: true,
		scavengeFn: func() {
			started <- struct{}{}
			<-release
		},
	}
	e.scavenge()
	e.scavenge() // dropped while the first is still running

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first scavenge never started")
	}
	select {
	case <-started:
		t.Fatal("second scavenge ran despite single-flight guard")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
}

// --- ffmpeg-dependent end-to-end tests (skipped when ffmpeg/ffprobe absent) ---

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
}

// genTone writes a short mono sine-tone file at path via ffmpeg's lavfi source.
func genTone(t *testing.T, path string, seconds int) {
	t.Helper()
	gen := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+strconv.Itoa(seconds),
		"-ac", "1", "-ar", "44100", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("could not generate tone (codec unavailable?): %v: %s", err, out)
	}
}

func TestProbeEndToEnd(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.wav")
	genTone(t, src, 2)

	eng, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := eng.Probe(ctx, src)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Channels != 1 {
		t.Errorf("channels = %d, want 1", res.Channels)
	}
	if res.SampleRate != 44100 {
		t.Errorf("sample_rate = %d, want 44100", res.SampleRate)
	}
	if res.DurationMs < 1500 || res.DurationMs > 2500 {
		t.Errorf("duration = %dms, want ~2000", res.DurationMs)
	}
	if res.Silent {
		t.Error("a 440Hz tone must not be classified silent")
	}
}

func TestProbe_SilenceIsDetected(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "silence.wav")
	gen := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono", "-t", "2", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("could not generate silence: %v: %s", err, out)
	}

	eng, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Probe(context.Background(), src)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Silent {
		t.Errorf("a null (silent) source must be classified silent, got %+v", res)
	}
}

func TestProbe_InvalidContentIsTerminal(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "notaudio.mp3")
	if err := os.WriteFile(bad, []byte("this is definitely not an audio file"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	eng, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Probe(context.Background(), bad); !errors.Is(err, ErrInvalidAudio) {
		t.Fatalf("garbage input must be ErrInvalidAudio, got %v", err)
	}
}

func TestNormalizeEndToEnd_StreamsToPUT(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.wav")
	genTone(t, src, 2)

	// A stub PUT target that records how many bytes it received.
	var mu sync.Mutex
	var got int64
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		method = r.Method
		mu.Unlock()
		n, _ := io.Copy(io.Discard, r.Body)
		mu.Lock()
		got = n
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	eng, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Normalize(context.Background(), src, srv.URL+"/normalized.opus", 16000)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if method != http.MethodPut {
		t.Errorf("upload method = %q, want PUT", method)
	}
	if res.SampleRate != 16000 || res.Channels != 1 {
		t.Errorf("derivative meta = %dHz/%dch, want 16000/1", res.SampleRate, res.Channels)
	}
	if res.BytesWritten <= 0 || res.BytesWritten != got {
		t.Errorf("bytes_written = %d, server received = %d", res.BytesWritten, got)
	}
	if res.DurationMs < 1500 || res.DurationMs > 2500 {
		t.Errorf("duration = %dms, want ~2000", res.DurationMs)
	}
}

func TestSegmentCutEndToEnd(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.wav")
	genTone(t, src, 3)

	eng, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seg, err := eng.SegmentCut(context.Background(), src, 1000, 2000)
	if err != nil {
		t.Fatalf("SegmentCut: %v", err)
	}
	if seg.MIMEType != "audio/wav" {
		t.Errorf("mime = %q, want audio/wav", seg.MIMEType)
	}
	// A valid WAV begins with the RIFF magic.
	if len(seg.WAV) < 4 || string(seg.WAV[:4]) != "RIFF" {
		t.Errorf("segment is not a WAV (len=%d)", len(seg.WAV))
	}
}

func TestSegmentCut_EmptyWindowIsInvalid(t *testing.T) {
	eng := &Engine{targetSampleRate: 16000, sem: make(chan struct{}, 1)}
	if _, err := eng.SegmentCut(context.Background(), "https://x", 500, 500); !errors.Is(err, ErrInvalidAudio) {
		t.Fatalf("empty window must be ErrInvalidAudio, got %v", err)
	}
}
