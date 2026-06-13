// Command imessage-agent is a LOCAL macOS CLI that indexes your iMessages into
// Asker. There is no iMessage cloud API, so the agent reads the Messages SQLite
// database (~/Library/Messages/chat.db) on your own machine, renders each chat
// to a text transcript, and uploads it to Asker through the gateway's
// authenticated POST /v1/upload endpoint — where it becomes a searchable FILE
// document in your own tenant.
//
// Privacy posture: everything runs locally. The agent reads chat.db read-only,
// never sends data anywhere except the Asker gateway you point it at, and
// scopes everything to your tenant via your OIDC bearer token (the agent never
// chooses a tenant; the gateway derives it from the verified token). Message
// bodies are printed only under --dry-run and are never logged.
//
// Because there is no SQLite driver in the frozen build, the agent shells out
// to the system "sqlite3" binary (present on macOS) with `sqlite3 -json`. macOS
// guards chat.db behind Full Disk Access — see the README for granting it.
//
// Usage:
//
//	imessage-agent --gateway-url http://127.0.0.1:8080 --token "$OIDC_TOKEN"
//	imessage-agent --dry-run            # print transcripts, upload nothing
//	imessage-agent --since 0            # ignore saved state, re-read everything
//
// M2 scope: uploads chat transcripts as FILE documents via /v1/upload. Richer
// per-message CHAT_MESSAGE ingestion (the mapping in internal/imsg) awaits a
// future bulk-document API; see the README.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// defaultStateName is the state file name; it lives next to the user's config
// under the OS user-config dir by default.
const defaultStateName = "state.json"

// uploadTimeout bounds a single transcript upload round-trip.
const uploadTimeout = 60 * time.Second

func main() {
	if err := realMain(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "imessage-agent:", err)
		os.Exit(1)
	}
}

// realMain is the testable entrypoint: it parses flags, wires the real sqlite3
// reader and gateway uploader, and runs one sync pass. It returns an error
// (which main turns into a non-zero exit) rather than calling os.Exit, so a
// test can drive it. out/errOut are the stdout/stderr sinks.
func realMain(args []string, out, errOut *os.File) error {
	fs := flag.NewFlagSet("imessage-agent", flag.ContinueOnError)
	fs.SetOutput(errOut)

	defaultDB := defaultDBPath()
	dbPath := fs.String("db", defaultDB, "path to the Messages chat.db SQLite database")
	gatewayURL := fs.String("gateway-url", "", "Asker gateway base URL, e.g. http://127.0.0.1:8080 (required unless --dry-run)")
	token := fs.String("token", "", "OIDC bearer token for the gateway (required unless --dry-run)")
	statePath := fs.String("state", defaultStatePath(), "path to the incremental high-water state file")
	since := fs.Int64("since", -1, "override the incremental high-water ROWID (-1 = use the state file; 0 = re-read all messages)")
	dryRun := fs.Bool("dry-run", false, "print rendered transcripts to stdout and upload nothing")

	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Resolve the high-water mark: an explicit non-negative --since wins,
	// otherwise read the persisted state.
	st, err := loadState(*statePath)
	if err != nil {
		return err
	}
	start := st.LastRowID
	if *since >= 0 {
		start = *since
		logger.Info("overriding high-water mark from --since", "since_rowid", start)
	}

	if !*dryRun {
		if *gatewayURL == "" {
			return fmt.Errorf("--gateway-url is required unless --dry-run")
		}
		if *token == "" {
			return fmt.Errorf("--token is required unless --dry-run")
		}
	}

	reader, err := newSQLite3Reader(*dbPath)
	if err != nil {
		return err
	}

	var up uploader
	if !*dryRun {
		up = &gatewayUploader{
			client:  &http.Client{Timeout: uploadTimeout},
			baseURL: *gatewayURL,
			token:   *token,
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opts := options{since: start, dryRun: *dryRun}
	res, err := runSync(ctx, logger, reader, up, out, opts)
	if err != nil {
		return err
	}

	// Persist the advanced high-water mark only on a clean pass (a partial
	// upload failure returns early above and leaves state untouched, so the
	// next run retries the unsent chats). Dry-run never advances state.
	if !*dryRun && res.highWaterMove {
		if err := saveState(*statePath, state{LastRowID: res.newHighWater}); err != nil {
			return fmt.Errorf("persist state: %w", err)
		}
	}

	logger.Info("sync complete",
		"messages", res.messages,
		"chats", res.chats,
		"uploaded", res.uploaded,
		"high_water_rowid", res.newHighWater,
		"dry_run", *dryRun,
	)
	return nil
}

// defaultDBPath is the standard Messages database location for the current
// user: ~/Library/Messages/chat.db. It falls back to a bare relative path when
// the home directory cannot be resolved (the reader then reports a clear
// not-found error).
func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("Library", "Messages", "chat.db")
	}
	return filepath.Join(home, "Library", "Messages", "chat.db")
}

// defaultStatePath is the default high-water state file under the OS
// user-config dir: <config>/asker/imessage-agent/state.json.
func defaultStatePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return defaultStateName
	}
	return filepath.Join(dir, "asker", "imessage-agent", defaultStateName)
}
