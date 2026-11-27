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

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestRuntimeControlRestartRetainedDataDir(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	retainedDataDir := t.TempDir()
	uiDir := t.TempDir()

	peerConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP peer: %v", err)
	}
	defer func() { _ = peerConn.Close() }()

	readPeerFrames := func(maxFrames int, wait time.Duration) []mavcodec.Frame {
		t.Helper()
		var out []mavcodec.Frame
		_ = peerConn.SetReadDeadline(time.Now().Add(wait))
		buf := make([]byte, 512)
		for len(out) < maxFrames {
			n, _, readErr := peerConn.ReadFromUDP(buf)
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

	readKVPhase := func(rt *Runtime, commandID string) string {
		js, jsErr := rt.NATSClient().JetStream()
		if jsErr != nil {
			return ""
		}
		kv, kvErr := js.KeyValue(ctx, graph.BucketEntityStates)
		if kvErr != nil {
			return ""
		}
		entry, getErr := kv.Get(ctx, commandID)
		if getErr != nil {
			return ""
		}
		var es graph.EntityState
		if jsonErr := json.Unmarshal(entry.Value(), &es); jsonErr != nil {
			return ""
		}
		for _, tr := range es.Triples {
			if tr.Predicate == control.PredicateCommandPhase {
				if s, ok := tr.Object.(string); ok {
					return s
				}
			}
		}
		return ""
	}

	postCommand := func(baseURL, body string) control.CommandResult {
		t.Helper()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+control.CommandFrontDoorPath, bytes.NewBufferString(body))
		if reqErr != nil {
			t.Fatalf("NewRequest: %v", reqErr)
		}
		req.Header.Set("Origin", baseURL)
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			t.Fatalf("POST /control/command: %v", doErr)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /control/command status=%d body=%s", resp.StatusCode, string(raw))
		}
		var env control.Envelope[control.CommandResult]
		if decErr := json.Unmarshal(raw, &env); decErr != nil || !env.OK || env.Result == nil {
			t.Fatalf("decode command envelope: %+v err=%v", env, decErr)
		}
		return *env.Result
	}

	// --- Boot #1 on retainedDataDir ---
	rt1, err := StartEphemeral(ctx, Options{
		DataDir:   retainedDataDir,
		Logger:    slog.New(slog.DiscardHandler),
		Overrides: PortOverrides{MetricsPort: -1},
		UIDir:     uiDir,
		Version:   "test-m3.1-boot1",
	}, 4)
	if err != nil {
		t.Fatalf("StartEphemeral boot 1: %v", err)
	}
	if err := rt1.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady boot 1: %v", err)
	}

	udpDst1, err := net.ResolveUDPAddr("udp4", rt1.Endpoints().MAVLinkUDPAddr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr boot 1: %v", err)
	}
	hbMsg1, err := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
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
		t.Fatalf("EncodeVehicleHeartbeat hb1: %v", err)
	}
	hb1, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    1,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message:     hbMsg1,
	})
	if err != nil {
		t.Fatalf("EncodeFrame hb1: %v", err)
	}
	if _, err := peerConn.WriteToUDP(hb1, udpDst1); err != nil {
		t.Fatalf("WriteToUDP hb1: %v", err)
	}
	time.Sleep(40 * time.Millisecond)

	// 1) Complete one command on Boot #1.
	cmdDone := postCommand(rt1.Endpoints().HTTPBaseURL, `{"sysid":1,"action":"mode-autonomous"}`)
	_ = readPeerFrames(2, 500*time.Millisecond)
	ackDone, _ := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    2,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message: mavcodec.CommandAck{
			Command:      mavcodec.MAVCmdDoSetMode,
			Result:       mavcodec.MAVResultAccepted,
			TargetSystem: mavcodec.GCSSystemID,
		},
	})
	_, _ = peerConn.WriteToUDP(ackDone, udpDst1)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && readKVPhase(rt1, cmdDone.CommandID) != control.PhaseConfirmed {
		time.Sleep(15 * time.Millisecond)
	}
	if got := readKVPhase(rt1, cmdDone.CommandID); got != control.PhaseConfirmed {
		t.Fatalf("boot 1 completed command phase = %q, want confirmed", got)
	}

	// 2) Submit a second command (`hold-position`) whose ACK is never sent before Boot #1 stops.
	cmdIndeterminate := postCommand(rt1.Endpoints().HTTPBaseURL, `{"sysid":1,"action":"hold-position"}`)
	_ = readPeerFrames(2, 500*time.Millisecond)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && readKVPhase(rt1, cmdIndeterminate.CommandID) == "" {
		time.Sleep(15 * time.Millisecond)
	}

	stopCtx1, stopCancel1 := context.WithTimeout(ctx, 10*time.Second)
	stopErr1 := rt1.Stop(stopCtx1)
	stopCancel1()
	if stopErr1 != nil {
		t.Fatalf("Stop boot 1: %v", stopErr1)
	}

	// --- Boot #2 on the SAME retainedDataDir ---
	rt2, err := StartEphemeral(ctx, Options{
		DataDir:   retainedDataDir,
		Logger:    slog.New(slog.DiscardHandler),
		Overrides: PortOverrides{MetricsPort: -1},
		UIDir:     uiDir,
		Version:   "test-m3.1-boot2",
	}, 4)
	if err != nil {
		t.Fatalf("StartEphemeral boot 2: %v", err)
	}
	defer func() {
		stopCtx2, stopCancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel2()
		_ = rt2.Stop(stopCtx2)
	}()
	if err := rt2.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady boot 2: %v", err)
	}

	// Pre-restart pending command MUST recover to commit_unknown in ENTITY_STATES without retransmitting #76!
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && readKVPhase(rt2, cmdIndeterminate.CommandID) != control.PhaseCommitUnknown {
		time.Sleep(20 * time.Millisecond)
	}
	if got := readKVPhase(rt2, cmdIndeterminate.CommandID); got != control.PhaseCommitUnknown {
		t.Fatalf("boot 2 recovered indeterminate command phase = %q, want commit_unknown", got)
	}
	for _, f := range readPeerFrames(4, 150*time.Millisecond) {
		if cl, isCmd := f.Message.(mavcodec.CommandLong); isCmd {
			t.Fatalf("boot 2 retransmitted pre-restart COMMAND_LONG: %+v", cl)
		}
	}

	// Admit vehicle on Boot #2 with fresh Hold-Position HEARTBEAT -> reconciles recovered command to confirmed,
	// and a new command on Boot #2 succeeds with a collision-free restart-safe ID!
	udpDst2, err := net.ResolveUDPAddr("udp4", rt2.Endpoints().MAVLinkUDPAddr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr boot 2: %v", err)
	}
	hbMsg2, err := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
		Armed:        true,
		SystemStatus: mavcodec.MAVStateActive,
		Custom: mavcodec.CustomMode{
			LinkTier:         mavcodec.LinkTierSITLLocal,
			HealthFlags:      mavcodec.HealthFlags{CRSFLinkHealthy: true, CompanionTrunkHealthy: true},
			Domain:           mavcodec.DomainQuadcopter,
			CompanionMode:    mavcodec.ModeHoldPosition,
			ControlAuthority: mavcodec.AuthorityCompanionTeleop,
		},
	})
	if err != nil {
		t.Fatalf("EncodeVehicleHeartbeat hb2: %v", err)
	}
	hb2, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    3,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message:     hbMsg2,
	})
	if err != nil {
		t.Fatalf("EncodeFrame hb2: %v", err)
	}
	if _, err := peerConn.WriteToUDP(hb2, udpDst2); err != nil {
		t.Fatalf("WriteToUDP hb2: %v", err)
	}

	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && readKVPhase(rt2, cmdIndeterminate.CommandID) != control.PhaseConfirmed {
		time.Sleep(20 * time.Millisecond)
	}
	if got := readKVPhase(rt2, cmdIndeterminate.CommandID); got != control.PhaseConfirmed {
		t.Fatalf("boot 2 post-restart HEARTBEAT failed to reconcile recovered command: phase=%q", got)
	}

	cmdBoot2 := postCommand(rt2.Endpoints().HTTPBaseURL, `{"sysid":1,"action":"mode-teleop"}`)
	if cmdBoot2.CommandID == cmdDone.CommandID || cmdBoot2.CommandID == cmdIndeterminate.CommandID {
		t.Fatalf("boot 2 reused pre-restart command entity ID: %s", cmdBoot2.CommandID)
	}
	// Resolve cmdBoot2 cleanly via its #77 ACCEPTED before shutting down Boot #2.
	ackBoot2, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    4,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message: mavcodec.CommandAck{
			Command: mavcodec.MAVCmdDoSetMode,
			Result:  mavcodec.MAVResultAccepted,
		},
	})
	if err != nil {
		t.Fatalf("EncodeFrame ackBoot2: %v", err)
	}
	if _, err := peerConn.WriteToUDP(ackBoot2, udpDst2); err != nil {
		t.Fatalf("WriteToUDP ackBoot2: %v", err)
	}
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && readKVPhase(rt2, cmdBoot2.CommandID) != control.PhaseConfirmed {
		time.Sleep(20 * time.Millisecond)
	}

	// Boot #3 on the SAME DataDir: cmdIndeterminate is now terminal (PhaseConfirmed via HEARTBEAT, AckResult=-1)
	// in JetStream ENTITY_STATES! Verify LifecycleStore.ListCommands recovers its unacked status so a new
	// Hold-Position command on Boot #3 treats a delayed #77 ACCEPTED as ambiguous (commit_unknown)!
	stopCtx2Explicit, stopCancel2Explicit := context.WithTimeout(ctx, 10*time.Second)
	stopErr2 := rt2.Stop(stopCtx2Explicit)
	stopCancel2Explicit()
	if stopErr2 != nil {
		t.Fatalf("Stop boot 2: %v", stopErr2)
	}

	rt3, err := StartEphemeral(ctx, Options{
		DataDir:   retainedDataDir,
		Logger:    slog.New(slog.DiscardHandler),
		Overrides: PortOverrides{MetricsPort: -1},
		UIDir:     uiDir,
		Version:   "test-m3.1-boot3",
	}, 4)
	if err != nil {
		t.Fatalf("StartEphemeral boot 3: %v", err)
	}
	rt3Stopped := false
	defer func() {
		if rt3Stopped {
			return
		}
		stopCtx3, stopCancel3 := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel3()
		_ = rt3.Stop(stopCtx3)
	}()
	if err := rt3.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady boot 3: %v", err)
	}

	udpDst3, err := net.ResolveUDPAddr("udp4", rt3.Endpoints().MAVLinkUDPAddr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr boot 3: %v", err)
	}
	hbMsg3, err := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
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
		t.Fatalf("EncodeVehicleHeartbeat hb3: %v", err)
	}
	hb3, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    5,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message:     hbMsg3,
	})
	if err != nil {
		t.Fatalf("EncodeFrame hb3: %v", err)
	}
	if _, err := peerConn.WriteToUDP(hb3, udpDst3); err != nil {
		t.Fatalf("WriteToUDP hb3: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if target, ok := rt3.MAVLinkIngest().ActuationTarget(1); ok && target.Armed {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	cmdBoot3 := postCommand(rt3.Endpoints().HTTPBaseURL, `{"sysid":1,"action":"hold-position"}`)
	_ = readPeerFrames(2, 100*time.Millisecond)

	// Send delayed #77 ACCEPTED for MAVCmdNavLoiterUnlim while vehicle is still in teleop-velocity mode:
	// Boot #3 MUST transition cmdBoot3 to commit_unknown (not confirmed) because cmdIndeterminate was never ACKed!
	staleLoiterAck, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    6,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message: mavcodec.CommandAck{
			Command: mavcodec.MAVCmdNavLoiterUnlim,
			Result:  mavcodec.MAVResultAccepted,
		},
	})
	if err != nil {
		t.Fatalf("EncodeFrame staleLoiterAck: %v", err)
	}
	if _, err := peerConn.WriteToUDP(staleLoiterAck, udpDst3); err != nil {
		t.Fatalf("WriteToUDP staleLoiterAck: %v", err)
	}
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && readKVPhase(rt3, cmdBoot3.CommandID) != control.PhaseCommitUnknown {
		time.Sleep(20 * time.Millisecond)
	}
	if got := readKVPhase(rt3, cmdBoot3.CommandID); got != control.PhaseCommitUnknown {
		t.Fatalf("boot 3 failed to preserve terminal heartbeat-reconciled ACK ambiguity: phase=%q, want commit_unknown", got)
	}

	// Reconcile cmdBoot3 via #0 HEARTBEAT (ModeHoldPosition), then deliver cmdBoot3's sole late #77 ACCEPTED
	// while idle so LifecycleStore.RecordConsumedAck (UpdateFromOperator) persists AckResult != -1 in JetStream!
	hbBoot3Loiter, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    7,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message:     hbMsg2,
	})
	if err != nil {
		t.Fatalf("EncodeFrame hbBoot3Loiter: %v", err)
	}
	if _, err := peerConn.WriteToUDP(hbBoot3Loiter, udpDst3); err != nil {
		t.Fatalf("WriteToUDP hbBoot3Loiter: %v", err)
	}
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && readKVPhase(rt3, cmdBoot3.CommandID) != control.PhaseConfirmed {
		time.Sleep(20 * time.Millisecond)
	}
	if got := readKVPhase(rt3, cmdBoot3.CommandID); got != control.PhaseConfirmed {
		t.Fatalf("boot 3 failed to reconcile cmdBoot3 via ModeHoldPosition heartbeat: phase=%q", got)
	}

	idleAckBoot3, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    8,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message: mavcodec.CommandAck{
			Command: mavcodec.MAVCmdNavLoiterUnlim,
			Result:  mavcodec.MAVResultAccepted,
		},
	})
	if err != nil {
		t.Fatalf("EncodeFrame idleAckBoot3: %v", err)
	}
	if _, err := peerConn.WriteToUDP(idleAckBoot3, udpDst3); err != nil {
		t.Fatalf("WriteToUDP idleAckBoot3: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	// Boot #4 on the SAME retainedDataDir: both prior Loiter commands have now consumed their ACKs
	// durably in JetStream ENTITY_STATES, so a new Hold-Position command receiving explicit DENIED
	// MUST transition to PhaseRejected (NOT commit_unknown)!
	stopCtx3Explicit, stopCancel3Explicit := context.WithTimeout(ctx, 10*time.Second)
	stopErr3 := rt3.Stop(stopCtx3Explicit)
	stopCancel3Explicit()
	rt3Stopped = true
	if stopErr3 != nil {
		t.Fatalf("Stop boot 3: %v", stopErr3)
	}

	rt4, err := StartEphemeral(ctx, Options{
		DataDir:   retainedDataDir,
		Logger:    slog.New(slog.DiscardHandler),
		Overrides: PortOverrides{MetricsPort: -1},
		UIDir:     uiDir,
		Version:   "test-m3.1-boot4",
	}, 4)
	if err != nil {
		t.Fatalf("StartEphemeral boot 4: %v", err)
	}
	defer func() {
		stopCtx4, stopCancel4 := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel4()
		_ = rt4.Stop(stopCtx4)
	}()
	if err := rt4.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady boot 4: %v", err)
	}

	udpDst4, err := net.ResolveUDPAddr("udp4", rt4.Endpoints().MAVLinkUDPAddr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr boot 4: %v", err)
	}
	hb4, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    9,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message:     hbMsg3,
	})
	if err != nil {
		t.Fatalf("EncodeFrame hb4: %v", err)
	}
	if _, err := peerConn.WriteToUDP(hb4, udpDst4); err != nil {
		t.Fatalf("WriteToUDP hb4: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if target, ok := rt4.MAVLinkIngest().ActuationTarget(1); ok && target.Armed {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	cmdBoot4 := postCommand(rt4.Endpoints().HTTPBaseURL, `{"sysid":1,"action":"hold-position"}`)
	_ = readPeerFrames(2, 100*time.Millisecond)

	deniedAckBoot4, err := mavcodec.EncodeFrame(mavcodec.Frame{
		Sequence:    10,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message: mavcodec.CommandAck{
			Command: mavcodec.MAVCmdNavLoiterUnlim,
			Result:  mavcodec.MAVResultDenied,
		},
	})
	if err != nil {
		t.Fatalf("EncodeFrame deniedAckBoot4: %v", err)
	}
	if _, err := peerConn.WriteToUDP(deniedAckBoot4, udpDst4); err != nil {
		t.Fatalf("WriteToUDP deniedAckBoot4: %v", err)
	}
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && readKVPhase(rt4, cmdBoot4.CommandID) != control.PhaseRejected {
		time.Sleep(20 * time.Millisecond)
	}
	if got := readKVPhase(rt4, cmdBoot4.CommandID); got != control.PhaseRejected {
		t.Fatalf("boot 4 explicit DENIED after idle ACK consumption = %q, want %q (not commit_unknown)", got, control.PhaseRejected)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestRuntimeBackwardClockTeleopEngageAndCollidedTimestampFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t0 := time.Date(2026, 11, 27, 20, 45, 0, 0, time.UTC)
	clockNs := t0.UnixNano()
	readClock := func() time.Time {
		return time.Unix(0, clockNs).UTC()
	}
	setClock := func(next time.Time) {
		clockNs = next.UTC().UnixNano()
	}

	// Guard wall clock for concurrent runtime HTTP + UDP goroutines.
	type syncedClock struct {
		ch chan func()
	}
	sc := &syncedClock{ch: make(chan func(), 1)}
	sc.ch <- func() {}
	getWall := func() time.Time {
		tok := <-sc.ch
		cur := readClock()
		sc.ch <- tok
		return cur
	}
	putWall := func(next time.Time) {
		tok := <-sc.ch
		setClock(next)
		sc.ch <- tok
	}

	rt, err := StartEphemeral(ctx, Options{
		DataDir:   t.TempDir(),
		Logger:    slog.New(slog.DiscardHandler),
		Overrides: PortOverrides{MetricsPort: -1},
		UIDir:     t.TempDir(),
		Version:   "test-m3.1-wallclock",
		Now:       getWall,
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

	bridge := runtimeMAVLinkBridge{comp: &rt.mavlinkIngest}
	if _, ok := any(bridge).(control.ObservationResolver); !ok {
		t.Fatal("runtimeMAVLinkBridge must implement control.ObservationResolver")
	}

	baseURL := rt.Endpoints().HTTPBaseURL
	udpDst, err := net.ResolveUDPAddr("udp4", rt.Endpoints().MAVLinkUDPAddr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr: %v", err)
	}
	peerConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	defer func() { _ = peerConn.Close() }()

	sendHB := func(seq uint8, mode mavcodec.CompanionMode, auth mavcodec.ControlAuthority, waitObsRev uint64) {
		t.Helper()
		hbMsg, encErr := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
			Armed:        true,
			SystemStatus: mavcodec.MAVStateActive,
			Custom: mavcodec.CustomMode{
				LinkTier:         mavcodec.LinkTierSITLLocal,
				HealthFlags:      mavcodec.HealthFlags{CRSFLinkHealthy: true, CompanionTrunkHealthy: true},
				Domain:           mavcodec.DomainQuadcopter,
				CompanionMode:    mode,
				ControlAuthority: auth,
			},
		})
		if encErr != nil {
			t.Fatalf("EncodeVehicleHeartbeat: %v", encErr)
		}
		raw, encErr := mavcodec.EncodeFrame(mavcodec.Frame{
			Sequence:    seq,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message:     hbMsg,
		})
		if encErr != nil {
			t.Fatalf("EncodeFrame: %v", encErr)
		}
		if _, wErr := peerConn.WriteToUDP(raw, udpDst); wErr != nil {
			t.Fatalf("WriteToUDP: %v", wErr)
		}
		waitDeadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(waitDeadline) {
			if tgt, ok := rt.MAVLinkIngest().ActuationTarget(1); ok && tgt.ObservationRev >= waitObsRev {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for ObservationRev >= %d", waitObsRev)
	}

	postTeleopJSON := func(body any) (int, map[string]any) {
		t.Helper()
		rawBody, mErr := json.Marshal(body)
		if mErr != nil {
			t.Fatalf("json.Marshal: %v", mErr)
		}
		req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+control.TeleopFrontDoorPath, bytes.NewReader(rawBody))
		if rErr != nil {
			t.Fatalf("NewRequestWithContext: %v", rErr)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, dErr := http.DefaultClient.Do(req)
		if dErr != nil {
			t.Fatalf("POST /control/teleop: %v", dErr)
		}
		defer func() { _ = resp.Body.Close() }()
		respBytes, _ := io.ReadAll(resp.Body)
		var parsed map[string]any
		_ = json.Unmarshal(respBytes, &parsed)
		return resp.StatusCode, parsed
	}

	// 1) Admit sysid=1 at t0, engage and stop session s1 over live HTTP /control/teleop.
	sendHB(1, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, 1)
	tgt1, _ := rt.MAVLinkIngest().ActuationTarget(1)
	if code, body := postTeleopJSON(map[string]any{
		"sysid":                 1,
		"session":               "host-s1",
		"seq":                   1,
		"vx":                    0.4,
		"engage":                true,
		"observed_ms":           t0.UnixMilli(),
		"observation_rev":       tgt1.ObservationRev,
		"authority_incarnation": tgt1.AuthorityIncarnation,
		"authority_epoch":       tgt1.AuthorityEpoch,
	}); code != http.StatusOK {
		t.Fatalf("initial HTTP Engage status=%d body=%v, want 200", code, body)
	}
	if code, body := postTeleopJSON(map[string]any{
		"sysid":   1,
		"session": "host-s1",
		"seq":     2,
		"stop":    true,
	}); code != http.StatusOK {
		t.Fatalf("initial HTTP Stop status=%d body=%v, want 200", code, body)
	}

	// 2) Step host wall clock BACKWARD by 60 seconds and verify timestamp-only HTTP Engage succeeds
	// end-to-end through runtimeMAVLinkBridge.ResolveObservation!
	tBack := t0.Add(-60 * time.Second)
	putWall(tBack)
	sendHB(2, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, 2)
	if code, body := postTeleopJSON(map[string]any{
		"sysid":       1,
		"session":     "host-s2-after-backward-step",
		"seq":         1,
		"vx":          0.6,
		"engage":      true,
		"observed_ms": tBack.UnixMilli(),
	}); code != http.StatusOK {
		t.Fatalf("post-backward-clock HTTP Engage (observed_ms only) status=%d body=%v, want 200", code, body)
	}
	if code, body := postTeleopJSON(map[string]any{
		"sysid":   1,
		"session": "host-s2-after-backward-step",
		"seq":     2,
		"stop":    true,
	}); code != http.StatusOK {
		t.Fatalf("post-backward-clock HTTP Stop status=%d body=%v, want 200", code, body)
	}

	// 3) Collided timestamp across authority boundary over live UDP + HTTP:
	// P2 at tClick -> P1 override at tClick+20ms -> backward clock step landing P2 recovery on tClick!
	tClick := t0.Add(5 * time.Second)
	putWall(tClick)
	sendHB(3, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, 3)

	putWall(tClick.Add(20 * time.Millisecond))
	sendHB(4, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride, 4)

	putWall(tClick)
	sendHB(5, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, 5)
	recTgt, _ := rt.MAVLinkIngest().ActuationTarget(1)

	code, body := postTeleopJSON(map[string]any{
		"sysid":       1,
		"session":     "host-stale-collided-ts",
		"seq":         1,
		"vx":          0.9,
		"engage":      true,
		"observed_ms": tClick.UnixMilli(),
	})
	errObj, _ := body["error"].(map[string]any)
	if code != http.StatusConflict || errObj["code"] != "preemption_latched" {
		t.Fatalf("collided timestamp-only HTTP Engage status=%d body=%v, want 409 preemption_latched", code, body)
	}

	if code, body := postTeleopJSON(map[string]any{
		"sysid":                 1,
		"session":               "host-fresh-explicit-rev",
		"seq":                   1,
		"vx":                    0.7,
		"engage":                true,
		"observed_ms":           tClick.UnixMilli(),
		"observation_rev":       recTgt.ObservationRev,
		"authority_incarnation": recTgt.AuthorityIncarnation,
		"authority_epoch":       recTgt.AuthorityEpoch,
	}); code != http.StatusOK {
		t.Fatalf("explicit observation_rev HTTP Engage status=%d body=%v, want 200", code, body)
	}
}
