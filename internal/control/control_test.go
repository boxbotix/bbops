package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxbotix/bbops/internal/mavcodec"
	"github.com/boxbotix/bbops/internal/mavlink"
	"github.com/boxbotix/bbops/internal/uxsvocab"
	"github.com/c360studio/semengine/pkg/lifecycle"
)

type fakeTargetSource struct {
	mu      sync.Mutex
	targets map[uint8]mavlink.ActuationTarget
}

func newFakeTargetSource() *fakeTargetSource {
	return &fakeTargetSource{
		targets: make(map[uint8]mavlink.ActuationTarget),
	}
}

func (f *fakeTargetSource) set(t mavlink.ActuationTarget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets[t.SysID] = t
}

func (f *fakeTargetSource) ActuationTarget(sysid uint8) (mavlink.ActuationTarget, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.targets[sysid]
	return t, ok
}

type sentFrameRecord struct {
	frame mavcodec.Frame
	peer  netip.AddrPort
}

type fakeFrameSender struct {
	mu      sync.Mutex
	sent    []sentFrameRecord
	sendErr error
}

func (f *fakeFrameSender) SendFrame(frame mavcodec.Frame, peer netip.AddrPort) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, sentFrameRecord{frame: frame, peer: peer})
	return nil
}

func (f *fakeFrameSender) snapshot() []sentFrameRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]sentFrameRecord, len(f.sent))
	copy(cp, f.sent)
	return cp
}

type storeTransition struct {
	entityID  string
	newPhase  string
	note      string
	source    lifecycle.TransitionSource
	ackResult int
	detail    string
	updatedMs int64
}

type fakeCommandStore struct {
	mu          sync.Mutex
	entities    map[string]CommandEntity
	transitions []storeTransition
}

func newFakeCommandStore() *fakeCommandStore {
	return &fakeCommandStore{
		entities: make(map[string]CommandEntity),
	}
}

func (s *fakeCommandStore) Create(_ context.Context, cmd *CommandEntity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entities[cmd.ID]; exists {
		return errors.New("duplicate entity ID")
	}
	if !Workflow().Transitions.IsValidTransition(cmd.PhaseState, PhaseConfirmed) && cmd.PhaseState != PhasePending {
		return errors.New("unexpected initial phase")
	}
	s.entities[cmd.ID] = *cmd
	return nil
}

func (s *fakeCommandStore) Transition(
	_ context.Context,
	entityID, newPhase, note string,
	source lifecycle.TransitionSource,
	ackResult int,
	detail string,
	updatedMs int64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.entities[entityID]
	if !ok {
		return errors.New("unknown entity ID")
	}
	if !Workflow().Transitions.IsValidTransition(cur.PhaseState, newPhase) {
		return errors.New("invalid workflow transition " + cur.PhaseState + " -> " + newPhase)
	}
	cur.PhaseState = newPhase
	cur.AckResult = ackResult
	cur.Detail = detail
	cur.UpdatedMs = updatedMs
	s.entities[entityID] = cur
	s.transitions = append(s.transitions, storeTransition{
		entityID:  entityID,
		newPhase:  newPhase,
		note:      note,
		source:    source,
		ackResult: ackResult,
		detail:    detail,
		updatedMs: updatedMs,
	})
	return nil
}

func (s *fakeCommandStore) get(entityID string) (CommandEntity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entities[entityID]
	return e, ok
}

func countMessages[T any](records []sentFrameRecord) int {
	n := 0
	for _, r := range records {
		if _, ok := r.frame.Message.(T); ok {
			n++
		}
	}
	return n
}

