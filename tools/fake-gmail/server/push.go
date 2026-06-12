package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// pusher delivers Pub/Sub-style push notifications to watch targets. Real
// Gmail publishes to Cloud Pub/Sub which pushes to a subscriber endpoint; in
// dev there is no Pub/Sub, so the fake POSTs the push envelope directly to
// the URL the watcher supplied in the X-Asker-Push-Url header (ADR-008).
//
// Delivery is asynchronous and best-effort: a few quick retries, then the
// failure is logged and dropped — never fatal, exactly like a missed push in
// production (the connector's polling fallback covers the gap).
type pusher struct {
	log     *slog.Logger
	client  *http.Client
	backoff time.Duration // base retry backoff; shrunk in tests
	nextID  atomic.Uint64
	wg      sync.WaitGroup
	now     func() time.Time
}

const pushAttempts = 3

func newPusher(log *slog.Logger, client *http.Client, now func() time.Time) *pusher {
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	return &pusher{log: log, client: client, backoff: 100 * time.Millisecond, now: now}
}

// notify fires one push for the mailbox change if a live watch exists.
// Safe to call with a nil watch.
func (p *pusher) notify(watch *watchTarget, email string, historyID uint64) {
	if watch == nil || watch.pushURL == "" {
		return
	}
	if p.now().After(watch.expiration) {
		p.log.Debug("watch expired, skipping push", "email", email)
		return
	}
	body, err := p.envelope(email, historyID)
	if err != nil { // cannot happen with these types; defensive
		p.log.Error("marshal push envelope", "error", err)
		return
	}
	url := watch.pushURL
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.deliver(url, email, historyID, body)
	}()
}

func (p *pusher) envelope(email string, historyID uint64) ([]byte, error) {
	data, err := json.Marshal(pushData{EmailAddress: email, HistoryID: historyID})
	if err != nil {
		return nil, err
	}
	env := pushEnvelope{
		Message: pushMessage{
			Data:        base64.StdEncoding.EncodeToString(data),
			MessageID:   fmt.Sprintf("fake-push-%d", p.nextID.Add(1)),
			PublishTime: p.now().UTC().Format(time.RFC3339Nano),
		},
		Subscription: "projects/fake-gmail/subscriptions/asker-dev",
	}
	return json.Marshal(env)
}

func (p *pusher) deliver(url, email string, historyID uint64, body []byte) {
	var lastErr error
	for attempt := 1; attempt <= pushAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(p.backoff * time.Duration(1<<(attempt-2)))
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			p.log.Error("build push request", "url", url, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.log.Debug("push delivered", "email", email, "history_id", historyID, "url", url)
			return
		}
		lastErr = fmt.Errorf("push endpoint returned status %d", resp.StatusCode)
	}
	p.log.Warn("push delivery failed, dropping notification",
		"email", email, "history_id", historyID, "url", url,
		"attempts", pushAttempts, "error", lastErr)
}

// wait blocks until all in-flight pushes finish.
func (p *pusher) wait() {
	p.wg.Wait()
}
