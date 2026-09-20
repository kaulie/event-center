// Command eventd runs the event center: webhook ingestion, the event log and
// the push dispatcher.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kaulie/event-center/internal/api"
	"github.com/kaulie/event-center/internal/config"
	"github.com/kaulie/event-center/internal/dispatch"
	"github.com/kaulie/event-center/internal/metrics"
	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/service"
	"github.com/kaulie/event-center/internal/store"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "eventd:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// Reported here, not in config: Load runs before this handler exists.
	if cfg.HTTPAddrWarning != "" {
		log.Warn("listen address fallback",
			"detail", cfg.HTTPAddrWarning, "addr", cfg.HTTPAddr, "addr_source", cfg.HTTPAddrSource)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	if err := seedSources(ctx, st, cfg, log); err != nil {
		return err
	}

	reg := metrics.New()
	svc := service.New(st, reg, log)

	if !cfg.DispatcherDisabled && cfg.PushEnabled {
		d := dispatch.New(st, dispatch.Config{
			BatchSize:    cfg.PushBatchSize,
			MaxAttempts:  cfg.PushMaxAttempts,
			BaseBackoff:  cfg.PushBaseBackoff,
			MaxBackoff:   cfg.PushMaxBackoff,
			PollInterval: cfg.PushPollInterval,
			Timeout:      cfg.PushTimeout,
		}, reg, log)
		go d.Run(ctx)
		log.Info("push dispatcher started",
			"batch_size", cfg.PushBatchSize, "max_attempts", cfg.PushMaxAttempts)
	} else {
		log.Warn("push dispatcher disabled")
	}

	if cfg.RetentionDays > 0 {
		go runJanitor(ctx, st, time.Duration(cfg.RetentionDays)*24*time.Hour, log)
	}

	srv := api.New(svc, reg, log, api.Config{
		AdminToken:        cfg.AdminToken,
		PullDefaultSize:   cfg.PullDefaultSize,
		PullMaxSize:       cfg.PullMaxSize,
		PullWaitMax:       cfg.PullWaitMax,
		Version:           version,
		IngressLogBody:    cfg.IngressLogBody,
		IngressLogBodyMax: cfg.IngressLogBodyMax,
		IngressLogPath:    cfg.IngressLogPath,
	})
	defer func() { _ = srv.Close() }()
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("event center listening", "addr", cfg.HTTPAddr, "addr_source", cfg.HTTPAddrSource,
			"version", version, "db", cfg.DBPath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// seedSources registers the built-in GitHub source the first time the service
// starts with a secret configured. It never overwrites an existing row, so an
// operator edit through the admin API survives restarts.
func seedSources(ctx context.Context, st *store.Store, cfg *config.Config, log *slog.Logger) error {
	if _, err := st.GetSource(ctx, "github"); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	if cfg.GitHubSecret == "" {
		log.Warn("github source not seeded: set EVENTD_GITHUB_SECRET or create it via the admin API")
		return nil
	}
	src := &model.Source{
		ID:            "github",
		Kind:          "webhook",
		Secret:        cfg.GitHubSecret,
		VerifyMode:    model.VerifyHMACSHA256,
		TypePrefix:    "github",
		DefaultStream: "github",
		Enabled:       true,
	}
	if err := st.UpsertSource(ctx, src); err != nil {
		return err
	}
	log.Info("seeded github source", "stream", src.DefaultStream, "verify_mode", src.VerifyMode)
	return nil
}

// runJanitor drops events older than the retention window once a day.
func runJanitor(ctx context.Context, st *store.Store, retention time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().UTC().Add(-retention)
			n, err := st.PurgeOlderThan(ctx, cutoff)
			if err != nil {
				log.Error("retention purge failed", "error", err)
				continue
			}
			if n > 0 {
				log.Info("retention purge finished", "deleted", n, "cutoff", cutoff)
			}
		}
	}
}
