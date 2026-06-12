package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// redisSeen is a minimal Redis client implementing seenStore with the single
// command this service needs: SET key 1 NX EX <ttl> (atomic SETNX + TTL).
//
// Why hand-rolled: the M1 dependency contract lists go-redis/v9, but it is
// absent from the frozen go.mod (which must not be edited), so the one
// command is spoken in RESP2 over a plain TCP connection with stdlib only.
// Swap in go-redis behind the seenStore interface once the dependency lands.
//
// One pooled connection guarded by a mutex is plenty: the consumer invokes
// the handler sequentially. A command that fails on the pooled connection is
// retried once on a fresh one (the server may have closed an idle socket);
// any remaining error surfaces to the handler, which fails OPEN.
type redisSeen struct {
	addr    string
	timeout time.Duration

	mu   sync.Mutex
	conn net.Conn
	br   *bufio.Reader
}

// redisTimeout bounds dial/read/write per command so a hung Redis cannot
// stall the pipeline beyond a beat (the handler proceeds without dedupe).
const redisTimeout = 2 * time.Second

func newRedisSeen(addr string) *redisSeen {
	return &redisSeen{addr: addr, timeout: redisTimeout}
}

// SetNX implements seenStore.
func (r *redisSeen) SetNX(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	args := []string{"SET", key, "1", "NX", "EX", strconv.FormatInt(int64(ttl/time.Second), 10)}

	r.mu.Lock()
	defer r.mu.Unlock()
	line, err := r.roundTrip(ctx, args)
	if err != nil && ctx.Err() == nil {
		line, err = r.roundTrip(ctx, args) // once more on a fresh connection
	}
	if err != nil {
		return false, fmt.Errorf("redis setnx: %w", err)
	}

	switch {
	case line == "+OK":
		return true, nil
	case line == "$-1" || line == "_": // RESP2 / RESP3 null: key already set
		return false, nil
	case strings.HasPrefix(line, "-"):
		return false, fmt.Errorf("redis setnx: server error: %s", strings.TrimPrefix(line, "-"))
	default:
		return false, fmt.Errorf("redis setnx: unexpected reply %q", line)
	}
}

// roundTrip sends one command and reads one reply line. On any error the
// connection is dropped so the next attempt dials fresh.
func (r *redisSeen) roundTrip(ctx context.Context, args []string) (string, error) {
	if r.conn == nil {
		d := net.Dialer{Timeout: r.timeout}
		conn, err := d.DialContext(ctx, "tcp", r.addr)
		if err != nil {
			return "", err
		}
		r.conn = conn
		r.br = bufio.NewReader(conn)
	}

	deadline := time.Now().Add(r.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := r.conn.SetDeadline(deadline); err != nil {
		r.dropConn()
		return "", err
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&buf, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := r.conn.Write(buf.Bytes()); err != nil {
		r.dropConn()
		return "", err
	}

	line, err := r.br.ReadString('\n')
	if err != nil {
		r.dropConn()
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (r *redisSeen) dropConn() {
	if r.conn != nil {
		_ = r.conn.Close()
		r.conn = nil
		r.br = nil
	}
}

// Close implements seenStore.
func (r *redisSeen) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropConn()
	return nil
}
