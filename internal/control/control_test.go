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
	onSend  func(frame mavcodec.Frame, peer netip.AddrPort) error
}

func (f *fakeFrameSender) SendFrame(frame mavcodec.Frame, peer netip.AddrPort) error {
	f.mu.Lock()
	hook := f.onSend
	sendErr := f.sendErr
	f.mu.Unlock()
	if hook != nil {
		if err := hook(frame, peer); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if sendErr != nil {
		return sendErr
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
	mu           sync.Mutex
	entities     map[string]CommandEntity
	transitions  []storeTransition
	listErr      error
	afterCreate  func(cmd CommandEntity)
	onTransition func(entityID, newPhase string) error
}

var _ RecoverableCommandStore = (*fakeCommandStore)(nil)

func newFakeCommandStore() *fakeCommandStore {
	return &fakeCommandStore{
		entities: make(map[string]CommandEntity),
	}
}

func (s *fakeCommandStore) Create(_ context.Context, cmd *CommandEntity) error {
	s.mu.Lock()
	if _, exists := s.entities[cmd.ID]; exists {
		s.mu.Unlock()
		return errors.New("duplicate entity ID")
	}
	if !Workflow().Transitions.IsValidTransition(cmd.PhaseState, PhaseConfirmed) && cmd.PhaseState != PhasePending {
		s.mu.Unlock()
		return errors.New("unexpected initial phase")
	}
	cp := *cmd
	s.entities[cmd.ID] = cp
	hook := s.afterCreate
	s.mu.Unlock()
	if hook != nil {
		hook(cp)
	}
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
	hook := s.onTransition
	s.mu.Unlock()
	if hook != nil {
		if err := hook(entityID, newPhase); err != nil {
			return err
		}
	}

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

func (s *fakeCommandStore) Get(_ context.Context, entityID string) (*CommandEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entities[entityID]
	if !ok {
		return nil, errors.New("unknown entity ID")
	}
	cp := e
	return &cp, nil
}

func (s *fakeCommandStore) ListNonTerminal(_ context.Context) ([]CommandEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []CommandEntity
	for _, e := range s.entities {
		if !e.IsTerminal() {
			out = append(out, e)
		}
	}
	return out, nil
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
			refCmdAmbiguous   bool
			refUnackedCount   int
			refCmdDeadline    time.Time
			refCmdUnknownNs   int64
		)

		for step := 0; step < 65; step++ {
			switch rng.Intn(6) {
			case 0: // SubmitTeleop
				engage := rng.Intn(2) == 0
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
						(refLeaseActive || engage) &&
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
						t.Fatalf("seed %d step %d expected teleop refusal for target=%+v latched=%v active=%v engage=%v", seed, step, curTarget, refLatched, refLeaseActive, engage)
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
					refCmdAmbiguous = refUnackedCount > 0
					refUnackedCount++
					hbFloor := now.UTC().UnixNano()
					if curTarget.HeartbeatObservedUnixNs > hbFloor {
						hbFloor = curTarget.HeartbeatObservedUnixNs
					}
					refCmdUnknownNs = hbFloor
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
					if refCmdAmbiguous || refUnackedCount > 1 {
						refCmdAmbiguous = true
						refCmdPhase = PhaseCommitUnknown
						hbFloor := now.UTC().UnixNano()
						if curTarget.HeartbeatObservedUnixNs > hbFloor {
							hbFloor = curTarget.HeartbeatObservedUnixNs
						}
						if hbFloor > refCmdUnknownNs {
							refCmdUnknownNs = hbFloor
						}
					} else {
						refUnackedCount = 0
						if accept {
							refCmdPhase = PhaseConfirmed
						} else {
							refCmdPhase = PhaseRejected
						}
					}
				}

			case 3: // Inbound vehicle state / link transition
				now = now.Add(5 * time.Millisecond)
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
					curTarget.HeartbeatObservedUnixNs = now.UTC().UnixNano()
				}
				targets.set(curTarget)

				kind := mavlink.EventKindHeartbeatEdge
				if curTarget.LinkLost {
					kind = mavlink.EventKindLinkLost
				}
				svc.OnTransitionEvent(mavlink.TransitionEvent{
					Kind:          kind,
					MsgID:         mavcodec.MsgIDHeartbeat,
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
					refCmdAmbiguous = false
					if refCmdPhase == PhasePending || refCmdPhase == PhaseCommitUnknown {
						refCmdPhase = PhasePreempted
					}
				} else {
					if curTarget.LinkTier == uxsvocab.LinkTierMeshSupervisory || !curTarget.Armed || curTarget.Authority != uxsvocab.AuthorityCompanionTeleop {
						refLeaseActive = false
					}
					if refCmdPhase == PhaseCommitUnknown &&
						curTarget.HeartbeatObservedUnixNs > refCmdUnknownNs &&
						curTarget.CompanionMode == uxsvocab.ModeHoldPosition {
						refCmdPhase = PhaseConfirmed
						sawHeartbeatReconcile++
					}
				}

			case 4, 5: // Advance clock + service tick
				delta := time.Duration(80+rng.Intn(900)) * time.Millisecond
				now = now.Add(delta)
				if rng.Intn(2) == 0 && !curTarget.LinkLost {
					curTarget.HeartbeatObservedUnixNs = now.UTC().UnixNano()
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
					hbFloor := now.UTC().UnixNano()
					if curTarget.HeartbeatObservedUnixNs > hbFloor {
						hbFloor = curTarget.HeartbeatObservedUnixNs
					}
					if hbFloor > refCmdUnknownNs {
						refCmdUnknownNs = hbFloor
					}
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

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
// spec: uxs-control / Pin-on-admission MAVLink v2 UDP command egress and 1 Hz GCS liveness
func TestServicePreemptionAndPeerChangeFenceBeforeTransmit(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)

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

	peerA := netip.MustParseAddrPort("127.0.0.1:19701")
	peerB := netip.MustParseAddrPort("127.0.0.1:19702")
	baseTarget := mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                peerA,
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets.set(baseTarget)

	// 1a. #76 COMMAND_LONG: RC override arrives after Create commits and before Create returns.
	var capturedCmdID string
	store.afterCreate = func(cmd CommandEntity) {
		capturedCmdID = cmd.ID
		tg := baseTarget
		tg.Authority = uxsvocab.AuthorityDirectCRSFOverride
		targets.set(tg)
		svc.OnTransitionEvent(mavlink.TransitionEvent{
			Kind:          mavlink.EventKindHeartbeatEdge,
			MsgID:         mavcodec.MsgIDHeartbeat,
			SysID:         1,
			EntityID:      tg.EntityID,
			Authority:     uxsvocab.AuthorityDirectCRSFOverride,
			CompanionMode: uxsvocab.ModeHoldPosition,
			LinkTier:      tg.LinkTier,
			Armed:         true,
			Timestamp:     now.Add(5 * time.Millisecond).UnixMilli(),
		})
	}
	_, err = svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  1,
		Action: ActionHoldPosition,
	})
	var ce *ControlError
	if !errors.As(err, &ce) || ce.Code != "authority_preempted" {
		t.Fatalf("SubmitCommand preempted during Create got err=%v, want authority_preempted", err)
	}
	if got := countMessages[mavcodec.CommandLong](sender.snapshot()); got != 0 {
		t.Fatalf("preempted SubmitCommand transmitted %d COMMAND_LONG frames, want 0", got)
	}
	if ent, ok := store.get(capturedCmdID); !ok || ent.PhaseState != PhasePreempted {
		t.Fatalf("stored command after Create-time preemption = %+v (ok=%v), want phase=%q", ent, ok, PhasePreempted)
	}

	// 1b. #76 COMMAND_LONG: pinned peer changes while Create commits.
	targets.set(baseTarget)
	store.afterCreate = func(cmd CommandEntity) {
		capturedCmdID = cmd.ID
		tg := baseTarget
		tg.PeerAddr = peerB
		targets.set(tg)
	}
	_, err = svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  1,
		Action: ActionHoldPosition,
	})
	if !errors.As(err, &ce) || ce.Code != "vehicle_peer_changed" {
		t.Fatalf("SubmitCommand peer-changed during Create got err=%v, want vehicle_peer_changed", err)
	}
	if got := countMessages[mavcodec.CommandLong](sender.snapshot()); got != 0 {
		t.Fatalf("peer-changed SubmitCommand transmitted %d COMMAND_LONG frames, want 0", got)
	}
	if ent, ok := store.get(capturedCmdID); !ok || ent.PhaseState != PhaseRejected {
		t.Fatalf("stored command after Create-time peer change = %+v (ok=%v), want phase=%q", ent, ok, PhaseRejected)
	}
	store.afterCreate = nil

	// 1c. #84 SET_POSITION_TARGET_LOCAL_NED: preemption arrives right before #84 write (during initial GCS #0 HEARTBEAT send).
	now = now.Add(2 * time.Second)
	targets.set(baseTarget)
	sender.onSend = func(frame mavcodec.Frame, _ netip.AddrPort) error {
		if _, isHB := frame.Message.(mavcodec.Heartbeat); isHB {
			tg := baseTarget
			tg.Authority = uxsvocab.AuthorityDirectCRSFOverride
			targets.set(tg)
			svc.OnTransitionEvent(mavlink.TransitionEvent{
				Kind:          mavlink.EventKindHeartbeatEdge,
				MsgID:         mavcodec.MsgIDHeartbeat,
				SysID:         1,
				EntityID:      tg.EntityID,
				Authority:     uxsvocab.AuthorityDirectCRSFOverride,
				CompanionMode: uxsvocab.ModeHoldPosition,
				LinkTier:      tg.LinkTier,
				Armed:         true,
				Timestamp:     now.UnixMilli(),
			})
		}
		return nil
	}
	_, err = svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:  1,
		Vx:     2.5,
		Engage: true,
	})
	if !errors.As(err, &ce) || (ce.Code != "authority_not_teleop" && ce.Code != "preemption_latched") {
		t.Fatalf("SubmitTeleop preempted before #84 write got err=%v, want authority_not_teleop/preemption_latched", err)
	}
	if got := countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot()); got != 0 {
		t.Fatalf("preempted SubmitTeleop transmitted %d #84 frames, want 0", got)
	}
	if svc.HasActiveLease(1) || !svc.IsPreempted(1) {
		t.Fatalf("after pre-write preemption: HasActiveLease=%v IsPreempted=%v, want false/true", svc.HasActiveLease(1), svc.IsPreempted(1))
	}

	// 1d. #84 SET_POSITION_TARGET_LOCAL_NED: pinned peer changes right before #84 write.
	now = now.Add(2 * time.Second)
	targets.set(baseTarget)
	sender.onSend = func(frame mavcodec.Frame, _ netip.AddrPort) error {
		if _, isHB := frame.Message.(mavcodec.Heartbeat); isHB {
			tg := baseTarget
			tg.PeerAddr = peerB
			targets.set(tg)
		}
		return nil
	}
	_, err = svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:  1,
		Vx:     2.5,
		Engage: true,
	})
	if !errors.As(err, &ce) || ce.Code != "vehicle_peer_changed" {
		t.Fatalf("SubmitTeleop peer-changed before #84 write got err=%v, want vehicle_peer_changed", err)
	}
	if got := countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot()); got != 0 {
		t.Fatalf("peer-changed SubmitTeleop transmitted %d #84 frames, want 0", got)
	}
	if svc.HasActiveLease(1) {
		t.Fatal("expected no active lease after pre-write peer change")
	}
	sender.onSend = nil
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceAmbiguousAckAndEmergencyDisarm(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 1, 10, 0, 0, time.UTC)

	svc, err := NewService(Options{
		Org:               "boxbotix",
		Platform:          "bbops",
		Targets:           targets,
		Sender:            sender,
		Store:             store,
		Now:               func() time.Time { return now },
		CommandAckTimeout: 1500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	target := mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeDisarmed,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   false,
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19711"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets.set(target)

	// 1. Arm (400) times out -> Disarm (400) submitted -> delayed Arm ACK (400, ACCEPTED) arrives.
	// Neither Arm nor Disarm may become confirmed until fresh vehicle-state evidence arrives!
	armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  1,
		Action: ActionArm,
	})
	if err != nil {
		t.Fatalf("SubmitCommand arm: %v", err)
	}

	now = now.Add(1600 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("timed-out Arm phase = %q, want %q", ent.PhaseState, PhaseCommitUnknown)
	}

	now = now.Add(50 * time.Millisecond)
	disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  1,
		Action: ActionDisarm,
	})
	if err != nil {
		t.Fatalf("SubmitCommand disarm after arm timeout: %v", err)
	}

	// Deliver delayed Arm ACK (400, ACCEPTED) while vehicle actually armed: Disarm MUST NOT confirm!
	now = now.Add(50 * time.Millisecond)
	target.Armed = true
	target.CompanionMode = uxsvocab.ModeTeleopVelocity
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)

	svc.OnCommandAck(1, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}, now)

	armEnt, _ := store.get(armRes.CommandID)
	disarmEnt, _ := store.get(disarmRes.CommandID)
	if disarmEnt.PhaseState == PhaseConfirmed {
		t.Fatalf("delayed Arm ACK falsely confirmed Disarm: %+v", disarmEnt)
	}
	if armEnt.PhaseState != PhaseCommitUnknown || disarmEnt.PhaseState != PhaseCommitUnknown {
		t.Fatalf("expected both Arm and Disarm to preserve commit_unknown ambiguity, got arm=%q disarm=%q", armEnt.PhaseState, disarmEnt.PhaseState)
	}

	// Fresh HEARTBEAT still reporting Armed=true keeps Disarm in commit_unknown.
	now = now.Add(200 * time.Millisecond)
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("Disarm confirmed while vehicle still Armed=true: %+v", ent)
	}

	// Fresh HEARTBEAT reporting Armed=false confirms Disarm while superseded Arm stays commit_unknown.
	now = now.Add(200 * time.Millisecond)
	target.Armed = false
	target.CompanionMode = uxsvocab.ModeDisarmed
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("fresh disarmed HEARTBEAT failed to confirm Disarm: %+v", ent)
	}
	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("superseded Arm phase = %q, want %q", ent.PhaseState, PhaseCommitUnknown)
	}

	// 2. Emergency Disarm MUST remain available even while Arm (400) is still PhasePending!
	now = now.Add(500 * time.Millisecond)
	arm2Res, err := svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  1,
		Action: ActionArm,
	})
	if err != nil {
		t.Fatalf("SubmitCommand second arm: %v", err)
	}
	now = now.Add(20 * time.Millisecond)
	emergDisarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  1,
		Action: ActionDisarm,
	})
	if err != nil {
		t.Fatalf("Emergency Disarm blocked while Arm pending: %v", err)
	}
	if ent, _ := store.get(arm2Res.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("superseded pending Arm phase = %q, want %q", ent.PhaseState, PhaseCommitUnknown)
	}
	// Ambiguous 400 ACK still cannot confirm Emergency Disarm without fresh disarmed HEARTBEAT.
	now = now.Add(20 * time.Millisecond)
	svc.OnCommandAck(1, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}, now)
	if ent, _ := store.get(emergDisarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("ambiguous ACK confirmed Emergency Disarm without heartbeat: %+v", ent)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceDeadmanDecoupledFromBlockedCommandPersistence(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()

	svc, err := NewService(Options{
		Org:               "boxbotix",
		Platform:          "bbops",
		Targets:           targets,
		Sender:            sender,
		Store:             store,
		TeleopLeaseTTL:    160 * time.Millisecond,
		TickInterval:      15 * time.Millisecond,
		CommandAckTimeout: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	peerA := netip.MustParseAddrPort("127.0.0.1:19721")
	peerB := netip.MustParseAddrPort("127.0.0.1:19722")
	targets.set(mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                peerA,
		HeartbeatObservedUnixNs: time.Now().UTC().UnixNano(),
	})
	targets.set(mavlink.ActuationTarget{
		SysID:                   2,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.2",
		Domain:                  uxsvocab.DomainRover,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                peerB,
		HeartbeatObservedUnixNs: time.Now().UTC().UnixNano(),
	})

	enteredBlockedWrite := make(chan struct{})
	releaseBlockedWrite := make(chan struct{})
	var blockOnce sync.Once
	store.onTransition = func(_ string, newPhase string) error {
		if newPhase == PhaseCommitUnknown {
			blockOnce.Do(func() {
				close(enteredBlockedWrite)
			})
			<-releaseBlockedWrite
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("svc.Start: %v", err)
	}
	defer func() {
		close(releaseBlockedWrite)
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = svc.Stop(stopCtx)
	}()

	// Trigger Vehicle A (#1) command timeout so command persistence blocks inside store.Transition.
	if _, err := svc.SubmitCommand(ctx, CommandRequest{
		SysID:  1,
		Action: ActionHoldPosition,
	}); err != nil {
		t.Fatalf("SubmitCommand vehicle 1: %v", err)
	}
	select {
	case <-enteredBlockedWrite:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vehicle 1 timeout Transition to block")
	}

	// While Vehicle A's timeout write remains blocked, send one nudge on Vehicle B (#2)
	// and assert its single zero-velocity frame arrives within < 250ms!
	nudgeStart := time.Now()
	if _, err := svc.SubmitTeleop(ctx, TeleopRequest{
		SysID:  2,
		Vx:     1.8,
		Engage: true,
	}); err != nil {
		t.Fatalf("SubmitTeleop vehicle 2 while vehicle 1 blocked: %v", err)
	}

	deadline := nudgeStart.Add(245 * time.Millisecond)
	var zeroArrivedAt time.Time
	for time.Now().Before(deadline) {
		var zeroCount int
		for _, rec := range sender.snapshot() {
			sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED)
			if ok && sp.TargetSystem == 2 && sp.Vx == 0 && sp.Vy == 0 && sp.Vz == 0 && sp.YawRate == 0 {
				zeroCount++
			}
		}
		if zeroCount > 0 && !svc.HasActiveLease(2) {
			zeroArrivedAt = time.Now()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if zeroArrivedAt.IsZero() {
		t.Fatalf("vehicle 2 deadman missed <250ms deadline while vehicle 1 store write remained blocked (HasActiveLease=%v)", svc.HasActiveLease(2))
	}
	if elapsed := zeroArrivedAt.Sub(nudgeStart); elapsed >= 250*time.Millisecond {
		t.Fatalf("vehicle 2 deadman zero frame arrived after %v, want < 250ms", elapsed)
	}

	// Verify Vehicle A's write is STILL blocked right now (proving genuine concurrency decoupling)
	// and only a single zero frame was emitted for Vehicle B.
	select {
	case <-releaseBlockedWrite:
		t.Fatal("releaseBlockedWrite closed prematurely")
	default:
	}
	var bZeroCount int
	for _, rec := range sender.snapshot() {
		sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED)
		if ok && sp.TargetSystem == 2 && sp.Vx == 0 && sp.Vy == 0 && sp.Vz == 0 && sp.YawRate == 0 {
			bZeroCount++
		}
	}
	if bZeroCount != 1 {
		t.Fatalf("vehicle 2 deadman zero frames = %d, want 1", bZeroCount)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceTeleopSessionAndSequenceFencing(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 1, 20, 0, 0, time.UTC)

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
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19731"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	})

	// Engage session "sess-1" at seq=1, send seq=3, then reject out-of-order seq=2!
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "sess-1",
		Seq:     1,
		Vx:      0,
		Engage:  true,
	}); err != nil {
		t.Fatalf("engage sess-1: %v", err)
	}
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "sess-1",
		Seq:     3,
		Vx:      0.5,
	}); err != nil {
		t.Fatalf("nudge seq=3: %v", err)
	}
	var ce *ControlError
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "sess-1",
		Seq:     2,
		Vx:      3.5,
	}); !errors.As(err, &ce) || ce.Code != "teleop_seq_superseded" {
		t.Fatalf("superseded nudge seq=2 got err=%v, want teleop_seq_superseded", err)
	}

	// Stop arrives BEFORE an in-flight older nudge (both with and without session token):
	// the delayed nudge MUST NOT reopen the stopped lease or emit nonzero velocity!
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "sess-1",
		Seq:     5,
		Stop:    true,
	}); err != nil {
		t.Fatalf("stop sess-1: %v", err)
	}
	countAfterStop := countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot())

	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "sess-1",
		Seq:     4,
		Vx:      2.8,
	}); !errors.As(err, &ce) || ce.Code != "teleop_session_stopped" {
		t.Fatalf("post-stop delayed nudge (session) got err=%v, want teleop_session_stopped", err)
	}
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID: 1,
		Vx:    2.8,
	}); !errors.As(err, &ce) || ce.Code != "teleop_not_engaged" {
		t.Fatalf("post-stop delayed nudge (sessionless) got err=%v, want teleop_not_engaged", err)
	}
	if svc.HasActiveLease(1) {
		t.Fatal("stopped lease was reopened by delayed nudge")
	}
	if got := countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot()); got != countAfterStop {
		t.Fatalf("delayed nudges emitted %d extra #84 frames after Stop, want 0", got-countAfterStop)
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceStatustextDoesNotReconcileCommitUnknown(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 1, 30, 0, 0, time.UTC)

	svc, err := NewService(Options{
		Org:               "boxbotix",
		Platform:          "bbops",
		Targets:           targets,
		Sender:            sender,
		Store:             store,
		Now:               func() time.Time { return now },
		CommandAckTimeout: 1500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	target := mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeHoldPosition, // cached Hold state prior to timeout
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19741"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets.set(target)

	res, err := svc.SubmitCommand(context.Background(), CommandRequest{
		SysID:  1,
		Action: ActionHoldPosition,
	})
	if err != nil {
		t.Fatalf("SubmitCommand hold-position: %v", err)
	}

	now = now.Add(1600 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(res.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("expected commit_unknown at timeout, got %q", ent.PhaseState)
	}

	// Deliver STATUSTEXT (#253), SYS_STATUS (#1), and non-heartbeat link_restored (#31) carrying cached Hold state:
	// none of these is a fresh post-timeout #0 HEARTBEAT, so command MUST stay commit_unknown!
	now = now.Add(100 * time.Millisecond)
	for _, ev := range []mavlink.TransitionEvent{
		{
			Kind:          mavlink.EventKindStatusText,
			MsgID:         mavcodec.MsgIDStatusText,
			SysID:         1,
			EntityID:      target.EntityID,
			Authority:     uxsvocab.AuthorityCompanionTeleop,
			CompanionMode: uxsvocab.ModeHoldPosition,
			LinkTier:      target.LinkTier,
			Armed:         true,
			Timestamp:     now.UnixMilli(),
		},
		{
			Kind:          mavlink.EventKindFailsafeIncrement,
			MsgID:         mavcodec.MsgIDSysStatus,
			SysID:         1,
			EntityID:      target.EntityID,
			Authority:     uxsvocab.AuthorityCompanionTeleop,
			CompanionMode: uxsvocab.ModeHoldPosition,
			LinkTier:      target.LinkTier,
			Armed:         true,
			Timestamp:     now.UnixMilli(),
		},
		{
			Kind:          mavlink.EventKindLinkRestored,
			MsgID:         mavcodec.MsgIDAttitudeQuaternion,
			SysID:         1,
			EntityID:      target.EntityID,
			Authority:     uxsvocab.AuthorityCompanionTeleop,
			CompanionMode: uxsvocab.ModeHoldPosition,
			LinkTier:      target.LinkTier,
			Armed:         true,
			Timestamp:     now.UnixMilli(),
		},
	} {
		svc.OnTransitionEvent(ev)
		if ent, _ := store.get(res.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("non-heartbeat event %s/%d falsely confirmed Hold: %+v", ev.Kind, ev.MsgID, ent)
		}
	}

	// Even a HeartbeatEdge event whose HeartbeatObservedUnixNs did NOT advance past timeout cannot confirm.
	svc.OnTransitionEvent(mavlink.TransitionEvent{
		Kind:          mavlink.EventKindHeartbeatEdge,
		MsgID:         mavcodec.MsgIDHeartbeat,
		SysID:         1,
		EntityID:      target.EntityID,
		Authority:     uxsvocab.AuthorityCompanionTeleop,
		CompanionMode: uxsvocab.ModeHoldPosition,
		LinkTier:      target.LinkTier,
		Armed:         true,
		Timestamp:     now.UnixMilli(),
	})
	if ent, _ := store.get(res.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("stale-timestamp HeartbeatEdge confirmed Hold without post-timeout heartbeat: %+v", ent)
	}

	// Fresh post-timeout #0 HEARTBEAT updates HeartbeatObservedUnixNs -> confirms Hold!
	now = now.Add(200 * time.Millisecond)
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc.OnTransitionEvent(mavlink.TransitionEvent{
		Kind:          mavlink.EventKindHeartbeatEdge,
		MsgID:         mavcodec.MsgIDHeartbeat,
		SysID:         1,
		EntityID:      target.EntityID,
		Authority:     uxsvocab.AuthorityCompanionTeleop,
		CompanionMode: uxsvocab.ModeHoldPosition,
		LinkTier:      target.LinkTier,
		Armed:         true,
		Timestamp:     now.UnixMilli(),
	})
	if ent, _ := store.get(res.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("fresh post-timeout HeartbeatEdge failed to confirm Hold: %+v", ent)
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceTransientPersistenceFailureRetainedAcrossAckTimeoutAndPreempt(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 1, 40, 0, 0, time.UTC)

	svc, err := NewService(Options{
		Org:               "boxbotix",
		Platform:          "bbops",
		Targets:           targets,
		Sender:            sender,
		Store:             store,
		Now:               func() time.Time { return now },
		CommandAckTimeout: 1500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	target := mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19751"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets.set(target)

	// 1. Fail ACK Transition write, restore store, advance past deadline -> durable record reaches confirmed with 0 retries.
	cmd1, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
	if err != nil {
		t.Fatalf("SubmitCommand 1: %v", err)
	}
	sentAfterCmd1 := countMessages[mavcodec.CommandLong](sender.snapshot())

	store.onTransition = func(_ string, _ string) error {
		return errors.New("transient KV timeout")
	}
	now = now.Add(100 * time.Millisecond)
	svc.OnCommandAck(1, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdNavLoiterUnlim,
		Result:  mavcodec.MAVResultAccepted,
	}, now)
	if ent, _ := store.get(cmd1.CommandID); ent.PhaseState != PhasePending {
		t.Fatalf("expected failed write to leave store pending temporarily, got %q", ent.PhaseState)
	}

	store.onTransition = nil
	now = now.Add(2 * time.Second) // past ackDeadline
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(cmd1.CommandID); ent.PhaseState != PhaseConfirmed || ent.AckResult != int(mavcodec.MAVResultAccepted) {
		t.Fatalf("restored store after ACK write failure = %+v, want confirmed", ent)
	}
	if got := countMessages[mavcodec.CommandLong](sender.snapshot()); got != sentAfterCmd1 {
		t.Fatalf("persistence retry retransmitted %d #76 frames, want 0", got-sentAfterCmd1)
	}

	// 2. Fail timeout Transition write, restore store, advance -> durable record reaches commit_unknown.
	cmd2, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionModeAutonomous})
	if err != nil {
		t.Fatalf("SubmitCommand 2: %v", err)
	}
	store.onTransition = func(_ string, _ string) error {
		return errors.New("transient KV timeout on commit_unknown")
	}
	now = now.Add(1600 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(cmd2.CommandID); ent.PhaseState != PhasePending {
		t.Fatalf("expected failed timeout write to leave store pending temporarily, got %q", ent.PhaseState)
	}
	store.onTransition = nil
	now = now.Add(50 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(cmd2.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("restored store after timeout write failure = %+v, want commit_unknown", ent)
	}

	// 3. Fail preemption Transition write, restore store, advance -> durable record reaches preempted.
	now = now.Add(50 * time.Millisecond)
	store.onTransition = func(_ string, _ string) error {
		return errors.New("transient KV timeout on preempted")
	}
	tgPreempt := target
	tgPreempt.Authority = uxsvocab.AuthorityDirectCRSFOverride
	targets.set(tgPreempt)
	svc.OnTransitionEvent(mavlink.TransitionEvent{
		Kind:          mavlink.EventKindHeartbeatEdge,
		MsgID:         mavcodec.MsgIDHeartbeat,
		SysID:         1,
		EntityID:      target.EntityID,
		Authority:     uxsvocab.AuthorityDirectCRSFOverride,
		CompanionMode: uxsvocab.ModeHoldPosition,
		LinkTier:      target.LinkTier,
		Armed:         true,
		Timestamp:     now.UnixMilli(),
	})
	if ent, _ := store.get(cmd2.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("expected failed preempt write to stay commit_unknown temporarily, got %q", ent.PhaseState)
	}
	store.onTransition = nil
	now = now.Add(50 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(cmd2.CommandID); ent.PhaseState != PhasePreempted {
		t.Fatalf("restored store after preempt write failure = %+v, want preempted", ent)
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceRestartSafeIDsAndIndeterminateRecovery(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	sharedStore := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 1, 50, 0, 0, time.UTC)

	target := mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19761"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets.set(target)

	svc1, err := NewService(Options{
		Org:      "boxbotix",
		Platform: "bbops",
		Targets:  targets,
		Sender:   sender,
		Store:    sharedStore,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewService 1: %v", err)
	}

	// Complete one command on svc1 and leave a second command pending before restart.
	doneCmd, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionModeAutonomous})
	if err != nil {
		t.Fatalf("svc1 SubmitCommand 1: %v", err)
	}
	svc1.OnCommandAck(1, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdDoSetMode,
		Result:  mavcodec.MAVResultAccepted,
	}, now)

	pendingBeforeRestart, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
	if err != nil {
		t.Fatalf("svc1 SubmitCommand 2: %v", err)
	}
	sentBeforeRestart := countMessages[mavcodec.CommandLong](sender.snapshot())

	// Construct svc2 on the SAME store at the exact same clock instant:
	// 1) pre-restart pending command recovers as commit_unknown with 0 retransmits,
	// 2) first new command on svc2 uses a distinct restart-safe ID (never duplicate entity ID),
	// 3) fresh post-restart HEARTBEAT reconciles the recovered command!
	svc2, err := NewService(Options{
		Org:      "boxbotix",
		Platform: "bbops",
		Targets:  targets,
		Sender:   sender,
		Store:    sharedStore,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewService 2: %v", err)
	}
	svc2.Advance(context.Background(), now)

	if got := countMessages[mavcodec.CommandLong](sender.snapshot()); got != sentBeforeRestart {
		t.Fatalf("svc2 restart recovery retransmitted %d #76 frames, want 0", got-sentBeforeRestart)
	}
	if ent, _ := sharedStore.get(pendingBeforeRestart.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("recovered pre-restart pending command = %+v, want commit_unknown", ent)
	}

	postRestartCmd, err := svc2.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
	if err != nil {
		t.Fatalf("svc2 first command after restart failed (ID collision?): %v", err)
	}
	if postRestartCmd.CommandID == doneCmd.CommandID || postRestartCmd.CommandID == pendingBeforeRestart.CommandID {
		t.Fatalf("svc2 reused pre-restart command ID: %s", postRestartCmd.CommandID)
	}

	now = now.Add(200 * time.Millisecond)
	target.CompanionMode = uxsvocab.ModeHoldPosition
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc2.Advance(context.Background(), now)
	if ent, _ := sharedStore.get(pendingBeforeRestart.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("post-restart fresh HEARTBEAT failed to reconcile recovered command: %+v", ent)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceFloat32WireRangeValidation(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 2, 0, 0, 0, time.UTC)

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
		Domain:                  uxsvocab.DomainSurfaceBoat,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19771"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	})

	posOverflow := math.Nextafter(float64(math.MaxFloat32)*2, math.Inf(1))
	negOverflow := -posOverflow
	// Exact IEEE-754 binary32 rounding overflow boundary (~3.4028236e+38 -> float32(+Inf)).
	posBoundaryOverflow := 3.4028236e+38
	negBoundaryOverflow := -3.4028236e+38

	for _, bad := range []TeleopRequest{
		{SysID: 1, Vx: posBoundaryOverflow, Engage: true},
		{SysID: 1, Vx: negBoundaryOverflow, Engage: true},
		{SysID: 1, Vy: posOverflow, Engage: true},
		{SysID: 1, Vz: negOverflow, Engage: true},
		{SysID: 1, YawRate: 1e308, Engage: true},
		{SysID: 1, YawRate: -1e308, Engage: true},
	} {
		var ce *ControlError
		if _, err := svc.SubmitTeleop(context.Background(), bad); !errors.As(err, &ce) || ce.Code != "non_finite_value" {
			t.Fatalf("SubmitTeleop(%+v) got err=%v, want non_finite_value", bad, err)
		}
		if svc.HasActiveLease(1) {
			t.Fatalf("out-of-range float32 teleop granted lease: %+v", bad)
		}
	}
	if got := len(sender.snapshot()); got != 0 {
		t.Fatalf("out-of-range teleop requests sent %d frames, want 0", got)
	}

	for _, badCmd := range []CommandRequest{
		{SysID: 1, Action: ActionSITLCompanionBlackout, Param1: posBoundaryOverflow},
		{SysID: 1, Action: ActionSITLCompanionBlackout, Param1: negBoundaryOverflow},
		{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1e308},
		{SysID: 1, Action: ActionSITLCRSFOverride, Param1: -1e308},
		{SysID: 1, Action: ActionSITLCrossCurrent, Param1: posBoundaryOverflow, Param2: 1.0},
		{SysID: 1, Action: ActionSITLCrossCurrent, Param1: 1.0, Param2: negBoundaryOverflow},
		{SysID: 1, Action: ActionHoldPosition, Param3: posBoundaryOverflow},
	} {
		var ce *ControlError
		if _, err := svc.SubmitCommand(context.Background(), badCmd); !errors.As(err, &ce) || ce.Code != "non_finite_value" {
			t.Fatalf("SubmitCommand(%+v) got err=%v, want non_finite_value", badCmd, err)
		}
	}
	if got := len(sender.snapshot()); got != 0 {
		t.Fatalf("out-of-range command requests sent %d frames, want 0", got)
	}

	// Exact ±math.MaxFloat32 finite boundaries remain accepted and finite on the wire.
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Vx:      float64(math.MaxFloat32),
		Vy:      -float64(math.MaxFloat32),
		Engage:  true,
		Session: "float32-bounds",
		Seq:     1,
	}); err != nil {
		t.Fatalf("SubmitTeleop at ±MaxFloat32 failed: %v", err)
	}
	for _, rec := range sender.snapshot() {
		if sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED); ok {
			if math.IsInf(float64(sp.Vx), 0) || math.IsInf(float64(sp.Vy), 0) {
				t.Fatalf("wire #84 payload contained Inf: %+v", sp)
			}
		}
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceFinalWriterSerializationWithStopPreemptionAndSequence(t *testing.T) {
	now := time.Date(2026, 11, 27, 4, 0, 0, 0, time.UTC)
	peer := netip.MustParseAddrPort("127.0.0.1:19801")

	newHarness := func() (*fakeTargetSource, *fakeFrameSender, *fakeCommandStore, *Service) {
		targets := newFakeTargetSource()
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()
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
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: now.UnixNano(),
		})
		return targets, sender, store, svc
	}

	// 1a) Nudge acquires final writer first while Stop blocks behind egressMu[1]:
	// wire order is strictly [nonzero, zero] and final lease is inactive.
	{
		_, sender, _, svc := newHarness()
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-race-1",
			Seq:     1,
			Vx:      0.5,
			Engage:  true,
		}); err != nil {
			t.Fatalf("engage: %v", err)
		}

		nudgeInSend := make(chan struct{})
		releaseNudge := make(chan struct{})
		sender.mu.Lock()
		sender.sent = nil
		sender.onSend = func(frame mavcodec.Frame, _ netip.AddrPort) error {
			if sp, ok := frame.Message.(mavcodec.SetPositionTargetLocalNED); ok && sp.Vx > 0 {
				close(nudgeInSend)
				<-releaseNudge
			}
			return nil
		}
		sender.mu.Unlock()

		nudgeDone := make(chan error, 1)
		go func() {
			_, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:   1,
				Session: "s-race-1",
				Seq:     2,
				Vx:      1.8,
			})
			nudgeDone <- err
		}()
		<-nudgeInSend

		stopDone := make(chan error, 1)
		go func() {
			_, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:   1,
				Session: "s-race-1",
				Stop:    true,
			})
			stopDone <- err
		}()
		// Give Stop goroutine a moment to queue on egressMu[1], then release Nudge.
		time.Sleep(15 * time.Millisecond)
		close(releaseNudge)

		if err := <-nudgeDone; err != nil {
			t.Fatalf("nudge err: %v", err)
		}
		if err := <-stopDone; err != nil {
			t.Fatalf("stop err: %v", err)
		}
		if svc.HasActiveLease(1) {
			t.Fatal("lease stayed active after serialized Stop")
		}
		var vxOrder []float32
		for _, rec := range sender.snapshot() {
			if sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED); ok {
				vxOrder = append(vxOrder, sp.Vx)
			}
		}
		if len(vxOrder) != 2 || vxOrder[0] != 1.8 || vxOrder[1] != 0 {
			t.Fatalf("serialized [Nudge, Stop] wire Vx order = %v, want [1.8 0]", vxOrder)
		}
	}

	// 1b) Stop acquires final writer first while Nudge blocks behind egressMu[1]:
	// wire emits only [zero] and Nudge fails with teleop_session_stopped.
	{
		_, sender, _, svc := newHarness()
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-race-2",
			Seq:     1,
			Vx:      0.5,
			Engage:  true,
		}); err != nil {
			t.Fatalf("engage: %v", err)
		}

		stopInSend := make(chan struct{})
		releaseStop := make(chan struct{})
		sender.mu.Lock()
		sender.sent = nil
		sender.onSend = func(frame mavcodec.Frame, _ netip.AddrPort) error {
			if sp, ok := frame.Message.(mavcodec.SetPositionTargetLocalNED); ok && sp.Vx == 0 {
				close(stopInSend)
				<-releaseStop
			}
			return nil
		}
		sender.mu.Unlock()

		stopDone := make(chan error, 1)
		go func() {
			_, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:   1,
				Session: "s-race-2",
				Stop:    true,
			})
			stopDone <- err
		}()
		<-stopInSend

		nudgeDone := make(chan error, 1)
		go func() {
			_, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:   1,
				Session: "s-race-2",
				Seq:     2,
				Vx:      2.2,
			})
			nudgeDone <- err
		}()
		time.Sleep(15 * time.Millisecond)
		close(releaseStop)

		if err := <-stopDone; err != nil {
			t.Fatalf("stop err: %v", err)
		}
		var ce *ControlError
		if err := <-nudgeDone; !errors.As(err, &ce) || ce.Code != "teleop_session_stopped" {
			t.Fatalf("expected teleop_session_stopped after Stop won egressMu, got %v", err)
		}
		var vxOrder []float32
		for _, rec := range sender.snapshot() {
			if sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED); ok {
				vxOrder = append(vxOrder, sp.Vx)
			}
		}
		if len(vxOrder) != 1 || vxOrder[0] != 0 {
			t.Fatalf("serialized [Stop, Nudge] wire Vx order = %v, want [0]", vxOrder)
		}
	}

	// 1c) Out-of-order sequence nudges (seq=3 winning egressMu ahead of seq=2, including Engage=true)
	// reject seq=2 before transmit so older setpoints never overwrite newer wire frames.
	{
		_, sender, _, svc := newHarness()
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-seq",
			Seq:     1,
			Vx:      0.4,
			Engage:  true,
		}); err != nil {
			t.Fatalf("engage: %v", err)
		}

		seq3InSend := make(chan struct{})
		releaseSeq3 := make(chan struct{})
		sender.mu.Lock()
		sender.sent = nil
		sender.onSend = func(frame mavcodec.Frame, _ netip.AddrPort) error {
			if sp, ok := frame.Message.(mavcodec.SetPositionTargetLocalNED); ok && sp.Vx == 3.0 {
				close(seq3InSend)
				<-releaseSeq3
			}
			return nil
		}
		sender.mu.Unlock()

		seq3Done := make(chan error, 1)
		go func() {
			_, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:   1,
				Session: "s-seq",
				Seq:     3,
				Vx:      3.0,
			})
			seq3Done <- err
		}()
		<-seq3InSend

		seq2Done := make(chan error, 1)
		seq2EngageDone := make(chan error, 1)
		go func() {
			_, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:   1,
				Session: "s-seq",
				Seq:     2,
				Vx:      2.0,
			})
			seq2Done <- err
		}()
		go func() {
			_, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:   1,
				Session: "s-seq",
				Seq:     2,
				Vx:      2.5,
				Engage:  true,
			})
			seq2EngageDone <- err
		}()
		time.Sleep(15 * time.Millisecond)
		close(releaseSeq3)

		if err := <-seq3Done; err != nil {
			t.Fatalf("seq3 err: %v", err)
		}
		var ce *ControlError
		if err := <-seq2Done; !errors.As(err, &ce) || ce.Code != "teleop_seq_superseded" {
			t.Fatalf("seq2 expected teleop_seq_superseded, got %v", err)
		}
		if err := <-seq2EngageDone; !errors.As(err, &ce) || ce.Code != "teleop_seq_superseded" {
			t.Fatalf("seq2+engage expected teleop_seq_superseded, got %v", err)
		}
		var vxOrder []float32
		for _, rec := range sender.snapshot() {
			if sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED); ok {
				vxOrder = append(vxOrder, sp.Vx)
			}
		}
		if len(vxOrder) != 1 || vxOrder[0] != 3.0 {
			t.Fatalf("serialized sequence wire Vx order = %v, want [3]", vxOrder)
		}
	}

	// 1d) #76 COMMAND_LONG paused in SendFrame while OnTransitionEvent(DirectCRSFOverride)
	// waits on egressMu[1]: command is marked dispatched first and then immediately preempted!
	{
		targets, sender, store, svc := newHarness()
		cmdInSend := make(chan struct{})
		releaseCmd := make(chan struct{})
		sender.onSend = func(frame mavcodec.Frame, _ netip.AddrPort) error {
			if _, ok := frame.Message.(mavcodec.CommandLong); ok {
				close(cmdInSend)
				<-releaseCmd
			}
			return nil
		}

		type cmdOut struct {
			res CommandResult
			err error
		}
		cmdDone := make(chan cmdOut, 1)
		go func() {
			res, err := svc.SubmitCommand(context.Background(), CommandRequest{
				SysID:  1,
				Action: ActionHoldPosition,
			})
			cmdDone <- cmdOut{res: res, err: err}
		}()
		<-cmdInSend

		preemptDone := make(chan struct{})
		go func() {
			cur, _ := targets.ActuationTarget(1)
			cur.Authority = uxsvocab.AuthorityDirectCRSFOverride
			targets.set(cur)
			svc.OnTransitionEvent(mavlink.TransitionEvent{
				Kind:      mavlink.EventKindHeartbeatEdge,
				MsgID:     mavcodec.MsgIDHeartbeat,
				SysID:     1,
				EntityID:  cur.EntityID,
				Authority: uxsvocab.AuthorityDirectCRSFOverride,
				LinkTier:  cur.LinkTier,
				Armed:     true,
				Timestamp: now.UnixMilli(),
			})
			close(preemptDone)
		}()
		time.Sleep(15 * time.Millisecond)
		close(releaseCmd)

		out := <-cmdDone
		<-preemptDone
		if out.err != nil {
			t.Fatalf("SubmitCommand failed unexpectedly after SendFrame succeeded: %v", out.err)
		}
		if ent, ok := store.get(out.res.CommandID); !ok || ent.PhaseState != PhasePreempted {
			t.Fatalf("command after concurrent preemption = %+v, want preempted", ent)
		}
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceAmbiguousAckAcrossReconciliationAndIncompleteRecovery(t *testing.T) {
	// Part A: Arm times out -> armed #0 HEARTBEAT reconciles Arm -> Disarm submitted ->
	// delayed Arm #77 ACCEPTED arrives while vehicle is still armed -> Disarm MUST stay commit_unknown
	// until a disarmed #0 HEARTBEAT arrives.
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)

	svc, err := NewService(Options{
		Org:               "boxbotix",
		Platform:          "bbops",
		Targets:           targets,
		Sender:            sender,
		Store:             store,
		Now:               func() time.Time { return now },
		CommandAckTimeout: 1500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	target := mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeDisarmed,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   false,
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19811"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets.set(target)

	armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
	if err != nil {
		t.Fatalf("SubmitCommand Arm: %v", err)
	}
	now = now.Add(1600 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("Arm after timeout = %q, want commit_unknown", ent.PhaseState)
	}

	// Armed #0 HEARTBEAT reconciles Arm without any #77 COMMAND_ACK having arrived.
	now = now.Add(100 * time.Millisecond)
	target.Armed = true
	target.CompanionMode = uxsvocab.ModeTeleopVelocity
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("Arm after armed HEARTBEAT = %q, want confirmed", ent.PhaseState)
	}

	// Submit Disarm while vehicle is armed; then deliver delayed Arm #77 ACCEPTED while still armed!
	now = now.Add(50 * time.Millisecond)
	disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
	if err != nil {
		t.Fatalf("SubmitCommand Disarm: %v", err)
	}
	now = now.Add(20 * time.Millisecond)
	svc.OnCommandAck(1, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}, now)
	if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("delayed Arm ACK falsely confirmed Disarm while armed: %+v", ent)
	}

	// Armed #0 HEARTBEAT keeps Disarm commit_unknown; disarmed #0 HEARTBEAT confirms Disarm.
	now = now.Add(100 * time.Millisecond)
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("armed HEARTBEAT confirmed Disarm prematurely: %+v", ent)
	}

	now = now.Add(100 * time.Millisecond)
	target.Armed = false
	target.CompanionMode = uxsvocab.ModeDisarmed
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("disarmed HEARTBEAT failed to confirm Disarm: %+v", ent)
	}

	// Part B: Restart with transient ListNonTerminal failure does NOT block Emergency Disarm,
	// yet treats #77 COMMAND_ACK as ambiguous until recovery completes and vehicle disarms.
	sharedStore := newFakeCommandStore()
	svc1, err := NewService(Options{
		Org:      "boxbotix",
		Platform: "bbops",
		Targets:  targets,
		Sender:   sender,
		Store:    sharedStore,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewService svc1: %v", err)
	}
	target.Armed = true
	target.CompanionMode = uxsvocab.ModeTeleopVelocity
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)

	preRestartArm, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
	if err != nil {
		t.Fatalf("svc1 SubmitCommand Arm: %v", err)
	}

	// Simulate restart where ListNonTerminal transiently fails during Start + Emergency Disarm.
	now = now.Add(200 * time.Millisecond)
	sharedStore.mu.Lock()
	sharedStore.listErr = errors.New("transient kv list error")
	sharedStore.mu.Unlock()

	svc2, err := NewService(Options{
		Org:      "boxbotix",
		Platform: "bbops",
		Targets:  targets,
		Sender:   sender,
		Store:    sharedStore,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewService svc2: %v", err)
	}
	postRestartDisarm, err := svc2.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
	if err != nil {
		t.Fatalf("Emergency Disarm blocked by incomplete recovery: %v", err)
	}

	// Delayed pre-restart Arm #77 ACCEPTED arrives before recovery succeeds: Disarm MUST stay commit_unknown!
	now = now.Add(25 * time.Millisecond)
	svc2.OnCommandAck(1, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}, now)
	if ent, _ := sharedStore.get(postRestartDisarm.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("pre-recovery ACK falsely confirmed post-restart Disarm: %+v", ent)
	}

	// Recovery now succeeds: pre-restart Arm moves to commit_unknown without overwriting current-boot Disarm,
	// and a fresh disarmed #0 HEARTBEAT confirms postRestartDisarm.
	sharedStore.mu.Lock()
	sharedStore.listErr = nil
	sharedStore.mu.Unlock()
	now = now.Add(100 * time.Millisecond)
	svc2.Advance(context.Background(), now)
	if ent, _ := sharedStore.get(preRestartArm.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("recovered preRestartArm = %+v, want commit_unknown", ent)
	}

	now = now.Add(100 * time.Millisecond)
	target.Armed = false
	target.CompanionMode = uxsvocab.ModeDisarmed
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc2.Advance(context.Background(), now)
	if ent, _ := sharedStore.get(postRestartDisarm.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("disarmed HEARTBEAT failed to confirm postRestartDisarm: %+v", ent)
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceReservedCommandNotAckEligibleBeforeDispatch(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 6, 0, 0, 0, time.UTC)

	svc, err := NewService(Options{
		Org:               "boxbotix",
		Platform:          "bbops",
		Targets:           targets,
		Sender:            sender,
		Store:             store,
		Now:               func() time.Time { return now },
		CommandAckTimeout: 1500 * time.Millisecond,
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
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19821"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	})

	armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
	if err != nil {
		t.Fatalf("SubmitCommand Arm: %v", err)
	}

	// While Disarm is inside store.Create (reserved, not yet UDP-dispatched):
	// 1) Arm's #77 ACCEPTED arrives -> must confirm Arm cleanly without touching reserved Disarm.
	// 2) Clock advances 1400ms during slow Create -> Disarm's 1500ms timeout must start at UDP dispatch!
	store.afterCreate = func(cmd CommandEntity) {
		if cmd.Action != ActionDisarm {
			return
		}
		now = now.Add(1400 * time.Millisecond)
		svc.OnCommandAck(1, mavcodec.CommandAck{
			Command: mavcodec.MAVCmdComponentArmDisarm,
			Result:  mavcodec.MAVResultAccepted,
		}, now)
	}

	disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
	store.afterCreate = nil
	if err != nil {
		t.Fatalf("SubmitCommand Disarm: %v", err)
	}

	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("Arm should be confirmed by its pre-Disarm-dispatch ACK, got %+v", ent)
	}
	if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhasePending {
		t.Fatalf("reserved Disarm mutated before UDP dispatch: %+v, want pending", ent)
	}

	// 200ms after actual UDP dispatch (1600ms after pre-Create): Disarm must STILL be pending!
	now = now.Add(200 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhasePending {
		t.Fatalf("Disarm timed out 200ms after UDP dispatch (used pre-Create timestamp): %+v", ent)
	}

	// Unambiguous #77 ACCEPTED for Disarm now confirms Disarm cleanly because Arm was already ACKed before Disarm dispatched!
	now = now.Add(50 * time.Millisecond)
	svc.OnCommandAck(1, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}, now)
	if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("Disarm after unambiguous post-dispatch ACK = %+v, want confirmed", ent)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceFailedTeleopRenewalPreservesDeadmanDeadline(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)

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

	targets.set(mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19831"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	})

	// t0: engage with Vx = 1.4 (original deadline = t0 + 200ms).
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "s-deadman",
		Seq:     1,
		Vx:      1.4,
		Engage:  true,
	}); err != nil {
		t.Fatalf("initial SubmitTeleop: %v", err)
	}

	// t0 + 60ms: transient UDP send error fails renewal; original t0 + 200ms lease MUST remain active!
	now = now.Add(60 * time.Millisecond)
	sender.mu.Lock()
	sender.sendErr = errors.New("temporary udp buffer full")
	sender.mu.Unlock()

	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "s-deadman",
		Seq:     2,
		Vx:      1.4,
	}); err == nil {
		t.Fatal("expected failed renewal to return error")
	}
	if !svc.HasActiveLease(1) {
		t.Fatal("failed renewal deleted active teleop lease instead of preserving original deadline")
	}

	sender.mu.Lock()
	sender.sendErr = nil
	sender.mu.Unlock()

	// t0 + 190ms: still within original 200ms TTL.
	now = now.Add(130 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if !svc.HasActiveLease(1) {
		t.Fatal("lease expired before original 200ms deadline")
	}

	// t0 + 205ms (< 250ms from last successful setpoint at t0!): expires original deadline and sends zero-velocity #84!
	before := countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot())
	now = now.Add(15 * time.Millisecond)
	svc.Advance(context.Background(), now)
	if svc.HasActiveLease(1) {
		t.Fatal("lease remained active past original 200ms deadline after failed renewal")
	}
	frames := sender.snapshot()
	if got := countMessages[mavcodec.SetPositionTargetLocalNED](frames); got != before+1 {
		t.Fatalf("deadman expiry after failed renewal sent %d #84 frames, want 1", got-before)
	}
	lastSP, ok := frames[len(frames)-1].frame.Message.(mavcodec.SetPositionTargetLocalNED)
	if !ok || lastSP.Vx != 0 || lastSP.Vy != 0 || lastSP.Vz != 0 || lastSP.YawRate != 0 {
		t.Fatalf("last #84 frame = %+v, want zero velocity", frames[len(frames)-1].frame.Message)
	}
}

