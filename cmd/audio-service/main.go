// Command audio-service is the gRPC audio transcode+transcribe microservice
// (CON-282). It serves audio.v1.AudioService (Probe / Normalize /
// TranscribeSegment) backed by ffmpeg/ffprobe and Gemini multimodal, plus a
// standard grpc.health.v1 endpoint. It is internal-only — the Ogen API reaches
// it over the private network.
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	audiov1 "github.com/ogen-app/audio-service/gen/audio/v1"
	"github.com/ogen-app/audio-service/internal/audioengine"
	"github.com/ogen-app/audio-service/internal/config"
	"github.com/ogen-app/audio-service/internal/logging"
	"github.com/ogen-app/audio-service/internal/runtimetune"
	"github.com/ogen-app/audio-service/internal/server"
	"github.com/ogen-app/audio-service/internal/transcribe"
)

// serviceName is the health-check service key for AudioService (registered
// alongside the "" overall-server key so orchestrators can probe either).
const serviceName = "audio.v1.AudioService"

func main() {
	cfg, err := config.Load()
	if err != nil {
		// Pre-logger: config drives the logger's level/format, so a load
		// failure can only report through the stdlib default (CON-107 keeps
		// boot log.Fatal*).
		log.Fatalf("audio-service: config: %v", err)
	}

	logger := logging.New(cfg)

	// Bound the heap to the container and return burst-freed memory to the OS so
	// RSS tracks real usage instead of holding a high-water mark.
	runtimetune.Apply(logger, cfg)

	engine, err := audioengine.New(audioengine.Config{
		FFprobePath:       cfg.FFprobePath,
		FFmpegPath:        cfg.FFmpegPath,
		Workers:           cfg.Workers,
		ProbeTimeout:      cfg.ProbeTimeout,
		NormalizeTimeout:  cfg.NormalizeTimeout,
		TranscribeTimeout: cfg.TranscribeTimeout,
		TargetSampleRate:  cfg.TargetSampleRate,
		ScavengeOnIdle:    cfg.ScavengeOnIdle,
	})
	if err != nil {
		logger.Error("init engine", "component", "boot", "err", err)
		os.Exit(1)
	}
	defer engine.Close()

	// The transcriber is key-optional: with no GEMINI_API_KEY, Probe/Normalize
	// still serve and TranscribeSegment returns Unavailable until a key is set.
	transcriber, err := transcribe.New(context.Background(), cfg.GeminiAPIKey)
	if err != nil {
		logger.Error("init transcriber", "component", "boot", "err", err)
		os.Exit(1)
	}
	if !transcriber.Available() {
		logger.Warn("no gemini api key configured; TranscribeSegment will return Unavailable",
			"component", "boot")
	}

	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Error("listen", "component", "boot", "addr", cfg.Listen, "err", err)
		os.Exit(1)
	}

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(logging.UnaryServerInterceptor(logger)),
		grpc.ChainStreamInterceptor(logging.StreamServerInterceptor(logger)),
	)
	audiov1.RegisterAudioServiceServer(srv, server.New(engine, transcriber, cfg.TranscribeModel))

	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	hs.SetServingStatus(serviceName, healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logger.Info("shutting down", "component", "boot")
		srv.GracefulStop()
	}()

	logger.Info("listening", "component", "boot", "addr", cfg.Listen, "workers", cfg.Workers)
	if err := srv.Serve(lis); err != nil {
		logger.Error("serve", "component", "boot", "err", err)
		os.Exit(1)
	}
}
