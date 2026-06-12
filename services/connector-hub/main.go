// Command connector-hub runs every tenant's connector instances: it
// schedules full/incremental syncs against the control plane's instance
// records, receives source webhooks, accepts direct uploads from the
// gateway, and is the single chokepoint through which connector-emitted
// Documents enter the pipeline (docs.raw), with ts.ingested stamped and the
// instance tenant enforced on every document.
//
// Service contract (M1): HTTP :9300 (HUB_ADDR) for /webhooks/*, /upload and
// /v1/sync-status; health endpoints on :9301 (HUB_HEALTH_ADDR); container
// healthcheck via the -healthcheck self-probe flag.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/asker/asker/services/connector-hub/internal/hub"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit 0/1 (container healthcheck)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "connector-hub")

	cfg, err := hub.LoadConfig()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if *healthcheck {
		os.Exit(hub.RunHealthcheck(cfg.HealthAddr))
	}

	deps, err := buildDeps(context.Background(), cfg)
	if err != nil {
		logger.Error("failed to wire connectors", "error", err)
		os.Exit(1)
	}

	if err := hub.Run(context.Background(), cfg, deps, logger); err != nil {
		logger.Error("connector-hub exited", "error", err)
		os.Exit(1)
	}
}
