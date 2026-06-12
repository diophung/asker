package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// redisCounter is a minimal RESP2 Redis client implementing exactly the
// INCR+EXPIRE pipeline the rate limiter needs. go.mod is frozen and go-redis
// is not in it, so the two commands are spoken directly over TCP with the
// standard library; a small connection pool avoids a dial per request.
type redisCounter struct {
	addr    string
	timeout time.Duration
	pool    chan *redisConn
}

type redisConn struct {
	c  net.Conn
	br *bufio.Reader
}

func newRedisCounter(addr string) *redisCounter {
	return &redisCounter{
		addr:    addr,
		timeout: 2 * time.Second, // a slow Redis must not stall requests; fail open fast
		pool:    make(chan *redisConn, 8),
	}
}

// Incr pipelines INCR key + EXPIRE key ttl in one round trip and returns the
// post-increment count. Refreshing the TTL on every hit is harmless because
// the key embeds the window start.
func (rc *redisCounter) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	conn, err := rc.get(ctx)
	if err != nil {
		return 0, err
	}
	n, err := rc.incrOn(ctx, conn, key, ttl)
	if err != nil {
		// The connection state is unknown (possibly unread replies): drop it.
		_ = conn.c.Close()
		return 0, err
	}
	rc.put(conn)
	return n, nil
}

func (rc *redisCounter) incrOn(ctx context.Context, conn *redisConn, key string, ttl time.Duration) (int64, error) {
	deadline := time.Now().Add(rc.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.c.SetDeadline(deadline); err != nil {
		return 0, fmt.Errorf("redis: set deadline: %w", err)
	}

	cmd := appendRESPCommand(nil, "INCR", key)
	cmd = appendRESPCommand(cmd, "EXPIRE", key, strconv.FormatInt(int64(ttl/time.Second), 10))
	if _, err := conn.c.Write(cmd); err != nil {
		return 0, fmt.Errorf("redis: write: %w", err)
	}

	n, err := readRESPInt(conn.br) // INCR reply
	if err != nil {
		return 0, err
	}
	if _, err := readRESPInt(conn.br); err != nil { // EXPIRE reply
		return 0, err
	}
	return n, nil
}

// get returns a pooled connection or dials a new one.
func (rc *redisCounter) get(ctx context.Context) (*redisConn, error) {
	select {
	case conn := <-rc.pool:
		return conn, nil
	default:
	}
	d := net.Dialer{Timeout: rc.timeout}
	c, err := d.DialContext(ctx, "tcp", rc.addr)
	if err != nil {
		return nil, fmt.Errorf("redis: dial %s: %w", rc.addr, err)
	}
	return &redisConn{c: c, br: bufio.NewReader(c)}, nil
}

// put returns a healthy connection to the pool, closing it when full.
func (rc *redisCounter) put(conn *redisConn) {
	select {
	case rc.pool <- conn:
	default:
		_ = conn.c.Close()
	}
}

// appendRESPCommand appends the RESP2 encoding of a command to b.
func appendRESPCommand(b []byte, args ...string) []byte {
	b = append(b, '*')
	b = strconv.AppendInt(b, int64(len(args)), 10)
	b = append(b, '\r', '\n')
	for _, a := range args {
		b = append(b, '$')
		b = strconv.AppendInt(b, int64(len(a)), 10)
		b = append(b, '\r', '\n')
		b = append(b, a...)
		b = append(b, '\r', '\n')
	}
	return b
}

// readRESPInt reads one reply that must be a RESP integer. Error replies and
// any other type are reported as errors (the caller discards the connection).
func readRESPInt(br *bufio.Reader) (int64, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("redis: read reply: %w", err)
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		return 0, fmt.Errorf("redis: empty reply")
	}
	switch line[0] {
	case ':':
		n, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("redis: malformed integer reply %q", line)
		}
		return n, nil
	case '-':
		return 0, fmt.Errorf("redis: error reply: %s", line[1:])
	default:
		return 0, fmt.Errorf("redis: unexpected reply type %q", line)
	}
}
