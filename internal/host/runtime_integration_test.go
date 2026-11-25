package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/boxbotix/bbops/configs"
	"github.com/boxbotix/bbops/internal/host"
	"github.com/boxbotix/bbops/internal/mavcodec"
	"github.com/boxbotix/bbops/internal/mavlink"
	"github.com/boxbotix/bbops/internal/uxsvocab"
	"github.com/c360studio/semengine/config"
	"github.com/c360studio/semengine/graph"
	"github.com/c360studio/semengine/message"
	"github.com/c360studio/semengine/natsclient"
	"github.com/c360studio/semengine/payloadregistry"
	"github.com/c360studio/semengine/vocabulary"
	"github.com/gorilla/websocket"
	natsserver "github.com/nats-io/nats-server/v2/server"
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
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/ws/telemetry", http.StatusServiceUnavailable, "NOT READY")
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/ws/graph", http.StatusServiceUnavailable, "NOT READY")
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

	// 4. Dial output/websocket #2 (ENTITY_STATES kv-watch) both directly and via same-origin /ws/graph.
	frontDoorWSBase := strings.Replace(endpoints.HTTPBaseURL, "http://", "ws://", 1)
	for _, graphWSURL := range []string{endpoints.GraphWS, frontDoorWSBase + "/ws/graph"} {
		graphConn := dialWebSocketWithRetry(ctx, t, graphWSURL)
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
						_ = graphConn.Close()
						t.Fatalf("decode WS #2 (%s) put payload: %v", graphWSURL, err)
					}
					if stored.ID != entityID {
						_ = graphConn.Close()
						t.Fatalf("WS #2 (%s) put payload id = %q, want %q", graphWSURL, stored.ID, entityID)
					}
					sawPut = true
				}
			case "synced":
				if frame.Bucket != graph.BucketEntityStates {
					_ = graphConn.Close()
					t.Fatalf("WS #2 (%s) synced bucket = %q, want %s", graphWSURL, frame.Bucket, graph.BucketEntityStates)
				}
				sawSynced = true
			}
		}
		_ = graphConn.Close()
		if !sawPut {
			t.Fatalf("WS #2 (%s) reached synced without delivering snapshot put for %s", graphWSURL, entityID)
		}
	}

	// 5. Query POST /graph-gateway/graphql for exact entity and prefix match (once graph-index coalesces).
	queryVehicleGraphQL(ctx, t, endpoints.HTTPBaseURL+"/graph-gateway/graphql", entityID, entityPrefix)

	// 5b. Verify standalone front-door WebSocket routes reject foreign browser Origin headers
	// with 403 Forbidden while accepting explicit same-origin Origin headers on both routes.
	for _, wsRoute := range []string{"/ws/telemetry", "/ws/graph"} {
		badHdr := http.Header{"Origin": []string{"http://evil.example:3000"}}
		badConn, badResp, badErr := websocket.DefaultDialer.DialContext(ctx, frontDoorWSBase+wsRoute, badHdr)
		if badConn != nil {
			_ = badConn.Close()
		}
		if badResp != nil && badResp.Body != nil {
			_ = badResp.Body.Close()
		}
		if badErr == nil || badResp == nil || badResp.StatusCode != http.StatusForbidden {
			t.Fatalf("foreign Origin dial on %s = (err=%v, resp=%v), want HTTP 403", wsRoute, badErr, badResp)
		}

		sameOriginHdr := http.Header{"Origin": []string{endpoints.HTTPBaseURL}}
		okConn, okResp, okErr := websocket.DefaultDialer.DialContext(ctx, frontDoorWSBase+wsRoute, sameOriginHdr)
		if okResp != nil && okResp.Body != nil {
			_ = okResp.Body.Close()
		}
		if okErr != nil {
			t.Fatalf("same-origin dial on %s failed: %v", wsRoute, okErr)
		}
		_ = okConn.Close()
	}

	// 6. Dial output/websocket #1 (bbops.telemetry.>) both directly and via same-origin /ws/telemetry (native nil header).
	telemetryConn := dialWebSocketWithRetry(ctx, t, endpoints.TelemetryWS)
	defer func() { _ = telemetryConn.Close() }()
	frontDoorTelemetryConn := dialWebSocketWithRetry(ctx, t, frontDoorWSBase+"/ws/telemetry")
	defer func() { _ = frontDoorTelemetryConn.Close() }()

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

	for _, conn := range []*websocket.Conn{telemetryConn, frontDoorTelemetryConn} {
		for wantSeq := 1; wantSeq <= 2; wantSeq++ {
			frame := readWSFrame(t, conn, 5*time.Second)
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

	// Keep frontDoorTelemetryConn open across rt.Stop to verify ShutdownCtx aborts
	// active hijacked /ws/* ReverseProxy connections without blocking HTTP shutdown.
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
	udpA, err := host.AllocateLoopbackUDPPorts(1)
	if err != nil {
		t.Fatalf("allocate boot-1 UDP port: %v", err)
	}

	// Boot 1: cold start on dataDir with explicit listener overrides portsA + udpA.
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
			MAVLinkUDPPort:  udpA[0],
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

	// Allocate distinct port set B + udpB for Boot 2 on the same persisted JetStream KV store.
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
	var udpB int
	for range 10 {
		candUDP, allocErr := host.AllocateLoopbackUDPPorts(1)
		if allocErr != nil {
			t.Fatalf("allocate boot-2 UDP port: %v", allocErr)
		}
		if candUDP[0] != udpA[0] {
			udpB = candUDP[0]
			break
		}
	}
	if udpB == 0 {
		t.Fatalf("failed to allocate UDP port distinct from %d", udpA[0])
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
			MAVLinkUDPPort:  udpB,
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
		ep2.MAVLinkUDPPort != udpB || ep2.MAVLinkUDPAddr != fmt.Sprintf("127.0.0.1:%d", udpB) ||
		ep2.TelemetryWS != fmt.Sprintf("ws://127.0.0.1:%d/ws", portsB[1]) ||
		ep2.GraphWS != fmt.Sprintf("ws://127.0.0.1:%d/ws", portsB[2]) {
		t.Fatalf("Boot 2 advertised endpoints = %+v, want portsB %v udpB %d", ep2, portsB, udpB)
	}

	// Verify every requested Boot 2 listener answers on portsB.
	assertHTTPGet(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", portsB[0]), http.StatusOK, "READY")
	assertHTTPGet(t, fmt.Sprintf("http://127.0.0.1:%d/", portsB[0]), http.StatusOK, "<html>warm-cop</html>")
	assertMetricsReachable(t, portsB[3])
	wsTel2 := dialWebSocketWithRetry(ctx, t, ep2.TelemetryWS)
	_ = wsTel2.Close()
	wsGraph2 := dialWebSocketWithRetry(ctx, t, ep2.GraphWS)
	_ = wsGraph2.Close()

	// Verify every Boot 1 TCP listener on portsA and UDP listener on udpA[0] is released.
	for _, oldPort := range portsA {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort))
		conn, dialErr := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			t.Fatalf("stale Boot 1 listener still accepting connections on %s", addr)
		}
	}
	reboundUDP, reboundErr := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: udpA[0]})
	if reboundErr != nil {
		t.Fatalf("stale Boot 1 UDP socket still holding 127.0.0.1:%d: %v", udpA[0], reboundErr)
	}
	_ = reboundUDP.Close()
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

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
// spec: host-runtime / ADR-115 GraphQL front door and dual output/websocket streaming planes
func TestRuntimeMAVLinkThreeLaneUDPIngest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	rt, err := host.StartEphemeral(ctx, host.Options{
		DataDir: t.TempDir(),
		UIDir:   t.TempDir(),
		Version: "v0.1.0-m12-test",
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

	endpoints := rt.Endpoints()
	ingestComp := rt.MAVLinkIngest()
	if ingestComp == nil || !ingestComp.BoundAddr().IsValid() {
		t.Fatalf("MAVLinkIngest = %v, expected active bound UDP component", ingestComp)
	}
	serverUDPAddr, err := net.ResolveUDPAddr("udp", endpoints.MAVLinkUDPAddr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr(%q): %v", endpoints.MAVLinkUDPAddr, err)
	}

	// Connect WS #1 (bbops.telemetry.>) and WS #2 (ENTITY_STATES kv-watch) before sending UDP datagrams.
	telWS := dialWebSocketWithRetry(ctx, t, endpoints.TelemetryWS)
	defer func() { _ = telWS.Close() }()

	graphWS := dialWebSocketWithRetry(ctx, t, endpoints.GraphWS)
	defer func() { _ = graphWS.Close() }()
	// Drain initial empty-snapshot `synced` barrier on WS #2.
	for {
		f := readWSFrame(t, graphWS, 10*time.Second)
		if f.Type == "synced" {
			break
		}
	}

	openUDPClient := func() *net.UDPConn {
		c, listenErr := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		if listenErr != nil {
			t.Fatalf("ListenUDP client: %v", listenErr)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	sendFrame := func(conn *net.UDPConn, sysid, compid, seq uint8, msg mavcodec.Message) {
		t.Helper()
		raw, encErr := mavcodec.EncodeFrame(mavcodec.Frame{
			Sequence:    seq,
			SystemID:    sysid,
			ComponentID: compid,
			Message:     msg,
		})
		if encErr != nil {
			t.Fatalf("EncodeFrame: %v", encErr)
		}
		if _, wErr := conn.WriteToUDP(raw, serverUDPAddr); wErr != nil {
			t.Fatalf("WriteToUDP: %v", wErr)
		}
	}

	noiseClient := openUDPClient()
	// 1. Send benign GCS HEARTBEAT (sysid=255), pre-admission kinematics (sysid=9), and corrupted datagram;
	// verify /readyz stays 200 READY and no ghost peer is admitted.
	sendFrame(noiseClient, 255, 190, 0, mavcodec.Heartbeat{Type: 6, Autopilot: 8})
	sendFrame(noiseClient, 9, 1, 1, mavcodec.LocalPositionNED{X: 42})
	if _, err := noiseClient.WriteToUDP([]byte{0xFD, 0x09, 0x00, 0x00, 0x01, 0x01, 0x01, 0xFF}, serverUDPAddr); err != nil {
		t.Fatalf("WriteToUDP corrupt: %v", err)
	}
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/readyz", http.StatusOK, "READY")
	if peers := ingestComp.Peers(); len(peers) != 0 {
		t.Fatalf("unexpected ghost peers admitted from noise traffic: %+v", peers)
	}

	// 2. Open 3 vehicle UDP sockets: sysid=1 (Quadcopter), sysid=2 (Rover), sysid=3 (SurfaceBoat).
	s45 := float32(math.Sqrt2 / 2)
	vehicles := []struct {
		sysid   uint8
		domain  mavcodec.VehicleDomain
		domName string
		quat    [4]float32
		wantYaw float64
		latE7   int32
		lonE7   int32
		conn    *net.UDPConn
	}{
		{
			sysid: 1, domain: mavcodec.DomainQuadcopter, domName: uxsvocab.DomainQuadcopter,
			quat: [4]float32{s45, 0, 0, s45}, wantYaw: 90.0,
			latE7: 377749000, lonE7: -1224194000, conn: openUDPClient(),
		},
		{
			sysid: 2, domain: mavcodec.DomainRover, domName: uxsvocab.DomainRover,
			quat: [4]float32{0, 0, 0, 1}, wantYaw: 180.0,
			latE7: 377750000, lonE7: -1224195000, conn: openUDPClient(),
		},
		{
			sysid: 3, domain: mavcodec.DomainSurfaceBoat, domName: uxsvocab.DomainSurfaceBoat,
			quat: [4]float32{s45, 0, 0, -s45}, wantYaw: 270.0,
			latE7: 377751000, lonE7: -1224196000, conn: openUDPClient(),
		},
	}

	for _, v := range vehicles {
		hb, hbErr := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
			Armed: true,
			Custom: mavcodec.CustomMode{
				Domain:           v.domain,
				ControlAuthority: mavcodec.AuthorityCompanionTeleop,
				CompanionMode:    mavcodec.ModeTeleopVelocity,
				LinkTier:         mavcodec.LinkTierSITLLocal,
				HealthFlags:      uxsvocab.HealthFlags{ClockSyncLocked: true, CompanionTrunkHealthy: true},
			},
		})
		if hbErr != nil {
			t.Fatalf("EncodeVehicleHeartbeat(%d): %v", v.sysid, hbErr)
		}
		sendFrame(v.conn, v.sysid, 1, 1, hb)
	}

	waitForCondition(t, 5*time.Second, "all 3 vehicle UDP peer endpoints recorded", func() bool {
		for _, v := range vehicles {
			gotPeer, ok := ingestComp.PeerAddr(v.sysid)
			wantPeer := v.conn.LocalAddr().(*net.UDPAddr).AddrPort()
			if !ok || gotPeer != wantPeer {
				return false
			}
		}
		return true
	})

	// Stream #1 SYS_STATUS (with nonzero historical failsafe count on join to verify no false baseline alert),
	// #31 ATTITUDE_QUATERNION, #32 LOCAL_POSITION_NED, and #33 GLOBAL_POSITION_INT
	// (with Hdg=4500 which must NOT override #31's quaternion yaw!).
	for _, v := range vehicles {
		sendFrame(v.conn, v.sysid, 1, 2, mavcodec.SysStatus{
			Load: 950, ErrorsComm: 1, ErrorsCount1: uint16(v.sysid), ErrorsCount2: 1400, ErrorsCount3: 600,
		})
		sendFrame(v.conn, v.sysid, 1, 3, mavcodec.AttitudeQuaternion{
			Q1: v.quat[0], Q2: v.quat[1], Q3: v.quat[2], Q4: v.quat[3], Yawspeed: 0.15,
		})
		sendFrame(v.conn, v.sysid, 1, 4, mavcodec.LocalPositionNED{
			X: float32(v.sysid) * 10, Y: -5, Z: -20, Vx: 2.5, Vy: -0.5, Vz: 0,
		})
		sendFrame(v.conn, v.sysid, 1, 5, mavcodec.GlobalPositionInt{
			Lat: v.latE7, Lon: v.lonE7, Alt: 15000, Hdg: 4500,
		})
	}

	// Inject sanitized-out bad packets (NaN #31 and out-of-bounds #33) from noiseClient for sysid=1;
	// verify /readyz stays 200 READY and sysid=1's PeerAddr is preserved.
	sendFrame(noiseClient, 1, 1, 90, mavcodec.AttitudeQuaternion{Q1: float32(math.NaN())})
	sendFrame(noiseClient, 1, 1, 91, mavcodec.GlobalPositionInt{Lat: 900_000_001, Lon: 0, Hdg: 0})
	assertHTTPGet(t, endpoints.HTTPBaseURL+"/readyz", http.StatusOK, "READY")

	// Verify Lane 1: WS #1 receives all 9 uncoalesced TelemetryFrames (#31 seq=3, #32 seq=4, #33 seq=5)
	// on exact concrete subjects bbops.telemetry.{1,2,3} without any spurious baseline TransitionEvent.
	type receivedTelemetry struct {
		subject string
		frame   mavlink.TelemetryFrame
	}
	gotTelemetry := make(map[uint8][]receivedTelemetry, 3)
	seenFinalSeq := make(map[uint8]bool, len(vehicles))
	totalFrames := 0
	for totalFrames < 9 && len(seenFinalSeq) < len(vehicles) {
		wsFrame := readWSFrame(t, telWS, 5*time.Second)
		if wsFrame.Type != "data" {
			continue
		}
		if strings.HasPrefix(wsFrame.Subject, "bbops.telemetry.events.") {
			t.Fatalf("unexpected pre-transition Lane 3 event on %s: %s", wsFrame.Subject, string(wsFrame.Payload))
		}
		var tf mavlink.TelemetryFrame
		if err := json.Unmarshal(wsFrame.Payload, &tf); err != nil {
			t.Fatalf("unmarshal WS #1 TelemetryFrame (%s): %v", wsFrame.Subject, err)
		}
		gotTelemetry[tf.SysID] = append(gotTelemetry[tf.SysID], receivedTelemetry{
			subject: wsFrame.Subject,
			frame:   tf,
		})
		if tf.Seq == 5 {
			seenFinalSeq[tf.SysID] = true
		}
		totalFrames++
	}
	type seqMsgPair struct {
		seq   uint8
		msgID uint32
	}
	wantSeqMsgs := []seqMsgPair{
		{seq: 3, msgID: mavcodec.MsgIDAttitudeQuaternion},
		{seq: 4, msgID: mavcodec.MsgIDLocalPositionNED},
		{seq: 5, msgID: mavcodec.MsgIDGlobalPositionInt},
	}
	for _, v := range vehicles {
		frames := gotTelemetry[v.sysid]
		if len(frames) != len(wantSeqMsgs) {
			t.Fatalf("sysid=%d WS #1 frame count = %d (%+v), want %d (#31, #32, #33)",
				v.sysid, len(frames), frames, len(wantSeqMsgs))
		}
		wantSubject := fmt.Sprintf("bbops.telemetry.%d", v.sysid)
		gotPairs := make([]seqMsgPair, 0, len(frames))
		for _, rec := range frames {
			if rec.subject != wantSubject {
				t.Fatalf("sysid=%d WS #1 subject = %q, want %q", v.sysid, rec.subject, wantSubject)
			}
			gotPairs = append(gotPairs, seqMsgPair{seq: rec.frame.Seq, msgID: rec.frame.MsgID})
		}
		if !slices.Equal(gotPairs, wantSeqMsgs) {
			t.Fatalf("sysid=%d WS #1 (seq, msg_id) sequence = %+v, want %+v", v.sysid, gotPairs, wantSeqMsgs)
		}
		tf := frames[len(frames)-1].frame
		if tf.Domain != v.domName || !tf.HasGlobalPos || tf.NEDXYZ[0] != float64(v.sysid)*10 || math.Abs(tf.HdgDeg-v.wantYaw) > 1e-2 {
			t.Fatalf("sysid=%d fused WS #1 TelemetryFrame = %+v, want domain=%s nedX=%v yaw=%v HasGlobalPos=true",
				v.sysid, tf, v.domName, float64(v.sysid)*10, v.wantYaw)
		}
		wantPeer := v.conn.LocalAddr().(*net.UDPAddr).AddrPort()
		if gotPeer, ok := ingestComp.PeerAddr(v.sysid); !ok || gotPeer != wantPeer {
			t.Fatalf("sysid=%d PeerAddr = (%v, %v), want (%v, true)", v.sysid, gotPeer, ok, wantPeer)
		}
	}

	// Verify Lane 2: 1 Hz periodic coalesced batch materializes all 3 vehicle entities in ENTITY_STATES + WS #2.
	platform := rt.Platform()
	wantEntities := make(map[string]bool, 3)
	for _, v := range vehicles {
		eid, idErr := uxsvocab.VehicleEntityID(platform.Org, platform.Platform, v.sysid)
		if idErr != nil {
			t.Fatalf("VehicleEntityID(%d): %v", v.sysid, idErr)
		}
		wantEntities[eid] = false
	}
	seenCount := 0
	for seenCount < 3 {
		wsFrame := readWSFrame(t, graphWS, 5*time.Second)
		if wsFrame.Type == "put" && wsFrame.Bucket == graph.BucketEntityStates {
			if seen, tracked := wantEntities[wsFrame.Key]; tracked && !seen {
				wantEntities[wsFrame.Key] = true
				seenCount++
			}
		}
	}

	// 3. Verify Lane 3 instant edge dual-route (< 1 Hz periodic cadence):
	//    - sysid=1: HEARTBEAT authority preemption CompanionTeleop -> DirectCRSFOverride + TeleopDeadmanClamped
	//    - sysid=2: #1 SYS_STATUS failsafe increment 2 -> 3
	//    - sysid=3: #253 STATUSTEXT "USV HOLD ENGAGED"
	edgeStart := time.Now()
	crsfHB, err := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
		Armed: true,
		Custom: mavcodec.CustomMode{
			Domain:           mavcodec.DomainQuadcopter,
			ControlAuthority: mavcodec.AuthorityDirectCRSFOverride,
			CompanionMode:    mavcodec.ModeHoldPosition,
			LinkTier:         mavcodec.LinkTierMeshHighRate,
			HealthFlags:      uxsvocab.HealthFlags{ClockSyncLocked: true, TeleopDeadmanClamped: true, CRSFLinkHealthy: true},
		},
	})
	if err != nil {
		t.Fatalf("EncodeVehicleHeartbeat CRSF override: %v", err)
	}
	sendFrame(vehicles[0].conn, 1, 1, 50, crsfHB)
	sendFrame(vehicles[1].conn, 2, 1, 50, mavcodec.SysStatus{Load: 1100, ErrorsCount1: 3})
	sendFrame(vehicles[2].conn, 3, 1, 50, mavcodec.StatusText{Severity: 3, Text: "USV HOLD ENGAGED"})

	gotEvents := make(map[uint8]mavlink.TransitionEvent, 3)
	for len(gotEvents) < 3 {
		wsFrame := readWSFrame(t, telWS, 5*time.Second)
		if wsFrame.Type != "data" || !strings.HasPrefix(wsFrame.Subject, "bbops.telemetry.events.") {
			continue
		}
		var ev mavlink.TransitionEvent
		if err := json.Unmarshal(wsFrame.Payload, &ev); err != nil {
			t.Fatalf("unmarshal WS #1 TransitionEvent (%s): %v", wsFrame.Subject, err)
		}
		gotEvents[ev.SysID] = ev
	}
	if ev1 := gotEvents[1]; ev1.Kind != mavlink.EventKindHeartbeatEdge ||
		ev1.Authority != uxsvocab.AuthorityDirectCRSFOverride ||
		ev1.PrevAuthority != uxsvocab.AuthorityCompanionTeleop ||
		!ev1.Flags.TeleopDeadmanClamped {
		t.Fatalf("sysid=1 Lane 3 TransitionEvent = %+v", ev1)
	}
	if ev2 := gotEvents[2]; ev2.Kind != mavlink.EventKindFailsafeIncrement || ev2.Failsafes != 3 {
		t.Fatalf("sysid=2 Lane 3 TransitionEvent = %+v", ev2)
	}
	if ev3 := gotEvents[3]; ev3.Kind != mavlink.EventKindStatusText || ev3.StatusText == nil || ev3.StatusText.Text != "USV HOLD ENGAGED" {
		t.Fatalf("sysid=3 Lane 3 TransitionEvent = %+v", ev3)
	}

	// Verify out-of-band SKG upsert converges in GraphQL / ENTITY_STATES without waiting for the 1000ms periodic tick!
	vehicle1ID, _ := uxsvocab.VehicleEntityID(platform.Org, platform.Platform, 1)
	gqlURL := endpoints.HTTPBaseURL + "/graph-gateway/graphql"
	waitForCondition(t, 5*time.Second, "GraphQL reflects urgent DirectCRSFOverride edge on vehicle.1", func() bool {
		reqBody, _ := json.Marshal(map[string]any{
			"query":     `query($id: String!) { entity(id: $id) { entity { triples { predicate object } } } }`,
			"variables": map[string]any{"id": vehicle1ID},
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
					Entity struct {
						Triples []struct {
							Predicate string `json:"predicate"`
							Object    any    `json:"object"`
						} `json:"triples"`
					} `json:"entity"`
				} `json:"entity"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &decoded) != nil || decoded.Data.Entity == nil {
			return false
		}
		byPred := make(map[string]any, len(decoded.Data.Entity.Entity.Triples))
		for _, tr := range decoded.Data.Entity.Entity.Triples {
			byPred[tr.Predicate] = tr.Object
		}
		return len(byPred) == 23 &&
			byPred[vocabulary.EntityIndexingProfile] == vocabulary.IndexingProfileSignal &&
			byPred[uxsvocab.PredicateAuthorityState] == uxsvocab.AuthorityDirectCRSFOverride
	})
	if edgeElapsed := time.Since(edgeStart); edgeElapsed >= 850*time.Millisecond {
		t.Fatalf("urgent Lane 3 edge + SKG GraphQL convergence took %v, expected out-of-band flush well below 1s periodic tick", edgeElapsed)
	}
}

// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestRuntimeUpgradeFromM02PersistedFlowPreservesOperatorSettings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	seedLegacyKV := func(t *testing.T, dataDir string, mutate func(*config.Config)) {
		t.Helper()
		legacyCfg, err := config.NewLoader().LoadFromBytes(configs.DefaultFlowJSON)
		if err != nil {
			t.Fatalf("LoadFromBytes: %v", err)
		}
		delete(legacyCfg.Components, host.MAVLinkIngestComponent)
		if len(legacyCfg.Components) != 7 || legacyCfg.Version != "1.0.0" {
			t.Fatalf("legacy flow = (%d components, version %q), want (7, \"1.0.0\")", len(legacyCfg.Components), legacyCfg.Version)
		}
		if mutate != nil {
			mutate(legacyCfg)
		}

		srv, err := natsserver.NewServer(&natsserver.Options{
			Host:      "127.0.0.1",
			Port:      -1,
			NoLog:     true,
			NoSigs:    true,
			JetStream: true,
			StoreDir:  filepath.Join(dataDir, "jetstream"),
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		srv.Start()
		if !srv.ReadyForConnections(10 * time.Second) {
			srv.Shutdown()
			srv.WaitForShutdown()
			t.Fatal("legacy embedded NATS did not become ready")
		}
		defer func() {
			srv.Shutdown()
			srv.WaitForShutdown()
		}()

		discardLogger := slog.New(slog.DiscardHandler)
		legacyCfg.NATS.URLs = []string{srv.ClientURL()}
		client, err := natsclient.NewClient(srv.ClientURL(), natsclient.WithLogger(discardLogger))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if err := client.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer func() { _ = client.Close(context.Background()) }()
		if err := client.WaitForConnection(ctx); err != nil {
			t.Fatalf("WaitForConnection: %v", err)
		}

		if err := config.NewStreamsManager(client, discardLogger).EnsureStreams(ctx, legacyCfg); err != nil {
			t.Fatalf("EnsureStreams: %v", err)
		}
		cfgMgr, err := config.NewConfigManager(legacyCfg, client, discardLogger)
		if err != nil {
			t.Fatalf("NewConfigManager: %v", err)
		}
		if err := cfgMgr.Start(ctx); err != nil {
			t.Fatalf("cfgMgr.Start: %v", err)
		}
		if _, hasMAV := cfgMgr.GetConfig().Get().Components[host.MAVLinkIngestComponent]; hasMAV {
			t.Fatal("legacy KV unexpectedly contained mavlink-ingest")
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := cfgMgr.Stop(stopCtx); err != nil {
			t.Fatalf("cfgMgr.Stop: %v", err)
		}
	}

	// 1. Seed a pre-M1.2 (7-component, version 1.0.0) JetStream directory with operator customizations:
	//    - metrics.config.path = "/operator-custom-metrics" (default "/metrics")
	//    - telemetry-websocket output path = "/operator-tel-ws" (default "/ws")
	//    - graph-index.config.coalesce_ms = 225 (default 500)
	dataDir := t.TempDir()
	uiDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte("<html>upgraded-cop</html>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	seedLegacyKV(t, dataDir, func(c *config.Config) {
		metricsSvc := c.Services["metrics"]
		metricsSvc.Config = json.RawMessage(`{"port":9090,"path":"/operator-custom-metrics"}`)
		c.Services["metrics"] = metricsSvc

		giComp := c.Components["graph-index"]
		var giDoc map[string]any
		if err := json.Unmarshal(giComp.Config, &giDoc); err != nil {
			t.Fatalf("decode graph-index config: %v", err)
		}
		giDoc["coalesce_ms"] = 225
		rawGI, err := json.Marshal(giDoc)
		if err != nil {
			t.Fatalf("encode graph-index config: %v", err)
		}
		giComp.Config = rawGI
		c.Components["graph-index"] = giComp

		telComp := c.Components[host.TelemetryWSComponent]
		var telDoc map[string]any
		if err := json.Unmarshal(telComp.Config, &telDoc); err != nil {
			t.Fatalf("decode telemetry-websocket config: %v", err)
		}
		telDoc["path"] = "/operator-tel-ws"
		rawTel, err := json.Marshal(telDoc)
		if err != nil {
			t.Fatalf("encode telemetry-websocket config: %v", err)
		}
		telComp.Config = rawTel
		c.Components[host.TelemetryWSComponent] = telComp
	})

	// 2. Boot the upgraded M1.2 runtime against the pre-M1.2 dataDir using the default embedded flow.
	rt, err := host.StartEphemeral(ctx, host.Options{
		DataDir: dataDir,
		UIDir:   uiDir,
		Version: "v0.3.0-m1.2",
	}, 3)
	if err != nil {
		t.Fatalf("upgrade StartEphemeral failed: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if stopErr := rt.Stop(stopCtx); stopErr != nil {
			t.Errorf("upgraded Stop: %v", stopErr)
		}
	})

	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	if err := rt.WaitReady(readyCtx); err != nil {
		t.Fatalf("upgraded WaitReady: %v", err)
	}

	ep := rt.Endpoints()
	assertHTTPGet(t, ep.HTTPBaseURL+"/readyz", http.StatusOK, "READY")

	// 3. Verify existing customized operator settings were preserved (not wiped out by file push).
	if !strings.HasSuffix(ep.TelemetryWS, "/operator-tel-ws") {
		t.Fatalf("upgraded TelemetryWS = %q, want preserved /operator-tel-ws suffix", ep.TelemetryWS)
	}
	customMetricsURL := fmt.Sprintf("http://127.0.0.1:%d/operator-custom-metrics", ep.MetricsPort)
	respCustom, err := http.Get(customMetricsURL) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", customMetricsURL, err)
	}
	metricsBody, _ := io.ReadAll(respCustom.Body)
	_ = respCustom.Body.Close()
	if respCustom.StatusCode != http.StatusOK || !bytes.Contains(metricsBody, []byte("go_goroutines")) {
		t.Fatalf("GET %s = (%d, %d bytes), want 200 Prometheus payload on preserved operator metrics path",
			customMetricsURL, respCustom.StatusCode, len(metricsBody))
	}
	defaultTelWSURL := strings.TrimSuffix(ep.TelemetryWS, "/operator-tel-ws") + "/ws"
	if badConn, badResp, badErr := websocket.DefaultDialer.DialContext(ctx, defaultTelWSURL, nil); badErr == nil {
		_ = badConn.Close()
		t.Fatalf("dial %s unexpectedly succeeded when operator customized path to /operator-tel-ws", defaultTelWSURL)
	} else if badResp != nil && badResp.Body != nil {
		_ = badResp.Body.Close()
	}

	js, err := rt.NATSClient().JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	bucketName, err := config.BucketName("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("BucketName: %v", err)
	}
	kv, err := js.KeyValue(ctx, bucketName)
	if err != nil {
		t.Fatalf("KeyValue(%q): %v", bucketName, err)
	}
	giEntry, err := kv.Get(ctx, "components.graph-index")
	if err != nil {
		t.Fatalf("kv.Get components.graph-index: %v", err)
	}
	var persistedGI struct {
		Config struct {
			CoalesceMS int `json:"coalesce_ms"`
		} `json:"config"`
	}
	if err := json.Unmarshal(giEntry.Value(), &persistedGI); err != nil {
		t.Fatalf("decode persisted graph-index: %v", err)
	}
	if persistedGI.Config.CoalesceMS != 225 {
		t.Fatalf("persisted graph-index coalesce_ms = %d, want preserved 225", persistedGI.Config.CoalesceMS)
	}
	mavEntry, err := kv.Get(ctx, "components."+host.MAVLinkIngestComponent)
	if err != nil {
		t.Fatalf("kv.Get components.%s: %v", host.MAVLinkIngestComponent, err)
	}
	if !bytes.Contains(mavEntry.Value(), []byte(`"mavlink-udp"`)) || !bytes.Contains(mavEntry.Value(), []byte(`14550`)) {
		t.Fatalf("persisted %s = %s, want factory mavlink-udp with un-overridden port 14550", host.MAVLinkIngestComponent, string(mavEntry.Value()))
	}

	// 4. Verify the migrated MAVLink UDP listener is bound and streams live telemetry onto the customized WS #1 path.
	ingestComp := rt.MAVLinkIngest()
	if ingestComp == nil || ingestComp.BoundAddr().String() != ep.MAVLinkUDPAddr {
		t.Fatalf("upgraded MAVLinkIngest = %v, want bound %s", ingestComp, ep.MAVLinkUDPAddr)
	}
	telWS := dialWebSocketWithRetry(ctx, t, ep.TelemetryWS)
	defer func() { _ = telWS.Close() }()

	udpDst, err := net.ResolveUDPAddr("udp", ep.MAVLinkUDPAddr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr(%q): %v", ep.MAVLinkUDPAddr, err)
	}
	udpClient, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	defer func() { _ = udpClient.Close() }()

	hbMsg, err := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
		Armed: true,
		Custom: mavcodec.CustomMode{
			Domain:           mavcodec.DomainRover,
			ControlAuthority: mavcodec.AuthorityCompanionTeleop,
			CompanionMode:    mavcodec.ModeTeleopVelocity,
			LinkTier:         mavcodec.LinkTierSITLLocal,
		},
	})
	if err != nil {
		t.Fatalf("EncodeVehicleHeartbeat: %v", err)
	}
	for i, msg := range []mavcodec.Message{
		hbMsg,
		mavcodec.LocalPositionNED{X: 77, Y: -3, Z: 0},
	} {
		raw, encErr := mavcodec.EncodeFrame(mavcodec.Frame{
			Sequence: uint8(i + 1), SystemID: 1, ComponentID: 1, Message: msg,
		})
		if encErr != nil {
			t.Fatalf("EncodeFrame: %v", encErr)
		}
		if _, wErr := udpClient.WriteToUDP(raw, udpDst); wErr != nil {
			t.Fatalf("WriteToUDP: %v", wErr)
		}
	}

	wsFrame := readWSFrame(t, telWS, 5*time.Second)
	var tf mavlink.TelemetryFrame
	if err := json.Unmarshal(wsFrame.Payload, &tf); err != nil {
		t.Fatalf("unmarshal upgraded WS #1 frame: %v", err)
	}
	if wsFrame.Subject != "bbops.telemetry.1" || tf.SysID != 1 || tf.MsgID != mavcodec.MsgIDLocalPositionNED || tf.NEDXYZ[0] != 77 {
		t.Fatalf("upgraded UDP -> custom WS #1 frame = (%q, %+v), want subject=bbops.telemetry.1 NEDXYZ[0]=77", wsFrame.Subject, tf)
	}

	// 5. Verify a corrupt pre-M1.2 KV store (missing required graph-ingest) fails closed WITHOUT mutating KV.
	corruptDir := t.TempDir()
	seedLegacyKV(t, corruptDir, func(c *config.Config) {
		delete(c.Components, "graph-ingest")
	})
	if _, corruptErr := host.StartEphemeral(ctx, host.Options{
		DataDir: corruptDir,
		UIDir:   uiDir,
	}, 1); corruptErr == nil || !strings.Contains(corruptErr.Error(), `required component "graph-ingest" is missing`) {
		t.Fatalf("corrupt KV StartEphemeral err = %v, want missing graph-ingest error", corruptErr)
	}
	verifySrv, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, JetStream: true,
		StoreDir: filepath.Join(corruptDir, "jetstream"),
	})
	if err != nil {
		t.Fatalf("verifySrv NewServer: %v", err)
	}
	verifySrv.Start()
	defer func() {
		verifySrv.Shutdown()
		verifySrv.WaitForShutdown()
	}()
	if !verifySrv.ReadyForConnections(10 * time.Second) {
		t.Fatal("verifySrv not ready")
	}
	verifyClient, err := natsclient.NewClient(verifySrv.ClientURL(), natsclient.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("verifyClient NewClient: %v", err)
	}
	if err := verifyClient.Connect(ctx); err != nil {
		t.Fatalf("verifyClient Connect: %v", err)
	}
	defer func() { _ = verifyClient.Close(context.Background()) }()
	verifyJS, err := verifyClient.JetStream()
	if err != nil {
		t.Fatalf("verifyClient JetStream: %v", err)
	}
	corruptKV, err := verifyJS.KeyValue(ctx, bucketName)
	if err != nil {
		t.Fatalf("verifyJS KeyValue: %v", err)
	}
	if _, getErr := corruptKV.Get(ctx, "components."+host.MAVLinkIngestComponent); !errors.Is(getErr, jetstream.ErrKeyNotFound) {
		t.Fatalf("corrupt KV unexpectedly mutated with components.%s (err=%v)", host.MAVLinkIngestComponent, getErr)
	}
}

type testExternalContactPayload struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Source      string    `json:"source"`
	Observed    time.Time `json:"observed"`
}

func (p *testExternalContactPayload) Schema() message.Type {
	return message.Type{Domain: "cop", Category: "contact", Version: "v1"}
}

func (p *testExternalContactPayload) Validate() error {
	if p.ID == "" {
		return errors.New("contact id required")
	}
	return nil
}

func (p *testExternalContactPayload) MarshalJSON() ([]byte, error) {
	type alias testExternalContactPayload
	return json.Marshal((*alias)(p))
}

func (p *testExternalContactPayload) UnmarshalJSON(data []byte) error {
	type alias testExternalContactPayload
	return json.Unmarshal(data, (*alias)(p))
}

func (p *testExternalContactPayload) EntityID() string { return p.ID }

func (p *testExternalContactPayload) Triples() []message.Triple {
	ts := p.Observed
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return []message.Triple{
		{
			Subject:    p.ID,
			Predicate:  vocabulary.DCTermsTitle,
			Object:     p.Title,
			Source:     "ais-ingest-test",
			Timestamp:  ts,
			Confidence: 1.0,
		},
		{
			Subject:    p.ID,
			Predicate:  "dc.terms.description",
			Object:     p.Description,
			Source:     "ais-ingest-test",
			Timestamp:  ts,
			Confidence: 1.0,
		},
		{
			Subject:    p.ID,
			Predicate:  "cop.contact.source",
			Object:     p.Source,
			Source:     "ais-ingest-test",
			Timestamp:  ts,
			Confidence: 1.0,
		},
	}
}

var _ graph.Graphable = (*testExternalContactPayload)(nil)

// spec: host-runtime / Read-only SKG lexical and neural search HTTP bridge
func TestRuntimeGraphGatewaySearchBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	rt, err := host.StartEphemeral(ctx, host.Options{
		DataDir: t.TempDir(),
		UIDir:   t.TempDir(),
		Version: "v0.2.0-search-test",
		RegisterPayloads: func(reg *payloadregistry.Registry) error {
			return reg.Register(&payloadregistry.Registration{
				Factory:     func() any { return &testExternalContactPayload{} },
				Domain:      "cop",
				Category:    "contact",
				Version:     "v1",
				Description: "In-process COP external contact test payload",
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

	searchURL := rt.Endpoints().HTTPBaseURL + host.GraphSearchFrontDoorPath

	// 1. Pre-ready POST returns 503 Service Unavailable, and GET returns 405 Method Not Allowed.
	if !rt.IsReady() {
		preResp, postErr := http.Post(searchURL, "application/json", strings.NewReader(`{"verb":"globalSearch","params":{"query":"rover"}}`)) //nolint:noctx
		if postErr != nil {
			t.Fatalf("pre-ready POST /graph-gateway/search: %v", postErr)
		}
		preBody, _ := io.ReadAll(preResp.Body)
		_ = preResp.Body.Close()
		if preResp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("pre-ready POST status = %d (%s), want 503", preResp.StatusCode, string(preBody))
		}
	}

	assertHTTPGet(t, searchURL, http.StatusMethodNotAllowed, "method not allowed\n")

	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	if err := rt.WaitReady(readyCtx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	// 2. Malformed or non-whitelisted requests fail closed with 400 after readiness.
	badResp, err := http.Post(searchURL, "application/json", strings.NewReader(`{"verb":"graph.mutation.apply","params":{"query":"rover"}}`)) //nolint:noctx
	if err != nil {
		t.Fatalf("malformed POST: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-whitelisted verb status = %d, want 400", badResp.StatusCode)
	}

	// 3. Ingest both an ownship UXS vehicle and an external COP contact into ENTITY_STATES.
	platform := rt.Platform()
	vehicleSnap, err := uxsvocab.NewVehicleSnapshot(platform.Org, platform.Platform, 2, uxsvocab.DomainRover)
	if err != nil {
		t.Fatalf("NewVehicleSnapshot: %v", err)
	}
	vehicleSnap.Title = "UXS Vanguard Rover"
	vehicleSnap.ObservedAt = time.Now().UTC().Truncate(time.Millisecond)
	vehicleID := vehicleSnap.EntityID()
	vehicleBytes, err := json.Marshal(message.NewBaseMessage(uxsvocab.VehiclePayloadType, vehicleSnap, uxsvocab.TripleSource))
	if err != nil {
		t.Fatalf("marshal vehicle BaseMessage: %v", err)
	}
	if err := rt.NATSClient().PublishToStream(ctx, "entity."+vehicleID, vehicleBytes); err != nil {
		t.Fatalf("publish vehicle entity: %v", err)
	}

	contactID := fmt.Sprintf("%s.%s.cop.ais.vessel.harbor7", platform.Org, platform.Platform)
	contact := &testExternalContactPayload{
		ID:          contactID,
		Title:       "AIS Harbor Escort",
		Description: "Coastal radar and AIS vessel near north channel",
		Source:      "ais",
		Observed:    time.Now().UTC().Truncate(time.Millisecond),
	}
	contactBytes, err := json.Marshal(message.NewBaseMessage(contact.Schema(), contact, "ais-ingest-test"))
	if err != nil {
		t.Fatalf("marshal contact BaseMessage: %v", err)
	}
	if err := rt.NATSClient().PublishToStream(ctx, "entity."+contactID, contactBytes); err != nil {
		t.Fatalf("publish contact entity: %v", err)
	}

	// 4. Poll globalSearch until graph-lexical indexes both entities, verifying matches + indexed_revision.
	waitForCondition(t, 10*time.Second, "globalSearch returns Vanguard Rover", func() bool {
		resp, ok := postGraphSearch(t, searchURL, `{"verb":"globalSearch","params":{"query":"Vanguard","limit":10}}`)
		if !ok || !resp.OK {
			return false
		}
		var qr graph.QueryResponse[graph.GlobalSearchData]
		if err := json.Unmarshal(resp.Result, &qr); err != nil || qr.Data.Text == nil {
			return false
		}
		return qr.IndexedRevision > 0 && slices.ContainsFunc(qr.Data.Text.Matches, func(m graph.TextSearchMatch) bool {
			return m.ID == vehicleID
		})
	})

	waitForCondition(t, 10*time.Second, "localSearch returns AIS Harbor Escort in its group", func() bool {
		body := fmt.Sprintf(`{"verb":"localSearch","params":{"entity_id":%q,"query":"Harbor","level":0,"limit":5}}`, contactID)
		resp, ok := postGraphSearch(t, searchURL, body)
		if !ok || !resp.OK {
			return false
		}
		var qr graph.QueryResponse[graph.LocalSearchData]
		if err := json.Unmarshal(resp.Result, &qr); err != nil {
			return false
		}
		return qr.IndexedRevision > 0 && slices.ContainsFunc(qr.Data.Matches, func(m graph.TextSearchMatch) bool {
			return m.ID == contactID
		})
	})

	// 5. Verify semantic and similar surface structured not_enabled refusals when embedding_enabled: false.
	for _, tc := range []struct {
		verb string
		body string
	}{
		{verb: "semantic", body: `{"verb":"semantic","params":{"query":"coastal escort","limit":5}}`},
		{verb: "similar", body: fmt.Sprintf(`{"verb":"similar","params":{"entity_id":%q,"limit":5}}`, vehicleID)},
	} {
		resp, ok := postGraphSearch(t, searchURL, tc.body)
		if !ok {
			t.Fatalf("%s HTTP call failed", tc.verb)
		}
		if resp.OK || resp.Error == nil || resp.Error.Code != "not_enabled" {
			t.Fatalf("%s response = %+v (err=%+v), want ok=false error.code=not_enabled", tc.verb, resp, resp.Error)
		}
	}
}

func postGraphSearch(t *testing.T, searchURL, body string) (host.GraphSearchResponse, bool) {
	t.Helper()
	httpResp, err := http.Post(searchURL, "application/json", strings.NewReader(body)) //nolint:noctx
	if err != nil {
		return host.GraphSearchResponse{}, false
	}
	raw, readErr := io.ReadAll(httpResp.Body)
	_ = httpResp.Body.Close()
	if readErr != nil || httpResp.StatusCode != http.StatusOK {
		return host.GraphSearchResponse{}, false
	}
	var parsed host.GraphSearchResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return host.GraphSearchResponse{}, false
	}
	return parsed, true
}
