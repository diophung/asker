package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// The bespoke RESP wire-format tests were removed along with the hand-rolled
// client: SETNX semantics, TTL encoding, and reconnects are go-redis/v9's
// contract now. What remains is ours: errors must surface (wrapped) so the
// handler can fail OPEN, and Close must release the client.

func TestRedisSeenConnectionRefused(t *testing.T) {
	t.Parallel()
	// A listener that is immediately closed yields a port with nothing on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	r := newRedisSeen(addr)
	defer func() { _ = r.Close() }()
	_, err = r.Seen(context.Background(), "k")
	if err == nil {
		t.Fatal("Seen against a dead address: want error (handler fails open on it)")
	}
	if !strings.Contains(err.Error(), "redis exists:") {
		t.Errorf("Seen error = %q, want it wrapped with %q", err, "redis exists:")
	}
	if err := r.MarkSeen(context.Background(), "k", time.Hour); err == nil ||
		!strings.Contains(err.Error(), "redis set:") {
		t.Errorf("MarkSeen error = %v, want it wrapped with %q", err, "redis set:")
	}
}

func TestRedisSeenClose(t *testing.T) {
	t.Parallel()
	r := newRedisSeen("127.0.0.1:0")
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := r.Seen(context.Background(), "k"); err == nil {
		t.Error("Seen after Close: want error")
	}
}
