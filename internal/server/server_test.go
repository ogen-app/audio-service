package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	audiov1 "github.com/ogen-app/audio-service/gen/audio/v1"
	"github.com/ogen-app/audio-service/internal/audioengine"
	"github.com/ogen-app/audio-service/internal/transcribe"
)

func TestProbe_EmptyURLIsInvalidArgument(t *testing.T) {
	// The url check short-circuits before the engine is touched, so a nil
	// engine is safe here.
	s := New(nil, nil, "")
	_, err := s.Probe(context.Background(), &audiov1.ProbeRequest{SourceUrl: "   "})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty source_url must be InvalidArgument, got %v", err)
	}
}

func TestNormalize_MissingURLsAreInvalidArgument(t *testing.T) {
	s := New(nil, nil, "")
	if _, err := s.Normalize(context.Background(), &audiov1.NormalizeRequest{DestPutUrl: "https://x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing source_url must be InvalidArgument, got %v", err)
	}
	if _, err := s.Normalize(context.Background(), &audiov1.NormalizeRequest{SourceUrl: "https://x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing dest_put_url must be InvalidArgument, got %v", err)
	}
}

func TestTranscribeSegment_ValidatesWindowAndURL(t *testing.T) {
	s := New(nil, unavailableTranscriber{}, "gemini-2.5-flash")
	if _, err := s.TranscribeSegment(context.Background(), &audiov1.TranscribeSegmentRequest{StartMs: 0, EndMs: 100}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty normalized_url must be InvalidArgument, got %v", err)
	}
	if _, err := s.TranscribeSegment(context.Background(), &audiov1.TranscribeSegmentRequest{NormalizedUrl: "https://x", StartMs: 100, EndMs: 100}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("end_ms<=start_ms must be InvalidArgument, got %v", err)
	}
}

func TestTranscribeSegment_UnavailableWhenNoKey(t *testing.T) {
	s := New(nil, unavailableTranscriber{}, "gemini-2.5-flash")
	_, err := s.TranscribeSegment(context.Background(), &audiov1.TranscribeSegmentRequest{
		NormalizedUrl: "https://x", StartMs: 0, EndMs: 1000,
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("no transcriber key must be Unavailable, got %v", err)
	}
}

type unavailableTranscriber struct{}

func (unavailableTranscriber) Available() bool { return false }
func (unavailableTranscriber) Transcribe(context.Context, transcribe.Segment) (*transcribe.Result, error) {
	return nil, transcribe.ErrUnavailable
}

func TestMapEngineErr(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want codes.Code
	}{
		{"invalid audio", fmt.Errorf("%w: no audio stream", audioengine.ErrInvalidAudio), codes.InvalidArgument},
		{"silent", fmt.Errorf("%w: whole file", audioengine.ErrSilent), codes.InvalidArgument},
		{"normalize failure", fmt.Errorf("%w: ffmpeg exited 1", audioengine.ErrNormalize), codes.Internal},
		{"deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), codes.DeadlineExceeded},
		{"canceled", fmt.Errorf("wrap: %w", context.Canceled), codes.Canceled},
		{"transient", errors.New("ffprobe failed: connection refused"), codes.Internal},
	}
	for _, tc := range cases {
		if got := status.Code(mapEngineErr(tc.in)); got != tc.want {
			t.Errorf("%s: mapEngineErr code = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMapTranscribeErr(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want codes.Code
	}{
		{"unavailable key", transcribe.ErrUnavailable, codes.Unavailable},
		{"deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), codes.DeadlineExceeded},
		{"canceled", fmt.Errorf("wrap: %w", context.Canceled), codes.Canceled},
		{"gemini 429", genai.APIError{Code: 429, Message: "rate limited"}, codes.ResourceExhausted},
		{"gemini 503", genai.APIError{Code: 503, Message: "unavailable"}, codes.Unavailable},
		{"gemini 500", genai.APIError{Code: 500, Message: "internal"}, codes.Unavailable},
		{"gemini 504", genai.APIError{Code: 504, Message: "gateway timeout"}, codes.DeadlineExceeded},
		{"gemini 400", genai.APIError{Code: 400, Message: "bad request"}, codes.Internal},
		{"other", errors.New("some transport blip"), codes.Internal},
	}
	for _, tc := range cases {
		if got := status.Code(mapTranscribeErr(tc.in)); got != tc.want {
			t.Errorf("%s: mapTranscribeErr code = %v, want %v", tc.name, got, tc.want)
		}
	}
}
