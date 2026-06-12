package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// Scheduler defaults: at most maxConcurrentSyncs syncs run hub-wide at once;
// repeated failures back off exponentially from failureBackoffBase to
// failureBackoffCap.
const (
	maxConcurrentSyncs = 4
	failureBackoffBase = 5 * time.Second
	failureBackoffCap  = 5 * time.Minute
)

// scheduler reconciles a set of per-instance sync workers against the
// control plane's cross-tenant instance listing (SchedulerService, the one
// tenant-exempt RPC). Each ACTIVE instance gets exactly one worker goroutine,
// so an instance's syncs are serialized by construction; global concurrency
// is bounded by a semaphore.
type scheduler struct {
	cp       controlplanev1.ControlPlaneServiceClient
	sched    controlplanev1.SchedulerServiceClient
	registry *sdk.Registry
	emit     *emitter
	logger   *slog.Logger

	webhookBase  string
	syncInterval time.Duration
	tick         time.Duration
	backoffBase  time.Duration
	backoffCap   time.Duration
	now          func() time.Time

	sem chan struct{}

	mu      sync.Mutex
	workers map[string]*worker
	wg      sync.WaitGroup
}

// schedulerOpts collects the scheduler's collaborators; zero optional fields
// take the production defaults above.
type schedulerOpts struct {
	cp           controlplanev1.ControlPlaneServiceClient
	sched        controlplanev1.SchedulerServiceClient
	registry     *sdk.Registry
	emit         *emitter
	logger       *slog.Logger
	webhookBase  string
	syncInterval time.Duration
	tick         time.Duration
	backoffBase  time.Duration // optional
	backoffCap   time.Duration // optional
	now          func() time.Time
}

func newScheduler(o schedulerOpts) *scheduler {
	if o.backoffBase <= 0 {
		o.backoffBase = failureBackoffBase
	}
	if o.backoffCap <= 0 {
		o.backoffCap = failureBackoffCap
	}
	if o.now == nil {
		o.now = time.Now
	}
	return &scheduler{
		cp:           o.cp,
		sched:        o.sched,
		registry:     o.registry,
		emit:         o.emit,
		logger:       o.logger,
		webhookBase:  strings.TrimRight(o.webhookBase, "/"),
		syncInterval: o.syncInterval,
		tick:         o.tick,
		backoffBase:  o.backoffBase,
		backoffCap:   o.backoffCap,
		now:          o.now,
		sem:          make(chan struct{}, maxConcurrentSyncs),
		workers:      make(map[string]*worker),
	}
}

// instanceSnap is one worker's current view of its instance: the validated
// tenant identity plus the control-plane record.
type instanceSnap struct {
	tenant tenancy.Context
	inst   *controlplanev1.ConnectorInstance
}

// worker is the per-instance sync loop handle. wake (buffered 1) coalesces
// webhook-triggered "sync now" requests; snap is refreshed on every
// reconcile so config changes apply on the next pass.
type worker struct {
	instanceID string
	cancel     context.CancelFunc
	wake       chan struct{}

	mu   sync.Mutex
	snap instanceSnap
}

func (w *worker) update(snap instanceSnap) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.snap = snap
}

func (w *worker) snapshot() instanceSnap {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snap
}

// Run reconciles immediately, then every tick, until ctx is canceled; it
// returns only after every worker goroutine has drained.
func (s *scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	s.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			s.stopAll()
			s.wg.Wait()
			return
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

// reconcile aligns the running worker set with the control plane: a worker
// per ACTIVE instance, none for PAUSED/ERROR/deleted ones. A listing failure
// keeps the current workers (better stale syncs than none).
func (s *scheduler) reconcile(ctx context.Context) {
	lctx, cancel := context.WithTimeout(ctx, s.tick)
	resp, err := s.sched.ListAllInstances(lctx, &controlplanev1.ListAllInstancesRequest{})
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			s.logger.ErrorContext(ctx, "ListAllInstances failed; keeping current workers", "error", err)
		}
		return
	}

	desired := make(map[string]instanceSnap, len(resp.GetInstances()))
	for _, ti := range resp.GetInstances() {
		inst := ti.GetInstance()
		if inst.GetId() == "" {
			continue
		}
		// Fail closed per instance: a tenant that does not survive
		// re-validation schedules nothing.
		tc, err := tenancy.FromHeaderValue(ti.GetTenantId())
		if err != nil {
			s.logger.ErrorContext(ctx, "instance has invalid tenant; skipping",
				"instance_id", inst.GetId(), "error", err)
			continue
		}
		if inst.GetStatus() != controlplanev1.ConnectorStatus_ACTIVE {
			continue
		}
		desired[inst.GetId()] = instanceSnap{tenant: tc, inst: inst}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, snap := range desired {
		if w, ok := s.workers[id]; ok {
			w.update(snap)
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		w := &worker{instanceID: id, cancel: cancel, wake: make(chan struct{}, 1)}
		w.snap = snap
		s.workers[id] = w
		s.wg.Add(1)
		s.logger.InfoContext(ctx, "starting sync worker",
			"instance_id", id, "connector_id", snap.inst.GetConnectorId(), "tenant", snap.tenant.TenantID())
		go s.runWorker(wctx, w)
	}
	for id, w := range s.workers {
		if _, ok := desired[id]; !ok {
			s.logger.InfoContext(ctx, "stopping sync worker (instance paused or removed)", "instance_id", id)
			w.cancel()
			delete(s.workers, id)
		}
	}
}

func (s *scheduler) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, w := range s.workers {
		w.cancel()
		delete(s.workers, id)
	}
}

