package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// stubConnector implements Connector with a fixed Spec; sync methods are
// inert. It exists only for Registry tests.
type stubConnector struct{ spec Spec }

func (s stubConnector) Spec() Spec                                             { return s.spec }
func (s stubConnector) Validate(context.Context, Config) error                 { return nil }
func (s stubConnector) FullSync(context.Context, Config, Emit) (Cursor, error) { return "", nil }
func (s stubConnector) IncrementalSync(_ context.Context, _ Config, cur Cursor, _ Emit) (Cursor, error) {
	return cur, nil
}
func (s stubConnector) HandleWebhook(context.Context, Config, *http.Request, Emit) error {
	return ErrWebhookUnsupported
}

func stub(id string) stubConnector {
	return stubConnector{spec: Spec{
		ID:           id,
		DisplayName:  "Stub " + id,
		AuthType:     AuthNone,
		ConfigSchema: json.RawMessage(`{"type":"object"}`),
	}}
}

func TestRegistryRegisterErrors(t *testing.T) {
	r := NewRegistry()

	if err := r.Register(nil); err == nil {
		t.Error("Register(nil) returned nil error")
	}
	if err := r.Register(stub("")); err == nil {
		t.Error("Register with empty spec ID returned nil error")
	}
	if err := r.Register(stub("gmail")); err != nil {
		t.Fatalf("first Register(gmail) failed: %v", err)
	}
	if err := r.Register(stub("gmail")); err == nil {
		t.Error("duplicate Register(gmail) returned nil error")
	}
	// Failed registrations must not have polluted the registry.
	if got := len(r.List()); got != 1 {
		t.Errorf("registry has %d entries after error cases, want 1", got)
	}
}

func TestRegistryGetAndList(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"slack", "gmail", "upload"} { // deliberately unsorted
		if err := r.Register(stub(id)); err != nil {
			t.Fatalf("Register(%s): %v", id, err)
		}
	}

	c, ok := r.Get("gmail")
	if !ok {
		t.Fatal("Get(gmail) not found")
	}
	if got := c.Spec().ID; got != "gmail" {
		t.Errorf("Get(gmail) returned connector with ID %q", got)
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get(missing) reported found")
	}

	specs := r.List()
	want := []string{"gmail", "slack", "upload"}
	if len(specs) != len(want) {
		t.Fatalf("List returned %d specs, want %d", len(specs), len(want))
	}
	for i, w := range want {
		if specs[i].ID != w {
			t.Errorf("List()[%d].ID = %q, want %q (sorted by ID)", i, specs[i].ID, w)
		}
	}
}

func TestRegistryConcurrent(t *testing.T) {
	r := NewRegistry()
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("conn-%02d", i)
			if err := r.Register(stub(id)); err != nil {
				t.Errorf("Register(%s): %v", id, err)
			}
			r.Get(id)
			r.List()
		}()
	}
	wg.Wait()
	if got := len(r.List()); got != n {
		t.Errorf("registry has %d entries, want %d", got, n)
	}
}
