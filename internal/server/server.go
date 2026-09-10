// Package server implements the audio.v1.AudioService gRPC service on top of the
// ffmpeg/ffprobe engine (Probe/Normalize/segment cut) and the Gemini transcriber.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	audiov1 "github.com/ogen-app/audio-service/gen/audio/v1"
	"github.com/ogen-app/audio-service/internal/audioengine"
	"github.com/ogen-app/audio-service/internal/transcribe"
)

// Transcriber is the subset of the Gemini client the server needs, narrowed to
// an interface so tests can substitute a fake without a real API key.
type Transcriber interface {
	Available() bool
	Transcribe(ctx context.Context, seg transcribe.Segment) (*transcribe.Result, error)
}

// Server implements audiov1.AudioServiceServer.
type Server struct {
	audiov1.UnimplementedAudioServiceServer
	engine       *audioengine.Engine
	transcriber  Transcriber
	defaultModel string
}

// New wires the server with its engine and transcriber. defaultModel is the
// fallback Gemini model id used when a TranscribeSegment request leaves model
// empty (the request field still wins when set).
func New(engine *audioengine.Engine, transcriber Transcriber, defaultModel string) *Server {
	return &Server{engine: engine, transcriber: transcriber, defaultModel: defaultModel}
}

// Probe reads the audio at source_url and returns its metadata plus a
// silent/zero-length verdict. Undecodable or silent input is a terminal
// InvalidArgument; transient faults are Internal so the caller can degrade.
func (s *Server) Probe(ctx context.Context, req *audiov1.ProbeRequest) (*audiov1.ProbeResponse, error) {
	url := strings.TrimSpace(req.GetSourceUrl())
	if url == "" {
		return nil, status.Error(codes.InvalidArgument, "source_url is required")
	}

	res, err := s.engine.Probe(ctx, url)
	if err != nil {
		return nil, mapEngineErr(err)
	}

	slog.InfoContext(ctx, "probe complete",
		"component", "server.probe",
		"filename", req.GetFilename(),
		"duration_ms", res.DurationMs,
		"codec", res.Codec,
		"container", res.Container,
		"channels", res.Channels,
		"sample_rate", res.SampleRate,
		"silent", res.Silent,
	)
	return &audiov1.ProbeResponse{
		DurationMs: res.DurationMs,
		Channels:   int32(res.Channels),
		SampleRate: int32(res.SampleRate),
		Container:  res.Container,
		Codec:      res.Codec,
		Silent:     res.Silent,
	}, nil
}

// Normalize transcodes source_url to mono @ target_sample_rate Opus and streams
// it to dest_put_url, returning the derivative's metadata. A transcode/upload
// failure is Internal (transient, retry the whole step).
func (s *Server) Normalize(ctx context.Context, req *audiov1.NormalizeRequest) (*audiov1.NormalizeResponse, error) {
	src := strings.TrimSpace(req.GetSourceUrl())
	dst := strings.TrimSpace(req.GetDestPutUrl())
	if src == "" {
		return nil, status.Error(codes.InvalidArgument, "source_url is required")
	}
	if dst == "" {
		return nil, status.Error(codes.InvalidArgument, "dest_put_url is required")
	}

	res, err := s.engine.Normalize(ctx, src, dst, int(req.GetTargetSampleRate()))
	if err != nil {
		return nil, mapEngineErr(err)
	}

	slog.InfoContext(ctx, "normalize complete",
		"component", "server.normalize",
		"duration_ms", res.DurationMs,
		"sample_rate", res.SampleRate,
		"channels", res.Channels,
		"bytes_written", res.BytesWritten,
	)
	return &audiov1.NormalizeResponse{
		DurationMs:   res.DurationMs,
		SampleRate:   int32(res.SampleRate),
		Channels:     int32(res.Channels),
		BytesWritten: res.BytesWritten,
	}, nil
}

