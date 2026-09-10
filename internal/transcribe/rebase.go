package transcribe

// rebaseUtterances converts the model's segment-relative utterances into
// utterances on the ORIGINAL asset timeline by adding seg.StartMs to each
// offset, and normalises the optional/approximate fields:
//
//   - A missing or non-positive timestamp (nil, or <=0) falls back to the
//     corresponding segment window bound (start->seg.StartMs, end->seg.EndMs).
//   - A missing confidence becomes -1 (Gemini rarely exposes one); a reported
//     confidence is preserved even when a timestamp had to be inferred.
//   - A missing is_speech defaults to true (text-bearing utterances are speech).
//   - Rebased offsets are clamped into [seg.StartMs, seg.EndMs] so a model that
//     over-runs the window can't emit an offset outside the segment it was given.
func rebaseUtterances(in []modelUtterance, seg Segment) []Utterance {
	if len(in) == 0 {
		return fallbackUtterances(seg)
	}
	out := make([]Utterance, 0, len(in))
	for _, u := range in {
		out = append(out, rebaseOne(u, seg))
	}
	return out
}

func rebaseOne(u modelUtterance, seg Segment) Utterance {
	// Rebase start: model offset is relative to the segment start. A missing or
	// non-positive start falls back to the segment start (approximate).
	start := seg.StartMs
	if u.StartMs != nil && *u.StartMs > 0 {
		start = seg.StartMs + *u.StartMs
	}

	// Rebase end: fall back to the segment end when absent/degenerate.
	end := seg.EndMs
	if u.EndMs != nil && *u.EndMs > 0 {
		end = seg.StartMs + *u.EndMs
	}

	start = clamp(start, seg.StartMs, seg.EndMs)
	end = clamp(end, seg.StartMs, seg.EndMs)
	if end < start {
		end = start
	}

	// Keep the model's confidence when it reported one; -1 marks "not exposed"
	// (the common case — Gemini rarely reports a per-utterance confidence).
	confidence := float32(-1)
	if u.Confidence != nil {
		confidence = *u.Confidence
	}

	isSpeech := true
	if u.IsSpeech != nil {
		isSpeech = *u.IsSpeech
	}

	return Utterance{
		Text:       u.Text,
		StartMs:    start,
		EndMs:      end,
		Confidence: confidence,
		Language:   u.Language,
		IsSpeech:   isSpeech,
	}
}

// fallbackUtterances is returned when the model gives nothing usable: a single
// approximate, non-speech-agnostic utterance spanning the whole segment window,
// marked approximate (confidence -1). Text is empty because we have no reliable
// transcript to attribute.
func fallbackUtterances(seg Segment) []Utterance {
	return []Utterance{{
		Text:       "",
		StartMs:    seg.StartMs,
		EndMs:      seg.EndMs,
		Confidence: -1,
		Language:   seg.LanguageHint,
		IsSpeech:   false,
	}}
}

func clamp(v, lo, hi int64) int64 {
	return min(max(v, lo), hi)
}
