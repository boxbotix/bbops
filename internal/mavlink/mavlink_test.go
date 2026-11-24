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
			if s.telemetryPrefix != DefaultTelemetryPrefix || s.entitySubject != DefaultEntitySubject {
				t.Fatalf("default subjects = (%q, %q)", s.telemetryPrefix, s.entitySubject)
			}
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
		if out.Accepted || out.CountedError || out.Telemetry != nil || out.Event != nil || out.UrgentSKGSync {
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
	if !admitOut.Accepted || admitOut.CountedError || admitOut.Event != nil || admitOut.UrgentSKGSync || admitOut.Telemetry != nil {
		t.Fatalf("admission HEARTBEAT outcome = %+v", admitOut)
	}
	if gotPeer, ok := tracker.PeerAddr(2); !ok || gotPeer != peerA {
		t.Fatalf("PeerAddr(2) = (%v, %v), want (%v, true)", gotPeer, ok, peerA)
	}

	// Identical HEARTBEAT from rotated peerB updates PeerAddr without emitting an edge event.
	steadyOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 4, validRoverHB), peerB, t0.Add(time.Second))
	if !steadyOut.Accepted || steadyOut.Event != nil || steadyOut.UrgentSKGSync {
		t.Fatalf("steady HEARTBEAT outcome = %+v", steadyOut)
	}
	if gotPeer, ok := tracker.PeerAddr(2); !ok || gotPeer != peerB {
		t.Fatalf("PeerAddr(2) after rotation = (%v, %v), want (%v, true)", gotPeer, ok, peerB)
	}

	// 4. Heading fallback (#33 before #31) and quaternion precedence (#31 overrides #33.Hdg).
	gpUnknownHdg := mavcodec.GlobalPositionInt{
		Lat: 377749000,
		Lon: -1224194000,
		Alt: 12345,
		Hdg: 65535, // UINT16_MAX = unknown
	}
	gpOut1 := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 5, gpUnknownHdg), peerB, t0.Add(2*time.Second))
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
	gpOutZero := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 5, gpZeroNorthHdg), peerB, t0.Add(2500*time.Millisecond))
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
	gpOut2 := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 6, gpFallbackHdg), peerB, t0.Add(3*time.Second))
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
	gpOutLostHdg := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 6, gpUnknownHdg), peerB, tLostHdg)
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
	if hbWhileUnknownOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 6, hbSteadyWhileUnknown), peerB, tHealthWhileUnknown); !hbWhileUnknownOut.Accepted {
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
	gpOutRestoredNorth := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 6, gpZeroNorthHdg), peerB, tRestoredNorth)
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
	attOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 7, attEast), peerB, t0.Add(4*time.Second))
	if !attOut.Accepted || attOut.Telemetry == nil || !attOut.Telemetry.HasAttitude || attOut.Telemetry.HasLocalPos ||
		math.Abs(attOut.Telemetry.HdgDeg-90.0) > 1e-4 || !attOut.Telemetry.HasGlobalPos {
		t.Fatalf("ATTITUDE_QUATERNION East outcome = %+v", attOut.Telemetry)
	}

	// Subsequent #33 with Hdg=18000 must NOT overwrite #31's 90.0 deg heading!
	gpSouthHdg := mavcodec.GlobalPositionInt{Lat: -900_000_000, Lon: 1_800_000_000, Alt: -500, Hdg: 18000}
	gpOut3 := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 8, gpSouthHdg), peerB, t0.Add(5*time.Second))
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
	}), peerB, t0.Add(6*time.Second))
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
		if out.Accepted || !out.CountedError || out.Telemetry != nil || out.Event != nil || out.UrgentSKGSync {
			t.Fatalf("bad payload %d (%T) outcome = %+v, want rejected", i, badMsg, out)
		}
	}
	snapAfterBad, _ := tracker.Snapshot(2)
	if snapAfterBad != snapBeforeBad {
		t.Fatalf("sanitization failure mutated snapshot:\nbefore=%+v\nafter =%+v", snapBeforeBad, snapAfterBad)
	}
	if gotPeer, _ := tracker.PeerAddr(2); gotPeer != peerB {
		t.Fatalf("rejected packet overwrote PeerAddr: got %v, want %v", gotPeer, peerB)
	}

	// 6. Lane 3 edge transitions: HEARTBEAT preemption, #1 SYS_STATUS failsafe increment, #253 STATUSTEXT.
	preemptHB := makeVehicleHB(
		mavcodec.DomainRover,
		true,
		mavcodec.AuthorityDirectCRSFOverride,
		mavcodec.ModeAutonomousVelocity,
		mavcodec.LinkTierMeshHighRate,
		uxsvocab.HealthFlags{TeleopDeadmanClamped: true},
	)
	hbEdge := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 40, preemptHB), peerB, t0.Add(8*time.Second))
	if !hbEdge.Accepted || !hbEdge.UrgentSKGSync || hbEdge.Event == nil {
		t.Fatalf("HEARTBEAT preemption outcome = %+v", hbEdge)
	}
	if hbEdge.Event.Kind != EventKindHeartbeatEdge || hbEdge.Event.PrevAuthority != uxsvocab.AuthorityMCUFailsafe ||
		hbEdge.Event.Authority != uxsvocab.AuthorityDirectCRSFOverride {
		t.Fatalf("HEARTBEAT edge event = %+v", hbEdge.Event)
	}
	wantReasons := []string{"armed_changed", "authority_changed", "companion_mode_changed", "link_tier_changed", "health_flags_changed"}
	if !slices.Equal(hbEdge.Event.Reasons, wantReasons) {
		t.Fatalf("HEARTBEAT edge reasons = %v, want %v", hbEdge.Event.Reasons, wantReasons)
	}

	// #1 SYS_STATUS: initial nonzero baseline (7) and steady sample (7) update snapshot without a false alert;
	// strict increment (7 -> 8) emits exactly one failsafe_increment edge; decrement (8 -> 6) updates without edge.
	sysBaseline := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 41, mavcodec.SysStatus{
		Load: 1200, ErrorsComm: 3, ErrorsCount1: 7, ErrorsCount2: 1850, ErrorsCount3: 900,
	}), peerB, t0.Add(9*time.Second))
	if !sysBaseline.Accepted || sysBaseline.UrgentSKGSync || sysBaseline.Event != nil {
		t.Fatalf("SYS_STATUS initial baseline(7) outcome = %+v, want no false failsafe alert", sysBaseline)
	}
	if snap, _ := tracker.Snapshot(2); snap.Failsafes != 7 || snap.LoopDtUs != 1200 {
		t.Fatalf("SYS_STATUS initial baseline(7) snapshot = %+v, want Failsafes=7 LoopDtUs=1200", snap)
	}

	sysSteady := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 42, mavcodec.SysStatus{
		Load: 1100, ErrorsComm: 4, ErrorsCount1: 7, ErrorsCount2: 1850, ErrorsCount3: 850,
	}), peerB, t0.Add(10*time.Second))
	if !sysSteady.Accepted || sysSteady.UrgentSKGSync || sysSteady.Event != nil {
		t.Fatalf("SYS_STATUS steady(7->7) outcome = %+v, want no edge event", sysSteady)
	}
	if snap, _ := tracker.Snapshot(2); snap.Failsafes != 7 || snap.LoopDtUs != 1100 {
		t.Fatalf("SYS_STATUS steady(7->7) snapshot = %+v, want Failsafes=7 LoopDtUs=1100", snap)
	}

	sysInc := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 43, mavcodec.SysStatus{
		Load: 1050, ErrorsComm: 5, ErrorsCount1: 8, ErrorsCount2: 1900, ErrorsCount3: 820,
	}), peerB, t0.Add(11*time.Second))
	if !sysInc.Accepted || !sysInc.UrgentSKGSync || sysInc.Event == nil || sysInc.Event.Kind != EventKindFailsafeIncrement || sysInc.Event.Failsafes != 8 {
		t.Fatalf("SYS_STATUS increment(7->8) outcome = %+v, want failsafe_increment(8)", sysInc)
	}
	if snap, _ := tracker.Snapshot(2); snap.Failsafes != 8 || snap.LoopDtUs != 1050 {
		t.Fatalf("SYS_STATUS increment(7->8) snapshot = %+v, want Failsafes=8 LoopDtUs=1050", snap)
	}

	sysDec := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 44, mavcodec.SysStatus{
		Load: 1000, ErrorsComm: 5, ErrorsCount1: 6, ErrorsCount2: 1900, ErrorsCount3: 800,
	}), peerB, t0.Add(12*time.Second))
	if !sysDec.Accepted || sysDec.UrgentSKGSync || sysDec.Event != nil {
		t.Fatalf("SYS_STATUS decrement(8->6) outcome = %+v, want no edge event", sysDec)
	}
	if snap, _ := tracker.Snapshot(2); snap.Failsafes != 6 || snap.LoopDtUs != 1000 {
		t.Fatalf("SYS_STATUS decrement(8->6) snapshot = %+v, want Failsafes=6 LoopDtUs=1000", snap)
	}

	// #253 STATUSTEXT over wire + NUL-padded IngestFrame triggers immediate Lane 3 edge + SKG flush.
	stOut := tracker.IngestDatagram(encodeFrameBytes(t, 2, 1, 45, mavcodec.StatusText{
		Severity: 2,
		Text:     "CRSF OVERRIDE ENGAGED",
		ID:       7,
		ChunkSeq: 0,
	}), peerB, t0.Add(13*time.Second))
	if !stOut.Accepted || !stOut.UrgentSKGSync || stOut.Event == nil || stOut.Event.Kind != EventKindStatusText ||
		stOut.Event.StatusText == nil || stOut.Event.StatusText.Text != "CRSF OVERRIDE ENGAGED" {
		t.Fatalf("STATUSTEXT wire outcome = %+v", stOut)
	}
	stNulOut := tracker.IngestFrame(mavcodec.Frame{
		SystemID: 2, ComponentID: 1, Sequence: 46,
		Message: mavcodec.StatusText{Severity: 4, Text: "PADDED\x00\x00"},
	}, peerB, t0.Add(14*time.Second))
	if !stNulOut.Accepted || stNulOut.Event == nil || stNulOut.Event.StatusText.Text != "PADDED" {
		t.Fatalf("STATUSTEXT NUL-trim outcome = %+v", stNulOut)
	}

	// Verify #0 HEARTBEAT (t0+8s), #1 SYS_STATUS (t0+9..12s), and #253 STATUSTEXT (t0+13..14s)
	// advanced health/status ObservedAt to t0+14s WITHOUT advancing #32/#33 kinematic triple timestamps!
	finalSnap, ok := tracker.Snapshot(2)
	if !ok {
		t.Fatal("missing final sysid=2 snapshot")
	}
	finalTriples := finalSnap.Triples()
	byPred := make(map[string]message.Triple, len(finalTriples))
	for _, tr := range finalTriples {
		byPred[tr.Predicate] = tr
	}
	if !byPred[uxsvocab.PredicateHealthArmed].Timestamp.Equal(t0.Add(14 * time.Second)) {
		t.Fatalf("health triple timestamp = %v, want %v", byPred[uxsvocab.PredicateHealthArmed].Timestamp, t0.Add(14*time.Second))
	}
	if !byPred[uxsvocab.PredicateKinematicsNED].Timestamp.Equal(t0.Add(6*time.Second)) ||
		byPred[uxsvocab.PredicateKinematicsNED].Context != "measured" {
		t.Fatalf("NED triple after health/status updates = %+v, want Timestamp=%v Context=measured",
			byPred[uxsvocab.PredicateKinematicsNED], t0.Add(6*time.Second))
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

	// Admit sysid=1 (Quadcopter) and sysid=2 (Rover).
	hb1 := makeVehicleHB(mavcodec.DomainQuadcopter, false, mavcodec.AuthorityMCUFailsafe, mavcodec.ModeDisarmed, mavcodec.LinkTierSITLLocal, uxsvocab.HealthFlags{})
	hb2 := makeVehicleHB(mavcodec.DomainRover, false, mavcodec.AuthorityMCUFailsafe, mavcodec.ModeDisarmed, mavcodec.LinkTierSITLLocal, uxsvocab.HealthFlags{})
	tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: 1, Sequence: 1, Message: hb1}, peer, t0)
	tracker.IngestFrame(mavcodec.Frame{SystemID: 2, ComponentID: 1, Sequence: 1, Message: hb2}, peer, t0)

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

	// Enqueue empty batch (no-op), then batch #0 which stalls inside publishBatch.
	pub.EnqueuePeriodic(nil)
	b0, err := tracker.BuildPeriodicBatch()
	if err != nil {
		t.Fatalf("BuildPeriodicBatch b0: %v", err)
	}
	pub.EnqueuePeriodic(b0)

	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first batch to enter publishBatch")
	}

	// While publishBatch is stalled, enqueue 5 periodic batches (LoopDtUs = 100..500) without blocking!
	enqueueStart := time.Now()
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
		batch, bErr := tracker.BuildPeriodicBatch()
		if bErr != nil {
			t.Fatalf("BuildPeriodicBatch step %d: %v", step, bErr)
		}
		pub.EnqueuePeriodic(batch)
	}
	if elapsed := time.Since(enqueueStart); elapsed > 100*time.Millisecond {
		t.Fatalf("EnqueuePeriodic blocked under stalled publisher (%v)", elapsed)
	}
	if gotDropped := pub.DroppedPeriodic(); gotDropped != 3 {
		t.Fatalf("DroppedPeriodic = %d, want 3 (5 queued into cap-%d channel)", gotDropped, PeriodicQueueCap)
	}

	// Now trigger a Lane 3 urgent edge ONLY on sysid=1 (Armed=true, Authority=DirectCRSFOverride, LoopDtUs stays 500)
	// AFTER periodic batches 4 & 5 were already snapshotted into periodicCh!
	edgeHB := makeVehicleHB(mavcodec.DomainQuadcopter, true, mavcodec.AuthorityDirectCRSFOverride, mavcodec.ModeHoldPosition, mavcodec.LinkTierMeshHighRate, uxsvocab.HealthFlags{TeleopDeadmanClamped: true})
	tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: 1, Sequence: 99, Message: edgeHB}, peer, t0.Add(10*time.Second))
	pub.NotifyUrgent(1)

	// Release batch #0 and wait for urgent + remaining cap-2 periodic batches to flush.
	close(releaseFirst)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 3 {
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
	if len(gotCalls) < 3 {
		t.Fatalf("recorded publishBatch calls = %d, want >= 3", len(gotCalls))
	}

	// Call 1 (immediately after stalled call 0) MUST be the preempting urgent flush for sysid=1!
	if len(gotCalls[1].snaps) != 1 || gotCalls[1].snaps[0].SysID != 1 || !gotCalls[1].snaps[0].Armed ||
		gotCalls[1].snaps[0].Authority != uxsvocab.AuthorityDirectCRSFOverride {
		t.Fatalf("call[1] = %+v, want urgent sysid=1 %s snapshot ahead of periodic queue", gotCalls[1].snaps, uxsvocab.AuthorityDirectCRSFOverride)
	}

	// Subsequent periodic drains MUST NOT regress sysid=1 back to Armed=false/MCUFailsafe (rev 5/6 < urgent rev 7),
	// while sysid=2 MUST advance to LoopDtUs=500!
	latestBySysID := map[uint8]uxsvocab.VehicleSnapshot{}
	for idx, c := range gotCalls {
		for _, s := range c.snaps {
			if idx > 1 && s.SysID == 1 && !s.Armed {
				t.Fatalf("stale periodic batch overwrote newer urgent edge state at call[%d]: %+v", idx, s)
			}
			latestBySysID[s.SysID] = s
		}
	}
	if latestBySysID[1].Authority != uxsvocab.AuthorityDirectCRSFOverride || !latestBySysID[1].Armed {
		t.Fatalf("final sysid=1 state = %+v, want Armed %s", latestBySysID[1], uxsvocab.AuthorityDirectCRSFOverride)
	}
	if latestBySysID[2].LoopDtUs != 500 {
		t.Fatalf("final sysid=2 LoopDtUs = %d, want 500 from newest retained periodic batch", latestBySysID[2].LoopDtUs)
	}

	// Verify publishBatch and buildUrgent errors invoke onError without panicking.
	var failCount atomic.Int32
	failPub := NewPublisher(
		DefaultEntitySubject,
		func(_ context.Context, _ string, _ [][]byte) error {
			return errors.New("jetstream publish timeout")
		},
		func(_ []uint8) ([]SnapshotBatchItem, error) {
			return nil, errors.New("build urgent failure")
		},
		func() { failCount.Add(1) },
	)
	failPub.NotifyUrgent(1)
	failPub.EnqueuePeriodic(b0)
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
	failCancel()
	<-failDone
	if got := failCount.Load(); got != 2 {
		t.Fatalf("onError count = %d, want 2 (1 urgent build error + 1 periodic publish error)", got)
	}

	// Verify partial urgent batch failure (`PublishBatchToStream` acknowledging item 0 and failing on item 1):
	// 1. Acknowledged sysid=1 (rev=2) MUST NOT regress to [new, old] when a queued stale periodic batch (rev=1) drains.
	// 2. Equal revisions (rev=2) MUST remain eligible for recovery so unacknowledged sysid=2 (rev=2) recovers on retry.
	partialTracker, err := NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker partial: %v", err)
	}
	partialTracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: 1, Sequence: 1, Message: hb1}, peer, t0)
	partialTracker.IngestFrame(mavcodec.Frame{SystemID: 2, ComponentID: 1, Sequence: 1, Message: hb2}, peer, t0)
	staleRev1Batch, err := partialTracker.BuildPeriodicBatch()
	if err != nil {
		t.Fatalf("BuildPeriodicBatch staleRev1: %v", err)
	}

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
		func() { partialErrors.Add(1) },
	)

	// Queue stale rev=1 periodic batch BEFORE urgent rev=2 notifications, then start worker.
	partialPub.EnqueuePeriodic(staleRev1Batch)
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
	// Give worker time to drain staleRev1Batch from periodicCh after the failed urgent batch.
	time.Sleep(25 * time.Millisecond)

	// Now enqueue an equal-revision (rev=2) periodic batch to verify recovery of unacknowledged sysid=2.
	recoveryRev2Batch, err := partialTracker.BuildPeriodicBatch()
	if err != nil {
		t.Fatalf("BuildPeriodicBatch recoveryRev2: %v", err)
	}
	partialPub.EnqueuePeriodic(recoveryRev2Batch)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		partialMu.Lock()
		recovered := len(deliveredArmedBySysID[2]) >= 1
		partialMu.Unlock()
		if recovered {
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
		t.Fatalf("unacknowledged sysid=2 recovery publications = %v, want [true] from equal-rev retry", sys2History)
	}
}

// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
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
		admitted     bool
		domain       string
		armed        bool
		authority    string
		mode         string
		tier         string
		flags        uxsvocab.HealthFlags
		hasSysStatus bool
		failsafes    int
		hasAttitude  bool
		hasGlobalPos bool
		hasHeading   bool
		hdgDeg       float64
		latLonAlt    [3]float64
		nedXYZ       [3]float64
		rev          uint64
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
		sawAdmission         int
		sawSysStatusBaseline int
		sawQuatOverride      int
		sawHdgFallback       int
		sawHdgInvalidation   int
		sawHeartbeatEdge     int
		sawFailsafeEdge      int
		sawStatusText        int
		sawSanitizedReject   int
		sawDroppedPeriodic   int
	)

	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		tracker, err := NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		ref := map[uint8]*refVehicle{
			1: {},
			2: {},
			3: {},
		}

		var publishedMu sync.Mutex
		materialized := map[uint8]uxsvocab.VehicleSnapshot{}
		mergedTriples := map[uint8][]message.Triple{}
		lastObservedAt := map[uint8]time.Time{}

		pub := NewPublisher(
			DefaultEntitySubject,
			func(_ context.Context, _ string, msgs [][]byte) error {
				publishedMu.Lock()
				defer publishedMu.Unlock()
				for _, raw := range msgs {
					bm, uErr := decoder.Decode(raw)
					if uErr != nil {
						t.Fatalf("seed %d: decoder.Decode: %v", seed, uErr)
					}
					snap := *bm.Payload().(*uxsvocab.VehicleSnapshot)
					if prevTS, ok := lastObservedAt[snap.SysID]; ok && snap.ObservedAt.Before(prevTS) {
						t.Fatalf("seed %d sysid=%d: monotonic ObservedAt regressed (%v < %v)", seed, snap.SysID, snap.ObservedAt, prevTS)
					}
					lastObservedAt[snap.SysID] = snap.ObservedAt
					materialized[snap.SysID] = snap
					rep := graph.ReplaceBySource(mergedTriples[snap.SysID], snap.Triples())
					if len(rep.Stale) != 0 {
						t.Fatalf("seed %d sysid=%d: ReplaceBySource rejected triples as stale: %+v", seed, snap.SysID, rep.Stale)
					}
					mergedTriples[snap.SysID] = rep.Triples
				}
				return nil
			},
			tracker.BuildUrgentBatch,
			func() { t.Errorf("seed %d: unexpected publisher error", seed) },
		)

		now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		for step := range 60 {
			now = now.Add(50 * time.Millisecond)
			sysid := uint8(1 + rng.IntN(3))
			peer := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(40000+int(sysid)*10+rng.IntN(5)))
			rv := ref[sysid]

			switch rng.IntN(7) {
			case 0: // #0 HEARTBEAT
				dom := domains[int(sysid-1)%len(domains)]
				armed := rng.IntN(2) == 1
				auth := authorities[rng.IntN(len(authorities))]
				mode := modes[rng.IntN(len(modes))]
				tier := tiers[rng.IntN(len(tiers))]
				flags := uxsvocab.HealthFlags{TeleopDeadmanClamped: rng.IntN(3) == 0}
				hb := makeVehicleHB(dom, armed, auth, mode, tier, flags)

				out := tracker.IngestFrame(mavcodec.Frame{SystemID: sysid, ComponentID: 1, Sequence: uint8(step), Message: hb}, peer, now)
				if !out.Accepted || out.CountedError {
					t.Fatalf("seed %d step %d: valid HB rejected: %+v", seed, step, out)
				}

				domS, _ := dom.VocabString()
				authS, _ := auth.VocabString()
				modeS, _ := mode.VocabString()
				tierS, _ := tier.VocabString()
				if !rv.admitted {
					rv.admitted = true
					sawAdmission++
					if out.Event != nil || out.UrgentSKGSync {
						t.Fatalf("seed %d step %d: first HB emitted edge event: %+v", seed, step, out)
					}
				} else {
					changed := rv.domain != domS || rv.armed != armed || rv.authority != authS ||
						rv.mode != modeS || rv.tier != tierS || rv.flags != flags
					if changed != (out.Event != nil && out.UrgentSKGSync) {
						t.Fatalf("seed %d step %d: HB edge mismatch changed=%v out=%+v", seed, step, changed, out)
					}
					if changed {
						sawHeartbeatEdge++
						pub.NotifyUrgent(sysid)
					}
				}
				rv.domain, rv.armed, rv.authority, rv.mode, rv.tier, rv.flags = domS, armed, authS, modeS, tierS, flags
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
				wantEdge := rv.hasSysStatus && int(nextFS) > rv.failsafes
				if wantEdge != (out.Event != nil && out.UrgentSKGSync) {
					t.Fatalf("seed %d step %d: SYS_STATUS edge mismatch want=%v out=%+v", seed, step, wantEdge, out)
				}
				if !rv.hasSysStatus && nextFS > 0 {
					sawSysStatusBaseline++
				}
				if wantEdge {
					sawFailsafeEdge++
					pub.NotifyUrgent(sysid)
				}
				rv.hasSysStatus = true
				rv.failsafes = int(nextFS)
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
				if rv.hasGlobalPos {
					sawQuatOverride++
				}
				rv.hasAttitude = true
				rv.hasHeading = true
				rv.hdgDeg = wantYaw
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
				rv.nedXYZ = [3]float64{float64(x), 5, -10}
				rv.rev++
				if out.Telemetry == nil || out.Telemetry.NEDXYZ != rv.nedXYZ {
					t.Fatalf("seed %d: #32 telemetry = %+v, want %v", seed, out.Telemetry, rv.nedXYZ)
				}

			case 5: // #253 STATUSTEXT
				out := tracker.IngestFrame(mavcodec.Frame{
					SystemID: sysid, ComponentID: 1, Sequence: uint8(step),
					Message: mavcodec.StatusText{Severity: 6, Text: "OK\x00"},
				}, peer, now)
				if !rv.admitted {
					continue
				}
				sawStatusText++
				rv.rev++
				if !out.UrgentSKGSync || out.Event == nil || out.Event.StatusText.Text != "OK" {
					t.Fatalf("seed %d: #253 outcome = %+v", seed, out)
				}
				pub.NotifyUrgent(sysid)

			case 6: // Enqueue periodic burst (sometimes > PeriodicQueueCap before draining)
				burst := 1 + rng.IntN(4)
				for range burst {
					batch, bErr := tracker.BuildPeriodicBatch()
					if bErr != nil {
						t.Fatalf("seed %d: BuildPeriodicBatch: %v", seed, bErr)
					}
					pub.EnqueuePeriodic(batch)
				}
			}
		}

		// Final periodic enqueue + drain Publisher worker to convergence.
		finalBatch, err := tracker.BuildPeriodicBatch()
		if err != nil {
			t.Fatalf("seed %d: final BuildPeriodicBatch: %v", seed, err)
		}
		pub.EnqueuePeriodic(finalBatch)
		sawDroppedPeriodic += int(pub.DroppedPeriodic())

		runCtx, runCancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			pub.Run(runCtx)
		}()
		time.Sleep(15 * time.Millisecond)
		runCancel()
		<-done

		// Verify materialized SKG snapshots match the reference model for every admitted sysid.
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
				snap.Failsafes != rv.failsafes || snap.HasHeading != rv.hasHeading || angularDiffDeg(snap.HeadingDeg, rv.hdgDeg) > 1e-2 ||
				snap.NED != rv.nedXYZ || [3]float64{snap.Latitude, snap.Longitude, snap.Altitude} != rv.latLonAlt {
				t.Fatalf("seed %d sysid=%d materialized mismatch:\ngot =%+v\nwant=%+v", seed, sysid, snap, *rv)
			}
			var gotHdgTr *message.Triple
			for i := range mergedTriples[sysid] {
				if mergedTriples[sysid][i].Predicate == uxsvocab.PredicateKinematicsHeadingDeg {
					gotHdgTr = &mergedTriples[sysid][i]
					break
				}
			}
			if gotHdgTr == nil {
				t.Fatalf("seed %d sysid=%d: missing heading triple in ReplaceBySource output", seed, sysid)
			}
			gotHdgVal, okHdg := gotHdgTr.Object.(float64)
			wantConf := 0.0
			wantCtx := ""
			if rv.hasHeading {
				wantConf = 1.0
				wantCtx = "measured"
			}
			if !okHdg || gotHdgTr.Confidence != wantConf || gotHdgTr.Context != wantCtx || angularDiffDeg(gotHdgVal, rv.hdgDeg) > 1e-2 {
				t.Fatalf("seed %d sysid=%d ReplaceBySource heading triple = %+v, want hasHeading=%v hdg=%v",
					seed, sysid, *gotHdgTr, rv.hasHeading, rv.hdgDeg)
			}
		}
		publishedMu.Unlock()
	}

	if sawAdmission == 0 || sawSysStatusBaseline == 0 || sawQuatOverride == 0 || sawHdgFallback == 0 || sawHdgInvalidation == 0 ||
		sawHeartbeatEdge == 0 || sawFailsafeEdge == 0 || sawStatusText == 0 ||
		sawSanitizedReject == 0 || sawDroppedPeriodic == 0 {
		t.Fatalf("insufficient property boundary reach: admit=%d sysBaseline=%d quatOverride=%d hdgFallback=%d hdgInvalid=%d hbEdge=%d fsEdge=%d st=%d reject=%d drop=%d",
			sawAdmission, sawSysStatusBaseline, sawQuatOverride, sawHdgFallback, sawHdgInvalidation, sawHeartbeatEdge, sawFailsafeEdge, sawStatusText, sawSanitizedReject, sawDroppedPeriodic)
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
		if !outPost.Accepted && after != before {
			t.Fatalf("rejected datagram mutated sysid=1 snapshot:\nbefore=%+v\nafter =%+v", before, after)
		}
		if err := after.Validate(); err != nil {
			t.Fatalf("admitted snapshot violated Validate() after arbitrary datagram: %v", err)
		}
		if triples := after.Triples(); len(triples) != 22 {
			t.Fatalf("Triples() count = %d, want 22", len(triples))
		}
	})
}
