package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"emulator-aws-sqs/internal/auth"
	"emulator-aws-sqs/internal/clock"
	"emulator-aws-sqs/internal/config"
	"emulator-aws-sqs/internal/httpx"
	"emulator-aws-sqs/internal/model"
	"emulator-aws-sqs/internal/service"
	sqlitestore "emulator-aws-sqs/internal/storage/sqlite"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		panic(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	}))

	clk := clock.RealClock{}
	store, err := sqlitestore.Open(cfg.DataSource)
	if err != nil {
		logger.Error("failed to open storage", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		logger.Error("failed to initialize storage", "error", err)
		os.Exit(1)
	}

	registry := auth.NewRegistry(cfg.AccountID, auth.Credential{
		AccessKeyID:     cfg.DefaultAccessKeyID,
		SecretAccessKey: cfg.DefaultSecretAccessKey,
		SessionToken:    cfg.DefaultSessionToken,
		AccountID:       cfg.AccountID,
	})
	if err := registry.LoadFile(cfg.CredentialsFile); err != nil {
		logger.Error("failed to load credentials", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: httpx.Handler{
			Registry:   model.Default,
			Clock:      clk,
			Auth:       auth.NewService(cfg.AuthMode, clk, registry, cfg.AllowedRegions, cfg.Region, cfg.AccountID),
			Dispatcher: service.NewSQS(store, clk, cfg),
			Logger:     logger,
		},
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Info("starting sqsd", "listen_addr", cfg.ListenAddr, "public_base_url", cfg.PublicBaseURL, "region", cfg.Region)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped with error", "error", err)
		os.Exit(1)
	}
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToUpper(raw) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