// spec: uxs-control / Pin-on-admission MAVLink v2 UDP command egress and 1 Hz GCS liveness
// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceGCSHeartbeatAndValidation(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()

	if _, err := NewService(Options{
		Targets:        targets,
		Sender:         sender,
		Store:          store,
		TeleopLeaseTTL: 250 * time.Millisecond,
	}); err == nil {
		t.Fatal("expected TeleopLeaseTTL >= 250ms to fail validation")
	}

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	svc, err := NewService(Options{
		Org:                  "boxbotix",
		Platform:             "bbops",
		Targets:              targets,
		Sender:               sender,
		Store:                store,
		Now:                  func() time.Time { return now },
		TeleopLeaseTTL:       200 * time.Millisecond,
		GCSHeartbeatInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	peer := netip.MustParseAddrPort("127.0.0.1:19451")
	targets.set(mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                peer,
		HeartbeatObservedUnixNs: now.UnixNano(),
	})

	// Idle Advance emits zero GCS heartbeats.
	svc.Advance(context.Background(), now)
	if got := len(sender.snapshot()); got != 0 {
		t.Fatalf("idle Advance emitted %d frames, want 0", got)
	}

	// First teleop nudge emits initial 1 Hz GCS #0 HEARTBEAT + #84 velocity frame.
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:  1,
		Vx:     1.2,
		Engage: true,
	}); err != nil {
		t.Fatalf("SubmitTeleop initial: %v", err)
	}

	frames := sender.snapshot()
	if len(frames) != 2 {
		t.Fatalf("initial SubmitTeleop sent %d frames, want 2 (GCS #0 + #84)", len(frames))
	}
	hb, ok := frames[0].frame.Message.(mavcodec.Heartbeat)
	if !ok {
		t.Fatalf("frame[0] = %T, want mavcodec.Heartbeat", frames[0].frame.Message)
	}
	if frames[0].frame.SystemID != mavcodec.GCSSystemID ||
		frames[0].frame.ComponentID != mavcodec.CompIDMissionPlanner ||
		hb.Type != mavcodec.MAVTypeGCS ||
		hb.Autopilot != mavcodec.AutopilotGeneric ||
		hb.SystemStatus != mavcodec.MAVStateActive ||
		frames[0].peer != peer {
		t.Fatalf("unexpected GCS heartbeat frame: %+v peer=%v", frames[0].frame, frames[0].peer)
	}

	// Renew teleop every 150ms through t+1050ms; exactly one more GCS #0 HEARTBEAT should fire at >= 1s.
	for step := 1; step <= 7; step++ {
		now = now.Add(150 * time.Millisecond)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID: 1,
			Vx:    1.2,
		}); err != nil {
			t.Fatalf("SubmitTeleop step %d: %v", step, err)
		}
		svc.Advance(context.Background(), now)
	}
	if gotHB := countMessages[mavcodec.Heartbeat](sender.snapshot()); gotHB != 2 {
		t.Fatalf("GCS heartbeat count across 1.05s active teleop = %d, want 2", gotHB)
	}

	// Expire lease (> 200ms silent) and advance another 2s: GCS heartbeats must stop once idle.
	now = now.Add(250 * time.Millisecond)
	svc.Advance(context.Background(), now)
	hbAfterExpire := countMessages[mavcodec.Heartbeat](sender.snapshot())
	now = now.Add(2 * time.Second)
	svc.Advance(context.Background(), now)
	if gotHB := countMessages[mavcodec.Heartbeat](sender.snapshot()); gotHB != hbAfterExpire {
		t.Fatalf("idle post-expiry Advance emitted extra GCS heartbeats: got %d, want %d", gotHB, hbAfterExpire)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceTeleopSafetyGatesAndDeadmanExpiry(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()

	now := time.Date(2026, 11, 26, 12, 5, 0, 0, time.UTC)
	svc, err := NewService(Options{
		Org:            "boxbotix",
		Platform:       "bbops",
		Targets:        targets,
		Sender:         sender,
		Store:          store,
		Now:            func() time.Time { return now },
		TeleopLeaseTTL: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	peer := netip.MustParseAddrPort("127.0.0.1:19452")
	baseTarget := mavlink.ActuationTarget{
		SysID:                   2,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.2",
		Domain:                  uxsvocab.DomainRover,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierMeshHighRate,
		Armed:                   true,
		PeerAddr:                peer,
		HeartbeatObservedUnixNs: now.UnixNano(),
	}

	// Unadmitted vehicle rejected with 404.
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{SysID: 2, Vx: 1.0}); err == nil {
		t.Fatal("expected unadmitted vehicle teleop refusal")
	}

	// Non-finite floats, invalid sysid, and unsupported frame rejected with 0 UDP sends.
	targets.set(baseTarget)
	badRequests := []TeleopRequest{
		{SysID: 0, Vx: 1},
		{SysID: 255, Vx: 1},
		{SysID: 2, Vx: math.NaN()},
		{SysID: 2, Vy: math.Inf(1)},
		{SysID: 2, Vz: math.Inf(-1)},
		{SysID: 2, YawRate: math.NaN()},
		{SysID: 2, Frame: "global-wgs84", Vx: 1},
	}
	for i, req := range badRequests {
		if _, err := svc.SubmitTeleop(context.Background(), req); err == nil {
			t.Fatalf("bad request %d unexpectedly succeeded: %+v", i, req)
		}
	}

	// Safety gate refusals: mesh-supervisory, disarmed, link_lost, non-teleop authority.
	gateCases := []struct {
		name   string
		mutate func(*mavlink.ActuationTarget)
		code   string
	}{
		{
			name:   "mesh-supervisory",
			mutate: func(tg *mavlink.ActuationTarget) { tg.LinkTier = uxsvocab.LinkTierMeshSupervisory },
			code:   "supervisory_link_tier",
		},
		{
			name:   "disarmed",
			mutate: func(tg *mavlink.ActuationTarget) { tg.Armed = false },
			code:   "vehicle_disarmed",
		},
		{
			name:   "link_lost",
			mutate: func(tg *mavlink.ActuationTarget) { tg.LinkLost = true },
			code:   "vehicle_link_lost",
		},
		{
			name:   "companion-autonomous",
			mutate: func(tg *mavlink.ActuationTarget) { tg.Authority = uxsvocab.AuthorityCompanionAutonomous },
			code:   "authority_not_teleop",
		},
		{
			name:   "direct-crsf-override",
			mutate: func(tg *mavlink.ActuationTarget) { tg.Authority = uxsvocab.AuthorityDirectCRSFOverride },
			code:   "authority_not_teleop",
		},
	}
	for _, tc := range gateCases {
		tg := baseTarget
		tc.mutate(&tg)
		targets.set(tg)
		_, err := svc.SubmitTeleop(context.Background(), TeleopRequest{SysID: 2, Vx: 1.5})
		var ce *ControlError
		if !errors.As(err, &ce) || ce.Code != tc.code {
			t.Fatalf("%s: got err=%v, want ControlError code=%q", tc.name, err, tc.code)
		}
	}
	if got := len(sender.snapshot()); got != 0 {
		t.Fatalf("safety-gated teleop refusals transmitted %d UDP frames, want 0", got)
	}

	// Valid body-ned setpoint transmits #84 with MAV_FRAME_BODY_NED and velocity-only TypeMask.
	targets.set(baseTarget)
	res, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   2,
		Frame:   FrameBodyNED,
		Vx:      2.25,
		Vy:      -0.5,
		Vz:      0,
		YawRate: 0.35,
		Engage:  true,
	})
	if err != nil || !res.LeaseActive {
		t.Fatalf("valid SubmitTeleop: res=%+v err=%v", res, err)
	}
	if !svc.HasActiveLease(2) {
		t.Fatal("expected active lease on sysid 2")
	}

	var lastSetpoint mavcodec.SetPositionTargetLocalNED
	for _, rec := range sender.snapshot() {
		if sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED); ok {
			lastSetpoint = sp
		}
	}
	if lastSetpoint.CoordinateFrame != mavcodec.MAVFrameBodyNED ||
		lastSetpoint.TypeMask != mavcodec.PositionTargetTypeMaskVelocityOnly ||
		lastSetpoint.TargetSystem != 2 ||
		lastSetpoint.TargetComponent != mavcodec.CompIDAutopilot1 ||
		math.Abs(float64(lastSetpoint.Vx)-2.25) > 1e-5 {
		t.Fatalf("unexpected #84 setpoint payload: %+v", lastSetpoint)
	}

	// Silent browser past 200ms (< 250ms deadman) emits zero-velocity #84 and revokes lease.
	beforeCount := countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot())
	now = now.Add(205 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if svc.HasActiveLease(2) {
		t.Fatal("expected expired lease to be cleared at 205ms")
	}
	afterFrames := sender.snapshot()
	if got := countMessages[mavcodec.SetPositionTargetLocalNED](afterFrames); got != beforeCount+1 {
		t.Fatalf("expected 1 deadman zero-velocity #84 frame on expiry, got delta=%d", got-beforeCount)
	}
	zeroMsg, ok := afterFrames[len(afterFrames)-1].frame.Message.(mavcodec.SetPositionTargetLocalNED)
	if !ok || zeroMsg.Vx != 0 || zeroMsg.Vy != 0 || zeroMsg.Vz != 0 || zeroMsg.YawRate != 0 {
		t.Fatalf("expected zero-velocity #84 deadman frame, got %+v", afterFrames[len(afterFrames)-1].frame.Message)
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceDiscreteCommandsCommitUnknownAndPreemption(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()

	now := time.Date(2026, 11, 26, 12, 10, 0, 0, time.UTC)
	svc, err := NewService(Options{
		Org:                   "boxbotix",
		Platform:              "bbops",
		Targets:               targets,
		Sender:                sender,
		Store:                 store,
		Now:                   func() time.Time { return now },
		CommandAckTimeout:     1500 * time.Millisecond,
		SupervisoryAckTimeout: 3500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	peer := netip.MustParseAddrPort("127.0.0.1:19453")
	target := mavlink.ActuationTarget{
		SysID:                   3,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.3",
		Domain:                  uxsvocab.DomainSurfaceBoat,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierDirectBench,
		Armed:                   true,
		PeerAddr:                peer,
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets.set(target)

	// 1. MAV_CMD_USER_1 is strictly gated to sitl-local.
	_, err = svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  3,
		Action: ActionSITLCrossCurrent,
		Param1: 0.8,
		Param2: -0.4,
	})
	var ce *ControlError
	if !errors.As(err, &ce) || ce.Code != "sitl_only_action" || ce.Status != http.StatusForbidden {
		t.Fatalf("non-SITL MAV_CMD_USER_1 got err=%v, want 403 sitl_only_action", err)
	}
	if got := countMessages[mavcodec.CommandLong](sender.snapshot()); got != 0 {
		t.Fatalf("blocked SITL command transmitted %d #76 frames, want 0", got)
	}

	// Switch sysid 3 to sitl-local and verify MAV_CMD_USER_1 succeeds and correlates #77 ACCEPTED.
	target.LinkTier = uxsvocab.LinkTierSITLLocal
	targets.set(target)
	sitlRes, err := svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  3,
		Action: ActionSITLCrossCurrent,
		Param1: 0.8,
		Param2: -0.4,
	})
	if err != nil || sitlRes.CmdID != mavcodec.MAVCmdUser1 || sitlRes.Phase != PhasePending {
		t.Fatalf("sitl-local MAV_CMD_USER_1: res=%+v err=%v", sitlRes, err)
	}
	// Duplicate pending command of same cmdID is rejected without sending a second #76.
	if _, err := svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  3,
		Action: ActionSITLCrossCurrent,
	}); !errors.As(err, &ce) || ce.Code != "command_in_flight" {
		t.Fatalf("duplicate pending command got err=%v, want command_in_flight", err)
	}
	// MAV_RESULT_IN_PROGRESS keeps command pending.
	svc.OnCommandAck(3, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdUser1,
		Result:  mavcodec.MAVResultInProgress,
	}, now.Add(50*time.Millisecond))
	if ent, _ := store.get(sitlRes.CommandID); ent.PhaseState != PhasePending {
		t.Fatalf("IN_PROGRESS transitioned phase to %q, want pending", ent.PhaseState)
	}
	// MAV_RESULT_ACCEPTED transitions pending -> confirmed.
	svc.OnCommandAck(3, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdUser1,
		Result:  mavcodec.MAVResultAccepted,
	}, now.Add(80*time.Millisecond))
	if ent, _ := store.get(sitlRes.CommandID); ent.PhaseState != PhaseConfirmed || ent.AckResult != int(mavcodec.MAVResultAccepted) {
		t.Fatalf("ACCEPTED command entity = %+v, want confirmed/0", ent)
	}

	// 2. Dropped #77 COMMAND_ACK: zero blind retries -> commit_unknown -> reconciled on NEXT #0 HEARTBEAT.
	cmdBefore := countMessages[mavcodec.CommandLong](sender.snapshot())
	holdRes, err := svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  3,
		Action: ActionHoldPosition,
	})
	if err != nil {
		t.Fatalf("SubmitCommand hold-position: %v", err)
	}
	// Suppose vehicle already switched to hold-position at t+200ms BEFORE timeout (while #77 was dropped).
	now = now.Add(200 * time.Millisecond)
	target.CompanionMode = uxsvocab.ModeHoldPosition
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)

	// At t+1600ms (past 1500ms timeout), Advance transitions pending -> commit_unknown with ZERO retries.
	now = now.Add(1400 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if gotCmds := countMessages[mavcodec.CommandLong](sender.snapshot()); gotCmds != cmdBefore+1 {
		t.Fatalf("dropped-ACK command sent %d #76 frames, want exactly 1 (zero retries)", gotCmds-cmdBefore)
	}
	entUnknown, _ := store.get(holdRes.CommandID)
	if entUnknown.PhaseState != PhaseCommitUnknown || entUnknown.Detail != CommitUnknownDetail {
		t.Fatalf("timed-out command entity = %+v, want phase=%q detail=%q", entUnknown, PhaseCommitUnknown, CommitUnknownDetail)
	}

	// Another Advance BEFORE a new post-timeout #0 HEARTBEAT must keep commit_unknown!
	now = now.Add(50 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(holdRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("command prematurely reconciled without post-timeout heartbeat: %+v", ent)
	}

	// Next #0 HEARTBEAT arrives after timeout: Advance reconciles commit_unknown -> confirmed.
	now = now.Add(500 * time.Millisecond)
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(holdRes.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("post-timeout heartbeat failed to reconcile commit_unknown -> confirmed: %+v", ent)
	}

	// 3. Hardware preemption revokes active #84 lease, marks pending #76 preempted, and latches until Engage=true.
	target.CompanionMode = uxsvocab.ModeTeleopVelocity
	target.Authority = uxsvocab.AuthorityCompanionTeleop
	targets.set(target)
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:  3,
		Vx:     1.0,
		Engage: true,
	}); err != nil {
		t.Fatalf("SubmitTeleop before preemption: %v", err)
	}
	armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  3,
		Action: ActionModeAutonomous,
	})
	if err != nil {
		t.Fatalf("SubmitCommand before preemption: %v", err)
	}

	now = now.Add(20 * time.Millisecond)
	target.Authority = uxsvocab.AuthorityDirectCRSFOverride
	targets.set(target)
	svc.OnTransitionEvent(mavlink.TransitionEvent{
		Kind:          mavlink.EventKindHeartbeatEdge,
		SysID:         3,
		EntityID:      target.EntityID,
		Authority:     uxsvocab.AuthorityDirectCRSFOverride,
		CompanionMode: uxsvocab.ModeTeleopVelocity,
		LinkTier:      target.LinkTier,
		Armed:         true,
		Timestamp:     now.UnixMilli(),
	})

	if svc.HasActiveLease(3) || !svc.IsPreempted(3) {
		t.Fatalf("after CRSF override: HasActiveLease=%v IsPreempted=%v, want false/true", svc.HasActiveLease(3), svc.IsPreempted(3))
	}
	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhasePreempted {
		t.Fatalf("pending command after CRSF override = %+v, want preempted", ent)
	}

	// Authority returns to companion-teleop, but un-engaged teleop nudges MUST fail until explicit Engage=true!
	now = now.Add(100 * time.Millisecond)
	target.Authority = uxsvocab.AuthorityCompanionTeleop
	targets.set(target)
	svc.OnTransitionEvent(mavlink.TransitionEvent{
		Kind:          mavlink.EventKindHeartbeatEdge,
		SysID:         3,
		EntityID:      target.EntityID,
		Authority:     uxsvocab.AuthorityCompanionTeleop,
		CompanionMode: uxsvocab.ModeTeleopVelocity,
		LinkTier:      target.LinkTier,
		Armed:         true,
		Timestamp:     now.UnixMilli(),
	})
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{SysID: 3, Vx: 0.8}); !errors.As(err, &ce) || ce.Code != "preemption_latched" {
		t.Fatalf("un-engaged teleop after preemption got err=%v, want preemption_latched", err)
	}
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{SysID: 3, Vx: 0.8, Engage: true}); err != nil {
		t.Fatalf("explicit Engage=true teleop after preemption failed: %v", err)
	}
	if svc.IsPreempted(3) || !svc.HasActiveLease(3) {
		t.Fatal("expected explicit Engage=true to clear preemption latch and activate lease")
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestPropertyControlHistoryAgainstReferenceModel(t *testing.T) {
	var (
		sawTeleopAccepted         int
		sawLeaseExpiryZero        int
		sawMeshSupervisoryRefusal int
		sawCommitUnknown          int
		sawHeartbeatReconcile     int
		sawPreemptionLatch        int
		sawExplicitReengage       int
	)

	tiers := []string{
		uxsvocab.LinkTierSITLLocal,
		uxsvocab.LinkTierDirectBench,
		uxsvocab.LinkTierMeshHighRate,
		uxsvocab.LinkTierMeshSupervisory,
	}
	authorities := []string{
		uxsvocab.AuthorityCompanionTeleop,
		uxsvocab.AuthorityCompanionAutonomous,
		uxsvocab.AuthorityDirectCRSFOverride,
		uxsvocab.AuthorityMCUFailsafe,
	}

	for seed := int64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		targets := newFakeTargetSource()
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 26, 13, 0, 0, 0, time.UTC)

		svc, err := NewService(Options{
			Org:                   "boxbotix",
			Platform:              "bbops",
			Targets:               targets,
			Sender:                sender,
			Store:                 store,
			Now:                   func() time.Time { return now },
			TeleopLeaseTTL:        200 * time.Millisecond,
			CommandAckTimeout:     1500 * time.Millisecond,
			SupervisoryAckTimeout: 3500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("seed %d NewService: %v", seed, err)
		}

		peer := netip.MustParseAddrPort("127.0.0.1:19501")
		curTarget := mavlink.ActuationTarget{
			SysID:                   1,
			EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
			Domain:                  uxsvocab.DomainQuadcopter,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			CompanionMode:           uxsvocab.ModeTeleopVelocity,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			Armed:                   true,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: now.UnixNano(),
		}
		targets.set(curTarget)

		var (
			refLeaseActive    bool
			refLeaseExpiresAt time.Time
			refLatched        bool
			refCmdID          string
			refCmdPhase       string
			refCmdDeadline    time.Time
			refCmdUnknownNs   int64
		)

		for step := 0; step < 65; step++ {
			switch rng.Intn(6) {
			case 0: // SubmitTeleop
				engage := rng.Intn(3) == 0
				stop := !engage && rng.Intn(5) == 0
				_, callErr := svc.SubmitTeleop(context.Background(), TeleopRequest{
					SysID:  1,
					Vx:     0.6,
					Engage: engage,
					Stop:   stop,
				})
				if stop {
					refLeaseActive = false
				} else {
					canTeleop := !curTarget.LinkLost &&
						curTarget.LinkTier != uxsvocab.LinkTierMeshSupervisory &&
						curTarget.Armed &&
						curTarget.Authority == uxsvocab.AuthorityCompanionTeleop &&
						(!refLatched || engage)
					if curTarget.LinkTier == uxsvocab.LinkTierMeshSupervisory && !curTarget.LinkLost {
						sawMeshSupervisoryRefusal++
					}
					if canTeleop {
						if callErr != nil {
							t.Fatalf("seed %d step %d expected teleop success, got %v", seed, step, callErr)
						}
						if refLatched && engage {
							sawExplicitReengage++
						}
						refLatched = false
						refLeaseActive = true
						refLeaseExpiresAt = now.Add(200 * time.Millisecond)
						sawTeleopAccepted++
					} else if callErr == nil {
						t.Fatalf("seed %d step %d expected teleop refusal for target=%+v latched=%v engage=%v", seed, step, curTarget, refLatched, engage)
					} else if !curTarget.LinkLost && curTarget.LinkTier != uxsvocab.LinkTierMeshSupervisory &&
						curTarget.Armed && curTarget.Authority == uxsvocab.AuthorityCompanionTeleop && refLatched && !engage {
						sawPreemptionLatch++
					}
				}

			case 1: // SubmitCommand (hold-position)
				res, callErr := svc.SubmitCommand(context.Background(), CommandRequest{
					SysID:  1,
					Action: ActionHoldPosition,
				})
				canCmd := !curTarget.LinkLost &&
					curTarget.Authority != uxsvocab.AuthorityDirectCRSFOverride &&
					(curTarget.Authority != uxsvocab.AuthorityMCUFailsafe || !curTarget.Armed) &&
					refCmdPhase != PhasePending
				if canCmd {
					if callErr != nil {
						t.Fatalf("seed %d step %d expected command success, got %v", seed, step, callErr)
					}
					refCmdID = res.CommandID
					refCmdPhase = PhasePending
					wait := 1500 * time.Millisecond
					if curTarget.LinkTier == uxsvocab.LinkTierMeshSupervisory {
						wait = 3500 * time.Millisecond
					}
					refCmdDeadline = now.Add(wait)
				} else if callErr == nil {
					t.Fatalf("seed %d step %d expected command refusal", seed, step)
				}

			case 2: // Inbound #77 COMMAND_ACK
				if refCmdID != "" && (refCmdPhase == PhasePending || refCmdPhase == PhaseCommitUnknown) {
					accept := rng.Intn(2) == 0
					ackRes := mavcodec.MAVResultDenied
					if accept {
						ackRes = mavcodec.MAVResultAccepted
					}
					svc.OnCommandAck(1, mavcodec.CommandAck{
						Command: mavcodec.MAVCmdNavLoiterUnlim,
						Result:  ackRes,
					}, now)
					if accept {
						refCmdPhase = PhaseConfirmed
					} else {
						refCmdPhase = PhaseRejected
					}
				}

			case 3: // Inbound vehicle state / link transition
				curTarget.LinkLost = rng.Intn(6) == 0
				curTarget.LinkTier = tiers[rng.Intn(len(tiers))]
				curTarget.Authority = authorities[rng.Intn(len(authorities))]
				curTarget.Armed = rng.Intn(5) != 0
				if rng.Intn(2) == 0 {
					curTarget.CompanionMode = uxsvocab.ModeHoldPosition
				} else {
					curTarget.CompanionMode = uxsvocab.ModeTeleopVelocity
				}
				if !curTarget.LinkLost {
					curTarget.HeartbeatObservedUnixNs = now.UnixNano()
				}
				targets.set(curTarget)

				kind := mavlink.EventKindHeartbeatEdge
				if curTarget.LinkLost {
					kind = mavlink.EventKindLinkLost
				}
				svc.OnTransitionEvent(mavlink.TransitionEvent{
					Kind:          kind,
					SysID:         1,
					EntityID:      curTarget.EntityID,
					Authority:     curTarget.Authority,
					CompanionMode: curTarget.CompanionMode,
					LinkTier:      curTarget.LinkTier,
					Armed:         curTarget.Armed,
					Timestamp:     now.UnixMilli(),
				})

				isPreempt := curTarget.LinkLost ||
					curTarget.Authority == uxsvocab.AuthorityDirectCRSFOverride ||
					curTarget.Authority == uxsvocab.AuthorityMCUFailsafe
				if isPreempt {
					refLeaseActive = false
					refLatched = true
					if refCmdPhase == PhasePending || refCmdPhase == PhaseCommitUnknown {
						refCmdPhase = PhasePreempted
					}
				} else {
					if curTarget.LinkTier == uxsvocab.LinkTierMeshSupervisory || !curTarget.Armed || curTarget.Authority != uxsvocab.AuthorityCompanionTeleop {
						refLeaseActive = false
					}
					if refCmdPhase == PhaseCommitUnknown && curTarget.CompanionMode == uxsvocab.ModeHoldPosition {
						refCmdPhase = PhaseConfirmed
						sawHeartbeatReconcile++
					}
				}

			case 4, 5: // Advance clock + service tick
				delta := time.Duration(80+rng.Intn(900)) * time.Millisecond
				now = now.Add(delta)
				if rng.Intn(2) == 0 && !curTarget.LinkLost {
					curTarget.HeartbeatObservedUnixNs = now.UnixNano()
					targets.set(curTarget)
				}
				beforeSetpoints := countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot())
				wasActive := refLeaseActive
				svc.Advance(context.Background(), now)
				if refLeaseActive && !now.Before(refLeaseExpiresAt) {
					refLeaseActive = false
					if wasActive && !curTarget.LinkLost && curTarget.LinkTier != uxsvocab.LinkTierMeshSupervisory && curTarget.Authority == uxsvocab.AuthorityCompanionTeleop {
						if countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot()) == beforeSetpoints+1 {
							sawLeaseExpiryZero++
						}
					}
				}
				if refCmdPhase == PhasePending && !now.Before(refCmdDeadline) {
					refCmdPhase = PhaseCommitUnknown
					refCmdUnknownNs = now.UTC().UnixNano()
					sawCommitUnknown++
				} else if refCmdPhase == PhaseCommitUnknown && !curTarget.LinkLost &&
					curTarget.HeartbeatObservedUnixNs > refCmdUnknownNs &&
					curTarget.CompanionMode == uxsvocab.ModeHoldPosition {
					refCmdPhase = PhaseConfirmed
					sawHeartbeatReconcile++
				}
			}

			if gotLease := svc.HasActiveLease(1); gotLease != refLeaseActive {
				t.Fatalf("seed %d step %d HasActiveLease=%v, ref=%v", seed, step, gotLease, refLeaseActive)
			}
			if gotLatched := svc.IsPreempted(1); gotLatched != refLatched {
				t.Fatalf("seed %d step %d IsPreempted=%v, ref=%v", seed, step, gotLatched, refLatched)
			}
			if refCmdID != "" {
				ent, ok := store.get(refCmdID)
				if !ok || ent.PhaseState != refCmdPhase {
					t.Fatalf("seed %d step %d command %s phase=%q, ref=%q", seed, step, refCmdID, ent.PhaseState, refCmdPhase)
				}
			}
		}
	}

	if sawTeleopAccepted == 0 || sawLeaseExpiryZero == 0 || sawMeshSupervisoryRefusal == 0 ||
		sawCommitUnknown == 0 || sawHeartbeatReconcile == 0 || sawPreemptionLatch == 0 || sawExplicitReengage == 0 {
		t.Fatalf("insufficient property boundary reach: teleop=%d expiryZero=%d meshRefusal=%d unknown=%d reconcile=%d latch=%d reengage=%d",
			sawTeleopAccepted, sawLeaseExpiryZero, sawMeshSupervisoryRefusal,
			sawCommitUnknown, sawHeartbeatReconcile, sawPreemptionLatch, sawExplicitReengage)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestHTTPControlHandlersValidationAndEnvelopes(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC)

	svc, err := NewService(Options{
		Org:      "boxbotix",
		Platform: "bbops",
		Targets:  targets,
		Sender:   sender,
		Store:    store,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	targets.set(mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19601"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	})

	// Unknown JSON field rejected with 400 invalid_json.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, CommandFrontDoorPath, strings.NewReader(`{"sysid":1,"action":"arm","extra":true}`))
	svc.ServeCommand(rec, req, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown JSON field status = %d, want 400", rec.Code)
	}

	// Oversized JSON body (> 16 KiB) rejected with 400 request_too_large.
	bigBody := `{"sysid":1,"action":"` + strings.Repeat("a", MaxControlBodyBytes+64) + `"}`
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, CommandFrontDoorPath, strings.NewReader(bigBody))
	svc.ServeCommand(rec, req, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "request_too_large") {
		t.Fatalf("oversized body status=%d body=%s, want 400 request_too_large", rec.Code, rec.Body.String())
	}

	// Valid POST /control/command returns 200 ok=true envelope.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, CommandFrontDoorPath, bytes.NewBufferString(`{"sysid":1,"action":"hold-position"}`))
	svc.ServeCommand(rec, req, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ServeCommand valid status=%d body=%s", rec.Code, rec.Body.String())
	}
	var cmdEnv Envelope[CommandResult]
	if err := json.Unmarshal(rec.Body.Bytes(), &cmdEnv); err != nil || !cmdEnv.OK || cmdEnv.Result == nil || cmdEnv.Result.Phase != PhasePending {
		t.Fatalf("ServeCommand envelope = %+v err=%v", cmdEnv, err)
	}

	// Valid POST /control/teleop returns 200 ok=true envelope.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, TeleopFrontDoorPath, bytes.NewBufferString(`{"sysid":1,"vx":0.9,"engage":true}`))
	svc.ServeTeleop(rec, req, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ServeTeleop valid status=%d body=%s", rec.Code, rec.Body.String())
	}
	var telEnv Envelope[TeleopResult]
	if err := json.Unmarshal(rec.Body.Bytes(), &telEnv); err != nil || !telEnv.OK || telEnv.Result == nil || !telEnv.Result.LeaseActive {
		t.Fatalf("ServeTeleop envelope = %+v err=%v", telEnv, err)
	}
}
