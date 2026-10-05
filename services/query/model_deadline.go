package main

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// Native model services use the remaining caller budget for bounded admission
// and queue expiry. The HTTP context still cancels transport independently.
func setRequestTimeoutHeader(req *http.Request) error {
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		ms := time.Until(deadline).Milliseconds()
		if ms <= 0 {
			return context.DeadlineExceeded
		}
		req.Header.Set("X-Request-Timeout-Ms", strconv.FormatInt(ms, 10))
	}
	return nil
}
