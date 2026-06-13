package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Stats accumulates progress across the run; read for periodic progress lines
// and the final summary. All counters are atomic (the runner is concurrent).
type Stats struct {
	docs    atomic.Int64
	bytes   atomic.Int64
	errors  atomic.Int64
	tenants atomic.Int64
	rare    atomic.Int64
}

// Runner orchestrates generation + feeding: it walks the tenant plans from the
// resume point, generates each tenant's docs in the main goroutine (cheap,
// deterministic, low memory — one tenant at a time), and fans the feed I/O out
// across concurrency workers. Progress is reported on a ticker; the checkpoint
// is saved at each tenant boundary so a crash loses at most one tenant.
type Runner struct {
	Spec           Spec
	Feeder         Feeder
	Concurrency    int
	CheckpointPath string
	ProgressEvery  time.Duration
	Out            io.Writer // progress + summary sink (os.Stderr in main)
	// Now allows tests to control timing; defaults to time.Now.
	Now func() time.Time
}

// RunResult is the final summary returned to main and printed.
type RunResult struct {
	Tenants     int64
	Docs        int64
	Bytes       int64
	RareTokens  int64
	Errors      int64
	Elapsed     time.Duration
	StartTenant int
	EndTenant   int
}

// Run executes the corpus generation+feed, honoring resume (from the checkpoint
// at CheckpointPath, if any) and ctx cancellation (Ctrl-C => clean stop with a
// saved checkpoint). It returns the run summary. The generation layer is fully
// deterministic; only the feed I/O is concurrent and order-independent (each
// doc carries its own tenant group and id).
func (r *Runner) Run(ctx context.Context) (RunResult, error) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	var stats Stats

	cp, err := loadCheckpoint(r.CheckpointPath)
	if err != nil {
		return RunResult{}, err
	}
	startTenant := 0
	rareBase := 0
	if cp.LastTenant >= 0 {
		if !specsEqual(cp.Spec, r.Spec) {
			return RunResult{}, fmt.Errorf(
				"synthgen: checkpoint %s was built with a different spec; refusing to resume (delete it to start fresh)",
				r.CheckpointPath)
		}
		startTenant = cp.LastTenant + 1
		rareBase = cp.RareBase
		stats.docs.Store(cp.DocsFed)
		stats.bytes.Store(cp.BytesFed)
		stats.tenants.Store(int64(startTenant))
		stats.rare.Store(int64(rareBase))
		_, _ = fmt.Fprintf(r.Out, "synthgen: resuming from checkpoint at tenant %d (rare base %d, %d docs fed)\n",
			startTenant, rareBase, cp.DocsFed)
	}

	plans := PlanTenants(r.Spec)
	if startTenant >= len(plans) {
		_, _ = fmt.Fprintln(r.Out, "synthgen: checkpoint already complete; nothing to do")
		return r.result(&stats, start, now(), startTenant, len(plans)-1), nil
	}

	// Progress ticker.
	stopProgress := make(chan struct{})
	var progressWG sync.WaitGroup
	if r.ProgressEvery > 0 {
		progressWG.Add(1)
		go func() {
			defer progressWG.Done()
			t := time.NewTicker(r.ProgressEvery)
			defer t.Stop()
			for {
				select {
				case <-stopProgress:
					return
				case <-t.C:
					r.printProgress(&stats, start, now(), len(plans))
				}
			}
		}()
	}

	conc := r.Concurrency
	if conc < 1 {
		conc = 1
	}

	endTenant := startTenant
	var runErr error
tenantLoop:
	for ti := startTenant; ti < len(plans); ti++ {
		select {
		case <-ctx.Done():
			runErr = ctx.Err()
			break tenantLoop
		default:
		}

		tp := plans[ti]
		docs, rareUsed := GenerateTenant(r.Spec, tp, rareBase)
		if err := r.feedTenant(ctx, conc, docs, &stats); err != nil {
			runErr = err
			break tenantLoop
		}
		rareBase += rareUsed
		stats.tenants.Add(1)
		stats.rare.Store(int64(rareBase))
		endTenant = ti

		// Checkpoint at the tenant boundary: a crash loses at most this tenant.
		if err := saveCheckpoint(r.CheckpointPath, Checkpoint{
			Spec:          r.Spec,
			LastTenant:    ti,
			RareBase:      rareBase,
			DocsFed:       stats.docs.Load(),
			BytesFed:      stats.bytes.Load(),
			UpdatedUnixMs: now().UnixMilli(),
		}); err != nil {
			runErr = err
			break tenantLoop
		}
	}

	close(stopProgress)
	progressWG.Wait()

	res := r.result(&stats, start, now(), startTenant, endTenant)
	return res, runErr
}

