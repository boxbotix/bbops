package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/c360studio/semengine/graph"
	"github.com/c360studio/semengine/natsclient"
	"github.com/c360studio/semengine/pkg/errs"
)

const (
	// GraphSearchFrontDoorPath is the read-only HTTP POST endpoint bridging
	// browser and MCP SKG queries to SemEngine graph.query.<verb> subjects.
	GraphSearchFrontDoorPath = "/graph-gateway/search"

	// MaxGraphSearchBodyBytes caps inbound POST /graph-gateway/search bodies at 64 KiB.
	MaxGraphSearchBodyBytes = 64 * 1024

	// DefaultGraphSearchTimeout bounds each NATS graph.query.<verb> request.
	DefaultGraphSearchTimeout = 5 * time.Second
)

// GraphRequester executes a single non-retrying NATS request/reply with ADR-060
// classified error decoding (matching natsclient.Client.RequestClassified).
type GraphRequester func(ctx context.Context, subject string, data []byte, timeout time.Duration) ([]byte, error)

// GraphSearchRequest is the strict inbound JSON envelope for POST /graph-gateway/search.
type GraphSearchRequest struct {
	Verb   string          `json:"verb"`
	Params json.RawMessage `json:"params"`
}

// GraphSearchResponse is the deterministic JSON envelope returned by POST /graph-gateway/search.
type GraphSearchResponse struct {
	OK     bool              `json:"ok"`
	Verb   string            `json:"verb,omitempty"`
	Result json.RawMessage   `json:"result,omitempty"`
	Error  *GraphSearchError `json:"error,omitempty"`
}

// GraphSearchError reports structured validation, transport, or ADR-060 handler refusals.
type GraphSearchError struct {
	Class   string         `json:"class,omitempty"`
	Code    string         `json:"code,omitempty"`
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
}

func serveGraphSearch(
	w http.ResponseWriter,
	r *http.Request,
	requester GraphRequester,
	shutdownCtx context.Context,
) {
	limited := http.MaxBytesReader(w, r.Body, MaxGraphSearchBodyBytes)
	defer func() { _ = limited.Close() }()

	rawBody, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeGraphSearchError(w, http.StatusBadRequest, "", "invalid", "request_too_large", "request body exceeds 64 KiB limit", nil)
			return
		}
		writeGraphSearchError(w, http.StatusBadRequest, "", "invalid", "bad_request", "failed to read request body", nil)
		return
	}

	var envelope GraphSearchRequest
	if err := decodeStrictJSONObject(rawBody, &envelope); err != nil {
		writeGraphSearchError(w, http.StatusBadRequest, "", "invalid", "invalid_json", err.Error(), nil)
		return
	}

	subject, payload, err := buildGraphQueryPayload(envelope.Verb, envelope.Params)
	if err != nil {
		code := "invalid_params"
		if errors.Is(err, errUnsupportedGraphVerb) {
			code = "unsupported_verb"
		}
		writeGraphSearchError(w, http.StatusBadRequest, envelope.Verb, "invalid", code, err.Error(), nil)
		return
	}

	reqCtx := r.Context()
	if shutdownCtx != nil {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithCancel(reqCtx)
		stopCancel := context.AfterFunc(shutdownCtx, cancel)
		defer func() {
			stopCancel()
			cancel()
		}()
	}

	replyBytes, err := requester(reqCtx, subject, payload, DefaultGraphSearchTimeout)
	if err != nil {
		var ce *errs.ClassifiedError
		if errors.As(err, &ce) {
			msg := strings.TrimSpace(ce.Message)
			if msg == "" {
				msg = ce.Error()
			}
			writeGraphSearchJSON(w, http.StatusOK, GraphSearchResponse{
				OK:   false,
				Verb: envelope.Verb,
				Error: &GraphSearchError{
					Class:   strings.ToLower(ce.Class.String()),
					Code:    ce.Code,
					Message: msg,
					Detail:  ce.Detail,
				},
			})
			return
		}
		if natsclient.IsNoResponders(err) {
			writeGraphSearchError(w, http.StatusServiceUnavailable, envelope.Verb, "transient", "no_responders", "graph query responder unavailable", nil)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeGraphSearchError(w, http.StatusGatewayTimeout, envelope.Verb, "transient", "timeout", "graph query timed out", nil)
			return
		}
		writeGraphSearchError(w, http.StatusBadGateway, envelope.Verb, "transient", "upstream_error", "graph query transport failed", nil)
		return
	}

	if !json.Valid(replyBytes) {
		writeGraphSearchError(w, http.StatusBadGateway, envelope.Verb, "transient", "invalid_upstream_json", "graph query returned invalid JSON", nil)
		return
	}

	writeGraphSearchJSON(w, http.StatusOK, GraphSearchResponse{
		OK:     true,
		Verb:   envelope.Verb,
		Result: json.RawMessage(replyBytes),
	})
}