// spec: uxs-control / Pin-on-admission MAVLink v2 UDP command egress and 1 Hz GCS liveness
// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceSupersededCommandStopsIdleGCSHeartbeats(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 8, 0, 0, 0, time.UTC)

	svc, err := NewService(Options{
		Org:                  "boxbotix",
		Platform:             "bbops",
		Targets:              targets,
		Sender:               sender,
		Store:                store,
		Now:                  func() time.Time { return now },
		GCSHeartbeatInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	target := mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19841"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets.set(target)

	armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
	if err != nil {
		t.Fatalf("SubmitCommand Arm: %v", err)
	}
	now = now.Add(50 * time.Millisecond)
	disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
	if err != nil {
		t.Fatalf("SubmitCommand Disarm: %v", err)
	}
	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("superseded Arm phase = %q, want commit_unknown", ent.PhaseState)
	}

	// Ambiguous #77 transitions Disarm to commit_unknown, then disarmed #0 HEARTBEAT confirms Disarm.
	now = now.Add(20 * time.Millisecond)
	svc.OnCommandAck(1, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}, now)
	now = now.Add(50 * time.Millisecond)
	target.Armed = false
	target.CompanionMode = uxsvocab.ModeDisarmed
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("Disarm phase = %q, want confirmed", ent.PhaseState)
	}

	// With no active teleop lease and no pending command awaiting ACK, 5s of idle ticks emit ZERO GCS heartbeats!
	hbBeforeIdle := countMessages[mavcodec.Heartbeat](sender.snapshot())
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		svc.Advance(context.Background(), now)
	}
	if got := countMessages[mavcodec.Heartbeat](sender.snapshot()) - hbBeforeIdle; got != 0 {
		t.Fatalf("superseded history leaked %d idle 1 Hz GCS HEARTBEAT frames, want 0", got)
	}
}