// feedTenant fans the docs of one tenant out across conc workers. It returns
// the first feed error (and stops feeding the rest of the tenant); per-doc
// errors are also counted so a load run can tolerate a few and report them.
func (r *Runner) feedTenant(ctx context.Context, conc int, docs []GenDoc, stats *Stats) error {
	if len(docs) == 0 {
		return nil
	}
	work := make(chan GenDoc)
	errCh := make(chan error, conc)
	var wg sync.WaitGroup
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range work {
				if wctx.Err() != nil {
					return
				}
				if err := r.Feeder.Feed(wctx, d); err != nil {
					stats.errors.Add(1)
					select {
					case errCh <- err:
					default:
					}
					continue
				}
				stats.docs.Add(1)
				stats.bytes.Add(int64(approxFeedBytes(d.Doc)))
			}
		}()
	}

	for _, d := range docs {
		select {
		case <-wctx.Done():
			// fed by a worker error or ctx cancel
		case work <- d:
		}
		if wctx.Err() != nil {
			break
		}
	}
	close(work)
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
	}
}

func (r *Runner) result(stats *Stats, start, end time.Time, startTenant, endTenant int) RunResult {
	return RunResult{
		Tenants:     stats.tenants.Load(),
		Docs:        stats.docs.Load(),
		Bytes:       stats.bytes.Load(),
		RareTokens:  stats.rare.Load(),
		Errors:      stats.errors.Load(),
		Elapsed:     end.Sub(start),
		StartTenant: startTenant,
		EndTenant:   endTenant,
	}
}

func (r *Runner) printProgress(stats *Stats, start, now time.Time, totalTenants int) {
	elapsed := now.Sub(start).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}
	docs := stats.docs.Load()
	bytes := stats.bytes.Load()
	tenants := stats.tenants.Load()
	dps := float64(docs) / elapsed
	var eta string
	if tenants > 0 && totalTenants > 0 {
		fracTenants := float64(tenants) / float64(totalTenants)
		if fracTenants > 0 {
			totalEst := elapsed / fracTenants
			eta = (time.Duration((totalEst - elapsed) * float64(time.Second))).Round(time.Second).String()
		}
	}
	pf := printfTo(r.Out)
	pf("synthgen: %d/%d tenants, %d docs, %s, %.0f docs/s, errors=%d, eta=%s\n",
		tenants, totalTenants, docs, humanBytes(bytes), dps, stats.errors.Load(), eta)
}

// printfTo returns a Fprintf-style closure that writes to w and discards the
// (always-ignorable, progress/summary) write error — keeping call sites clean
// and errcheck-clean.
func printfTo(w io.Writer) func(format string, args ...any) {
	return func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
}

// PrintSummary writes the final corpus summary for the load report to cite.
func PrintSummary(out io.Writer, feeder Feeder, res RunResult) {
	pf := printfTo(out)
	pf("== synthgen summary ==\n")
	pf("  target:          %s\n", feeder.Name())
	pf("  tenants fed:     %d (indices %d..%d)\n", res.Tenants, res.StartTenant, res.EndTenant)
	pf("  docs fed:        %d\n", res.Docs)
	pf("  approx bytes:    %s\n", humanBytes(res.Bytes))
	pf("  rare tokens:     %d", res.RareTokens)
	if res.RareTokens > 0 {
		pf("  (RareToken(0)..RareToken(%d): %s..%s)",
			res.RareTokens-1, RareToken(0), RareToken(int(res.RareTokens)-1))
	}
	pf("\n")
	pf("  feed errors:     %d\n", res.Errors)
	pf("  elapsed:         %s\n", res.Elapsed.Round(time.Millisecond))
	if res.Elapsed > 0 {
		pf("  throughput:      %.0f docs/s\n", float64(res.Docs)/res.Elapsed.Seconds())
	}
}
