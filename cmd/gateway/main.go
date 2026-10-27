package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/app"
	"github.com/luca/llm-protocol-gateway/internal/gateway"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	rt := app.New()
	// Headless mode: prefer GATEWAY_ADDR when set; otherwise expose on all
	// interfaces by default so existing LaunchAgent / LAN setups keep working.
	cfg := app.Config{
		Port:          app.DefaultPort,
		PreferEnvAddr: true,
		// Stamped at build time by scripts/build-version.sh; empty for a plain
		// `go run`, in which case the console never offers an upgrade.
		Version: gateway.BuildVersion,
		// PreferEnvAddr also selects the headless first-run default (webExposed=true)
		// when SQLite has no webExposed setting yet.
	}
	if err := rt.Start(cfg); err != nil {
		slog.Error("gateway start failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	// Log the entry point: without it a restart during a long maintenance pass
	// shows nothing between "Stopping…" and the kill, which reads as a hung deploy.
	slog.Info("shutdown requested")
	// Last-resort guarantee that the process leaves: Stop bounds every step it
	// takes, but a wait on the runtime lock itself cannot be interrupted from
	// inside. Exiting here lands no more than the deadline already cost; being
	// SIGKILLed at TimeoutStopSec loses the whole write backlog.
	guard := time.AfterFunc(app.ShutdownHardLimit, func() {
		slog.Error("gateway did not finish shutting down; exiting anyway", "limit", app.ShutdownHardLimit.String())
		os.Exit(1)
	})
	shutdownCtx, cancel := context.WithTimeout(context.Background(), app.DefaultShutdownBudget)
	defer cancel()
	if err := rt.Stop(shutdownCtx); err != nil {
		slog.Error("gateway shutdown failed", "error", err)
	}
	guard.Stop()
	slog.Info("gateway stopped")
}
