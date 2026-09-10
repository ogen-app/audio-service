package transcribe

import (
	"context"
	"testing"
)

func i64(v int64) *int64     { return &v }
func f32(v float32) *float32 { return &v }
func b(v bool) *bool         { return &v }

func TestRebaseUtterances_AddsSegmentStart(t *testing.T) {
	seg := Segment{StartMs: 10_000, EndMs: 20_000, LanguageHint: "en"}
	in := []modelUtterance{
		{Text: "hello", StartMs: i64(0), EndMs: i64(1500), Confidence: f32(0.9), Language: "en", IsSpeech: b(true)},
		{Text: "world", StartMs: i64(2000), EndMs: i64(3000), Language: "en", IsSpeech: b(true)},
	}
	got := rebaseUtterances(in, seg)
	if len(got) != 2 {
		t.Fatalf("want 2 utterances, got %d", len(got))
	}
	// First: 0..1500 within segment → 10000..11500 on the original timeline.
	if got[0].StartMs != 10_000 || got[0].EndMs != 11_500 {
		t.Errorf("first rebased to %d..%d, want 10000..11500", got[0].StartMs, got[0].EndMs)
	}
	if got[0].Confidence != 0.9 {
		t.Errorf("confidence = %v, want 0.9", got[0].Confidence)
	}
	// Second: 2000..3000 → 12000..13000.
	if got[1].StartMs != 12_000 || got[1].EndMs != 13_000 {
		t.Errorf("second rebased to %d..%d, want 12000..13000", got[1].StartMs, got[1].EndMs)
	}
	// No confidence given → -1.
	if got[1].Confidence != -1 {
		t.Errorf("missing confidence must be -1, got %v", got[1].Confidence)
	}
}

func TestRebaseUtterances_MissingTimestampsFallBackToWindow(t *testing.T) {
	seg := Segment{StartMs: 5_000, EndMs: 8_000}
	// No start/end offsets at all → approximate: span the whole window, conf -1.
	in := []modelUtterance{{Text: "mumble", IsSpeech: b(true)}}
	got := rebaseUtterances(in, seg)
	if len(got) != 1 {
		t.Fatalf("want 1 utterance, got %d", len(got))
	}
	if got[0].StartMs != 5_000 || got[0].EndMs != 8_000 {
		t.Errorf("fallback window = %d..%d, want 5000..8000", got[0].StartMs, got[0].EndMs)
	}
	if got[0].Confidence != -1 {
		t.Errorf("approximate utterance must be conf -1, got %v", got[0].Confidence)
	}
}

func TestRebaseUtterances_ClampsOverrunToWindow(t *testing.T) {
	seg := Segment{StartMs: 1_000, EndMs: 2_000}
	// Model claims 5000ms into the segment — past the 1000ms window. Clamp to end.
	in := []modelUtterance{{Text: "over", StartMs: i64(500), EndMs: i64(5000), IsSpeech: b(true)}}
	got := rebaseUtterances(in, seg)
	if got[0].StartMs != 1_500 {
		t.Errorf("start = %d, want 1500", got[0].StartMs)
	}
	if got[0].EndMs != 2_000 { // clamped to seg.EndMs
		t.Errorf("end = %d, want clamped to 2000", got[0].EndMs)
	}
}

func TestRebaseUtterances_EmptyInputYieldsFallback(t *testing.T) {
	seg := Segment{StartMs: 0, EndMs: 4_000, LanguageHint: "de"}
	got := rebaseUtterances(nil, seg)
	if len(got) != 1 || got[0].IsSpeech {
		t.Fatalf("empty input must yield one non-speech fallback, got %+v", got)
	}
	if got[0].StartMs != 0 || got[0].EndMs != 4_000 || got[0].Confidence != -1 {
		t.Errorf("fallback = %+v", got[0])
	}
	if got[0].Language != "de" {
		t.Errorf("fallback language should carry the hint, got %q", got[0].Language)
	}
}

func TestRebaseUtterances_IsSpeechDefaultsTrue(t *testing.T) {
	seg := Segment{StartMs: 0, EndMs: 1_000}
	in := []modelUtterance{{Text: "hi", StartMs: i64(100), EndMs: i64(200)}} // is_speech nil
	got := rebaseUtterances(in, seg)
	if !got[0].IsSpeech {
		t.Errorf("text-bearing utterance with nil is_speech should default to speech")
	}
}

func TestParseModelJSON(t *testing.T) {
	raw := `{"detected_language":"en","utterances":[{"text":"hi","start_ms":0,"end_ms":500,"is_speech":true}]}`
	r, err := parseModelJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.DetectedLanguage != "en" || len(r.Utterances) != 1 || r.Utterances[0].Text != "hi" {
		t.Errorf("parsed wrong: %+v", r)
	}
}

func TestParseModelJSON_StripsCodeFence(t *testing.T) {
	raw := "```json\n{\"detected_language\":\"fr\",\"utterances\":[]}\n```"
	r, err := parseModelJSON(raw)
	if err != nil {
		t.Fatalf("parse fenced: %v", err)
	}
	if r.DetectedLanguage != "fr" {
		t.Errorf("detected_language = %q, want fr", r.DetectedLanguage)
	}
}

func TestParseModelJSON_EmptyIsError(t *testing.T) {
	if _, err := parseModelJSON("   "); err == nil {
		t.Error("empty reply must be an error")
	}
	if _, err := parseModelJSON("not json at all"); err == nil {
		t.Error("garbage reply must be an error")
	}
}

func TestTranscribe_UnavailableWithoutKey(t *testing.T) {
	c := &Client{} // nil inner = no key
	if c.Available() {
		t.Fatal("client with nil inner must report unavailable")
	}
	_, err := c.Transcribe(context.Background(), Segment{Audio: []byte{1}, Model: "gemini-2.5-flash"})
	if err != ErrUnavailable {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestNew_EmptyKeyIsUnavailableNotError(t *testing.T) {
	c, err := New(context.Background(), "")
	if err != nil {
		t.Fatalf("empty key must not error at construction: %v", err)
	}
	if c.Available() {
		t.Error("empty key must yield an unavailable client")
	}
}
