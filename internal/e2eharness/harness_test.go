package e2eharness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boxbotix/bbops/internal/e2eharness"
	"github.com/boxbotix/bbops/internal/mavlink"
	"github.com/gorilla/websocket"
)

// spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
func TestEulerAndGeodesyOraclesAndValidation(t *testing.T) {
	// Independent 3x3 ZYX rotation matrix oracle: R = Rz(yaw) * Ry(pitch) * Rx(roll)
	// applied to body forward [1, 0, 0]^T must equal quaternion rotation q * [0, 1, 0, 0] * q*.
	cases := [][3]float64{
		{0, 0, 0},
		{14, -8, 132},
		{-25, 19, 270},
		{3, 1, 90},
	}
	for _, tc := range cases {
		rollRad := tc[0] * math.Pi / 180.0
		pitchRad := tc[1] * math.Pi / 180.0
		yawRad := tc[2] * math.Pi / 180.0

		wantNorth := math.Cos(yawRad) * math.Cos(pitchRad)
		wantEast := math.Sin(yawRad) * math.Cos(pitchRad)
		wantDown := -math.Sin(pitchRad)
		_ = rollRad

		q := e2eharness.EulerZYXDegToQuaternion(tc[0], tc[1], tc[2])
		w, x, y, z := float64(q[0]), float64(q[1]), float64(q[2]), float64(q[3])
		norm := math.Sqrt(w*w + x*x + y*y + z*z)
		if math.Abs(norm-1.0) > 1e-5 {
			t.Fatalf("quaternion norm for %v = %v, want 1.0", tc, norm)
		}

		// Rotate body X=[1,0,0] to NED via q * (0, 1, 0, 0) * q_conj:
		gotNorth := 1.0 - 2.0*(y*y+z*z)
		gotEast := 2.0 * (x*y + w*z)
		gotDown := 2.0 * (x*z - w*y)
		if math.Abs(gotNorth-wantNorth) > 1e-5 || math.Abs(gotEast-wantEast) > 1e-5 || math.Abs(gotDown-wantDown) > 1e-5 {
			t.Fatalf("rotated forward vector for %v = [%v,%v,%v], want [%v,%v,%v]",
				tc, gotNorth, gotEast, gotDown, wantNorth, wantEast, wantDown)
		}

		// Rotate body Y=[0,1,0] to NED to verify roll dependence as well:
		sr, cr := math.Sin(rollRad), math.Cos(rollRad)
		sp, cp := math.Sin(pitchRad), math.Cos(pitchRad)
		sy, cy := math.Sin(yawRad), math.Cos(yawRad)
		wantRightNorth := cy*sp*sr - sy*cr
		wantRightEast := sy*sp*sr + cy*cr
		wantRightDown := cp * sr
		gotRightNorth := 2.0 * (x*y - w*z)
		gotRightEast := 1.0 - 2.0*(x*x+z*z)
		gotRightDown := 2.0 * (y*z + w*x)
		if math.Abs(gotRightNorth-wantRightNorth) > 1e-5 ||
			math.Abs(gotRightEast-wantRightEast) > 1e-5 ||
			math.Abs(gotRightDown-wantRightDown) > 1e-5 {
			t.Fatalf("rotated right vector for %v = [%v,%v,%v], want [%v,%v,%v]",
				tc, gotRightNorth, gotRightEast, gotRightDown, wantRightNorth, wantRightEast, wantRightDown)
		}
	}

	origin := [3]float64{37.7749, -122.4194, 10.0}
	lla, err := e2eharness.LocalNEDToLLA([3]float64{100.0, 200.0, -50.0}, origin)
	if err != nil {
		t.Fatalf("LocalNEDToLLA valid: %v", err)
	}
	if lla[0] <= origin[0] || lla[1] <= origin[1] || math.Abs(lla[2]-60.0) > 1e-6 {
		t.Fatalf("LocalNEDToLLA([100,200,-50]) = %v, expected NE offset and alt=60m", lla)
	}
	if _, badErr := e2eharness.LocalNEDToLLA([3]float64{0, 0, 0}, [3]float64{95.0, 0, 0}); badErr == nil {
		t.Fatal("expected LocalNEDToLLA to reject out-of-bounds origin latitude")
	}
	if _, badNedErr := e2eharness.LocalNEDToLLA([3]float64{math.NaN(), 0, 0}, origin); badNedErr == nil {
		t.Fatal("expected LocalNEDToLLA to reject NaN NED component")
	}

	// Missing UIDir / missing index.html must fail closed before starting runtime.
	if _, err := e2eharness.StartStack(context.Background(), e2eharness.Options{}); err == nil {
		t.Fatal("expected StartStack with empty UIDir to fail")
	}
	emptyDir := t.TempDir()
	if _, err := e2eharness.StartStack(context.Background(), e2eharness.Options{UIDir: emptyDir}); err == nil {
		t.Fatal("expected StartStack without index.html to fail")
	}

	badSeeds := []e2eharness.ExternalContactSeed{
		{ID: "boxbotix.local.robotics.uxs.vehicle.1", Title: "Not COP", Latitude: 37.7, Longitude: -122.4},
		{ID: "boxbotix.local.sensor.cop.vessel.1", Title: "   ", Latitude: 37.7, Longitude: -122.4},
		{ID: "boxbotix.local.sensor.cop.vessel.1", Title: "Bad Lat", Latitude: 91.0, Longitude: -122.4},
		{ID: "boxbotix.local.sensor.cop.vessel.1", Title: "Bad Lon", Latitude: 37.7, Longitude: -181.0},
	}
	for i, seed := range badSeeds {
		if err := seed.Validate(); err == nil {
			t.Fatalf("badSeeds[%d] (%+v) unexpectedly passed Validate()", i, seed)
		}
	}
}

// spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
func TestStartStackStreamsMultiDomainTelemetryEdgesAndPathSearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	uiDir := t.TempDir()
	const wantIndex = "<!doctype html><title>bbops E2E</title>"
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte(wantIndex), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	stack, err := e2eharness.StartStack(ctx, e2eharness.Options{
		UIDir:             uiDir,
		TelemetryInterval: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("StartStack: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if stopErr := stack.Stop(stopCtx); stopErr != nil {
			t.Errorf("Stop: %v", stopErr)
		}
	})

	info := stack.Info()
	if !info.Ready || !strings.HasPrefix(info.BaseURL, "http://127.0.0.1:") {
		t.Fatalf("unexpected StackInfo: %+v", info)
	}

	// Verify front-door static UI serving.
	resp, err := http.Get(info.BaseURL + "/") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != wantIndex {
		t.Fatalf("GET / = %d %q, want 200 %q", resp.StatusCode, string(body), wantIndex)
	}

	// Verify WS #1 (/ws/telemetry) receives live frames and Lane 3 transitions via PulseCRSFOverride + EmitStatusText.
	wsBase := strings.Replace(info.BaseURL, "http://", "ws://", 1)
	telConn, telHTTPResp, err := websocket.DefaultDialer.DialContext(ctx, wsBase+"/ws/telemetry", http.Header{
		"Origin": []string{info.BaseURL},
	})
	if telHTTPResp != nil && telHTTPResp.Body != nil {
		_ = telHTTPResp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial /ws/telemetry: %v", err)
	}
	defer func() { _ = telConn.Close() }()

	if err := stack.PulseCRSFOverride(99); err == nil {
		t.Fatal("expected PulseCRSFOverride(99) to reject unknown sysid")
	}
	if err := stack.EmitStatusText(1, 3, ""); err == nil {
		t.Fatal("expected EmitStatusText with empty text to fail")
	}
	if err := stack.PulseCRSFOverride(1); err != nil {
		t.Fatalf("PulseCRSFOverride(1): %v", err)
	}
	if err := stack.EmitStatusText(3, 4, "CROSS CURRENT DRIFT"); err != nil {
		t.Fatalf("EmitStatusText(3): %v", err)
	}

	sawAuthorityEvent := false
	sawStatusTextEvent := false
	sawBoatKinematics := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && (!sawAuthorityEvent || !sawStatusTextEvent || !sawBoatKinematics) {
		_ = telConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, raw, readErr := telConn.ReadMessage()
		if readErr != nil {
			t.Fatalf("read /ws/telemetry: %v", readErr)
		}
		var env struct {
			Type    string          `json:"type"`
			Subject string          `json:"subject"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || env.Type != "data" {
			continue
		}
		if strings.HasPrefix(env.Subject, "bbops.telemetry.events.") {
			var ev mavlink.TransitionEvent
			if err := json.Unmarshal(env.Payload, &ev); err != nil {
				t.Fatalf("decode TransitionEvent: %v", err)
			}
			if ev.SysID == 1 && ev.Kind == mavlink.EventKindHeartbeatEdge && ev.Authority == "direct-crsf-override" {
				sawAuthorityEvent = true
			}
			if ev.SysID == 3 && ev.Kind == mavlink.EventKindStatusText && ev.StatusText != nil && ev.StatusText.Text == "CROSS CURRENT DRIFT" {
				sawStatusTextEvent = true
			}
		} else if env.Subject == "bbops.telemetry.3" {
			var tf mavlink.TelemetryFrame
			if err := json.Unmarshal(env.Payload, &tf); err == nil && tf.HasLocalPos && tf.NEDVel[0] > 1.0 && tf.NEDVel[1] > 1.0 {
				sawBoatKinematics = true
			}
		}
	}
	if !sawAuthorityEvent || !sawStatusTextEvent || !sawBoatKinematics {
		t.Fatalf("missing expected WS1 frames: authority=%v statustext=%v boatKinematics=%v",
			sawAuthorityEvent, sawStatusTextEvent, sawBoatKinematics)
	}

	// Verify exact GraphQL pathSearch query shape used by ui/src/lib/graphql.ts.
	gqlReq, err := json.Marshal(map[string]any{
		"query": `query PathSearch($startEntity: String!, $direction: String!, $maxDepth: Int!, $maxNodes: Int) {
			pathSearch(startEntity: $startEntity, direction: $direction, maxDepth: $maxDepth, maxNodes: $maxNodes) {
				entities { id depth }
				paths { from predicate to }
				truncated
			}
		}`,
		"variables": map[string]any{
			"startEntity": info.Vehicles.Quadcopter,
			"direction":   "both",
			"maxDepth":    2,
			"maxNodes":    50,
		},
	})
	if err != nil {
		t.Fatalf("marshal GraphQL query: %v", err)
	}
	gqlResp, err := http.Post(info.BaseURL+"/graph-gateway/graphql", "application/json", bytes.NewReader(gqlReq)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST /graph-gateway/graphql: %v", err)
	}
	gqlRaw, _ := io.ReadAll(gqlResp.Body)
	_ = gqlResp.Body.Close()
	if gqlResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /graph-gateway/graphql = %d %s", gqlResp.StatusCode, string(gqlRaw))
	}
	var gqlOut struct {
		Data struct {
			PathSearch *struct {
				Entities []struct {
					ID    string `json:"id"`
					Depth int    `json:"depth"`
				} `json:"entities"`
				Paths [][]struct {
					From      string `json:"from"`
					Predicate string `json:"predicate"`
					To        string `json:"to"`
				} `json:"paths"`
				Truncated bool `json:"truncated"`
			} `json:"pathSearch"`
		} `json:"data"`
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := json.Unmarshal(gqlRaw, &gqlOut); err != nil {
		t.Fatalf("decode pathSearch response %s: %v", string(gqlRaw), err)
	}
	if len(gqlOut.Errors) > 0 || gqlOut.Data.PathSearch == nil || len(gqlOut.Data.PathSearch.Paths) == 0 {
		t.Fatalf("unexpected pathSearch response: %s", string(gqlRaw))
	}

	obs := time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC)
	rec := obs.Add(12500 * time.Millisecond)
	exp := obs.Add(5 * time.Minute)
	unc := 14.5
	if err := stack.UpsertExternalContact(ctx, e2eharness.ExternalContactSeed{
		ID:                info.Org + "." + info.Platform + ".sensor.cop.vessel.delayed-check",
		Title:             "Delayed Check Contact",
		ContactType:       "cargo-vessel",
		Source:            "ais-test",
		FeedStatus:        "degraded",
		Latitude:          37.772,
		Longitude:         -122.423,
		ObservedAt:        &obs,
		ReceivedAt:        &rec,
		ExpiresAt:         &exp,
		UncertaintyMeters: &unc,
	}); err != nil {
		t.Fatalf("UpsertExternalContact full provenance: %v", err)
	}
}
