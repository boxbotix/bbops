package host

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/boxbotix/bbops/internal/control"
	"github.com/boxbotix/bbops/internal/mavcodec"
	"github.com/c360studio/semengine/graph"
)

// spec: uxs-control / Pin-on-admission MAVLink v2 UDP command egress and 1 Hz GCS liveness
// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestRuntimeControlEndpointsAndLifecycleCAS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, err := StartEphemeral(ctx, Options{
		DataDir:   t.TempDir(),
		Logger:    slog.New(slog.DiscardHandler),
		Overrides: PortOverrides{MetricsPort: -1},
		UIDir:     t.TempDir(),
		Version:   "test-m3.1",
	}, 4)
	if err != nil {
		t.Fatalf("StartEphemeral: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = rt.Stop(stopCtx)
	}()

	if err := rt.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	baseURL := rt.Endpoints().HTTPBaseURL

	// 1. Method & same-origin enforcement on /control/command and /control/teleop.
	for _, route := range []string{control.CommandFrontDoorPath, control.TeleopFrontDoorPath} {
		getResp, err := http.Get(baseURL + route) //nolint:noctx
		if err != nil {
			t.Fatalf("GET %s: %v", route, err)
		}
		_ = getResp.Body.Close()
		if getResp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s status = %d, want 405", route, getResp.StatusCode)
		}

		evilReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+route, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequestWithContext %s: %v", route, err)
		}
		evilReq.Header.Set("Origin", "http://evil.example")
		evilReq.Header.Set("Content-Type", "application/json")
		evilResp, err := http.DefaultClient.Do(evilReq)
		if err != nil {
			t.Fatalf("cross-origin POST %s: %v", route, err)
		}
		_ = evilResp.Body.Close()
		if evilResp.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-origin POST %s status = %d, want 403", route, evilResp.StatusCode)
		}
	}

	// 2. Admit vehicle sysid=1 from pinned loopback UDP peer.
	udpDst, err := net.ResolveUDPAddr("udp", rt.Endpoints().MAVLinkUDPAddr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr: %v", err)
	}
	peerConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP peer: %v", err)
	}
	defer func() { _ = peerConn.Close() }()

	spoofConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP spoof: %v", err)
	}
	defer func() { _ = spoofConn.Close() }()

	hbMsg, err := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
		Armed:        true,
		SystemStatus: mavcodec.MAVStateActive,
		Custom: mavcodec.CustomMode{
			LinkTier:         mavcodec.LinkTierSITLLocal,
			HealthFlags:      mavcodec.HealthFlags{CRSFLinkHealthy: true, CompanionTrunkHealthy: true},
			Domain:           mavcodec.DomainQuadcopter,
			CompanionMode:    mavcodec.ModeTeleopVelocity,
			ControlAuthority: mavcodec.AuthorityCompanionTeleop,
		},
	})
	if err != nil {
		t.Fatalf("EncodeVehicleHeartbeat: %v", err)
	}
	hbBytes, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    1,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message:     hbMsg,
	})
	if err != nil {
		t.Fatalf("EncodeFrame heartbeat: %v", err)
	}
	if _, err := peerConn.WriteToUDP(hbBytes, udpDst); err != nil {
		t.Fatalf("peerConn WriteToUDP heartbeat: %v", err)
	}

	admitDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(admitDeadline) {
		if target, ok := rt.MAVLinkIngest().ActuationTarget(1); ok && target.Armed {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if _, ok := rt.MAVLinkIngest().ActuationTarget(1); !ok {
		t.Fatal("vehicle sysid=1 was not admitted within 3s")
	}

	// Spoofed datagram from second port claiming sysid=1 while live must NOT hijack TX peer.
	if _, err := spoofConn.WriteToUDP(hbBytes, udpDst); err != nil {
		t.Fatalf("spoofConn WriteToUDP: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	// 3. Same-origin POST /control/teleop delivers GCS #0 HEARTBEAT + #84 velocity frame to peerConn (never spoofConn).
	teleopReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+control.TeleopFrontDoorPath,
		bytes.NewBufferString(`{"sysid":1,"frame":"body-ned","vx":1.4,"vy":-0.3,"vz":0,"yaw_rate":0.2,"engage":true}`))
	if err != nil {
		t.Fatalf("NewRequest teleop: %v", err)
	}
	teleopReq.Header.Set("Origin", baseURL)
	teleopReq.Header.Set("Content-Type", "application/json")
	teleopResp, err := http.DefaultClient.Do(teleopReq)
	if err != nil {
		t.Fatalf("POST /control/teleop: %v", err)
	}
	teleopBody, _ := io.ReadAll(teleopResp.Body)
	_ = teleopResp.Body.Close()
	if teleopResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /control/teleop status=%d body=%s", teleopResp.StatusCode, string(teleopBody))
	}

	readPeerFrames := func(conn *net.UDPConn, maxFrames int, wait time.Duration) []mavcodec.Frame {
		t.Helper()
		var out []mavcodec.Frame
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		buf := make([]byte, 512)
		for len(out) < maxFrames {
			n, _, readErr := conn.ReadFromUDP(buf)
			if readErr != nil {
				break
			}
			f, decErr := mavcodec.DecodeFrame(buf[:n])
			if decErr != nil {
				t.Fatalf("DecodeFrame outbound: %v", decErr)
			}
			out = append(out, f)
		}
		return out
	}

	outbound := readPeerFrames(peerConn, 2, time.Second)
	var sawGCSHB, sawSetpoint bool
	for _, f := range outbound {
		switch m := f.Message.(type) {
		case mavcodec.Heartbeat:
			if f.SystemID == mavcodec.GCSSystemID && m.Type == mavcodec.MAVTypeGCS {
				sawGCSHB = true
			}
		case mavcodec.SetPositionTargetLocalNED:
			if m.TargetSystem == 1 && m.CoordinateFrame == mavcodec.MAVFrameBodyNED &&
				math.Abs(float64(m.Vx)-1.4) < 1e-4 {
				sawSetpoint = true
			}
		}
	}
	if !sawGCSHB || !sawSetpoint {
		t.Fatalf("outbound frames on pinned peer missing GCS heartbeat (%v) or #84 setpoint (%v): %+v", sawGCSHB, sawSetpoint, outbound)
	}
	if spoofFrames := readPeerFrames(spoofConn, 1, 50*time.Millisecond); len(spoofFrames) != 0 {
		t.Fatalf("spoof peer unexpectedly received %d outbound frames", len(spoofFrames))
	}

	// 4. Same-origin POST /control/command sends single #76 COMMAND_LONG, creates CommandEntity in ENTITY_STATES,
	// and CAS-transitions pending -> confirmed upon inbound #77 COMMAND_ACK from pinned peer.
	cmdReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+control.CommandFrontDoorPath,
		bytes.NewBufferString(`{"sysid":1,"action":"hold-position"}`))
	if err != nil {
		t.Fatalf("NewRequest command: %v", err)
	}
	cmdReq.Header.Set("Origin", baseURL)
	cmdReq.Header.Set("Content-Type", "application/json")
	cmdResp, err := http.DefaultClient.Do(cmdReq)
	if err != nil {
		t.Fatalf("POST /control/command: %v", err)
	}
	cmdBody, _ := io.ReadAll(cmdResp.Body)
	_ = cmdResp.Body.Close()
	if cmdResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /control/command status=%d body=%s", cmdResp.StatusCode, string(cmdBody))
	}
	var cmdEnv control.Envelope[control.CommandResult]
	if err := json.Unmarshal(cmdBody, &cmdEnv); err != nil || !cmdEnv.OK || cmdEnv.Result == nil {
		t.Fatalf("decode command envelope: %+v err=%v", cmdEnv, err)
	}
	commandID := cmdEnv.Result.CommandID

	cmdFrames := readPeerFrames(peerConn, 2, time.Second)
	var sawHoldCmd bool
	for _, f := range cmdFrames {
		if cl, ok := f.Message.(mavcodec.CommandLong); ok && cl.Command == mavcodec.MAVCmdNavLoiterUnlim && cl.Confirmation == 0 {
			sawHoldCmd = true
		}
	}
	if !sawHoldCmd {
		t.Fatalf("expected outbound #76 MAV_CMD_NAV_LOITER_UNLIM on pinned peer, got %+v", cmdFrames)
	}

	js, err := rt.NATSClient().JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.KeyValue(ctx, graph.BucketEntityStates)
	if err != nil {
		t.Fatalf("open ENTITY_STATES KV: %v", err)
	}

	readCommandPhase := func() (string, int) {
		entry, kvErr := kv.Get(ctx, commandID)
		if kvErr != nil {
			return "", -999
		}
		var es graph.EntityState
		if jsonErr := json.Unmarshal(entry.Value(), &es); jsonErr != nil {
			return "", -999
		}
		phase := ""
		ackRes := -999
		for _, tr := range es.Triples {
			switch tr.Predicate {
			case control.PredicateCommandPhase:
				if s, ok := tr.Object.(string); ok {
					phase = s
				}
			case control.PredicateCommandAckResult:
				if v, ok := tr.Object.(float64); ok {
					ackRes = int(v)
				}
			}
		}
		return phase, ackRes
	}

	pendingDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(pendingDeadline) {
		if phase, _ := readCommandPhase(); phase == control.PhasePending {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if phase, ackRes := readCommandPhase(); phase != control.PhasePending || ackRes != control.UnacknowledgedResult {
		t.Fatalf("ENTITY_STATES before ACK = (phase=%q, ackRes=%d), want (%q, %d)", phase, ackRes, control.PhasePending, control.UnacknowledgedResult)
	}

	ackBytes, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    2,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message: mavcodec.CommandAck{
			Command:      mavcodec.MAVCmdNavLoiterUnlim,
			Result:       mavcodec.MAVResultAccepted,
			TargetSystem: mavcodec.GCSSystemID,
		},
	})
	if err != nil {
		t.Fatalf("EncodeFrame CommandAck: %v", err)
	}
	if _, err := peerConn.WriteToUDP(ackBytes, udpDst); err != nil {
		t.Fatalf("peerConn WriteToUDP CommandAck: %v", err)
	}

	confirmDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(confirmDeadline) {
		if phase, ackRes := readCommandPhase(); phase == control.PhaseConfirmed && ackRes == int(mavcodec.MAVResultAccepted) {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	phase, ackRes := readCommandPhase()
	t.Fatalf("ENTITY_STATES after #77 ACCEPTED = (phase=%q, ackRes=%d), want (%q, 0)", phase, ackRes, control.PhaseConfirmed)
}
