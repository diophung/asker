package outlookcal

import (
	"io"
	"log/slog"
	"time"

	"github.com/asker/asker/connectors/sdk"
)

// fixedNow is the deterministic clock used in tests so tombstone deleted_at is
// stable.
var fixedNow = time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)

// newTestConnector returns a Connector with a silent logger and a fixed clock,
// so tests are deterministic and quiet.
func newTestConnector() sdk.Connector {
	return New(
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithClock(func() time.Time { return fixedNow }),
	)
}

// testConnector returns the concrete *Connector for white-box unit tests that
// call unexported methods (e.g. tombstoneDocument).
func testConnector() *Connector {
	return &Connector{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: func() time.Time { return fixedNow },
	}
}