// lookup returns the current snapshot for a scheduled instance (the webhook
// receiver's registry of known instances).
func (s *scheduler) lookup(instanceID string) (instanceSnap, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workers[instanceID]
	if !ok {
		return instanceSnap{}, false
	}
	return w.snapshot(), true
}

// trigger requests an immediate sync pass for the instance (webhook path).
// The buffered send coalesces bursts; per-instance serialization is the
// worker loop itself.
func (s *scheduler) trigger(instanceID string) {
	s.mu.Lock()
	w, ok := s.workers[instanceID]
	s.mu.Unlock()
	if ok {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// runWorker is the per-instance loop: sync, then sleep until the steady
// interval elapses, a webhook wakes it, or ctx ends. Consecutive failures
// back off exponentially (capped); any success resets the schedule.
func (s *scheduler) runWorker(ctx context.Context, w *worker) {
	defer s.wg.Done()
	failures := 0
	delay := time.Duration(0) // first pass runs immediately
	for {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-w.wake:
				timer.Stop()
			}
		}
		// Bounded concurrency across all workers.
		select {
		case <-ctx.Done():
			return
		case s.sem <- struct{}{}:
		}
		err := s.syncOnce(ctx, w)
		<-s.sem

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			failures++
			delay = backoffDelay(s.backoffBase, s.backoffCap, failures)
			s.logger.WarnContext(ctx, "sync pass failed",
				"instance_id", w.instanceID, "consecutive_failures", failures,
				"retry_in", delay.String(), "error", err)
		} else {
			failures = 0
			delay = s.syncInterval
		}
	}
}

// backoffDelay returns base doubled per consecutive failure beyond the
// first, capped at limit.
func backoffDelay(base, limit time.Duration, failures int) time.Duration {
	d := base
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= limit {
			return limit
		}
	}
	return min(d, limit)
}

// syncRun is the mutable bookkeeping for one sync pass. All fields are
// touched from the worker goroutine only (Checkpoint is invoked
// synchronously inside FullSync); emitted is atomic because the emit
// callback signature does not promise a goroutine.
type syncRun struct {
	instanceID string
	cursor     string
	started    *timestamppb.Timestamp
	completed  *timestamppb.Timestamp
	lastError  string
	docsBase   int64
	emitted    atomic.Int64
}

func (r *syncRun) state(phase controlplanev1.SyncPhase) *controlplanev1.SyncState {
	return &controlplanev1.SyncState{
		ConnectorInstanceId: r.instanceID,
		Cursor:              r.cursor,
		Phase:               phase,
		LastSyncStarted:     r.started,
		LastSyncCompleted:   r.completed,
		LastError:           r.lastError,
		DocsEmitted:         r.docsBase + r.emitted.Load(),
	}
}