// TranscribeSegment cuts [start_ms, end_ms) from the normalized derivative and
// transcribes it via Gemini, returning utterances rebased to the original
// timeline plus Gemini usage. Gemini transport faults map to
// Unavailable/ResourceExhausted/DeadlineExceeded (transient).
func (s *Server) TranscribeSegment(ctx context.Context, req *audiov1.TranscribeSegmentRequest) (*audiov1.TranscribeSegmentResponse, error) {
	url := strings.TrimSpace(req.GetNormalizedUrl())
	if url == "" {
		return nil, status.Error(codes.InvalidArgument, "normalized_url is required")
	}
	if req.GetEndMs() <= req.GetStartMs() {
		return nil, status.Error(codes.InvalidArgument, "end_ms must be greater than start_ms")
	}
	if !s.transcriber.Available() {
		return nil, status.Error(codes.Unavailable, "transcription backend not configured (no gemini api key)")
	}

	model := strings.TrimSpace(req.GetModel())
	if model == "" {
		model = s.defaultModel
	}
	if model == "" {
		return nil, status.Error(codes.InvalidArgument, "model is required (no default configured)")
	}

	// Cut the segment from the normalized derivative (ffmpeg range-read).
	seg, err := s.engine.SegmentCut(ctx, url, req.GetStartMs(), req.GetEndMs())
	if err != nil {
		return nil, mapEngineErr(err)
	}

	// Transcribe the cut with Gemini; utterances come back on the original
	// timeline (rebased by the transcriber using StartMs).
	res, err := s.transcriber.Transcribe(ctx, transcribe.Segment{
		Audio:        seg.WAV,
		MIMEType:     seg.MIMEType,
		StartMs:      req.GetStartMs(),
		EndMs:        req.GetEndMs(),
		LanguageHint: strings.TrimSpace(req.GetLanguageHint()),
		Model:        model,
	})
	if err != nil {
		return nil, mapTranscribeErr(err)
	}

	utterances := make([]*audiov1.Utterance, 0, len(res.Utterances))
	for _, u := range res.Utterances {
		utterances = append(utterances, &audiov1.Utterance{
			Text:       u.Text,
			StartMs:    u.StartMs,
			EndMs:      u.EndMs,
			Confidence: u.Confidence,
			Language:   u.Language,
			IsSpeech:   u.IsSpeech,
		})
	}

	slog.InfoContext(ctx, "transcribe segment complete",
		"component", "server.transcribe",
		"start_ms", req.GetStartMs(),
		"end_ms", req.GetEndMs(),
		"model", model,
		"detected_language", res.DetectedLanguage,
		"utterances", len(utterances),
		"input_tokens", res.InputTokens,
		"output_tokens", res.OutputTokens,
	)
	return &audiov1.TranscribeSegmentResponse{
		DetectedLanguage: res.DetectedLanguage,
		Utterances:       utterances,
		InputTokens:      res.InputTokens,
		OutputTokens:     res.OutputTokens,
	}, nil
}

// mapEngineErr classifies an engine (ffmpeg/ffprobe) error into a gRPC status.
// Terminal content verdicts (unsupported/corrupt container, zero-length, silent)
// are InvalidArgument so the client doesn't retry. A normalization failure is a
// distinct Internal (transient). A context deadline/cancel surfaces as its own
// code so callers see the timeout.
func mapEngineErr(err error) error {
	switch {
	case errors.Is(err, audioengine.ErrInvalidAudio), errors.Is(err, audioengine.ErrSilent):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, audioengine.ErrNormalize):
		return status.Error(codes.Internal, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// mapTranscribeErr classifies a Gemini transcription error. An unconfigured key
// is Unavailable; HTTP 429 / 5xx map to ResourceExhausted / Unavailable
// (transient); a deadline surfaces as DeadlineExceeded. Anything else is
// Internal.
func mapTranscribeErr(err error) error {
	if errors.Is(err, transcribe.ErrUnavailable) {
		return status.Error(codes.Unavailable, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	switch code := geminiHTTPStatus(err); {
	case code == http.StatusTooManyRequests:
		return status.Error(codes.ResourceExhausted, err.Error())
	case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout:
		// 408/504 are timeouts (check before the general 5xx range so 504 doesn't
		// fall through to Unavailable).
		return status.Error(codes.DeadlineExceeded, err.Error())
	case code >= 500 && code <= 599:
		return status.Error(codes.Unavailable, err.Error())
	case code == http.StatusBadRequest || code == http.StatusUnprocessableEntity:
		// A malformed request to Gemini is our bug, not the caller's; surface as
		// Internal rather than pushing InvalidArgument back to the API.
		return status.Error(codes.Internal, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

// geminiHTTPStatus extracts the HTTP status code from a genai.APIError, or 0 if
// the error isn't one. genai.APIError is a value type, so errors.As targets a
// non-pointer variable of it.
func geminiHTTPStatus(err error) int {
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return 0
}
