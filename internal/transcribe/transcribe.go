// Package transcribe turns a bounded audio segment into utterances via Gemini
// multimodal (the Gemini Developer API, CON-282). It is deliberately thin: the
// ffmpeg segment cut happens in audioengine; this package only sends the segment
// bytes to Gemini with a structured-output prompt, parses the JSON reply, and
// rebases each utterance's offset onto the ORIGINAL asset timeline.
//
// Per the v1 decision there is no dedicated ASR backend, so per-utterance
// offsets and confidence are model-reported and approximate. When the model
// returns no/zero timestamps for an utterance the transcriber falls back to the
// segment window bounds and marks confidence -1 (recorded as approximate). A
// reply that is not usable JSON at all is an error (ErrDegenerateOutput).
package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// ErrUnavailable is returned when no Gemini API key is configured, so Transcribe
// cannot run. The server maps it to codes.Unavailable (transient: a key can be
// set without a restart).
var ErrUnavailable = errors.New("transcribe: gemini api key not configured")

// ErrDegenerateOutput is returned when Gemini's reply is unusable: it ran into
// the output-token cap (in practice a repetition loop, never a real transcript
// of a bounded segment) or is not valid JSON for the schema. It is transient (sampling is non-deterministic, so a retry usually succeeds);
// the server maps it to codes.Unavailable.
var ErrDegenerateOutput = errors.New("transcribe: model output hit the token cap (degenerate reply)")

// Output budget for a segment, in model tokens. Even fast speech is ~4 text
// tokens/s; the per-utterance JSON roughly triples that. 40 tok/s plus a fixed
// floor is generous headroom while stopping a looping model in seconds rather
// than letting it stream tens of thousands of tokens for minutes.
const (
	outputTokensBase      = 1024
	outputTokensPerSecond = 40
)

// Client wraps a genai.Client for the Gemini Developer API backend.
type Client struct {
	inner *genai.Client
}

// New builds a Gemini Developer API client from apiKey. An empty key yields a
// nil-inner Client whose Transcribe returns ErrUnavailable — Probe/Normalize
// still serve, matching the embedder's key-optional boot in ogen.
func New(ctx context.Context, apiKey string) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return &Client{}, nil
	}
	inner, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("transcribe: new gemini client: %w", err)
	}
	return &Client{inner: inner}, nil
}

// Available reports whether a Gemini key was configured at construction.
func (c *Client) Available() bool { return c != nil && c.inner != nil }

// Segment is the input to Transcribe: the cut audio and the window it was cut
// from (on the ORIGINAL asset timeline, in ms). StartMs is the rebase origin.
type Segment struct {
	Audio        []byte
	MIMEType     string
	StartMs      int64
	EndMs        int64
	LanguageHint string // optional BCP-47; empty = auto-detect
	Model        string // Gemini model id (from the request; never compiled in)
}

// Utterance is one transcribed span on the ORIGINAL asset timeline.
type Utterance struct {
	Text       string
	StartMs    int64
	EndMs      int64
	Confidence float32 // -1 when the model didn't expose one
	Language   string
	IsSpeech   bool
}

// Result is Transcribe's output: the utterances plus Gemini usage.
type Result struct {
	DetectedLanguage string
	Utterances       []Utterance
	InputTokens      int64
	OutputTokens     int64
}

// Transcribe sends seg.Audio to Gemini with a structured-output prompt and
// returns utterances rebased to the original timeline. The model id comes from
// seg.Model; it is never hardcoded. Gemini transport errors propagate as-is so
// the server can classify them (Unavailable / ResourceExhausted /
// DeadlineExceeded).
func (c *Client) Transcribe(ctx context.Context, seg Segment) (*Result, error) {
	if !c.Available() {
		return nil, ErrUnavailable
	}
	if strings.TrimSpace(seg.Model) == "" {
		return nil, fmt.Errorf("transcribe: empty model id")
	}
	if len(seg.Audio) == 0 {
		return nil, fmt.Errorf("transcribe: empty audio segment")
	}

	parts := []*genai.Part{
		genai.NewPartFromText(promptText(seg.LanguageHint)),
		genai.NewPartFromBytes(seg.Audio, orMIME(seg.MIMEType)),
	}
	contents := []*genai.Content{genai.NewContentFromParts(parts, genai.RoleUser)}

	// Temperature is left at the model default: Gemini 3 models are tuned for
	// 1.0, and at temperature 0 they cut the transcript short (observed: one
	// utterance of a 6-sentence clip, end_ms dropped).
	cfg := &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
		ResponseSchema:   responseSchema(seg),
		MaxOutputTokens:  maxOutputTokens(seg),
	}

	resp, err := c.inner.Models.GenerateContent(ctx, seg.Model, contents, cfg)
	if err != nil {
		return nil, err
	}

	in, out := usageTokens(resp)
	if finishReason(resp) == genai.FinishReasonMaxTokens {
		return nil, fmt.Errorf("%w: output_tokens=%d", ErrDegenerateOutput, out)
	}
	parsed, perr := parseModelJSON(resp.Text())
	if perr != nil {
		// An unparseable reply is a model failure, not an empty transcript:
		// returning a blank "OK" here is what hid the integer-loop bug. Surface
		// it as transient so the caller retries.
		return nil, fmt.Errorf("%w: finish_reason=%s output_tokens=%d: %v",
			ErrDegenerateOutput, finishReason(resp), out, perr)
	}

	return &Result{
		DetectedLanguage: parsed.DetectedLanguage,
		Utterances:       rebaseUtterances(parsed.Utterances, seg),
		InputTokens:      in,
		OutputTokens:     out,
	}, nil
}