// syncOnce performs one full sync pass for the worker's instance:
//
//	cursor == "" and phase not yet INCREMENTAL  -> FullSync (initial or
//	    restarted backfill; an interrupted backfill that checkpointed a
//	    cursor resumes through IncrementalSync from that cursor instead)
//	otherwise                                   -> IncrementalSync(cursor)
//
// sdk.ErrCursorExpired from IncrementalSync clears the cursor, records phase
// FULL_SYNC, and reruns FullSync immediately within the same pass.
func (s *scheduler) syncOnce(ctx context.Context, w *worker) error {
	snap := w.snapshot()
	inst := snap.inst
	tctx := tenancy.WithContext(ctx, snap.tenant)

	stResp, err := s.cp.GetSyncState(tctx, &controlplanev1.GetSyncStateRequest{
		ConnectorInstanceId: inst.GetId(),
	})
	if err != nil {
		return fmt.Errorf("hub: get sync state for %s: %w", inst.GetId(), err)
	}
	st := stResp.GetState()

	run := &syncRun{
		instanceID: inst.GetId(),
		cursor:     st.GetCursor(),
		completed:  st.GetLastSyncCompleted(),
		docsBase:   st.GetDocsEmitted(),
	}

	conn, ok := s.registry.Get(inst.GetConnectorId())
	if !ok {
		err := fmt.Errorf("hub: connector %q is not registered", inst.GetConnectorId())
		s.recordFailure(tctx, run, err)
		return err
	}

	full := run.cursor == "" && st.GetPhase() != controlplanev1.SyncPhase_INCREMENTAL
	phase := controlplanev1.SyncPhase_INCREMENTAL
	if full {
		phase = controlplanev1.SyncPhase_FULL_SYNC
	}

	// Mark the pass started before any source traffic.
	run.started = timestamppb.New(s.now().UTC())
	if err := s.writeState(tctx, run, phase); err != nil {
		return err
	}

	// Checkpoint persists the connector's mid-backfill cursor so an
	// interrupted FullSync resumes (via IncrementalSync from the checkpoint)
	// instead of restarting.
	checkpoint := func(cctx context.Context, cur sdk.Cursor) error {
		run.cursor = string(cur)
		return s.writeState(tenancy.WithContext(cctx, snap.tenant), run, controlplanev1.SyncPhase_FULL_SYNC)
	}

	cfg, err := s.buildConfig(tctx, snap, checkpoint)
	if err != nil {
		s.recordFailure(tctx, run, err)
		return err
	}

	emit := sdk.Emit(s.emit.emitFor(snap.tenant, inst.GetConnectorId(), &run.emitted))

	var cur sdk.Cursor
	var syncErr error
	if full {
		cur, syncErr = conn.FullSync(tctx, cfg, emit)
	} else {
		cur, syncErr = conn.IncrementalSync(tctx, cfg, sdk.Cursor(run.cursor), emit)
		if errors.Is(syncErr, sdk.ErrCursorExpired) {
			// The source can no longer replay this cursor: clear it, record
			// the FULL_SYNC restart, and backfill immediately.
			s.logger.WarnContext(ctx, "cursor expired at source; restarting full sync",
				"instance_id", inst.GetId(), "connector_id", inst.GetConnectorId())
			run.cursor = ""
			run.started = timestamppb.New(s.now().UTC())
			if err := s.writeState(tctx, run, controlplanev1.SyncPhase_FULL_SYNC); err != nil {
				return err
			}
			cur, syncErr = conn.FullSync(tctx, cfg, emit)
		}
	}
	if syncErr != nil {
		s.recordFailure(tctx, run, syncErr)
		return syncErr
	}

	run.cursor = string(cur)
	run.completed = timestamppb.New(s.now().UTC())
	run.lastError = ""
	return s.writeState(tctx, run, controlplanev1.SyncPhase_INCREMENTAL)
}

// writeState upserts the instance's sync state via the control plane.
func (s *scheduler) writeState(tctx context.Context, run *syncRun, phase controlplanev1.SyncPhase) error {
	if _, err := s.cp.SetSyncState(tctx, &controlplanev1.SetSyncStateRequest{State: run.state(phase)}); err != nil {
		return fmt.Errorf("hub: set sync state for %s: %w", run.instanceID, err)
	}
	return nil
}

// recordFailure best-effort persists phase FAILED + last_error; the sync
// error itself is what the caller propagates (and what drives backoff).
func (s *scheduler) recordFailure(tctx context.Context, run *syncRun, cause error) {
	run.lastError = cause.Error()
	if err := s.writeState(tctx, run, controlplanev1.SyncPhase_FAILED); err != nil {
		s.logger.ErrorContext(tctx, "recording sync failure failed",
			"instance_id", run.instanceID, "error", err, "cause", cause)
	}
}

// buildConfig assembles the sdk.Config for one run: the decrypted token from
// the control-plane vault (absent for AuthNone connectors) and the instance
// ConfigJSON with the hub-owned webhook_url merged in. tctx must carry the
// instance tenant.
func (s *scheduler) buildConfig(tctx context.Context, snap instanceSnap, checkpoint sdk.Checkpoint) (sdk.Config, error) {
	token, err := s.fetchToken(tctx, snap.inst.GetId())
	if err != nil {
		return sdk.Config{}, err
	}
	cfgJSON, err := mergeWebhookURL(snap.inst.GetConfigJson(), s.webhookBase, snap.inst.GetConnectorId(), snap.inst.GetId())
	if err != nil {
		return sdk.Config{}, err
	}
	if checkpoint == nil {
		checkpoint = sdk.NopCheckpoint
	}
	return sdk.Config{
		Tenant:     snap.tenant,
		InstanceID: snap.inst.GetId(),
		ConfigJSON: cfgJSON,
		Token:      token,
		Checkpoint: checkpoint,
	}, nil
}

// fetchToken returns the instance's decrypted credential, or nil when none
// is stored (AuthNone connectors, or not yet connected).
func (s *scheduler) fetchToken(tctx context.Context, instanceID string) ([]byte, error) {
	resp, err := s.cp.GetToken(tctx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: instanceID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("hub: get token for %s: %w", instanceID, err)
	}
	return resp.GetToken(), nil
}

// mergeWebhookURL merges the hub-owned webhook_url key into the instance
// configuration object.
func mergeWebhookURL(configJSON []byte, base, connectorID, instanceID string) ([]byte, error) {
	m := make(map[string]any)
	if len(configJSON) > 0 {
		if err := json.Unmarshal(configJSON, &m); err != nil {
			return nil, fmt.Errorf("hub: instance config is not a JSON object: %w", err)
		}
	}
	m["webhook_url"] = fmt.Sprintf("%s/webhooks/%s/%s", strings.TrimRight(base, "/"), connectorID, instanceID)
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("hub: merge webhook_url: %w", err)
	}
	return out, nil
}
