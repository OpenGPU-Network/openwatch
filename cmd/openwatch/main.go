package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/openwatch/openwatch/internal/api"
	"github.com/openwatch/openwatch/internal/config"
	"github.com/openwatch/openwatch/internal/docker"
	"github.com/openwatch/openwatch/internal/metrics"
	"github.com/openwatch/openwatch/internal/notify"
	"github.com/openwatch/openwatch/internal/updater"
)

// Version is the build-time version string. Set via ldflags:
//
//	go build -ldflags="-X main.Version=1.2.3" ./cmd/openwatch
//
// The default "dev" value is returned by the /health endpoint and
// logged at startup when no ldflag is supplied, so development
// builds stay obviously distinguishable from release artifacts.
var Version = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("load config")
	}

	logger := buildLogger(cfg.LogLevel, cfg.LogFormat)
	logger.Info().
		Str("version", Version).
		Int("interval", cfg.Interval).
		Str("schedule", cfg.Schedule).
		Bool("cleanup", cfg.Cleanup).
		Bool("label_enable", cfg.LabelEnable).
		Bool("rollback_on_failure", cfg.RollbackOnFailure).
		Bool("http_api", cfg.HTTPAPI).
		Int("healthcheck_timeout", cfg.HealthcheckTimeout).
		Int("stop_timeout", cfg.StopTimeout).
		Str("log_level", cfg.LogLevel).
		Msg("openwatch starting")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Daemon-level Docker client. Ping happens inside NewClient so we fail
	// fast on a misconfigured DOCKER_HOST.
	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	cli, err := docker.NewClient(pingCtx)
	pingCancel()
	if err != nil {
		logger.Fatal().Err(err).Msg("docker client init failed")
	}
	defer cli.Close()

	// Construct the notifier from the configured URL. An empty URL
	// degrades to a NoopNotifier inside notify.New — that path emits a
	// single debug line at startup, so the operator can see in logs
	// that notifications are intentionally off. A non-empty URL that
	// fails to parse is a hard configuration error and halts startup
	// here with a sanitized message (the raw URL is never echoed).
	notifier, err := notify.New(cfg.NotifyURL, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("notifier init failed")
	}

	// Prometheus metrics register against a dedicated registry so
	// OpenWatch's /metrics endpoint only exposes its own counters —
	// never anything third-party libraries may have pushed into the
	// global default registry.
	mtx, err := metrics.New()
	if err != nil {
		logger.Fatal().Err(err).Msg("metrics init failed")
	}

	// Thread-safe in-memory container state store. The watcher writes
	// to it during every tick; the HTTP API reads from it via
	// Watcher.State(). One instance per daemon.
	state := updater.NewStateStore()

	w := updater.New(cfg, cli, logger, notifier, state, mtx)

	// Install signal handlers for graceful shutdown. The single ctx
	// covers both the watcher and the (optional) HTTP API — cancelling
	// it once makes every goroutine drain in lock step.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sig
		logger.Info().Str("signal", s.String()).Msg("shutdown requested")
		cancel()
	}()

	// Optional HTTP API. Runs in its own goroutine so Serve's block
	// on the listener does not stop us from also running the
	// watcher loop below. Both share the same ctx, so either a
	// watcher crash or a signal tears the whole daemon down.
	if cfg.HTTPAPI {
		apiServer := api.New(api.Config{
			Addr:    ":8080",
			Trigger: w,
			Metrics: mtx,
			Version: Version,
			Log:     logger,
		})
		go func() {
			if err := apiServer.Serve(ctx); err != nil {
				logger.Error().Err(err).Msg("http api exited with error")
				cancel()
			}
		}()
	}

	if err := w.Run(ctx); err != nil {
		logger.Fatal().Err(err).Msg("watcher exited with error")
	}
	logger.Info().Msg("openwatch stopped")
}

func buildLogger(levelStr, format string) zerolog.Logger {
	level, err := zerolog.ParseLevel(strings.ToLower(levelStr))
	if err != nil || levelStr == "" {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	var l zerolog.Logger
	if strings.ToLower(format) == "json" {
		l = zerolog.New(os.Stdout).With().Timestamp().Logger()
	} else {
		l = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}).With().Timestamp().Logger()
	}
	return l.Level(level)
}
