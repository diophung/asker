package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/platform/kafkautil"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// freeAddr grabs an ephemeral 127.0.0.1 port. The tiny close-then-reuse race
// is acceptable in a local integration test.
func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("service never became ready at %s: %v", url, lastErr)
}

// TestRunEndToEnd boots the whole hub (telemetry, EnsureTopics against
// kfake, real producer, gRPC clients against the fake control plane, both
// HTTP servers, scheduler), drives a backfill, an upload and the sync-status
// listing over the wire, and shuts down gracefully.
func TestRunEndToEnd(t *testing.T) {
	kcfg := newKafkaEnv(t)
	f := newFakeControlPlane()
	_, _, cpAddr := startFakeControlPlane(t, f)

	conn := &fakeConnector{id: "gmail"}
	conn.fullSyncFn = func(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
		if err := emit(ctx, testDoc(tenantA, "gmail:e2e-1")); err != nil {
			return "", err
		}
		return "e2e-cursor", nil
	}
	registry := sdk.NewRegistry()
	if err := registry.Register(conn); err != nil {
		t.Fatalf("Register: %v", err)
	}
	upload := UploadFunc(func(_ context.Context, tc tenancy.Context, file io.Reader, filename, title, _ string, _ int64) (*askerv1.Document, error) {
		body, err := io.ReadAll(file)
		if err != nil {
			return nil, err
		}
		return &askerv1.Document{
			TenantId:       string(tc.TenantID()),
			DocId:          "upload:e2e",
			SourceNativeId: filename,
			Type:           askerv1.DocType_FILE,
			Title:          title,
			BodyText:       string(body),
			VersionEtag:    "v1",
		}, nil
	})

	cfg := Config{
		Addr:             freeAddr(t),
		HealthAddr:       freeAddr(t),
		WebhookBase:      "http://connector-hub:9300",
		ControlPlaneAddr: cpAddr,
		SyncInterval:     50 * time.Millisecond,
		SchedulerTick:    25 * time.Millisecond,
		Kafka:            kcfg,
	}

	f.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, Deps{Registry: registry, Upload: upload}, testLogger()) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned error on shutdown: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("Run did not shut down within 30s")
		}
	}()

	waitReady(t, "http://"+cfg.HealthAddr+"/readyz")

	// Container healthcheck self-probe path.
	if code := RunHealthcheck(cfg.HealthAddr); code != 0 {
		t.Errorf("RunHealthcheck = %d, want 0", code)
	}

	// The scheduler picked up the instance and completed the backfill.
	waitFor(t, 15*time.Second, func() bool {
		w, ok := f.lastWrite()
		return ok && w.state.GetPhase() == controlplanev1.SyncPhase_INCREMENTAL && w.state.GetCursor() == "e2e-cursor"
	}, "backfill completion through the real wiring")

	client := &http.Client{Timeout: 10 * time.Second}
	base := "http://" + cfg.Addr
	uploadURL := base + "/upload"

	// /upload without a tenant fails closed.
	body1, contentType := multipartBody(t, "e2e.txt", "e2e body", "E2E")
	httpReq, err := http.NewRequest(http.MethodPost, uploadURL, body1)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Content-Type", contentType)
	resp, err := client.Do(httpReq)
	if err != nil {
		t.Fatalf("POST /upload (no tenant): %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("upload without tenant = %d, want 401", resp.StatusCode)
	}

	// /upload with the tenant header round-trips a doc_id.
	body2, contentType2 := multipartBody(t, "e2e.txt", "e2e body", "E2E")
	httpReq2, err := http.NewRequest(http.MethodPost, uploadURL, body2)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq2.Header.Set("Content-Type", contentType2)
	httpReq2.Header.Set(tenantHeader, tenantA)
	resp2, err := client.Do(httpReq2)
	if err != nil {
		t.Fatalf("POST /upload: %v", err)
	}
	body, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("upload = %d (body %s), want 202", resp2.StatusCode, body)
	}
	var uploadResp map[string]string
	if err := json.Unmarshal(body, &uploadResp); err != nil {
		t.Fatalf("upload response not JSON: %v", err)
	}
	if uploadResp["doc_id"] != "upload:e2e" {
		t.Errorf("upload response = %v", uploadResp)
	}

	// /v1/sync-status lists the instance with its sync state inlined.
	statusReq, err := http.NewRequest(http.MethodGet, base+"/v1/sync-status", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	statusReq.Header.Set(tenantHeader, tenantA)
	resp3, err := client.Do(statusReq)
	if err != nil {
		t.Fatalf("GET /v1/sync-status: %v", err)
	}
	statusBody, _ := io.ReadAll(resp3.Body)
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("sync-status = %d (body %s), want 200", resp3.StatusCode, statusBody)
	}
	var statusResp struct {
		Instances []instanceStatus `json:"instances"`
	}
	if err := json.Unmarshal(statusBody, &statusResp); err != nil {
		t.Fatalf("sync-status not JSON: %v", err)
	}
	if len(statusResp.Instances) != 1 || statusResp.Instances[0].Instance.ID != instGmail {
		t.Fatalf("sync-status = %s", statusBody)
	}
	if statusResp.Instances[0].Sync.Phase != "INCREMENTAL" {
		t.Errorf("sync phase = %q, want INCREMENTAL", statusResp.Instances[0].Sync.Phase)
	}

	// Both the synced doc and the uploaded doc reached docs.raw.
	recs := fetchRawRecords(t, kcfg, 2, 30*time.Second)
	if len(recs) < 2 {
		t.Errorf("docs.raw records = %d, want >= 2 (sync + upload)", len(recs))
	}
}

// TestRunValidatesDeps proves Run fails fast on a broken wiring rather than
// serving without connectors.
func TestRunValidatesDeps(t *testing.T) {
	cfg := Config{
		Addr: ":0", HealthAddr: ":0", WebhookBase: "http://x",
		ControlPlaneAddr: "127.0.0.1:1", SyncInterval: time.Second, SchedulerTick: time.Second,
	}
	if err := Run(context.Background(), cfg, Deps{}, testLogger()); err == nil {
		t.Error("Run accepted empty Deps")
	}
	reg := sdk.NewRegistry()
	if err := Run(context.Background(), cfg, Deps{Registry: reg}, testLogger()); err == nil {
		t.Error("Run accepted nil Upload")
	}
}

// TestRunFailsWhenKafkaUnreachable: EnsureTopics is a startup precondition.
func TestRunFailsWhenKafkaUnreachable(t *testing.T) {
	reg := sdk.NewRegistry()
	if err := reg.Register(&fakeConnector{id: "gmail"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	cfg := Config{
		Addr: ":0", HealthAddr: ":0", WebhookBase: "http://x",
		ControlPlaneAddr: "127.0.0.1:1",
		SyncInterval:     time.Second, SchedulerTick: time.Second,
		// A port nothing listens on: EnsureTopics must fail fast.
		Kafka: kafkautil.Config{Brokers: []string{"127.0.0.1:1"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Run(ctx, cfg, Deps{Registry: reg, Upload: stubUpload(nil)}, testLogger())
	if err == nil {
		t.Error("Run succeeded with unreachable Kafka")
	}
}
