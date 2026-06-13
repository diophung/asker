package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// fakeRedis is a minimal RESP2 server speaking just enough protocol for the
// GET / SET ... EX commands the cache issues. Handshake commands go-redis
// sends on connect (HELLO, CLIENT SETINFO) fall through to the -ERR branch,
// which the client tolerates by design (RESP2 fallback).
type fakeRedis struct {
	lis net.Listener

	mu   sync.Mutex
	data map[string][]byte
	ttls map[string]int64
	sets int
	gets int
}

func startFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeRedis{lis: lis, data: make(map[string][]byte), ttls: make(map[string]int64)}
	go f.serve()
	t.Cleanup(func() { _ = lis.Close() })
	return f
}

func (f *fakeRedis) addr() string { return f.lis.Addr().String() }

func (f *fakeRedis) serve() {
	for {
		conn, err := f.lis.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeRedis) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	for {
		args, err := readRESPCommand(br)
		if err != nil {
			return
		}
		switch strings.ToUpper(string(args[0])) {
		case "GET":
			f.mu.Lock()
			f.gets++
			val, ok := f.data[string(args[1])]
			f.mu.Unlock()
			if !ok {
				_, _ = conn.Write([]byte("$-1\r\n"))
				continue
			}
			_, _ = fmt.Fprintf(conn, "$%d\r\n", len(val))
			_, _ = conn.Write(val)
			_, _ = conn.Write([]byte("\r\n"))
		case "SET":
			if len(args) != 5 || strings.ToUpper(string(args[3])) != "EX" {
				_, _ = fmt.Fprintf(conn, "-ERR unsupported SET shape\r\n")
				continue
			}
			ttl, err := strconv.ParseInt(string(args[4]), 10, 64)
			if err != nil {
				_, _ = fmt.Fprintf(conn, "-ERR bad EX value\r\n")
				continue
			}
			f.mu.Lock()
			f.sets++
			f.data[string(args[1])] = append([]byte(nil), args[2]...)
			f.ttls[string(args[1])] = ttl
			f.mu.Unlock()
			_, _ = conn.Write([]byte("+OK\r\n"))
		default:
			_, _ = fmt.Fprintf(conn, "-ERR unknown command\r\n")
		}
	}
}