func orMIME(m string) string {
	if strings.TrimSpace(m) == "" {
		return "audio/wav"
	}
	return m
}

// maxOutputTokens sizes the reply budget to the segment window.
func maxOutputTokens(seg Segment) int32 {
	secs := max((seg.EndMs-seg.StartMs+999)/1000, 1)
	return int32(outputTokensBase + outputTokensPerSecond*secs)
}

// finishReason returns the first candidate's finish reason, or "" when absent.
func finishReason(resp *genai.GenerateContentResponse) genai.FinishReason {
	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0] == nil {
		return ""
	}
	return resp.Candidates[0].FinishReason
}

// usageTokens pulls Gemini's prompt/candidate token counts from the response,
// tolerating a nil UsageMetadata.
func usageTokens(resp *genai.GenerateContentResponse) (in, out int64) {
	if resp == nil || resp.UsageMetadata == nil {
		return 0, 0
	}
	return int64(resp.UsageMetadata.PromptTokenCount), int64(resp.UsageMetadata.CandidatesTokenCount)
}

// promptText builds the transcription instruction, optionally biasing to a
// language hint. It asks for offsets WITHIN the segment (0-based); the server
// rebases by adding the request's start_ms.
func promptText(languageHint string) string {
	var b strings.Builder
	b.WriteString("You are a precise speech transcriber. Transcribe the attached audio segment. ")
	b.WriteString("Return ONLY JSON matching the provided schema. ")
	b.WriteString("Split the transcript into utterances (one per natural sentence or speaker turn). ")
	b.WriteString("For each utterance give start_ms and end_ms as integer millisecond offsets measured from the START of THIS audio segment (the segment begins at 0). ")
	b.WriteString("Set is_speech=false and text=\"\" for a run that contains no speech. ")
	b.WriteString("If you cannot determine timestamps, omit them (use 0) and they will be treated as approximate. ")
	b.WriteString("Set confidence to a 0..1 value when you can estimate it, otherwise -1. ")
	if h := strings.TrimSpace(languageHint); h != "" {
		fmt.Fprintf(&b, "The primary spoken language is likely %q; still report the actual detected language. ", h)
	} else {
		b.WriteString("Auto-detect the spoken language and report it. ")
	}
	return b.String()
}

// responseSchema is the structured-output schema constraining Gemini's reply.
//
// The integer offsets MUST carry minimum/maximum bounds: an unbounded integer
// lets constrained decoding emit digits forever, and Gemini 3 models fall into
// exactly that loop ("start_ms": 6000000…) until the token cap, on clean
// speech. Bounding them to the window length also keeps offsets in range. The
// timestamps are ordered (and required) ahead of the text so the model commits
// to the span before transcribing it.
func responseSchema(seg Segment) *genai.Schema {
	windowMs := float64(max(seg.EndMs-seg.StartMs, 0))
	offset := func() *genai.Schema {
		return &genai.Schema{Type: genai.TypeInteger, Minimum: genai.Ptr(0.0), Maximum: genai.Ptr(windowMs)}
	}
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"detected_language": {Type: genai.TypeString},
			"utterances": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"start_ms":   offset(),
						"end_ms":     offset(),
						"text":       {Type: genai.TypeString},
						"is_speech":  {Type: genai.TypeBoolean},
						"language":   {Type: genai.TypeString},
						"confidence": {Type: genai.TypeNumber, Minimum: genai.Ptr(-1.0), Maximum: genai.Ptr(1.0)},
					},
					PropertyOrdering: utteranceFields,
					Required:         utteranceFields,
				},
			},
		},
		PropertyOrdering: []string{"detected_language", "utterances"},
		Required:         []string{"detected_language", "utterances"},
	}
}

var utteranceFields = []string{"start_ms", "end_ms", "text", "is_speech", "language", "confidence"}

// modelReply is the shape parseModelJSON decodes Gemini's JSON into.
type modelReply struct {
	DetectedLanguage string           `json:"detected_language"`
	Utterances       []modelUtterance `json:"utterances"`
}

type modelUtterance struct {
	Text       string   `json:"text"`
	StartMs    *int64   `json:"start_ms"`
	EndMs      *int64   `json:"end_ms"`
	Confidence *float32 `json:"confidence"`
	Language   string   `json:"language"`
	IsSpeech   *bool    `json:"is_speech"`
}

// parseModelJSON decodes the model's JSON reply, tolerating markdown code fences
// some models still wrap around JSON despite responseMIMEType=application/json.
func parseModelJSON(s string) (*modelReply, error) {
	s = stripCodeFence(strings.TrimSpace(s))
	if s == "" {
		return nil, fmt.Errorf("transcribe: empty model reply")
	}
	var r modelReply
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, fmt.Errorf("transcribe: decode model reply: %w", err)
	}
	return &r, nil
}

// stripCodeFence removes a leading ```json / ``` fence and trailing ``` if the
// model wrapped its JSON in one.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the first fence line (``` or ```json) and any trailing fence.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}
