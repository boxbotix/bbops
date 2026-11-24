package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxbotix/bbops/internal/host"
	"github.com/c360studio/semengine/pkg/errs"
	"github.com/nats-io/nats.go"
)

// spec: host-runtime / Read-only SKG lexical and neural search HTTP bridge
func TestGraphSearchMiddlewareGuardsAndDispatch(t *testing.T) {
	var ready atomic.Bool
	var callCount atomic.Int32
	var lastSubject string
	var lastPayload []byte

	var requesterReply []byte
	var requesterErr error

	mw, err := host.NewUIMiddleware(t.TempDir(), ready.Load, host.WebSocketProxyRoutes{
		GraphSearch: func(_ context.Context, subject string, data []byte, _ time.Duration) ([]byte, error) {
			callCount.Add(1)
			lastSubject = subject
			lastPayload = append([]byte(nil), data...)
			return requesterReply, requesterErr
		},
	})
	if err != nil {
		t.Fatalf("NewUIMiddleware: %v", err)
	}

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unexpected fallback", http.StatusTeapot)
	}))

	// 1. Non-POST fails closed with 405 before readiness and without touching NATS.
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, host.GraphSearchFrontDoorPath, nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s pre-ready status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s Allow header = %q, want %q", method, allow, http.MethodPost)
		}
	}
	if got := callCount.Load(); got != 0 {
		t.Fatalf("NATS calls after non-POST = %d, want 0", got)
	}

	// 2. Pre-ready POST fails closed with 503 NOT READY without touching NATS.
	preRec := httptest.NewRecorder()
	preReq := httptest.NewRequest(http.MethodPost, host.GraphSearchFrontDoorPath, strings.NewReader(`{"verb":"globalSearch","params":{"query":"rover"}}`))
	handler.ServeHTTP(preRec, preReq)
	if preRec.Code != http.StatusServiceUnavailable || strings.TrimSpace(preRec.Body.String()) != "NOT READY" {
		t.Fatalf("pre-ready POST = %d %q, want 503 NOT READY", preRec.Code, preRec.Body.String())
	}
	if got := callCount.Load(); got != 0 {
		t.Fatalf("NATS calls pre-ready = %d, want 0", got)
	}

	ready.Store(true)

	// 3. Invalid verbs, unknown fields, blank parameters, out-of-bounds limits, and >64 KiB bodies fail closed with 400.
	oversizeQuery := strings.Repeat("x", host.MaxGraphSearchBodyBytes+32)
	invalidPayloads := []struct {
		name     string
		body     string
		wantCode string
	}{
		{name: "empty body", body: "", wantCode: "invalid_json"},
		{name: "array body", body: `[]`, wantCode: "invalid_json"},
		{name: "trailing json", body: `{"verb":"globalSearch","params":{"query":"ok"}}{}`, wantCode: "invalid_json"},
		{name: "unknown envelope field", body: `{"verb":"globalSearch","params":{"query":"ok"},"extra":1}`, wantCode: "invalid_json"},
		{name: "mutation verb rejected", body: `{"verb":"graph.mutation.apply","params":{"query":"ok"}}`, wantCode: "unsupported_verb"},
		{name: "pathSearch verb rejected", body: `{"verb":"pathSearch","params":{"query":"ok"}}`, wantCode: "unsupported_verb"},
		{name: "unknown inner field", body: `{"verb":"globalSearch","params":{"query":"ok","sneaky":true}}`, wantCode: "invalid_params"},
		{name: "blank globalSearch query", body: `{"verb":"globalSearch","params":{"query":"   "}}`, wantCode: "invalid_params"},
		{name: "globalSearch level out of bounds", body: `{"verb":"globalSearch","params":{"query":"ok","level":3}}`, wantCode: "invalid_params"},
		{name: "globalSearch limit negative", body: `{"verb":"globalSearch","params":{"query":"ok","limit":-1}}`, wantCode: "invalid_params"},
		{name: "globalSearch limit above max", body: `{"verb":"globalSearch","params":{"query":"ok","limit":101}}`, wantCode: "invalid_params"},
		{name: "localSearch missing entity_id", body: `{"verb":"localSearch","params":{"query":"ok"}}`, wantCode: "invalid_params"},
		{name: "localSearch missing query", body: `{"verb":"localSearch","params":{"entity_id":"e1"}}`, wantCode: "invalid_params"},
		{name: "semantic min_revision rejected", body: `{"verb":"semantic","params":{"query":"ok","limit":5,"min_revision":1}}`, wantCode: "invalid_params"},
		{name: "similar missing entity_id", body: `{"verb":"similar","params":{"limit":5}}`, wantCode: "invalid_params"},
		{name: "similar min_revision rejected", body: `{"verb":"similar","params":{"entity_id":"e1","limit":5,"min_revision":2}}`, wantCode: "invalid_params"},
		{name: "oversize body", body: `{"verb":"globalSearch","params":{"query":"` + oversizeQuery + `"}}`, wantCode: "request_too_large"},
	}

	for _, tc := range invalidPayloads {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, host.GraphSearchFrontDoorPath, strings.NewReader(tc.body))
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d (%s), want 400", tc.name, rec.Code, rec.Body.String())
		}
		var resp host.GraphSearchResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s unmarshal error body: %v", tc.name, err)
		}
		if resp.OK || resp.Error == nil || resp.Error.Code != tc.wantCode {
			t.Fatalf("%s response = %+v (err=%+v), want ok=false code=%q", tc.name, resp, resp.Error, tc.wantCode)
		}
	}
	if got := callCount.Load(); got != 0 {
		t.Fatalf("NATS calls after invalid payloads = %d, want 0", got)
	}

	// 4. All four whitelisted read-only verbs forward to exact graph.query.<verb> subjects.
	validVerbs := []struct {
		verb        string
		body        string
		wantSubject string
	}{
		{
			verb:        "globalSearch",
			body:        `{"verb":"globalSearch","params":{"query":"rover","level":1,"limit":10}}`,
			wantSubject: "graph.query.globalSearch",
		},
		{
			verb:        "localSearch",
			body:        `{"verb":"localSearch","params":{"entity_id":"boxbotix.bbops.uxs.mavlink.rover.1","query":"rover","level":0,"limit":5}}`,
			wantSubject: "graph.query.localSearch",
		},
		{
			verb:        "semantic",
			body:        `{"verb":"semantic","params":{"query":"surface vessel drift","limit":8}}`,
			wantSubject: "graph.query.semantic",
		},
		{
			verb:        "similar",
			body:        `{"verb":"similar","params":{"entity_id":"boxbotix.bbops.uxs.mavlink.boat.3","limit":6}}`,
			wantSubject: "graph.query.similar",
		},
	}

	requesterReply = []byte(`{"data":{"matched":1},"indexed_revision":42,"producer":"graph-query"}`)
	requesterErr = nil

	for _, tc := range validVerbs {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, host.GraphSearchFrontDoorPath, strings.NewReader(tc.body))
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d (%s), want 200", tc.verb, rec.Code, rec.Body.String())
		}
		if lastSubject != tc.wantSubject || !json.Valid(lastPayload) {
			t.Fatalf("%s forwarded to (%q, %s), want %q", tc.verb, lastSubject, string(lastPayload), tc.wantSubject)
		}
		var resp host.GraphSearchResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s decode response: %v", tc.verb, err)
		}
		if !resp.OK || resp.Verb != tc.verb || !bytes.Contains(resp.Result, []byte(`"indexed_revision":42`)) {
			t.Fatalf("%s response = %+v, want ok=true with indexed_revision=42", tc.verb, resp)
		}
	}

	// 5. Classified ADR-060 refusal (e.g., not_enabled) surfaces deterministically with HTTP 200 ok=false.
	requesterReply = nil
	requesterErr = errs.ClassifiedCodeDetail(
		errs.ErrorInvalid,
		"not_enabled",
		map[string]any{"embedding_enabled": false},
		errors.New("embedding search is disabled"),
	)
	ceRec := httptest.NewRecorder()
	ceReq := httptest.NewRequest(http.MethodPost, host.GraphSearchFrontDoorPath, strings.NewReader(`{"verb":"semantic","params":{"query":"beacon","limit":5}}`))
	handler.ServeHTTP(ceRec, ceReq)
	if ceRec.Code != http.StatusOK {
		t.Fatalf("classified refusal status = %d, want 200", ceRec.Code)
	}
	var ceResp host.GraphSearchResponse
	if err := json.Unmarshal(ceRec.Body.Bytes(), &ceResp); err != nil {
		t.Fatalf("decode classified refusal: %v", err)
	}
	if ceResp.OK || ceResp.Error == nil || ceResp.Error.Code != "not_enabled" {
		t.Fatalf("classified refusal = %+v (err=%+v), want code=not_enabled", ceResp, ceResp.Error)
	}

	// 6. Transport errors map deterministically (no responders -> 503, timeout -> 504).
	requesterErr = nats.ErrNoResponders
	nrRec := httptest.NewRecorder()
	handler.ServeHTTP(nrRec, httptest.NewRequest(http.MethodPost, host.GraphSearchFrontDoorPath, strings.NewReader(`{"verb":"globalSearch","params":{"query":"rover"}}`)))
	if nrRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no responders status = %d, want 503", nrRec.Code)
	}

	requesterErr = context.DeadlineExceeded
	toRec := httptest.NewRecorder()
	handler.ServeHTTP(toRec, httptest.NewRequest(http.MethodPost, host.GraphSearchFrontDoorPath, strings.NewReader(`{"verb":"globalSearch","params":{"query":"rover"}}`)))
	if toRec.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d, want 504", toRec.Code)
	}
}
