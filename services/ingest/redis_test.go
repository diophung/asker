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

// fakeRedis is a scripted RESP2 server: it parses incoming commands and
// answers from a queue of canned replies (the special reply "CLOSE" drops the
// connection instead of answering, simulating a server-side idle disconnect).
type fakeRedis struct {
	ln net.Listener

	mu      sync.Mutex
	replies []string
	cmds    [][]string
}

func newFakeRedis(t *testing.T, replies ...string) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeRedis{ln: ln, replies: replies}
	t.Cleanup(func() { _ = ln.Close() })
	go f.acceptLoop()
	return f
}

func (f *fakeRedis) addr() string { return f.ln.Addr().String() }

func (f *fakeRedis) acceptLoop() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serve(conn)
	}
}

func (f *fakeRedis) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	for {
		cmd, err := readRESPCommand(br)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.cmds = append(f.cmds, cmd)
		reply := "+OK\r\n"
		if len(f.replies) > 0 {
			reply = f.replies[0]
			f.replies = f.replies[1:]
		}
		f.mu.Unlock()
		if reply == "CLOSE" {
			return
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}

func (f *fakeRedis) commands() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.cmds))
	copy(out, f.cmds)
	return out
}

// readRESPCommand parses one RESP array-of-bulk-strings command.
func readRESPCommand(br *bufio.Reader) ([]string, error) {
	header, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	header = strings.TrimRight(header, "\r\n")
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("unexpected command header %q", header)
	}
	n, err := strconv.Atoi(header[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		sizeLine, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeLine = strings.TrimRight(sizeLine, "\r\n")
		if !strings.HasPrefix(sizeLine, "$") {
			return nil, fmt.Errorf("unexpected bulk header %q", sizeLine)
		}
		size, err := strconv.Atoi(sizeLine[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2) // payload + \r\n
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

func TestRedisSeenSetNXWireFormat(t *testing.T) {
	t.Parallel()
	srv := newFakeRedis(t, "+OK\r\n", "$-1\r\n")
	r := newRedisSeen(srv.addr())
	defer func() { _ = r.Close() }()
	ctx := context.Background()

	fresh, err := r.SetNX(ctx, "ingest:seen:doc-1:etag-1", 48*time.Hour)
	if err != nil {
		t.Fatalf("SetNX: %v", err)
	}
	if !fresh {
		t.Error("first SetNX = false, want true")
	}

	fresh, err = r.SetNX(ctx, "ingest:seen:doc-1:etag-1", 48*time.Hour)
	if err != nil {
		t.Fatalf("second SetNX: %v", err)
	}
	if fresh {
		t.Error("second SetNX = true, want false (key already set)")
	}

	cmds := srv.commands()
	if len(cmds) != 2 {
		t.Fatalf("server saw %d commands, want 2", len(cmds))
	}
	want := []string{"SET", "ingest:seen:doc-1:etag-1", "1", "NX", "EX", "172800"}
	for i, cmd := range cmds {
		if len(cmd) != len(want) {
			t.Fatalf("command %d = %v, want %v", i, cmd, want)
		}
		for j := range want {
			if cmd[j] != want[j] {
				t.Errorf("command %d arg %d = %q, want %q", i, j, cmd[j], want[j])
			}
		}
	}
}

func TestRedisSeenResp3Null(t *testing.T) {
	t.Parallel()
	srv := newFakeRedis(t, "_\r\n")
	r := newRedisSeen(srv.addr())
	defer func() { _ = r.Close() }()

	fresh, err := r.SetNX(context.Background(), "k", time.Hour)
	if err != nil {
		t.Fatalf("SetNX: %v", err)
	}
	if fresh {
		t.Error("SetNX = true on RESP3 null, want false")
	}
}

func TestRedisSeenServerError(t *testing.T) {
	t.Parallel()
	srv := newFakeRedis(t, "-ERR wrong number of arguments\r\n")
	r := newRedisSeen(srv.addr())
	defer func() { _ = r.Close() }()

	_, err := r.SetNX(context.Background(), "k", time.Hour)
	if err == nil || !strings.Contains(err.Error(), "wrong number of arguments") {
		t.Errorf("SetNX = %v, want server error surfaced", err)
	}
}

func TestRedisSeenUnexpectedReply(t *testing.T) {
	t.Parallel()
	srv := newFakeRedis(t, ":1\r\n")
	r := newRedisSeen(srv.addr())
	defer func() { _ = r.Close() }()

	_, err := r.SetNX(context.Background(), "k", time.Hour)
	if err == nil || !strings.Contains(err.Error(), "unexpected reply") {
		t.Errorf("SetNX = %v, want unexpected-reply error", err)
	}
}

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
	if _, err := r.SetNX(context.Background(), "k", time.Hour); err == nil {
		t.Error("SetNX against a dead address: want error (handler fails open on it)")
	}
}

// TestRedisSeenReconnectsAfterServerClose simulates a server-side idle
// disconnect: the first command on the stale pooled connection fails and the
// client must transparently retry on a fresh connection.
func TestRedisSeenReconnectsAfterServerClose(t *testing.T) {
	t.Parallel()
	srv := newFakeRedis(t, "+OK\r\n", "CLOSE", "+OK\r\n")
	r := newRedisSeen(srv.addr())
	defer func() { _ = r.Close() }()
	ctx := context.Background()

	if _, err := r.SetNX(ctx, "k1", time.Hour); err != nil {
		t.Fatalf("first SetNX: %v", err)
	}
	// Server drops the connection on the next command; the retry dials fresh.
	fresh, err := r.SetNX(ctx, "k2", time.Hour)
	if err != nil {
		t.Fatalf("SetNX after server close: %v", err)
	}
	if !fresh {
		t.Error("SetNX after reconnect = false, want true")
	}
}
