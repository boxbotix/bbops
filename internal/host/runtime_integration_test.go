package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/boxbotix/bbops/internal/host"
	"github.com/c360studio/semengine/graph"
	"github.com/c360studio/semengine/message"
	"github.com/c360studio/semengine/payloadregistry"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go/jetstream"
)

type testVehiclePayload struct {
	ID       string    `json:"id"`
	Domain   string    `json:"domain"`
	Callsign string    `json:"callsign"`
	Observed time.Time `json:"observed"`
}

func (p *testVehiclePayload) Schema() message.Type {
	return message.Type{Domain: "test", Category: "vehicle", Version: "v1"}
}

func (p *testVehiclePayload) Validate() error {
	if p.ID == "" {
		return errors.New("vehicle id required")
	}
	return nil
}

func (p *testVehiclePayload) MarshalJSON() ([]byte, error) {
	type alias testVehiclePayload
	return json.Marshal((*alias)(p))
}

func (p *testVehiclePayload) UnmarshalJSON(data []byte) error {
	type alias testVehiclePayload
	return json.Unmarshal(data, (*alias)(p))
}

func (p *testVehiclePayload) EntityID() string { return p.ID }

func (p *testVehiclePayload) Triples() []message.Triple {
	if p.ID == "" {
		return nil
	}
	ts := p.Observed
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return []message.Triple{
		{
			Subject:    p.ID,
			Predicate:  "test.vehicle.domain",
			Object:     p.Domain,
			Source:     "host-integration-test",
			Timestamp:  ts,
			Confidence: 1.0,
		},
		{
			Subject:    p.ID,
			Predicate:  "test.vehicle.callsign",
			Object:     p.Callsign,
			Source:     "host-integration-test",
			Timestamp:  ts,
			Confidence: 1.0,
		},
	}
}

var _ graph.Graphable = (*testVehiclePayload)(nil)

