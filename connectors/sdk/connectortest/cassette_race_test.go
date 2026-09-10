package connectortest

import (
	"io"
	"net/http"
	"sync"
	"testing"
)

// A connector that fetches concurrently drives the ReplayServer from several
// goroutines at once (net/http serves each connection on its own goroutine).
// Cassette.match does a check-then-act on Interaction.played, so without a
// lock this races — it failed CI intermittently on PRs that touched no Go
// code at all. Run with -race; it fails reliably against an unsynchronized
// match.
func TestReplayServerConcurrentRequestsAreRaceFree(t *testing.T) {
	const n = 64

	interactions := make([]*Interaction, 0, n)
	for range n {
		interactions = append(interactions, &Interaction{
			Request:  RecordedRequest{Method: "GET", Path: "/poll"},
			Response: RecordedResponse{Status: 200, Body: "ok"},
		})
	}
	rs := NewReplayServer(t, &Cassette{Name: "race", Interactions: interactions})

	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(rs.URL() + "/poll") //nolint:noctx // test request
			if err != nil {
				codes[i] = -1
				return
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			codes[i] = resp.StatusCode
		}()
	}
	wg.Wait()

	// Each interaction is single-use, and there are exactly as many as there
	// are requests: every request must be served. Under the race two callers
	// could both see played == 0 for one interaction, double-serving it and
	// starving another (a replay miss).
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200 (each request must consume its own interaction)", i, c)
		}
	}
}
