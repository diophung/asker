package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/asker/asker/services/connector-hub/internal/hub"
)

func testDepsConfig(t *testing.T) hub.Config {
	t.Helper()
	return hub.Config{
		KEKFile: filepath.Join(t.TempDir(), "kek.bin"),
		// A port nothing listens on: the blob store's bucket bootstrap must
		// fail fast instead of hanging.
		MinIOEndpoint:  "127.0.0.1:1",
		MinIOAccessKey: "asker-minio",
		MinIOSecretKey: "asker-minio-secret",
		MinIOBucket:    "asker-blobs",
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildDepsFailsWithoutKEK(t *testing.T) {
	cfg := testDepsConfig(t)
	// A KEK path inside a nonexistent directory cannot be created.
	cfg.KEKFile = filepath.Join(t.TempDir(), "no-such-dir", "kek.bin")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := buildDeps(ctx, cfg, quietLogger()); err == nil {
		t.Error("buildDeps succeeded with an uncreatable KEK file")
	}
}

func TestBuildDepsFailsWhenObjectStoreUnreachable(t *testing.T) {
	cfg := testDepsConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := buildDeps(ctx, cfg, quietLogger()); err == nil {
		t.Error("buildDeps succeeded with an unreachable object store")
	}
}
