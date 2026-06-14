package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/asker/asker/connectors/confluence"
	"github.com/asker/asker/connectors/gcal"
	"github.com/asker/asker/connectors/gdrive"
	"github.com/asker/asker/connectors/gmail"
	"github.com/asker/asker/connectors/ical"
	"github.com/asker/asker/connectors/jira"
	"github.com/asker/asker/connectors/msteams"
	outlookcal "github.com/asker/asker/connectors/outlook-cal"
	outlookmail "github.com/asker/asker/connectors/outlook-mail"
	"github.com/asker/asker/connectors/s3"
	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/slack"
	"github.com/asker/asker/connectors/upload"
	whatsappexport "github.com/asker/asker/connectors/whatsapp-export"
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
	// KEK selection (ADR-015 §3): Vault Transit when VAULT_ADDR is set, else the
	// dev file KEK — fail closed in production with no Vault. The control plane
	// makes the IDENTICAL choice (main.go) with the same key name, so DEKs
	// wrapped by either service interoperate.
	kek, err := crypto.SelectKEK(crypto.KEKSelection{
		VaultAddr:    cfg.VaultAddr,
		VaultToken:   cfg.VaultToken,
		VaultKeyName: cfg.VaultKEKKeyName,
		KEKFile:      cfg.KEKFile,
		IsProd:       cfg.IsProd(),
	})
	if err != nil {
		return hub.Deps{}, fmt.Errorf("select KEK: %w", err)
	}
	if cfg.VaultAddr != "" {
		logger.Info("KEK provider: Vault Transit", "vault_addr", cfg.VaultAddr, "key_name", cfg.VaultKEKKeyName)
	} else {
		logger.Warn("KEK provider: DEV file KEK (no VAULT_ADDR) — not for production", "kek_file", cfg.KEKFile)
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
	// In-process connectors. Out-of-process plugins (ADR-010) register via the
	// same sdk.Registry once the hub gains plugin supervision.
	connectors := []sdk.Connector{
		gmail.New(gmail.WithLogger(logger)),
		upload.New(),
		outlookmail.New(outlookmail.WithLogger(logger)),
		gdrive.New(gdrive.WithLogger(logger)),
		gcal.New(gcal.WithLogger(logger)),
		outlookcal.New(outlookcal.WithLogger(logger)),
		slack.New(slack.WithLogger(logger)),
		confluence.New(confluence.WithLogger(logger)),
		jira.New(jira.WithLogger(logger)),
		s3.New(s3.WithLogger(logger)),
		ical.New(ical.WithLogger(logger)),
		whatsappexport.New(whatsappexport.WithLogger(logger)),
		msteams.New(msteams.WithLogger(logger)),
	}
	for _, c := range connectors {
		if err := registry.Register(c); err != nil {
			return hub.Deps{}, fmt.Errorf("register %s connector: %w", c.Spec().ID, err)
		}
	}

	uploadFn := func(ctx context.Context, tenant tenancy.Context, file io.Reader, filename, title, contentType string, size int64) (*askerv1.Document, error) {
		return upload.HandleUpload(ctx, upload.Deps{Blobs: blobs, Now: time.Now},
			tenant, file, filename, title, contentType, size)
	}

	// The same tenant-encrypted blob store backs the internal-only media
	// endpoint (ADR-013): the Python enrich worker reads originals and writes
	// thumbnails/keyframes through it because the envelope crypto stays in Go.
	return hub.Deps{Registry: registry, Upload: uploadFn, MediaBlobs: blobs}, nil
}