var errUnsupportedGraphVerb = errors.New("unsupported graph search verb")

func buildGraphQueryPayload(verb string, rawParams json.RawMessage) (string, []byte, error) {
	switch verb {
	case "globalSearch":
		var req graph.GlobalSearchRequest
		if err := decodeStrictJSONObject(rawParams, &req); err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(req.Query) == "" {
			return "", nil, errors.New("query is required")
		}
		if req.Level < 0 || req.Level > 2 {
			return "", nil, fmt.Errorf("level %d out of range [0..2]", req.Level)
		}
		if req.Limit < 0 || req.Limit > graph.MaxSimilarityLimit {
			return "", nil, fmt.Errorf("limit %d out of range [0..%d]", req.Limit, graph.MaxSimilarityLimit)
		}
		encoded, err := json.Marshal(req)
		return "graph.query.globalSearch", encoded, err

	case "localSearch":
		var req graph.LocalSearchRequest
		if err := decodeStrictJSONObject(rawParams, &req); err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(req.EntityID) == "" {
			return "", nil, errors.New("entity_id is required")
		}
		if strings.TrimSpace(req.Query) == "" {
			return "", nil, errors.New("query is required")
		}
		if req.Level < 0 || req.Level > 2 {
			return "", nil, fmt.Errorf("level %d out of range [0..2]", req.Level)
		}
		if req.Limit < 0 || req.Limit > graph.MaxSimilarityLimit {
			return "", nil, fmt.Errorf("limit %d out of range [0..%d]", req.Limit, graph.MaxSimilarityLimit)
		}
		encoded, err := json.Marshal(req)
		return "graph.query.localSearch", encoded, err

	case "semantic":
		var req graph.SemanticRequest
		if err := decodeStrictJSONObject(rawParams, &req); err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(req.Query) == "" {
			return "", nil, errors.New("query is required")
		}
		if req.Limit < 0 || req.Limit > graph.MaxSimilarityLimit {
			return "", nil, fmt.Errorf("limit %d out of range [0..%d]", req.Limit, graph.MaxSimilarityLimit)
		}
		if req.MinRevision > 0 {
			return "", nil, errors.New("min_revision is not supported on semantic search")
		}
		encoded, err := json.Marshal(req)
		return "graph.query.semantic", encoded, err

	case "similar":
		var req graph.SimilarRequest
		if err := decodeStrictJSONObject(rawParams, &req); err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(req.EntityID) == "" {
			return "", nil, errors.New("entity_id is required")
		}
		if req.Limit < 0 || req.Limit > graph.MaxSimilarityLimit {
			return "", nil, fmt.Errorf("limit %d out of range [0..%d]", req.Limit, graph.MaxSimilarityLimit)
		}
		if req.MinRevision > 0 {
			return "", nil, errors.New("min_revision is not supported on similar search")
		}
		encoded, err := json.Marshal(req)
		return "graph.query.similar", encoded, err

	default:
		return "", nil, fmt.Errorf("%w %q", errUnsupportedGraphVerb, verb)
	}
}

func decodeStrictJSONObject(raw []byte, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("expected JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON content")
	}
	return nil
}

func writeGraphSearchError(
	w http.ResponseWriter,
	status int,
	verb string,
	class string,
	code string,
	message string,
	detail map[string]any,
) {
	writeGraphSearchJSON(w, status, GraphSearchResponse{
		OK:   false,
		Verb: verb,
		Error: &GraphSearchError{
			Class:   class,
			Code:    code,
			Message: message,
			Detail:  detail,
		},
	})
}

func writeGraphSearchJSON(w http.ResponseWriter, status int, payload GraphSearchResponse) {
	encoded, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}