type wsEnvelope struct {
	Type      string          `json:"type"`
	Subject   string          `json:"subject"`
	Bucket    string          `json:"bucket"`
	Key       string          `json:"key"`
	Revision  uint64          `json:"revision"`
	Timestamp int64           `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
// spec: host-runtime / ADR-115 GraphQL front door and dual output/websocket streaming planes
func TestRuntimeEndToEndSKGAndDualWebSockets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	uiDir := t.TempDir()
	const wantIndex = "<!doctype html><title>bbops COP</title>"
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte(wantIndex), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	rt, err := host.StartEphemeral(ctx, host.Options{
		DataDir: t.TempDir(),
		UIDir:   uiDir,
		Version: "v0.2.0-test",
		RegisterPayloads: func(reg *payloadregistry.Registry) error {
			return reg.Register(&payloadregistry.Registration{
				Factory:     func() any { return &testVehiclePayload{} },
				Domain:      "test",
				Category:    "vehicle",
				Version:     "v1",
				Description: "In-process M0.2 host integration test entity payload",
			})
		},
	}, 3)
	if err != nil {
		t.Fatalf("StartEphemeral: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if stopErr := rt.Stop(stopCtx); stopErr != nil {
			t.Errorf("Stop: %v", stopErr)
		}
	})

	endpoints := rt.Endpoints()

	// 1. Verify /healthz (liveness) immediately, wait for /readyz (ADR-109 readiness across all components), and verify static UI fallback.
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/healthz", http.StatusOK, "OK")
	waitForCondition(t, 15*time.Second, "/readyz returns 200 READY", func() bool {
		resp, getErr := http.Get(endpoints.HTTPBaseURL + "/readyz") //nolint:noctx
		if getErr != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK && string(body) == "READY"
	})
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/", http.StatusOK, wantIndex)

	// 2. Independently verify JetStream ENTITY stream bounds and ENTITY_STATES KV History: 1 compaction.
	js, err := rt.NATSClient().JetStream()
	if err != nil {
		t.Fatalf("JetStream handle: %v", err)
	}
	stream, err := js.Stream(ctx, host.EntityStreamName)
	if err != nil {
		t.Fatalf("lookup ENTITY stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("ENTITY stream info: %v", err)
	}
	if info.Config.MaxBytes != 268435456 || info.Config.MaxAge != 24*time.Hour || info.Config.Discard != jetstream.DiscardOld {
		t.Fatalf("ENTITY stream config = %+v, want max_bytes=268435456 max_age=24h discard=old", info.Config)
	}

	kv, err := rt.NATSClient().GetKeyValueBucket(ctx, graph.BucketEntityStates)
	if err != nil {
		t.Fatalf("lookup ENTITY_STATES KV: %v", err)
	}
	kvStatus, err := kv.Status(ctx)
	if err != nil {
		t.Fatalf("ENTITY_STATES status: %v", err)
	}
	if kvStatus.History() != 1 || kvStatus.TTL() != 0 {
		t.Fatalf("ENTITY_STATES history=%d ttl=%v, want history=1 ttl=0", kvStatus.History(), kvStatus.TTL())
	}

	// 3. Publish a test entity through entity.> -> graph-ingest -> ENTITY_STATES.
	platform := rt.Platform()
	entityPrefix := fmt.Sprintf("%s.%s.uxs.mavlink.drone", platform.Org, platform.Platform)
	entityID := entityPrefix + ".alpha1"
	payload := &testVehiclePayload{
		ID:       entityID,
		Domain:   "Quadcopter",
		Callsign: "ALPHA-1",
		Observed: time.Now().UTC().Truncate(time.Millisecond),
	}
	wireBytes, err := json.Marshal(message.NewBaseMessage(payload.Schema(), payload, "host-integration-test"))
	if err != nil {
		t.Fatalf("marshal BaseMessage: %v", err)
	}
	if err := rt.NATSClient().PublishToStream(ctx, "entity."+entityID, wireBytes); err != nil {
		t.Fatalf("PublishToStream entity: %v", err)
	}

	// Wait until ENTITY_STATES materializes the entity via graph-ingest.
	waitForCondition(t, 10*time.Second, "entity materialized in ENTITY_STATES", func() bool {
		_, getErr := kv.Get(ctx, entityID)
		return getErr == nil
	})

	// 4. Dial output/websocket #2 (ENTITY_STATES kv-watch) and verify snapshot `put` + `synced`.
	graphConn := dialWebSocketWithRetry(ctx, t, endpoints.GraphWS)
	defer func() { _ = graphConn.Close() }()

	sawPut := false
	sawSynced := false
	for !sawSynced {
		frame := readWSFrame(t, graphConn, 10*time.Second)
		switch frame.Type {
		case "put":
			if frame.Bucket == graph.BucketEntityStates && frame.Key == entityID {
				var stored struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(frame.Payload, &stored); err != nil {
					t.Fatalf("decode WS #2 put payload: %v", err)
				}
				if stored.ID != entityID {
					t.Fatalf("WS #2 put payload id = %q, want %q", stored.ID, entityID)
				}
				sawPut = true
			}
		case "synced":
			if frame.Bucket != graph.BucketEntityStates {
				t.Fatalf("WS #2 synced bucket = %q, want %s", frame.Bucket, graph.BucketEntityStates)
			}
			sawSynced = true
		}
	}
	if !sawPut {
		t.Fatalf("WS #2 reached synced without delivering snapshot put for %s", entityID)
	}

	// 5. Query POST /graph-gateway/graphql for exact entity and prefix match (once graph-index coalesces).
	queryVehicleGraphQL(ctx, t, endpoints.HTTPBaseURL+"/graph-gateway/graphql", entityID, entityPrefix)

	// 6. Dial output/websocket #1 (bbops.telemetry.>) and verify rapid frames arrive uncoalesced in order.
	telemetryConn := dialWebSocketWithRetry(ctx, t, endpoints.TelemetryWS)
	defer func() { _ = telemetryConn.Close() }()

	nc := rt.NATSClient().GetConnection()
	if nc == nil {
		t.Fatal("expected underlying NATS connection")
	}
	for seq := 1; seq <= 2; seq++ {
		frameBytes := []byte(fmt.Sprintf(`{"seq":%d,"roll_rad":0.12,"pitch_rad":-0.04}`, seq))
		if err := nc.Publish("bbops.telemetry.test", frameBytes); err != nil {
			t.Fatalf("publish bbops.telemetry.test seq=%d: %v", seq, err)
		}
	}
	if err := nc.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush telemetry publish: %v", err)
	}

	for wantSeq := 1; wantSeq <= 2; wantSeq++ {
		frame := readWSFrame(t, telemetryConn, 5*time.Second)
		if frame.Type != "data" || frame.Subject != "bbops.telemetry.test" {
			t.Fatalf("WS #1 frame = %+v, want type=data subject=bbops.telemetry.test", frame)
		}
		var telemetry struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(frame.Payload, &telemetry); err != nil {
			t.Fatalf("decode WS #1 telemetry payload: %v", err)
		}
		if telemetry.Seq != wantSeq {
			t.Fatalf("WS #1 telemetry seq = %d, want %d (uncoalesced order)", telemetry.Seq, wantSeq)
		}
	}

	// 7. Verify /health reports 200 OK once all components and NATS are healthy, then stop cleanly.
	resp, err := http.Get(endpoints.HTTPBaseURL + "/health") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", resp.StatusCode)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	if err := rt.Stop(stopCtx); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
}

func assertHTTPGet(t *testing.T, url string, wantStatus int, wantBody string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", url, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s status = %d, want %d (body=%q)", url, resp.StatusCode, wantStatus, string(body))
	}
	if string(body) != wantBody {
		t.Fatalf("GET %s body = %q, want %q", url, string(body), wantBody)
	}
}

func dialWebSocketWithRetry(ctx context.Context, t *testing.T, wsURL string) *websocket.Conn {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			return conn
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("dial %s failed before deadline: %v", wsURL, lastErr)
	return nil
}

func readWSFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) wsEnvelope {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	var env wsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal WS frame %q: %v", string(raw), err)
	}
	return env
}

func queryVehicleGraphQL(ctx context.Context, t *testing.T, endpoint, entityID, prefix string) {
	t.Helper()
	postGQL := func(query string, vars map[string]any, dst any) (int, string, error) {
		reqBody, err := json.Marshal(map[string]any{"query": query, "variables": vars})
		if err != nil {
			t.Fatalf("marshal GraphQL request: %v", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("build GraphQL request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, string(raw), fmt.Errorf("status %d", resp.StatusCode)
		}
		return resp.StatusCode, string(raw), json.Unmarshal(raw, dst)
	}

	var lastStatus int
	var lastBody string
	defer func() {
		if t.Failed() {
			t.Logf("last GraphQL status=%d body=%s", lastStatus, lastBody)
		}
	}()

	// 5a. Exact entity lookup via POST /graph-gateway/graphql -> graph-query -> graph-ingest.
	waitForCondition(t, 10*time.Second, "GraphQL entity(id) returns vehicle", func() bool {
		var decoded struct {
			Data struct {
				Entity *struct {
					Entity struct {
						ID      string `json:"id"`
						Triples []struct {
							Predicate string `json:"predicate"`
							Object    any    `json:"object"`
						} `json:"triples"`
					} `json:"entity"`
				} `json:"entity"`
			} `json:"data"`
			Errors []struct{ Message string } `json:"errors"`
		}
		status, body, err := postGQL(
			`query($id: String!) { entity(id: $id) { kvRevision entity { id triples { predicate object } } } }`,
			map[string]any{"id": entityID},
			&decoded,
		)
		lastStatus, lastBody = status, body
		if err != nil || len(decoded.Errors) > 0 || decoded.Data.Entity == nil {
			return false
		}
		if decoded.Data.Entity.Entity.ID != entityID {
			return false
		}
		preds := map[string]any{}
		for _, tr := range decoded.Data.Entity.Entity.Triples {
			preds[tr.Predicate] = tr.Object
		}
		return preds["test.vehicle.domain"] == "Quadcopter" && preds["test.vehicle.callsign"] == "ALPHA-1"
	})

	// 5b. Prefix lookup via POST /graph-gateway/graphql.
	waitForCondition(t, 10*time.Second, "GraphQL entitiesByPrefix returns vehicle", func() bool {
		var decoded struct {
			Data struct {
				EntitiesByPrefix *struct {
					Total    int `json:"total"`
					Entities []struct {
						ID string `json:"id"`
					} `json:"entities"`
				} `json:"entitiesByPrefix"`
			} `json:"data"`
			Errors []struct{ Message string } `json:"errors"`
		}
		status, body, err := postGQL(
			`query($p: String!) { entitiesByPrefix(prefix: $p, limit: 10) { total entities { id } } }`,
			map[string]any{"p": prefix},
			&decoded,
		)
		lastStatus, lastBody = status, body
		return err == nil && len(decoded.Errors) == 0 &&
			decoded.Data.EntitiesByPrefix != nil && decoded.Data.EntitiesByPrefix.Total >= 1 &&
			decoded.Data.EntitiesByPrefix.Entities[0].ID == entityID
	})

	// 5c. Predicate lookup via POST /graph-gateway/graphql -> graph-query -> graph-index (after 500ms coalesce).
	waitForCondition(t, 10*time.Second, "GraphQL entitiesByPredicate returns vehicle from graph-index", func() bool {
		var decoded struct {
			Data struct {
				EntitiesByPredicate *struct {
					Entities []string `json:"entities"`
				} `json:"entitiesByPredicate"`
			} `json:"data"`
			Errors []struct{ Message string } `json:"errors"`
		}
		status, body, err := postGQL(
			`query($pred: String!, $val: String) { entitiesByPredicate(predicate: $pred, value: $val, limit: 10) { entities } }`,
			map[string]any{"pred": "test.vehicle.domain", "val": "Quadcopter"},
			&decoded,
		)
		lastStatus, lastBody = status, body
		return err == nil && len(decoded.Errors) == 0 &&
			decoded.Data.EntitiesByPredicate != nil && len(decoded.Data.EntitiesByPredicate.Entities) == 1 &&
			decoded.Data.EntitiesByPredicate.Entities[0] == entityID
	})
}

func waitForCondition(t *testing.T, timeout time.Duration, desc string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}
