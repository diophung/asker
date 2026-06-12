package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/asker/asker/connectors/gmail"
	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/upload"
	"github.com/asker/asker/connectors/upload/blob"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
	"github.com/asker/asker/services/connector-hub/internal/hub"
)

// buildDeps wires the in-process connectors (gmail, upload) and the upload
// blob store into the hub. This is the only file that imports connector
// packages: the hub core (internal/hub) stays connector-agnostic so it can
// be tested against fakes, and the M2 out-of-process plugin transport
// replaces only this wiring.
func buildDeps(ctx context.Context, cfg hub.Config) (hub.Deps, error) {
	blobs, err := blob.NewStore(ctx, blob.Config{
		Endpoint:  cfg.MinIOEndpoint,
		AccessKey: cfg.MinIOAccessKey,
		SecretKey: cfg.MinIOSecretKey,
		Bucket:    cfg.MinIOBucket,
		UseSSL:    cfg.MinIOUseSSL,
	})
	if err != nil {
		return hub.Deps{}, fmt.Errorf("create blob store: %w", err)
	}

	registry := sdk.NewRegistry()
	if err := registry.Register(gmail.New()); err != nil {
		return hub.Deps{}, fmt.Errorf("register gmail connector: %w", err)
	}
	if err := registry.Register(upload.New(blobs)); err != nil {
		return hub.Deps{}, fmt.Errorf("register upload connector: %w", err)
	}

	uploadFn := func(ctx context.Context, tenant tenancy.Context, file io.Reader, filename, title, contentType string, size int64) (*askerv1.Document, error) {
		return upload.HandleUpload(ctx, upload.Deps{Blobs: blobs, Now: time.Now},
			tenant, file, filename, title, contentType, size)
	}

	return hub.Deps{Registry: registry, Upload: uploadFn}, nil
}
