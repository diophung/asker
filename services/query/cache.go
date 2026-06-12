package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"github.com/asker/asker/platform/tenancy"
)

// cacheTTL is the result-cache lifetime (build contract: 60s).
const cacheTTL = 60 * time.Second

// resultCache abstracts the exactly two Redis operations the query path uses
// so tests can substitute a fake and a Redis outage degrades to "no cache"
// instead of failing searches.
type resultCache interface {
	// Get returns the cached value and whether the key was present.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores value under key with the given TTL.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// cacheKey derives the result-cache key "q:<tenant>:<sha256 of normalized
// request>". The hash input is a canonical, field-separated rendering of the
// normalized request (proto wire marshaling is not guaranteed deterministic,
// so it is not hashed directly). Doc types are sorted so order-insensitive
// equivalent requests share an entry.
func cacheKey(tenant tenancy.TenantID, req *queryv1.SearchRequest) string {
	types := make([]int32, 0, len(req.GetDocTypes()))
	for _, t := range req.GetDocTypes() {
		types = append(types, int32(t))
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })

	var b strings.Builder
	b.WriteString(req.GetQuery())
	b.WriteByte(0x1f)
	for _, t := range types {
		fmt.Fprintf(&b, "%d,", t)
	}
	b.WriteByte(0x1f)
	writeTimestampField := func(set bool, sec int64, nanos int32) {
		if set {
			fmt.Fprintf(&b, "%d.%d", sec, nanos)
		} else {
			b.WriteByte('-')
		}
		b.WriteByte(0x1f)
	}
	writeTimestampField(req.GetFromDate() != nil, req.GetFromDate().GetSeconds(), req.GetFromDate().GetNanos())
	writeTimestampField(req.GetToDate() != nil, req.GetToDate().GetSeconds(), req.GetToDate().GetNanos())
	b.WriteString(req.GetParticipant())
	fmt.Fprintf(&b, "\x1f%d\x1f%d\x1f%d", req.GetLimit(), req.GetOffset(), req.GetMode())

	sum := sha256.Sum256([]byte(b.String()))
	return "q:" + string(tenant) + ":" + hex.EncodeToString(sum[:])
}

// redisCache is a minimal RESP2 Redis client implementing resultCache with
// only the two commands the query path needs (GET, SET ... EX). It is written
// against the standard library because go-redis/v9 is named in the build
// contract but absent from the frozen go.mod (recorded as a build issue);
// swapping go-redis in later only touches this file.
type redisCache struct {
	addr string

	mu   sync.Mutex
	idle []*redisConn
}

const (
	// redisOpTimeout bounds a single cache operation so a sick Redis cannot
	// eat the query latency budget (cache check is a 5ms line item).
	redisOpTimeout = 250 * time.Millisecond
	redisMaxIdle   = 4
)

// errNilReply is the RESP nil bulk string: key absent (healthy protocol state).
var errNilReply = errors.New("redis: nil reply")

// redisServerError is an -ERR style reply; the connection remains usable.
type redisServerError struct{ msg string }

func (e *redisServerError) Error() string { return "redis: server error: " + e.msg }

type redisConn struct {
	nc net.Conn
	br *bufio.Reader
}

func (c *redisConn) close() { _ = c.nc.Close() }

func newRedisCache(addr string) *redisCache { return &redisCache{addr: addr} }

// Get implements resultCache.
func (c *redisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	reply, err := c.do(ctx, []byte("GET"), []byte(key))
	if errors.Is(err, errNilReply) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return reply, true, nil
}

// Set implements resultCache.
func (c *redisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	secs := int64(ttl / time.Second)
	if secs < 1 {
		secs = 1
	}
	_, err := c.do(ctx, []byte("SET"), []byte(key), value, []byte("EX"), []byte(strconv.FormatInt(secs, 10)))
	if errors.Is(err, errNilReply) {
		return nil
	}
	return err
}

// Close drops all idle connections (process shutdown).
func (c *redisCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.idle {
		conn.close()
	}
	c.idle = nil
}

// do runs one command and reads one reply. IO errors poison the connection;
// protocol-level replies (nil, -ERR) keep it pooled.
func (c *redisCache) do(ctx context.Context, args ...[]byte) ([]byte, error) {
	conn, err := c.acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("redis: connect %s: %w", c.addr, err)
	}

	deadline := time.Now().Add(redisOpTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.nc.SetDeadline(deadline); err != nil {
		conn.close()
		return nil, fmt.Errorf("redis: set deadline: %w", err)
	}
	if err := writeRedisCommand(conn.nc, args); err != nil {
		conn.close()
		return nil, fmt.Errorf("redis: write command: %w", err)
	}

	reply, err := readRedisReply(conn.br)
	switch {
	case err == nil:
		c.release(conn)
		return reply, nil
	case errors.Is(err, errNilReply):
		c.release(conn)
		return nil, errNilReply
	default:
		var srvErr *redisServerError
		if errors.As(err, &srvErr) {
			c.release(conn)
			return nil, err
		}
		conn.close()
		return nil, err
	}
}

func (c *redisCache) acquire(ctx context.Context) (*redisConn, error) {
	c.mu.Lock()
	if n := len(c.idle); n > 0 {
		conn := c.idle[n-1]
		c.idle = c.idle[:n-1]
		c.mu.Unlock()
		return conn, nil
	}
	c.mu.Unlock()

	d := net.Dialer{Timeout: redisOpTimeout}
	nc, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, err
	}
	return &redisConn{nc: nc, br: bufio.NewReader(nc)}, nil
}

func (c *redisCache) release(conn *redisConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.idle) >= redisMaxIdle {
		conn.close()
		return
	}
	c.idle = append(c.idle, conn)
}

// writeRedisCommand encodes args as a RESP array of bulk strings (binary-safe)
// and writes it in one syscall.
func writeRedisCommand(w io.Writer, args [][]byte) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&buf, "$%d\r\n", len(a))
		buf.Write(a)
		buf.WriteString("\r\n")
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// readRedisReply reads one RESP2 reply. Only the types GET/SET can produce
// are handled.
func readRedisReply(br *bufio.Reader) ([]byte, error) {
	line, err := readRedisLine(br)
	if err != nil {
		return nil, err
	}
	if line == "" {
		return nil, errors.New("redis: empty reply line")
	}
	switch line[0] {
	case '+', ':':
		return []byte(line[1:]), nil
	case '-':
		return nil, &redisServerError{msg: line[1:]}
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("redis: malformed bulk length %q", line[1:])
		}
		if n < 0 {
			return nil, errNilReply
		}
		buf := make([]byte, n+2) // payload + trailing CRLF
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, fmt.Errorf("redis: read bulk payload: %w", err)
		}
		return buf[:n], nil
	default:
		return nil, fmt.Errorf("redis: unexpected reply type %q", line[0])
	}
}

func readRedisLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
