package main

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	documentv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// TestSearchTenantChokepointAndShape is THE chokepoint property test: the
// tenant seen by the QueryService (through the real client+server tenancy
// interceptors) must equal the verified JWT subject — even when the caller
// tries to smuggle a different tenant via headers — and the REST response
// must match the pinned shape exactly.
func TestSearchTenantChokepointAndShape(t *testing.T) {
	env := newTestEnv(t)
	created := time.Date(2026, 5, 1, 10, 30, 0, 0, time.UTC)
	modified := time.Date(2026, 5, 2, 8, 0, 0, 0, time.UTC)
	env.query.resp = &queryv1.SearchResponse{
		Hits: []*queryv1.Hit{{
			DocId:       "doc-1",
			ConnectorId: "gmail",
			Type:        documentv1.DocType_EMAIL,
			Title:       "Quarterly report",
			Snippet:     "the <hi>report</hi> is ready",
			Score:       0.875,
			Created:     timestamppb.New(created),
			Modified:    timestamppb.New(modified),
			Metadata:    map[string]string{"thread_id": "t-1"},
		}},
		Total:    42,
		Degraded: "keyword-only",
		TookMs:   12,
		Cached:   true,
	}

	rec := env.do(http.MethodGet,
		"/v1/search?q=hello+world&types=EMAIL,CALENDAR_EVENT&from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z&participant=alice%40example.com&limit=5&offset=10&mode=keyword",
		nil,
		http.Header{
			// Attacker-controlled tenant headers must be ignored end to end.
			"X-Asker-Tenant": []string{"attacker-tenant"},
			"X-Tenant-Id":    []string{"attacker-tenant"},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	gotTenant, gotReq := env.query.captured()
	if string(gotTenant) != testSubject {
		t.Errorf("tenant at QueryService = %q, want JWT subject %q", gotTenant, testSubject)
	}
	wantReq := &queryv1.SearchRequest{
		Query:       "hello world",
		DocTypes:    []documentv1.DocType{documentv1.DocType_EMAIL, documentv1.DocType_CALENDAR_EVENT},
		FromDate:    timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		ToDate:      timestamppb.New(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)),
		Participant: "alice@example.com",
		Limit:       5,
		Offset:      10,
		Mode:        queryv1.SearchMode_KEYWORD,
	}
	if !proto.Equal(gotReq, wantReq) {
		t.Errorf("SearchRequest =\n%v\nwant\n%v", gotReq, wantReq)
	}

	want := map[string]any{
		"hits": []any{map[string]any{
			"doc_id":        "doc-1",
			"connector_id":  "gmail",
			"type":          "EMAIL",
			"title":         "Quarterly report",
			"snippet":       "the <hi>report</hi> is ready",
			"score":         0.875,
			"created":       "2026-05-01T10:30:00Z",
			"modified":      "2026-05-02T08:00:00Z",
			"metadata":      map[string]any{"thread_id": "t-1"},
			"start_ms":      float64(0),
			"end_ms":        float64(0),
			"modality":      "",
			"thumbnail_key": "",
		}},
		"total":    float64(42),
		"degraded": "keyword-only",
		"took_ms":  float64(12),
		"cached":   true,
	}
	got := decodeObject(t, rec)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("response =\n%#v\nwant\n%#v", got, want)
	}
}

func TestSearchDefaultsAndEmptyHits(t *testing.T) {
	env := newTestEnv(t)
	env.query.resp = &queryv1.SearchResponse{TookMs: 3}

	rec := env.do(http.MethodGet, "/v1/search?q=nothing", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// hits must be [] (never null), metadata defaults exercised elsewhere.
	if !strings.Contains(rec.Body.String(), `"hits":[]`) {
		t.Errorf("body = %s, want \"hits\":[]", rec.Body.String())
	}
	_, gotReq := env.query.captured()
	if gotReq.GetMode() != queryv1.SearchMode_HYBRID {
		t.Errorf("default mode = %v, want HYBRID", gotReq.GetMode())
	}
	if gotReq.GetLimit() != 0 || gotReq.GetOffset() != 0 {
		t.Errorf("limit/offset = %d/%d, want 0/0 (query service applies defaults)", gotReq.GetLimit(), gotReq.GetOffset())
	}
	if len(gotReq.GetDocTypes()) != 0 || gotReq.GetFromDate() != nil || gotReq.GetToDate() != nil {
		t.Errorf("unexpected filters in %v", gotReq)
	}
}

func TestSearchNilMetadataBecomesEmptyObject(t *testing.T) {
	env := newTestEnv(t)
	env.query.resp = &queryv1.SearchResponse{
		Hits: []*queryv1.Hit{{DocId: "d", Type: documentv1.DocType_FILE}},
	}
	rec := env.do(http.MethodGet, "/v1/search?q=x", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"metadata":{}`) {
		t.Errorf("body = %s, want \"metadata\":{}", body)
	}
	// nil timestamps render as empty strings, keys still present.
	if !strings.Contains(body, `"created":""`) || !strings.Contains(body, `"modified":""`) {
		t.Errorf("body = %s, want empty created/modified", body)
	}
}

func TestSearchBadParams(t *testing.T) {
	env := newTestEnv(t)
	for name, query := range map[string]string{
		"unknown type":          "q=x&types=BOGUS",
		"unspecified type":      "q=x&types=DOC_TYPE_UNSPECIFIED",
		"empty type in list":    "q=x&types=EMAIL,",
		"lowercase type":        "q=x&types=email",
		"bad from":              "q=x&from=yesterday",
		"bad to":                "q=x&to=2026-13-45",
		"bad mode":              "q=x&mode=fuzzy",
		"uppercase mode":        "q=x&mode=HYBRID",
		"non-integer limit":     "q=x&limit=ten",
		"negative limit":        "q=x&limit=-1",
		"non-integer offset":    "q=x&offset=2.5",
		"negative offset":       "q=x&offset=-9",
		"limit above int32 max": "q=x&limit=2147483648",
	} {
		t.Run(name, func(t *testing.T) {
			rec := env.do(http.MethodGet, "/v1/search?"+query, nil, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			if decodeObject(t, rec)["error"] == "" {
				t.Error("missing error message")
			}
		})
	}
}

func TestSearchUpstreamErrorMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		wantStatus int
	}{
		"unavailable -> 502":       {status.Error(codes.Unavailable, "down"), http.StatusBadGateway},
		"invalid argument -> 400":  {status.Error(codes.InvalidArgument, "bad query"), http.StatusBadRequest},
		"resource exhausted->429":  {status.Error(codes.ResourceExhausted, "slow down"), http.StatusTooManyRequests},
		"deadline -> 504":          {status.Error(codes.DeadlineExceeded, "late"), http.StatusGatewayTimeout},
		"unauthenticated -> 500":   {status.Error(codes.Unauthenticated, "tenant"), http.StatusInternalServerError},
		"internal default -> 502":  {status.Error(codes.Internal, "boom"), http.StatusBadGateway},
		"not a grpc status -> 502": {errors.New("plain failure"), http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			env.query.err = tc.err
			rec := env.do(http.MethodGet, "/v1/search?q=x", nil, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if decodeObject(t, rec)["error"] == "" {
				t.Error("missing error message")
			}
		})
	}
}

func TestSearchRequiresAuth(t *testing.T) {
	env := newTestEnv(t)
	req, rec := newRawRequest(http.MethodGet, "/v1/search?q=x")
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if gotTenant, _ := env.query.captured(); gotTenant != "" {
		t.Errorf("QueryService was reached without auth (tenant %q)", gotTenant)
	}
}