func readRESPCommand(br *bufio.Reader) ([][]byte, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 || line[0] != '*' {
		return nil, fmt.Errorf("bad array header %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([][]byte, 0, n)
	for range n {
		hdr, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		if len(hdr) == 0 || hdr[0] != '$' {
			return nil, fmt.Errorf("bad bulk header %q", hdr)
		}
		size, err := strconv.Atoi(hdr[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		args = append(args, buf[:size])
	}
	return args, nil
}

func TestRedisCacheRoundTrip(t *testing.T) {
	f := startFakeRedis(t)
	c := newRedisCache(f.addr())
	defer c.Close()
	ctx := context.Background()

	// Miss first.
	if _, ok, err := c.Get(ctx, "q:t:abc"); err != nil || ok {
		t.Fatalf("Get(miss) = ok=%v err=%v, want miss with nil error", ok, err)
	}

	// Binary-safe value (embedded CRLF and NUL like proto bytes can carry).
	val := []byte("binary\r\nvalue\x00with junk")
	if err := c.Set(ctx, "q:t:abc", val, cacheTTL); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(ctx, "q:t:abc")
	if err != nil || !ok {
		t.Fatalf("Get(hit) = ok=%v err=%v, want hit", ok, err)
	}
	if !bytes.Equal(got, val) {
		t.Errorf("Get = %q, want %q", got, val)
	}

	f.mu.Lock()
	ttl := f.ttls["q:t:abc"]
	f.mu.Unlock()
	if ttl != 60 {
		t.Errorf("SET EX ttl = %d, want 60", ttl)
	}
}

func TestRedisCacheEmptyValue(t *testing.T) {
	f := startFakeRedis(t)
	c := newRedisCache(f.addr())
	defer c.Close()
	ctx := context.Background()

	if err := c.Set(ctx, "k", nil, time.Second); err != nil {
		t.Fatalf("Set(empty): %v", err)
	}
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get = ok=%v err=%v, want present", ok, err)
	}
	if len(got) != 0 {
		t.Errorf("Get = %q, want empty", got)
	}
}

func TestRedisCacheServerErrorSurfaces(t *testing.T) {
	f := startFakeRedis(t)
	c := newRedisCache(f.addr())
	defer c.Close()

	// A zero TTL makes go-redis send a plain SET, which the fake rejects with
	// -ERR; the server error must surface so the caller can skip the cache.
	if err := c.Set(context.Background(), "k", []byte("v"), 0); err == nil {
		t.Fatal("server error reply must surface from Set")
	}

	// The connection survives a protocol-level error and is reused.
	if err := c.Set(context.Background(), "k", []byte("v"), time.Second); err != nil {
		t.Fatalf("Set after server error: %v", err)
	}
}

func TestRedisCacheDownReturnsError(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close() // nothing listens here anymore

	c := newRedisCache(addr)
	defer c.Close()
	if _, _, err := c.Get(context.Background(), "k"); err == nil {
		t.Error("Get against a down Redis must return an error")
	}
	if err := c.Set(context.Background(), "k", []byte("v"), time.Second); err == nil {
		t.Error("Set against a down Redis must return an error")
	}
}

func TestCacheKey(t *testing.T) {
	base := func() *queryv1.SearchRequest {
		return normalizeRequest(&queryv1.SearchRequest{
			Query:       "quarterly plans",
			DocTypes:    []askerv1.DocType{askerv1.DocType_EMAIL, askerv1.DocType_FILE},
			FromDate:    timestamppb.New(time.Unix(1718000000, 0)),
			Participant: "alice",
			Limit:       20,
		})
	}

	k1 := cacheKey("tenant-a", base(), true)
	if !strings.HasPrefix(k1, "q:tenant-a:") {
		t.Errorf("key = %q, want q:tenant-a: prefix", k1)
	}
	if k2 := cacheKey("tenant-a", base(), true); k2 != k1 {
		t.Errorf("identical requests produced different keys: %q vs %q", k1, k2)
	}

	// Doc-type order must not matter.
	reordered := base()
	reordered.DocTypes = []askerv1.DocType{askerv1.DocType_FILE, askerv1.DocType_EMAIL}
	if k := cacheKey("tenant-a", reordered, true); k != k1 {
		t.Error("doc-type order changed the cache key")
	}

	// Whether the CLIP arm ran must change the key: a CLIP-less result must
	// never be served for a request that now plans the CLIP arm.
	if k := cacheKey("tenant-a", base(), false); k == k1 {
		t.Error("the CLIP-arm flag did not change the cache key")
	}

	// Every dimension must change the key.
	variants := map[string]*queryv1.SearchRequest{}
	v := base()
	v.Query = "other"
	variants["query"] = v
	v = base()
	v.DocTypes = nil
	variants["types"] = v
	v = base()
	v.FromDate = nil
	variants["from"] = v
	v = base()
	v.ToDate = timestamppb.New(time.Unix(1718000001, 0))
	variants["to"] = v
	v = base()
	v.Participant = "bob"
	variants["participant"] = v
	v = base()
	v.Limit = 50
	variants["limit"] = v
	v = base()
	v.Offset = 20
	variants["offset"] = v
	v = base()
	v.Mode = queryv1.SearchMode_KEYWORD
	variants["mode"] = v
	for name, req := range variants {
		if k := cacheKey("tenant-a", req, true); k == k1 {
			t.Errorf("changing %s did not change the cache key", name)
		}
	}

	// Different tenants never share keys, even for identical requests.
	if k := cacheKey("tenant-b", base(), true); k == k1 {
		t.Error("different tenants share a cache key")
	}
}
