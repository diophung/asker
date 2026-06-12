package main

import (
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// upstreamError maps a gRPC call failure to a JSON HTTP error. Upstream
// message text is only surfaced for InvalidArgument (validation feedback the
// caller can act on); everything else gets an opaque body and a server-side
// log with the real error.
func (d *deps) upstreamError(w http.ResponseWriter, r *http.Request, op string, err error) {
	st, _ := status.FromError(err)
	code, msg := http.StatusBadGateway, "upstream error"
	switch st.Code() {
	case codes.InvalidArgument:
		code, msg = http.StatusBadRequest, st.Message()
	case codes.NotFound:
		code, msg = http.StatusNotFound, "not found"
	case codes.AlreadyExists:
		code, msg = http.StatusConflict, "already exists"
	case codes.PermissionDenied:
		code, msg = http.StatusForbidden, "forbidden"
	case codes.ResourceExhausted:
		code, msg = http.StatusTooManyRequests, "rate limited"
	case codes.DeadlineExceeded:
		code, msg = http.StatusGatewayTimeout, "upstream timeout"
	case codes.Unauthenticated, codes.FailedPrecondition:
		// Tenant plumbing failed (client interceptor refused a tenant-less
		// call, or the server rejected our metadata). That is a gateway bug,
		// not a client error: fail closed as an internal error.
		code, msg = http.StatusInternalServerError, "internal error"
	}
	d.logger.Error("upstream call failed",
		"op", op, "path", r.URL.Path, "grpc_code", st.Code().String(), "error", err)
	writeJSON(w, code, map[string]string{"error": msg})
}
