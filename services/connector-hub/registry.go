package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/asker/asker/connectors/gmail"
	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/upload"
	"github.com/asker/asker/platform/blob"
	"github.com/asker/asker/platform/crypto"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
	"github.com/asker/asker/services/connector-hub/internal/hub"
)

// buildDeps wires the in-process connectors (gmail, upload) and the
// tenant-encrypted blob store into the hub. This is the only file that
// imports connector packages: the hub core (internal/hub) stays
// connector-agnostic so it is testable against fakes, and the M2
// out-of-process plugin transport replaces only this wiring.
func buildDeps(ctx context.Context, cfg hub.Config, logger *slog.Logger) (hub.Deps, error) {
	kek, err := crypto.NewFileKEK(cfg.KEKFile)
	if err != nil {
		return hub.Deps{}, fmt.Errorf("load KEK %s: %w", cfg.KEKFile, err)
	}
	// Blob DEKs are wrapped by the shared dev KEK and persisted on the same
	// volume so encrypted uploads survive hub restarts (see dekstore.go).
	dekDir := filepath.Join(filepath.Dir(cfg.KEKFile), "connector-hub-deks")
	cipher := crypto.NewTenantCipher(kek, newFileDEKStore(dekDir))

	blobs, err := blob.New(ctx, blob.Config{
		Endpoint:  cfg.MinIOEndpoint,
		AccessKey: cfg.MinIOAccessKey,
		SecretKey: cfg.MinIOSecretKey,
		Bucket:    cfg.MinIOBucket,
		UseSSL:    cfg.MinIOUseSSL,
	}, cipher)
	if err != nil {
		return hub.Deps{}, fmt.Errorf("create blob store: %w", err)
	}

	registry := sdk.NewRegistry()
	if err := registry.Register(gmail.New(gmail.WithLogger(logger))); err != nil {
		return hub.Deps{}, fmt.Errorf("register gmail connector: %w", err)
	}
	if err := registry.Register(upload.New()); err != nil {
		return hub.Deps{}, fmt.Errorf("register upload connector: %w", err)
	}

	uploadFn := func(ctx context.Context, tenant tenancy.Context, file io.Reader, filename, title, contentType string, size int64) (*askerv1.Document, error) {
		return upload.HandleUpload(ctx, upload.Deps{Blobs: blobs, Now: time.Now},
			tenant, file, filename, title, contentType, size)
	}

	return hub.Deps{Registry: registry, Upload: uploadFn}, nil
}
