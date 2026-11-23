package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/boxbotix/bbops/configs"
	"github.com/boxbotix/bbops/internal/host"
	"github.com/boxbotix/bbops/internal/uxsvocab"
	"github.com/c360studio/semengine/graph"
	"github.com/c360studio/semengine/message"
	"github.com/c360studio/semengine/payloadregistry"
	"github.com/c360studio/semengine/vocabulary"
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

	// 1. Verify /healthz liveness immediately, verify cold-start pre-ready gating on both /readyz and /,
	// then wait with a bounded deadline for ADR-109 readiness and verify static UI + /services/health diagnostic JSON.
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/healthz", http.StatusOK, "OK")
	if rt.IsReady() {
		t.Fatal("expected cold-start runtime to report IsReady=false before graph-websocket ENTITY_STATES catch-up")
	}
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/readyz", http.StatusServiceUnavailable, "NOT READY")
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/", http.StatusServiceUnavailable, "NOT READY")
	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	if err := rt.WaitReady(readyCtx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/readyz", http.StatusOK, "READY")
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/", http.StatusOK, wantIndex)

	svcHealthResp, err := http.Get(endpoints.HTTPBaseURL + "/services/health") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /services/health: %v", err)
	}
	svcHealthBody, err := io.ReadAll(svcHealthResp.Body)
	_ = svcHealthResp.Body.Close()
	if err != nil {
		t.Fatalf("read /services/health: %v", err)
	}
	if svcHealthResp.StatusCode != http.StatusOK || strings.Contains(string(svcHealthBody), wantIndex) {
		t.Fatalf("GET /services/health = %d %q, want 200 JSON (not SPA)", svcHealthResp.StatusCode, string(svcHealthBody))
	}
	var svcHealthJSON map[string]any
	if err := json.Unmarshal(svcHealthBody, &svcHealthJSON); err != nil {
		t.Fatalf("unmarshal /services/health JSON %q: %v", string(svcHealthBody), err)
	}

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

// spec: host-runtime / Host CLI version and flag entrypoint
// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
func TestRuntimeWarmRestartAppliesOverridesAndReleasesOldListeners(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	uiDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte("<html>warm-cop</html>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	portsA, err := host.AllocateLoopbackPorts(4)
	if err != nil {
		t.Fatalf("allocate boot-1 ports: %v", err)
	}

	// Boot 1: cold start on dataDir with explicit listener overrides portsA.
	rt1, err := host.Start(ctx, host.Options{
		DataDir: dataDir,
		UIDir:   uiDir,
		Version: "v0.2.0-boot1",
		Overrides: host.PortOverrides{
			BindHost:        "127.0.0.1",
			HTTPPort:        portsA[0],
			TelemetryWSPort: portsA[1],
			GraphWSPort:     portsA[2],
			MetricsPort:     portsA[3],
		},
	})
	if err != nil {
		t.Fatalf("Boot 1 Start: %v", err)
	}
	readyCtx1, readyCancel1 := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel1()
	if err := rt1.WaitReady(readyCtx1); err != nil {
		_ = rt1.Stop(context.Background())
		t.Fatalf("Boot 1 WaitReady: %v", err)
	}
	assertHTTPGet(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", portsA[0]), http.StatusOK, "READY")
	assertMetricsReachable(t, portsA[3])
	wsTel1 := dialWebSocketWithRetry(ctx, t, fmt.Sprintf("ws://127.0.0.1:%d/ws", portsA[1]))
	_ = wsTel1.Close()
	wsGraph1 := dialWebSocketWithRetry(ctx, t, fmt.Sprintf("ws://127.0.0.1:%d/ws", portsA[2]))
	_ = wsGraph1.Close()

	stopCtx1, stopCancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel1()
	if err := rt1.Stop(stopCtx1); err != nil {
		t.Fatalf("Boot 1 Stop: %v", err)
	}

	// Allocate distinct port set B for Boot 2 on the same persisted JetStream KV store.
	var portsB []int
	for range 10 {
		cand, allocErr := host.AllocateLoopbackPorts(4)
		if allocErr != nil {
			t.Fatalf("allocate boot-2 ports: %v", allocErr)
		}
		distinct := true
		for _, pb := range cand {
			for _, pa := range portsA {
				if pb == pa {
					distinct = false
				}
			}
		}
		if distinct {
			portsB = cand
			break
		}
	}
	if len(portsB) != 4 {
		t.Fatalf("failed to allocate 4 ports distinct from %v", portsA)
	}

	// Boot 2: warm restart against same dataDir (equal config version triggers syncFromKV).
	rt2, err := host.Start(ctx, host.Options{
		DataDir: dataDir,
		UIDir:   uiDir,
		Version: "v0.2.0-boot2",
		Overrides: host.PortOverrides{
			BindHost:        "127.0.0.1",
			HTTPPort:        portsB[0],
			TelemetryWSPort: portsB[1],
			GraphWSPort:     portsB[2],
			MetricsPort:     portsB[3],
		},
	})
	if err != nil {
		t.Fatalf("Boot 2 Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx2, stopCancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel2()
		if stopErr := rt2.Stop(stopCtx2); stopErr != nil {
			t.Errorf("Boot 2 Stop: %v", stopErr)
		}
	})

	readyCtx2, readyCancel2 := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel2()
	if err := rt2.WaitReady(readyCtx2); err != nil {
		t.Fatalf("Boot 2 WaitReady: %v", err)
	}

	ep2 := rt2.Endpoints()
	if ep2.HTTPPort != portsB[0] || ep2.MetricsPort != portsB[3] ||
		ep2.TelemetryWS != fmt.Sprintf("ws://127.0.0.1:%d/ws", portsB[1]) ||
		ep2.GraphWS != fmt.Sprintf("ws://127.0.0.1:%d/ws", portsB[2]) {
		t.Fatalf("Boot 2 advertised endpoints = %+v, want portsB %v", ep2, portsB)
	}

	// Verify every requested Boot 2 listener answers on portsB.
	assertHTTPGet(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", portsB[0]), http.StatusOK, "READY")
	assertHTTPGet(t, fmt.Sprintf("http://127.0.0.1:%d/", portsB[0]), http.StatusOK, "<html>warm-cop</html>")
	assertMetricsReachable(t, portsB[3])
	wsTel2 := dialWebSocketWithRetry(ctx, t, ep2.TelemetryWS)
	_ = wsTel2.Close()
	wsGraph2 := dialWebSocketWithRetry(ctx, t, ep2.GraphWS)
	_ = wsGraph2.Close()

	// Verify every Boot 1 listener on portsA is gone.
	for _, oldPort := range portsA {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort))
		conn, dialErr := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			t.Fatalf("stale Boot 1 listener still accepting connections on %s", addr)
		}
	}
}

