package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/zanel1u/cloud-cli-proxy/internal/controlplane/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := app.Config{
		Addr:           envOrDefault("CONTROL_PLANE_ADDR", ":8080"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		MigrationDir:   "internal/store/migrations",
		AdminUsername:  envOrDefault("ADMIN_USERNAME", "admin"),
		AdminPassword:  os.Getenv("ADMIN_PASSWORD"),
		AdminJWTSecret: os.Getenv("ADMIN_JWT_SECRET"),

		SSHProxyAddr:              envOrDefault("SSH_PROXY_ADDR", ":2222"),
		SSHProxyContainerUser:     envOrDefault("SSH_PROXY_CONTAINER_USER", envOrDefault("CLOUD_CLI_PROXY_WORKER_USER", "work")),
		SSHProxyContainerPassword: envOrDefault("SSH_PROXY_CONTAINER_PASSWORD", "work"),
		SSHProxyHostKeyPath:       envOrDefault("SSH_PROXY_HOST_KEY_PATH", defaultSSHProxyHostKeyPath()),
	}

	if cfg.DatabaseURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	if cfg.AdminJWTSecret == "" {
		slog.Warn("ADMIN_JWT_SECRET not set, admin API disabled")
	}

	controlPlane, err := app.New(ctx, cfg)
	if err != nil {
		slog.Error("failed to initialize control-plane", "error", err)
		os.Exit(1)
	}

	if err := controlPlane.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("control-plane stopped with error", "error", err)
		os.Exit(1)
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func defaultSSHProxyHostKeyPath() string {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/var/lib/cloud-cli-proxy"
	}
	return filepath.Join(dataDir, "ssh-proxy", "ssh_host_ed25519_key")
}
