package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRedis is a minimal RESP2 server good enough for INCR/EXPIRE: it parses
// command arrays and keeps real counters, recording what it saw.
type fakeRedis struct {
	lis net.Listener

	mu      sync.Mutex
	counts  map[string]int64
	expires map[string]int64 // key -> seconds from the last EXPIRE
	conns   int
	// scripted reply: when non-empty it is sent verbatim for every command.
	cannedReply string
}

func newFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeRedis{lis: lis, counts: map[string]int64{}, expires: map[string]int64{}}
	go f.acceptLoop()
	t.Cleanup(func() { _ = lis.Close() })
	return f
}

func (f *fakeRedis) addr() string { return f.lis.Addr().String() }

func (f *fakeRedis) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func (f *fakeRedis) acceptLoop() {
	for {
		conn, err := f.lis.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns++
		f.mu.Unlock()
		go f.serve(conn)
	}
}

func (f *fakeRedis) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	for {
		args, err := readRESPCommand(br)
		if err != nil {
			return
		}
		reply := f.handle(args)
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}

func (f *fakeRedis) handle(args []string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cannedReply != "" {
		return f.cannedReply
	}
	if len(args) == 0 {
		return "-ERR empty command\r\n"
	}
	switch strings.ToUpper(args[0]) {
	case "INCR":
		f.counts[args[1]]++
		return fmt.Sprintf(":%d\r\n", f.counts[args[1]])
	case "EXPIRE":
		sec, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return "-ERR value is not an integer\r\n"
		}
		f.expires[args[1]] = sec
		return ":1\r\n"
	default:
		return fmt.Sprintf("-ERR unknown command '%s'\r\n", args[0])
	}
}

// readRESPCommand parses one client command (RESP array of bulk strings).
func readRESPCommand(br *bufio.Reader) ([]string, error) {
	header, err := respLine(br)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("want array, got %q", header)
	}
	n, err := strconv.Atoi(header[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for range n {
		sizeLine, err := respLine(br)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(sizeLine, "$") {
			return nil, fmt.Errorf("want bulk string, got %q", sizeLine)
		}
		size, err := strconv.Atoi(sizeLine[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2) // payload + CRLF
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func respLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func TestRedisCounterIncrAndExpire(t *testing.T) {
	srv := newFakeRedis(t)
	rc := newRedisCounter(srv.addr())
	ctx := context.Background()

	for want := int64(1); want <= 3; want++ {
		got, err := rc.Incr(ctx, "rl:tenant-a:123", rateWindowTTL)
		if err != nil {
			t.Fatalf("Incr: %v", err)
		}
		if got != want {
			t.Fatalf("Incr = %d, want %d", got, want)
		}
	}
	if got, err := rc.Incr(ctx, "rl:tenant-b:123", rateWindowTTL); err != nil || got != 1 {
		t.Fatalf("Incr other key = %d, %v; want 1, nil", got, err)
	}

	srv.mu.Lock()
	if sec := srv.expires["rl:tenant-a:123"]; sec != 90 {
		t.Errorf("EXPIRE seconds = %d, want 90", sec)
	}
	srv.mu.Unlock()

	// The pooled connection must be reused, not redialed per call.
	if got := srv.connCount(); got != 1 {
		t.Errorf("connections = %d, want 1 (pooling)", got)
	}
}

func TestRedisCounterConcurrent(t *testing.T) {
	srv := newFakeRedis(t)
	rc := newRedisCounter(srv.addr())
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := rc.Incr(context.Background(), "k", rateWindowTTL); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Incr: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.counts["k"] != n {
		t.Errorf("count = %d, want %d", srv.counts["k"], n)
	}
}

func TestRedisCounterErrors(t *testing.T) {
	t.Run("server down", func(t *testing.T) {
		rc := newRedisCounter("127.0.0.1:1")
		if _, err := rc.Incr(context.Background(), "k", rateWindowTTL); err == nil {
			t.Fatal("want dial error")
		}
	})

	t.Run("error reply", func(t *testing.T) {
		srv := newFakeRedis(t)
		srv.mu.Lock()
		srv.cannedReply = "-ERR oom\r\n"
		srv.mu.Unlock()
		rc := newRedisCounter(srv.addr())
		_, err := rc.Incr(context.Background(), "k", rateWindowTTL)
		if err == nil || !strings.Contains(err.Error(), "oom") {
			t.Fatalf("err = %v, want error reply surfaced", err)
		}
	})

	t.Run("protocol garbage", func(t *testing.T) {
		srv := newFakeRedis(t)
		srv.mu.Lock()
		srv.cannedReply = "?what\r\n"
		srv.mu.Unlock()
		rc := newRedisCounter(srv.addr())
		if _, err := rc.Incr(context.Background(), "k", rateWindowTTL); err == nil {
			t.Fatal("want protocol error")
		}
	})

	t.Run("context already expired", func(t *testing.T) {
		srv := newFakeRedis(t)
		rc := newRedisCounter(srv.addr())
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if _, err := rc.Incr(ctx, "k", rateWindowTTL); err == nil {
			t.Fatal("want deadline error")
		}
	})

	t.Run("connection dropped after error, next call recovers", func(t *testing.T) {
		srv := newFakeRedis(t)
		rc := newRedisCounter(srv.addr())
		srv.mu.Lock()
		srv.cannedReply = "-ERR transient\r\n"
		srv.mu.Unlock()
		if _, err := rc.Incr(context.Background(), "k", rateWindowTTL); err == nil {
			t.Fatal("want error")
		}
		srv.mu.Lock()
		srv.cannedReply = ""
		srv.mu.Unlock()
		got, err := rc.Incr(context.Background(), "k", rateWindowTTL)
		if err != nil || got != 1 {
			t.Fatalf("recovery Incr = %d, %v; want 1, nil", got, err)
		}
		if srv.connCount() != 2 {
			t.Errorf("connections = %d, want 2 (bad conn discarded)", srv.connCount())
		}
	})
}