func assertMetricsReachable(t *testing.T, port int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/metrics", port)
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
}

// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
// spec: host-runtime / ADR-115 GraphQL front door and dual output/websocket streaming planes
func TestRuntimeInvalidServiceConfigDoesNotPoisonDataDir(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	dataDir := t.TempDir()

	// Boot 1: disabled mandatory component-manager must fail before persisting to KV.
	var brokenDoc map[string]any
	if err := json.Unmarshal(configs.DefaultFlowJSON, &brokenDoc); err != nil {
		t.Fatalf("unmarshal default flow: %v", err)
	}
	brokenDoc["services"].(map[string]any)["component-manager"] = map[string]any{
		"enabled": false,
		"config":  map[string]any{},
	}
	brokenRaw, err := json.Marshal(brokenDoc)
	if err != nil {
		t.Fatalf("marshal broken flow: %v", err)
	}
	brokenPath := filepath.Join(t.TempDir(), "broken-component-manager.json")
	if err := os.WriteFile(brokenPath, brokenRaw, 0o600); err != nil {
		t.Fatalf("write broken flow: %v", err)
	}

	if rtBroken, err := host.StartEphemeral(ctx, host.Options{
		ConfigPath: brokenPath,
		DataDir:    dataDir,
	}, 2); err == nil {
		_ = rtBroken.Stop(context.Background())
		t.Fatal("expected Boot 1 with disabled component-manager to fail")
	} else if !strings.Contains(err.Error(), "mandatory service component-manager cannot be disabled") {
		t.Fatalf("Boot 1 error = %v, want mandatory service diagnostic", err)
	}

	// Boot 2: corrected config with SemEngine day-duration ENTITY max_age ("1d") on the SAME
	// dataDir must succeed cleanly without poisoned KV state and provision MaxAge=24h.
	var validDoc map[string]any
	if err := json.Unmarshal(configs.DefaultFlowJSON, &validDoc); err != nil {
		t.Fatalf("unmarshal default flow for Boot 2: %v", err)
	}
	validDoc["streams"].(map[string]any)["ENTITY"].(map[string]any)["max_age"] = "1d"
	validRaw, err := json.Marshal(validDoc)
	if err != nil {
		t.Fatalf("marshal 1d flow: %v", err)
	}
	validPath := filepath.Join(t.TempDir(), "one-day-entity.json")
	if err := os.WriteFile(validPath, validRaw, 0o600); err != nil {
		t.Fatalf("write 1d flow: %v", err)
	}

	rtValid, err := host.StartEphemeral(ctx, host.Options{
		ConfigPath: validPath,
		DataDir:    dataDir,
		UIDir:      t.TempDir(),
		Version:    "v0.2.0-recovered",
	}, 4)
	if err != nil {
		t.Fatalf("Boot 2 with valid 1d config on shared dataDir failed (KV poisoned or 1d rejected): %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if stopErr := rtValid.Stop(stopCtx); stopErr != nil {
			t.Errorf("Boot 2 Stop: %v", stopErr)
		}
	})

	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	if err := rtValid.WaitReady(readyCtx); err != nil {
		t.Fatalf("Boot 2 WaitReady: %v", err)
	}

	assertHTTPGet(t, rtValid.Endpoints().HTTPBaseURL+"/readyz", http.StatusOK, "READY")

	js, err := rtValid.NATSClient().JetStream()
	if err != nil {
		t.Fatalf("Boot 2 JetStream handle: %v", err)
	}
	stream, err := js.Stream(ctx, host.EntityStreamName)
	if err != nil {
		t.Fatalf("Boot 2 lookup ENTITY stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("Boot 2 ENTITY stream info: %v", err)
	}
	if info.Config.MaxAge != 24*time.Hour {
		t.Fatalf("Boot 2 ENTITY stream MaxAge = %v, want 24h from \"1d\"", info.Config.MaxAge)
	}

	gqlReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		rtValid.Endpoints().HTTPBaseURL+"/graph-gateway/graphql",
		bytes.NewBufferString(`{"query":"{ entitiesByPrefix(prefix: \"boxbotix.\", limit: 10) { total entities { id } } }"}`))
	if err != nil {
		t.Fatalf("build GraphQL request: %v", err)
	}
	gqlReq.Header.Set("Content-Type", "application/json")
	gqlResp, err := http.DefaultClient.Do(gqlReq)
	if err != nil {
		t.Fatalf("POST /graph-gateway/graphql: %v", err)
	}
	_ = gqlResp.Body.Close()
	if gqlResp.StatusCode != http.StatusOK {
		t.Fatalf("Boot 2 GraphQL status = %d, want 200", gqlResp.StatusCode)
	}
}

