package connectortest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCassetteErrors(t *testing.T) {
	if _, err := LoadCassette(""); err == nil {
		t.Error("LoadCassette(\"\") returned nil error")
	}
	if _, err := LoadCassette(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("LoadCassette(missing) returned nil error")
	}

	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCassette(bad); err == nil {
		t.Error("LoadCassette(malformed) returned nil error")
	}

	// A cassette whose body_base64 is not valid base64 must fail at load.
	badB64 := filepath.Join(t.TempDir(), "badb64.json")
	if err := os.WriteFile(badB64, []byte(`{"interactions":[{"request":{"method":"GET","path":"/x"},"response":{"status":200,"body_base64":"!!!"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCassette(badB64); err == nil {
		t.Error("LoadCassette(bad base64) returned nil error")
	}

	// A cassette with an unparseable query must fail at load.
	badQ := filepath.Join(t.TempDir(), "badq.json")
	if err := os.WriteFile(badQ, []byte(`{"interactions":[{"request":{"method":"GET","path":"/x","query":"%zz"},"response":{"status":200}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCassette(badQ); err == nil {
		t.Error("LoadCassette(bad query) returned nil error")
	}
}

func TestLoadCassetteName(t *testing.T) {
	c, err := LoadCassette("testdata/notes_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "notes_contract.json" {
		t.Errorf("cassette Name = %q, want notes_contract.json", c.Name)
	}
	if len(c.Interactions) != 3 {
		t.Fatalf("got %d interactions, want 3", len(c.Interactions))
	}
}

func TestMatchByMethodPathQuery(t *testing.T) {
	c, err := LoadCassette("testdata/notes_contract.json")
	if err != nil {
		t.Fatal(err)
	}

	// page=2 must select the second interaction even though page=1 appears
	// first: matching is by query value, not order.
	in := c.match("GET", "/notes", "page=2", nil)
	if in == nil {
		t.Fatal("no match for GET /notes?page=2")
	}
	body, _ := in.Response.Bytes()
	if !contains(body, "n-3") {
		t.Errorf("page=2 returned the wrong body: %s", body)
	}

	// Wrong method does not match.
	if got := c.match("POST", "/notes", "page=1", nil); got != nil {
		t.Error("POST matched a GET interaction")
	}
	// Wrong path does not match.
	if got := c.match("GET", "/other", "page=1", nil); got != nil {
		t.Error("a wrong path matched")
	}
}

func TestMatchQueryIsOrderIndependent(t *testing.T) {
	cas := &Cassette{Name: "t", Interactions: []*Interaction{
		{Request: RecordedRequest{Method: "GET", Path: "/x", Query: "a=1&b=2"},
			Response: RecordedResponse{Status: 200, Body: "ok"}},
	}}
	if in := cas.match("GET", "/x", "b=2&a=1", nil); in == nil {
		t.Error("query set match failed for reordered keys")
	}
}

func TestMatchSequencing(t *testing.T) {
	// Two interactions share a matcher; they must be served in file order, and
	// a third identical request misses.
	c, err := LoadCassette("testdata/sequence.json")
	if err != nil {
		t.Fatal(err)
	}
	first := c.match("GET", "/poll", "", nil)
	second := c.match("GET", "/poll", "", nil)
	third := c.match("GET", "/poll", "", nil)
	if first == nil || second == nil {
		t.Fatal("expected two sequential matches")
	}
	b1, _ := first.Response.Bytes()
	b2, _ := second.Response.Bytes()
	if string(b1) != "first" || string(b2) != "second" {
		t.Errorf("sequence order wrong: %q then %q", b1, b2)
	}
	if third != nil {
		t.Error("a third identical request should miss once both interactions are consumed")
	}
}

func TestMatchBodyPin(t *testing.T) {
	cas := &Cassette{Name: "t", Interactions: []*Interaction{
		{Request: RecordedRequest{Method: "POST", Path: "/x", Body: `{"a":1}`},
			Response: RecordedResponse{Status: 200, Body: "matched-a"}},
		{Request: RecordedRequest{Method: "POST", Path: "/x", Body: `{"a":2}`},
			Response: RecordedResponse{Status: 200, Body: "matched-b"}},
	}}
	in := cas.match("POST", "/x", "", []byte(`{"a":2}`))
	if in == nil {
		t.Fatal("body pin did not disambiguate")
	}
	if b, _ := in.Response.Bytes(); string(b) != "matched-b" {
		t.Errorf("matched the wrong body-pinned interaction: %s", b)
	}
	// A body that matches neither pin misses.
	if got := cas.match("POST", "/x", "", []byte(`{"a":3}`)); got != nil {
		t.Error("a non-pinned body matched a pinned interaction")
	}
}

func TestMatchBodySHA256Pin(t *testing.T) {
	// sha256 of `{"a":1}` body.
	cas := &Cassette{Name: "t", Interactions: []*Interaction{
		{Request: RecordedRequest{Method: "POST", Path: "/x", BodySHA256: sha256Hex(`{"a":1}`)},
			Response: RecordedResponse{Status: 200, Body: "ok"}},
	}}
	if in := cas.match("POST", "/x", "", []byte(`{"a":1}`)); in == nil {
		t.Error("sha256 body pin failed to match the right body")
	}
	if got := cas.match("POST", "/x", "", []byte(`{"a":9}`)); got != nil {
		t.Error("sha256 body pin matched a different body")
	}
}

func TestBinaryBodyRoundTrip(t *testing.T) {
	c, err := LoadCassette("testdata/binary.json")
	if err != nil {
		t.Fatal(err)
	}
	in := c.match("GET", "/blob", "", nil)
	if in == nil {
		t.Fatal("no match for GET /blob")
	}
	body, err := in.Response.Bytes()
	if err != nil {
		t.Fatalf("decode binary body: %v", err)
	}
	want := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0xff, 0xfe}
	if string(body) != string(want) {
		t.Errorf("binary body = % x, want % x", body, want)
	}
}

func TestCleanPath(t *testing.T) {
	cases := map[string]string{
		"":           "/",
		"/":          "/",
		"/a//b":      "/a/b",
		"/a/./b":     "/a/b",
		"/a/b/":      "/a/b",
		"/notes":     "/notes",
		"/a//b/./c/": "/a/b/c",
	}
	for in, want := range cases {
		if got := cleanPath(in); got != want {
			t.Errorf("cleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBaseName(t *testing.T) {
	cases := map[string]string{
		"testdata/x.json":      "x.json",
		"x.json":               "x.json",
		"/a/b/c":               "c",
		`win\\path\\file.json`: "file.json",
	}
	for in, want := range cases {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetBodyChoosesEncoding(t *testing.T) {
	var r RecordedResponse
	r.setBody([]byte("hello"))
	if r.Body != "hello" || r.BodyBase64 != "" {
		t.Errorf("UTF-8 body stored wrong: body=%q b64=%q", r.Body, r.BodyBase64)
	}
	r.setBody([]byte{0xff, 0xfe})
	if r.Body != "" || r.BodyBase64 == "" {
		t.Errorf("binary body stored wrong: body=%q b64=%q", r.Body, r.BodyBase64)
	}
	r.setBody(nil)
	if r.Body != "" || r.BodyBase64 != "" {
		t.Errorf("empty body should clear both fields: body=%q b64=%q", r.Body, r.BodyBase64)
	}
}
