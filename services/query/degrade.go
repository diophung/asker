package main

import (
	"context"
	"errors"
)

// Degradation ladder (spec §2.6, ADR-006). M1 has no rerank rung, so the
// ladder is:
//
//  1. TEI failure/timeout while embedding a HYBRID query -> keyword-only
//     retrieval, degraded="keyword-only" (handled in server.Search).
//  2. Vespa hybrid-profile failure (transport error, client-side timeout,
//     HTTP 5xx) -> exactly ONE retry with the keyword profile,
//     degraded="keyword-only" (handled here).
//  3. Vespa keyword-profile failure -> error to the caller; there is nothing
//     left to shed.
//
// A search never fails closed on a degradable path. VECTOR mode opts out of
// rung 1 by contract (the caller asked for vectors specifically, so silent
// keyword results would be a lie) and consequently never reaches rung 2 with
// a retryable shape.
//
// Failure classification lives with the Vespa client: it wraps retryable
// failures in degradableError. HTTP 4xx and in-body query errors are terminal
// — they indicate a malformed query that a keyword retry built from the same
// inputs would mostly reproduce.

// degradedKeywordOnly is the SearchResponse.degraded marker for a vector path
// that was skipped or shed.
const degradedKeywordOnly = "keyword-only"

// degradableError marks a retrieval failure the ladder may absorb.
type degradableError struct{ err error }

func (e *degradableError) Error() string { return e.err.Error() }
func (e *degradableError) Unwrap() error { return e.err }

// isDegradable reports whether err allows stepping down the ladder.
func isDegradable(err error) bool {
	var d *degradableError
	return errors.As(err, &d)
}

// searchWithDegradation executes the Vespa retrieval, applying rung 2: a
// degradable hybrid-profile failure is retried once as keyword. It returns
// the result and whether the keyword fallback served it.
func (s *server) searchWithDegradation(ctx context.Context, vq vespaQuery) (vespaResult, bool, error) {
	res, err := s.vespa.Search(ctx, vq)
	if err == nil {
		return res, false, nil
	}
	if vq.Kind != retrieveHybrid || !isDegradable(err) {
		return vespaResult{}, false, err
	}

	s.logger.Warn("vespa hybrid retrieval failed; retrying keyword-only",
		"tenant", string(vq.Tenant), "error", err)
	vq.Kind = retrieveKeyword
	vq.Vector = nil
	res, err = s.vespa.Search(ctx, vq)
	if err != nil {
		return vespaResult{}, true, err
	}
	return res, true, nil
}