// spec: mavlink-ingest / Constant-predicate Graphable vehicle snapshot payload
// spec: mavlink-ingest / UXS consumer vocabulary pack and ADR-102 vehicle entity identity
func TestHostIngestsUXSVehicleSnapshotWithSignalIndexingProfile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Boot with zero custom RegisterPayloads to prove D3 Option A registers
	// uxsvocab + uxs.vehicle.v1 automatically on host.Start.
	rt, err := host.StartEphemeral(ctx, host.Options{
		DataDir: t.TempDir(),
		UIDir:   t.TempDir(),
		Version: "v0.1.0-m11-test",
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

	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	if err := rt.WaitReady(readyCtx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	platform := rt.Platform()
	snap1, err := uxsvocab.NewVehicleSnapshot(platform.Org, platform.Platform, 7, uxsvocab.DomainSurfaceBoat)
	if err != nil {
		t.Fatalf("NewVehicleSnapshot: %v", err)
	}
	snap1.Flags = uxsvocab.HealthFlags{ClockSyncLocked: true, CompanionTrunkHealthy: true}
	snap1.Quaternion = [4]float64{0.70710678, 0, 0, 0.70710678}
	snap1.ObservedAt = time.Date(2026, 11, 23, 21, 10, 0, 0, time.UTC)

	entityID := snap1.EntityID()
	msg1, err := json.Marshal(message.NewBaseMessage(snap1.Schema(), snap1, "m11-host-test"))
	if err != nil {
		t.Fatalf("Marshal BaseMessage 1: %v", err)
	}
	if err := rt.NATSClient().PublishToStream(ctx, "entity.uxs.upsert", msg1); err != nil {
		t.Fatalf("PublishToStream 1: %v", err)
	}

	predBucket, err := rt.NATSClient().GetKeyValueBucket(ctx, graph.BucketPredicateIndex)
	if err != nil {
		t.Fatalf("lookup PREDICATE_INDEX: %v", err)
	}
	expectedIndexPreds := append(uxsvocab.SnapshotPredicates(), vocabulary.EntityIndexingProfile)
	initialRevs := make(map[string]uint64, len(expectedIndexPreds))
	waitForCondition(t, 10*time.Second, "PREDICATE_INDEX populated for all 23 vehicle predicates", func() bool {
		for _, pred := range expectedIndexPreds {
			entry, getErr := predBucket.Get(ctx, pred+"."+entityID)
			if getErr != nil || entry.Revision() == 0 {
				return false
			}
			initialRevs[pred] = entry.Revision()
		}
		return true
	})

	// Publish a second snapshot with updated values (armed + altitude + authority) and
	// verify ENTITY_STATES advances to kvRevision >= 2 while PREDICATE_INDEX per-key
	// revisions remain unchanged (zero predicate-index rewrite amplification).
	snap2 := *snap1
	snap2.Armed = true
	snap2.Authority = uxsvocab.AuthorityCompanionTeleop
	snap2.CompanionMode = uxsvocab.ModeTeleopVelocity
	snap2.Altitude = 88.5
	snap2.ObservedAt = snap1.ObservedAt.Add(time.Second)

	msg2, err := json.Marshal(message.NewBaseMessage(snap2.Schema(), &snap2, "m11-host-test"))
	if err != nil {
		t.Fatalf("Marshal BaseMessage 2: %v", err)
	}
	if err := rt.NATSClient().PublishToStream(ctx, "entity.uxs.upsert", msg2); err != nil {
		t.Fatalf("PublishToStream 2: %v", err)
	}

	gqlURL := rt.Endpoints().HTTPBaseURL + "/graph-gateway/graphql"
	waitForCondition(t, 10*time.Second, "GraphQL entity reflects updated VehicleSnapshot + signal profile", func() bool {
		reqBody, _ := json.Marshal(map[string]any{
			"query":     `query($id: String!) { entity(id: $id) { kvRevision entity { id triples { predicate object } } } }`,
			"variables": map[string]any{"id": entityID},
		})
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, gqlURL, bytes.NewReader(reqBody))
		if reqErr != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			return false
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}

		var decoded struct {
			Data struct {
				Entity *struct {
					KVRevision uint64 `json:"kvRevision"`
					Entity     struct {
						ID      string `json:"id"`
						Triples []struct {
							Predicate string `json:"predicate"`
							Object    any    `json:"object"`
						} `json:"triples"`
					} `json:"entity"`
				} `json:"entity"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &decoded) != nil || decoded.Data.Entity == nil || decoded.Data.Entity.KVRevision < 2 {
			return false
		}

		byPred := make(map[string]any, len(decoded.Data.Entity.Entity.Triples))
		for _, tr := range decoded.Data.Entity.Entity.Triples {
			byPred[tr.Predicate] = tr.Object
		}
		if len(byPred) != 23 {
			return false
		}
		if byPred[vocabulary.EntityIndexingProfile] != vocabulary.IndexingProfileSignal {
			return false
		}
		if byPred[uxsvocab.PredicateDomainKind] != uxsvocab.DomainSurfaceBoat {
			return false
		}
		if byPred[uxsvocab.PredicateAuthorityState] != uxsvocab.AuthorityCompanionTeleop {
			return false
		}
		if byPred[uxsvocab.PredicateHealthArmed] != true {
			return false
		}
		// D2 Option A: flags and quaternion arrive as native JSON map/slice in GraphQL.
		flagsMap, ok := byPred[uxsvocab.PredicateHealthFlags].(map[string]any)
		if !ok || flagsMap["clock_sync_locked"] != true {
			return false
		}
		quatSlice, ok := byPred[uxsvocab.PredicateAttitudeQuaternion].([]any)
		return ok && len(quatSlice) == 4
	})

	// Deterministically wait for graph-index to finish re-indexing snap2 via /metrics
	// (no time.Sleep) and assert unchanged index-projection + zero extra predicate puts.
	metricsURL := fmt.Sprintf("http://127.0.0.1:%d/metrics", rt.Endpoints().MetricsPort)
	var metricsBody string
	waitForCondition(t, 10*time.Second, "graph-index processed second VehicleSnapshot with unchanged projection", func() bool {
		resp, getErr := http.Get(metricsURL)
		if getErr != nil {
			return false
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		metricsBody = string(raw)
		return strings.Contains(metricsBody, "semengine_graph_index_events_processed_total 2") &&
			strings.Contains(metricsBody, `semengine_graph_index_reindex_events_total{result="unchanged"} 1`)
	})
	if !strings.Contains(metricsBody, `semengine_graph_index_reconcile_operations_total{index_type="predicate",operation="put",result="success"} 23`) {
		t.Fatalf("expected exactly 23 predicate-index put operations across 2 snapshots; metrics:\n%s", metricsBody)
	}
	for _, pred := range expectedIndexPreds {
		entry, getErr := predBucket.Get(ctx, pred+"."+entityID)
		if getErr != nil {
			t.Fatalf("PREDICATE_INDEX missing %s.%s: %v", pred, entityID, getErr)
		}
		if entry.Revision() != initialRevs[pred] {
			t.Fatalf("PREDICATE_INDEX %s.%s revision = %d, want %d (unexpected rewrite)",
				pred, entityID, entry.Revision(), initialRevs[pred])
		}
	}
}
