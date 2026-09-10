package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// Unset (not empty) so envconfig applies the struct-tag defaults — an
	// empty-but-set var is a value envconfig tries to parse.
	for _, k := range []string{
		"AUDIO_SERVICE_LISTEN", "AUDIO_SERVICE_WORKERS", "PROBE_TIMEOUT",
		"NORMALIZE_TIMEOUT", "TRANSCRIBE_TIMEOUT", "FFPROBE_PATH", "FFMPEG_PATH",
		"TARGET_SAMPLE_RATE", "TRANSCRIBE_MODEL",
	} {
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != ":50051" {
		t.Errorf("Listen default = %q, want :50051", c.Listen)
	}
	if c.Workers != 4 {
		t.Errorf("Workers default = %d, want 4", c.Workers)
	}
	if c.FFprobePath != "ffprobe" || c.FFmpegPath != "ffmpeg" {
		t.Errorf("binary defaults = %q/%q, want ffprobe/ffmpeg", c.FFprobePath, c.FFmpegPath)
	}
	if c.ProbeTimeout.Seconds() != 90 {
		t.Errorf("ProbeTimeout default = %v, want 90s", c.ProbeTimeout)
	}
	if c.TargetSampleRate != 16000 {
		t.Errorf("TargetSampleRate default = %d, want 16000", c.TargetSampleRate)
	}
	if c.TranscribeModel != "gemini-2.5-flash" {
		t.Errorf("TranscribeModel default = %q, want gemini-2.5-flash", c.TranscribeModel)
	}
}

func TestLoadNormalisesBarePort(t *testing.T) {
	t.Setenv("AUDIO_SERVICE_LISTEN", "50051")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != ":50051" {
		t.Errorf("bare port not normalised: got %q, want :50051", c.Listen)
	}
}
