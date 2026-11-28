package mavlink

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxbotix/bbops/internal/mavcodec"
	"github.com/boxbotix/bbops/internal/uxsvocab"
	"github.com/c360studio/semengine/component"
	"github.com/c360studio/semengine/graph"
	"github.com/c360studio/semengine/message"
	"github.com/c360studio/semengine/natsclient"
	"github.com/c360studio/semengine/payloadregistry"
	"github.com/c360studio/semengine/types"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

func allocateLoopbackUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP ephemeral: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port
}

func makeVehicleHB(
	domain mavcodec.VehicleDomain,
	armed bool,
	auth mavcodec.ControlAuthority,
	mode mavcodec.CompanionMode,
	tier mavcodec.LinkTier,
	flags uxsvocab.HealthFlags,
) mavcodec.Heartbeat {
	hb, err := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
		Armed: armed,
		Custom: mavcodec.CustomMode{
			Domain:           domain,
			ControlAuthority: auth,
			CompanionMode:    mode,
			LinkTier:         tier,
			HealthFlags:      flags,
		},
	})
	if err != nil {
		panic(err)
	}
	return hb
}

func encodeFrameBytes(t *testing.T, sysid, compid, seq uint8, msg mavcodec.Message) []byte {
	t.Helper()
	raw, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    seq,
		SystemID:    sysid,
		ComponentID: compid,
		Message:     msg,
	})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	return raw
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestConfigAndComponentLifecycle(t *testing.T) {
	t.Run("resolveConfig applies defaults and enforces rate, port, and subject invariants", func(t *testing.T) {
		for _, emptyRaw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{}`)} {
			s, err := resolveConfig(emptyRaw)
			if err != nil {
				t.Fatalf("resolveConfig(%q): %v", string(emptyRaw), err)
			}
			if s.udpHost != DefaultUDPHost || s.udpPort != DefaultUDPPort {
				t.Fatalf("default UDP = %s:%d, want %s:%d", s.udpHost, s.udpPort, DefaultUDPHost, DefaultUDPPort)
			}
			if s.skgHz != 1.0 || s.skgInterval != time.Second {
				t.Fatalf("default skg = (%v, %v), want (1.0, 1s)", s.skgHz, s.skgInterval)
			}
			if s.linkTimeout != 3*time.Second || s.supervisoryTimeout != 10*time.Second {
				t.Fatalf("default timeouts = (%v, %v), want (3s, 10s)", s.linkTimeout, s.supervisoryTimeout)
			}
			if s.telemetryPrefix != DefaultTelemetryPrefix || s.entitySubject != DefaultEntitySubject {
				t.Fatalf("default subjects = (%q, %q)", s.telemetryPrefix, s.entitySubject)
			}
		}

		sCustomTimeouts, err := resolveConfig(json.RawMessage(`{"link_timeout_ms":400,"supervisory_timeout_ms":1200}`))
		if err != nil {
			t.Fatalf("resolveConfig custom timeouts: %v", err)
		}
		if sCustomTimeouts.linkTimeout != 400*time.Millisecond || sCustomTimeouts.supervisoryTimeout != 1200*time.Millisecond {
			t.Fatalf("custom timeouts = (%v, %v), want (400ms, 1200ms)", sCustomTimeouts.linkTimeout, sCustomTimeouts.supervisoryTimeout)
		}

		sAutoRaisedSup, err := resolveConfig(json.RawMessage(`{"link_timeout_ms":15000}`))
		if err != nil {
			t.Fatalf("resolveConfig raised link_timeout_ms: %v", err)
		}
		if sAutoRaisedSup.linkTimeout != 15*time.Second || sAutoRaisedSup.supervisoryTimeout != 15*time.Second {
			t.Fatalf("auto-raised supervisory timeout = (%v, %v), want (15s, 15s)", sAutoRaisedSup.linkTimeout, sAutoRaisedSup.supervisoryTimeout)
		}

		for _, validHz := range []struct {
			raw  string
			want time.Duration
		}{
			{`{"skg_hz":1}`, time.Second},
			{`{"skg_hz":2.5}`, 400 * time.Millisecond},
			{`{"skg_hz":5}`, 200 * time.Millisecond},
		} {
			s, err := resolveConfig(json.RawMessage(validHz.raw))
			if err != nil {
				t.Fatalf("resolveConfig(%s): %v", validHz.raw, err)
			}
			if s.skgInterval != validHz.want {
				t.Fatalf("skgInterval(%s) = %v, want %v", validHz.raw, s.skgInterval, validHz.want)
			}
		}

		emptyHostCfg := json.RawMessage(`{
			"skg_hz": 4,
			"ports": {
				"inputs": [{"name":"mavlink_udp","external":true,"config":{"kind":"network","protocol":"UDP","host":"","port":14555}}],
				"outputs": [
					{"name":"telemetry","config":{"kind":"nats","subject":"bbops.telemetry.*"}},
					{"name":"entity_upsert","config":{"kind":"nats","subject":"entity.uxs.upsert"}}
				]
			}
		}`)
		sEmptyHost, err := resolveConfig(emptyHostCfg)
		if err != nil {
			t.Fatalf("resolveConfig empty host: %v", err)
		}
		if sEmptyHost.udpHost != "0.0.0.0" || sEmptyHost.udpPort != 14555 || sEmptyHost.telemetryPrefix != "bbops.telemetry" {
			t.Fatalf("resolved empty-host config = %+v", sEmptyHost)
		}

		invalidConfigs := []string{
			`{"unknown_field": true}`,
			`{"skg_hz": 0}`,
			`{"skg_hz": 0.99}`,
			`{"skg_hz": 5.01}`,
			`{"skg_hz": -1}`,
			`{"link_timeout_ms": 0}`,
			`{"link_timeout_ms": 199}`,
			`{"link_timeout_ms": 300001}`,
			`{"supervisory_timeout_ms": 199}`,
			`{"supervisory_timeout_ms": 300001}`,
			`{"link_timeout_ms": 4000, "supervisory_timeout_ms": 3999}`,
			`{"ports":{"inputs":[{"name":"a","config":{"kind":"network","protocol":"udp","port":14550}},{"name":"b","config":{"kind":"network","protocol":"udp","port":14551}}]}}`,
			`{"ports":{"inputs":[{"name":"mavlink_udp","config":{"kind":"network","protocol":"tcp","port":14550}}]}}`,
			`{"ports":{"inputs":[{"name":"mavlink_udp","config":{"kind":"network","protocol":"udp","port":1023}}]}}`,
			`{"ports":{"inputs":[{"name":"mavlink_udp","config":{"kind":"network","protocol":"udp","port":65536}}]}}`,
			`{"ports":{"outputs":[{"name":"telemetry","config":{"kind":"nats","subject":"bbops.telemetry.>"}}]}}`,
			`{"ports":{"outputs":[{"name":"telemetry","config":{"kind":"nats","subject":"bbops.telemetry.>"}},{"name":"telemetry","config":{"kind":"nats","subject":"bbops.telemetry.>"}}]}}`,
			`{"ports":{"outputs":[{"name":"entity_upsert","config":{"kind":"nats","subject":"entity.uxs.upsert"}},{"name":"entity_upsert","config":{"kind":"nats","subject":"entity.uxs.upsert"}}]}}`,
			`{"ports":{"outputs":[{"name":"telemetry","config":{"kind":"nats","subject":"bbops.telemetry.>"}},{"name":"bogus","config":{"kind":"nats","subject":"entity.uxs.upsert"}}]}}`,
			`{"ports":{"outputs":[{"name":"telemetry","config":{"kind":"nats","subject":"bbops.telemetry.>"}},{"name":"entity_upsert","config":{"kind":"nats","subject":"   "}}]}}`,
			`{"ports":{"outputs":[{"name":"telemetry","config":{"kind":"nats","subject":".>"}},{"name":"entity_upsert","config":{"kind":"nats","subject":"entity.uxs.upsert"}}]}}`,
			`{"ports":{"outputs":[{"name":"telemetry","config":{"kind":"nats","subject":"bbops.*.telemetry.>"}},{"name":"entity_upsert","config":{"kind":"nats","subject":"entity.uxs.upsert"}}]}}`,
			`{"ports":{"outputs":[{"name":"telemetry","config":{"kind":"nats","subject":"bbops.telemetry.>"}},{"name":"entity_upsert","config":{"kind":"nats","subject":"entity.uxs.>"}}]}}`,
			`{"ports":{"outputs":[{"name":"telemetry","config":{"kind":"nats","subject":"bbops.telemetry.>"}},{"name":"entity_upsert","config":{"kind":"nats","subject":".entity.uxs"}}]}}`,
		}
		for _, bad := range invalidConfigs {
			if _, err := resolveConfig(json.RawMessage(bad)); err == nil {
				t.Fatalf("expected resolveConfig error for %s", bad)
			}
			if _, err := declarePorts(json.RawMessage(bad), DefaultInstanceName); err == nil {
				t.Fatalf("expected declarePorts error for %s", bad)
			}
		}

		if _, err := NewTrackerWithTimeouts("boxbotix", "bbops", 0, time.Second); err == nil {
			t.Fatal("expected NewTrackerWithTimeouts error for non-positive linkTimeout")
		}
		if _, err := NewTrackerWithTimeouts("boxbotix", "bbops", 4*time.Second, 3*time.Second); err == nil {
			t.Fatal("expected NewTrackerWithTimeouts error when supervisoryTimeout < linkTimeout")
		}
	})

	t.Run("component registration and ADR-108/113 lifecycle invariants", func(t *testing.T) {
		if err := Register(nil); err == nil {
			t.Fatal("expected error for nil registry")
		}
		reg := component.NewRegistry()
		var hooked *Component
		if err := RegisterWithHook(reg, func(c *Component) { hooked = c }); err != nil {
			t.Fatalf("RegisterWithHook: %v", err)
		}
		if _, err := declarePorts(json.RawMessage(`{}`), DefaultInstanceName); err != nil {
			t.Fatalf("declarePorts valid: %v", err)
		}

		// Stop before Start is a safe no-op; Start before Initialize fails.
		uninitComp, err := NewComponent(json.RawMessage(`{}`), component.Dependencies{})
		if err != nil {
			t.Fatalf("NewComponent: %v", err)
		}
		if err := uninitComp.Stop(context.Background()); err != nil {
			t.Fatalf("Stop before Start: %v", err)
		}
		if err := uninitComp.Start(context.Background()); err == nil {
			t.Fatal("expected Start before Initialize to fail")
		}
		if err := uninitComp.Initialize(); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		canceledCtx, cancelNow := context.WithCancel(context.Background())
		cancelNow()
		if err := uninitComp.Start(canceledCtx); err == nil {
			t.Fatal("expected Start with canceled context to fail")
		}
		if err := uninitComp.Start(context.Background()); err == nil {
			t.Fatal("expected Start with nil NATSClient to fail")
		}

		// Boot a minimal embedded NATS server to exercise live Start / UDP bind / Stop release.
		srv, err := natsserver.NewServer(&natsserver.Options{
			Host:      "127.0.0.1",
			Port:      -1,
			NoLog:     true,
			NoSigs:    true,
			JetStream: true,
			StoreDir:  t.TempDir(),
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		srv.Start()
		t.Cleanup(func() {
			srv.Shutdown()
			srv.WaitForShutdown()
		})
		if !srv.ReadyForConnections(5 * time.Second) {
			t.Fatal("embedded NATS server not ready")
		}

		nc, err := natsclient.NewClient(srv.ClientURL(), natsclient.WithMaxReconnects(0))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if err := nc.Connect(context.Background()); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		t.Cleanup(func() { _ = nc.Close(context.Background()) })

		udpPort := allocateLoopbackUDPPort(t)
		rawCfg, err := json.Marshal(map[string]any{
			"skg_hz": 5,
			"ports": map[string]any{
				"inputs": []map[string]any{
					{
						"name":     "mavlink_udp",
						"external": true,
						"config": map[string]any{
							"kind":     "network",
							"protocol": "udp",
							"host":     "127.0.0.1",
							"port":     udpPort,
						},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("marshal rawCfg: %v", err)
		}

		compCfg := types.ComponentConfig{
			Type:    types.ComponentTypeInput,
			Name:    FactoryName,
			Enabled: true,
			Config:  rawCfg,
		}
		if _, err := reg.Declare(DefaultInstanceName, compCfg); err != nil {
			t.Fatalf("reg.Declare: %v", err)
		}
		disc, err := reg.CreateComponent(DefaultInstanceName, compCfg, component.Dependencies{
			NATSClient: nc,
			Platform:   types.PlatformMeta{Org: "boxbotix", Platform: "bbops"},
		}, nil)
		if err != nil {
			t.Fatalf("factory construct: %v", err)
		}
		liveComp, ok := disc.(*Component)
		if !ok || hooked != liveComp {
			t.Fatalf("constructed component = %T (hooked=%p, live=%p)", disc, hooked, liveComp)
		}
		if len(liveComp.InputPorts()) != 1 || len(liveComp.OutputPorts()) != 2 {
			t.Fatalf("ports = (%d in, %d out), want (1, 2)", len(liveComp.InputPorts()), len(liveComp.OutputPorts()))
		}
		if liveComp.BoundAddr().IsValid() || liveComp.UDPConn() != nil {
			t.Fatal("expected unbound UDP state before Start")
		}

		if err := liveComp.Initialize(); err != nil {
			t.Fatalf("liveComp.Initialize: %v", err)
		}
		startCtx, startCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer startCancel()
		if err := liveComp.Start(startCtx); err != nil {
			t.Fatalf("liveComp.Start: %v", err)
		}
		if !liveComp.Health().Healthy {
			t.Fatalf("Health = %+v, want Healthy=true", liveComp.Health())
		}
		_ = liveComp.DataFlow()
		if gotPort := int(liveComp.BoundAddr().Port()); gotPort != udpPort || liveComp.UDPConn() == nil {
			t.Fatalf("BoundAddr = %v (conn=%v), want port %d", liveComp.BoundAddr(), liveComp.UDPConn(), udpPort)
		}
		if err := liveComp.Start(startCtx); err == nil {
			t.Fatal("expected second Start while running to fail")
		}

		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := liveComp.Stop(stopCtx); err != nil {
			t.Fatalf("liveComp.Stop: %v", err)
		}
		if liveComp.UDPConn() != nil || liveComp.Health().Healthy {
			t.Fatalf("post-Stop state: conn=%v health=%+v", liveComp.UDPConn(), liveComp.Health())
		}
		rebound, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: udpPort})
		if err != nil {
			t.Fatalf("UDP socket %d not released after Stop: %v", udpPort, err)
		}
		_ = rebound.Close()
	})
}

// referenceBodyXHeadingDeg computes the horizontal NED heading of the body +X axis
// directly from the ZYX direction-cosine matrix R = Rz(yaw)*Ry(pitch)*Rx(roll)
// without touching quaternion formulas, serving as an independent mathematical oracle.
func referenceBodyXHeadingDeg(rollDeg, pitchDeg, yawDeg float64) (quatWXYZ [4]float64, wantHeadingDeg float64) {
	phi := rollDeg * (math.Pi / 180.0)
	theta := pitchDeg * (math.Pi / 180.0)
	psi := yawDeg * (math.Pi / 180.0)

	// Body +X unit vector [1, 0, 0]^T mapped to NED via R = Rz(psi)*Ry(theta)*Rx(phi):
	// north = R_11 = cos(psi)*cos(theta), east = R_21 = sin(psi)*cos(theta).
	r11 := math.Cos(psi) * math.Cos(theta)
	r21 := math.Sin(psi) * math.Cos(theta)
	want := math.Atan2(r21, r11) * (180.0 / math.Pi)
	if want < 0 {
		want += 360.0
	}
	if want >= 360.0 || math.Abs(want) < 1e-12 {
		want = 0
	}

	cr, sr := math.Cos(phi*0.5), math.Sin(phi*0.5)
	cp, sp := math.Cos(theta*0.5), math.Sin(theta*0.5)
	cy, sy := math.Cos(psi*0.5), math.Sin(psi*0.5)

	w := cy*cp*cr + sy*sp*sr
	x := cy*cp*sr - sy*sp*cr
	y := cy*sp*cr + sy*cp*sr
	z := sy*cp*cr - cy*sp*sr
	return [4]float64{w, x, y, z}, want
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestQuaternionYawDegIndependentOracle(t *testing.T) {
	tracker, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	peer := netip.MustParseAddrPort("127.0.0.1:41001")
	t0 := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)

	hb := makeVehicleHB(
		mavcodec.DomainSurfaceBoat,
		true,
		mavcodec.AuthorityCompanionAutonomous,
		mavcodec.ModeAutonomousVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{},
	)
	if out := tracker.IngestDatagram(encodeFrameBytes(t, 3, 1, 0, hb), peer, t0); !out.Accepted {
		t.Fatalf("admit USV heartbeat failed: %+v", out)
	}

	angles := []struct {
		roll, pitch, yaw float64
		scale            float32
	}{
		{0, 0, 0, 1.0},
		{0, 0, 90, 1.0},
		{0, 0, 180, -1.0},
		{0, 0, 270, 2.5},
		{0, 0, 359.75, 0.4},
		{0, 0, -1e-9, 1.0},
		{22.5, -15.0, 42.25, 1.0},
		{-35.0, 28.0, 137.5, -1.75},
		{48.0, 33.0, 224.8, 3.2},
		{-60.0, -45.0, 315.125, 0.8},
	}

	for i, tc := range angles {
		q, wantYaw := referenceBodyXHeadingDeg(tc.roll, tc.pitch, tc.yaw)
		att := mavcodec.AttitudeQuaternion{
			TimeBootMs: uint32(100 + i*50),
			Q1:         float32(q[0]) * tc.scale,
			Q2:         float32(q[1]) * tc.scale,
			Q3:         float32(q[2]) * tc.scale,
			Q4:         float32(q[3]) * tc.scale,
			Rollspeed:  0.1,
			Pitchspeed: -0.2,
			Yawspeed:   0.3,
		}
		out := tracker.IngestDatagram(encodeFrameBytes(t, 3, 1, uint8(i+1), att), peer, t0.Add(time.Duration(i)*50*time.Millisecond))
		if !out.Accepted || out.CountedError || out.Telemetry == nil {
			t.Fatalf("case %d (%+v): unexpected outcome %+v", i, tc, out)
		}
		gotQ := out.Telemetry.QuatWXYZ
		norm := math.Sqrt(gotQ[0]*gotQ[0] + gotQ[1]*gotQ[1] + gotQ[2]*gotQ[2] + gotQ[3]*gotQ[3])
		if math.Abs(norm-1.0) > 1e-6 {
			t.Fatalf("case %d: normalized quaternion norm = %v, want 1.0", i, norm)
		}
		gotYaw := out.Telemetry.HdgDeg
		if gotYaw < 0 || gotYaw >= 360.0 || math.Signbit(gotYaw) {
			t.Fatalf("case %d: HdgDeg = %v (signbit=%v), want [0, 360) without -0", i, gotYaw, math.Signbit(gotYaw))
		}
		diff := math.Abs(gotYaw - wantYaw)
		if diff > 180.0 {
			diff = 360.0 - diff
		}
		if diff > 1e-2 {
			t.Fatalf("case %d (r=%v p=%v y=%v scale=%v): HdgDeg = %v, want %v (diff=%v)",
				i, tc.roll, tc.pitch, tc.yaw, tc.scale, gotYaw, wantYaw, diff)
		}
	}
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestTrackerSessionAdmissionSanitizationAndThreeLanes(t *testing.T) {
	if _, err := NewTracker("Bad Org!", "bbops"); err == nil {
		t.Fatal("expected NewTracker error for invalid org segment")
	}

	tracker, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	peerA := netip.MustParseAddrPort("127.0.0.1:45001")
	peerB := netip.MustParseAddrPort("127.0.0.1:45002")
	t0 := time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC)

	// 1. Pre-admission packets for sysid=2 are ignored without admitting a ghost session.
	prePackets := []mavcodec.Message{
		mavcodec.SysStatus{Load: 500, ErrorsCount1: 1},
		mavcodec.AttitudeQuaternion{Q1: 1},
		mavcodec.LocalPositionNED{X: 1, Y: 2, Z: -3},
		mavcodec.GlobalPositionInt{Lat: 377749000, Lon: -1224194000, Alt: 15000, Hdg: 9000},
		mavcodec.StatusText{Severity: 4, Text: "PRE_ADMIT"},
	}
	for _, msg := range prePackets {
		out := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 1, msg), peerA, t0)
		if out.Accepted || out.CountedError || out.Telemetry != nil || len(out.Events) != 0 || out.UrgentSKGSync {
			t.Fatalf("pre-admission %T unexpected outcome: %+v", msg, out)
		}
	}
	if _, ok := tracker.Snapshot(2); ok {
		t.Fatal("ghost session created before first valid 0xB1 HEARTBEAT")
	}
	if _, ok := tracker.PeerAddr(2); ok {
		t.Fatal("peerAddr recorded before first valid 0xB1 HEARTBEAT")
	}

	// 2. Peer GCS heartbeats (MAV_TYPE_GCS=6, custom_mode=0) are ignored with CountedError=false,
	// whereas malformed frames, MAVLink v1, invalid sysid/compid, and non-0xB1 vehicle heartbeats count errors.
	gcsHB := mavcodec.Heartbeat{
		Type:         6, // MAV_TYPE_GCS
		Autopilot:    8, // MAV_AUTOPILOT_INVALID
		BaseMode:     0,
		CustomMode:   0,
		SystemStatus: 4,
	}
	gcsOut := tracker.IngestDatagram(encodeFrameBytes(t, 255, 190, 1, gcsHB), peerA, t0)
	if gcsOut.Accepted || gcsOut.CountedError {
		t.Fatalf("peer GCS heartbeat outcome = %+v, want silently ignored", gcsOut)
	}

	validRoverHB := makeVehicleHB(
		mavcodec.DomainRover,
		false,
		mavcodec.AuthorityMCUFailsafe,
		mavcodec.ModeDisarmed,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{},
	)
	for name, badDatagram := range map[string][]byte{
		"truncated":      {0xFD, 0x01, 0x02},
		"mavlink_v1":     {0xFE, 0x09, 0x01, 0x01, 0x01, 0x00, 0, 0, 0, 0, 2, 0, 1, 4, 3, 0xAA, 0xBB},
		"sysid_255_hb":   encodeFrameBytes(t, 255, 1, 1, validRoverHB),
		"compid_2_hb":    encodeFrameBytes(t, 2, 2, 1, validRoverHB),
		"sysid_255_ned":  encodeFrameBytes(t, 255, 1, 1, mavcodec.LocalPositionNED{X: 1}),
		"compid_191_ned": encodeFrameBytes(t, 2, 191, 1, mavcodec.LocalPositionNED{X: 1}),
	} {
		out := tracker.IngestDatagram(badDatagram, peerA, t0)
		if out.Accepted || !out.CountedError {
			t.Fatalf("bad datagram %q outcome = %+v, want CountedError=true", name, out)
		}
	}
	if sys0Out := tracker.IngestFrame(mavcodec.Frame{SystemID: 0, ComponentID: 1, Message: validRoverHB}, peerA, t0); !sys0Out.CountedError {
		t.Fatalf("sysid=0 HB frame outcome = %+v, want CountedError=true", sys0Out)
	}
	if nilFrameOut := tracker.IngestFrame(mavcodec.Frame{SystemID: 2, ComponentID: 1}, peerA, t0); !nilFrameOut.CountedError {
		t.Fatalf("nil Message frame outcome = %+v, want CountedError=true", nilFrameOut)
	}

	// Outbound actuation frames (#76 COMMAND_LONG) are ignored cleanly without counting errors.
	cmdOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 2, mavcodec.CommandLong{Command: 400}), peerA, t0)
	if cmdOut.Accepted || cmdOut.CountedError {
		t.Fatalf("COMMAND_LONG outcome = %+v, want ignored", cmdOut)
	}

	// 3. First valid 0xB1 HEARTBEAT admits sysid=2 without a spurious edge event.
	admitOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 3, validRoverHB), peerA, t0)
	if !admitOut.Accepted || admitOut.CountedError || len(admitOut.Events) != 0 || admitOut.UrgentSKGSync || admitOut.Telemetry != nil {
		t.Fatalf("admission HEARTBEAT outcome = %+v", admitOut)
	}
	if gotPeer, ok := tracker.PeerAddr(2); !ok || gotPeer != peerA {
		t.Fatalf("PeerAddr(2) = (%v, %v), want (%v, true)", gotPeer, ok, peerA)
	}

	// OQ1 Option A: Mismatched peerB while sysid=2 is live is rejected without redirecting PeerAddr or emitting events.
	spoofOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 4, validRoverHB), peerB, t0.Add(500*time.Millisecond))
	if spoofOut.Accepted || !spoofOut.CountedError || len(spoofOut.Events) != 0 || spoofOut.UrgentSKGSync {
		t.Fatalf("spoofed peerB HEARTBEAT while live outcome = %+v, want rejected", spoofOut)
	}
	if gotPeer, ok := tracker.PeerAddr(2); !ok || gotPeer != peerA {
		t.Fatalf("PeerAddr(2) after spoofed peerB = (%v, %v), want (%v, true)", gotPeer, ok, peerA)
	}

	// Steady HEARTBEAT from pinned peerA refreshes liveness without emitting an edge event.
	steadyOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 4, validRoverHB), peerA, t0.Add(time.Second))
	if !steadyOut.Accepted || len(steadyOut.Events) != 0 || steadyOut.UrgentSKGSync {
		t.Fatalf("steady HEARTBEAT outcome = %+v", steadyOut)
	}

	// 4. Heading fallback (#33 before #31) and quaternion precedence (#31 overrides #33.Hdg).
	gpUnknownHdg := mavcodec.GlobalPositionInt{
		Lat: 377749000,
		Lon: -1224194000,
		Alt: 12345,
		Hdg: 65535, // UINT16_MAX = unknown
	}
	gpOut1 := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 5, gpUnknownHdg), peerA, t0.Add(2*time.Second))
	if !gpOut1.Accepted || gpOut1.Telemetry == nil || !gpOut1.Telemetry.HasGlobalPos || gpOut1.Telemetry.HasLocalPos ||
		gpOut1.Telemetry.HasAttitude || gpOut1.Telemetry.HasHeading || gpOut1.Telemetry.QuatWXYZ != [4]float64{0, 0, 0, 0} || gpOut1.Telemetry.HdgDeg != 0 {
		t.Fatalf("GLOBAL_POSITION_INT(Hdg=65535) outcome = %+v, want HasHeading=false", gpOut1)
	}
	snapUnknownHdg, _ := tracker.Snapshot(2)
	if hdgTr := snapUnknownHdg.Triples()[17]; hdgTr.Confidence != 0 || hdgTr.Context != "" {
		t.Fatalf("GLOBAL_POSITION_INT(Hdg=65535) heading triple = %+v, want Confidence=0 Context=\"\"", hdgTr)
	}

	gpZeroNorthHdg := gpUnknownHdg
	gpZeroNorthHdg.Hdg = 0 // measured 0.00 deg North before any #31
	gpOutZero := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 5, gpZeroNorthHdg), peerA, t0.Add(2500*time.Millisecond))
	if !gpOutZero.Accepted || gpOutZero.Telemetry == nil || gpOutZero.Telemetry.HasAttitude ||
		!gpOutZero.Telemetry.HasHeading || gpOutZero.Telemetry.HdgDeg != 0 {
		t.Fatalf("GLOBAL_POSITION_INT(Hdg=0) outcome = %+v, want HasHeading=true HdgDeg=0", gpOutZero.Telemetry)
	}
	snapZeroHdg, _ := tracker.Snapshot(2)
	if hdgTr := snapZeroHdg.Triples()[17]; hdgTr.Confidence != 1.0 || hdgTr.Context != "measured" || hdgTr.Object != 0.0 {
		t.Fatalf("GLOBAL_POSITION_INT(Hdg=0) heading triple = %+v, want Confidence=1 Context=measured Object=0", hdgTr)
	}

	gpFallbackHdg := gpUnknownHdg
	gpFallbackHdg.Hdg = 27450 // 274.50 deg before any #31
	gpOut2 := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 6, gpFallbackHdg), peerA, t0.Add(3*time.Second))
	if !gpOut2.Accepted || gpOut2.Telemetry == nil || gpOut2.Telemetry.HasAttitude || gpOut2.Telemetry.HasLocalPos ||
		!gpOut2.Telemetry.HasHeading || gpOut2.Telemetry.QuatWXYZ != [4]float64{0, 0, 0, 0} || math.Abs(gpOut2.Telemetry.HdgDeg-274.5) > 1e-6 {
		t.Fatalf("GLOBAL_POSITION_INT(Hdg=27450) HdgDeg = %v, want 274.5 with HasAttitude=false HasHeading=true", gpOut2.Telemetry)
	}
	findTriple := func(triples []message.Triple, pred string) message.Triple {
		t.Helper()
		for _, tr := range triples {
			if tr.Predicate == pred {
				return tr
			}
		}
		t.Fatalf("predicate %q missing from merged triples", pred)
		return message.Triple{}
	}
	snap274, _ := tracker.Snapshot(2)
	skgMerged := graph.ReplaceBySource(nil, snap274.Triples())
	if len(skgMerged.Stale) != 0 {
		t.Fatalf("ReplaceBySource(274.5) stale = %+v, want empty", skgMerged.Stale)
	}
	if hdgTr := findTriple(skgMerged.Triples, uxsvocab.PredicateKinematicsHeadingDeg); hdgTr.Confidence != 1.0 || hdgTr.Context != "measured" || hdgTr.Object != 274.5 {
		t.Fatalf("ReplaceBySource(274.5) heading triple = %+v, want Object=274.5 Confidence=1 Context=measured", hdgTr)
	}

	// Subsequent #33 with unknown Hdg=65535 before any #31 MUST invalidate fallback heading on both lanes
	// AND carry obsNs so SemEngine graph.ReplaceBySource replaces the stored 274.5 deg triple!
	tLostHdg := t0.Add(3250 * time.Millisecond)
	gpOutLostHdg := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 6, gpUnknownHdg), peerA, tLostHdg)
	if !gpOutLostHdg.Accepted || gpOutLostHdg.Telemetry == nil || gpOutLostHdg.Telemetry.HasAttitude ||
		gpOutLostHdg.Telemetry.HasHeading || gpOutLostHdg.Telemetry.HdgDeg != 0 {
		t.Fatalf("GLOBAL_POSITION_INT(Hdg=65535 after 274.5) = %+v, want HasHeading=false HdgDeg=0", gpOutLostHdg.Telemetry)
	}
	snapLostHdg, _ := tracker.Snapshot(2)
	skgMerged = graph.ReplaceBySource(skgMerged.Triples, snapLostHdg.Triples())
	if len(skgMerged.Stale) != 0 {
		t.Fatalf("ReplaceBySource(unknown 65535 after 274.5) rejected invalidation as stale: %+v", skgMerged.Stale)
	}
	if hdgTr := findTriple(skgMerged.Triples, uxsvocab.PredicateKinematicsHeadingDeg); hdgTr.Confidence != 0 || hdgTr.Context != "" || hdgTr.Object != 0.0 || !hdgTr.Timestamp.Equal(tLostHdg) {
		t.Fatalf("ReplaceBySource(unknown 65535 after 274.5) heading triple = %+v, want Object=0 Confidence=0 Context=\"\" Timestamp=%v", hdgTr, tLostHdg)
	}

	// Health-only update while heading is unknown MUST keep heading unmeasured at tLostHdg (neither reverting to 274.5 nor advancing heading timestamp).
	tHealthWhileUnknown := t0.Add(3375 * time.Millisecond)
	hbSteadyWhileUnknown := makeVehicleHB(mavcodec.DomainRover, false, mavcodec.AuthorityMCUFailsafe, mavcodec.ModeDisarmed, mavcodec.LinkTierSITLLocal, uxsvocab.HealthFlags{})
	if hbWhileUnknownOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 6, hbSteadyWhileUnknown), peerA, tHealthWhileUnknown); !hbWhileUnknownOut.Accepted {
		t.Fatalf("HEARTBEAT while heading unknown outcome = %+v", hbWhileUnknownOut)
	}
	snapHealthWhileUnknown, _ := tracker.Snapshot(2)
	skgMerged = graph.ReplaceBySource(skgMerged.Triples, snapHealthWhileUnknown.Triples())
	if len(skgMerged.Stale) != 0 {
		t.Fatalf("ReplaceBySource(health-only while heading unknown) stale = %+v, want empty", skgMerged.Stale)
	}
	if hdgTr := findTriple(skgMerged.Triples, uxsvocab.PredicateKinematicsHeadingDeg); hdgTr.Confidence != 0 || hdgTr.Context != "" || hdgTr.Object != 0.0 || !hdgTr.Timestamp.Equal(tLostHdg) {
		t.Fatalf("ReplaceBySource(health-only while heading unknown) heading triple = %+v, want Confidence=0 Timestamp=%v", hdgTr, tLostHdg)
	}
	if armedTr := findTriple(skgMerged.Triples, uxsvocab.PredicateHealthArmed); !armedTr.Timestamp.Equal(tHealthWhileUnknown) {
		t.Fatalf("ReplaceBySource(health-only while heading unknown) armed timestamp = %v, want %v", armedTr.Timestamp, tHealthWhileUnknown)
	}

	// Measured 0.00 deg North restores HasHeading=true and Confidence=1 / Context="measured" in ReplaceBySource.
	tRestoredNorth := t0.Add(3500 * time.Millisecond)
	gpOutRestoredNorth := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 6, gpZeroNorthHdg), peerA, tRestoredNorth)
	if !gpOutRestoredNorth.Accepted || gpOutRestoredNorth.Telemetry == nil || gpOutRestoredNorth.Telemetry.HasAttitude ||
		!gpOutRestoredNorth.Telemetry.HasHeading || gpOutRestoredNorth.Telemetry.HdgDeg != 0 {
		t.Fatalf("GLOBAL_POSITION_INT(Hdg=0 after unknown) = %+v, want HasHeading=true HdgDeg=0", gpOutRestoredNorth.Telemetry)
	}
	snapRestoredNorth, _ := tracker.Snapshot(2)
	skgMerged = graph.ReplaceBySource(skgMerged.Triples, snapRestoredNorth.Triples())
	if len(skgMerged.Stale) != 0 {
		t.Fatalf("ReplaceBySource(Hdg=0 after unknown) stale = %+v, want empty", skgMerged.Stale)
	}
	if hdgTr := findTriple(skgMerged.Triples, uxsvocab.PredicateKinematicsHeadingDeg); hdgTr.Confidence != 1.0 || hdgTr.Context != "measured" || hdgTr.Object != 0.0 || !hdgTr.Timestamp.Equal(tRestoredNorth) {
		t.Fatalf("ReplaceBySource(Hdg=0 after unknown) heading triple = %+v, want Confidence=1 Context=measured Object=0 Timestamp=%v", hdgTr, tRestoredNorth)
	}

	// #31 arrives with 90 deg East yaw ([cos(45), 0, 0, sin(45)]).
	s45 := float32(math.Sqrt2 / 2)
	attEast := mavcodec.AttitudeQuaternion{Q1: s45, Q2: 0, Q3: 0, Q4: s45, Yawspeed: 0.25}
	attOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 7, attEast), peerA, t0.Add(4*time.Second))
	if !attOut.Accepted || attOut.Telemetry == nil || !attOut.Telemetry.HasAttitude || attOut.Telemetry.HasLocalPos ||
		math.Abs(attOut.Telemetry.HdgDeg-90.0) > 1e-4 || !attOut.Telemetry.HasGlobalPos {
		t.Fatalf("ATTITUDE_QUATERNION East outcome = %+v", attOut.Telemetry)
	}

	// Subsequent #33 with Hdg=18000 must NOT overwrite #31's 90.0 deg heading!
	gpSouthHdg := mavcodec.GlobalPositionInt{Lat: -900_000_000, Lon: 1_800_000_000, Alt: -500, Hdg: 18000}
	gpOut3 := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 8, gpSouthHdg), peerA, t0.Add(5*time.Second))
	if !gpOut3.Accepted || gpOut3.Telemetry == nil {
		t.Fatalf("GLOBAL_POSITION_INT boundary outcome = %+v", gpOut3)
	}
	if math.Abs(gpOut3.Telemetry.HdgDeg-90.0) > 1e-4 {
		t.Fatalf("HdgDeg overwritten by #33 after #31: got %v, want 90.0", gpOut3.Telemetry.HdgDeg)
	}
	if gpOut3.Telemetry.LatLonAlt != [3]float64{-90.0, 180.0, -0.5} {
		t.Fatalf("LatLonAlt = %v, want [-90, 180, -0.5]", gpOut3.Telemetry.LatLonAlt)
	}

	// #32 LOCAL_POSITION_NED fuses with existing attitude + global position.
	nedOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 9, mavcodec.LocalPositionNED{
		X: 12.5, Y: -4.25, Z: -0.5, Vx: 1.5, Vy: -0.5, Vz: 0.0,
	}), peerA, t0.Add(6*time.Second))
	if !nedOut.Accepted || nedOut.Telemetry == nil || !nedOut.Telemetry.HasLocalPos ||
		nedOut.Telemetry.NEDXYZ != [3]float64{12.5, -4.25, -0.5} ||
		nedOut.Telemetry.NEDVel != [3]float64{1.5, -0.5, 0} || !nedOut.Telemetry.HasGlobalPos {
		t.Fatalf("LOCAL_POSITION_NED fused frame = %+v", nedOut.Telemetry)
	}

	// 5. Out-of-bounds & non-finite sanitization rejects bad packets while preserving last-good state.
	snapBeforeBad, _ := tracker.Snapshot(2)
	nan32 := float32(math.NaN())
	inf32 := float32(math.Inf(1))
	badPayloads := []mavcodec.Message{
		mavcodec.AttitudeQuaternion{Q1: 0, Q2: 0, Q3: 0, Q4: 0},
		mavcodec.AttitudeQuaternion{Q1: 1e-4, Q2: 0, Q3: 0, Q4: 0},
		mavcodec.AttitudeQuaternion{Q1: nan32, Q2: 0, Q3: 0, Q4: 1},
		mavcodec.AttitudeQuaternion{Q1: 1, Rollspeed: inf32},
		mavcodec.LocalPositionNED{X: nan32},
		mavcodec.LocalPositionNED{Vz: -inf32},
		mavcodec.GlobalPositionInt{Lat: 900_000_001, Lon: 0, Hdg: 0},
		mavcodec.GlobalPositionInt{Lat: -900_000_001, Lon: 0, Hdg: 0},
		mavcodec.GlobalPositionInt{Lat: 0, Lon: 1_800_000_001, Hdg: 0},
		mavcodec.GlobalPositionInt{Lat: 0, Lon: -1_800_000_001, Hdg: 0},
		mavcodec.GlobalPositionInt{Lat: 0, Lon: 0, Hdg: 36000},
		mavcodec.GlobalPositionInt{Lat: 0, Lon: 0, Hdg: 65534},
	}
	evilPeer := netip.MustParseAddrPort("127.0.0.1:59999")
	for i, badMsg := range badPayloads {
		out := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, uint8(20+i), badMsg), evilPeer, t0.Add(7*time.Second))
		if out.Accepted || !out.CountedError || out.Telemetry != nil || len(out.Events) != 0 || out.UrgentSKGSync {
			t.Fatalf("bad payload %d (%T) outcome = %+v, want rejected", i, badMsg, out)
		}
	}
	snapAfterBad, _ := tracker.Snapshot(2)
	if snapAfterBad != snapBeforeBad {
		t.Fatalf("sanitization failure mutated snapshot:\nbefore=%+v\nafter =%+v", snapBeforeBad, snapAfterBad)
	}
	if gotPeer, _ := tracker.PeerAddr(2); gotPeer != peerA {
		t.Fatalf("rejected packet overwrote PeerAddr: got %v, want %v", gotPeer, peerA)
	}

	// 6. Lane 3 edge transitions: HEARTBEAT preemption, #1 SYS_STATUS failsafe increment, and event-only #253 STATUSTEXT.
	preemptHB := makeVehicleHB(
		mavcodec.DomainRover,
		true,
		mavcodec.AuthorityDirectCRSFOverride,
		mavcodec.ModeAutonomousVelocity,
		mavcodec.LinkTierMeshHighRate,
		uxsvocab.HealthFlags{TeleopDeadmanClamped: true},
	)
	hbEdge := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 40, preemptHB), peerA, t0.Add(8*time.Second))
	if !hbEdge.Accepted || !hbEdge.UrgentSKGSync || len(hbEdge.Events) != 1 {
		t.Fatalf("HEARTBEAT preemption outcome = %+v", hbEdge)
	}
	if hbEdge.Events[0].Kind != EventKindHeartbeatEdge || hbEdge.Events[0].PrevAuthority != uxsvocab.AuthorityMCUFailsafe ||
		hbEdge.Events[0].Authority != uxsvocab.AuthorityDirectCRSFOverride {
		t.Fatalf("HEARTBEAT edge event = %+v", hbEdge.Events[0])
	}
	wantReasons := []string{"armed_changed", "authority_changed", "companion_mode_changed", "link_tier_changed", "health_flags_changed"}
	if !slices.Equal(hbEdge.Events[0].Reasons, wantReasons) {
		t.Fatalf("HEARTBEAT edge reasons = %v, want %v", hbEdge.Events[0].Reasons, wantReasons)
	}

	// #1 SYS_STATUS: initial nonzero baseline (7) and steady sample (7) update snapshot without a false alert;
	// strict increment (7 -> 8) emits exactly one failsafe_increment edge; decrement (8 -> 6) updates without edge.
	sysBaseline := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 41, mavcodec.SysStatus{
		Load: 1200, ErrorsComm: 3, ErrorsCount1: 7, ErrorsCount2: 1850, ErrorsCount3: 900,
	}), peerA, t0.Add(9*time.Second))
	if !sysBaseline.Accepted || sysBaseline.UrgentSKGSync || len(sysBaseline.Events) != 0 {
		t.Fatalf("SYS_STATUS initial baseline(7) outcome = %+v, want no false failsafe alert", sysBaseline)
	}
	if snap, _ := tracker.Snapshot(2); snap.Failsafes != 7 || snap.LoopDtUs != 1200 {
		t.Fatalf("SYS_STATUS initial baseline(7) snapshot = %+v, want Failsafes=7 LoopDtUs=1200", snap)
	}

	sysSteady := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 42, mavcodec.SysStatus{
		Load: 1100, ErrorsComm: 4, ErrorsCount1: 7, ErrorsCount2: 1850, ErrorsCount3: 850,
	}), peerA, t0.Add(10*time.Second))
	if !sysSteady.Accepted || sysSteady.UrgentSKGSync || len(sysSteady.Events) != 0 {
		t.Fatalf("SYS_STATUS steady(7->7) outcome = %+v, want no edge event", sysSteady)
	}
	if snap, _ := tracker.Snapshot(2); snap.Failsafes != 7 || snap.LoopDtUs != 1100 {
		t.Fatalf("SYS_STATUS steady(7->7) snapshot = %+v, want Failsafes=7 LoopDtUs=1100", snap)
	}

	sysInc := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 43, mavcodec.SysStatus{
		Load: 1050, ErrorsComm: 5, ErrorsCount1: 8, ErrorsCount2: 1900, ErrorsCount3: 820,
	}), peerA, t0.Add(11*time.Second))
	if !sysInc.Accepted || !sysInc.UrgentSKGSync || len(sysInc.Events) != 1 || sysInc.Events[0].Kind != EventKindFailsafeIncrement || sysInc.Events[0].Failsafes != 8 {
		t.Fatalf("SYS_STATUS increment(7->8) outcome = %+v, want failsafe_increment(8)", sysInc)
	}
	if snap, _ := tracker.Snapshot(2); snap.Failsafes != 8 || snap.LoopDtUs != 1050 {
		t.Fatalf("SYS_STATUS increment(7->8) snapshot = %+v, want Failsafes=8 LoopDtUs=1050", snap)
	}

	sysDec := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 44, mavcodec.SysStatus{
		Load: 1000, ErrorsComm: 5, ErrorsCount1: 6, ErrorsCount2: 1900, ErrorsCount3: 800,
	}), peerA, t0.Add(12*time.Second))
	if !sysDec.Accepted || sysDec.UrgentSKGSync || len(sysDec.Events) != 0 {
		t.Fatalf("SYS_STATUS decrement(8->6) outcome = %+v, want no edge event", sysDec)
	}
	if snap, _ := tracker.Snapshot(2); snap.Failsafes != 6 || snap.LoopDtUs != 1000 {
		t.Fatalf("SYS_STATUS decrement(8->6) snapshot = %+v, want Failsafes=6 LoopDtUs=1000", snap)
	}

	// Steady-state #253 STATUSTEXT emits immediate Lane 3 statustext event WITHOUT triggering an urgent or periodic SKG rewrite (`OQ3 Option A`).
	stOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 45, mavcodec.StatusText{
		Severity: 2,
		Text:     "CRSF OVERRIDE ENGAGED",
		ID:       7,
		ChunkSeq: 0,
	}), peerA, t0.Add(13*time.Second))
	if !stOut.Accepted || stOut.UrgentSKGSync || len(stOut.Events) != 1 || stOut.Events[0].Kind != EventKindStatusText ||
		stOut.Events[0].StatusText == nil || stOut.Events[0].StatusText.Text != "CRSF OVERRIDE ENGAGED" {
		t.Fatalf("STATUSTEXT wire outcome = %+v, want UrgentSKGSync=false and statustext event", stOut)
	}
	stNulOut := tracker.IngestFrame(mavcodec.Frame{
		SystemID: 2, ComponentID: 1, Sequence: 46,
		Message: mavcodec.StatusText{Severity: 4, Text: "PADDED\x00\x00"},
	}, peerA, t0.Add(14*time.Second))
	if !stNulOut.Accepted || stNulOut.UrgentSKGSync || len(stNulOut.Events) != 1 || stNulOut.Events[0].StatusText.Text != "PADDED" {
		t.Fatalf("STATUSTEXT NUL-trim outcome = %+v", stNulOut)
	}

	// Verify #0 HEARTBEAT (t0+8s) kept heartbeat triple timestamps at t0+8s, #1 SYS_STATUS (t0+9..12s) advanced
	// SYS_STATUS counter/timing timestamps to t0+12s, #32 stayed at t0+6s, and steady-state #253 STATUSTEXT (t0+13..14s) left all untouched!
	finalSnap, ok := tracker.Snapshot(2)
	if !ok {
		t.Fatal("missing final sysid=2 snapshot")
	}
	finalTriples := finalSnap.Triples()
	byPred := make(map[string]message.Triple, len(finalTriples))
	for _, tr := range finalTriples {
		byPred[tr.Predicate] = tr
	}
	if !byPred[uxsvocab.PredicateHealthArmed].Timestamp.Equal(t0.Add(8 * time.Second)) {
		t.Fatalf("heartbeat triple timestamp = %v, want %v (from #0 HEARTBEAT at t0+8s)", byPred[uxsvocab.PredicateHealthArmed].Timestamp, t0.Add(8*time.Second))
	}
	if !byPred[uxsvocab.PredicateHealthFailsafes].Timestamp.Equal(t0.Add(12*time.Second)) ||
		!byPred[uxsvocab.PredicateHealthLoopDtUs].Timestamp.Equal(t0.Add(12*time.Second)) {
		t.Fatalf("SYS_STATUS triple timestamps = (%v, %v), want %v (untouched by steady-state STATUSTEXT)",
			byPred[uxsvocab.PredicateHealthFailsafes].Timestamp, byPred[uxsvocab.PredicateHealthLoopDtUs].Timestamp, t0.Add(12*time.Second))
	}
	if !byPred[uxsvocab.PredicateKinematicsNED].Timestamp.Equal(t0.Add(6*time.Second)) ||
		byPred[uxsvocab.PredicateKinematicsNED].Context != "measured" {
		t.Fatalf("NED triple after health/status updates = %+v, want Timestamp=%v Context=measured",
			byPred[uxsvocab.PredicateKinematicsNED], t0.Add(6*time.Second))
	}
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestTrackerLinkLossSilenceThresholdsAndRestoration(t *testing.T) {
	tracker, err := NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("NewTrackerWithTimeouts: %v", err)
	}
	peer := netip.MustParseAddrPort("127.0.0.1:45100")
	t0 := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)

	// Admit sysid=1 on mesh-high-rate (3s threshold) and sysid=2 on mesh-supervisory (10s threshold).
	hbHigh := makeVehicleHB(mavcodec.DomainQuadcopter, true, mavcodec.AuthorityCompanionTeleop, mavcodec.ModeTeleopVelocity, mavcodec.LinkTierMeshHighRate, uxsvocab.HealthFlags{ClockSyncLocked: true})
	hbSup := makeVehicleHB(mavcodec.DomainRover, false, mavcodec.AuthorityMCUFailsafe, mavcodec.ModeDisarmed, mavcodec.LinkTierMeshSupervisory, uxsvocab.HealthFlags{})
	tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 10, hbHigh), peer, t0)
	tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 20, hbSup), peer, t0)

	initBatch, err := tracker.BuildPeriodicBatch(nil)
	if err != nil || len(initBatch) != 2 {
		t.Fatalf("initial BuildPeriodicBatch = (%d, %v), want 2 items", len(initBatch), err)
	}
	confirmed := map[uint8]uint64{
		initBatch[0].SysID: initBatch[0].Rev,
		initBatch[1].SysID: initBatch[1].Rev,
	}

	// Rejected datagrams (GCS HB, bad sysid/compid, NaN #31, out-of-bounds #33, outbound #76) MUST NOT refresh lastPacketAt.
	tReject := t0.Add(2500 * time.Millisecond)
	_ = tracker.IngestDatagram(encodeFrameBytes(t, 255, 190, 1, mavcodec.Heartbeat{Type: 6, Autopilot: 8}), peer, tReject)
	_ = tracker.IngestDatagram(encodeFrameBytes(t, 1, 2, 11, hbHigh), peer, tReject)
	_ = tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 12, mavcodec.AttitudeQuaternion{Q1: float32(math.NaN())}), peer, tReject)
	_ = tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 13, mavcodec.GlobalPositionInt{Lat: 900_000_001}), peer, tReject)
	_ = tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 14, mavcodec.CommandLong{Command: 400}), peer, tReject)

	// Boundary 1: threshold - 1ns emits zero link_lost events.
	if evs := tracker.EvaluateSilence(t0.Add(3*time.Second - time.Nanosecond)); len(evs) != 0 {
		t.Fatalf("EvaluateSilence(3s - 1ns) = %+v, want empty", evs)
	}
	if tracker.IsLinkLost(1) || tracker.IsLinkLost(2) {
		t.Fatal("unexpected IsLinkLost before threshold")
	}

	// Boundary 2: exact 3s threshold marks sysid=1 lost once while keeping mesh-supervisory sysid=2 online
	// and freezing sysid=1's VehicleSnapshot.ObservedAt at t0 (`OQ2 Option A`).
	evs3s := tracker.EvaluateSilence(t0.Add(3 * time.Second))
	if len(evs3s) != 1 || evs3s[0].SysID != 1 || evs3s[0].Kind != EventKindLinkLost ||
		evs3s[0].Seq != 10 || evs3s[0].MsgID != mavcodec.MsgIDHeartbeat ||
		!slices.Equal(evs3s[0].Reasons, []string{"silence_timeout"}) ||
		evs3s[0].Timestamp != t0.Add(3*time.Second).UnixMilli() {
		t.Fatalf("EvaluateSilence(3s) = %+v, want single sysid=1 link_lost event", evs3s)
	}
	if !tracker.IsLinkLost(1) || tracker.IsLinkLost(2) {
		t.Fatalf("IsLinkLost at 3s = (sys1=%v, sys2=%v), want (true, false)", tracker.IsLinkLost(1), tracker.IsLinkLost(2))
	}
	if snap1, _ := tracker.Snapshot(1); !snap1.ObservedAt.Equal(t0) {
		t.Fatalf("link_lost mutated sysid=1 ObservedAt = %v, want frozen %v", snap1.ObservedAt, t0)
	}
	if silentBatch, bErr := tracker.BuildPeriodicBatch(confirmed); bErr != nil || len(silentBatch) != 0 {
		t.Fatalf("BuildPeriodicBatch while silent = (%+v, %v), want empty", silentBatch, bErr)
	}

	// Repeated silent ticks emit zero duplicate link_lost events for sysid=1, and sysid=2 stays online through 10s - 1ns.
	for _, ts := range []time.Time{
		t0.Add(4 * time.Second),
		t0.Add(9 * time.Second),
		t0.Add(10*time.Second - time.Nanosecond),
	} {
		if evs := tracker.EvaluateSilence(ts); len(evs) != 0 {
			t.Fatalf("EvaluateSilence(%v) = %+v, want zero events", ts.Sub(t0), evs)
		}
	}

	// Boundary 3: exact 10s supervisory threshold marks sysid=2 lost.
	evs10s := tracker.EvaluateSilence(t0.Add(10 * time.Second))
	if len(evs10s) != 1 || evs10s[0].SysID != 2 || evs10s[0].Kind != EventKindLinkLost || evs10s[0].LinkTier != uxsvocab.LinkTierMeshSupervisory {
		t.Fatalf("EvaluateSilence(10s) = %+v, want sysid=2 mesh-supervisory link_lost", evs10s)
	}

	// Rejected datagram while lost MUST NOT clear linkLost or emit link_restored.
	badWhileLost := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 15, mavcodec.AttitudeQuaternion{}), peer, t0.Add(10500*time.Millisecond))
	if badWhileLost.Accepted || !badWhileLost.CountedError || !tracker.IsLinkLost(1) {
		t.Fatalf("rejected #31 while lost cleared linkLost: out=%+v lost=%v", badWhileLost, tracker.IsLinkLost(1))
	}

	// Resumption A: sysid=1 resumes with #32 LOCAL_POSITION_NED -> emits link_restored + Telemetry + UrgentSKGSync.
	tResume := t0.Add(11 * time.Second)
	res1 := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 16, mavcodec.LocalPositionNED{X: 4, Y: -2, Z: -8}), peer, tResume)
	if !res1.Accepted || !res1.UrgentSKGSync || res1.Telemetry == nil || len(res1.Events) != 1 ||
		res1.Events[0].Kind != EventKindLinkRestored || tracker.IsLinkLost(1) {
		t.Fatalf("sysid=1 kinematic resume outcome = %+v (lost=%v)", res1, tracker.IsLinkLost(1))
	}
	if snap1, _ := tracker.Snapshot(1); !snap1.ObservedAt.Equal(tResume) {
		t.Fatalf("sysid=1 ObservedAt on resume = %v, want %v", snap1.ObservedAt, tResume)
	}

	// Resumption B: sysid=2 resumes with #0 HEARTBEAT that ALSO flips authority (MCUFailsafe -> DirectCRSFOverride)
	// and cuts LinkTier over from mesh-supervisory (10s) to mesh-high-rate (3s) -> emits [link_restored, heartbeat_edge]!
	hbCutover := makeVehicleHB(mavcodec.DomainRover, true, mavcodec.AuthorityDirectCRSFOverride, mavcodec.ModeHoldPosition, mavcodec.LinkTierMeshHighRate, uxsvocab.HealthFlags{TeleopDeadmanClamped: true})
	res2 := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 21, hbCutover), peer, tResume)
	if !res2.Accepted || !res2.UrgentSKGSync || len(res2.Events) != 2 ||
		res2.Events[0].Kind != EventKindLinkRestored || res2.Events[1].Kind != EventKindHeartbeatEdge ||
		res2.Events[1].PrevAuthority != uxsvocab.AuthorityMCUFailsafe || res2.Events[1].Authority != uxsvocab.AuthorityDirectCRSFOverride {
		t.Fatalf("sysid=2 concurrent HEARTBEAT resume outcome = %+v, want [link_restored, heartbeat_edge]", res2)
	}

	// Verify dynamic LinkTier cutover: sysid=2 now times out after 3s (tResume + 3s) instead of 10s!
	// Establish SYS_STATUS baseline on sysid=2 at tResume + 1s first so we can test concurrent failsafe_increment on resume.
	_ = tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 22, mavcodec.SysStatus{Load: 900, ErrorsCount1: 2}), peer, tResume.Add(time.Second))
	if evs := tracker.EvaluateSilence(tResume.Add(4*time.Second - time.Nanosecond)); len(evs) != 1 || evs[0].SysID != 1 {
		t.Fatalf("EvaluateSilence(tResume + 4s - 1ns) = %+v, want only sysid=1 lost while sysid=2 is 1ns short", evs)
	}
	if evs := tracker.EvaluateSilence(tResume.Add(4 * time.Second)); len(evs) != 1 || evs[0].SysID != 2 {
		t.Fatalf("EvaluateSilence(tResume + 4s) = %+v, want sysid=2 lost at new 3s mesh-high-rate threshold", evs)
	}

	// Resumption C: sysid=2 resumes with #1 SYS_STATUS incrementing failsafes (2 -> 3) -> [link_restored, failsafe_increment].
	tResumeFS := tResume.Add(5 * time.Second)
	resFS := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 23, mavcodec.SysStatus{Load: 950, ErrorsCount1: 3}), peer, tResumeFS)
	if !resFS.Accepted || !resFS.UrgentSKGSync || len(resFS.Events) != 2 ||
		resFS.Events[0].Kind != EventKindLinkRestored || resFS.Events[1].Kind != EventKindFailsafeIncrement {
		t.Fatalf("sysid=2 concurrent SYS_STATUS resume = %+v, want [link_restored, failsafe_increment]", resFS)
	}

	// Resumption D: sysid=1 (still lost from tResume + 4s - 1ns) resumes via #253 STATUSTEXT -> [link_restored, statustext] + UrgentSKGSync,
	// whereas a follow-up steady-state #253 STATUSTEXT refreshes lastPacketAt without UrgentSKGSync.
	tResumeST := tResume.Add(6 * time.Second)
	resST := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 17, mavcodec.StatusText{Severity: 5, Text: "REJOINED"}), peer, tResumeST)
	if !resST.Accepted || !resST.UrgentSKGSync || len(resST.Events) != 2 ||
		resST.Events[0].Kind != EventKindLinkRestored || resST.Events[1].Kind != EventKindStatusText {
		t.Fatalf("sysid=1 STATUSTEXT resume = %+v, want [link_restored, statustext] + UrgentSKGSync", resST)
	}
	steadyST := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 18, mavcodec.StatusText{Severity: 6, Text: "KEEPALIVE"}), peer, tResumeST.Add(2*time.Second))
	if !steadyST.Accepted || steadyST.UrgentSKGSync || len(steadyST.Events) != 1 || steadyST.Events[0].Kind != EventKindStatusText {
		t.Fatalf("sysid=1 steady STATUSTEXT = %+v, want event-only without UrgentSKGSync", steadyST)
	}
	if evs := tracker.EvaluateSilence(tResumeST.Add(4 * time.Second)); len(evs) != 1 || evs[0].SysID != 2 || tracker.IsLinkLost(1) {
		t.Fatalf("EvaluateSilence 2s after sysid=1 steady STATUSTEXT = %+v (lost1=%v), want only silent sysid=2 lost while sysid=1 stays fresh",
			evs, tracker.IsLinkLost(1))
	}
}

func hasMonotonicClock(ts time.Time) bool {
	return strings.Contains(ts.String(), " m=")
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestTrackerMonotonicSilenceAcrossWallClockSteps(t *testing.T) {
	tracker, err := NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("NewTrackerWithTimeouts: %v", err)
	}
	peer := netip.MustParseAddrPort("127.0.0.1:45101")
	hb := makeVehicleHB(mavcodec.DomainQuadcopter, true, mavcodec.AuthorityCompanionTeleop, mavcodec.ModeTeleopVelocity, mavcodec.LinkTierSITLLocal, uxsvocab.HealthFlags{})

	// Verify public IngestDatagram(time.Now()) preserves Go's monotonic clock reading while publishing UTC ObservedAt.
	liveTracker, err := NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("NewTrackerWithTimeouts(live): %v", err)
	}
	liveTracker.IngestDatagram(encodeFrameBytes(t, 9, 1, 1, hb), peer, time.Now())
	if sample, ok := liveTracker.lastPacketSample(9); !ok || !hasMonotonicClock(sample) {
		t.Fatalf("lastPacketSample(9) = (%v, ok=%v), want preserved monotonic clock reading", sample, ok)
	}
	if snap9, ok := liveTracker.Snapshot(9); !ok || snap9.ObservedAt.Location() != time.UTC {
		t.Fatalf("Snapshot(9).ObservedAt = %v, want UTC location", snap9.ObservedAt)
	}

	wall0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	mono0 := time.Now()
	hbFrame1 := mavcodec.Frame{SystemID: 1, ComponentID: 1, Sequence: 1, Message: hb}
	tracker.ingestFrameAt(hbFrame1, peer, wall0, mono0)

	// Direction 1: 1-minute BACKWARD wall-clock step right after packet arrival.
	// Silence MUST still fire at +3s monotonic (not delayed by 60s), while publishing the stepped UTC wall timestamp.
	wallBack := wall0.Add(-1 * time.Minute)
	if evs := tracker.evaluateSilenceAt(wallBack.Add(3*time.Second-time.Nanosecond), mono0.Add(3*time.Second-time.Nanosecond)); len(evs) != 0 {
		t.Fatalf("backward step before 3s monotonic = %+v, want empty", evs)
	}
	backLossWall := wallBack.Add(3 * time.Second)
	evsBack := tracker.evaluateSilenceAt(backLossWall, mono0.Add(3*time.Second))
	var sys1Lost *TransitionEvent
	for i := range evsBack {
		if evsBack[i].SysID == 1 {
			sys1Lost = &evsBack[i]
		}
	}
	if sys1Lost == nil || sys1Lost.Kind != EventKindLinkLost || sys1Lost.Timestamp != backLossWall.UnixMilli() {
		t.Fatalf("backward wall-step silence at +3s monotonic = %+v, want sysid=1 link_lost with stepped Timestamp=%d",
			evsBack, backLossWall.UnixMilli())
	}
	if snap1, _ := tracker.Snapshot(1); !snap1.ObservedAt.Equal(wall0) {
		t.Fatalf("backward wall-step link_lost mutated ObservedAt = %v, want %v", snap1.ObservedAt, wall0)
	}

	// Resuming packet while wall clock is still behind wall0 MUST update lastPacketAt monotonically and restore cleanly.
	resumeBackWall := wall0.Add(-55 * time.Second)
	resBack := tracker.ingestFrameAt(
		mavcodec.Frame{SystemID: 1, ComponentID: 1, Sequence: 2, Message: mavcodec.LocalPositionNED{X: 7}},
		peer,
		resumeBackWall,
		mono0.Add(4*time.Second),
	)
	if !resBack.Accepted || len(resBack.Events) != 1 || resBack.Events[0].Kind != EventKindLinkRestored ||
		resBack.Events[0].Timestamp != resumeBackWall.UnixMilli() || resBack.Telemetry == nil ||
		resBack.Telemetry.Timestamp != resumeBackWall.UnixMilli() {
		t.Fatalf("resume under backward wall clock = %+v, want link_restored + Telemetry at %d", resBack, resumeBackWall.UnixMilli())
	}
	if evs := tracker.evaluateSilenceAt(wall0.Add(-53*time.Second), mono0.Add(6900*time.Millisecond)); len(evs) != 0 && slices.ContainsFunc(evs, func(e TransitionEvent) bool { return e.SysID == 1 }) {
		t.Fatalf("unexpected early re-loss after backward-wall resume: %+v", evs)
	}
	if evs := tracker.evaluateSilenceAt(wall0.Add(-52*time.Second), mono0.Add(7*time.Second)); !slices.ContainsFunc(evs, func(e TransitionEvent) bool { return e.SysID == 1 && e.Kind == EventKindLinkLost }) {
		t.Fatalf("missing +3s monotonic re-loss after backward-wall resume: %+v", evs)
	}

	// Direction 2: 1-minute FORWARD wall-clock step 100ms after admission MUST NOT cause a false link_lost.
	hbFrame2 := mavcodec.Frame{SystemID: 2, ComponentID: 1, Sequence: 1, Message: hb}
	tracker.ingestFrameAt(hbFrame2, peer, wall0, mono0)
	wallFwdJump := wall0.Add(time.Minute)
	if evs := tracker.evaluateSilenceAt(wallFwdJump, mono0.Add(100*time.Millisecond)); slices.ContainsFunc(evs, func(e TransitionEvent) bool { return e.SysID == 2 }) || tracker.IsLinkLost(2) {
		t.Fatalf("forward +1m wall step at +100ms monotonic caused false link_lost: evs=%+v lost=%v", evs, tracker.IsLinkLost(2))
	}
	if evs := tracker.evaluateSilenceAt(wallFwdJump.Add(2900*time.Millisecond-time.Nanosecond), mono0.Add(3*time.Second-time.Nanosecond)); slices.ContainsFunc(evs, func(e TransitionEvent) bool { return e.SysID == 2 }) {
		t.Fatalf("forward wall step fired before +3s monotonic: %+v", evs)
	}
	fwdLossWall := wallFwdJump.Add(2900 * time.Millisecond)
	evsFwd := tracker.evaluateSilenceAt(fwdLossWall, mono0.Add(3*time.Second))
	if !slices.ContainsFunc(evsFwd, func(e TransitionEvent) bool {
		return e.SysID == 2 && e.Kind == EventKindLinkLost && e.Timestamp == fwdLossWall.UnixMilli()
	}) {
		t.Fatalf("forward wall step at +3s monotonic = %+v, want sysid=2 link_lost with Timestamp=%d", evsFwd, fwdLossWall.UnixMilli())
	}
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestComponentSerializesSilenceLossBeforeConcurrentResumption(t *testing.T) {
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.Start()
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded NATS server not ready")
	}

	pubNC, err := natsclient.NewClient(srv.ClientURL(), natsclient.WithMaxReconnects(0))
	if err != nil {
		t.Fatalf("NewClient(pub): %v", err)
	}
	if err := pubNC.Connect(context.Background()); err != nil {
		t.Fatalf("Connect(pub): %v", err)
	}
	t.Cleanup(func() { _ = pubNC.Close(context.Background()) })

	subNC, err := natsclient.NewClient(srv.ClientURL(), natsclient.WithMaxReconnects(0))
	if err != nil {
		t.Fatalf("NewClient(sub): %v", err)
	}
	if err := subNC.Connect(context.Background()); err != nil {
		t.Fatalf("Connect(sub): %v", err)
	}
	t.Cleanup(func() { _ = subNC.Close(context.Background()) })

	sub, err := subNC.GetConnection().SubscribeSync("bbops.telemetry.>")
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	if err := subNC.GetConnection().Flush(); err != nil {
		t.Fatalf("Flush subscriber: %v", err)
	}

	udpPort := allocateLoopbackUDPPort(t)
	rawCfg, err := json.Marshal(map[string]any{
		"skg_hz":                 5,
		"link_timeout_ms":        220,
		"supervisory_timeout_ms": 1000,
		"ports": map[string]any{
			"inputs": []map[string]any{
				{
					"name":     "mavlink_udp",
					"external": true,
					"config": map[string]any{
						"kind":     "network",
						"protocol": "udp",
						"host":     "127.0.0.1",
						"port":     udpPort,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal rawCfg: %v", err)
	}

	comp, err := NewComponent(rawCfg, component.Dependencies{
		NATSClient: pubNC,
		Platform:   types.PlatformMeta{Org: "boxbotix", Platform: "bbops"},
	})
	if err != nil {
		t.Fatalf("NewComponent: %v", err)
	}

	// Block JetStream acknowledgements on the SKG Publisher worker for the entire contention test
	// to prove neither readLoop nor tickLoop blocks on JetStream acks.
	releaseJetStream := make(chan struct{})
	comp.publisher.publishBatch = func(ctx context.Context, _ string, _ [][]byte) error {
		select {
		case <-releaseJetStream:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	evalPaused := make(chan struct{})
	readerAtBoundary := make(chan struct{})
	var evalOnce sync.Once
	var readerOnce sync.Once

	comp.afterSilenceEval = func(ev TransitionEvent) {
		if ev.SysID == 1 && ev.Kind == EventKindLinkLost {
			evalOnce.Do(func() {
				close(evalPaused)
				select {
				case <-readerAtBoundary:
				case <-time.After(3 * time.Second):
				}
				// Give readLoop a window where, without per-sysid serialization, it would
				// complete IngestFrame + publishCoreJSON([link_restored, telemetry]) before link_lost.
				time.Sleep(15 * time.Millisecond)
			})
		}
	}
	comp.beforeIngestFrame = func(frame mavcodec.Frame) {
		if frame.SystemID == 1 && frame.Message != nil && frame.Message.MessageID() == mavcodec.MsgIDLocalPositionNED {
			readerOnce.Do(func() {
				close(readerAtBoundary)
			})
		}
	}

	if err := comp.Initialize(); err != nil {
		t.Fatalf("comp.Initialize: %v", err)
	}
	startCtx, startCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer startCancel()
	if err := comp.Start(startCtx); err != nil {
		t.Fatalf("comp.Start: %v", err)
	}
	defer func() {
		close(releaseJetStream)
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if stopErr := comp.Stop(stopCtx); stopErr != nil {
			t.Errorf("comp.Stop: %v", stopErr)
		}
	}()

	udpClient, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP client: %v", err)
	}
	defer func() { _ = udpClient.Close() }()
	dest := net.UDPAddrFromAddrPort(comp.BoundAddr())

	hb := makeVehicleHB(mavcodec.DomainQuadcopter, true, mavcodec.AuthorityCompanionTeleop, mavcodec.ModeTeleopVelocity, mavcodec.LinkTierSITLLocal, uxsvocab.HealthFlags{})
	if _, err := udpClient.WriteToUDP(encodeFrameBytes(t, 1, 1, 1, hb), dest); err != nil {
		t.Fatalf("WriteToUDP(HB): %v", err)
	}

	// Verify Component's live readLoop preserved Go's monotonic clock reading on lastPacketAt.
	admitDeadline := time.Now().Add(2 * time.Second)
	for {
		if sample, ok := comp.tracker.lastPacketSample(1); ok {
			if !hasMonotonicClock(sample) {
				t.Fatalf("Component readLoop stripped monotonic clock reading: %v", sample)
			}
			break
		}
		if time.Now().After(admitDeadline) {
			t.Fatal("timed out waiting for sysid=1 admission")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Wait until tickLoop marks sysid=1 link-lost and pauses right before publishing link_lost.
	select {
	case <-evalPaused:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for tickLoop silence evaluation")
	}

	// Inject resuming #32 LOCAL_POSITION_NED while tickLoop is paused inside afterSilenceEval.
	if _, err := udpClient.WriteToUDP(encodeFrameBytes(t, 1, 1, 2, mavcodec.LocalPositionNED{X: 42, Y: -3, Z: -10}), dest); err != nil {
		t.Fatalf("WriteToUDP(LocalPositionNED): %v", err)
	}

	// Collect wire order while JetStream publishBatch remains blocked!
	var wireOrder []string
	for len(wireOrder) < 3 {
		msg, nextErr := sub.NextMsg(3 * time.Second)
		if nextErr != nil {
			t.Fatalf("NextMsg(%d): %v (wireOrder so far: %v)", len(wireOrder), nextErr, wireOrder)
		}
		switch msg.Subject {
		case "bbops.telemetry.events.1":
			var ev TransitionEvent
			if uErr := json.Unmarshal(msg.Data, &ev); uErr != nil {
				t.Fatalf("unmarshal TransitionEvent: %v", uErr)
			}
			wireOrder = append(wireOrder, ev.Kind)
		case "bbops.telemetry.1":
			var tf TelemetryFrame
			if uErr := json.Unmarshal(msg.Data, &tf); uErr != nil {
				t.Fatalf("unmarshal TelemetryFrame: %v", uErr)
			}
			if !tf.HasLocalPos || tf.NEDXYZ[0] != 42 {
				t.Fatalf("TelemetryFrame = %+v, want NEDXYZ[0]=42", tf)
			}
			wireOrder = append(wireOrder, "telemetry")
		default:
			t.Fatalf("unexpected NATS subject %q", msg.Subject)
		}
	}
	wantOrder := []string{EventKindLinkLost, EventKindLinkRestored, "telemetry"}
	if !slices.Equal(wireOrder, wantOrder) {
		t.Fatalf("wire order = %v, want %v", wireOrder, wantOrder)
	}
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestPublisherBackpressureAndUrgentPriority(t *testing.T) {
	type recordedCall struct {
		subject string
		snaps   []uxsvocab.VehicleSnapshot
	}

	if err := uxsvocab.Register(); err != nil {
		t.Fatalf("uxsvocab.Register: %v", err)
	}
	reg := payloadregistry.New()
	if err := uxsvocab.RegisterPayloads(reg); err != nil {
		t.Fatalf("uxsvocab.RegisterPayloads: %v", err)
	}
	decoder := message.NewDecoder(reg)

	decodeBatch := func(t *testing.T, msgs [][]byte) []uxsvocab.VehicleSnapshot {
		t.Helper()
		out := make([]uxsvocab.VehicleSnapshot, 0, len(msgs))
		for _, raw := range msgs {
			bm, err := decoder.Decode(raw)
			if err != nil {
				t.Fatalf("decoder.Decode: %v", err)
			}
			snap, ok := bm.Payload().(*uxsvocab.VehicleSnapshot)
			if !ok {
				t.Fatalf("payload = %T, want *uxsvocab.VehicleSnapshot", bm.Payload())
			}
			out = append(out, *snap)
		}
		return out
	}

	tracker, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	peer := netip.MustParseAddrPort("127.0.0.1:46001")
	t0 := time.Date(2026, 11, 24, 2, 0, 0, 0, time.UTC)

	// Periodic notification with zero admitted vehicles is a clean no-op.
	emptyTracker, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker empty: %v", err)
	}
	if emptyItems, bErr := emptyTracker.BuildPeriodicBatch(nil); bErr != nil || len(emptyItems) != 0 {
		t.Fatalf("empty BuildPeriodicBatch = (%+v, %v), want empty", emptyItems, bErr)
	}

	// Admit sysid=1 (Quadcopter) and sysid=2 (Rover).
	hb1 := makeVehicleHB(mavcodec.DomainQuadcopter, false, mavcodec.AuthorityMCUFailsafe, mavcodec.ModeDisarmed, mavcodec.LinkTierSITLLocal, uxsvocab.HealthFlags{})
	hb2 := makeVehicleHB(mavcodec.DomainRover, false, mavcodec.AuthorityMCUFailsafe, mavcodec.ModeDisarmed, mavcodec.LinkTierSITLLocal, uxsvocab.HealthFlags{})
	tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: 1, Sequence: 1, Message: hb1}, peer, t0)
	tracker.IngestFrame(mavcodec.Frame{SystemID: 2, ComponentID: 1, Sequence: 1, Message: hb2}, peer, t0)
	if skippedAll, bErr := tracker.BuildPeriodicBatch(map[uint8]uint64{1: 1, 2: 1}); bErr != nil || len(skippedAll) != 0 {
		t.Fatalf("BuildPeriodicBatch with all revisions confirmed = (%+v, %v), want empty pre-marshal skip", skippedAll, bErr)
	}
	if only2, bErr := tracker.BuildPeriodicBatch(map[uint8]uint64{1: 1}); bErr != nil || len(only2) != 1 || only2[0].SysID != 2 {
		t.Fatalf("BuildPeriodicBatch with sysid=1 confirmed = (%+v, %v), want only sysid=2", only2, bErr)
	}

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var calls []recordedCall
	var errCount atomic.Int32

	pub := NewPublisher(
		DefaultEntitySubject,
		func(_ context.Context, subject string, msgs [][]byte) error {
			snaps := decodeBatch(t, msgs)
			mu.Lock()
			callIdx := len(calls)
			calls = append(calls, recordedCall{subject: subject, snaps: snaps})
			mu.Unlock()
			if callIdx == 0 {
				close(firstEntered)
				<-releaseFirst
			}
			return nil
		},
		tracker.BuildUrgentBatch,
		tracker.BuildPeriodicBatch,
		func() { errCount.Add(1) },
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pub.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})

	// Trigger initial periodic flush (#0), which stalls inside publishBatch.
	pub.NotifyPeriodic()

	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first batch to enter publishBatch")
	}

	// While publishBatch is stalled, advance state 5 times (LoopDtUs = 100..500) and call NotifyPeriodic
	// without blocking or building stale intermediate snapshots on the caller goroutine!
	notifyStart := time.Now()
	for step := 1; step <= 5; step++ {
		tracker.IngestFrame(mavcodec.Frame{
			SystemID:    1,
			ComponentID: 1,
			Sequence:    uint8(10 + step),
			Message:     mavcodec.SysStatus{Load: uint16(step * 100)},
		}, peer, t0.Add(time.Duration(step)*time.Second))
		tracker.IngestFrame(mavcodec.Frame{
			SystemID:    2,
			ComponentID: 1,
			Sequence:    uint8(10 + step),
			Message:     mavcodec.SysStatus{Load: uint16(step * 100)},
		}, peer, t0.Add(time.Duration(step)*time.Second))
		pub.NotifyPeriodic()
	}
	if elapsed := time.Since(notifyStart); elapsed > 100*time.Millisecond {
		t.Fatalf("NotifyPeriodic blocked under stalled publisher (%v)", elapsed)
	}

	// Trigger a Lane 3 urgent edge ONLY on sysid=1 (Armed=true, Authority=DirectCRSFOverride, LoopDtUs stays 500)
	// while periodicPending is already set.
	edgeHB := makeVehicleHB(mavcodec.DomainQuadcopter, true, mavcodec.AuthorityDirectCRSFOverride, mavcodec.ModeHoldPosition, mavcodec.LinkTierMeshHighRate, uxsvocab.HealthFlags{TeleopDeadmanClamped: true})
	tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: 1, Sequence: 99, Message: edgeHB}, peer, t0.Add(10*time.Second))
	pub.NotifyUrgent(1)

	// Release stalled call #0 and wait for urgent flush (#1) + coalesced build-at-flush periodic batch (#2).
	close(releaseFirst)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 3 && !pub.hasPending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(25 * time.Millisecond)

	mu.Lock()
	gotCalls := slices.Clone(calls)
	mu.Unlock()

	if errCount.Load() != 0 {
		t.Fatalf("unexpected publisher error count = %d", errCount.Load())
	}
	if len(gotCalls) != 3 {
		t.Fatalf("recorded publishBatch calls = %d, want exactly 3 (initial + preempting urgent + 1 coalesced periodic)", len(gotCalls))
	}

	// Call 1 (immediately after stalled call 0) MUST be the preempting urgent flush for sysid=1!
	if len(gotCalls[1].snaps) != 1 || gotCalls[1].snaps[0].SysID != 1 || !gotCalls[1].snaps[0].Armed ||
		gotCalls[1].snaps[0].Authority != uxsvocab.AuthorityDirectCRSFOverride {
		t.Fatalf("call[1] = %+v, want urgent sysid=1 %s snapshot ahead of periodic flush", gotCalls[1].snaps, uxsvocab.AuthorityDirectCRSFOverride)
	}

	// Call 2 (coalesced build-at-flush periodic batch) MUST skip already-confirmed sysid=1 (rev 7 <= 7)
	// and publish ONLY sysid=2's newest live state (LoopDtUs = 500)!
	if len(gotCalls[2].snaps) != 1 || gotCalls[2].snaps[0].SysID != 2 || gotCalls[2].snaps[0].LoopDtUs != 500 {
		t.Fatalf("call[2] = %+v, want single build-at-flush sysid=2 snapshot with LoopDtUs=500", gotCalls[2].snaps)
	}

	// Subsequent NotifyPeriodic ticks while sessions are silent / unchanged MUST NOT re-upsert!
	tracker.EvaluateSilence(t0.Add(20 * time.Second))
	pub.NotifyPeriodic()
	pub.NotifyPeriodic()
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && pub.hasPending() {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(25 * time.Millisecond)
	mu.Lock()
	afterSilentCalls := len(calls)
	mu.Unlock()
	if afterSilentCalls != 3 {
		t.Fatalf("publishBatch calls after silent NotifyPeriodic = %d, want 3 (zero redundant re-upserts)", afterSilentCalls)
	}

	// Verify buildUrgent, buildPeriodic, and publishBatch errors invoke onError without panicking.
	var failCount atomic.Int32
	var buildPeriodicAttempt atomic.Int32
	failPub := NewPublisher(
		DefaultEntitySubject,
		func(_ context.Context, _ string, _ [][]byte) error {
			return errors.New("jetstream publish timeout")
		},
		func(_ []uint8) ([]SnapshotBatchItem, error) {
			return nil, errors.New("build urgent failure")
		},
		func(confirmed map[uint8]uint64) ([]SnapshotBatchItem, error) {
			if buildPeriodicAttempt.Add(1) == 1 {
				return nil, errors.New("build periodic failure")
			}
			return tracker.BuildPeriodicBatch(confirmed)
		},
		func() { failCount.Add(1) },
	)
	failPub.NotifyUrgent(1)
	failPub.NotifyPeriodic()
	failCtx, failCancel := context.WithCancel(context.Background())
	failDone := make(chan struct{})
	go func() {
		defer close(failDone)
		failPub.Run(failCtx)
	}()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && failCount.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	// Trigger a second periodic flush that builds items and hits the publishBatch error path.
	failPub.NotifyPeriodic()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && failCount.Load() < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	failCancel()
	<-failDone
	if got := failCount.Load(); got != 3 {
		t.Fatalf("onError count = %d, want 3 (urgent build + periodic build + periodic publish)", got)
	}

	// Verify partial urgent batch failure (`PublishBatchToStream` acknowledging item 0 and failing on item 1):
	// 1. Acknowledged sysid=1 (rev=2) MUST NOT regress to [new, old] because stale rev=1 snapshots are never queued.
	// 2. Unconfirmed revisions remain > lastConfirmedRev so the next NotifyPeriodic recovers sysid=2 (rev=2).
	partialTracker, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker partial: %v", err)
	}
	partialTracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: 1, Sequence: 1, Message: hb1}, peer, t0)
	partialTracker.IngestFrame(mavcodec.Frame{SystemID: 2, ComponentID: 1, Sequence: 1, Message: hb2}, peer, t0)

	edgeHB2 := makeVehicleHB(mavcodec.DomainRover, true, mavcodec.AuthorityDirectCRSFOverride, mavcodec.ModeHoldPosition, mavcodec.LinkTierMeshHighRate, uxsvocab.HealthFlags{TeleopDeadmanClamped: true})
	partialTracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: 1, Sequence: 2, Message: edgeHB}, peer, t0.Add(time.Second))
	partialTracker.IngestFrame(mavcodec.Frame{SystemID: 2, ComponentID: 1, Sequence: 2, Message: edgeHB2}, peer, t0.Add(time.Second))

	var partialMu sync.Mutex
	deliveredArmedBySysID := map[uint8][]bool{}
	var partialErrors atomic.Int32
	var batchAttempt atomic.Int32
	partialFailed := make(chan struct{})

	partialPub := NewPublisher(
		DefaultEntitySubject,
		func(_ context.Context, _ string, msgs [][]byte) error {
			snaps := decodeBatch(t, msgs)
			attempt := batchAttempt.Add(1)
			partialMu.Lock()
			if attempt == 1 {
				// Simulate SemEngine PublishBatchToStream acknowledging sysid=1 (msgs[0])
				// before returning an error on sysid=2 (msgs[1]).
				deliveredArmedBySysID[snaps[0].SysID] = append(deliveredArmedBySysID[snaps[0].SysID], snaps[0].Armed)
				partialMu.Unlock()
				close(partialFailed)
				return errors.New("simulated JetStream ack failure on item 1")
			}
			for _, s := range snaps {
				deliveredArmedBySysID[s.SysID] = append(deliveredArmedBySysID[s.SysID], s.Armed)
			}
			partialMu.Unlock()
			return nil
		},
		partialTracker.BuildUrgentBatch,
		partialTracker.BuildPeriodicBatch,
		func() { partialErrors.Add(1) },
	)

	partialPub.NotifyUrgent(1)
	partialPub.NotifyUrgent(2)

	partialCtx, partialCancel := context.WithCancel(context.Background())
	partialDone := make(chan struct{})
	go func() {
		defer close(partialDone)
		partialPub.Run(partialCtx)
	}()

	select {
	case <-partialFailed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for simulated partial urgent batch failure")
	}

	// Trigger NotifyPeriodic to verify unconfirmed rev=2 recovers sysid=2 without regressing sysid=1.
	partialPub.NotifyPeriodic()

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		partialMu.Lock()
		recovered := len(deliveredArmedBySysID[2]) >= 1
		partialMu.Unlock()
		if recovered && !partialPub.hasPending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	partialCancel()
	<-partialDone

	if partialErrors.Load() != 1 {
		t.Fatalf("partialErrors = %d, want 1", partialErrors.Load())
	}
	partialMu.Lock()
	sys1History := slices.Clone(deliveredArmedBySysID[1])
	sys2History := slices.Clone(deliveredArmedBySysID[2])
	partialMu.Unlock()

	if len(sys1History) == 0 || slices.Contains(sys1History, false) {
		t.Fatalf("sysid=1 publications after partial ack = %v, want only Armed=true (no [new old] regression)", sys1History)
	}
	if !slices.Equal(sys2History, []bool{true}) {
		t.Fatalf("unacknowledged sysid=2 recovery publications = %v, want [true] from build-at-flush retry", sys2History)
	}
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestTrackerRestartClearsRetainedGraphPoseDomains(t *testing.T) {
	peer := netip.MustParseAddrPort("127.0.0.1:47001")
	findTriple := func(triples []message.Triple, pred string) message.Triple {
		t.Helper()
		for _, tr := range triples {
			if tr.Predicate == pred {
				return tr
			}
		}
		t.Fatalf("predicate %q missing from merged triples", pred)
		return message.Triple{}
	}

	// Session 1 (12:00:00Z): vehicle measures 90° East yaw (#31), local NED (#32), and global fix (#33).
	tracker1, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker(1): %v", err)
	}
	t1 := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	hb := makeVehicleHB(
		mavcodec.DomainRover,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierMeshHighRate,
		uxsvocab.HealthFlags{ClockSyncLocked: true, CRSFLinkHealthy: true, CompanionTrunkHealthy: true},
	)
	tracker1.IngestDatagram(encodeFrameBytes(t, 1, 1, 1, hb), peer, t1)
	s45 := float32(math.Sqrt2 / 2)
	tracker1.IngestDatagram(encodeFrameBytes(t, 1, 1, 2, mavcodec.AttitudeQuaternion{
		Q1: s45, Q2: 0, Q3: 0, Q4: s45, Yawspeed: 0.1,
	}), peer, t1.Add(100*time.Millisecond))
	tracker1.IngestDatagram(encodeFrameBytes(t, 1, 1, 3, mavcodec.LocalPositionNED{
		X: 12.5, Y: -4.25, Z: -2.0, Vx: 1.2, Vy: -0.3, Vz: 0.0,
	}), peer, t1.Add(200*time.Millisecond))
	tracker1.IngestDatagram(encodeFrameBytes(t, 1, 1, 4, mavcodec.GlobalPositionInt{
		Lat: 377749000, Lon: -1224194000, Alt: 25000, Hdg: 9000,
	}), peer, t1.Add(300*time.Millisecond))

	snap1, ok := tracker1.Snapshot(1)
	if !ok {
		t.Fatal("missing session 1 snapshot")
	}
	retained := graph.ReplaceBySource(nil, snap1.Triples())
	if len(retained.Stale) != 0 {
		t.Fatalf("session 1 ReplaceBySource stale = %+v, want empty", retained.Stale)
	}
	if qTr := findTriple(retained.Triples, uxsvocab.PredicateAttitudeQuaternion); qTr.Confidence != 1.0 || qTr.Context != "measured" {
		t.Fatalf("session 1 quaternion triple = %+v, want measured", qTr)
	}

	// Session 2 restarts 1 hour later (13:00:00Z) on the same vehicle identity.
	tracker2, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker(2): %v", err)
	}
	t2 := t1.Add(time.Hour)
	tracker2.IngestDatagram(encodeFrameBytes(t, 1, 1, 1, hb), peer, t2)

	// Health-only #1 SYS_STATUS before any pose frame MUST preserve t2 on all 4 unmeasured pose domains.
	t2Health1 := t2.Add(100 * time.Millisecond)
	tracker2.IngestDatagram(encodeFrameBytes(t, 1, 1, 2, mavcodec.SysStatus{
		Load: 950, ErrorsComm: 1, ErrorsCount1: 0, ErrorsCount2: 1100, ErrorsCount3: 350,
	}), peer, t2Health1)
	snapAdmitHealth, _ := tracker2.Snapshot(1)
	for _, pred := range []string{
		uxsvocab.PredicateAttitudeQuaternion,
		uxsvocab.PredicateAttitudeRates,
		uxsvocab.PredicateKinematicsNED,
		uxsvocab.PredicateKinematicsVx,
		uxsvocab.PredicateKinematicsVy,
		uxsvocab.PredicateKinematicsVz,
		uxsvocab.PredicateKinematicsHeadingDeg,
	} {
		tr := findTriple(snapAdmitHealth.Triples(), pred)
		if tr.Confidence != 0.0 || tr.Context != "" || !tr.Timestamp.Equal(t2) {
			t.Fatalf("post-admission health-only %s = %+v, want Confidence=0 Timestamp=%v", pred, tr, t2)
		}
	}

	// Global fix with unknown heading (65535) arrives before any #31/#32 frame, then merges directly
	// onto Session 1's retained graph (exercising the case where Session 2 had not flushed yet).
	t2Global := t2.Add(250 * time.Millisecond)
	gpOut := tracker2.IngestDatagram(encodeFrameBytes(t, 1, 1, 3, mavcodec.GlobalPositionInt{
		Lat: 302672000, Lon: -977431000, Alt: 150000, Hdg: 65535,
	}), peer, t2Global)
	if !gpOut.Accepted || gpOut.Telemetry == nil || gpOut.Telemetry.HasAttitude || gpOut.Telemetry.HasHeading || gpOut.Telemetry.HasLocalPos || !gpOut.Telemetry.HasGlobalPos {
		t.Fatalf("session 2 direct telemetry after #33(Hdg=65535) = %+v, want only HasGlobalPos=true", gpOut.Telemetry)
	}

	snap2Global, _ := tracker2.Snapshot(1)
	retained = graph.ReplaceBySource(retained.Triples, snap2Global.Triples())
	if len(retained.Stale) != 0 {
		t.Fatalf("session 2 ReplaceBySource rejected unmeasured pose reset as stale: %+v", retained.Stale)
	}
	if qTr := findTriple(retained.Triples, uxsvocab.PredicateAttitudeQuaternion); qTr.Confidence != 0.0 || qTr.Context != "" || qTr.Object != ([4]float64{0, 0, 0, 0}) || !qTr.Timestamp.Equal(t2) {
		t.Fatalf("retained quaternion after restart = %+v, want [0,0,0,0] Confidence=0 Timestamp=%v", qTr, t2)
	}
	if nedTr := findTriple(retained.Triples, uxsvocab.PredicateKinematicsNED); nedTr.Confidence != 0.0 || nedTr.Context != "" || nedTr.Object != ([3]float64{0, 0, 0}) || !nedTr.Timestamp.Equal(t2) {
		t.Fatalf("retained NED after restart = %+v, want [0,0,0] Confidence=0 Timestamp=%v", nedTr, t2)
	}
	if hdgTr := findTriple(retained.Triples, uxsvocab.PredicateKinematicsHeadingDeg); hdgTr.Confidence != 0.0 || hdgTr.Context != "" || hdgTr.Object != 0.0 || !hdgTr.Timestamp.Equal(t2Global) {
		t.Fatalf("retained heading after restart = %+v, want 0 Confidence=0 Timestamp=%v", hdgTr, t2Global)
	}
	if latTr := findTriple(retained.Triples, "geo.location.latitude"); latTr.Confidence != 1.0 || latTr.Context != "measured" || math.Abs(latTr.Object.(float64)-30.2672) > 1e-6 || !latTr.Timestamp.Equal(t2Global) {
		t.Fatalf("retained latitude after restart = %+v, want 30.2672 Confidence=1 Timestamp=%v", latTr, t2Global)
	}

	// Another health-only update (#0 HEARTBEAT) keeps attitude/local-pos at t2 and global/heading at t2Global.
	t2Health2 := t2.Add(500 * time.Millisecond)
	tracker2.IngestDatagram(encodeFrameBytes(t, 1, 1, 4, hb), peer, t2Health2)
	snap2Health2, _ := tracker2.Snapshot(1)
	retained = graph.ReplaceBySource(retained.Triples, snap2Health2.Triples())
	if len(retained.Stale) != 0 {
		t.Fatalf("health-only ReplaceBySource stale = %+v", retained.Stale)
	}
	if qTr := findTriple(retained.Triples, uxsvocab.PredicateAttitudeQuaternion); qTr.Confidence != 0.0 || !qTr.Timestamp.Equal(t2) {
		t.Fatalf("quaternion after health-only update = %+v, want Confidence=0 Timestamp=%v", qTr, t2)
	}
	if hdgTr := findTriple(retained.Triples, uxsvocab.PredicateKinematicsHeadingDeg); hdgTr.Confidence != 0.0 || !hdgTr.Timestamp.Equal(t2Global) {
		t.Fatalf("heading after health-only update = %+v, want Confidence=0 Timestamp=%v", hdgTr, t2Global)
	}

	// Re-measure heading (#33 Hdg=18000), then local position (#32), then attitude (#31) one by one.
	t2Hdg := t2.Add(750 * time.Millisecond)
	tracker2.IngestDatagram(encodeFrameBytes(t, 1, 1, 5, mavcodec.GlobalPositionInt{
		Lat: 302672000, Lon: -977431000, Alt: 150000, Hdg: 18000,
	}), peer, t2Hdg)
	snap2Hdg, _ := tracker2.Snapshot(1)
	retained = graph.ReplaceBySource(retained.Triples, snap2Hdg.Triples())
	if hdgTr := findTriple(retained.Triples, uxsvocab.PredicateKinematicsHeadingDeg); hdgTr.Confidence != 1.0 || hdgTr.Context != "measured" || hdgTr.Object != 180.0 || !hdgTr.Timestamp.Equal(t2Hdg) {
		t.Fatalf("re-measured heading = %+v, want 180 Confidence=1 Timestamp=%v", hdgTr, t2Hdg)
	}
	if qTr := findTriple(retained.Triples, uxsvocab.PredicateAttitudeQuaternion); qTr.Confidence != 0.0 {
		t.Fatalf("attitude unexpectedly measured on #33: %+v", qTr)
	}
	if nedTr := findTriple(retained.Triples, uxsvocab.PredicateKinematicsNED); nedTr.Confidence != 0.0 {
		t.Fatalf("local NED unexpectedly measured on #33: %+v", nedTr)
	}

	t2Ned := t2.Add(1000 * time.Millisecond)
	tracker2.IngestDatagram(encodeFrameBytes(t, 1, 1, 6, mavcodec.LocalPositionNED{
		X: 15.0, Y: -8.0, Z: -4.0, Vx: 0.5, Vy: 0.0, Vz: 0.0,
	}), peer, t2Ned)
	snap2Ned, _ := tracker2.Snapshot(1)
	retained = graph.ReplaceBySource(retained.Triples, snap2Ned.Triples())
	if nedTr := findTriple(retained.Triples, uxsvocab.PredicateKinematicsNED); nedTr.Confidence != 1.0 || nedTr.Context != "measured" || nedTr.Object != ([3]float64{15, -8, -4}) || !nedTr.Timestamp.Equal(t2Ned) {
		t.Fatalf("re-measured NED = %+v, want [15,-8,-4] Confidence=1 Timestamp=%v", nedTr, t2Ned)
	}
	if qTr := findTriple(retained.Triples, uxsvocab.PredicateAttitudeQuaternion); qTr.Confidence != 0.0 {
		t.Fatalf("attitude unexpectedly measured on #32: %+v", qTr)
	}

	t2Att := t2.Add(1250 * time.Millisecond)
	c225 := float32(math.Cos(math.Pi / 8))
	s225 := float32(math.Sin(math.Pi / 8))
	tracker2.IngestDatagram(encodeFrameBytes(t, 1, 1, 7, mavcodec.AttitudeQuaternion{
		Q1: c225, Q2: 0, Q3: 0, Q4: s225,
	}), peer, t2Att)
	snap2Att, _ := tracker2.Snapshot(1)
	retained = graph.ReplaceBySource(retained.Triples, snap2Att.Triples())
	if qTr := findTriple(retained.Triples, uxsvocab.PredicateAttitudeQuaternion); qTr.Confidence != 1.0 || qTr.Context != "measured" || !qTr.Timestamp.Equal(t2Att) {
		t.Fatalf("re-measured attitude = %+v, want Confidence=1 Timestamp=%v", qTr, t2Att)
	}
	if hdgTr := findTriple(retained.Triples, uxsvocab.PredicateKinematicsHeadingDeg); hdgTr.Confidence != 1.0 || angularDiffDeg(hdgTr.Object.(float64), 45.0) > 1e-2 {
		t.Fatalf("attitude yaw heading = %+v, want 45 deg", hdgTr)
	}
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
// spec: uxs-control / Pin-on-admission MAVLink v2 UDP command egress and 1 Hz GCS liveness
func TestPropertyThreeLaneHistoryAgainstReferenceModel(t *testing.T) {
	if err := uxsvocab.Register(); err != nil {
		t.Fatalf("uxsvocab.Register: %v", err)
	}
	reg := payloadregistry.New()
	if err := uxsvocab.RegisterPayloads(reg); err != nil {
		t.Fatalf("uxsvocab.RegisterPayloads: %v", err)
	}
	decoder := message.NewDecoder(reg)

	type refVehicle struct {
		admitted      bool
		peerAddr      netip.AddrPort
		domain        string
		armed         bool
		authority     string
		mode          string
		tier          string
		flags         uxsvocab.HealthFlags
		hasSysStatus  bool
		failsafes     int
		hasAttitude   bool
		hasLocalPos   bool
		hasGlobalPos  bool
		hasHeading    bool
		hdgDeg        float64
		latLonAlt     [3]float64
		nedXYZ        [3]float64
		lastPacketAt  time.Time
		skgObservedAt time.Time
		linkLost      bool
		rev           uint64
	}

	domains := []mavcodec.VehicleDomain{mavcodec.DomainQuadcopter, mavcodec.DomainRover, mavcodec.DomainSurfaceBoat}
	authorities := []mavcodec.ControlAuthority{
		mavcodec.AuthorityDirectCRSFOverride,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.AuthorityCompanionAutonomous,
		mavcodec.AuthorityMCUFailsafe,
	}
	modes := []mavcodec.CompanionMode{
		mavcodec.ModeDisarmed,
		mavcodec.ModeTeleopVelocity,
		mavcodec.ModeAutonomousVelocity,
		mavcodec.ModeHoldPosition,
	}
	tiers := []mavcodec.LinkTier{
		mavcodec.LinkTierSITLLocal,
		mavcodec.LinkTierDirectBench,
		mavcodec.LinkTierMeshHighRate,
		mavcodec.LinkTierMeshSupervisory,
	}

	// Boundary reach & assertion activation counters across generated histories.
	var (
		sawAdmission             int
		sawSysStatusBaseline     int
		sawQuatOverride          int
		sawHdgFallback           int
		sawHdgInvalidation       int
		sawHeartbeatEdge         int
		sawFailsafeEdge          int
		sawStatusText            int
		sawSanitizedReject       int
		sawPinnedPeerReject      int
		sawPeerRebindAfterLoss   int
		sawCoalescedPeriodic     int
		sawTrackerRestart        int
		sawLinkLost              int
		sawSupervisoryHold       int
		sawLinkRestored          int
		sawConcurrentRestoreEdge int
		sawSilentNoReupsert      int
	)

	for seed := uint64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		tracker, err := NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
		if err != nil {
			t.Fatalf("NewTrackerWithTimeouts: %v", err)
		}
		ref := map[uint8]*refVehicle{
			1: {},
			2: {},
			3: {},
		}

		var publishedMu sync.Mutex
		var publishCalls atomic.Int64
		materialized := map[uint8]uxsvocab.VehicleSnapshot{}
		mergedTriples := map[uint8][]message.Triple{}
		lastObservedAt := map[uint8]time.Time{}

		materializedNotify := make(chan struct{}, 1)
		newPub := func() *Publisher {
			return NewPublisher(
				DefaultEntitySubject,
				func(_ context.Context, _ string, msgs [][]byte) error {
					publishCalls.Add(1)
					publishedMu.Lock()
					for _, raw := range msgs {
						bm, uErr := decoder.Decode(raw)
						if uErr != nil {
							publishedMu.Unlock()
							t.Fatalf("seed %d: decoder.Decode: %v", seed, uErr)
						}
						snap := *bm.Payload().(*uxsvocab.VehicleSnapshot)
						if prevTS, ok := lastObservedAt[snap.SysID]; ok && snap.ObservedAt.Before(prevTS) {
							publishedMu.Unlock()
							t.Fatalf("seed %d sysid=%d: monotonic ObservedAt regressed (%v < %v)", seed, snap.SysID, snap.ObservedAt, prevTS)
						}
						lastObservedAt[snap.SysID] = snap.ObservedAt
						materialized[snap.SysID] = snap
						rep := graph.ReplaceBySource(mergedTriples[snap.SysID], snap.Triples())
						if len(rep.Stale) != 0 {
							publishedMu.Unlock()
							t.Fatalf("seed %d sysid=%d: ReplaceBySource rejected triples as stale: %+v", seed, snap.SysID, rep.Stale)
						}
						mergedTriples[snap.SysID] = rep.Triples
					}
					publishedMu.Unlock()
					select {
					case materializedNotify <- struct{}{}:
					default:
					}
					return nil
				},
				func(sysids []uint8) ([]SnapshotBatchItem, error) {
					return tracker.BuildUrgentBatch(sysids)
				},
				func(confirmed map[uint8]uint64) ([]SnapshotBatchItem, error) {
					return tracker.BuildPeriodicBatch(confirmed)
				},
				func() { t.Errorf("seed %d: unexpected publisher error", seed) },
			)
		}
		pub := newPub()

		drainPub := func() {
			wantObserved := make(map[uint8]time.Time, len(ref))
			for sysid, rv := range ref {
				if !rv.admitted {
					continue
				}
				snap, ok := tracker.Snapshot(sysid)
				if !ok {
					t.Fatalf("seed %d: admitted sysid=%d missing from tracker", seed, sysid)
				}
				if !snap.ObservedAt.Equal(rv.skgObservedAt) {
					t.Fatalf("seed %d sysid=%d: Snapshot ObservedAt = %v, want reference %v", seed, sysid, snap.ObservedAt, rv.skgObservedAt)
				}
				wantObserved[sysid] = rv.skgObservedAt
			}

			converged := func() bool {
				if pub.hasPending() {
					return false
				}
				publishedMu.Lock()
				defer publishedMu.Unlock()
				for sysid, wantTS := range wantObserved {
					got, ok := materialized[sysid]
					if !ok || !got.ObservedAt.Equal(wantTS) {
						return false
					}
				}
				return true
			}

			runCtx, runCancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				pub.Run(runCtx)
			}()

			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			poll := time.NewTicker(time.Millisecond)
			defer poll.Stop()

			for !converged() {
				select {
				case <-materializedNotify:
				case <-poll.C:
				case <-deadline.C:
					runCancel()
					<-done
					t.Fatalf("seed %d: timed out waiting for publisher to materialize %d vehicle(s)", seed, len(wantObserved))
				}
			}

			runCancel()
			<-done
		}

		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		for step := range 75 {
			if seed%2 == 0 && step == 35 {
				pub.NotifyPeriodic()
				drainPub()

				tracker, err = NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
				if err != nil {
					t.Fatalf("seed %d: restart NewTrackerWithTimeouts: %v", seed, err)
				}
				pub = newPub()
				publishedMu.Lock()
				materialized = map[uint8]uxsvocab.VehicleSnapshot{}
				publishedMu.Unlock()
				ref = map[uint8]*refVehicle{
					1: {},
					2: {},
					3: {},
				}
				now = now.Add(time.Minute)
				sawTrackerRestart++
			}

			now = now.Add(50 * time.Millisecond)
			sysid := uint8(1 + rng.IntN(3))
			rv := ref[sysid]
			peer := rv.peerAddr
			if !rv.admitted {
				peer = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(40000+int(sysid)*10+rng.IntN(5)))
			} else if rng.IntN(5) == 0 {
				altPort := uint16(40000 + int(sysid)*10 + int((rv.peerAddr.Port()-uint16(40000+int(sysid)*10)+1)%5))
				peer = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), altPort)
			}

			switch rng.IntN(8) {
			case 0: // #0 HEARTBEAT
				dom := domains[int(sysid-1)%len(domains)]
				armed := rng.IntN(2) == 1
				auth := authorities[rng.IntN(len(authorities))]
				mode := modes[rng.IntN(len(modes))]
				tier := tiers[rng.IntN(len(tiers))]
				flags := uxsvocab.HealthFlags{TeleopDeadmanClamped: rng.IntN(3) == 0}
				hb := makeVehicleHB(dom, armed, auth, mode, tier, flags)

				out := tracker.IngestFrame(mavcodec.Frame{SystemID: sysid, ComponentID: 1, Sequence: uint8(step), Message: hb}, peer, now)
				if rv.admitted && !rv.linkLost && peer != rv.peerAddr {
					if out.Accepted || !out.CountedError {
						t.Fatalf("seed %d step %d: mismatched peer HB while live not rejected: %+v", seed, step, out)
					}
					if gotPeer, ok := tracker.PeerAddr(sysid); !ok || gotPeer != rv.peerAddr {
						t.Fatalf("seed %d step %d: PeerAddr redirected on live mismatch: got %v, want %v", seed, step, gotPeer, rv.peerAddr)
					}
					sawPinnedPeerReject++
					continue
				}
				if !out.Accepted || out.CountedError {
					t.Fatalf("seed %d step %d: valid HB rejected: %+v", seed, step, out)
				}

				domS, _ := dom.VocabString()
				authS, _ := auth.VocabString()
				modeS, _ := mode.VocabString()
				tierS, _ := tier.VocabString()
				if !rv.admitted {
					rv.admitted = true
					rv.peerAddr = peer
					sawAdmission++
					if len(out.Events) != 0 || out.UrgentSKGSync {
						t.Fatalf("seed %d step %d: first HB emitted edge event: %+v", seed, step, out)
					}
				} else {
					wasLost := rv.linkLost
					if wasLost && peer != rv.peerAddr {
						sawPeerRebindAfterLoss++
					}
					rv.peerAddr = peer
					changed := rv.domain != domS || rv.armed != armed || rv.authority != authS ||
						rv.mode != modeS || rv.tier != tierS || rv.flags != flags
					var wantKinds []string
					if wasLost {
						wantKinds = append(wantKinds, EventKindLinkRestored)
						sawLinkRestored++
					}
					if changed {
						wantKinds = append(wantKinds, EventKindHeartbeatEdge)
						sawHeartbeatEdge++
					}
					if wasLost && changed {
						sawConcurrentRestoreEdge++
					}
					gotKinds := make([]string, len(out.Events))
					for i := range out.Events {
						gotKinds[i] = out.Events[i].Kind
					}
					if !slices.Equal(gotKinds, wantKinds) || out.UrgentSKGSync != (len(wantKinds) > 0) {
						t.Fatalf("seed %d step %d: HB kinds=%v urgent=%v, want kinds=%v", seed, step, gotKinds, out.UrgentSKGSync, wantKinds)
					}
					if out.UrgentSKGSync {
						pub.NotifyUrgent(sysid)
					}
				}
				rv.domain, rv.armed, rv.authority, rv.mode, rv.tier, rv.flags = domS, armed, authS, modeS, tierS, flags
				rv.lastPacketAt = now
				rv.skgObservedAt = now
				rv.linkLost = false
				rv.rev++

			case 1: // #1 SYS_STATUS
				var nextFS uint16
				if !rv.hasSysStatus {
					nextFS = uint16(rng.IntN(6))
				} else {
					switch rng.IntN(3) {
					case 0:
						nextFS = uint16(rv.failsafes + 1)
					case 1:
						nextFS = uint16(rv.failsafes)
					default:
						if rv.failsafes > 0 {
							nextFS = uint16(rv.failsafes - 1)
						}
					}
				}
				out := tracker.IngestFrame(mavcodec.Frame{
					SystemID: sysid, ComponentID: 1, Sequence: uint8(step),
					Message: mavcodec.SysStatus{Load: 800, ErrorsCount1: nextFS},
				}, peer, now)
				if !rv.admitted {
					if out.Accepted {
						t.Fatalf("seed %d: unadmitted SYS_STATUS accepted", seed)
					}
					continue
				}
				if peer != rv.peerAddr {
					if out.Accepted || !out.CountedError {
						t.Fatalf("seed %d: mismatched peer SYS_STATUS accepted", seed)
					}
					sawPinnedPeerReject++
					continue
				}
				wasLost := rv.linkLost
				wantEdge := rv.hasSysStatus && int(nextFS) > rv.failsafes
				var wantKinds []string
				if wasLost {
					wantKinds = append(wantKinds, EventKindLinkRestored)
					sawLinkRestored++
				}
				if wantEdge {
					wantKinds = append(wantKinds, EventKindFailsafeIncrement)
					sawFailsafeEdge++
				}
				if wasLost && wantEdge {
					sawConcurrentRestoreEdge++
				}
				gotKinds := make([]string, len(out.Events))
				for i := range out.Events {
					gotKinds[i] = out.Events[i].Kind
				}
				if !slices.Equal(gotKinds, wantKinds) || out.UrgentSKGSync != (len(wantKinds) > 0) {
					t.Fatalf("seed %d step %d: SYS_STATUS kinds=%v urgent=%v, want kinds=%v", seed, step, gotKinds, out.UrgentSKGSync, wantKinds)
				}
				if !rv.hasSysStatus && nextFS > 0 {
					sawSysStatusBaseline++
				}
				if out.UrgentSKGSync {
					pub.NotifyUrgent(sysid)
				}
				rv.hasSysStatus = true
				rv.failsafes = int(nextFS)
				rv.lastPacketAt = now
				rv.skgObservedAt = now
				rv.linkLost = false
				rv.rev++

			case 2: // #31 ATTITUDE_QUATERNION (valid or injected NaN/zero-norm)
				if rng.IntN(4) == 0 {
					badQ := mavcodec.AttitudeQuaternion{} // zero norm
					if rng.IntN(2) == 0 {
						badQ.Q1 = float32(math.NaN())
					}
					out := tracker.IngestFrame(mavcodec.Frame{SystemID: sysid, ComponentID: 1, Sequence: uint8(step), Message: badQ}, peer, now)
					if !out.CountedError || out.Accepted {
						t.Fatalf("seed %d: invalid #31 not rejected: %+v", seed, out)
					}
					if rv.admitted && tracker.IsLinkLost(sysid) != rv.linkLost {
						t.Fatalf("seed %d: invalid #31 mutated linkLost state", seed)
					}
					sawSanitizedReject++
					continue
				}
				yaw := float64(rng.IntN(360))
				q, wantYaw := referenceBodyXHeadingDeg(5.0, -3.0, yaw)
				out := tracker.IngestFrame(mavcodec.Frame{
					SystemID: sysid, ComponentID: 1, Sequence: uint8(step),
					Message: mavcodec.AttitudeQuaternion{Q1: float32(q[0]), Q2: float32(q[1]), Q3: float32(q[2]), Q4: float32(q[3])},
				}, peer, now)
				if !rv.admitted {
					if out.Accepted {
						t.Fatalf("seed %d: unadmitted #31 accepted", seed)
					}
					continue
				}
				if peer != rv.peerAddr {
					if out.Accepted || !out.CountedError {
						t.Fatalf("seed %d: mismatched peer #31 accepted", seed)
					}
					sawPinnedPeerReject++
					continue
				}
				wasLost := rv.linkLost
				if wasLost {
					if len(out.Events) != 1 || out.Events[0].Kind != EventKindLinkRestored || !out.UrgentSKGSync {
						t.Fatalf("seed %d: #31 link_restored mismatch: %+v", seed, out)
					}
					sawLinkRestored++
					pub.NotifyUrgent(sysid)
				} else if len(out.Events) != 0 || out.UrgentSKGSync {
					t.Fatalf("seed %d: unexpected #31 edge: %+v", seed, out)
				}
				if rv.hasGlobalPos {
					sawQuatOverride++
				}
				rv.hasAttitude = true
				rv.hasHeading = true
				rv.hdgDeg = wantYaw
				rv.lastPacketAt = now
				rv.skgObservedAt = now
				rv.linkLost = false
				rv.rev++
				if out.Telemetry == nil || !out.Telemetry.HasHeading || angularDiffDeg(out.Telemetry.HdgDeg, wantYaw) > 1e-2 {
					t.Fatalf("seed %d: #31 telemetry heading = %+v, want %v", seed, out.Telemetry, wantYaw)
				}

			case 3: // #33 GLOBAL_POSITION_INT (valid or out-of-bounds)
				if rng.IntN(5) == 0 {
					out := tracker.IngestFrame(mavcodec.Frame{
						SystemID: sysid, ComponentID: 1, Sequence: uint8(step),
						Message: mavcodec.GlobalPositionInt{Lat: 900_000_001, Lon: 0, Hdg: 0},
					}, peer, now)
					if !out.CountedError || out.Accepted {
						t.Fatalf("seed %d: out-of-bounds #33 not rejected", seed)
					}
					if rv.admitted && tracker.IsLinkLost(sysid) != rv.linkLost {
						t.Fatalf("seed %d: invalid #33 mutated linkLost state", seed)
					}
					sawSanitizedReject++
					continue
				}
				hdgCdeg := uint16(rng.IntN(36000))
				if rng.IntN(4) == 0 {
					hdgCdeg = 65535
				}
				latE7 := int32(rng.IntN(1_800_000_001)) - 900_000_000
				lonE7 := int32(rng.IntN(3_600_000_001)) - 1_800_000_000
				out := tracker.IngestFrame(mavcodec.Frame{
					SystemID: sysid, ComponentID: 1, Sequence: uint8(step),
					Message: mavcodec.GlobalPositionInt{Lat: latE7, Lon: lonE7, Alt: 25000, Hdg: hdgCdeg},
				}, peer, now)
				if !rv.admitted {
					if out.Accepted {
						t.Fatalf("seed %d: unadmitted #33 accepted", seed)
					}
					continue
				}
				if peer != rv.peerAddr {
					if out.Accepted || !out.CountedError {
						t.Fatalf("seed %d: mismatched peer #33 accepted", seed)
					}
					sawPinnedPeerReject++
					continue
				}
				wasLost := rv.linkLost
				if wasLost {
					if len(out.Events) != 1 || out.Events[0].Kind != EventKindLinkRestored || !out.UrgentSKGSync {
						t.Fatalf("seed %d: #33 link_restored mismatch: %+v", seed, out)
					}
					sawLinkRestored++
					pub.NotifyUrgent(sysid)
				} else if len(out.Events) != 0 || out.UrgentSKGSync {
					t.Fatalf("seed %d: unexpected #33 edge: %+v", seed, out)
				}
				rv.hasGlobalPos = true
				rv.latLonAlt = [3]float64{float64(latE7) / 1e7, float64(lonE7) / 1e7, 25.0}
				if !rv.hasAttitude {
					if hdgCdeg < 36000 {
						rv.hdgDeg = float64(hdgCdeg) / 100.0
						rv.hasHeading = true
						sawHdgFallback++
					} else {
						if rv.hasHeading {
							sawHdgInvalidation++
						}
						rv.hdgDeg = 0
						rv.hasHeading = false
					}
				}
				rv.lastPacketAt = now
				rv.skgObservedAt = now
				rv.linkLost = false
				rv.rev++
				if out.Telemetry == nil || !out.Telemetry.HasGlobalPos || out.Telemetry.HasHeading != rv.hasHeading || angularDiffDeg(out.Telemetry.HdgDeg, rv.hdgDeg) > 1e-2 {
					t.Fatalf("seed %d: #33 telemetry = %+v, want hasHeading=%v hdg=%v", seed, out.Telemetry, rv.hasHeading, rv.hdgDeg)
				}

			case 4: // #32 LOCAL_POSITION_NED
				x := float32(rng.IntN(200) - 100)
				out := tracker.IngestFrame(mavcodec.Frame{
					SystemID: sysid, ComponentID: 1, Sequence: uint8(step),
					Message: mavcodec.LocalPositionNED{X: x, Y: 5, Z: -10},
				}, peer, now)
				if !rv.admitted {
					continue
				}
				if peer != rv.peerAddr {
					if out.Accepted || !out.CountedError {
						t.Fatalf("seed %d: mismatched peer #32 accepted", seed)
					}
					sawPinnedPeerReject++
					continue
				}
				wasLost := rv.linkLost
				if wasLost {
					if len(out.Events) != 1 || out.Events[0].Kind != EventKindLinkRestored || !out.UrgentSKGSync {
						t.Fatalf("seed %d: #32 link_restored mismatch: %+v", seed, out)
					}
					sawLinkRestored++
					pub.NotifyUrgent(sysid)
				} else if len(out.Events) != 0 || out.UrgentSKGSync {
					t.Fatalf("seed %d: unexpected #32 edge: %+v", seed, out)
				}
				rv.hasLocalPos = true
				rv.nedXYZ = [3]float64{float64(x), 5, -10}
				rv.lastPacketAt = now
				rv.skgObservedAt = now
				rv.linkLost = false
				rv.rev++
				if out.Telemetry == nil || !out.Telemetry.HasLocalPos || out.Telemetry.NEDXYZ != rv.nedXYZ {
					t.Fatalf("seed %d: #32 telemetry = %+v, want %v", seed, out.Telemetry, rv.nedXYZ)
				}

			case 5: // #253 STATUSTEXT (event-only in steady state; restores + UrgentSKGSync if silent)
				out := tracker.IngestFrame(mavcodec.Frame{
					SystemID: sysid, ComponentID: 1, Sequence: uint8(step),
					Message: mavcodec.StatusText{Severity: 6, Text: "OK\x00"},
				}, peer, now)
				if !rv.admitted {
					continue
				}
				if peer != rv.peerAddr {
					if out.Accepted || !out.CountedError {
						t.Fatalf("seed %d: mismatched peer #253 accepted", seed)
					}
					sawPinnedPeerReject++
					continue
				}
				sawStatusText++
				wasLost := rv.linkLost
				rv.lastPacketAt = now
				rv.linkLost = false
				if wasLost {
					rv.skgObservedAt = now
					rv.rev++
					if !out.UrgentSKGSync || len(out.Events) != 2 ||
						out.Events[0].Kind != EventKindLinkRestored ||
						out.Events[1].Kind != EventKindStatusText || out.Events[1].StatusText.Text != "OK" {
						t.Fatalf("seed %d: resuming #253 outcome = %+v", seed, out)
					}
					sawLinkRestored++
					sawConcurrentRestoreEdge++
					pub.NotifyUrgent(sysid)
				} else {
					if out.UrgentSKGSync || len(out.Events) != 1 ||
						out.Events[0].Kind != EventKindStatusText || out.Events[0].StatusText.Text != "OK" {
						t.Fatalf("seed %d: steady-state #253 outcome = %+v, want event-only without UrgentSKGSync", seed, out)
					}
				}

			case 6: // Coalesced periodic notification burst
				burst := 1 + rng.IntN(4)
				if burst > 1 {
					sawCoalescedPeriodic++
				}
				for range burst {
					pub.NotifyPeriodic()
				}

			case 7: // Virtual-clock silence evaluation across LinkTier thresholds + silent non-re-upsert check
				advance := time.Duration(2+rng.IntN(11)) * time.Second
				now = now.Add(advance)

				var wantLost []uint8
				anyAdmitted := false
				for s := uint8(1); s <= 3; s++ {
					srv := ref[s]
					if !srv.admitted {
						continue
					}
					anyAdmitted = true
					if srv.linkLost {
						continue
					}
					thresh := 3 * time.Second
					if srv.tier == uxsvocab.LinkTierMeshSupervisory {
						thresh = 10 * time.Second
					}
					elapsed := now.Sub(srv.lastPacketAt)
					if elapsed >= thresh {
						srv.linkLost = true
						wantLost = append(wantLost, s)
					} else if srv.tier == uxsvocab.LinkTierMeshSupervisory && elapsed >= 3*time.Second {
						sawSupervisoryHold++
					}
				}

				gotLostEvs := tracker.EvaluateSilence(now)
				gotLostSysIDs := make([]uint8, len(gotLostEvs))
				for i, ev := range gotLostEvs {
					if ev.Kind != EventKindLinkLost || ev.Timestamp != now.UnixMilli() {
						t.Fatalf("seed %d step %d: EvaluateSilence event = %+v", seed, step, ev)
					}
					gotLostSysIDs[i] = ev.SysID
				}
				if !slices.Equal(gotLostSysIDs, wantLost) {
					t.Fatalf("seed %d step %d: EvaluateSilence sysids = %v, want %v", seed, step, gotLostSysIDs, wantLost)
				}
				sawLinkLost += len(gotLostEvs)

				// Immediate re-evaluation at the same timestamp MUST NOT duplicate link_lost!
				if dup := tracker.EvaluateSilence(now); len(dup) != 0 {
					t.Fatalf("seed %d step %d: duplicate link_lost emitted: %+v", seed, step, dup)
				}
				for s := uint8(1); s <= 3; s++ {
					if !ref[s].admitted {
						continue
					}
					if tracker.IsLinkLost(s) != ref[s].linkLost {
						t.Fatalf("seed %d step %d sysid=%d: IsLinkLost=%v, want %v", seed, step, s, tracker.IsLinkLost(s), ref[s].linkLost)
					}
					if snap, _ := tracker.Snapshot(s); !snap.ObservedAt.Equal(ref[s].skgObservedAt) {
						t.Fatalf("seed %d step %d sysid=%d: EvaluateSilence mutated ObservedAt (%v != %v)", seed, step, s, snap.ObservedAt, ref[s].skgObservedAt)
					}
				}

				// Verify silent / unchanged sessions do not re-upsert on repeated NotifyPeriodic ticks.
				if anyAdmitted && rng.IntN(2) == 0 {
					pub.NotifyPeriodic()
					drainPub()
					beforeSilentCalls := publishCalls.Load()
					pub.NotifyPeriodic()
					drainPub()
					if afterSilentCalls := publishCalls.Load(); afterSilentCalls != beforeSilentCalls {
						t.Fatalf("seed %d step %d: silent NotifyPeriodic re-upserted (%d -> %d)", seed, step, beforeSilentCalls, afterSilentCalls)
					}
					sawSilentNoReupsert++
				}
			}
		}

		// Final periodic notify + drain Publisher worker to convergence.
		pub.NotifyPeriodic()
		drainPub()

		// Verify materialized SKG snapshots and retained ReplaceBySource triples match the reference model.
		publishedMu.Lock()
		for sysid, rv := range ref {
			snap, ok := materialized[sysid]
			if !rv.admitted {
				if ok {
					t.Fatalf("seed %d: unadmitted sysid=%d materialized", seed, sysid)
				}
				continue
			}
			if !ok {
				t.Fatalf("seed %d: admitted sysid=%d missing from materialized SKG", seed, sysid)
			}
			if snap.Domain != rv.domain || snap.Armed != rv.armed || snap.Authority != rv.authority ||
				snap.CompanionMode != rv.mode || snap.LinkTier != rv.tier || snap.Flags != rv.flags ||
				snap.Failsafes != rv.failsafes || snap.HasSysStatus != rv.hasSysStatus || snap.HasAttitude != rv.hasAttitude ||
				snap.HasLocalPos != rv.hasLocalPos || snap.HasGlobalPos != rv.hasGlobalPos ||
				snap.HasHeading != rv.hasHeading || angularDiffDeg(snap.HeadingDeg, rv.hdgDeg) > 1e-2 ||
				snap.NED != rv.nedXYZ || [3]float64{snap.Latitude, snap.Longitude, snap.Altitude} != rv.latLonAlt ||
				!snap.ObservedAt.Equal(rv.skgObservedAt) {
				t.Fatalf("seed %d sysid=%d materialized mismatch:\ngot =%+v\nwant=%+v", seed, sysid, snap, *rv)
			}
			assertMergedDomain := func(pred string, wantMeasured bool) message.Triple {
				for i := range mergedTriples[sysid] {
					if mergedTriples[sysid][i].Predicate == pred {
						tr := mergedTriples[sysid][i]
						wantConf := 0.0
						wantCtx := ""
						if wantMeasured {
							wantConf = 1.0
							wantCtx = "measured"
						}
						if tr.Confidence != wantConf || tr.Context != wantCtx {
							t.Fatalf("seed %d sysid=%d ReplaceBySource %s = %+v, want measured=%v", seed, sysid, pred, tr, wantMeasured)
						}
						return tr
					}
				}
				t.Fatalf("seed %d sysid=%d: missing %s in ReplaceBySource output", seed, sysid, pred)
				return message.Triple{}
			}
			assertMergedDomain(uxsvocab.PredicateHealthLoopDtUs, rv.hasSysStatus)
			assertMergedDomain(uxsvocab.PredicateHealthRTTUs, rv.hasSysStatus)
			assertMergedDomain(uxsvocab.PredicateAttitudeQuaternion, rv.hasAttitude)
			assertMergedDomain(uxsvocab.PredicateKinematicsNED, rv.hasLocalPos)
			assertMergedDomain("geo.location.latitude", rv.hasGlobalPos)
			gotHdgTr := assertMergedDomain(uxsvocab.PredicateKinematicsHeadingDeg, rv.hasHeading)
			gotHdgVal, okHdg := gotHdgTr.Object.(float64)
			if !okHdg || angularDiffDeg(gotHdgVal, rv.hdgDeg) > 1e-2 {
				t.Fatalf("seed %d sysid=%d ReplaceBySource heading value = %+v, want %v", seed, sysid, gotHdgTr, rv.hdgDeg)
			}
		}
		publishedMu.Unlock()
	}

	if sawAdmission == 0 || sawSysStatusBaseline == 0 || sawQuatOverride == 0 || sawHdgFallback == 0 || sawHdgInvalidation == 0 ||
		sawHeartbeatEdge == 0 || sawFailsafeEdge == 0 || sawStatusText == 0 ||
		sawSanitizedReject == 0 || sawPinnedPeerReject == 0 || sawPeerRebindAfterLoss == 0 ||
		sawCoalescedPeriodic == 0 || sawTrackerRestart == 0 ||
		sawLinkLost == 0 || sawSupervisoryHold == 0 || sawLinkRestored == 0 ||
		sawConcurrentRestoreEdge == 0 || sawSilentNoReupsert == 0 {
		t.Fatalf("insufficient property boundary reach: admit=%d sysBaseline=%d quatOverride=%d hdgFallback=%d hdgInvalid=%d hbEdge=%d fsEdge=%d st=%d reject=%d peerReject=%d peerRebind=%d coalesced=%d restart=%d lost=%d supHold=%d restored=%d concurrent=%d silentSkip=%d",
			sawAdmission, sawSysStatusBaseline, sawQuatOverride, sawHdgFallback, sawHdgInvalidation,
			sawHeartbeatEdge, sawFailsafeEdge, sawStatusText, sawSanitizedReject, sawPinnedPeerReject, sawPeerRebindAfterLoss,
			sawCoalescedPeriodic, sawTrackerRestart, sawLinkLost, sawSupervisoryHold, sawLinkRestored, sawConcurrentRestoreEdge, sawSilentNoReupsert)
	}
}

func angularDiffDeg(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360.0)
	if d > 180.0 {
		return 360.0 - d
	}
	return d
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func FuzzTrackerIngestDatagram(f *testing.F) {
	hb := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{},
	)
	seedMsgs := []mavcodec.Message{
		hb,
		mavcodec.Heartbeat{Type: 6, Autopilot: 8},
		mavcodec.SysStatus{Load: 500, ErrorsCount1: 1},
		mavcodec.AttitudeQuaternion{Q1: 1},
		mavcodec.AttitudeQuaternion{Q1: float32(math.NaN())},
		mavcodec.LocalPositionNED{X: 1, Y: 2, Z: -3},
		mavcodec.GlobalPositionInt{Lat: 377749000, Lon: -1224194000, Alt: 10000, Hdg: 18000},
		mavcodec.GlobalPositionInt{Lat: 900_000_001, Lon: 0, Hdg: 0},
		mavcodec.StatusText{Severity: 4, Text: "FUZZ_SEED"},
	}
	for i, m := range seedMsgs {
		raw, err := mavcodec.EncodeFrame(mavcodec.Frame{
			Sequence:    uint8(i),
			SystemID:    1,
			ComponentID: 1,
			Message:     m,
		})
		if err == nil {
			f.Add(raw)
		}
	}
	f.Add([]byte{0xFE, 0x09, 0x00, 0x01, 0x01, 0x00, 0, 0, 0, 0, 2, 0, 0x81, 4, 3, 0x11, 0x22})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, datagram []byte) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:14555")
		t0 := time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC)

		// First feed on an empty tracker, then admit sysid=1 and feed the same datagram again
		// so fuzzing reaches both pre-admission and post-admission paths!
		outPre := tracker.IngestDatagram(datagram, peer, t0)
		if outPre.Accepted && outPre.CountedError {
			t.Fatalf("mutually exclusive Accepted && CountedError on pre-admission: %+v", outPre)
		}

		admitRaw, err := mavcodec.EncodeFrame(mavcodec.Frame{
			Sequence: 0, SystemID: 1, ComponentID: 1, Message: hb,
		})
		if err != nil {
			t.Fatalf("EncodeFrame seed HB: %v", err)
		}
		tracker.IngestDatagram(admitRaw, peer, t0)
		before, _ := tracker.Snapshot(1)

		outPost := tracker.IngestDatagram(datagram, peer, t0.Add(time.Second))
		if outPost.Accepted && outPost.CountedError {
			t.Fatalf("mutually exclusive Accepted && CountedError on post-admission: %+v", outPost)
		}
		after, ok := tracker.Snapshot(1)
		if !ok {
			t.Fatal("admitted sysid=1 disappeared")
		}
		//Note: steady-state #253 STATUSTEXT is accepted without mutating Snapshot(1), whereas rejected datagrams MUST NOT mutate Snapshot(1).
		if !outPost.Accepted && after != before {
			t.Fatalf("rejected datagram mutated sysid=1 snapshot:\nbefore=%+v\nafter =%+v", before, after)
		}
		if err := after.Validate(); err != nil {
			t.Fatalf("admitted snapshot violated Validate() after arbitrary datagram: %v", err)
		}
		if triples := after.Triples(); len(triples) != 22 {
			t.Fatalf("Triples() count = %d, want 22", len(triples))
		}

		// Silence evaluation after arbitrary input must preserve the 22-predicate invariant and frozen ObservedAt.
		_ = tracker.EvaluateSilence(t0.Add(20 * time.Second))
		silentSnap, _ := tracker.Snapshot(1)
		if silentSnap != after {
			t.Fatalf("EvaluateSilence mutated VehicleSnapshot:\nbefore=%+v\nafter =%+v", after, silentSnap)
		}
	})
}

// spec: mavlink-ingest / Constant-predicate Graphable vehicle snapshot payload
// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestTrackerPositionOnlyPacketsFreezeHeartbeatTripleTimestamps(t *testing.T) {
	tracker, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	peer := netip.MustParseAddrPort("127.0.0.1:45210")
	t0 := time.Date(2026, 11, 25, 19, 0, 0, 0, time.UTC)

	hb := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierMeshHighRate,
		uxsvocab.HealthFlags{
			ClockSyncLocked:       true,
			CRSFLinkHealthy:       true,
			CompanionTrunkHealthy: true,
		},
	)
	if out := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 1, hb), peer, t0); !out.Accepted {
		t.Fatalf("initial HEARTBEAT rejected: %+v", out)
	}

	snap0, _ := tracker.Snapshot(1)
	storedTriples := snap0.Triples()

	heartbeatPredicates := []string{
		uxsvocab.PredicateDomainKind,
		uxsvocab.PredicateAuthorityState,
		uxsvocab.PredicateModeCompanion,
		uxsvocab.PredicateLinkTier,
		uxsvocab.PredicateHealthArmed,
		uxsvocab.PredicateHealthFlags,
	}

	// Feed 1 Hz #32 LOCAL_POSITION_NED for 6 seconds (plus #1 SYS_STATUS at T+5s) with zero heartbeats.
	for sec := 1; sec <= 6; sec++ {
		sampleTS := t0.Add(time.Duration(sec) * time.Second)
		posOut := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, uint8(1+sec), mavcodec.LocalPositionNED{
			X: float32(sec * 2), Y: -1.5, Z: -12.0, Vx: 2.0,
		}), peer, sampleTS)
		if !posOut.Accepted {
			t.Fatalf("LOCAL_POSITION_NED at +%ds rejected: %+v", sec, posOut)
		}
		if sec == 5 {
			sysOut := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 50, mavcodec.SysStatus{
				Load: 1100, ErrorsComm: 0, ErrorsCount1: 0, ErrorsCount2: 1400, ErrorsCount3: 390,
			}), peer, sampleTS)
			if !sysOut.Accepted {
				t.Fatalf("SYS_STATUS at +5s rejected: %+v", sysOut)
			}
		}

		snap, _ := tracker.Snapshot(1)
		rep := graph.ReplaceBySource(storedTriples, snap.Triples())
		if len(rep.Stale) != 0 {
			t.Fatalf("ReplaceBySource at +%ds rejected stale triples: %+v", sec, rep.Stale)
		}
		storedTriples = rep.Triples

		byPred := make(map[string]message.Triple, len(storedTriples))
		for _, tr := range storedTriples {
			byPred[tr.Predicate] = tr
		}
		for _, pred := range heartbeatPredicates {
			if !byPred[pred].Timestamp.Equal(t0) {
				t.Fatalf("after +%ds without HEARTBEAT, %s Timestamp = %v, want frozen %v",
					sec, pred, byPred[pred].Timestamp, t0)
			}
		}
		if !byPred[uxsvocab.PredicateKinematicsNED].Timestamp.Equal(sampleTS) {
			t.Fatalf("after +%ds, uxs.kinematics.ned Timestamp = %v, want %v",
				sec, byPred[uxsvocab.PredicateKinematicsNED].Timestamp, sampleTS)
		}
	}

	// Delivering a new #0 HEARTBEAT at T+7s advances all heartbeat safety predicates to T+7s.
	t7 := t0.Add(7 * time.Second)
	if out := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 60, hb), peer, t7); !out.Accepted {
		t.Fatalf("recovery HEARTBEAT at +7s rejected: %+v", out)
	}
	snap7, _ := tracker.Snapshot(1)
	rep7 := graph.ReplaceBySource(storedTriples, snap7.Triples())
	if len(rep7.Stale) != 0 {
		t.Fatalf("ReplaceBySource at +7s rejected stale triples: %+v", rep7.Stale)
	}
	storedTriples = rep7.Triples
	byPred7 := make(map[string]message.Triple, len(storedTriples))
	for _, tr := range storedTriples {
		byPred7[tr.Predicate] = tr
	}
	for _, pred := range heartbeatPredicates {
		if !byPred7[pred].Timestamp.Equal(t7) {
			t.Fatalf("after +7s HEARTBEAT, %s Timestamp = %v, want %v", pred, byPred7[pred].Timestamp, t7)
		}
	}
}

// spec: uxs-control / Pin-on-admission MAVLink v2 UDP command egress and 1 Hz GCS liveness
// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestTrackerPeerPinningAndCommandAck(t *testing.T) {
	if err := uxsvocab.Register(); err != nil {
		t.Fatalf("uxsvocab.Register: %v", err)
	}

	// Verify resolveConfig default loopback allowed_peers vs explicit CIDR/IP parsing.
	defaultCfg, err := resolveConfig(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("resolveConfig default: %v", err)
	}
	if len(defaultCfg.allowedPeers) != 2 {
		t.Fatalf("default loopback allowedPeers = %v, want [127.0.0.0/8 ::1/128]", defaultCfg.allowedPeers)
	}
	wildcardCfg, err := resolveConfig(json.RawMessage(`{"ports":{"inputs":[{"name":"mavlink_udp","external":true,"config":{"kind":"network","protocol":"udp","host":"0.0.0.0","port":14550}}]}}`))
	if err != nil {
		t.Fatalf("resolveConfig 0.0.0.0: %v", err)
	}
	if len(wildcardCfg.allowedPeers) != 0 {
		t.Fatalf("0.0.0.0 default allowedPeers = %v, want empty unless configured", wildcardCfg.allowedPeers)
	}
	customCfg, err := resolveConfig(json.RawMessage(`{"allowed_peers":["127.0.0.1","10.42.0.0/16","::1"]}`))
	if err != nil {
		t.Fatalf("resolveConfig custom allowed_peers: %v", err)
	}
	if len(customCfg.allowedPeers) != 3 {
		t.Fatalf("custom allowedPeers = %v, want 3 entries", customCfg.allowedPeers)
	}
	for _, badJSON := range []string{
		`{"allowed_peers":[""]}`,
		`{"allowed_peers":["not-a-cidr"]}`,
	} {
		if _, badErr := resolveConfig(json.RawMessage(badJSON)); badErr == nil {
			t.Fatalf("resolveConfig(%s) succeeded, want error", badJSON)
		}
	}

	tracker, err := NewTrackerWithOptions(
		"boxbotix",
		"bbops",
		3*time.Second,
		10*time.Second,
		customCfg.allowedPeers,
	)
	if err != nil {
		t.Fatalf("NewTrackerWithOptions: %v", err)
	}

	t0 := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	peerPinned := netip.MustParseAddrPort("127.0.0.1:41001")
	peerPinnedV6Mapped := netip.MustParseAddrPort("[::ffff:127.0.0.1]:41001")
	peerSpoofAllowed := netip.MustParseAddrPort("10.42.1.9:55555")
	peerDisallowed := netip.MustParseAddrPort("192.168.1.77:14550")
	peerRotated := netip.MustParseAddrPort("10.42.2.10:41002")

	hbTeleop := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)

	// Disallowed peer outside allowed_peers is rejected on admission.
	disallowedOut := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 1, hbTeleop), peerDisallowed, t0)
	if disallowedOut.Accepted || !disallowedOut.CountedError {
		t.Fatalf("disallowed peer admission outcome = %+v, want CountedError=true", disallowedOut)
	}
	if _, ok := tracker.ActuationTarget(1); ok {
		t.Fatal("disallowed peer must not admit sysid=1")
	}

	// #77 COMMAND_ACK before admission is ignored without accepting or counting an error.
	preAdmitAck := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 2, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}), peerPinned, t0)
	if preAdmitAck.Accepted || preAdmitAck.CountedError || preAdmitAck.CommandAck != nil {
		t.Fatalf("pre-admission COMMAND_ACK outcome = %+v, want ignored", preAdmitAck)
	}

	// Admit sysid=1 from peerPinned (via IPv4-mapped IPv6 to verify canonical normalization).
	admitOut := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 3, hbTeleop), peerPinnedV6Mapped, t0)
	if !admitOut.Accepted || admitOut.CountedError {
		t.Fatalf("admission outcome = %+v, want Accepted", admitOut)
	}
	target, ok := tracker.ActuationTarget(1)
	if !ok || target.PeerAddr != peerPinned || !target.Armed || target.Authority != uxsvocab.AuthorityCompanionTeleop || target.LinkLost {
		t.Fatalf("ActuationTarget(1) = (%+v, %v), want pinned %v armed companion-teleop", target, ok, peerPinned)
	}

	snapBeforeSpoof, _ := tracker.Snapshot(1)

	// While live (!linkLost), spoofed packets from another allowed peer MUST be rejected across all message types
	// without mutating VehicleSnapshot, redirecting PeerAddr, or refreshing freshness!
	spoofMsgs := []mavcodec.Message{
		makeVehicleHB(mavcodec.DomainRover, false, mavcodec.AuthorityMCUFailsafe, mavcodec.ModeDisarmed, mavcodec.LinkTierMeshSupervisory, uxsvocab.HealthFlags{}),
		mavcodec.SysStatus{Load: 999, ErrorsCount1: 5},
		mavcodec.AttitudeQuaternion{Q1: 1},
		mavcodec.LocalPositionNED{X: 99},
		mavcodec.GlobalPositionInt{Lat: 100, Lon: 200, Hdg: 9000},
		mavcodec.CommandAck{Command: mavcodec.MAVCmdComponentArmDisarm, Result: mavcodec.MAVResultDenied},
		mavcodec.StatusText{Severity: 1, Text: "SPOOF"},
	}
	for i, msg := range spoofMsgs {
		out := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, uint8(10+i), msg), peerSpoofAllowed, t0.Add(2*time.Second))
		if out.Accepted || !out.CountedError || out.CommandAck != nil || len(out.Events) != 0 || out.Telemetry != nil {
			t.Fatalf("spoofed message %d (%T) while live = %+v, want rejected", i, msg, out)
		}
	}
	snapAfterSpoof, _ := tracker.Snapshot(1)
	if snapAfterSpoof != snapBeforeSpoof {
		t.Fatalf("spoofed peer mutated snapshot:\nbefore=%+v\nafter =%+v", snapBeforeSpoof, snapAfterSpoof)
	}
	if gotPeer, _ := tracker.PeerAddr(1); gotPeer != peerPinned {
		t.Fatalf("PeerAddr(1) after spoof = %v, want %v", gotPeer, peerPinned)
	}

	// Because spoofed packets at t0+2s did NOT refresh freshness, silence evaluation at t0+3.1s marks sysid=1 link_lost!
	lostEvs := tracker.EvaluateSilence(t0.Add(3100 * time.Millisecond))
	if len(lostEvs) != 1 || lostEvs[0].SysID != 1 || lostEvs[0].Kind != EventKindLinkLost {
		t.Fatalf("EvaluateSilence after spoof = %+v, want link_lost on sysid=1", lostEvs)
	}
	if targetLost, ok := tracker.ActuationTarget(1); !ok || !targetLost.LinkLost {
		t.Fatalf("ActuationTarget(1) after silence = (%+v, %v), want LinkLost=true", targetLost, ok)
	}

	// Even after link_lost, a non-HEARTBEAT frame (#32 or #77) from rotated peer MUST still be rejected!
	rotNonHB := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 30, mavcodec.LocalPositionNED{X: 5}), peerRotated, t0.Add(3500*time.Millisecond))
	if rotNonHB.Accepted || !rotNonHB.CountedError {
		t.Fatalf("non-HEARTBEAT from rotated peer after link_lost = %+v, want rejected", rotNonHB)
	}
	rotDisallowedHB := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 31, hbTeleop), peerDisallowed, t0.Add(3600*time.Millisecond))
	if rotDisallowedHB.Accepted || !rotDisallowedHB.CountedError {
		t.Fatalf("disallowed peer HEARTBEAT after link_lost = %+v, want rejected", rotDisallowedHB)
	}

	// Valid 0xB1 #0 HEARTBEAT from allowed rotated peer after link_lost cleanly re-binds PeerAddr and emits link_restored.
	rebindOut := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 32, hbTeleop), peerRotated, t0.Add(4*time.Second))
	if !rebindOut.Accepted || rebindOut.CountedError || len(rebindOut.Events) != 1 || rebindOut.Events[0].Kind != EventKindLinkRestored {
		t.Fatalf("rebind HEARTBEAT after link_lost = %+v, want Accepted + link_restored", rebindOut)
	}
	if gotPeer, _ := tracker.PeerAddr(1); gotPeer != peerRotated {
		t.Fatalf("PeerAddr(1) after rebind = %v, want %v", gotPeer, peerRotated)
	}

	// #77 COMMAND_ACK from the newly pinned peer is accepted and surfaced on IngestOutcome.CommandAck.
	ackOut := tracker.IngestDatagram(encodeFrameBytes(t, 1, 1, 33, mavcodec.CommandAck{
		Command:      mavcodec.MAVCmdComponentArmDisarm,
		Result:       mavcodec.MAVResultAccepted,
		TargetSystem: mavcodec.GCSSystemID,
	}), peerRotated, t0.Add(4200*time.Millisecond))
	if !ackOut.Accepted || ackOut.CountedError || ackOut.CommandAck == nil ||
		ackOut.CommandAck.Command != mavcodec.MAVCmdComponentArmDisarm ||
		ackOut.CommandAck.Result != mavcodec.MAVResultAccepted {
		t.Fatalf("COMMAND_ACK from pinned peer = %+v, want CommandAck(400, Accepted)", ackOut)
	}
}

// spec: uxs-control / Pin-on-admission MAVLink v2 UDP command egress and 1 Hz GCS liveness
func TestComponentActuationCallbacksAndSendFrame(t *testing.T) {
	if err := uxsvocab.Register(); err != nil {
		t.Fatalf("uxsvocab.Register: %v", err)
	}

	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.Start()
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded NATS server not ready")
	}

	nc, err := natsclient.NewClient(srv.ClientURL(), natsclient.WithMaxReconnects(0))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := nc.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close(context.Background()) })

	udpPort := allocateLoopbackUDPPort(t)
	rawCfg, err := json.Marshal(map[string]any{
		"skg_hz":          4,
		"link_timeout_ms": 250,
		"ports": map[string]any{
			"inputs": []map[string]any{
				{
					"name":     "mavlink_udp",
					"external": true,
					"config": map[string]any{
						"kind":     "network",
						"protocol": "udp",
						"host":     "127.0.0.1",
						"port":     udpPort,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Marshal config: %v", err)
	}

	comp, err := NewComponent(rawCfg, component.Dependencies{
		NATSClient: nc,
		Platform:   types.PlatformMeta{Org: "boxbotix", Platform: "bbops"},
	})
	if err != nil {
		t.Fatalf("NewComponent: %v", err)
	}

	// SendFrame before Start returns an error cleanly.
	if err := comp.SendFrame(mavcodec.Frame{
		SystemID:    mavcodec.GCSSystemID,
		ComponentID: mavcodec.CompIDMissionPlanner,
		Message:     mavcodec.Heartbeat{Type: mavcodec.MAVTypeGCS, SystemStatus: mavcodec.MAVStateActive},
	}, netip.MustParseAddrPort("127.0.0.1:45550")); err == nil {
		t.Fatal("SendFrame before Start succeeded, want error")
	}

	evCh := make(chan TransitionEvent, 8)
	ackCh := make(chan mavcodec.CommandAck, 4)
	comp.SetActuationCallbacks(
		func(ev TransitionEvent) { evCh <- ev },
		func(sysid uint8, ack mavcodec.CommandAck, _ time.Time) {
			if sysid == 1 {
				ackCh <- ack
			}
		},
	)

	if err := comp.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := comp.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = comp.Stop(stopCtx)
	})

	simConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("simConn ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = simConn.Close() })

	bound := comp.BoundAddr()
	hbInit := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	if _, err := simConn.WriteToUDPAddrPort(encodeFrameBytes(t, 1, 1, 1, hbInit), bound); err != nil {
		t.Fatalf("WriteToUDPAddrPort admission HB: %v", err)
	}

	var target ActuationTarget
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tgt, ok := comp.ActuationTarget(1); ok {
			target = tgt
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !target.PeerAddr.IsValid() {
		t.Fatal("timed out waiting for sysid=1 admission")
	}

	// Send outbound #76 COMMAND_LONG via SendFrame and verify simConn receives exact wire frame.
	if err := comp.SendFrame(mavcodec.Frame{
		Sequence:    9,
		SystemID:    mavcodec.GCSSystemID,
		ComponentID: mavcodec.CompIDMissionPlanner,
		Message: mavcodec.CommandLong{
			TargetSystem:    1,
			TargetComponent: mavcodec.CompIDAutopilot1,
			Command:         mavcodec.MAVCmdComponentArmDisarm,
			Param1:          1,
		},
	}, target.PeerAddr); err != nil {
		t.Fatalf("SendFrame(#76): %v", err)
	}

	_ = simConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, _, err := simConn.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatalf("simConn ReadFromUDPAddrPort: %v", err)
	}
	rxFrame, err := mavcodec.DecodeFrame(buf[:n])
	if err != nil {
		t.Fatalf("DecodeFrame outbound #76: %v", err)
	}
	cmdLong, ok := rxFrame.Message.(mavcodec.CommandLong)
	if !ok || rxFrame.SystemID != mavcodec.GCSSystemID || rxFrame.ComponentID != mavcodec.CompIDMissionPlanner ||
		cmdLong.Command != mavcodec.MAVCmdComponentArmDisarm || cmdLong.TargetSystem != 1 || cmdLong.Param1 != 1 {
		t.Fatalf("decoded outbound #76 = (%+v, %+v)", rxFrame, cmdLong)
	}

	// Reply with #77 COMMAND_ACK + a preempting #0 HEARTBEAT and verify callbacks fire synchronously.
	if _, err := simConn.WriteToUDPAddrPort(encodeFrameBytes(t, 1, 1, 2, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}), bound); err != nil {
		t.Fatalf("WriteToUDPAddrPort #77: %v", err)
	}
	hbOverride := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityDirectCRSFOverride,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	if _, err := simConn.WriteToUDPAddrPort(encodeFrameBytes(t, 1, 1, 3, hbOverride), bound); err != nil {
		t.Fatalf("WriteToUDPAddrPort override HB: %v", err)
	}

	select {
	case gotAck := <-ackCh:
		if gotAck.Command != mavcodec.MAVCmdComponentArmDisarm || gotAck.Result != mavcodec.MAVResultAccepted {
			t.Fatalf("onAck callback = %+v, want (400, Accepted)", gotAck)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onAck callback")
	}

	select {
	case gotEv := <-evCh:
		if gotEv.Kind != EventKindHeartbeatEdge || gotEv.Authority != uxsvocab.AuthorityDirectCRSFOverride {
			t.Fatalf("onEvent callback = %+v, want heartbeat_edge -> direct-crsf-override", gotEv)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onEvent heartbeat_edge callback")
	}

	// Wait past link_timeout_ms (250ms) and verify link_lost also invokes onEvent!
	select {
	case lostEv := <-evCh:
		if lostEv.Kind != EventKindLinkLost || lostEv.SysID != 1 {
			t.Fatalf("onEvent silence callback = %+v, want link_lost on sysid=1", lostEv)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onEvent link_lost callback")
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestTrackerDuplicateCommandAckSuppressionAndAuthorityEpoch(t *testing.T) {
	tracker, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	peer := netip.MustParseAddrPort("127.0.0.1:24551")
	t0 := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

	hbAdmit := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	out0 := tracker.IngestFrame(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    1,
		Message:     hbAdmit,
	}, peer, t0)
	if len(out0.Events) != 0 {
		t.Fatalf("admission Events = %+v, want none", out0.Events)
	}
	if tg, ok := tracker.ActuationTarget(1); !ok || tg.AuthorityEpoch != 1 {
		t.Fatalf("ActuationTarget after admission = (%+v, %v), want AuthorityEpoch=1", tg, ok)
	}

	// First #77 COMMAND_ACK at Sequence=42 is emitted with CommandAckSeq=42.
	ackMsg := mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}
	outAck1 := tracker.IngestFrame(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    42,
		Message:     ackMsg,
	}, peer, t0.Add(10*time.Millisecond))
	if outAck1.CommandAck == nil || outAck1.CommandAckSeq != 42 || *outAck1.CommandAck != ackMsg {
		t.Fatalf("first COMMAND_ACK outcome = %+v, want CommandAckSeq=42 Accepted", outAck1)
	}

	// Exact duplicate delivery of Sequence=42 MUST be suppressed (CommandAck == nil, not counted as error).
	outAckDup := tracker.IngestFrame(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    42,
		Message:     ackMsg,
	}, peer, t0.Add(20*time.Millisecond))
	if !outAckDup.Accepted || outAckDup.CountedError || outAckDup.CommandAck != nil {
		t.Fatalf("duplicate COMMAND_ACK outcome = %+v, want Accepted=true CommandAck=nil", outAckDup)
	}

	// Distinct Sequence=43 with the same payload IS delivered.
	outAck2 := tracker.IngestFrame(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    43,
		Message:     ackMsg,
	}, peer, t0.Add(30*time.Millisecond))
	if outAck2.CommandAck == nil || outAck2.CommandAckSeq != 43 {
		t.Fatalf("distinct Sequence=43 COMMAND_ACK outcome = %+v, want CommandAckSeq=43", outAck2)
	}

	// After 256 intervening accepted frames (uint8 sequence wraparound), Sequence=42 IS delivered again.
	for i := 0; i < 256; i++ {
		tracker.IngestFrame(mavcodec.Frame{
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Sequence:    uint8(44 + i),
			Message:     mavcodec.LocalPositionNED{X: 1},
		}, peer, t0.Add(time.Duration(40+i)*time.Millisecond))
	}
	outAckWrapped := tracker.IngestFrame(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    42,
		Message:     ackMsg,
	}, peer, t0.Add(400*time.Millisecond))
	if outAckWrapped.CommandAck == nil || outAckWrapped.CommandAckSeq != 42 {
		t.Fatalf("wrapped Sequence=42 COMMAND_ACK after 256 packets = %+v, want delivered", outAckWrapped)
	}

	// Authority transition P2 -> P1 increments AuthorityEpoch (1 -> 2), P1 -> P2 increments (2 -> 3),
	// link_lost increments (3 -> 4), and link_restored increments (4 -> 5) while resetting ACK dedup slots!
	hbOverride := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityDirectCRSFOverride,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	outOv := tracker.IngestFrame(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    50,
		Message:     hbOverride,
	}, peer, t0.Add(500*time.Millisecond))
	if len(outOv.Events) != 1 || outOv.Events[0].AuthorityEpoch != 2 {
		t.Fatalf("override AuthorityEpoch = %+v, want 2", outOv.Events)
	}

	outRec := tracker.IngestFrame(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    51,
		Message:     hbAdmit,
	}, peer, t0.Add(550*time.Millisecond))
	if len(outRec.Events) != 1 || outRec.Events[0].AuthorityEpoch != 3 {
		t.Fatalf("recovery AuthorityEpoch = %+v, want 3", outRec.Events)
	}

	lostEvs := tracker.EvaluateSilence(t0.Add(5 * time.Second))
	if len(lostEvs) != 1 || lostEvs[0].AuthorityEpoch != 4 {
		t.Fatalf("link_lost AuthorityEpoch = %+v, want 4", lostEvs)
	}

	// Re-admit after link_lost: AuthorityEpoch advances to 5, and Sequence=42 is accepted cleanly on the new link session.
	outRestored := tracker.IngestFrame(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    1,
		Message:     hbAdmit,
	}, peer, t0.Add(6*time.Second))
	if len(outRestored.Events) == 0 || outRestored.Events[0].AuthorityEpoch != 5 {
		t.Fatalf("link_restored AuthorityEpoch = %+v, want 5", outRestored.Events)
	}
	outPostRestoreAck := tracker.IngestFrame(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    42,
		Message:     ackMsg,
	}, peer, t0.Add(6100*time.Millisecond))
	if outPostRestoreAck.CommandAck == nil {
		t.Fatal("expected Sequence=42 COMMAND_ACK to be delivered after link_lost/link_restored reset")
	}
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestTrackerIgnoresStaleAndDuplicateHeartbeatsForStateAndFreshness(t *testing.T) {
	tracker, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	peer := netip.MustParseAddrPort("127.0.0.1:45210")
	t0 := time.Date(2026, 11, 27, 21, 0, 0, 0, time.UTC)
	monoBase := time.Now()

	hbArmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	hbDisarmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		false,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeDisarmed,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)

	// 1. Admit armed vehicle at seq=10, monoBase + 100ms.
	outAdmit := tracker.IngestFrameWithClock(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    10,
		Message:     hbArmed,
	}, peer, t0, monoBase.Add(100*time.Millisecond))
	if !outAdmit.Accepted || len(outAdmit.Events) != 0 {
		t.Fatalf("admit outcome = %+v, want Accepted with 0 birth events", outAdmit)
	}
	baseTarget, ok := tracker.ActuationTarget(1)
	if !ok || !baseTarget.Armed || baseTarget.HeartbeatSeq != 1 || baseTarget.HeartbeatOrder == 0 {
		t.Fatalf("base target = %+v (ok=%v), want Armed=true HeartbeatSeq=1 HeartbeatOrder>0", baseTarget, ok)
	}

	// 2. Older monotonic sample (monoBase + 50ms < monoBase + 100ms) carrying disarmed seq=11 MUST NOT overwrite state or freshness.
	outOldMono := tracker.IngestFrameWithClock(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    11,
		Message:     hbDisarmed,
	}, peer, t0.Add(-50*time.Millisecond), monoBase.Add(50*time.Millisecond))
	if !outOldMono.Accepted || len(outOldMono.Events) != 0 || outOldMono.UrgentSKGSync {
		t.Fatalf("older monotonic heartbeat outcome = %+v, want Accepted=true with zero state/event mutation", outOldMono)
	}
	if got, _ := tracker.ActuationTarget(1); !got.Armed || got.HeartbeatSeq != baseTarget.HeartbeatSeq || got.HeartbeatOrder != baseTarget.HeartbeatOrder || got.ObservationRev != baseTarget.ObservationRev {
		t.Fatalf("target mutated on older monotonic heartbeat: got %+v, want %+v", got, baseTarget)
	}

	// 3. Duplicate seq=10 and out-of-order older seq=9 arriving at newer monotonic timestamps MUST NOT overwrite armed state or advance HeartbeatSeq/HeartbeatOrder.
	for i, staleSeq := range []uint8{10, 9} {
		outStale := tracker.IngestFrameWithClock(mavcodec.Frame{
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Sequence:    staleSeq,
			Message:     hbDisarmed,
		}, peer, t0.Add(time.Duration(i+1)*10*time.Millisecond), monoBase.Add(time.Duration(110+i*10)*time.Millisecond))
		if !outStale.Accepted || len(outStale.Events) != 0 || outStale.UrgentSKGSync {
			t.Fatalf("stale seq=%d heartbeat outcome = %+v, want zero events/sync", staleSeq, outStale)
		}
		if got, _ := tracker.ActuationTarget(1); !got.Armed || got.HeartbeatSeq != baseTarget.HeartbeatSeq || got.HeartbeatOrder != baseTarget.HeartbeatOrder {
			t.Fatalf("target mutated on stale seq=%d: got %+v, want %+v", staleSeq, got, baseTarget)
		}
		if snap, _ := tracker.Snapshot(1); !snap.Armed {
			t.Fatalf("snapshot Armed became false on stale seq=%d", staleSeq)
		}
	}

	// 4. Unconfirmed large gap (seq=80, delta=70 in 65..128) WITHholds HeartbeatOrder advancement until confirmed by seq=81.
	outGap := tracker.IngestFrameWithClock(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    80,
		Message:     hbArmed,
	}, peer, t0.Add(40*time.Millisecond), monoBase.Add(140*time.Millisecond))
	if !outGap.Accepted {
		t.Fatal("expected gap heartbeat seq=80 to be accepted")
	}
	if gotGap, _ := tracker.ActuationTarget(1); gotGap.HeartbeatOrder != baseTarget.HeartbeatOrder {
		t.Fatalf("unconfirmed gap seq=80 advanced HeartbeatOrder to %d, want held at %d", gotGap.HeartbeatOrder, baseTarget.HeartbeatOrder)
	}
	// Confirm gap with telemetry seq=81 -> promotes pending gap heartbeat order!
	_ = tracker.IngestFrameWithClock(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    81,
		Message:     mavcodec.AttitudeQuaternion{Q1: 1},
	}, peer, t0.Add(45*time.Millisecond), monoBase.Add(145*time.Millisecond))
	if gotConfirmed, _ := tracker.ActuationTarget(1); gotConfirmed.HeartbeatOrder <= baseTarget.HeartbeatOrder {
		t.Fatalf("confirmed gap seq=80->81 did not promote HeartbeatOrder: got %d, base %d", gotConfirmed.HeartbeatOrder, baseTarget.HeartbeatOrder)
	}

	// 5. Genuine forward seq=82 disarmed heartbeat updates state, increments HeartbeatSeq & HeartbeatOrder, and emits TransitionEvent.
	preDisarmTarget, _ := tracker.ActuationTarget(1)
	outFresh := tracker.IngestFrameWithClock(mavcodec.Frame{
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Sequence:    82,
		Message:     hbDisarmed,
	}, peer, t0.Add(60*time.Millisecond), monoBase.Add(160*time.Millisecond))
	if !outFresh.Accepted || len(outFresh.Events) != 1 || outFresh.Events[0].Armed {
		t.Fatalf("fresh seq=82 disarmed outcome = %+v, want 1 disarmed event", outFresh)
	}
	postTarget, _ := tracker.ActuationTarget(1)
	if postTarget.Armed || postTarget.HeartbeatSeq <= preDisarmTarget.HeartbeatSeq || postTarget.HeartbeatOrder <= preDisarmTarget.HeartbeatOrder {
		t.Fatalf("fresh seq=82 target = %+v, want Armed=false and monotonically higher HeartbeatSeq/HeartbeatOrder than %+v", postTarget, preDisarmTarget)
	}
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestTrackerRecoversAfterOver128LostPacketsAndHoldsWrappedDuplicateHeartbeat(t *testing.T) {
	peer := netip.MustParseAddrPort("127.0.0.1:45211")
	t0 := time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC)

	hbTeleopArmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	hbTeleopDisarmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		false,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeDisarmed,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	hbOverrideArmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityDirectCRSFOverride,
		mavcodec.ModeHoldPosition,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)

	t.Run("over_128_packet_loss_establishes_override_and_blocks_isolated_stale_teleop", func(t *testing.T) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		now := t0
		// Initial companion-teleop heartbeat at seq=10.
		if out := tracker.IngestFrame(mavcodec.Frame{
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Sequence:    10,
			Message:     hbTeleopArmed,
		}, peer, now); !out.Accepted {
			t.Fatalf("admit seq=10 failed: %+v", out)
		}

		// Lose 139 packets (> 128); consecutive direct-crsf-override heartbeats at seq=150 and seq=151 arrive.
		now = now.Add(100 * time.Millisecond)
		out150 := tracker.IngestFrame(mavcodec.Frame{
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Sequence:    150,
			Message:     hbOverrideArmed,
		}, peer, now)
		now = now.Add(100 * time.Millisecond)
		out151 := tracker.IngestFrame(mavcodec.Frame{
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Sequence:    151,
			Message:     hbOverrideArmed,
		}, peer, now)

		allEvents := append(slices.Clone(out150.Events), out151.Events...)
		if len(allEvents) != 1 || allEvents[0].Kind != EventKindHeartbeatEdge || allEvents[0].Authority != uxsvocab.AuthorityDirectCRSFOverride {
			t.Fatalf("seq=150..151 events = %+v, want single direct-crsf-override heartbeat_edge", allEvents)
		}
		ovTarget, ok := tracker.ActuationTarget(1)
		if !ok || ovTarget.Authority != uxsvocab.AuthorityDirectCRSFOverride || ovTarget.HeartbeatSeq <= 1 || ovTarget.HeartbeatOrder <= 1 {
			t.Fatalf("target after seq=150..151 = %+v, want direct-crsf-override with HeartbeatSeq>1 HeartbeatOrder>1", ovTarget)
		}

		// Isolated older companion-teleop heartbeats (seq=10, seq=11, seq=149) MUST NOT restore companion-teleop!
		for i, staleSeq := range []uint8{10, 149, 11} {
			now = now.Add(10 * time.Millisecond)
			outStale := tracker.IngestFrame(mavcodec.Frame{
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Sequence:    staleSeq,
				Message:     hbTeleopArmed,
			}, peer, now)
			if len(outStale.Events) != 0 {
				t.Fatalf("isolated stale seq=%d (#%d) emitted unexpected events: %+v", staleSeq, i, outStale.Events)
			}
			if cur, _ := tracker.ActuationTarget(1); cur.Authority != uxsvocab.AuthorityDirectCRSFOverride {
				t.Fatalf("isolated stale seq=%d restored authority=%q, want direct-crsf-override", staleSeq, cur.Authority)
			}
			// Normal stream continuation at seq=152+i wipes any unconfirmed gap candidate cleanly.
			now = now.Add(10 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Sequence:    uint8(152 + i),
				Message:     mavcodec.LocalPositionNED{X: 1},
			}, peer, now)
		}

		// Genuine >128 packet loss back to companion-teleop (seq=154 -> seq=40, 41; delta=142) recovers on consecutive confirmation!
		now = now.Add(100 * time.Millisecond)
		out40 := tracker.IngestFrame(mavcodec.Frame{
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Sequence:    40,
			Message:     hbTeleopArmed,
		}, peer, now)
		if len(out40.Events) != 0 {
			t.Fatalf("unconfirmed single gap seq=40 emitted early recovery event: %+v", out40.Events)
		}
		if cur, _ := tracker.ActuationTarget(1); cur.Authority != uxsvocab.AuthorityDirectCRSFOverride {
			t.Fatalf("single unconfirmed seq=40 flipped authority to %q before confirmation", cur.Authority)
		}
		now = now.Add(100 * time.Millisecond)
		out41 := tracker.IngestFrame(mavcodec.Frame{
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Sequence:    41,
			Message:     hbTeleopArmed,
		}, peer, now)
		if len(out41.Events) != 1 || out41.Events[0].Authority != uxsvocab.AuthorityCompanionTeleop {
			t.Fatalf("confirmed gap seq=40..41 events = %+v, want companion-teleop recovery edge", out41.Events)
		}
		if cur, _ := tracker.ActuationTarget(1); cur.Authority != uxsvocab.AuthorityCompanionTeleop {
			t.Fatalf("confirmed gap seq=40..41 authority = %q, want companion-teleop", cur.Authority)
		}
	})

	t.Run("wrapped_duplicate_disarmed_hb_held_until_corroborated", func(t *testing.T) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		now := t0
		// seq=1 disarmed, seq=2 armed, then telemetry seq=3..255, 0 (full uint8 wrap back to seq=1).
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopDisarmed}, peer, now)
		now = now.Add(10 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopArmed}, peer, now)
		seq := uint8(2)
		for i := 0; i < 254; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}
		armedTarget, _ := tracker.ActuationTarget(1)

		// Delayed network duplicate of seq=1 disarmed heartbeat arrives at wrap boundary -> MUST be held!
		now = now.Add(10 * time.Millisecond)
		outDupWrap := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopDisarmed}, peer, now)
		if len(outDupWrap.Events) != 0 || outDupWrap.UrgentSKGSync {
			t.Fatalf("wrapped duplicate seq=1 disarmed HB emitted %+v, want held", outDupWrap)
		}
		if snap, _ := tracker.Snapshot(1); !snap.Armed {
			t.Fatal("Snapshot(1).Armed flipped false on isolated wrapped duplicate seq=1 disarmed HB")
		}
		if got, _ := tracker.ActuationTarget(1); !got.Armed || got.HeartbeatSeq != armedTarget.HeartbeatSeq || got.HeartbeatOrder != armedTarget.HeartbeatOrder {
			t.Fatalf("ActuationTarget mutated on wrapped duplicate seq=1 HB: got %+v, want %+v", got, armedTarget)
		}

		// Corroborating disarmed HB at seq=2 confirms genuine post-wrap disarmed state!
		now = now.Add(10 * time.Millisecond)
		outCorr := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopDisarmed}, peer, now)
		if len(outCorr.Events) != 1 || outCorr.Events[0].Armed {
			t.Fatalf("corroborated disarmed HB seq=2 outcome = %+v, want disarmed edge", outCorr)
		}
		if snap, _ := tracker.Snapshot(1); snap.Armed {
			t.Fatal("Snapshot(1).Armed stayed true after corroborated seq=2 disarmed HB")
		}
	})
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestTrackerMultiWrapHistoryAndLossPreemption(t *testing.T) {
	peer := netip.MustParseAddrPort("127.0.0.1:45212")
	t0 := time.Date(2026, 11, 27, 23, 0, 0, 0, time.UTC)

	hbTeleopArmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	hbTeleopDisarmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		false,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeDisarmed,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	hbOverrideArmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityDirectCRSFOverride,
		mavcodec.ModeHoldPosition,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)

	t.Run("two_wraps_preserve_prior_disarmed_history_at_seq1", func(t *testing.T) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		now := t0
		// Cycle 0: seq=1 disarmed, seq=2 armed, telemetry seq=3..255, 0.
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopDisarmed}, peer, now)
		now = now.Add(5 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopArmed}, peer, now)
		seq := uint8(2)
		for i := 0; i < 254; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}

		// Cycle 1: seq=1 armed (overwrote single-state slot before fix!), telemetry seq=2..255, 0.
		now = now.Add(5 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopArmed}, peer, now)
		seq = 1
		for i := 0; i < 255; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}

		armedTarget, _ := tracker.ActuationTarget(1)

		// Cycle 2: delayed copy of Cycle 0 (seq=1, Disarmed) arrives -> MUST still be held!
		now = now.Add(10 * time.Millisecond)
		outOld := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopDisarmed}, peer, now)
		if len(outOld.Events) != 0 || outOld.UrgentSKGSync {
			t.Fatalf("two-wrap old (seq=1, Disarmed) emitted %+v, want held", outOld)
		}
		if snap, _ := tracker.Snapshot(1); !snap.Armed {
			t.Fatal("Snapshot(1).Armed flipped false on two-wrap old (seq=1, Disarmed)")
		}
		if got, _ := tracker.ActuationTarget(1); !got.Armed || got.HeartbeatSeq != armedTarget.HeartbeatSeq || got.HeartbeatOrder != armedTarget.HeartbeatOrder {
			t.Fatalf("ActuationTarget mutated on two-wrap old (seq=1, Disarmed): got %+v, want %+v", got, armedTarget)
		}

		// Corroborating disarmed HB at seq=2 confirms genuine disarmed transition!
		now = now.Add(10 * time.Millisecond)
		outCorr := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopDisarmed}, peer, now)
		if len(outCorr.Events) != 1 || outCorr.Events[0].Armed {
			t.Fatalf("corroborated seq=2 disarmed outcome = %+v, want disarmed edge", outCorr)
		}
		if snap, _ := tracker.Snapshot(1); snap.Armed {
			t.Fatal("Snapshot(1).Armed stayed true after corroborated seq=2 disarmed HB")
		}
	})

	t.Run("override_after_199_lost_packets_and_post_wrap_seq1_preempts_immediately", func(t *testing.T) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		now := t0
		// Packets 1..220 in companion-teleop.
		for s := 1; s <= 220; s++ {
			now = now.Add(2 * time.Millisecond)
			var msg mavcodec.Message = mavcodec.AttitudeQuaternion{Q1: 1}
			if s == 1 || s == 164 || s == 220 {
				msg = hbTeleopArmed
			}
			_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: uint8(s), Message: msg}, peer, now)
		}

		// Lose 199 packets (221..255, 0..163); receive consecutive override heartbeats at seq=164 and seq=165.
		now = now.Add(50 * time.Millisecond)
		out164 := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 164, Message: hbOverrideArmed}, peer, now)
		if len(out164.Events) != 1 || out164.Events[0].Authority != uxsvocab.AuthorityDirectCRSFOverride {
			t.Fatalf("seq=164 override after 199 lost packets events = %+v, want immediate direct-crsf-override edge", out164.Events)
		}
		target164, _ := tracker.ActuationTarget(1)
		if target164.Authority != uxsvocab.AuthorityDirectCRSFOverride || target164.HeartbeatOrder != 0 {
			t.Fatalf("target after unconfirmed seq=164 = %+v, want Authority=direct-crsf-override with uncertain HeartbeatOrder=0", target164)
		}

		now = now.Add(20 * time.Millisecond)
		out165 := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 165, Message: hbOverrideArmed}, peer, now)
		if len(out165.Events) != 0 {
			t.Fatalf("corroborating seq=165 override emitted duplicate events: %+v", out165.Events)
		}
		target165, _ := tracker.ActuationTarget(1)
		if target165.Authority != uxsvocab.AuthorityDirectCRSFOverride || target165.HeartbeatOrder == 0 {
			t.Fatalf("target after corroborated seq=165 = %+v, want confirmed HeartbeatOrder>0", target165)
		}

		// Subcase B: override at seq=1, companion at seq=2, telemetry through wrap, genuine override at seq=1.
		trackerWrap, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker wrap: %v", err)
		}
		now = t0
		_ = trackerWrap.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbOverrideArmed}, peer, now)
		now = now.Add(5 * time.Millisecond)
		_ = trackerWrap.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopArmed}, peer, now)
		seq := uint8(2)
		for i := 0; i < 254; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = trackerWrap.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}

		now = now.Add(10 * time.Millisecond)
		outWrapOv := trackerWrap.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbOverrideArmed}, peer, now)
		if len(outWrapOv.Events) != 1 || outWrapOv.Events[0].Authority != uxsvocab.AuthorityDirectCRSFOverride {
			t.Fatalf("post-wrap seq=1 override events = %+v, want immediate direct-crsf-override edge", outWrapOv.Events)
		}
		wrapTarget1, _ := trackerWrap.ActuationTarget(1)
		if wrapTarget1.Authority != uxsvocab.AuthorityDirectCRSFOverride || wrapTarget1.HeartbeatOrder != 0 {
			t.Fatalf("post-wrap seq=1 target = %+v, want Authority=direct-crsf-override with uncertain HeartbeatOrder=0", wrapTarget1)
		}

		now = now.Add(10 * time.Millisecond)
		_ = trackerWrap.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbOverrideArmed}, peer, now)
		wrapTarget2, _ := trackerWrap.ActuationTarget(1)
		if wrapTarget2.Authority != uxsvocab.AuthorityDirectCRSFOverride || wrapTarget2.HeartbeatOrder == 0 {
			t.Fatalf("post-wrap corroborated seq=2 target = %+v, want HeartbeatOrder>0", wrapTarget2)
		}
	})
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestTrackerWrapHeldTeleopSuspensionHoleLossAndHistoricalRepeatGuard(t *testing.T) {
	peer := netip.MustParseAddrPort("127.0.0.1:45213")
	t0 := time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC)

	hbTeleopArmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	hbTeleopDisarmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		false,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeDisarmed,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	hbOverrideArmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityDirectCRSFOverride,
		mavcodec.ModeHoldPosition,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)

	t.Run("wrap_held_disarmed_suspends_teleop_immediately_without_advancing_heartbeat_order", func(t *testing.T) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		now := t0
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopDisarmed}, peer, now)
		now = now.Add(5 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopArmed}, peer, now)
		seq := uint8(2)
		for i := 0; i < 254; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			var msg mavcodec.Message = mavcodec.AttitudeQuaternion{Q1: 1}
			if seq == 0 {
				msg = hbTeleopArmed
			}
			_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: msg}, peer, now)
		}

		armedTarget, _ := tracker.ActuationTarget(1)
		if armedTarget.TeleopUncertain {
			t.Fatalf("unexpected TeleopUncertain on confirmed armed target: %+v", armedTarget)
		}

		// Post-wrap seq=1 disarmed matches Cycle 0 (seq=1, Disarmed): TeleopUncertain MUST flip true
		// immediately while Armed, HeartbeatSeq, and HeartbeatOrder stay at the confirmed armed baseline.
		now = now.Add(10 * time.Millisecond)
		outHeld := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopDisarmed}, peer, now)
		if len(outHeld.Events) != 0 {
			t.Fatalf("wrap-held seq=1 disarmed emitted unexpected events: %+v", outHeld.Events)
		}
		heldTarget, _ := tracker.ActuationTarget(1)
		if !heldTarget.TeleopUncertain || heldTarget.TeleopUncertainReason != "vehicle_disarmed" {
			t.Fatalf("wrap-held seq=1 disarmed target = %+v, want TeleopUncertain=true reason=vehicle_disarmed", heldTarget)
		}
		if !heldTarget.Armed || heldTarget.HeartbeatSeq != armedTarget.HeartbeatSeq || heldTarget.HeartbeatOrder != armedTarget.HeartbeatOrder {
			t.Fatalf("wrap-held seq=1 disarmed mutated confirmed state: got %+v, want Armed=true seq=%d order=%d",
				heldTarget, armedTarget.HeartbeatSeq, armedTarget.HeartbeatOrder)
		}

		// Corroborating seq=2 disarmed confirms transition and clears TeleopUncertain.
		now = now.Add(10 * time.Millisecond)
		outCorr := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopDisarmed}, peer, now)
		if len(outCorr.Events) != 1 || outCorr.Events[0].Armed {
			t.Fatalf("corroborating seq=2 disarmed outcome = %+v, want disarmed edge", outCorr)
		}
		confirmedTarget, _ := tracker.ActuationTarget(1)
		if confirmedTarget.Armed || confirmedTarget.TeleopUncertain || confirmedTarget.TeleopUncertainReason != "" {
			t.Fatalf("confirmed target after seq=2 disarmed = %+v, want Armed=false TeleopUncertain=false", confirmedTarget)
		}
	})

	t.Run("loss_199_over_prior_holes_and_over_matching_prior_override_slot", func(t *testing.T) {
		// Subcase A: seq=164 and seq=165 were DROPPED holes in Cycle 0; lose 199 packets -> seq=164 override MUST preempt immediately!
		trackerHoles, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker holes: %v", err)
		}
		now := t0
		for s := 1; s <= 220; s++ {
			if s == 164 || s == 165 {
				continue
			}
			now = now.Add(2 * time.Millisecond)
			var msg mavcodec.Message = mavcodec.AttitudeQuaternion{Q1: 1}
			if s == 1 || s == 220 {
				msg = hbTeleopArmed
			}
			_ = trackerHoles.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: uint8(s), Message: msg}, peer, now)
		}
		now = now.Add(40 * time.Millisecond)
		outHole164 := trackerHoles.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 164, Message: hbOverrideArmed}, peer, now)
		if len(outHole164.Events) != 1 || outHole164.Events[0].Authority != uxsvocab.AuthorityDirectCRSFOverride {
			t.Fatalf("seq=164 override over Cycle 0 hole events = %+v, want immediate direct-crsf-override edge", outHole164.Events)
		}
		tgtHole164, _ := trackerHoles.ActuationTarget(1)
		if tgtHole164.Authority != uxsvocab.AuthorityDirectCRSFOverride || tgtHole164.HeartbeatOrder != 0 {
			t.Fatalf("target after seq=164 over hole = %+v, want Authority=direct-crsf-override HeartbeatOrder=0", tgtHole164)
		}
		now = now.Add(20 * time.Millisecond)
		_ = trackerHoles.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 165, Message: hbOverrideArmed}, peer, now)
		tgtHole165, _ := trackerHoles.ActuationTarget(1)
		if tgtHole165.Authority != uxsvocab.AuthorityDirectCRSFOverride || tgtHole165.HeartbeatOrder == 0 {
			t.Fatalf("target after seq=165 over hole = %+v, want confirmed HeartbeatOrder>0", tgtHole165)
		}

		// Subcase B: seq=164 WAS DirectCRSFOverride in Cycle 0 before seq=220 restored CompanionTeleop;
		// after 199 lost packets, seq=164 override matches recentHbFrame[164] -> TeleopUncertain MUST suspend teleop immediately!
		trackerDupSlot, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker dup slot: %v", err)
		}
		now = t0
		for s := 1; s <= 220; s++ {
			now = now.Add(2 * time.Millisecond)
			var msg mavcodec.Message = mavcodec.AttitudeQuaternion{Q1: 1}
			switch s {
			case 1, 220:
				msg = hbTeleopArmed
			case 164:
				msg = hbOverrideArmed
			}
			_ = trackerDupSlot.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: uint8(s), Message: msg}, peer, now)
		}
		now = now.Add(40 * time.Millisecond)
		outDup164 := trackerDupSlot.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 164, Message: hbOverrideArmed}, peer, now)
		if len(outDup164.Events) != 1 || outDup164.Events[0].Authority != uxsvocab.AuthorityDirectCRSFOverride {
			t.Fatalf("seq=164 override matching prior override slot events = %+v, want immediate direct-crsf-override edge", outDup164.Events)
		}
		tgtDup164, _ := trackerDupSlot.ActuationTarget(1)
		if tgtDup164.Authority != uxsvocab.AuthorityDirectCRSFOverride || tgtDup164.HeartbeatOrder != 0 {
			t.Fatalf("target after seq=164 matching prior override slot = %+v, want Authority=direct-crsf-override HeartbeatOrder=0", tgtDup164)
		}
		now = now.Add(20 * time.Millisecond)
		outDup165 := trackerDupSlot.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 165, Message: hbOverrideArmed}, peer, now)
		if len(outDup165.Events) != 0 {
			t.Fatalf("corroborating seq=165 override emitted duplicate events = %+v", outDup165.Events)
		}
		tgtDup165, _ := trackerDupSlot.ActuationTarget(1)
		if tgtDup165.Authority != uxsvocab.AuthorityDirectCRSFOverride || tgtDup165.TeleopUncertain || tgtDup165.HeartbeatOrder == 0 {
			t.Fatalf("target after corroborated seq=165 = %+v, want confirmed DirectCRSFOverride with TeleopUncertain=false", tgtDup165)
		}
	})

	t.Run("multiple_historical_disarmed_heartbeats_never_mutually_corroborate_after_wrap", func(t *testing.T) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		now := t0
		// Cycle 0: BOTH seq=1 and seq=2 are Disarmed; seq=3 is Armed; telemetry runs through wrap.
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopDisarmed}, peer, now)
		now = now.Add(5 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopDisarmed}, peer, now)
		now = now.Add(5 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 3, Message: hbTeleopArmed}, peer, now)
		seq := uint8(3)
		for i := 0; i < 253; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}
		armedBase, _ := tracker.ActuationTarget(1)

		// Delayed historical copies of BOTH (seq=1, Disarmed) AND (seq=2, Disarmed) arrive after wrap:
		// NEITHER may commit Disarmed or advance HeartbeatOrder!
		now = now.Add(10 * time.Millisecond)
		outDup1 := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopDisarmed}, peer, now)
		now = now.Add(10 * time.Millisecond)
		outDup2 := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopDisarmed}, peer, now)
		if len(outDup1.Events) != 0 || len(outDup2.Events) != 0 {
			t.Fatalf("delayed historical seq=1,2 disarmed copies corroborated each other: ev1=%+v ev2=%+v", outDup1.Events, outDup2.Events)
		}
		afterDups, _ := tracker.ActuationTarget(1)
		if !afterDups.Armed || afterDups.HeartbeatOrder != armedBase.HeartbeatOrder || !afterDups.TeleopUncertain {
			t.Fatalf("target after delayed seq=1,2 copies = %+v, want Armed=true HeartbeatOrder=%d TeleopUncertain=true",
				afterDups, armedBase.HeartbeatOrder)
		}

		// A genuine fresh Disarmed at seq=3 (which was Armed in Cycle 0, NOT Disarmed!) confirms immediately.
		now = now.Add(10 * time.Millisecond)
		outFresh3 := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 3, Message: hbTeleopDisarmed}, peer, now)
		if len(outFresh3.Events) != 1 || outFresh3.Events[0].Armed {
			t.Fatalf("fresh seq=3 disarmed outcome = %+v, want disarmed edge", outFresh3)
		}
		confirmed, _ := tracker.ActuationTarget(1)
		if confirmed.Armed || confirmed.TeleopUncertain || confirmed.HeartbeatOrder <= armedBase.HeartbeatOrder {
			t.Fatalf("target after fresh seq=3 disarmed = %+v, want Armed=false TeleopUncertain=false HeartbeatOrder>%d",
				confirmed, armedBase.HeartbeatOrder)
		}
	})
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestTrackerGapAndWrapSuspensionPersistenceAndRegularCadenceRecovery(t *testing.T) {
	peer := netip.MustParseAddrPort("127.0.0.1:45214")
	t0 := time.Date(2026, 11, 28, 2, 0, 0, 0, time.UTC)

	hbTeleopArmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	hbTeleopDisarmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		false,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeDisarmed,
		mavcodec.LinkTierSITLLocal,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)
	hbSupervisoryArmed := makeVehicleHB(
		mavcodec.DomainQuadcopter,
		true,
		mavcodec.AuthorityCompanionTeleop,
		mavcodec.ModeTeleopVelocity,
		mavcodec.LinkTierMeshSupervisory,
		uxsvocab.HealthFlags{CompanionTrunkHealthy: true},
	)

	t.Run("gap_disarmed_suspension_survives_delayed_attitude_until_fresh_heartbeat", func(t *testing.T) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		now := t0
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopArmed}, peer, now)
		for seq := uint8(2); seq <= 10; seq++ {
			now = now.Add(5 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}

		// Genuine disarmed heartbeat opens a sequence gap (seq=91, delta=81) and suspends motion.
		now = now.Add(20 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 91, Message: hbTeleopDisarmed}, peer, now)
		if tgt, _ := tracker.ActuationTarget(1); !tgt.TeleopUncertain || tgt.TeleopUncertainReason != "vehicle_disarmed" {
			t.Fatalf("gap disarmed target = %+v, want TeleopUncertain=true reason=vehicle_disarmed", tgt)
		}

		// Delayed attitude packet at seq=11 (normalDelta=1) MUST NOT clear motion suspension.
		now = now.Add(5 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 11, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		if tgt, _ := tracker.ActuationTarget(1); !tgt.TeleopUncertain || tgt.TeleopUncertainReason != "vehicle_disarmed" {
			t.Fatalf("delayed attitude seq=11 cleared motion suspension: %+v", tgt)
		}

		// Backward-reordered heartbeat from before seq=10 (seq=9) MUST NOT clear motion suspension either.
		now = now.Add(5 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 9, Message: hbTeleopArmed}, peer, now)
		if tgt, _ := tracker.ActuationTarget(1); !tgt.TeleopUncertain || tgt.TeleopUncertainReason != "vehicle_disarmed" {
			t.Fatalf("backward-reordered armed heartbeat seq=9 cleared motion suspension: %+v", tgt)
		}

		// Genuinely fresh unseen armed heartbeat at seq=12 clears suspension.
		now = now.Add(10 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 12, Message: hbTeleopArmed}, peer, now)
		if tgt, _ := tracker.ActuationTarget(1); tgt.TeleopUncertain || !tgt.Armed {
			t.Fatalf("fresh unseen armed heartbeat seq=12 did not clear suspension: %+v", tgt)
		}
	})

	t.Run("wrap_disarmed_suspension_survives_copied_earlier_armed_heartbeat", func(t *testing.T) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		now := t0
		// Cycle 0: seq=1 disarmed, seq=2 armed, seq=3 armed, telemetry through wrap.
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopDisarmed}, peer, now)
		now = now.Add(5 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopArmed}, peer, now)
		now = now.Add(5 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 3, Message: hbTeleopArmed}, peer, now)
		seq := uint8(3)
		for i := 0; i < 253; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}
		armedBase, _ := tracker.ActuationTarget(1)

		// Post-wrap genuine disarmed heartbeat at seq=1 suspends motion.
		now = now.Add(10 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopDisarmed}, peer, now)
		if tgt, _ := tracker.ActuationTarget(1); !tgt.TeleopUncertain || tgt.TeleopUncertainReason != "vehicle_disarmed" {
			t.Fatalf("post-wrap seq=1 disarmed target = %+v, want TeleopUncertain=true", tgt)
		}

		// Copy of earlier armed heartbeat at seq=2 MUST NOT clear TeleopUncertain or advance HeartbeatOrder.
		now = now.Add(10 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 2, Message: hbTeleopArmed}, peer, now)
		afterCopiedArmed, _ := tracker.ActuationTarget(1)
		if !afterCopiedArmed.TeleopUncertain || afterCopiedArmed.TeleopUncertainReason != "vehicle_disarmed" ||
			afterCopiedArmed.HeartbeatOrder != armedBase.HeartbeatOrder {
			t.Fatalf("copied Cycle 0 armed heartbeat cleared suspension or advanced order: got %+v, base %+v",
				afterCopiedArmed, armedBase)
		}

		// Fresh unseen armed heartbeat at seq=1 (never armed in Cycle 0) clears suspension.
		now = now.Add(10 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopArmed}, peer, now)
		if cleared, _ := tracker.ActuationTarget(1); cleared.TeleopUncertain || cleared.HeartbeatOrder <= armedBase.HeartbeatOrder {
			t.Fatalf("fresh unseen armed heartbeat seq=1 did not clear suspension: %+v", cleared)
		}
	})

	t.Run("regular_1hz_heartbeat_31hz_kinematics_cadence_recovers_from_populated_slots", func(t *testing.T) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		now := t0
		var seq uint8
		emitSecond := func(hb mavcodec.Heartbeat) IngestOutcome {
			seq++
			now = now.Add(31 * time.Millisecond)
			out := tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: hb}, peer, now)
			for k := 0; k < 31; k++ {
				seq++
				now = now.Add(31 * time.Millisecond)
				_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
			}
			return out
		}

		// 8 disarmed heartbeats + 8 armed heartbeats populate all 8 fixed cadence slots.
		for i := 0; i < 8; i++ {
			_ = emitSecond(hbTeleopDisarmed)
		}
		for i := 0; i < 8; i++ {
			_ = emitSecond(hbTeleopArmed)
		}
		if snap, _ := tracker.Snapshot(1); !snap.Armed {
			t.Fatal("expected Snapshot(1).Armed == true after 8 armed seconds")
		}

		// Delayed historical copies (1st and 2nd disarmed heartbeats) remain held, while sustained genuine
		// disarmed telemetry confirms Disarmed on the 4th consecutive heartbeat (well under 64 seconds).
		confirmedSecond := -1
		for sec := 1; sec <= 64; sec++ {
			out := emitSecond(hbTeleopDisarmed)
			snap, _ := tracker.Snapshot(1)
			if sec <= 2 && snap.Armed == false {
				t.Fatalf("delayed historical copy count=%d confirmed Disarmed too early", sec)
			}
			if !snap.Armed && confirmedSecond < 0 {
				confirmedSecond = sec
				if len(out.Events) == 0 || out.Events[0].Armed {
					t.Fatalf("confirming second %d did not emit disarmed TransitionEvent: %+v", sec, out.Events)
				}
			}
		}
		if confirmedSecond != maxConsecutiveHeldHeartbeats {
			t.Fatalf("confirmedSecond = %d, want %d consecutive heartbeats on regular 32 pkt/s cadence",
				confirmedSecond, maxConsecutiveHeldHeartbeats)
		}
		if snap, _ := tracker.Snapshot(1); snap.Armed {
			t.Fatal("Snapshot(1).Armed remained true after sustained genuine disarmed heartbeats")
		}
	})

	t.Run("uncertain_supervisory_heartbeat_propagates_uncertain_link_tier", func(t *testing.T) {
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		now := t0
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 1, Message: hbTeleopArmed}, peer, now)
		now = now.Add(20 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 90, Message: hbSupervisoryArmed}, peer, now)
		tgt, ok := tracker.ActuationTarget(1)
		if !ok || !tgt.TeleopUncertain || tgt.TeleopUncertainReason != "supervisory_link_only" || tgt.UncertainLinkTier != uxsvocab.LinkTierMeshSupervisory {
			t.Fatalf("uncertain supervisory target = %+v (ok=%v), want TeleopUncertain=true UncertainLinkTier=mesh-supervisory", tgt, ok)
		}
	})
}
