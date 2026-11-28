package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxbotix/bbops/internal/mavcodec"
	"github.com/boxbotix/bbops/internal/mavlink"
	"github.com/boxbotix/bbops/internal/uxsvocab"
	"github.com/c360studio/semengine/component"
	"github.com/c360studio/semengine/natsclient"
	"github.com/c360studio/semengine/payloadregistry"
	"github.com/c360studio/semengine/pkg/lifecycle"
	graphingest "github.com/c360studio/semengine/processor/graph-ingest"
	"github.com/c360studio/semengine/types"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
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

func (f *fakeFrameSender) countByMsgID(msgID uint32) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, rec := range f.sent {
		if rec.frame.Message != nil && rec.frame.Message.MessageID() == msgID {
			n++
		}
	}
	return n
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
	if ackResult >= 0 {
		cur.AckAccounted = 1
		if ackResult < 28 {
			cur.AckPoolMask |= (1 << uint(ackResult)) | ackPoolFormatV2Bit
		} else {
			cur.AckPoolMask |= ackPoolFormatV2Bit
		}
	}
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

func (s *fakeCommandStore) RecordConsumedAck(_ context.Context, entityID string, ackAccounted, ackPoolMask int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.entities[entityID]
	if !ok {
		return errors.New("unknown entity ID")
	}
	cur.AckAccounted = ackAccounted
	cur.AckPoolMask = ackPoolMask
	s.entities[entityID] = cur
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

func (s *fakeCommandStore) ListCommands(_ context.Context) ([]CommandEntity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]CommandEntity, 0, len(s.entities))
	for _, e := range s.entities {
		out = append(out, e)
	}
	return out, nil
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
	target.HeartbeatObservedUnixNs = now.UnixNano()
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
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:      3,
		Vx:         0.8,
		Engage:     true,
		ObservedMs: now.UnixMilli(),
	}); err != nil {
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
		sawDuplicateAckIgnored    int
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
			AuthorityEpoch:          1,
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
			refSeenMask       uint64
			refPreResetMask   uint64
			refSurplusTainted bool
			refCmdDeadline    time.Time
			refCmdUnknownNs   int64
			ackSeq            uint8
			hasPrevAck        bool
			prevAckSeq        uint8
			prevAck           mavcodec.CommandAck
		)

		for step := 0; step < 65; step++ {
			switch rng.Intn(6) {
			case 0: // SubmitTeleop
				if refLeaseActive && !now.Before(refLeaseExpiresAt) {
					refLeaseActive = false
				}
				engage := rng.Intn(2) == 0
				stop := !engage && rng.Intn(5) == 0
				_, callErr := svc.SubmitTeleop(context.Background(), TeleopRequest{
					SysID:          1,
					Vx:             0.6,
					Engage:         engage,
					Stop:           stop,
					AuthorityEpoch: curTarget.AuthorityEpoch,
					ObservedMs:     curTarget.HeartbeatObservedUnixNs / int64(time.Millisecond),
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
					refCmdAmbiguous = refUnackedCount > 0 || refSurplusTainted
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

			case 2: // Inbound #77 COMMAND_ACK (including duplicate packet replays)
				if hasPrevAck && rng.Intn(3) == 0 {
					svc.OnCommandAckFrame(1, prevAckSeq, prevAck, now)
					sawDuplicateAckIgnored++
					break
				}
				if refCmdID != "" && (refCmdPhase == PhasePending || refCmdPhase == PhaseCommitUnknown) {
					accept := rng.Intn(2) == 0
					ackRes := mavcodec.MAVResultDenied
					if accept {
						ackRes = mavcodec.MAVResultAccepted
					}
					ackSeq++
					ack := mavcodec.CommandAck{
						Command: mavcodec.MAVCmdNavLoiterUnlim,
						Result:  ackRes,
					}
					svc.OnCommandAckFrame(1, ackSeq, ack, now)
					hasPrevAck = true
					prevAckSeq = ackSeq
					prevAck = ack
					seenPreReset := (refPreResetMask & (uint64(1) << ackRes)) != 0
					refSeenMask |= uint64(1) << ackRes
					wasAmbiguous := refCmdAmbiguous || refUnackedCount > 1 || refSurplusTainted || seenPreReset
					if refUnackedCount > 0 {
						refUnackedCount--
					} else {
						refSurplusTainted = true
					}
					if wasAmbiguous {
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
				} else if refUnackedCount > 0 {
					ackRes := mavcodec.MAVResultAccepted
					if rng.Intn(2) == 0 {
						ackRes = mavcodec.MAVResultDenied
					}
					ackSeq++
					ack := mavcodec.CommandAck{
						Command: mavcodec.MAVCmdNavLoiterUnlim,
						Result:  ackRes,
					}
					svc.OnCommandAckFrame(1, ackSeq, ack, now)
					hasPrevAck = true
					prevAckSeq = ackSeq
					prevAck = ack
					refSeenMask |= uint64(1) << ackRes
					refUnackedCount--
				}

			case 3: // Inbound vehicle state / link transition
				now = now.Add(5 * time.Millisecond)
				if refLeaseActive && !now.Before(refLeaseExpiresAt) {
					refLeaseActive = false
				}
				prevLinkLost := curTarget.LinkLost
				prevTier := curTarget.LinkTier
				prevAuth := curTarget.Authority
				prevArmed := curTarget.Armed
				curTarget.LinkLost = rng.Intn(6) == 0
				curTarget.LinkTier = tiers[rng.Intn(len(tiers))]
				curTarget.Authority = authorities[rng.Intn(len(authorities))]
				curTarget.Armed = rng.Intn(5) != 0
				if prevLinkLost != curTarget.LinkLost || prevTier != curTarget.LinkTier ||
					prevAuth != curTarget.Authority || prevArmed != curTarget.Armed {
					curTarget.AuthorityEpoch++
				}
				if rng.Intn(2) == 0 {
					curTarget.CompanionMode = uxsvocab.ModeHoldPosition
				} else {
					curTarget.CompanionMode = uxsvocab.ModeTeleopVelocity
				}
				if !curTarget.LinkLost {
					curTarget.HeartbeatObservedUnixNs = now.UTC().UnixNano()
				} else {
					hasPrevAck = false
				}
				targets.set(curTarget)

				kind := mavlink.EventKindHeartbeatEdge
				if curTarget.LinkLost {
					kind = mavlink.EventKindLinkLost
				} else if prevLinkLost {
					kind = mavlink.EventKindLinkRestored
				}
				if kind == mavlink.EventKindLinkLost || kind == mavlink.EventKindLinkRestored {
					refPreResetMask |= refSeenMask
					hasPrevAck = false
				}
				svc.OnTransitionEvent(mavlink.TransitionEvent{
					Kind:           kind,
					MsgID:          mavcodec.MsgIDHeartbeat,
					SysID:          1,
					EntityID:       curTarget.EntityID,
					Authority:      curTarget.Authority,
					AuthorityEpoch: curTarget.AuthorityEpoch,
					CompanionMode:  curTarget.CompanionMode,
					LinkTier:       curTarget.LinkTier,
					Armed:          curTarget.Armed,
					Timestamp:      now.UnixMilli(),
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
		sawCommitUnknown == 0 || sawHeartbeatReconcile == 0 || sawPreemptionLatch == 0 ||
		sawExplicitReengage == 0 || sawDuplicateAckIgnored == 0 {
		t.Fatalf("insufficient property boundary reach: teleop=%d expiryZero=%d meshRefusal=%d unknown=%d reconcile=%d latch=%d reengage=%d dupAck=%d",
			sawTeleopAccepted, sawLeaseExpiryZero, sawMeshSupervisoryRefusal,
			sawCommitUnknown, sawHeartbeatReconcile, sawPreemptionLatch, sawExplicitReengage, sawDuplicateAckIgnored)
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
	recoveredTarget := baseTarget
	recoveredTarget.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(recoveredTarget)
	sender.onSend = func(frame mavcodec.Frame, _ netip.AddrPort) error {
		if _, isHB := frame.Message.(mavcodec.Heartbeat); isHB {
			tg := recoveredTarget
			tg.PeerAddr = peerB
			targets.set(tg)
		}
		return nil
	}
	_, err = svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:      1,
		Vx:         2.5,
		Engage:     true,
		ObservedMs: now.UnixMilli(),
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
		svc.OnCommandAckFrame(1, 10, mavcodec.CommandAck{
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

	// Unambiguous #77 ACCEPTED frame (distinct seq=11) for Disarm now confirms Disarm cleanly because Arm was already ACKed before Disarm dispatched!
	now = now.Add(50 * time.Millisecond)
	svc.OnCommandAckFrame(1, 11, mavcodec.CommandAck{
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

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceRecoveryPreservesTerminalHeartbeatAckAmbiguityAndSupersession(t *testing.T) {
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	peer := netip.MustParseAddrPort("127.0.0.1:19851")

	// Part A: Arm times out -> armed #0 HEARTBEAT reconciles Arm (terminal PhaseConfirmed, AckResult=-1) ->
	// service restarts on same store -> Disarm submitted -> delayed pre-restart Arm #77 ACCEPTED arrives
	// while vehicle is still armed -> Disarm MUST stay commit_unknown until a disarmed #0 HEARTBEAT arrives!
	{
		targets := newFakeTargetSource()
		sender := &fakeFrameSender{}
		sharedStore := newFakeCommandStore()

		target := mavlink.ActuationTarget{
			SysID:                   1,
			EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
			Domain:                  uxsvocab.DomainQuadcopter,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			CompanionMode:           uxsvocab.ModeDisarmed,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			Armed:                   false,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: now.UnixNano(),
		}
		targets.set(target)

		svc1, err := NewService(Options{
			Org:               "boxbotix",
			Platform:          "bbops",
			Targets:           targets,
			Sender:            sender,
			Store:             sharedStore,
			Now:               func() time.Time { return now },
			CommandAckTimeout: 1500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewService svc1: %v", err)
		}

		armRes, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
		if err != nil {
			t.Fatalf("svc1 SubmitCommand Arm: %v", err)
		}
		now = now.Add(1600 * time.Millisecond)
		svc1.Advance(context.Background(), now)

		now = now.Add(100 * time.Millisecond)
		target.Armed = true
		target.CompanionMode = uxsvocab.ModeTeleopVelocity
		target.HeartbeatObservedUnixNs = now.UnixNano()
		targets.set(target)
		svc1.Advance(context.Background(), now)
		if ent, _ := sharedStore.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed || ent.AckResult != UnacknowledgedResult {
			t.Fatalf("pre-restart Arm = %+v, want confirmed with UnacknowledgedResult (-1)", ent)
		}

		// Restart into svc2 on the same durable store!
		now = now.Add(100 * time.Millisecond)
		svc2, err := NewService(Options{
			Org:               "boxbotix",
			Platform:          "bbops",
			Targets:           targets,
			Sender:            sender,
			Store:             sharedStore,
			Now:               func() time.Time { return now },
			CommandAckTimeout: 1500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewService svc2: %v", err)
		}

		disarmRes, err := svc2.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("svc2 SubmitCommand Disarm: %v", err)
		}

		// Delayed #77 ACCEPTED from pre-restart Arm arrives while vehicle is still Armed=true:
		// Disarm MUST become commit_unknown (NEVER confirmed)!
		now = now.Add(25 * time.Millisecond)
		svc2.OnCommandAck(1, mavcodec.CommandAck{
			Command: mavcodec.MAVCmdComponentArmDisarm,
			Result:  mavcodec.MAVResultAccepted,
		}, now)
		if ent, _ := sharedStore.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("post-restart Disarm falsely confirmed by pre-restart Arm ACK: %+v", ent)
		}

		// Fresh disarmed #0 HEARTBEAT reconciles Disarm cleanly.
		now = now.Add(100 * time.Millisecond)
		target.Armed = false
		target.CompanionMode = uxsvocab.ModeDisarmed
		target.HeartbeatObservedUnixNs = now.UnixNano()
		targets.set(target)
		svc2.Advance(context.Background(), now)
		if ent, _ := sharedStore.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("disarmed HEARTBEAT failed to confirm Disarm: %+v", ent)
		}
	}

	// Part B: Superseded Arm (commit_unknown) followed by confirmed Disarm (even within the same millisecond
	// across c2 vs c10 sequence numbers) MUST NEVER be revived or confirmed after restart by a later armed HEARTBEAT,
	// and pre-send aborted commands (UnsentAckResult) do not taint post-restart ACKs!
	{
		targets := newFakeTargetSource()
		sender := &fakeFrameSender{}
		sharedStore := newFakeCommandStore()
		fixedNow := time.Date(2026, 11, 27, 9, 15, 0, 0, time.UTC)
		curNow := fixedNow

		target := mavlink.ActuationTarget{
			SysID:                   1,
			EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
			Domain:                  uxsvocab.DomainQuadcopter,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			CompanionMode:           uxsvocab.ModeTeleopVelocity,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			Armed:                   true,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: curNow.UnixNano(),
		}
		targets.set(target)

		svc1, err := NewService(Options{
			Org:               "boxbotix",
			Platform:          "bbops",
			Targets:           targets,
			Sender:            sender,
			Store:             sharedStore,
			Now:               func() time.Time { return curNow },
			CommandAckTimeout: 1500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewService svc1: %v", err)
		}

		// Advance cmdCounter so Arm gets c2 and Disarm gets c10 at identical IssuedMs/UpdatedMs
		// (where lexicographic ID comparison "c2" > "c10" would wrongly pick Arm as newer!).
		svc1.cmdCounter.Store(1)
		armRes, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
		if err != nil {
			t.Fatalf("SubmitCommand Arm (c2): %v", err)
		}
		svc1.cmdCounter.Store(9)
		disarmRes, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm (c10): %v", err)
		}
		if ent, _ := sharedStore.get(armRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("superseded Arm = %+v, want commit_unknown", ent)
		}

		// Time out Disarm to commit_unknown, then confirm via post-timeout disarmed HEARTBEAT.
		curNow = curNow.Add(1600 * time.Millisecond)
		svc1.Advance(context.Background(), curNow)
		curNow = curNow.Add(100 * time.Millisecond)
		target.Armed = false
		target.CompanionMode = uxsvocab.ModeDisarmed
		target.HeartbeatObservedUnixNs = curNow.UnixNano()
		targets.set(target)
		svc1.Advance(context.Background(), curNow)
		// Equalize UpdatedMs on sharedStore to force same-boot sequence comparison (c10 > c2).
		sharedStore.mu.Lock()
		disEnt := sharedStore.entities[disarmRes.CommandID]
		disEnt.UpdatedMs = fixedNow.UnixMilli()
		sharedStore.entities[disarmRes.CommandID] = disEnt
		sharedStore.mu.Unlock()
		if disEnt.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm before restart = %+v, want confirmed", disEnt)
		}

		// Also record an unsent HoldPosition failure (udp_send_failed -> UnsentAckResult) before restart.
		sender.mu.Lock()
		sender.sendErr = errors.New("socket down")
		sender.mu.Unlock()
		_, _ = svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		sender.mu.Lock()
		sender.sendErr = nil
		sender.mu.Unlock()

		// Restart into svc2 and deliver an armed #0 HEARTBEAT: superseded Arm MUST remain commit_unknown!
		curNow = curNow.Add(2 * time.Second)
		svc2, err := NewService(Options{
			Org:               "boxbotix",
			Platform:          "bbops",
			Targets:           targets,
			Sender:            sender,
			Store:             sharedStore,
			Now:               func() time.Time { return curNow },
			CommandAckTimeout: 1500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewService svc2: %v", err)
		}
		curNow = curNow.Add(100 * time.Millisecond)
		target.Armed = true
		target.CompanionMode = uxsvocab.ModeTeleopVelocity
		target.HeartbeatObservedUnixNs = curNow.UnixNano()
		targets.set(target)
		svc2.Advance(context.Background(), curNow)
		if ent, _ := sharedStore.get(armRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("superseded pre-restart Arm was revived and confirmed after restart: %+v", ent)
		}

		// HoldPosition had only an unsent pre-restart failure (UnsentAckResult), so a post-restart HoldPosition
		// receiving explicit DENIED transitions straight to PhaseRejected!
		holdRes, err := svc2.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		if err != nil {
			t.Fatalf("svc2 SubmitCommand HoldPosition: %v", err)
		}
		svc2.OnCommandAck(1, mavcodec.CommandAck{
			Command: mavcodec.MAVCmdNavLoiterUnlim,
			Result:  mavcodec.MAVResultDenied,
		}, curNow.Add(20*time.Millisecond))
		if ent, _ := sharedStore.get(holdRes.CommandID); ent.PhaseState != PhaseRejected {
			t.Fatalf("unsent pre-restart failure tainted post-restart HoldPosition ACK: %+v, want rejected", ent)
		}
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceSendFrameOrderingWithFastNegativeAckAndConcurrentDisarm(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	peer := netip.MustParseAddrPort("127.0.0.1:19861")

	// Part A: Fast #77 DENIED arriving inside SendFrame before SendFrame returns MUST immediately
	// reject the command rather than dropping the ACK and timing out to commit_unknown!
	{
		targets := newFakeTargetSource()
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()

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
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: now.UnixNano(),
		})

		sender.onSend = func(frame mavcodec.Frame, _ netip.AddrPort) error {
			if cl, ok := frame.Message.(mavcodec.CommandLong); ok {
				svc.OnCommandAck(1, mavcodec.CommandAck{
					Command: cl.Command,
					Result:  mavcodec.MAVResultDenied,
				}, now.Add(2*time.Millisecond))
			}
			return nil
		}

		res, err := svc.SubmitCommand(context.Background(), CommandRequest{
			SysID:  1,
			Action: ActionHoldPosition,
		})
		if err != nil {
			t.Fatalf("SubmitCommand HoldPosition: %v", err)
		}
		if res.Phase != PhaseRejected {
			t.Fatalf("SubmitCommand fast DENIED result phase = %q, want %q", res.Phase, PhaseRejected)
		}
		ent, ok := store.get(res.CommandID)
		if !ok || ent.PhaseState != PhaseRejected || ent.AckResult != int(mavcodec.MAVResultDenied) {
			t.Fatalf("stored command after fast DENIED = %+v, want rejected/%d", ent, mavcodec.MAVResultDenied)
		}
	}

	// Part B: Concurrent Disarm reserving while Arm is inside SendFrame MUST NOT delete Arm as unsent
	// or strand Arm in PhasePending — Arm transitions durably to PhaseCommitUnknown when Disarm sends!
	{
		targets := newFakeTargetSource()
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()

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
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: now.UnixNano(),
		})

		armInSend := make(chan struct{})
		releaseArmSend := make(chan struct{})
		sender.onSend = func(frame mavcodec.Frame, _ netip.AddrPort) error {
			if cl, ok := frame.Message.(mavcodec.CommandLong); ok && cl.Command == mavcodec.MAVCmdComponentArmDisarm && cl.Param1 == 1 {
				close(armInSend)
				<-releaseArmSend
			}
			return nil
		}

		type cmdOut struct {
			res CommandResult
			err error
		}
		armDone := make(chan cmdOut, 1)
		go func() {
			r, e := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
			armDone <- cmdOut{res: r, err: e}
		}()
		<-armInSend

		disarmCreated := make(chan struct{})
		store.afterCreate = func(cmd CommandEntity) {
			if cmd.Action == ActionDisarm {
				close(disarmCreated)
			}
		}
		disarmDone := make(chan cmdOut, 1)
		go func() {
			r, e := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			disarmDone <- cmdOut{res: r, err: e}
		}()
		<-disarmCreated
		// Give Disarm goroutine a moment to block on egressMu[1] behind Arm.
		time.Sleep(15 * time.Millisecond)
		close(releaseArmSend)

		armOut := <-armDone
		disarmOut := <-disarmDone
		store.afterCreate = nil
		if armOut.err != nil {
			t.Fatalf("Arm err = %v, want nil", armOut.err)
		}
		if disarmOut.err != nil {
			t.Fatalf("Disarm err = %v, want nil", disarmOut.err)
		}
		if ent, _ := store.get(armOut.res.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Arm superseded during SendFrame = %+v, want commit_unknown (not stranded pending)", ent)
		}
		if ent, _ := store.get(disarmOut.res.CommandID); ent.PhaseState != PhasePending {
			t.Fatalf("Disarm after superseding Arm = %+v, want pending", ent)
		}
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceIdleLateAckClearsUniqueOutstandingCountBeforeNextRejection(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC)

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
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19871"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets.set(target)

	// 1. Arm times out -> armed #0 HEARTBEAT reconciles Arm to PhaseConfirmed.
	armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
	if err != nil {
		t.Fatalf("SubmitCommand Arm: %v", err)
	}
	now = now.Add(1600 * time.Millisecond)
	svc.Advance(context.Background(), now)
	now = now.Add(100 * time.Millisecond)
	target.Armed = true
	target.CompanionMode = uxsvocab.ModeTeleopVelocity
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc.Advance(context.Background(), now)
	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("Arm after HEARTBEAT = %+v, want confirmed", ent)
	}

	// 2. Arm's single late #77 ACCEPTED arrives while idle (no inFlight commands).
	now = now.Add(50 * time.Millisecond)
	svc.OnCommandAck(1, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}, now)

	// 3. Subsequent Disarm receives an explicit #77 DENIED -> MUST become PhaseRejected (not commit_unknown)!
	now = now.Add(50 * time.Millisecond)
	disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
	if err != nil {
		t.Fatalf("SubmitCommand Disarm: %v", err)
	}
	now = now.Add(20 * time.Millisecond)
	svc.OnCommandAck(1, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultDenied,
	}, now)
	if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseRejected || ent.AckResult != int(mavcodec.MAVResultDenied) {
		t.Fatalf("Disarm explicit DENIED after idle late ACK = %+v, want rejected/%d", ent, mavcodec.MAVResultDenied)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceRevokedTeleopSessionCannotReengageWithoutFreshSession(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 11, 30, 0, 0, time.UTC)

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

	target := mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19881"),
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets.set(target)

	// Engage session "s1-e1" on click #1.
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "s1-e1",
		Seq:     1,
		Vx:      0,
		Engage:  true,
	}); err != nil {
		t.Fatalf("engage s1-e1: %v", err)
	}

	// Brief P1 direct-crsf-override followed by P2 companion-teleop recovery.
	now = now.Add(20 * time.Millisecond)
	target.Authority = uxsvocab.AuthorityDirectCRSFOverride
	targets.set(target)
	svc.OnTransitionEvent(mavlink.TransitionEvent{
		Kind:      mavlink.EventKindHeartbeatEdge,
		MsgID:     mavcodec.MsgIDHeartbeat,
		SysID:     1,
		EntityID:  target.EntityID,
		Authority: uxsvocab.AuthorityDirectCRSFOverride,
		LinkTier:  target.LinkTier,
		Armed:     true,
		Timestamp: now.UnixMilli(),
	})

	now = now.Add(20 * time.Millisecond)
	target.Authority = uxsvocab.AuthorityCompanionTeleop
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets.set(target)
	svc.OnTransitionEvent(mavlink.TransitionEvent{
		Kind:      mavlink.EventKindHeartbeatEdge,
		MsgID:     mavcodec.MsgIDHeartbeat,
		SysID:     1,
		EntityID:  target.EntityID,
		Authority: uxsvocab.AuthorityCompanionTeleop,
		LinkTier:  target.LinkTier,
		Armed:     true,
		Timestamp: now.UnixMilli(),
	})

	// Even a delayed Engage=true carrying the revoked session "s1-e1" MUST fail with preemption_latched!
	var ce *ControlError
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "s1-e1",
		Seq:     2,
		Vx:      0.8,
		Engage:  true,
	}); !errors.As(err, &ce) || ce.Code != "preemption_latched" {
		t.Fatalf("delayed Engage=true on revoked session got err=%v, want preemption_latched", err)
	}
	if svc.HasActiveLease(1) || !svc.IsPreempted(1) {
		t.Fatalf("after rejected stale-session engage: HasActiveLease=%v IsPreempted=%v, want false/true", svc.HasActiveLease(1), svc.IsPreempted(1))
	}

	// Fresh operator click minting session "s1-e2" clears preemption latch and engages cleanly!
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:      1,
		Session:    "s1-e2",
		Seq:        1,
		Vx:         0.8,
		Engage:     true,
		ObservedMs: now.UnixMilli(),
	}); err != nil {
		t.Fatalf("fresh session s1-e2 Engage=true failed: %v", err)
	}
	if !svc.HasActiveLease(1) || svc.IsPreempted(1) {
		t.Fatal("fresh session Engage=true did not activate lease and clear preemption latch")
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceBlockedVehicleWriterDoesNotStarveOtherVehicleDeadman(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()

	svc, err := NewService(Options{
		Org:            "boxbotix",
		Platform:       "bbops",
		Targets:        targets,
		Sender:         sender,
		Store:          store,
		TeleopLeaseTTL: 80 * time.Millisecond,
		TickInterval:   10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	peerA := netip.MustParseAddrPort("127.0.0.1:19891")
	peerB := netip.MustParseAddrPort("127.0.0.1:19892")
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("svc.Start: %v", err)
	}

	veh1BlockedInSend := make(chan struct{})
	releaseVeh1Send := make(chan struct{})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(releaseVeh1Send) })
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = svc.Stop(stopCtx)
	}()

	// Engage vehicle 1, then block vehicle 1's renewal writer inside SendFrame while holding egressMu[1].
	if _, err := svc.SubmitTeleop(ctx, TeleopRequest{
		SysID:   1,
		Session: "veh1-s1",
		Seq:     1,
		Vx:      1.2,
		Engage:  true,
	}); err != nil {
		t.Fatalf("engage vehicle 1: %v", err)
	}

	var blockVeh1Once sync.Once
	sender.mu.Lock()
	sender.onSend = func(frame mavcodec.Frame, peer netip.AddrPort) error {
		if sp, ok := frame.Message.(mavcodec.SetPositionTargetLocalNED); ok && peer == peerA && sp.Vx > 0 {
			blockVeh1Once.Do(func() {
				close(veh1BlockedInSend)
				<-releaseVeh1Send
			})
		}
		return nil
	}
	sender.mu.Unlock()

	veh1RenewDone := make(chan error, 1)
	go func() {
		_, err := svc.SubmitTeleop(ctx, TeleopRequest{
			SysID:   1,
			Session: "veh1-s1",
			Seq:     2,
			Vx:      1.5,
		})
		veh1RenewDone <- err
	}()
	select {
	case <-veh1BlockedInSend:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vehicle 1 renewal SendFrame to block")
	}

	// Wait past vehicle 1's 80ms TTL so vehicle 1 is already expired while egressMu[1] stays held!
	time.Sleep(100 * time.Millisecond)

	// Engage vehicle 2 once (80ms TTL) and assert its zero-velocity deadman frame fires within < 250ms
	// while vehicle 1 remains blocked!
	nudgeStart := time.Now()
	if _, err := svc.SubmitTeleop(ctx, TeleopRequest{
		SysID:   2,
		Session: "veh2-s1",
		Seq:     1,
		Vx:      2.0,
		Engage:  true,
	}); err != nil {
		t.Fatalf("engage vehicle 2 while vehicle 1 writer blocked: %v", err)
	}

	deadline := nudgeStart.Add(245 * time.Millisecond)
	var zeroArrivedAt time.Time
	for time.Now().Before(deadline) {
		var veh2Zero int
		for _, rec := range sender.snapshot() {
			sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED)
			if ok && sp.TargetSystem == 2 && sp.Vx == 0 && sp.Vy == 0 && sp.Vz == 0 && sp.YawRate == 0 {
				veh2Zero++
			}
		}
		if veh2Zero > 0 && !svc.HasActiveLease(2) {
			zeroArrivedAt = time.Now()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if zeroArrivedAt.IsZero() {
		t.Fatalf("vehicle 2 deadman starved past 250ms by blocked vehicle 1 writer (HasActiveLease(2)=%v)", svc.HasActiveLease(2))
	}
	if elapsed := zeroArrivedAt.Sub(nudgeStart); elapsed >= 250*time.Millisecond {
		t.Fatalf("vehicle 2 deadman elapsed = %v, want < 250ms", elapsed)
	}

	// Confirm vehicle 1 writer is STILL blocked right now, then release it cleanly.
	select {
	case err := <-veh1RenewDone:
		t.Fatalf("vehicle 1 renewal completed prematurely: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(releaseVeh1Send) })
	if err := <-veh1RenewDone; err != nil {
		t.Fatalf("vehicle 1 renewal after release failed: %v", err)
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceDuplicateCommandAckBeforeAndAfterDisarmNeverConfirmsArmedVehicle(t *testing.T) {
	// Subcase A: Sequenced MAVLink #77 frames (OnCommandAckFrame) — duplicate Arm ACK delivered
	// BOTH before Disarm (while idle) AND after Disarm (while Disarm is pending) without resending either command.
	t.Run("sequenced_frames_before_and_after_disarm", func(t *testing.T) {
		targets := newFakeTargetSource()
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 16, 10, 0, 0, time.UTC)

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

		peer := netip.MustParseAddrPort("127.0.0.1:19901")
		target := mavlink.ActuationTarget{
			SysID:                   1,
			EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
			Domain:                  uxsvocab.DomainQuadcopter,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			CompanionMode:           uxsvocab.ModeTeleopVelocity,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			Armed:                   false,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: now.UnixNano(),
		}
		targets.set(target)

		armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
		if err != nil {
			t.Fatalf("SubmitCommand Arm: %v", err)
		}
		armAck := mavcodec.CommandAck{
			Command: mavcodec.MAVCmdComponentArmDisarm,
			Result:  mavcodec.MAVResultAccepted,
		}
		now = now.Add(20 * time.Millisecond)
		svc.OnCommandAckFrame(1, 71, armAck, now)
		if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("Arm after seq=71 ACK = %+v, want confirmed", ent)
		}

		// Vehicle transitions to Armed=true.
		now = now.Add(10 * time.Millisecond)
		target.Armed = true
		target.HeartbeatObservedUnixNs = now.UnixNano()
		targets.set(target)

		// Replay duplicate Arm ACK (seq=71) BEFORE Disarm while idle.
		now = now.Add(10 * time.Millisecond)
		svc.OnCommandAckFrame(1, 71, armAck, now)

		// Submit Disarm (vehicle still reports Armed=true).
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}

		// Replay duplicate Arm ACK (seq=71) AFTER Disarm without any disarmed heartbeat!
		now = now.Add(15 * time.Millisecond)
		svc.OnCommandAckFrame(1, 71, armAck, now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhasePending {
			t.Fatalf("Disarm after duplicate seq=71 Arm ACK = %+v, want pending (never confirmed while armed)", ent)
		}

		// Genuine distinct seq=72 Disarm ACK now confirms Disarm cleanly.
		now = now.Add(15 * time.Millisecond)
		svc.OnCommandAckFrame(1, 72, armAck, now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm after distinct seq=72 ACK = %+v, want confirmed", ent)
		}
	})

	// Subcase B: Sequence-less OnCommandAck — duplicate Arm ACCEPTED delivered after or before Disarm
	// cannot prove packet ownership, so Disarm MUST enter commit_unknown (never confirmed while armed)
	// until a fresh disarmed #0 HEARTBEAT arrives.
	for _, dupBeforeDisarm := range []bool{false, true} {
		name := "unsequenced_duplicate_after_disarm"
		if dupBeforeDisarm {
			name = "unsequenced_duplicate_before_disarm"
		}
		t.Run(name, func(t *testing.T) {
			targets := newFakeTargetSource()
			sender := &fakeFrameSender{}
			store := newFakeCommandStore()
			now := time.Date(2026, 11, 27, 16, 15, 0, 0, time.UTC)

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
				PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19902"),
				HeartbeatObservedUnixNs: now.UnixNano(),
			}
			targets.set(target)

			armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
			if err != nil {
				t.Fatalf("SubmitCommand Arm: %v", err)
			}
			ackAccepted := mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			}
			now = now.Add(15 * time.Millisecond)
			svc.OnCommandAck(1, ackAccepted, now)
			if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed {
				t.Fatalf("Arm after first ACK = %+v, want confirmed", ent)
			}

			if dupBeforeDisarm {
				now = now.Add(10 * time.Millisecond)
				svc.OnCommandAck(1, ackAccepted, now)
			}

			disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand Disarm: %v", err)
			}

			now = now.Add(10 * time.Millisecond)
			svc.OnCommandAck(1, ackAccepted, now)
			ent, _ := store.get(disarmRes.CommandID)
			if ent.PhaseState == PhaseConfirmed {
				t.Fatalf("duplicate unsequenced ACK falsely confirmed Disarm with Armed=true: %+v", ent)
			}
			if ent.PhaseState != PhaseCommitUnknown {
				t.Fatalf("Disarm phase = %q, want commit_unknown", ent.PhaseState)
			}

			// Armed heartbeat MUST NOT confirm Disarm; only a fresh disarmed #0 HEARTBEAT reconciles it!
			now = now.Add(20 * time.Millisecond)
			target.HeartbeatObservedUnixNs = now.UnixNano()
			targets.set(target)
			svc.Advance(context.Background(), now)
			if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
				t.Fatalf("Disarm reconciled while still Armed=true: %+v", ent)
			}

			now = now.Add(20 * time.Millisecond)
			target.Armed = false
			target.HeartbeatObservedUnixNs = now.UnixNano()
			targets.set(target)
			svc.OnTransitionEvent(mavlink.TransitionEvent{
				Kind:      mavlink.EventKindHeartbeatEdge,
				MsgID:     mavcodec.MsgIDHeartbeat,
				SysID:     1,
				EntityID:  target.EntityID,
				Authority: target.Authority,
				LinkTier:  target.LinkTier,
				Armed:     false,
				Timestamp: now.UnixMilli(),
			})
			if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
				t.Fatalf("Disarm after fresh disarmed heartbeat = %+v, want confirmed", ent)
			}
		})
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceIdleAckConsumptionPersistsAcrossRestartBeforeExplicitDenial(t *testing.T) {
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 16, 20, 0, 0, time.UTC)
	peer := netip.MustParseAddrPort("127.0.0.1:19911")

	targets1 := newFakeTargetSource()
	sender1 := &fakeFrameSender{}
	svc1, err := NewService(Options{
		Org:               "boxbotix",
		Platform:          "bbops",
		Targets:           targets1,
		Sender:            sender1,
		Store:             store,
		Now:               func() time.Time { return now },
		CommandAckTimeout: 1000 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService boot 1: %v", err)
	}

	target := mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   false,
		PeerAddr:                peer,
		HeartbeatObservedUnixNs: now.UnixNano(),
	}
	targets1.set(target)

	// 1) Submit Arm -> timeout to commit_unknown -> reconcile via armed #0 HEARTBEAT (AckResult == -1).
	armRes, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
	if err != nil {
		t.Fatalf("SubmitCommand Arm: %v", err)
	}
	now = now.Add(1100 * time.Millisecond)
	svc1.Advance(context.Background(), now)
	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
		t.Fatalf("Arm after timeout = %+v, want commit_unknown", ent)
	}

	now = now.Add(50 * time.Millisecond)
	target.Armed = true
	target.HeartbeatObservedUnixNs = now.UnixNano()
	targets1.set(target)
	svc1.OnTransitionEvent(mavlink.TransitionEvent{
		Kind:      mavlink.EventKindHeartbeatEdge,
		MsgID:     mavcodec.MsgIDHeartbeat,
		SysID:     1,
		EntityID:  target.EntityID,
		Authority: target.Authority,
		LinkTier:  target.LinkTier,
		Armed:     true,
		Timestamp: now.UnixMilli(),
	})
	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed || ent.AckResult != UnacknowledgedResult {
		t.Fatalf("Arm after heartbeat reconcile = %+v, want PhaseConfirmed AckResult=-1", ent)
	}

	// 2) Deliver Arm's sole late #77 ACCEPTED while idle (no active command in flight).
	now = now.Add(50 * time.Millisecond)
	svc1.OnCommandAckFrame(1, 19, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultAccepted,
	}, now)
	wantAcceptedMask := (1 << int(mavcodec.MAVResultAccepted)) | ackPoolFormatV2Bit
	if ent, _ := store.get(armRes.CommandID); ent.AckResult != UnacknowledgedResult || ent.AckAccounted != 1 || ent.AckPoolMask != wantAcceptedMask {
		t.Fatalf("Arm after idle ACK consumption = %+v, want AckResult=-1 AckAccounted=1 AckPoolMask=%d", ent, wantAcceptedMask)
	}

	// 3) Restart immediately on the same store (Boot 2), submit Disarm, and deliver explicit DENIED.
	targets2 := newFakeTargetSource()
	targets2.set(target)
	sender2 := &fakeFrameSender{}
	svc2, err := NewService(Options{
		Org:               "boxbotix",
		Platform:          "bbops",
		Targets:           targets2,
		Sender:            sender2,
		Store:             store,
		Now:               func() time.Time { return now },
		CommandAckTimeout: 1000 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService boot 2: %v", err)
	}

	now = now.Add(50 * time.Millisecond)
	disarmRes, err := svc2.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
	if err != nil {
		t.Fatalf("SubmitCommand Disarm on boot 2: %v", err)
	}

	now = now.Add(20 * time.Millisecond)
	svc2.OnCommandAckFrame(1, 20, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultDenied,
	}, now)

	disarmEnt, ok := store.get(disarmRes.CommandID)
	if !ok || disarmEnt.PhaseState != PhaseRejected || disarmEnt.AckResult != int(mavcodec.MAVResultDenied) {
		t.Fatalf("Disarm after explicit DENIED on boot 2 = %+v, want PhaseRejected AckResult=%d (not commit_unknown)", disarmEnt, mavcodec.MAVResultDenied)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceOverdueTeleopRenewalLosingWorkerRaceNeverExtendsExpiredLease(t *testing.T) {
	targets := newFakeTargetSource()
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	now := time.Date(2026, 11, 27, 16, 30, 0, 0, time.UTC)

	svc, err := NewService(Options{
		Org:            "boxbotix",
		Platform:       "bbops",
		Targets:        targets,
		Sender:         sender,
		Store:          store,
		Now:            func() time.Time { return now },
		TeleopLeaseTTL: 220 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	peer := netip.MustParseAddrPort("127.0.0.1:19921")
	targets.set(mavlink.ActuationTarget{
		SysID:                   1,
		EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
		Domain:                  uxsvocab.DomainQuadcopter,
		Authority:               uxsvocab.AuthorityCompanionTeleop,
		AuthorityEpoch:          1,
		CompanionMode:           uxsvocab.ModeTeleopVelocity,
		LinkTier:                uxsvocab.LinkTierSITLLocal,
		Armed:                   true,
		PeerAddr:                peer,
		HeartbeatObservedUnixNs: now.UnixNano(),
	})

	// 1) Initial Engage nudge with Vx=1.0 (TTL = 250ms).
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:          1,
		Session:        "s1-race",
		Seq:            1,
		Vx:             1.0,
		Engage:         true,
		AuthorityEpoch: 1,
		ObservedMs:     now.UnixMilli(),
	}); err != nil {
		t.Fatalf("initial Engage SubmitTeleop: %v", err)
	}
	if !svc.HasActiveLease(1) {
		t.Fatal("expected active lease at t=0")
	}

	// 2) Advance clock 251ms (> 250ms TTL) WITHOUT running Advance first so SubmitTeleop
	// acquires egressMu[1] ahead of the background expiry worker!
	now = now.Add(251 * time.Millisecond)
	if svc.HasActiveLease(1) {
		t.Fatal("HasActiveLease(1) reported true 251ms after 250ms TTL")
	}

	var ce *ControlError
	_, err = svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "s1-race",
		Seq:     2,
		Vx:      2.0,
	})
	if !errors.As(err, &ce) || (ce.Code != "teleop_session_stopped" && ce.Code != "teleop_not_engaged") {
		t.Fatalf("overdue renewal got err=%v, want teleop_session_stopped/teleop_not_engaged", err)
	}
	if svc.HasActiveLease(1) {
		t.Fatal("overdue renewal extended an expired lease")
	}

	extractVx := func() []float32 {
		var vx []float32
		for _, rec := range sender.snapshot() {
			if sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED); ok {
				vx = append(vx, sp.Vx)
			}
		}
		return vx
	}
	if got := extractVx(); !slices.Equal(got, []float32{1.0, 0.0}) {
		t.Fatalf("recorded #84 Vx immediately after overdue renewal (before Advance) = %v, want [1 0]", got)
	}

	// Subsequent Advance tick must NOT emit a duplicate second zero-velocity frame.
	svc.Advance(context.Background(), now.Add(10*time.Millisecond))
	if got := extractVx(); !slices.Equal(got, []float32{1.0, 0.0}) {
		t.Fatalf("recorded #84 Vx after subsequent Advance = %v, want [1 0]", got)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceDelayedPreOverrideEngageRejectedAfterRecovery(t *testing.T) {
	for _, useEpoch := range []bool{true, false} {
		name := "authority_epoch"
		if !useEpoch {
			name = "observed_ms_fallback"
		}
		t.Run(name, func(t *testing.T) {
			targets := newFakeTargetSource()
			sender := &fakeFrameSender{}
			store := newFakeCommandStore()
			t0 := time.Date(2026, 11, 27, 16, 40, 0, 0, time.UTC)
			now := t0

			svc, err := NewService(Options{
				Org:            "boxbotix",
				Platform:       "bbops",
				Targets:        targets,
				Sender:         sender,
				Store:          store,
				Now:            func() time.Time { return now },
				TeleopLeaseTTL: 220 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}

			target := mavlink.ActuationTarget{
				SysID:                   1,
				EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
				Domain:                  uxsvocab.DomainQuadcopter,
				Authority:               uxsvocab.AuthorityCompanionTeleop,
				AuthorityEpoch:          1,
				CompanionMode:           uxsvocab.ModeTeleopVelocity,
				LinkTier:                uxsvocab.LinkTierSITLLocal,
				Armed:                   true,
				PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19931"),
				HeartbeatObservedUnixNs: t0.UnixNano(),
			}
			targets.set(target)

			// Operator clicked Engage at t0 BEFORE any lease existed on the server; request is delayed in transit.
			delayedReq := TeleopRequest{
				SysID:      1,
				Session:    "s1-delayed-pre-override",
				Seq:        1,
				Vx:         0.9,
				Engage:     true,
				ObservedMs: t0.UnixMilli(),
			}
			if useEpoch {
				delayedReq.AuthorityEpoch = 1
			}

			// At t0+20ms: P1 CRSF override arrives when NO lease exists yet -> latches preemption.
			now = t0.Add(20 * time.Millisecond)
			target.Authority = uxsvocab.AuthorityDirectCRSFOverride
			target.AuthorityEpoch = 2
			target.HeartbeatObservedUnixNs = now.UnixNano()
			targets.set(target)
			svc.OnTransitionEvent(mavlink.TransitionEvent{
				Kind:           mavlink.EventKindHeartbeatEdge,
				MsgID:          mavcodec.MsgIDHeartbeat,
				SysID:          1,
				EntityID:       target.EntityID,
				Authority:      uxsvocab.AuthorityDirectCRSFOverride,
				AuthorityEpoch: 2,
				LinkTier:       target.LinkTier,
				Armed:          true,
				Timestamp:      now.UnixMilli(),
			})

			// At t0+40ms: P2 companion-teleop recovers.
			tRecovery := t0.Add(40 * time.Millisecond)
			now = tRecovery
			target.Authority = uxsvocab.AuthorityCompanionTeleop
			target.AuthorityEpoch = 3
			target.HeartbeatObservedUnixNs = tRecovery.UnixNano()
			targets.set(target)
			svc.OnTransitionEvent(mavlink.TransitionEvent{
				Kind:           mavlink.EventKindHeartbeatEdge,
				MsgID:          mavcodec.MsgIDHeartbeat,
				SysID:          1,
				EntityID:       target.EntityID,
				Authority:      uxsvocab.AuthorityCompanionTeleop,
				AuthorityEpoch: 3,
				LinkTier:       target.LinkTier,
				Armed:          true,
				Timestamp:      tRecovery.UnixMilli(),
			})

			// At t0+60ms: delayed pre-override Engage=true arrives -> MUST be rejected with preemption_latched!
			now = t0.Add(60 * time.Millisecond)
			var ce *ControlError
			if _, err := svc.SubmitTeleop(context.Background(), delayedReq); !errors.As(err, &ce) || ce.Code != "preemption_latched" {
				t.Fatalf("delayed pre-override Engage=true got err=%v, want preemption_latched", err)
			}
			if svc.HasActiveLease(1) || !svc.IsPreempted(1) {
				t.Fatalf("after delayed pre-override Engage: HasActiveLease=%v IsPreempted=%v, want false/true", svc.HasActiveLease(1), svc.IsPreempted(1))
			}
			if got := countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot()); got != 0 {
				t.Fatalf("transmitted %d #84 frames for rejected pre-override Engage, want 0", got)
			}

			// Subsequent post-recovery operator Engage click (observing tRecovery / epoch=3) succeeds cleanly!
			freshReq := TeleopRequest{
				SysID:      1,
				Session:    "s1-post-recovery",
				Seq:        1,
				Vx:         0.5,
				Engage:     true,
				ObservedMs: tRecovery.UnixMilli(),
			}
			if useEpoch {
				freshReq.AuthorityEpoch = 3
			}
			if _, err := svc.SubmitTeleop(context.Background(), freshReq); err != nil {
				t.Fatalf("post-recovery Engage=true failed: %v", err)
			}
			if !svc.HasActiveLease(1) || svc.IsPreempted(1) {
				t.Fatal("expected post-recovery Engage=true to activate lease and clear preemption latch")
			}
		})
	}
}

func makeTestVehicleHeartbeatFrame(t *testing.T, sysid, seq uint8, armed bool, mode mavcodec.CompanionMode, auth mavcodec.ControlAuthority) mavcodec.Frame {
	return makeTestVehicleHeartbeatFrameWithTier(t, sysid, seq, armed, mode, auth, mavcodec.LinkTierSITLLocal)
}

func makeTestVehicleHeartbeatFrameWithTier(t *testing.T, sysid, seq uint8, armed bool, mode mavcodec.CompanionMode, auth mavcodec.ControlAuthority, tier mavcodec.LinkTier) mavcodec.Frame {
	t.Helper()
	hb, err := mavcodec.EncodeVehicleHeartbeat(mavcodec.VehicleHeartbeat{
		Armed:        armed,
		SystemStatus: mavcodec.MAVStateActive,
		Custom: mavcodec.CustomMode{
			LinkTier:         tier,
			Domain:           mavcodec.DomainQuadcopter,
			CompanionMode:    mode,
			ControlAuthority: auth,
			HealthFlags: uxsvocab.HealthFlags{
				ClockSyncLocked:       true,
				CRSFLinkHealthy:       true,
				CompanionTrunkHealthy: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("EncodeVehicleHeartbeat: %v", err)
	}
	return mavcodec.Frame{
		Sequence:    seq,
		SystemID:    sysid,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message:     hb,
	}
}

func deliverTrackerOutcome(svc *Service, out mavlink.IngestOutcome, now time.Time) {
	for _, ev := range out.Events {
		svc.OnTransitionEvent(ev)
	}
	if out.CommandAck != nil {
		svc.OnCommandAckFrame(out.SysID, out.CommandAckSeq, *out.CommandAck, now)
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServicePostLinkRestoreDelayedOldAckNeverConfirmsDisarmWhileArmed(t *testing.T) {
	t.Run("delayed_pre_loss_accepted_ack_stays_commit_unknown_until_disarmed_heartbeat", func(t *testing.T) {
		tracker, err := mavlink.NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
		if err != nil {
			t.Fatalf("NewTrackerWithTimeouts: %v", err)
		}
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)

		svc, err := NewService(Options{
			Org:               "boxbotix",
			Platform:          "bbops",
			Targets:           tracker,
			Sender:            sender,
			Store:             store,
			Now:               func() time.Time { return now },
			CommandAckTimeout: 1500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		peer := netip.MustParseAddrPort("127.0.0.1:19941")

		// 1) Admit disarmed vehicle on peer, submit Arm, confirm via seq=71 ACCEPTED, then armed #0 HEARTBEAT.
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 10, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(20 * time.Millisecond)
		armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
		if err != nil {
			t.Fatalf("SubmitCommand Arm: %v", err)
		}

		now = now.Add(20 * time.Millisecond)
		ackOut := tracker.IngestFrame(mavcodec.Frame{
			Sequence:    71,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now)
		deliverTrackerOutcome(svc, ackOut, now)
		if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed || ent.AckResult != int(mavcodec.MAVResultAccepted) {
			t.Fatalf("Arm after seq=71 ACCEPTED = %+v, want PhaseConfirmed AckResult=0", ent)
		}

		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 72, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		// 2) Cross link-loss silence boundary, then restore link on same peer while vehicle is STILL ARMED.
		now = now.Add(3100 * time.Millisecond)
		for _, ev := range tracker.EvaluateSilence(now) {
			svc.OnTransitionEvent(ev)
		}
		now = now.Add(40 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 73, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		// 3) Operator submits Disarm, then delayed network duplicate of Arm's old seq=71 ACCEPTED arrives!
		now = now.Add(20 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}

		now = now.Add(15 * time.Millisecond)
		replayOut := tracker.IngestFrame(mavcodec.Frame{
			Sequence:    71,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now)
		if replayOut.CommandAck == nil {
			t.Fatal("expected Tracker post-link-restore to forward seq=71 COMMAND_ACK after slot reset")
		}
		deliverTrackerOutcome(svc, replayOut, now)

		disarmEnt, ok := store.get(disarmRes.CommandID)
		if !ok || disarmEnt.PhaseState != PhaseCommitUnknown || disarmEnt.AckResult != UnacknowledgedResult {
			t.Fatalf("Disarm after delayed pre-loss seq=71 ACCEPTED = %+v, want PhaseCommitUnknown AckResult=-1 (never false PhaseConfirmed while armed)", disarmEnt)
		}

		// 4) Subsequent ARMED #0 HEARTBEAT must keep Disarm in commit_unknown.
		now = now.Add(50 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 74, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm while vehicle remains ARMED = %+v, want PhaseCommitUnknown", ent)
		}

		// 5) Genuine DISARMED #0 HEARTBEAT reconciles Disarm to PhaseConfirmed.
		now = now.Add(50 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 75, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed || ent.AckResult != UnacknowledgedResult {
			t.Fatalf("Disarm after fresh disarmed HEARTBEAT = %+v, want PhaseConfirmed AckResult=-1", ent)
		}
	})

	t.Run("unseen_denied_result_after_link_restore_still_rejects_immediately", func(t *testing.T) {
		tracker, err := mavlink.NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
		if err != nil {
			t.Fatalf("NewTrackerWithTimeouts: %v", err)
		}
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 18, 5, 0, 0, time.UTC)

		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   sender,
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		peer := netip.MustParseAddrPort("127.0.0.1:19942")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(10 * time.Millisecond)
		if _, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm}); err != nil {
			t.Fatalf("SubmitCommand Arm: %v", err)
		}
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    11,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)

		now = now.Add(3100 * time.Millisecond)
		for _, ev := range tracker.EvaluateSilence(now) {
			svc.OnTransitionEvent(ev)
		}
		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 12, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(20 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}
		now = now.Add(15 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    11,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)

		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseRejected || ent.AckResult != int(mavcodec.MAVResultDenied) {
			t.Fatalf("Disarm on unseen DENIED after link_restored = %+v, want PhaseRejected AckResult=%d", ent, mavcodec.MAVResultDenied)
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceSequenceWrapAcceptsGenuineDenialAt255And511Frames(t *testing.T) {
	for _, interveningTelemetry := range []int{0, 10, 100, 190, 255, 511} {
		t.Run(fmt.Sprintf("intervening_%d_frames", interveningTelemetry), func(t *testing.T) {
			tracker, err := mavlink.NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
			if err != nil {
				t.Fatalf("NewTrackerWithTimeouts: %v", err)
			}
			sender := &fakeFrameSender{}
			store := newFakeCommandStore()
			now := time.Date(2026, 11, 27, 18, 15, 0, 0, time.UTC)

			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   sender,
				Store:    store,
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}

			peer := netip.MustParseAddrPort("127.0.0.1:19943")
			seq := uint8(0)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, seq, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

			// First Hold command rejected at seq=42.
			now = now.Add(10 * time.Millisecond)
			hold1, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
			if err != nil {
				t.Fatalf("SubmitCommand hold1: %v", err)
			}
			ackSeq := uint8(42)
			now = now.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
				Sequence:    ackSeq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdNavLoiterUnlim,
					Result:  mavcodec.MAVResultDenied,
				},
			}, peer, now), now)
			if ent, _ := store.get(hold1.CommandID); ent.PhaseState != PhaseRejected {
				t.Fatalf("hold1 = %+v, want PhaseRejected", ent)
			}

			// Emit exact count of intervening #31 ATTITUDE_QUATERNION telemetry frames.
			curSeq := ackSeq
			for i := 0; i < interveningTelemetry; i++ {
				curSeq++
				now = now.Add(5 * time.Millisecond)
				out := tracker.IngestFrame(mavcodec.Frame{
					Sequence:    curSeq,
					SystemID:    1,
					ComponentID: mavcodec.CompIDAutopilot1,
					Message:     mavcodec.AttitudeQuaternion{Q1: 1},
				}, peer, now)
				if !out.Accepted {
					t.Fatalf("telemetry frame %d was not accepted", i)
				}
			}

			// Second Hold command followed by #77 COMMAND_ACK DENIED at seq=42.
			now = now.Add(5 * time.Millisecond)
			hold2, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
			if err != nil {
				t.Fatalf("SubmitCommand hold2: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			out2 := tracker.IngestFrame(mavcodec.Frame{
				Sequence:    ackSeq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdNavLoiterUnlim,
					Result:  mavcodec.MAVResultDenied,
				},
			}, peer, now)
			deliverTrackerOutcome(svc, out2, now)

			ent2, _ := store.get(hold2.CommandID)
			if interveningTelemetry < 128 {
				// Out-of-order delayed duplicate within backward sequence window -> suppressed!
				if out2.CommandAck != nil || ent2.PhaseState != PhasePending {
					t.Fatalf("expected duplicate suppression at %d intervening frames, got CommandAck=%v phase=%s", interveningTelemetry, out2.CommandAck, ent2.PhaseState)
				}
			} else if interveningTelemetry < 255 {
				// Boundary 64..127-frame trailing gap immediately preceding seq=42 (delta in 65..128):
				// stream position is genuinely uncertain between a delayed backward duplicate and a wrapped
				// reply after packet loss, so Tracker preserves the frame as uncertain and Service settles
				// owner-neutrally into PhaseCommitUnknown with negative receipt accounting.
				wantMask := 1 << int(mavcodec.MAVResultDenied)
				if out2.CommandAck == nil {
					t.Fatalf("expected uncertain boundary COMMAND_ACK to be preserved at %d intervening frames", interveningTelemetry)
				}
				if ent2.PhaseState != PhaseCommitUnknown || ent2.AckResult != UnacknowledgedResult || ent2.AckAccounted != 1 || (ent2.AckPoolMask&wantMask) == 0 {
					t.Fatalf("expected PhaseCommitUnknown AckResult=-1 AckAccounted=1 AckPoolMask|=%d at %d intervening frames, got %+v", wantMask, interveningTelemetry, ent2)
				}
			} else {
				// >= 255 intervening frames (1 full 256-sequence wrap at 255; 2 wraps at 511):
				// Tracker accepts the wrapped frame (never dropped!), while Service preserves an identical
				// repeated (seq=42, DENIED) receipt in PhaseCommitUnknown (AckResult=-1, AckAccounted=1)
				// so a delayed copy from hold1 cannot permanently lock hold2 into PhaseRejected; and a
				// distinct wrapped denial result (TemporarilyRejected) settles PhaseRejected directly.
				wantMask := 1 << int(mavcodec.MAVResultDenied)
				if out2.CommandAck == nil {
					t.Fatalf("Tracker dropped wrapped COMMAND_ACK after %d intervening telemetry frames", interveningTelemetry)
				}
				if ent2.PhaseState != PhaseCommitUnknown || ent2.AckResult != UnacknowledgedResult || ent2.AckAccounted != 1 || (ent2.AckPoolMask&wantMask) == 0 {
					t.Fatalf("Service mishandled wrapped identical (seq,DENIED) after %d intervening frames: ent2=%+v, want PhaseCommitUnknown AckResult=-1 AckAccounted=1", interveningTelemetry, ent2)
				}
			}
		})
	}

	t.Run("distinct_result_after_255_frames_settles_rejected", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 15, 40, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19944")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 0, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(10 * time.Millisecond)
		hold1, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		if err != nil {
			t.Fatalf("SubmitCommand hold1: %v", err)
		}
		ackSeq := uint8(42)
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    ackSeq,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdNavLoiterUnlim,
				Result:  mavcodec.MAVResultTemporarilyRejected,
			},
		}, peer, now), now)
		if ent1, _ := store.get(hold1.CommandID); ent1.PhaseState != PhaseRejected {
			t.Fatalf("hold1 = %+v, want PhaseRejected", ent1)
		}

		curSeq := ackSeq
		for i := 0; i < 255; i++ {
			curSeq++
			now = now.Add(2 * time.Millisecond)
			tracker.IngestFrame(mavcodec.Frame{
				Sequence:    curSeq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, now)
		}
		now = now.Add(5 * time.Millisecond)
		hold2, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		if err != nil {
			t.Fatalf("SubmitCommand hold2: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		out2 := tracker.IngestFrame(mavcodec.Frame{
			Sequence:    ackSeq,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdNavLoiterUnlim,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now)
		deliverTrackerOutcome(svc, out2, now)
		ent2, _ := store.get(hold2.CommandID)
		if out2.CommandAck == nil || ent2.PhaseState != PhaseRejected || ent2.AckResult != int(mavcodec.MAVResultDenied) {
			t.Fatalf("expected distinct wrapped denial at 255 frames to settle PhaseRejected, got out2.CommandAck=%v ent2=%+v", out2.CommandAck, ent2)
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceAmbiguousAckPoolAccountingIsOrderIndependentAndOwnerNeutral(t *testing.T) {
	runOrder := func(t *testing.T, first, second uint8) (CommandEntity, CommandEntity, *fakeCommandStore, time.Time, mavlink.ActuationTarget) {
		t.Helper()
		targets := newFakeTargetSource()
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 18, 25, 0, 0, time.UTC)

		svc, err := NewService(Options{
			Org:               "boxbotix",
			Platform:          "bbops",
			Targets:           targets,
			Sender:            sender,
			Store:             store,
			Now:               func() time.Time { return now },
			CommandAckTimeout: 1000 * time.Millisecond,
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
			PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19944"),
			HeartbeatObservedUnixNs: now.UnixNano(),
		}
		targets.set(target)

		// Arm times out to commit_unknown (still unacked), then emergency Disarm is submitted -> 2 unacked cmdID=400 commands!
		armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
		if err != nil {
			t.Fatalf("SubmitCommand Arm: %v", err)
		}
		now = now.Add(1100 * time.Millisecond)
		svc.Advance(context.Background(), now)

		now = now.Add(20 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}

		now = now.Add(15 * time.Millisecond)
		svc.OnCommandAckFrame(1, 31, mavcodec.CommandAck{
			Command: mavcodec.MAVCmdComponentArmDisarm,
			Result:  first,
		}, now)
		now = now.Add(15 * time.Millisecond)
		svc.OnCommandAckFrame(1, 32, mavcodec.CommandAck{
			Command: mavcodec.MAVCmdComponentArmDisarm,
			Result:  second,
		}, now)

		armEnt, _ := store.get(armRes.CommandID)
		disarmEnt, _ := store.get(disarmRes.CommandID)
		return armEnt, disarmEnt, store, now, target
	}

	armDeniedFirst, disarmDeniedFirst, store1, now1, target1 := runOrder(t, mavcodec.MAVResultDenied, mavcodec.MAVResultAccepted)
	armAcceptedFirst, disarmAcceptedFirst, _, _, _ := runOrder(t, mavcodec.MAVResultAccepted, mavcodec.MAVResultDenied)

	wantMask := (1 << int(mavcodec.MAVResultAccepted)) | (1 << int(mavcodec.MAVResultDenied)) | ackPoolFormatV2Bit
	for label, ent := range map[string]CommandEntity{
		"arm(DENIED->ACCEPTED)":    armDeniedFirst,
		"disarm(DENIED->ACCEPTED)": disarmDeniedFirst,
		"arm(ACCEPTED->DENIED)":    armAcceptedFirst,
		"disarm(ACCEPTED->DENIED)": disarmAcceptedFirst,
	} {
		if ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("%s PhaseState = %q, want %q", label, ent.PhaseState, PhaseCommitUnknown)
		}
		if ent.AckResult != UnacknowledgedResult {
			t.Fatalf("%s AckResult = %d, want owner-neutral %d (must never bind ambiguous reply code onto command entity)", label, ent.AckResult, UnacknowledgedResult)
		}
		if ent.AckAccounted != 1 {
			t.Fatalf("%s AckAccounted = %d, want 1", label, ent.AckAccounted)
		}
		if ent.AckPoolMask != wantMask {
			t.Fatalf("%s AckPoolMask = %d, want %d", label, ent.AckPoolMask, wantMask)
		}
	}

	// Verify restart recovery on store1 knows both unacked slots were accounted (unackedCount == 0):
	// reconcile Disarm via disarmed heartbeat, restart on store1, submit Arm, and verify an unseen
	// MAVResultTemporarilyRejected rejects Arm cleanly instead of forcing commit_unknown!
	now1 = now1.Add(40 * time.Millisecond)
	target1.Armed = false
	target1.CompanionMode = uxsvocab.ModeDisarmed
	target1.HeartbeatObservedUnixNs = now1.UnixNano()
	targetsReboot := newFakeTargetSource()
	targetsReboot.set(target1)
	svcReboot, err := NewService(Options{
		Org:      "boxbotix",
		Platform: "bbops",
		Targets:  targetsReboot,
		Sender:   &fakeFrameSender{},
		Store:    store1,
		Now:      func() time.Time { return now1 },
	})
	if err != nil {
		t.Fatalf("NewService reboot: %v", err)
	}
	svcReboot.OnTransitionEvent(mavlink.TransitionEvent{
		Kind:          mavlink.EventKindHeartbeatEdge,
		MsgID:         mavcodec.MsgIDHeartbeat,
		SysID:         1,
		EntityID:      target1.EntityID,
		Authority:     target1.Authority,
		CompanionMode: target1.CompanionMode,
		LinkTier:      target1.LinkTier,
		Armed:         false,
		Timestamp:     now1.UnixMilli(),
	})

	now1 = now1.Add(20 * time.Millisecond)
	freshArm, err := svcReboot.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
	if err != nil {
		t.Fatalf("SubmitCommand freshArm after reboot: %v", err)
	}
	now1 = now1.Add(10 * time.Millisecond)
	svcReboot.OnCommandAckFrame(1, 90, mavcodec.CommandAck{
		Command: mavcodec.MAVCmdComponentArmDisarm,
		Result:  mavcodec.MAVResultTemporarilyRejected,
	}, now1)
	if ent, _ := store1.get(freshArm.CommandID); ent.PhaseState != PhaseRejected || ent.AckResult != int(mavcodec.MAVResultTemporarilyRejected) {
		t.Fatalf("freshArm after reboot = %+v, want PhaseRejected AckResult=%d", ent, mavcodec.MAVResultTemporarilyRejected)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceEngageIncarnationFencingAndPostRecoverySessionProtection(t *testing.T) {
	t.Run("rejects_stale_incarnation_epoch_and_pre_override_observed_ms", func(t *testing.T) {
		trackerA, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker A: %v", err)
		}
		trackerB, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker B: %v", err)
		}
		if trackerA.Incarnation() == "" || trackerA.Incarnation() == trackerB.Incarnation() {
			t.Fatalf("expected distinct non-empty tracker incarnations: A=%q B=%q", trackerA.Incarnation(), trackerB.Incarnation())
		}

		peer := netip.MustParseAddrPort("127.0.0.1:19945")
		t0 := time.Date(2026, 11, 27, 18, 35, 0, 0, time.UTC)
		now := t0

		// Advance trackerA to epoch=9 via authority toggles on an earlier host incarnation.
		trackerA.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now)
		for i := 0; i < 4; i++ {
			now = now.Add(10 * time.Millisecond)
			trackerA.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, uint8(2+2*i), true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride), peer, now)
			now = now.Add(10 * time.Millisecond)
			trackerA.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, uint8(3+2*i), true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now)
		}
		targetA, ok := trackerA.ActuationTarget(1)
		if !ok || targetA.AuthorityEpoch != 9 {
			t.Fatalf("trackerA target = %+v, want epoch=9", targetA)
		}

		// Boot service on trackerB (fresh incarnation), run P1 override (epoch=2) + P2 recovery (epoch=3).
		sender := &fakeFrameSender{}
		svc, err := NewService(Options{
			Org:            "boxbotix",
			Platform:       "bbops",
			Targets:        trackerB,
			Sender:         sender,
			Store:          newFakeCommandStore(),
			Now:            func() time.Time { return now },
			TeleopLeaseTTL: 220 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		now = now.Add(20 * time.Millisecond)
		tPreOverride := now
		deliverTrackerOutcome(svc, trackerB.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 20, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svc, trackerB.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 21, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)

		now = now.Add(20 * time.Millisecond)
		tRecovery := now
		deliverTrackerOutcome(svc, trackerB.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 22, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		rejectedCases := []struct {
			name string
			req  TeleopRequest
		}{
			{
				name: "prior_incarnation_high_epoch_9",
				req: TeleopRequest{
					SysID:                1,
					Session:              "s-incA-e9",
					Seq:                  1,
					Vx:                   0.8,
					Engage:               true,
					AuthorityIncarnation: trackerA.Incarnation(),
					AuthorityEpoch:       9,
					ObservedMs:           tRecovery.UnixMilli(),
				},
			},
			{
				name: "missing_incarnation_spoofed_high_epoch_9",
				req: TeleopRequest{
					SysID:          1,
					Session:        "s-noinc-e9",
					Seq:            1,
					Vx:             0.8,
					Engage:         true,
					AuthorityEpoch: 9,
				},
			},
			{
				name: "prior_incarnation_matching_epoch_3",
				req: TeleopRequest{
					SysID:                1,
					Session:              "s-incA-e3",
					Seq:                  1,
					Vx:                   0.8,
					Engage:               true,
					AuthorityIncarnation: trackerA.Incarnation(),
					AuthorityEpoch:       3,
					ObservedMs:           tRecovery.UnixMilli(),
				},
			},
			{
				name: "current_incarnation_epoch_3_with_stale_pre_override_observed_ms",
				req: TeleopRequest{
					SysID:                1,
					Session:              "s-incB-stale-obs",
					Seq:                  1,
					Vx:                   0.8,
					Engage:               true,
					AuthorityIncarnation: trackerB.Incarnation(),
					AuthorityEpoch:       3,
					ObservedMs:           tPreOverride.UnixMilli(),
				},
			},
		}

		for _, tc := range rejectedCases {
			var ce *ControlError
			if _, err := svc.SubmitTeleop(context.Background(), tc.req); !errors.As(err, &ce) || ce.Code != "preemption_latched" {
				t.Fatalf("%s: got err=%v, want preemption_latched", tc.name, err)
			}
			if svc.HasActiveLease(1) {
				t.Fatalf("%s: unexpectedly activated teleop lease", tc.name)
			}
		}

		// Valid post-recovery Engage carrying trackerB.Incarnation(), epoch=3, and tRecovery succeeds!
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s-incB-valid",
			Seq:                  1,
			Vx:                   0.5,
			Engage:               true,
			AuthorityIncarnation: trackerB.Incarnation(),
			AuthorityEpoch:       3,
			ObservedMs:           tRecovery.UnixMilli(),
		}); err != nil {
			t.Fatalf("valid post-recovery Engage failed: %v", err)
		}
		if !svc.HasActiveLease(1) {
			t.Fatal("expected active lease after valid post-recovery Engage")
		}
	})

	for _, useEpoch := range []bool{false, true} {
		modeName := "observed_ms_fallback"
		if useEpoch {
			modeName = "authority_epoch"
		}
		t.Run("fresh_engage_then_delayed_pre_override_engage_never_evicts_active_session_"+modeName, func(t *testing.T) {
			targets := newFakeTargetSource()
			sender := &fakeFrameSender{}
			t0 := time.Date(2026, 11, 27, 18, 45, 0, 0, time.UTC)
			now := t0

			svc, err := NewService(Options{
				Org:            "boxbotix",
				Platform:       "bbops",
				Targets:        targets,
				Sender:         sender,
				Store:          newFakeCommandStore(),
				Now:            func() time.Time { return now },
				TeleopLeaseTTL: 220 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}

			target := mavlink.ActuationTarget{
				SysID:                   1,
				EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
				Domain:                  uxsvocab.DomainQuadcopter,
				Authority:               uxsvocab.AuthorityCompanionTeleop,
				AuthorityIncarnation:    "inc-cur",
				AuthorityEpoch:          1,
				CompanionMode:           uxsvocab.ModeTeleopVelocity,
				LinkTier:                uxsvocab.LinkTierSITLLocal,
				Armed:                   true,
				PeerAddr:                netip.MustParseAddrPort("127.0.0.1:19946"),
				HeartbeatObservedUnixNs: t0.UnixNano(),
			}
			targets.set(target)

			// Delayed pre-override Engage built at t0 BEFORE override!
			delayedPreOverrideReq := TeleopRequest{
				SysID:      1,
				Session:    "s1-stale-pre-override",
				Seq:        1,
				Vx:         1.9,
				Engage:     true,
				ObservedMs: t0.UnixMilli(),
			}
			if useEpoch {
				delayedPreOverrideReq.AuthorityIncarnation = "inc-cur"
				delayedPreOverrideReq.AuthorityEpoch = 1
			}

			// Override at t0+20ms (epoch=2), recovery at t0+40ms (epoch=3).
			now = t0.Add(20 * time.Millisecond)
			target.Authority = uxsvocab.AuthorityDirectCRSFOverride
			target.AuthorityEpoch = 2
			target.HeartbeatObservedUnixNs = now.UnixNano()
			targets.set(target)
			svc.OnTransitionEvent(mavlink.TransitionEvent{
				Kind:                 mavlink.EventKindHeartbeatEdge,
				MsgID:                mavcodec.MsgIDHeartbeat,
				SysID:                1,
				EntityID:             target.EntityID,
				Authority:            uxsvocab.AuthorityDirectCRSFOverride,
				AuthorityIncarnation: "inc-cur",
				AuthorityEpoch:       2,
				LinkTier:             target.LinkTier,
				Armed:                true,
				Timestamp:            now.UnixMilli(),
			})

			tRecovery := t0.Add(40 * time.Millisecond)
			now = tRecovery
			target.Authority = uxsvocab.AuthorityCompanionTeleop
			target.AuthorityEpoch = 3
			target.HeartbeatObservedUnixNs = tRecovery.UnixNano()
			targets.set(target)
			svc.OnTransitionEvent(mavlink.TransitionEvent{
				Kind:                 mavlink.EventKindHeartbeatEdge,
				MsgID:                mavcodec.MsgIDHeartbeat,
				SysID:                1,
				EntityID:             target.EntityID,
				Authority:            uxsvocab.AuthorityCompanionTeleop,
				AuthorityIncarnation: "inc-cur",
				AuthorityEpoch:       3,
				LinkTier:             target.LinkTier,
				Armed:                true,
				Timestamp:            tRecovery.UnixMilli(),
			})

			// 1) Valid post-recovery Engage ("s1-fresh-active") lands FIRST at t0+50ms!
			now = t0.Add(50 * time.Millisecond)
			freshReq := TeleopRequest{
				SysID:      1,
				Session:    "s1-fresh-active",
				Seq:        1,
				Vx:         0.4,
				Engage:     true,
				ObservedMs: tRecovery.UnixMilli(),
			}
			if useEpoch {
				freshReq.AuthorityIncarnation = "inc-cur"
				freshReq.AuthorityEpoch = 3
			}
			if _, err := svc.SubmitTeleop(context.Background(), freshReq); err != nil {
				t.Fatalf("fresh post-recovery Engage failed: %v", err)
			}
			if !svc.HasActiveLease(1) {
				t.Fatal("expected s1-fresh-active lease to be active")
			}

			// 2) Delayed pre-override Engage ("s1-stale-pre-override") arrives SECOND at t0+65ms:
			// MUST be rejected with preemption_latched and MUST NOT evict or overwrite s1-fresh-active!
			now = t0.Add(65 * time.Millisecond)
			var ce *ControlError
			if _, err := svc.SubmitTeleop(context.Background(), delayedPreOverrideReq); !errors.As(err, &ce) || ce.Code != "preemption_latched" {
				t.Fatalf("late pre-override Engage after fresh Engage got err=%v, want preemption_latched", err)
			}
			if !svc.HasActiveLease(1) {
				t.Fatal("late pre-override Engage evicted active fresh lease")
			}

			// 3) Renewal on s1-fresh-active (seq=2, Engage=false) MUST continue succeeding seamlessly!
			now = t0.Add(80 * time.Millisecond)
			if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:   1,
				Session: "s1-fresh-active",
				Seq:     2,
				Vx:      0.6,
			}); err != nil {
				t.Fatalf("s1-fresh-active renewal failed after rejected stale Engage: %v", err)
			}

			var gotVx []float32
			for _, rec := range sender.snapshot() {
				if sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED); ok {
					gotVx = append(gotVx, sp.Vx)
				}
			}
			if !slices.Equal(gotVx, []float32{0.4, 0.6}) {
				t.Fatalf("transmitted #84 Vx = %v, want [0.4 0.6] (never 1.9 from stale pre-override request)", gotVx)
			}
		})
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceSequenceWrapAmbiguousPositiveAcceptedAckRequiresFreshHeartbeat(t *testing.T) {
	t.Run("delayed wrapped ACCEPTED from prior Arm holds Disarm in commit_unknown until disarmed heartbeat", func(t *testing.T) {
		tracker, err := mavlink.NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
		if err != nil {
			t.Fatalf("NewTrackerWithTimeouts: %v", err)
		}
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 19, 0, 0, 0, time.UTC)

		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   sender,
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		peer := netip.MustParseAddrPort("127.0.0.1:19951")
		// Admit disarmed vehicle at seq=0.
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 0, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionAutonomous), peer, now), now)

		// 1) Operator submits Arm (cmd=400); vehicle replies at seq=42 with ACCEPTED and armed #0 HEARTBEAT at seq=43.
		now = now.Add(10 * time.Millisecond)
		armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
		if err != nil {
			t.Fatalf("SubmitCommand Arm: %v", err)
		}
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    42,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)
		if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed || ent.AckResult != int(mavcodec.MAVResultAccepted) {
			t.Fatalf("Arm = %+v, want PhaseConfirmed AckResult=0", ent)
		}
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 43, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		// 2) Vehicle emits 255 ordinary telemetry frames (seq=44..42->41) on an uninterrupted link,
		// aging seq=42 past the 256-sequence duplicate-frame window.
		curSeq := uint8(43)
		for i := 0; i < 254; i++ {
			curSeq++
			now = now.Add(5 * time.Millisecond)
			out := tracker.IngestFrame(mavcodec.Frame{
				Sequence:    curSeq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, now)
			if !out.Accepted {
				t.Fatalf("telemetry frame %d was not accepted", i)
			}
		}

		// 3) Operator submits Disarm (cmd=400) while vehicle is still Armed=true.
		now = now.Add(10 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}

		// Delayed network copy of Arm's (seq=42, cmd=400, ACCEPTED) arrives while Armed is still true!
		now = now.Add(10 * time.Millisecond)
		outDelayed := tracker.IngestFrame(mavcodec.Frame{
			Sequence:    42,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now)
		deliverTrackerOutcome(svc, outDelayed, now)

		entDisarm, _ := store.get(disarmRes.CommandID)
		if entDisarm.PhaseState != PhaseCommitUnknown || entDisarm.AckResult != UnacknowledgedResult {
			t.Fatalf("Disarm on wrap-ambiguous ACCEPTED while still Armed = %+v, want PhaseCommitUnknown AckResult=%d", entDisarm, UnacknowledgedResult)
		}

		// 4) Subsequent armed #0 HEARTBEAT MUST NOT falsely confirm Disarm.
		now = now.Add(200 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 43, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		svc.Advance(context.Background(), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm reconciled prematurely on armed heartbeat: %+v", ent)
		}

		// 5) Fresh disarmed #0 HEARTBEAT arrives -> Disarm reconciles to PhaseConfirmed!
		now = now.Add(200 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 44, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionAutonomous), peer, now), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm after fresh disarmed heartbeat = %+v, want PhaseConfirmed", ent)
		}
	})

	t.Run("genuine wrapped ACCEPTED confirms immediately when fresh post-dispatch heartbeat already reflects state", func(t *testing.T) {
		tracker, err := mavlink.NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
		if err != nil {
			t.Fatalf("NewTrackerWithTimeouts: %v", err)
		}
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 19, 5, 0, 0, time.UTC)

		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   sender,
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		peer := netip.MustParseAddrPort("127.0.0.1:19952")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 0, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionAutonomous), peer, now), now)

		now = now.Add(10 * time.Millisecond)
		armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
		if err != nil {
			t.Fatalf("SubmitCommand Arm: %v", err)
		}
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    42,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)
		if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("Arm = %+v, want PhaseConfirmed", ent)
		}
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 43, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		curSeq := uint8(43)
		for i := 0; i < 253; i++ {
			curSeq++
			now = now.Add(5 * time.Millisecond)
			tracker.IngestFrame(mavcodec.Frame{
				Sequence:    curSeq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, now)
		}

		now = now.Add(10 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}
		// Disarmed #0 HEARTBEAT arrives at seq=41 right before the wrapped ACCEPTED at seq=42!
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 41, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionAutonomous), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    42,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)

		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed || ent.AckResult != int(mavcodec.MAVResultAccepted) {
			t.Fatalf("Disarm with post-dispatch disarmed heartbeat + wrapped ACCEPTED = %+v, want PhaseConfirmed AckResult=0", ent)
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceSequenceWrapAcceptsGenuineDenialWithPacketLoss(t *testing.T) {
	dropCases := []struct {
		name     string
		dropSeqs map[uint8]bool
	}{
		{name: "drop_first_after_ack_seq_43", dropSeqs: map[uint8]bool{43: true}},
		{name: "drop_middle_seq_150", dropSeqs: map[uint8]bool{150: true}},
		{name: "drop_last_before_wrap_seq_41", dropSeqs: map[uint8]bool{41: true}},
		{name: "drop_5_packet_burst_seq_100_to_104", dropSeqs: map[uint8]bool{100: true, 101: true, 102: true, 103: true, 104: true}},
	}

	for _, tc := range dropCases {
		t.Run(tc.name, func(t *testing.T) {
			tracker, err := mavlink.NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
			if err != nil {
				t.Fatalf("NewTrackerWithTimeouts: %v", err)
			}
			sender := &fakeFrameSender{}
			store := newFakeCommandStore()
			now := time.Date(2026, 11, 27, 19, 10, 0, 0, time.UTC)

			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   sender,
				Store:    store,
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}

			peer := netip.MustParseAddrPort("127.0.0.1:19953")
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 0, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

			// 1) Hold #1 rejected at seq=42 with DENIED.
			now = now.Add(10 * time.Millisecond)
			hold1, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
			if err != nil {
				t.Fatalf("SubmitCommand hold1: %v", err)
			}
			now = now.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
				Sequence:    42,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdNavLoiterUnlim,
					Result:  mavcodec.MAVResultTemporarilyRejected,
				},
			}, peer, now), now)
			if ent, _ := store.get(hold1.CommandID); ent.PhaseState != PhaseRejected {
				t.Fatalf("hold1 = %+v, want PhaseRejected", ent)
			}

			// 2) Vehicle sends 255 intervening telemetry frames (43..255, 0..41), dropping tc.dropSeqs in UDP transit.
			curSeq := uint8(42)
			received := 0
			for i := 0; i < 255; i++ {
				curSeq++
				now = now.Add(5 * time.Millisecond)
				if tc.dropSeqs[curSeq] {
					continue
				}
				out := tracker.IngestFrame(mavcodec.Frame{
					Sequence:    curSeq,
					SystemID:    1,
					ComponentID: mavcodec.CompIDAutopilot1,
					Message:     mavcodec.AttitudeQuaternion{Q1: 1},
				}, peer, now)
				if !out.Accepted {
					t.Fatalf("telemetry frame seq=%d was not accepted", curSeq)
				}
				received++
			}
			if received != 255-len(tc.dropSeqs) {
				t.Fatalf("received %d telemetry frames, want %d", received, 255-len(tc.dropSeqs))
			}

			// 3) Hold #2 followed by genuine wrapped #77 COMMAND_ACK DENIED at seq=42!
			now = now.Add(10 * time.Millisecond)
			hold2, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
			if err != nil {
				t.Fatalf("SubmitCommand hold2: %v", err)
			}
			now = now.Add(10 * time.Millisecond)
			outAck := tracker.IngestFrame(mavcodec.Frame{
				Sequence:    42,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdNavLoiterUnlim,
					Result:  mavcodec.MAVResultDenied,
				},
			}, peer, now)
			if outAck.CommandAck == nil {
				t.Fatalf("Tracker dropped genuine wrapped COMMAND_ACK DENIED after %d received frames (%s)", received, tc.name)
			}
			deliverTrackerOutcome(svc, outAck, now)

			ent2, _ := store.get(hold2.CommandID)
			if ent2.PhaseState != PhaseRejected || ent2.AckResult != int(mavcodec.MAVResultDenied) {
				t.Fatalf("Service dropped genuine wrapped COMMAND_ACK DENIED (%s): ent2=%+v, want PhaseRejected AckResult=%d", tc.name, ent2, mavcodec.MAVResultDenied)
			}
		})
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceEarlierCommandUpdatedMsRemainsImmutableAcrossLaterCompletions(t *testing.T) {
	runWithStore := func(t *testing.T, store RecoverableCommandStore) {
		t.Helper()
		targets := newFakeTargetSource()
		sender := &fakeFrameSender{}
		t0 := time.Date(2026, 11, 27, 19, 20, 0, 0, time.UTC)
		now := t0

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

		peer := netip.MustParseAddrPort("127.0.0.1:19954")
		target := mavlink.ActuationTarget{
			SysID:                   1,
			EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
			Domain:                  uxsvocab.DomainQuadcopter,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			AuthorityEpoch:          1,
			CompanionMode:           uxsvocab.ModeTeleopVelocity,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			Armed:                   true,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: now.UnixNano(),
		}
		targets.set(target)

		// 1) Complete Hold #1 at t0 + 25ms (PhaseConfirmed).
		hold1, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		if err != nil {
			t.Fatalf("SubmitCommand hold1: %v", err)
		}
		tHold1Done := t0.Add(25 * time.Millisecond)
		now = tHold1Done
		svc.OnCommandAckFrame(1, 10, mavcodec.CommandAck{
			Command: mavcodec.MAVCmdNavLoiterUnlim,
			Result:  mavcodec.MAVResultAccepted,
		}, now)

		ent1Before, err := store.Get(context.Background(), hold1.CommandID)
		if err != nil || ent1Before.PhaseState != PhaseConfirmed || ent1Before.UpdatedMs != tHold1Done.UnixMilli() {
			t.Fatalf("hold1 after completion = %+v err=%v, want PhaseConfirmed UpdatedMs=%d", ent1Before, err, tHold1Done.UnixMilli())
		}

		// 2) 60 seconds later, submit and reject Hold #2 -> Hold #1.UpdatedMs MUST stay at tHold1Done!
		tHold2Issue := t0.Add(60 * time.Second)
		now = tHold2Issue
		target.HeartbeatObservedUnixNs = now.UnixNano()
		targets.set(target)
		hold2, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		if err != nil {
			t.Fatalf("SubmitCommand hold2: %v", err)
		}
		tHold2Done := tHold2Issue.Add(40 * time.Millisecond)
		now = tHold2Done
		svc.OnCommandAckFrame(1, 11, mavcodec.CommandAck{
			Command: mavcodec.MAVCmdNavLoiterUnlim,
			Result:  mavcodec.MAVResultDenied,
		}, now)

		ent1AfterHold2, err := store.Get(context.Background(), hold1.CommandID)
		if err != nil {
			t.Fatalf("Get hold1 after hold2: %v", err)
		}
		if ent1AfterHold2.UpdatedMs != tHold1Done.UnixMilli() {
			t.Fatalf("hold1.UpdatedMs mutated by later hold2 completion: got %d, want %d", ent1AfterHold2.UpdatedMs, tHold1Done.UnixMilli())
		}
		ent2Done, err := store.Get(context.Background(), hold2.CommandID)
		if err != nil || ent2Done.PhaseState != PhaseRejected || ent2Done.UpdatedMs != tHold2Done.UnixMilli() {
			t.Fatalf("hold2 = %+v err=%v, want PhaseRejected UpdatedMs=%d", ent2Done, err, tHold2Done.UnixMilli())
		}

		// 3) Another 60 seconds later, submit Hold #3, preempt it via CRSF override, then deliver its detached late ACK:
		// RecordConsumedAck updates AckAccounted on Hold #3 WITHOUT mutating Hold #3.UpdatedMs, Hold #2.UpdatedMs, or Hold #1.UpdatedMs!
		tHold3Issue := tHold2Issue.Add(60 * time.Second)
		now = tHold3Issue
		target.HeartbeatObservedUnixNs = now.UnixNano()
		targets.set(target)
		hold3, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		if err != nil {
			t.Fatalf("SubmitCommand hold3: %v", err)
		}
		tPreempt := tHold3Issue.Add(15 * time.Millisecond)
		now = tPreempt
		target.Authority = uxsvocab.AuthorityDirectCRSFOverride
		target.AuthorityEpoch = 2
		targets.set(target)
		svc.OnTransitionEvent(mavlink.TransitionEvent{
			Kind:           mavlink.EventKindHeartbeatEdge,
			MsgID:          mavcodec.MsgIDHeartbeat,
			SysID:          1,
			EntityID:       target.EntityID,
			Authority:      uxsvocab.AuthorityDirectCRSFOverride,
			AuthorityEpoch: 2,
			CompanionMode:  uxsvocab.ModeHoldPosition,
			LinkTier:       target.LinkTier,
			Armed:          true,
			Timestamp:      now.UnixMilli(),
		})

		tLateAck := tHold3Issue.Add(500 * time.Millisecond)
		now = tLateAck
		svc.OnCommandAckFrame(1, 12, mavcodec.CommandAck{
			Command: mavcodec.MAVCmdNavLoiterUnlim,
			Result:  mavcodec.MAVResultAccepted,
		}, now)

		ent3AfterLateAck, err := store.Get(context.Background(), hold3.CommandID)
		if err != nil {
			t.Fatalf("Get hold3 after late ACK: %v", err)
		}
		if ent3AfterLateAck.PhaseState != PhasePreempted || ent3AfterLateAck.AckAccounted != 1 {
			t.Fatalf("hold3 after detached late ACK = %+v, want PhasePreempted AckAccounted=1", ent3AfterLateAck)
		}
		if ent3AfterLateAck.UpdatedMs != tPreempt.UnixMilli() {
			t.Fatalf("hold3.UpdatedMs overwritten by RecordConsumedAck: got %d, want %d", ent3AfterLateAck.UpdatedMs, tPreempt.UnixMilli())
		}
		ent1Final, _ := store.Get(context.Background(), hold1.CommandID)
		ent2Final, _ := store.Get(context.Background(), hold2.CommandID)
		if ent1Final.UpdatedMs != tHold1Done.UnixMilli() || ent2Final.UpdatedMs != tHold2Done.UnixMilli() {
			t.Fatalf("historical UpdatedMs mutated: hold1=%d (want %d), hold2=%d (want %d)",
				ent1Final.UpdatedMs, tHold1Done.UnixMilli(), ent2Final.UpdatedMs, tHold2Done.UnixMilli())
		}
	}

	t.Run("fakeCommandStore", func(t *testing.T) {
		runWithStore(t, newFakeCommandStore())
	})

	t.Run("LifecycleStore_ENTITY_STATES_KV", func(t *testing.T) {
		srv, err := natsserver.NewServer(&natsserver.Options{
			Host:      "127.0.0.1",
			Port:      -1,
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

		if _, err := nc.CreateKeyValueBucket(context.Background(), jetstream.KeyValueConfig{
			Bucket: "ENTITY_STATES",
		}); err != nil {
			t.Fatalf("CreateKeyValueBucket ENTITY_STATES: %v", err)
		}
		js, err := nc.JetStream()
		if err != nil {
			t.Fatalf("JetStream: %v", err)
		}
		if _, err := js.CreateStream(context.Background(), jetstream.StreamConfig{
			Name:     "ENTITY",
			Subjects: []string{"entity.>"},
		}); err != nil {
			t.Fatalf("CreateStream ENTITY: %v", err)
		}

		payloadReg := payloadregistry.New()
		if err := lifecycle.RegisterPayloads(payloadReg); err != nil {
			t.Fatalf("RegisterPayloads: %v", err)
		}
		giDisc, err := graphingest.CreateGraphIngest(nil, component.Dependencies{
			NATSClient:      nc,
			PayloadRegistry: payloadReg,
			Platform:        types.PlatformMeta{Org: "boxbotix", Platform: "bbops"},
		})
		if err != nil {
			t.Fatalf("CreateGraphIngest: %v", err)
		}
		giComp := giDisc.(*graphingest.Component)
		if err := giComp.Initialize(); err != nil {
			t.Fatalf("giComp.Initialize: %v", err)
		}
		giCtx, giCancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			giCancel()
			_ = giComp.Stop(context.Background())
		})
		if err := giComp.Start(giCtx); err != nil {
			t.Fatalf("giComp.Start: %v", err)
		}

		lcStore, err := NewLifecycleStore(lifecycle.NewManager(nc, slog.Default()))
		if err != nil {
			t.Fatalf("NewLifecycleStore: %v", err)
		}
		runWithStore(t, lcStore)
	})
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceDisarmRearmInvalidatesPriorEpochEngage(t *testing.T) {
	for _, clearedLatchFirst := range []bool{true, false} {
		name := "latch_still_active_across_disarm_rearm"
		if clearedLatchFirst {
			name = "post_clear_disarm_rearm_rejects_old_epoch"
		}
		t.Run(name, func(t *testing.T) {
			tracker, err := mavlink.NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
			if err != nil {
				t.Fatalf("NewTrackerWithTimeouts: %v", err)
			}
			sender := &fakeFrameSender{}
			store := newFakeCommandStore()
			now := time.Date(2026, 11, 27, 19, 30, 0, 0, time.UTC)

			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   sender,
				Store:    store,
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}

			peer := netip.MustParseAddrPort("127.0.0.1:19955")
			// Epoch 1: admitted armed companion-teleop.
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 0, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

			// Epoch 2: P1 RC pilot override latches preemption.
			now = now.Add(20 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)

			// Epoch 3: P2 recovery returns armed companion-teleop.
			tRecovery := now.Add(20 * time.Millisecond)
			now = tRecovery
			outRecovery := tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now)
			deliverTrackerOutcome(svc, outRecovery, now)
			if len(outRecovery.Events) == 0 || outRecovery.Events[0].AuthorityEpoch != 3 {
				t.Fatalf("unexpected recovery events: %+v", outRecovery.Events)
			}
			recoveryEv := outRecovery.Events[0]

			if clearedLatchFirst {
				// Operator engages s1 during epoch=3 and cleanly stops s1.
				now = now.Add(10 * time.Millisecond)
				if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
					SysID:                1,
					Session:              "s1-epoch3",
					Seq:                  1,
					Vx:                   0.5,
					Engage:               true,
					ObservedMs:           recoveryEv.Timestamp,
					ObservationRev:       recoveryEv.ObservationRev,
					AuthorityIncarnation: recoveryEv.AuthorityIncarnation,
					AuthorityEpoch:       recoveryEv.AuthorityEpoch,
				}); err != nil {
					t.Fatalf("s1-epoch3 Engage: %v", err)
				}
				now = now.Add(10 * time.Millisecond)
				if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
					SysID:   1,
					Session: "s1-epoch3",
					Seq:     2,
					Stop:    true,
				}); err != nil {
					t.Fatalf("s1-epoch3 Stop: %v", err)
				}
			}

			// Delayed Engage built during epoch=3 (held in network transit while vehicle disarms & rearms).
			heldEpoch3Engage := TeleopRequest{
				SysID:                1,
				Session:              "s2-held-epoch3",
				Seq:                  1,
				Vx:                   1.4,
				Engage:               true,
				ObservedMs:           recoveryEv.Timestamp,
				ObservationRev:       recoveryEv.ObservationRev,
				AuthorityIncarnation: recoveryEv.AuthorityIncarnation,
				AuthorityEpoch:       recoveryEv.AuthorityEpoch,
			}

			// Epoch 4: Vehicle disarms.
			now = now.Add(50 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 3, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionAutonomous), peer, now), now)

			// Epoch 5: Vehicle rearms into a brand-new arming interval (armed companion-teleop).
			tRearm := now.Add(50 * time.Millisecond)
			now = tRearm
			outRearm := tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 4, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now)
			deliverTrackerOutcome(svc, outRearm, now)
			if len(outRearm.Events) == 0 || outRearm.Events[0].AuthorityEpoch != 5 {
				t.Fatalf("unexpected rearm events: %+v", outRearm.Events)
			}
			rearmEv := outRearm.Events[0]

			sentBeforeStale := countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot())

			// Delayed epoch=3 Engage arrives during epoch=5 -> MUST be rejected with 409 and 0 #84 frames!
			now = now.Add(10 * time.Millisecond)
			var ce *ControlError
			_, err = svc.SubmitTeleop(context.Background(), heldEpoch3Engage)
			if !errors.As(err, &ce) || ce.Status != http.StatusConflict {
				t.Fatalf("held epoch=3 Engage after disarm+rearm got err=%v, want 409 Conflict", err)
			}
			wantCode := "preemption_latched"
			if clearedLatchFirst {
				wantCode = "teleop_not_engaged"
			}
			if ce.Code != wantCode {
				t.Fatalf("held epoch=3 Engage error code = %q, want %q", ce.Code, wantCode)
			}

			// Steady-state SKG ObservedMs-only Engage from epoch=3 (without AuthorityEpoch) MUST ALSO be rejected!
			_, err = svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:      1,
				Session:    "s2b-held-epoch3-obs-only",
				Seq:        1,
				Vx:         1.1,
				Engage:     true,
				ObservedMs: recoveryEv.Timestamp,
			})
			if !errors.As(err, &ce) || ce.Status != http.StatusConflict {
				t.Fatalf("held epoch=3 ObservedMs-only Engage after disarm+rearm got err=%v, want 409 Conflict", err)
			}
			if svc.HasActiveLease(1) {
				t.Fatal("held epoch=3 Engage activated teleop lease in new arming interval")
			}
			if got := countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot()); got != sentBeforeStale {
				t.Fatalf("held epoch=3 Engage transmitted %d #84 frames, want 0", got-sentBeforeStale)
			}

			// Fresh epoch=5 Engage succeeds!
			if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:                1,
				Session:              "s3-fresh-epoch5",
				Seq:                  1,
				Vx:                   0.7,
				Engage:               true,
				ObservedMs:           rearmEv.Timestamp,
				ObservationRev:       rearmEv.ObservationRev,
				AuthorityIncarnation: rearmEv.AuthorityIncarnation,
				AuthorityEpoch:       rearmEv.AuthorityEpoch,
			}); err != nil {
				t.Fatalf("fresh epoch=5 Engage failed: %v", err)
			}
			if !svc.HasActiveLease(1) {
				t.Fatal("expected fresh epoch=5 Engage to activate lease")
			}
		})
	}

	t.Run("no_prior_preemption_disarm_rearm_rejects_old_epoch", func(t *testing.T) {
		targets := newFakeTargetSource()
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 19, 35, 0, 0, time.UTC)

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

		peer := netip.MustParseAddrPort("127.0.0.1:19957")
		target := mavlink.ActuationTarget{
			SysID:                   1,
			EntityID:                "boxbotix.bbops.robotics.uxs.vehicle.1",
			Domain:                  uxsvocab.DomainQuadcopter,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			AuthorityIncarnation:    "inc-steady",
			AuthorityEpoch:          1,
			CompanionMode:           uxsvocab.ModeTeleopVelocity,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			Armed:                   true,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: now.UnixNano(),
		}
		targets.set(target)

		heldEpoch1Engage := TeleopRequest{
			SysID:                1,
			Session:              "s-held-epoch1",
			Seq:                  1,
			Vx:                   1.2,
			Engage:               true,
			ObservedMs:           now.UnixMilli(),
			AuthorityIncarnation: "inc-steady",
			AuthorityEpoch:       1,
		}

		// Disarm (epoch=2) -> Rearm (epoch=3) with zero hardware preemption.
		now = now.Add(50 * time.Millisecond)
		target.Armed = false
		target.CompanionMode = uxsvocab.ModeDisarmed
		target.Authority = uxsvocab.AuthorityCompanionAutonomous
		target.AuthorityEpoch = 2
		target.HeartbeatObservedUnixNs = now.UnixNano()
		targets.set(target)
		svc.OnTransitionEvent(mavlink.TransitionEvent{
			Kind:                 mavlink.EventKindHeartbeatEdge,
			MsgID:                mavcodec.MsgIDHeartbeat,
			SysID:                1,
			EntityID:             target.EntityID,
			Authority:            target.Authority,
			AuthorityIncarnation: target.AuthorityIncarnation,
			AuthorityEpoch:       2,
			CompanionMode:        target.CompanionMode,
			LinkTier:             target.LinkTier,
			Armed:                false,
			Timestamp:            now.UnixMilli(),
		})

		now = now.Add(50 * time.Millisecond)
		target.Armed = true
		target.CompanionMode = uxsvocab.ModeTeleopVelocity
		target.Authority = uxsvocab.AuthorityCompanionTeleop
		target.AuthorityEpoch = 3
		target.HeartbeatObservedUnixNs = now.UnixNano()
		targets.set(target)
		svc.OnTransitionEvent(mavlink.TransitionEvent{
			Kind:                 mavlink.EventKindHeartbeatEdge,
			MsgID:                mavcodec.MsgIDHeartbeat,
			SysID:                1,
			EntityID:             target.EntityID,
			Authority:            target.Authority,
			AuthorityIncarnation: target.AuthorityIncarnation,
			AuthorityEpoch:       3,
			CompanionMode:        target.CompanionMode,
			LinkTier:             target.LinkTier,
			Armed:                true,
			Timestamp:            now.UnixMilli(),
		})

		var ce *ControlError
		if _, err := svc.SubmitTeleop(context.Background(), heldEpoch1Engage); !errors.As(err, &ce) || ce.Status != http.StatusConflict || ce.Code != "teleop_not_engaged" {
			t.Fatalf("held epoch=1 Engage after disarm+rearm got err=%v, want 409 teleop_not_engaged", err)
		}
		if svc.HasActiveLease(1) || countMessages[mavcodec.SetPositionTargetLocalNED](sender.snapshot()) != 0 {
			t.Fatal("held epoch=1 Engage must not activate lease or transmit #84")
		}
	})
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
// spec: uxs-control / Pin-on-admission MAVLink v2 UDP command egress and 1 Hz GCS liveness
func TestServiceBackwardWallClockStepDoesNotLockOutFreshEngage(t *testing.T) {
	tracker, err := mavlink.NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("NewTrackerWithTimeouts: %v", err)
	}
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	t0 := time.Date(2026, 11, 27, 19, 40, 0, 0, time.UTC)
	now := t0

	svc, err := NewService(Options{
		Org:                  "boxbotix",
		Platform:             "bbops",
		Targets:              tracker,
		Sender:               sender,
		Store:                store,
		Now:                  func() time.Time { return now },
		GCSHeartbeatInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	peer := netip.MustParseAddrPort("127.0.0.1:19956")
	monoNow := time.Now()
	outBirth := tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 0, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now, monoNow)
	deliverTrackerOutcome(svc, outBirth, now)
	birthTarget, ok := tracker.ActuationTarget(1)
	if !ok {
		t.Fatal("expected admitted ActuationTarget(1) after birth heartbeat")
	}
	birthObsMs := birthTarget.HeartbeatObservedUnixNs / int64(time.Millisecond)

	// 1) Engage s1 at t0 and Stop s1 at t0 + 20ms.
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:                1,
		Session:              "s1-before-jump",
		Seq:                  1,
		Vx:                   0.5,
		Engage:               true,
		ObservedMs:           birthObsMs,
		ObservationRev:       birthTarget.ObservationRev,
		AuthorityIncarnation: birthTarget.AuthorityIncarnation,
		AuthorityEpoch:       birthTarget.AuthorityEpoch,
	}); err != nil {
		t.Fatalf("s1 Engage at t0: %v", err)
	}
	now = t0.Add(20 * time.Millisecond)
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "s1-before-jump",
		Seq:     2,
		Stop:    true,
	}); err != nil {
		t.Fatalf("s1 Stop: %v", err)
	}

	// 2) Host wall clock steps BACKWARD by 60 seconds (tBack = t0 - 60s), and a fresh #0 HEARTBEAT arrives!
	tBack := t0.Add(-60 * time.Second)
	now = tBack
	monoNow = monoNow.Add(30 * time.Millisecond)
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now, monoNow), now)

	hbBeforeJumpEngage := countMessages[mavcodec.Heartbeat](sender.snapshot())

	// 3a) Steady-state UI Engage carrying ONLY ObservedMs = tBack.UnixMilli() (< t0.UnixMilli()) MUST succeed immediately!
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:      1,
		Session:    "s2-after-backward-jump-obs-ms-only",
		Seq:        1,
		Vx:         0.8,
		Engage:     true,
		ObservedMs: tBack.UnixMilli(),
	}); err != nil {
		t.Fatalf("fresh post-backward-jump Engage (ObservedMs-only) locked out: %v", err)
	}
	if !svc.HasActiveLease(1) {
		t.Fatal("expected s2 lease to be active after backward clock step")
	}
	if gotHB := countMessages[mavcodec.Heartbeat](sender.snapshot()); gotHB != hbBeforeJumpEngage+1 {
		t.Fatalf("1 Hz GCS #0 HEARTBEAT suppressed across backward clock jump: got %d, want %d", gotHB, hbBeforeJumpEngage+1)
	}

	// 3b) Even though birthObsMs (t0) is 60,000 ms LARGER than tBack, a delayed pre-jump Engage
	// carrying the older observation (birthTarget) MUST still be rejected as superseded!
	var ce *ControlError
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:                1,
		Session:              "s0-delayed-pre-jump",
		Seq:                  1,
		Vx:                   1.5,
		Engage:               true,
		ObservedMs:           birthObsMs,
		ObservationRev:       birthTarget.ObservationRev,
		AuthorityIncarnation: birthTarget.AuthorityIncarnation,
		AuthorityEpoch:       birthTarget.AuthorityEpoch,
	}); !errors.As(err, &ce) || ce.Code != "teleop_session_superseded" {
		t.Fatalf("delayed pre-jump Engage with numerically larger wall timestamp got err=%v, want teleop_session_superseded", err)
	}

	// 3c) P1 override + P2 recovery while clock is still behind t0 (tBack + 30ms < t0) also clears cleanly!
	now = tBack.Add(15 * time.Millisecond)
	monoNow = monoNow.Add(15 * time.Millisecond)
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 2, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now, monoNow), now)
	now = tBack.Add(30 * time.Millisecond)
	monoNow = monoNow.Add(15 * time.Millisecond)
	outRecovery := tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 3, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now, monoNow)
	deliverTrackerOutcome(svc, outRecovery, now)
	recEv := outRecovery.Events[0]

	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:                1,
		Session:              "s3-post-recovery-behind-t0",
		Seq:                  1,
		Vx:                   0.9,
		Engage:               true,
		ObservedMs:           recEv.Timestamp,
		ObservationRev:       recEv.ObservationRev,
		AuthorityIncarnation: recEv.AuthorityIncarnation,
		AuthorityEpoch:       recEv.AuthorityEpoch,
	}); err != nil {
		t.Fatalf("post-recovery Engage while wall clock is still behind t0 failed: %v", err)
	}
}

// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceCollidedTimestampAcrossAuthorityBoundaryRequiresExplicitRev(t *testing.T) {
	tracker, err := mavlink.NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	sender := &fakeFrameSender{}
	store := newFakeCommandStore()
	t0 := time.Date(2026, 11, 27, 20, 0, 0, 0, time.UTC)
	now := t0

	svc, err := NewService(Options{
		Org:      "boxbotix",
		Platform: "bbops",
		Targets:  tracker,
		Sender:   sender,
		Store:    store,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	peer := netip.MustParseAddrPort("127.0.0.1:19961")
	monoNow := time.Now()
	// 1) Pre-override P2 observation at t0 (operator clicks Engage carrying ObservedMs=t0, ObservationRev=0).
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 0, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now, monoNow), now)

	// 2) P1 RC override latches preemption at t0 + 20ms.
	now = t0.Add(20 * time.Millisecond)
	monoNow = monoNow.Add(20 * time.Millisecond)
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now, monoNow), now)

	// 3) Host wall clock steps backward by 20ms so P2 recovery lands on the EXACT same t0 millisecond!
	now = t0
	monoNow = monoNow.Add(20 * time.Millisecond)
	outRecovery := tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 2, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now, monoNow)
	deliverTrackerOutcome(svc, outRecovery, now)
	recTarget, ok := tracker.ActuationTarget(1)
	if !ok {
		t.Fatal("expected ActuationTarget(1) after recovery")
	}

	// 4) Delayed pre-override timestamp-only (ObservationRev=0) Engage at collided t0 MUST be rejected!
	var ce *ControlError
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:      1,
		Session:    "s-pre-override-collided-t0",
		Seq:        1,
		Vx:         1.0,
		Engage:     true,
		ObservedMs: t0.UnixMilli(),
	}); !errors.As(err, &ce) || ce.Code != "preemption_latched" {
		t.Fatalf("collided pre-override ObservedMs-only Engage got err=%v, want 409 preemption_latched", err)
	}
	if svc.HasActiveLease(1) {
		t.Fatal("expected no active lease after rejected collided timestamp-only Engage")
	}

	// 5) Post-recovery Engage carrying explicit current ObservationRev at t0 succeeds!
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:                1,
		Session:              "s-post-recovery-explicit-rev",
		Seq:                  1,
		Vx:                   0.6,
		Engage:               true,
		ObservedMs:           t0.UnixMilli(),
		ObservationRev:       recTarget.ObservationRev,
		AuthorityIncarnation: recTarget.AuthorityIncarnation,
		AuthorityEpoch:       recTarget.AuthorityEpoch,
	}); err != nil {
		t.Fatalf("post-recovery Engage with explicit ObservationRev failed: %v", err)
	}
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:   1,
		Session: "s-post-recovery-explicit-rev",
		Seq:     2,
		Stop:    true,
	}); err != nil {
		t.Fatalf("Stop post-recovery session: %v", err)
	}

	// 6) Unique backward-clock timestamp (t0 - 1500ms) with ObservationRev=0 still succeeds in steady state!
	tUniqueBack := t0.Add(-1500 * time.Millisecond)
	now = tUniqueBack
	monoNow = monoNow.Add(20 * time.Millisecond)
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 3, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now, monoNow), now)
	if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
		SysID:      1,
		Session:    "s-unique-backward-ts",
		Seq:        1,
		Vx:         0.5,
		Engage:     true,
		ObservedMs: tUniqueBack.UnixMilli(),
	}); err != nil {
		t.Fatalf("unique backward-clock timestamp-only Engage failed: %v", err)
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceWrappedAckGuardProtectsDisarmAndSITLVariants(t *testing.T) {
	advanceTelemetry := func(t *testing.T, tracker *mavlink.Tracker, peer netip.AddrPort, startSeq uint8, count int, now *time.Time) uint8 {
		t.Helper()
		seq := startSeq
		for i := 0; i < count; i++ {
			seq++
			*now = now.Add(2 * time.Millisecond)
			out := tracker.IngestFrame(mavcodec.Frame{
				Sequence:    seq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, *now)
			if !out.Accepted {
				t.Fatalf("telemetry frame %d was not accepted", i)
			}
		}
		return seq
	}

	t.Run("wrapped_arm_accepted_with_armed_mode_disarmed_hb_does_not_confirm_disarm", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 20, 10, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19962")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 49, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(5 * time.Millisecond)
		armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
		if err != nil {
			t.Fatalf("SubmitCommand Arm: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    50,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)
		if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("Arm phase = %q, want confirmed", ent.PhaseState)
		}

		// Vehicle is ARMED, advance 253 telemetry frames (to seq=47), then emit a fresh ARMED heartbeat at seq=48 whose companion_mode happens to be ModeDisarmed.
		curSeq := advanceTelemetry(t, tracker, peer, 50, 253, &now)
		curSeq++ // seq=48 after wrap
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, curSeq, true, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(5 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}
		// Fresh post-dispatch heartbeat at seq=49 STILL reports Armed=true (even with ModeDisarmed).
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 49, true, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		// Wrapped duplicated Arm ACCEPTED at seq=50 arrives while vehicle is still Armed=true!
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    50,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)

		entMid, _ := store.get(disarmRes.CommandID)
		if entMid.PhaseState != PhaseCommitUnknown || entMid.AckResult != UnacknowledgedResult {
			t.Fatalf("Disarm falsely confirmed while Armed=true: got %+v, want PhaseCommitUnknown AckResult=-1", entMid)
		}

		// Once vehicle actually disarms (Armed=false), fresh heartbeat reconciles Disarm to PhaseConfirmed.
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 51, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if entFinal, _ := store.get(disarmRes.CommandID); entFinal.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm after Armed=false heartbeat = %+v, want PhaseConfirmed", entFinal)
		}
	})

	t.Run("wrapped_arm_denied_does_not_reject_disarm", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 20, 15, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19963")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 59, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(5 * time.Millisecond)
		armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
		if err != nil {
			t.Fatalf("SubmitCommand Arm: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    60,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)
		if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseRejected {
			t.Fatalf("Arm phase = %q, want rejected", ent.PhaseState)
		}

		// Vehicle later arms and 254 telemetry frames + 1 armed heartbeat advance sequence to 59.
		curSeq := advanceTelemetry(t, tracker, peer, 60, 254, &now)
		curSeq++ // seq=59
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, curSeq, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(5 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}

		// Wrapped duplicated Arm DENIED at seq=60 arrives during Disarm!
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    60,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)

		entMid, _ := store.get(disarmRes.CommandID)
		wantMask := 1 << int(mavcodec.MAVResultDenied)
		if entMid.PhaseState != PhaseCommitUnknown || entMid.AckResult != UnacknowledgedResult || entMid.AckAccounted != 1 || (entMid.AckPoolMask&wantMask) == 0 {
			t.Fatalf("Disarm falsely rejected by duplicated Arm DENIED: got %+v, want PhaseCommitUnknown AckResult=-1 AckAccounted=1", entMid)
		}

		// Disarmed heartbeat reconciles Disarm to PhaseConfirmed.
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 61, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if entFinal, _ := store.get(disarmRes.CommandID); entFinal.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm after disarmed heartbeat = %+v, want PhaseConfirmed", entFinal)
		}
	})

	t.Run("wrapped_sitl_cross_current_accepted_does_not_confirm_sitl_blackout", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 20, 20, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19964")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 69, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(5 * time.Millisecond)
		ccRes, err := svc.SubmitCommand(context.Background(), CommandRequest{
			SysID:  1,
			Action: ActionSITLCrossCurrent,
			Param1: 0.4,
			Param2: 0.8,
		})
		if err != nil {
			t.Fatalf("SubmitCommand sitl-cross-current: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    70,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdUser1,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)
		if ent, _ := store.get(ccRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("sitl-cross-current phase = %q, want confirmed", ent.PhaseState)
		}

		_ = advanceTelemetry(t, tracker, peer, 70, 254, &now)

		now = now.Add(5 * time.Millisecond)
		boRes, err := svc.SubmitCommand(context.Background(), CommandRequest{
			SysID:  1,
			Action: ActionSITLCompanionBlackout,
			Param1: 350,
		})
		if err != nil {
			t.Fatalf("SubmitCommand sitl-companion-blackout: %v", err)
		}
		// Fresh heartbeat at seq=69 arrives (cannot corroborate sitl-companion-blackout).
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 69, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		// Wrapped duplicated sitl-cross-current ACCEPTED at seq=70 arrives!
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    70,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdUser1,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)

		entBO, _ := store.get(boRes.CommandID)
		wantMask := 1 << int(mavcodec.MAVResultAccepted)
		if entBO.PhaseState != PhaseCommitUnknown || entBO.AckResult != UnacknowledgedResult || entBO.AckAccounted != 1 || (entBO.AckPoolMask&wantMask) == 0 {
			t.Fatalf("sitl-companion-blackout falsely confirmed by duplicated sitl-cross-current ACK: got %+v, want PhaseCommitUnknown AckResult=-1 AckAccounted=1", entBO)
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceSequenceWrapWith64DroppedTelemetryFramesAndReorderGuard(t *testing.T) {
	dropCases := []struct {
		name            string
		dropStartOffset int
	}{
		{name: "drop_64_at_start", dropStartOffset: 0},
		{name: "drop_64_in_middle", dropStartOffset: 57},
		{name: "drop_64_near_end", dropStartOffset: 189},
		{name: "drop_64_trailing_boundary_before_ack", dropStartOffset: 191},
	}

	for _, tc := range dropCases {
		t.Run(tc.name, func(t *testing.T) {
			tracker, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker: %v", err)
			}
			store := newFakeCommandStore()
			now := time.Date(2026, 11, 27, 20, 25, 0, 0, time.UTC)
			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   &fakeFrameSender{},
				Store:    store,
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			peer := netip.MustParseAddrPort("127.0.0.1:19965")
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 41, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

			now = now.Add(5 * time.Millisecond)
			hold1, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
			if err != nil {
				t.Fatalf("SubmitCommand hold1: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
				Sequence:    42,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdNavLoiterUnlim,
					Result:  mavcodec.MAVResultDenied,
				},
			}, peer, now), now)
			if ent, _ := store.get(hold1.CommandID); ent.PhaseState != PhaseRejected {
				t.Fatalf("hold1 = %+v, want PhaseRejected", ent)
			}

			// Vehicle emits 255 intervening telemetry frames (seq 43..41), dropping 64 consecutive frames.
			curSeq := uint8(42)
			for i := 0; i < 255; i++ {
				curSeq++
				if i >= tc.dropStartOffset && i < tc.dropStartOffset+64 {
					continue
				}
				now = now.Add(2 * time.Millisecond)
				out := tracker.IngestFrame(mavcodec.Frame{
					Sequence:    curSeq,
					SystemID:    1,
					ComponentID: mavcodec.CompIDAutopilot1,
					Message:     mavcodec.AttitudeQuaternion{Q1: 1},
				}, peer, now)
				if !out.Accepted {
					t.Fatalf("telemetry frame %d was not accepted", i)
				}
			}

			now = now.Add(5 * time.Millisecond)
			hold2, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
			if err != nil {
				t.Fatalf("SubmitCommand hold2: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			out2 := tracker.IngestFrame(mavcodec.Frame{
				Sequence:    42,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdNavLoiterUnlim,
					Result:  mavcodec.MAVResultDenied,
				},
			}, peer, now)
			deliverTrackerOutcome(svc, out2, now)

			if out2.CommandAck == nil {
				t.Fatalf("%s: COMMAND_ACK at seq=42 was dropped after 255 vehicle frames (64 dropped)", tc.name)
			}
			ent2, _ := store.get(hold2.CommandID)
			wantMask := 1 << int(mavcodec.MAVResultDenied)
			if ent2.PhaseState != PhaseCommitUnknown || ent2.AckResult != UnacknowledgedResult || ent2.AckAccounted != 1 || (ent2.AckPoolMask&wantMask) == 0 {
				t.Fatalf("%s: hold2 repeated (seq=42, DENIED) = %+v, want PhaseCommitUnknown AckResult=-1 AckAccounted=1 AckPoolMask|=%d", tc.name, ent2, wantMask)
			}
		})
	}

	t.Run("two_packet_backward_reorder_does_not_false_wrap", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 20, 30, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19966")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 39, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(5 * time.Millisecond)
		hold1, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		if err != nil {
			t.Fatalf("SubmitCommand hold1: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    42,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdNavLoiterUnlim,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)
		if ent, _ := store.get(hold1.CommandID); ent.PhaseState != PhaseRejected {
			t.Fatalf("hold1 = %+v, want PhaseRejected", ent)
		}

		// Receive 43, 44, then delayed out-of-order 40, 41 (small backward reorder, NOT a 256-frame wrap!).
		for _, seq := range []uint8{43, 44, 40, 41} {
			now = now.Add(2 * time.Millisecond)
			tracker.IngestFrame(mavcodec.Frame{
				Sequence:    seq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, now)
		}

		now = now.Add(5 * time.Millisecond)
		hold2, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		if err != nil {
			t.Fatalf("SubmitCommand hold2: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		outDup := tracker.IngestFrame(mavcodec.Frame{
			Sequence:    42,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdNavLoiterUnlim,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now)
		deliverTrackerOutcome(svc, outDup, now)
		ent2, _ := store.get(hold2.CommandID)
		if outDup.CommandAck != nil || ent2.PhaseState != PhasePending {
			t.Fatalf("expected 2-packet backward reorder NOT to false-wrap seq=42: got outDup.CommandAck=%v phase=%s", outDup.CommandAck, ent2.PhaseState)
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceRepeatedIdenticalDisarmAcrossWrapPreservesAmbiguityAndResolves(t *testing.T) {
	advance255Telemetry := func(t *testing.T, tracker *mavlink.Tracker, peer netip.AddrPort, startSeq uint8, now *time.Time) uint8 {
		t.Helper()
		seq := startSeq
		for i := 0; i < 255; i++ {
			seq++
			*now = now.Add(2 * time.Millisecond)
			out := tracker.IngestFrame(mavcodec.Frame{
				Sequence:    seq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, *now)
			if !out.Accepted {
				t.Fatalf("telemetry frame %d was not accepted", i)
			}
		}
		return seq
	}

	for _, hbFirst := range []bool{false, true} {
		name := "delayed_disarm1_denial_then_own_accepted_then_disarmed_hb"
		if hbFirst {
			name = "delayed_disarm1_denial_then_disarmed_hb_then_own_accepted"
		}
		t.Run(name, func(t *testing.T) {
			tracker, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker: %v", err)
			}
			store := newFakeCommandStore()
			now := time.Date(2026, 11, 27, 21, 10, 0, 0, time.UTC)
			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   &fakeFrameSender{},
				Store:    store,
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			peer := netip.MustParseAddrPort("127.0.0.1:19971")
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 41, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

			// 1) Disarm #1 denied at seq=42.
			now = now.Add(5 * time.Millisecond)
			disarm1, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand disarm1: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
				Sequence:    42,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdComponentArmDisarm,
					Result:  mavcodec.MAVResultDenied,
				},
			}, peer, now), now)
			if ent1, _ := store.get(disarm1.CommandID); ent1.PhaseState != PhaseRejected {
				t.Fatalf("disarm1 = %+v, want PhaseRejected", ent1)
			}

			// 2) Advance 255 telemetry frames to seq=41; submit identical Disarm #2 (same action & param bits).
			_ = advance255Telemetry(t, tracker, peer, 42, &now)
			now = now.Add(5 * time.Millisecond)
			disarm2, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand disarm2: %v", err)
			}

			// 3) Delayed duplicate copy of Disarm #1's DENIED at seq=42 arrives after wrap!
			// Must be held in PhaseCommitUnknown (AckResult=-1, AckAccounted=1), NEVER locked into PhaseRejected!
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
				Sequence:    42,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdComponentArmDisarm,
					Result:  mavcodec.MAVResultDenied,
				},
			}, peer, now), now)
			entMid, _ := store.get(disarm2.CommandID)
			wantDeniedMask := 1 << int(mavcodec.MAVResultDenied)
			if entMid.PhaseState != PhaseCommitUnknown || entMid.AckResult != UnacknowledgedResult || entMid.AckAccounted != 1 || (entMid.AckPoolMask&wantDeniedMask) == 0 {
				t.Fatalf("disarm2 after ambiguous wrapped DENIED = %+v, want PhaseCommitUnknown AckResult=-1 AckAccounted=1", entMid)
			}

			// 4) Disarm #2's own ACCEPTED reply (seq=43) + disarmed #0 HEARTBEAT (seq=44) arrive -> settles PhaseConfirmed!
			if hbFirst {
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 44, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
				if entHB, _ := store.get(disarm2.CommandID); entHB.PhaseState != PhaseConfirmed {
					t.Fatalf("disarm2 after disarmed heartbeat = %+v, want PhaseConfirmed", entHB)
				}
				// Re-arm and submit Disarm #3 BEFORE Disarm #2's delayed ACCEPTED (seq=43) arrives:
				// wrapAckCredit ensures Disarm #2's late ACCEPTED CANNOT falsely confirm Disarm #3 while armed,
				// holding Disarm #3 in PhaseCommitUnknown until its own disarmed #0 HEARTBEAT arrives!
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 45, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
				now = now.Add(5 * time.Millisecond)
				disarm3, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
				if err != nil {
					t.Fatalf("SubmitCommand disarm3: %v", err)
				}
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
					Sequence:    43,
					SystemID:    1,
					ComponentID: mavcodec.CompIDAutopilot1,
					Message: mavcodec.CommandAck{
						Command: mavcodec.MAVCmdComponentArmDisarm,
						Result:  mavcodec.MAVResultAccepted,
					},
				}, peer, now), now)
				if ent3, _ := store.get(disarm3.CommandID); ent3.PhaseState != PhaseCommitUnknown || ent3.AckResult != UnacknowledgedResult {
					t.Fatalf("disarm3 after late Disarm #2 ACCEPTED while armed = %+v, want PhaseCommitUnknown AckResult=-1", ent3)
				}
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
					Sequence:    46,
					SystemID:    1,
					ComponentID: mavcodec.CompIDAutopilot1,
					Message: mavcodec.CommandAck{
						Command: mavcodec.MAVCmdComponentArmDisarm,
						Result:  mavcodec.MAVResultAccepted,
					},
				}, peer, now), now)
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 47, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
				if ent3Final, _ := store.get(disarm3.CommandID); ent3Final.PhaseState != PhaseConfirmed {
					t.Fatalf("disarm3 after own ACCEPTED + disarmed HB = %+v, want PhaseConfirmed", ent3Final)
				}
			} else {
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
					Sequence:    43,
					SystemID:    1,
					ComponentID: mavcodec.CompIDAutopilot1,
					Message: mavcodec.CommandAck{
						Command: mavcodec.MAVCmdComponentArmDisarm,
						Result:  mavcodec.MAVResultAccepted,
					},
				}, peer, now), now)
				entAfterAck, _ := store.get(disarm2.CommandID)
				if entAfterAck.PhaseState != PhaseConfirmed || entAfterAck.AckResult != int(mavcodec.MAVResultAccepted) {
					t.Fatalf("disarm2 after own ACCEPTED at seq=43 = %+v, want PhaseConfirmed AckResult=ACCEPTED", entAfterAck)
				}
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 44, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
				entFinal, _ := store.get(disarm2.CommandID)
				if entFinal.PhaseState != PhaseConfirmed || entFinal.AckResult != int(mavcodec.MAVResultAccepted) {
					t.Fatalf("disarm2 after disarmed heartbeat = %+v, want PhaseConfirmed AckResult=ACCEPTED", entFinal)
				}
			}
		})
	}

	t.Run("genuine_repeated_same_seq_denial_stays_commit_unknown_without_false_terminal", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 21, 15, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19972")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 41, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(5 * time.Millisecond)
		disarm1, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand disarm1: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    42,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)
		if ent1, _ := store.get(disarm1.CommandID); ent1.PhaseState != PhaseRejected {
			t.Fatalf("disarm1 = %+v, want PhaseRejected", ent1)
		}

		_ = advance255Telemetry(t, tracker, peer, 42, &now)
		now = now.Add(5 * time.Millisecond)
		disarm2, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand disarm2: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    42,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)

		// Unchanged armed heartbeat + timeout pass without distinguishing evidence -> remains PhaseCommitUnknown (AckResult=-1).
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 43, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(2 * time.Second)
		svc.Advance(context.Background(), now)

		ent2, _ := store.get(disarm2.CommandID)
		wantDeniedMask := 1 << int(mavcodec.MAVResultDenied)
		if ent2.PhaseState != PhaseCommitUnknown || ent2.AckResult != UnacknowledgedResult || ent2.AckAccounted != 1 || (ent2.AckPoolMask&wantDeniedMask) == 0 {
			t.Fatalf("genuine repeated same-(seq,DENIED) disarm2 = %+v, want PhaseCommitUnknown AckResult=-1 AckAccounted=1", ent2)
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceStaleOrDuplicateHeartbeatCannotConfirmDisarmWithWrappedArmAck(t *testing.T) {
	tracker, err := mavlink.NewTracker("boxbotix", "bbops")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	store := newFakeCommandStore()
	t0 := time.Date(2026, 11, 27, 21, 20, 0, 0, time.UTC)
	now := t0
	monoNow := time.Now()

	svc, err := NewService(Options{
		Org:      "boxbotix",
		Platform: "bbops",
		Targets:  tracker,
		Sender:   &fakeFrameSender{},
		Store:    store,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	peer := netip.MustParseAddrPort("127.0.0.1:19973")
	// 1) Disarmed birth at seq=58, then Arm confirmed at seq=60.
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 58, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now, monoNow), now)
	now = now.Add(5 * time.Millisecond)
	monoNow = monoNow.Add(5 * time.Millisecond)
	armRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm})
	if err != nil {
		t.Fatalf("SubmitCommand Arm: %v", err)
	}
	now = now.Add(5 * time.Millisecond)
	monoNow = monoNow.Add(5 * time.Millisecond)
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(mavcodec.Frame{
		Sequence:    60,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message: mavcodec.CommandAck{
			Command: mavcodec.MAVCmdComponentArmDisarm,
			Result:  mavcodec.MAVResultAccepted,
		},
	}, peer, now, monoNow), now)
	if ent, _ := store.get(armRes.CommandID); ent.PhaseState != PhaseConfirmed {
		t.Fatalf("Arm = %+v, want PhaseConfirmed", ent)
	}

	// 2) Advance 254 telemetry frames to seq=58, then armed heartbeat at seq=59.
	seq := uint8(60)
	for i := 0; i < 254; i++ {
		seq++
		now = now.Add(2 * time.Millisecond)
		monoNow = monoNow.Add(2 * time.Millisecond)
		_ = tracker.IngestFrameWithClock(mavcodec.Frame{
			Sequence:    seq,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message:     mavcodec.AttitudeQuaternion{Q1: 1},
		}, peer, now, monoNow)
	}
	seq++ // seq=59
	now = now.Add(5 * time.Millisecond)
	monoNow = monoNow.Add(5 * time.Millisecond)
	armedMono := monoNow
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, seq, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now, armedMono), now)

	// 3) Submit Disarm while vehicle is ARMED.
	now = now.Add(5 * time.Millisecond)
	monoNow = monoNow.Add(5 * time.Millisecond)
	disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
	if err != nil {
		t.Fatalf("SubmitCommand Disarm: %v", err)
	}

	// 4a) Older-monotonic disarmed #0 HEARTBEAT (seq=61, mono < armedMono) arrives!
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 61, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now.Add(-20*time.Millisecond), armedMono.Add(-10*time.Millisecond)), now)
	// 4b) Duplicate seq=59 and older out-of-order seq=58 disarmed #0 HEARTBEATs arrive at newer monotonic timestamps!
	now = now.Add(5 * time.Millisecond)
	monoNow = monoNow.Add(5 * time.Millisecond)
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 59, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now, monoNow), now)
	now = now.Add(5 * time.Millisecond)
	monoNow = monoNow.Add(5 * time.Millisecond)
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 58, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now, monoNow), now)

	// 5) Wrapped duplicated Arm ACCEPTED at seq=60 arrives -> Disarm MUST stay PhaseCommitUnknown (NOT PhaseConfirmed)!
	now = now.Add(5 * time.Millisecond)
	monoNow = monoNow.Add(5 * time.Millisecond)
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(mavcodec.Frame{
		Sequence:    60,
		SystemID:    1,
		ComponentID: mavcodec.CompIDAutopilot1,
		Message: mavcodec.CommandAck{
			Command: mavcodec.MAVCmdComponentArmDisarm,
			Result:  mavcodec.MAVResultAccepted,
		},
	}, peer, now, monoNow), now)

	entMid, _ := store.get(disarmRes.CommandID)
	if entMid.PhaseState != PhaseCommitUnknown || entMid.AckResult != UnacknowledgedResult {
		t.Fatalf("Disarm falsely confirmed via stale/duplicate disarmed heartbeat + wrapped Arm ACK: got %+v, want PhaseCommitUnknown AckResult=-1", entMid)
	}

	// 6) Genuine fresh post-dispatch disarmed #0 HEARTBEAT (seq=61, newer monoNow) reconciles Disarm to PhaseConfirmed.
	now = now.Add(10 * time.Millisecond)
	monoNow = monoNow.Add(10 * time.Millisecond)
	deliverTrackerOutcome(svc, tracker.IngestFrameWithClock(makeTestVehicleHeartbeatFrame(t, 1, 61, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now, monoNow), now)
	if entFinal, _ := store.get(disarmRes.CommandID); entFinal.PhaseState != PhaseConfirmed {
		t.Fatalf("Disarm after genuine fresh disarmed heartbeat = %+v, want PhaseConfirmed", entFinal)
	}
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceSITLCRSFOverrideZeroToggleReconcilesBothInitialAuthorities(t *testing.T) {
	advance254 := func(t *testing.T, tracker *mavlink.Tracker, peer netip.AddrPort, startSeq uint8, now *time.Time) uint8 {
		t.Helper()
		seq := startSeq
		for i := 0; i < 254; i++ {
			seq++
			*now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{
				Sequence:    seq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, *now)
		}
		return seq
	}

	toggleCases := []struct {
		name             string
		initialAuthority mavcodec.ControlAuthority
		toggledAuthority mavcodec.ControlAuthority
	}{
		{
			name:             "from_companion_teleop_to_direct_crsf_override",
			initialAuthority: mavcodec.AuthorityCompanionTeleop,
			toggledAuthority: mavcodec.AuthorityDirectCRSFOverride,
		},
		{
			name:             "from_direct_crsf_override_to_companion_teleop",
			initialAuthority: mavcodec.AuthorityDirectCRSFOverride,
			toggledAuthority: mavcodec.AuthorityCompanionTeleop,
		},
	}

	for _, tc := range toggleCases {
		t.Run(tc.name, func(t *testing.T) {
			tracker, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker: %v", err)
			}
			store := newFakeCommandStore()
			now := time.Date(2026, 11, 27, 21, 25, 0, 0, time.UTC)
			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   &fakeFrameSender{},
				Store:    store,
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			peer := netip.MustParseAddrPort("127.0.0.1:19974")
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 9, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

			// Seed a prior MAV_CMD_USER_1 ACCEPTED at seq=10 so seq=10 after wrap exercises wrapped-ACK corroboration too!
			now = now.Add(5 * time.Millisecond)
			ccRes, err := svc.SubmitCommand(context.Background(), CommandRequest{
				SysID:  1,
				Action: ActionSITLCrossCurrent,
				Param1: 0.5,
				Param2: 0.5,
			})
			if err != nil {
				t.Fatalf("SubmitCommand sitl-cross-current: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
				Sequence:    10,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdUser1,
					Result:  mavcodec.MAVResultAccepted,
				},
			}, peer, now), now)
			if ent, _ := store.get(ccRes.CommandID); ent.PhaseState != PhaseConfirmed {
				t.Fatalf("seed sitl-cross-current = %+v, want PhaseConfirmed", ent)
			}

			curSeq := advance254(t, tracker, peer, 10, &now) // seq=8
			curSeq++                                         // seq=9
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, curSeq, true, mavcodec.ModeHoldPosition, tc.initialAuthority), peer, now), now)

			// Submit ActionSITLCRSFOverride with Param1=0 (toggle semantics in e2eharness).
			now = now.Add(5 * time.Millisecond)
			togRes, err := svc.SubmitCommand(context.Background(), CommandRequest{
				SysID:  1,
				Action: ActionSITLCRSFOverride,
				Param1: 0,
			})
			if err != nil {
				t.Fatalf("SubmitCommand sitl-crsf-override toggle: %v", err)
			}

			// 1) Fresh heartbeat STILL reporting unchanged initialAuthority + wrapped MAV_CMD_USER_1 ACCEPTED at seq=10:
			// toggle has NOT taken effect, so command MUST remain PhaseCommitUnknown (NOT PhaseConfirmed)!
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 11, true, mavcodec.ModeHoldPosition, tc.initialAuthority), peer, now), now)
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
				Sequence:    10,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdUser1,
					Result:  mavcodec.MAVResultAccepted,
				},
			}, peer, now), now)
			entUnchanged, _ := store.get(togRes.CommandID)
			if entUnchanged.PhaseState != PhaseCommitUnknown {
				t.Fatalf("%s: unchanged authority falsely confirmed toggle: got %+v, want PhaseCommitUnknown", tc.name, entUnchanged)
			}

			// 2) Fresh heartbeat reporting toggledAuthority reconciles toggle to PhaseConfirmed!
			now = now.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 12, true, mavcodec.ModeHoldPosition, tc.toggledAuthority), peer, now), now)
			entToggled, _ := store.get(togRes.CommandID)
			if entToggled.PhaseState != PhaseConfirmed {
				t.Fatalf("%s: toggled authority did not confirm toggle: got %+v, want PhaseConfirmed", tc.name, entToggled)
			}
		})
	}

	t.Run("recovered_toggle_without_dispatch_authority_stays_uncertain", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		sharedStore := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 21, 30, 0, 0, time.UTC)
		peer := netip.MustParseAddrPort("127.0.0.1:19975")
		_ = tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 19, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now)

		svc1, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    sharedStore,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService 1: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		pendingToggle, err := svc1.SubmitCommand(context.Background(), CommandRequest{
			SysID:  1,
			Action: ActionSITLCRSFOverride,
			Param1: 0,
		})
		if err != nil {
			t.Fatalf("svc1 SubmitCommand sitl-crsf-override toggle: %v", err)
		}

		// Restart onto svc2: recovered toggle has no persisted dispatch authority, so it stays uncertain!
		svc2, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    sharedStore,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService 2: %v", err)
		}
		svc2.Advance(context.Background(), now)

		for i, auth := range []mavcodec.ControlAuthority{mavcodec.AuthorityCompanionTeleop, mavcodec.AuthorityDirectCRSFOverride} {
			now = now.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svc2, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, uint8(20+i), true, mavcodec.ModeHoldPosition, auth), peer, now), now)
			svc2.Advance(context.Background(), now)
			if ent, _ := sharedStore.get(pendingToggle.CommandID); ent.PhaseState != PhaseCommitUnknown {
				t.Fatalf("recovered toggle falsely confirmed without dispatch authority on auth=%d: %+v", auth, ent)
			}
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceWrapAckCreditSurvivesRestartAndLinkBlip(t *testing.T) {
	advance255 := func(t *testing.T, tracker *mavlink.Tracker, peer netip.AddrPort, startSeq uint8, now *time.Time) uint8 {
		t.Helper()
		seq := startSeq
		for i := 0; i < 255; i++ {
			seq++
			*now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{
				Sequence:    seq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, *now)
		}
		return seq
	}

	for _, freshTrackerOnRestart := range []bool{false, true} {
		name := "reused_tracker_across_service_restart"
		if freshTrackerOnRestart {
			name = "fresh_tracker_across_service_restart"
		}
		t.Run(name, func(t *testing.T) {
			tracker1, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker 1: %v", err)
			}
			sharedStore := newFakeCommandStore()
			now := time.Date(2026, 11, 27, 22, 10, 0, 0, time.UTC)
			svc1, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker1,
				Sender:   &fakeFrameSender{},
				Store:    sharedStore,
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService 1: %v", err)
			}
			peer := netip.MustParseAddrPort("127.0.0.1:19981")
			deliverTrackerOutcome(svc1, tracker1.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 41, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

			// 1) Disarm #1 rejected with DENIED at seq=42.
			now = now.Add(5 * time.Millisecond)
			disarm1, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand disarm1: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc1, tracker1.IngestFrame(mavcodec.Frame{
				Sequence:    42,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdComponentArmDisarm,
					Result:  mavcodec.MAVResultDenied,
				},
			}, peer, now), now)
			if ent1, _ := sharedStore.get(disarm1.CommandID); ent1.PhaseState != PhaseRejected {
				t.Fatalf("disarm1 = %+v, want PhaseRejected", ent1)
			}

			// 2) Wrap 255 telemetry frames to seq=41; submit Disarm #2; absorb delayed duplicate of Disarm #1 DENIED at seq=42.
			_ = advance255(t, tracker1, peer, 42, &now)
			now = now.Add(5 * time.Millisecond)
			disarm2, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand disarm2: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc1, tracker1.IngestFrame(mavcodec.Frame{
				Sequence:    42,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdComponentArmDisarm,
					Result:  mavcodec.MAVResultDenied,
				},
			}, peer, now), now)

			// 3) Reconcile Disarm #2 to PhaseConfirmed via disarmed #0 HEARTBEAT at seq=44 BEFORE Disarm #2's own ACCEPTED arrives!
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc1, tracker1.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 44, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			if ent2, _ := sharedStore.get(disarm2.CommandID); ent2.PhaseState != PhaseConfirmed || ent2.AckAccounted != 1 {
				t.Fatalf("disarm2 after HB reconciliation = %+v, want PhaseConfirmed AckAccounted=1", ent2)
			}

			// 4) Restart Service over sharedStore (with reused or fresh Tracker), re-arm vehicle, and submit Disarm #3.
			activeTracker := tracker1
			if freshTrackerOnRestart {
				activeTracker, err = mavlink.NewTracker("boxbotix", "bbops")
				if err != nil {
					t.Fatalf("NewTracker 2: %v", err)
				}
			}
			now = now.Add(10 * time.Millisecond)
			svc2, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  activeTracker,
				Sender:   &fakeFrameSender{},
				Store:    sharedStore,
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService 2: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc2, activeTracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 45, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

			now = now.Add(5 * time.Millisecond)
			disarm3, err := svc2.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand disarm3: %v", err)
			}

			// 5) Disarm #2's delayed ACCEPTED arrives while vehicle is still ARMED!
			// Recovered wrapAckCredit MUST prevent Disarm #3 from falsely confirming while armed!
			now = now.Add(5 * time.Millisecond)
			lateSeq := uint8(43)
			if freshTrackerOnRestart {
				lateSeq = 46
			}
			deliverTrackerOutcome(svc2, activeTracker.IngestFrame(mavcodec.Frame{
				Sequence:    lateSeq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdComponentArmDisarm,
					Result:  mavcodec.MAVResultAccepted,
				},
			}, peer, now), now)
			ent3Mid, _ := sharedStore.get(disarm3.CommandID)
			if ent3Mid.PhaseState != PhaseCommitUnknown || ent3Mid.AckResult != UnacknowledgedResult {
				t.Fatalf("%s: disarm3 falsely confirmed on late Disarm #2 ACCEPTED after restart: %+v", name, ent3Mid)
			}

			// 6) Disarm #3's own ACCEPTED + fresh disarmed #0 HEARTBEAT reconcile Disarm #3 to PhaseConfirmed; settled credit does NOT taint svc3!
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc2, activeTracker.IngestFrame(mavcodec.Frame{
				Sequence:    47,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdComponentArmDisarm,
					Result:  mavcodec.MAVResultAccepted,
				},
			}, peer, now), now)
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc2, activeTracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 48, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			if ent3Final, _ := sharedStore.get(disarm3.CommandID); ent3Final.PhaseState != PhaseConfirmed {
				t.Fatalf("%s: disarm3 after own ACCEPTED + disarmed HB = %+v, want PhaseConfirmed", name, ent3Final)
			}

			now = now.Add(10 * time.Millisecond)
			svc3, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  activeTracker,
				Sender:   &fakeFrameSender{},
				Store:    sharedStore,
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService 3: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc3, activeTracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 49, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			now = now.Add(5 * time.Millisecond)
			disarm4, err := svc3.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand disarm4: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc3, activeTracker.IngestFrame(mavcodec.Frame{
				Sequence:    50,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdComponentArmDisarm,
					Result:  mavcodec.MAVResultFailed,
				},
			}, peer, now), now)
			if ent4, _ := sharedStore.get(disarm4.CommandID); ent4.PhaseState != PhaseRejected || ent4.AckResult != int(mavcodec.MAVResultFailed) {
				t.Fatalf("%s: settled credit leaked into svc3 disarm4: %+v, want PhaseRejected(FAILED)", name, ent4)
			}
		})
	}

	t.Run("same_process_link_blip_and_restore", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 22, 15, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19984")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 41, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(5 * time.Millisecond)
		disarm1, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand disarm1: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    42,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)
		if ent1, _ := store.get(disarm1.CommandID); ent1.PhaseState != PhaseRejected {
			t.Fatalf("disarm1 = %+v, want PhaseRejected", ent1)
		}

		_ = advance255(t, tracker, peer, 42, &now)
		now = now.Add(5 * time.Millisecond)
		disarm2, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand disarm2: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    42,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 44, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent2, _ := store.get(disarm2.CommandID); ent2.PhaseState != PhaseConfirmed {
			t.Fatalf("disarm2 after HB reconciliation = %+v, want PhaseConfirmed", ent2)
		}

		// Link blip (>3s silence) + restore bumps LinkEpoch and runs snapshotPreResetAckHistoryLocked.
		now = now.Add(4 * time.Second)
		for _, ev := range tracker.EvaluateSilence(now) {
			svc.OnTransitionEvent(ev)
		}
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 45, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		now = now.Add(5 * time.Millisecond)
		disarm3, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand disarm3: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    46,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)
		if ent3Mid, _ := store.get(disarm3.CommandID); ent3Mid.PhaseState != PhaseCommitUnknown || ent3Mid.AckResult != UnacknowledgedResult {
			t.Fatalf("disarm3 falsely confirmed on late Disarm #2 ACCEPTED after link blip: %+v", ent3Mid)
		}

		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    47,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 48, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent3Final, _ := store.get(disarm3.CommandID); ent3Final.PhaseState != PhaseConfirmed {
			t.Fatalf("disarm3 after own ACCEPTED + disarmed HB = %+v, want PhaseConfirmed", ent3Final)
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceDelayedWrappedOrUnconfirmedGapDisarmedHeartbeatCannotReconcileTimeout(t *testing.T) {
	t.Run("delayed_wrapped_seq1_disarmed_hb_held_until_corroborated", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 22, 20, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19982")

		// seq=1 disarmed HB, seq=2 armed HB, telemetry seq=3..255, 0.
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		seq := uint8(2)
		for i := 0; i < 254; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{
				Sequence:    seq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, now)
		}

		// Submit Disarm and advance past ACK deadline -> PhaseCommitUnknown.
		now = now.Add(5 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}
		now = now.Add(2 * time.Second)
		svc.Advance(context.Background(), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm after timeout = %+v, want PhaseCommitUnknown", ent)
		}

		// Delayed network copy of seq=1 disarmed HB arrives -> MUST NOT flip Snapshot(1).Armed or confirm Disarm!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		svc.Advance(context.Background(), now)
		if snap, _ := tracker.Snapshot(1); !snap.Armed {
			t.Fatal("Snapshot(1).Armed became false on delayed wrapped seq=1 disarmed HB")
		}
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm falsely confirmed on delayed wrapped seq=1 disarmed HB: %+v", ent)
		}

		// Corroborated post-timeout disarmed HB at seq=2 confirms genuine disarm!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm after corroborated seq=2 disarmed HB = %+v, want PhaseConfirmed", ent)
		}
	})

	t.Run("unconfirmed_gap_disarmed_hb_does_not_reconcile_timeout", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 22, 25, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19983")

		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 9, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}
		now = now.Add(2 * time.Second)
		svc.Advance(context.Background(), now)

		// Fresh ARMED heartbeat at seq=10 advances HeartbeatOrder=2 after timeout.
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		// Unconfirmed gap disarmed HB at seq=80 (delta=70) arrives -> MUST NOT flip Armed or reconcile Disarm!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 80, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		svc.Advance(context.Background(), now)
		if snap, _ := tracker.Snapshot(1); !snap.Armed {
			t.Fatal("Snapshot(1).Armed flipped false on unconfirmed gap seq=80 disarmed HB")
		}
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm falsely confirmed on unconfirmed gap seq=80 disarmed HB: %+v", ent)
		}

		// Normal seq=11 telemetry cancels unconfirmed seq=80; then confirmed seq=90 + seq=91 reconciles Disarm!
		now = now.Add(10 * time.Millisecond)
		_ = tracker.IngestFrame(mavcodec.Frame{
			Sequence:    11,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message:     mavcodec.AttitudeQuaternion{Q1: 1},
		}, peer, now)
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 90, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm confirmed before seq=90 gap was confirmed: %+v", ent)
		}
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    91,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message:     mavcodec.AttitudeQuaternion{Q1: 1},
		}, peer, now), now)
		if snap, _ := tracker.Snapshot(1); snap.Armed {
			t.Fatal("Snapshot(1).Armed stayed true after confirmed gap seq=90..91")
		}
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm after confirmed gap seq=90..91 = %+v, want PhaseConfirmed", ent)
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
func TestServiceDuplicateOldDenialAndLegacyUpgradePreserveWrapAckCredit(t *testing.T) {
	t.Run("extra_old_denial_after_restart_and_wrap_preserves_credit", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		sharedStore := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 23, 10, 0, 0, time.UTC)
		svc1, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    sharedStore,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService 1: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19991")

		// 1) Disarm #1 -> DENIED at seq=10.
		deliverTrackerOutcome(svc1, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 9, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		disarm1, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand disarm1: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc1, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    10,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)
		if ent1, _ := sharedStore.get(disarm1.CommandID); ent1.PhaseState != PhaseRejected {
			t.Fatalf("disarm1 = %+v, want PhaseRejected", ent1)
		}

		// 2) Wrap uint8 stream -> Disarm #2 held on duplicate seq=10 DENIED, then confirmed via disarmed HB -> wrapAckCredit = 1.
		seq := uint8(10)
		for i := 0; i < 255; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{
				Sequence:    seq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, now)
		}
		now = now.Add(5 * time.Millisecond)
		disarm2, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand disarm2: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc1, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    10,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc1, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 11, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent2, _ := sharedStore.get(disarm2.CommandID); ent2.PhaseState != PhaseConfirmed {
			t.Fatalf("disarm2 = %+v, want PhaseConfirmed", ent2)
		}

		// 3) Restart Service (svc2) on sharedStore, re-arm vehicle, wrap stream again, and deliver extra idle copy of Disarm #1 DENIED!
		now = now.Add(10 * time.Millisecond)
		svc2, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    sharedStore,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService 2: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc2, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 12, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		seq = 12
		for i := 0; i < 253; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{
				Sequence:    seq,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message:     mavcodec.AttitudeQuaternion{Q1: 1},
			}, peer, now)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc2, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    10,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)

		// 4) Submit Disarm #3 while Armed; Disarm #2's delayed ACCEPTED arrives while still Armed -> MUST stay PhaseCommitUnknown!
		now = now.Add(5 * time.Millisecond)
		disarm3, err := svc2.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand disarm3: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc2, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    13,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)
		if snap, _ := tracker.Snapshot(1); !snap.Armed {
			t.Fatal("expected tracker to still report Armed=true")
		}
		if ent3, _ := sharedStore.get(disarm3.CommandID); ent3.PhaseState != PhaseCommitUnknown || ent3.AckResult != UnacknowledgedResult {
			t.Fatalf("disarm3 falsely confirmed while armed after extra old DENIED + late ACCEPTED: %+v", ent3)
		}

		// Fresh disarmed heartbeat reconciles Disarm #3 cleanly.
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc2, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 14, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent3Final, _ := sharedStore.get(disarm3.CommandID); ent3Final.PhaseState != PhaseConfirmed {
			t.Fatalf("disarm3 after fresh disarmed HB = %+v, want PhaseConfirmed", ent3Final)
		}
	})

	t.Run("legacy_round8_persisted_row_without_wrap_held_bit_recovers_credit", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		legacyStore := newFakeCommandStore()
		tBase := time.Date(2026, 11, 27, 23, 15, 0, 0, time.UTC)
		deniedBit := 1 << int(mavcodec.MAVResultDenied)

		// Seed exact Round 8 persisted CommandEntity rows (no ackPoolWrapHeldBit on cmd2!).
		legacyStore.entities["boxbotix.bbops.robotics.uxs.command.s10000000000000nlegacyr8cmd001"] = CommandEntity{
			ID:           "boxbotix.bbops.robotics.uxs.command.s10000000000000nlegacyr8cmd001",
			PhaseState:   PhaseRejected,
			Target:       "boxbotix.bbops.robotics.uxs.vehicle.1",
			SysID:        1,
			CmdID:        int(mavcodec.MAVCmdComponentArmDisarm),
			Action:       ActionDisarm,
			AckResult:    int(mavcodec.MAVResultDenied),
			AckAccounted: 1,
			AckPoolMask:  deniedBit,
			IssuedMs:     tBase.UnixMilli(),
			UpdatedMs:    tBase.Add(5 * time.Millisecond).UnixMilli(),
		}
		legacyStore.entities["boxbotix.bbops.robotics.uxs.command.s10000000000010nlegacyr8cmd002"] = CommandEntity{
			ID:           "boxbotix.bbops.robotics.uxs.command.s10000000000010nlegacyr8cmd002",
			PhaseState:   PhaseConfirmed,
			Target:       "boxbotix.bbops.robotics.uxs.vehicle.1",
			SysID:        1,
			CmdID:        int(mavcodec.MAVCmdComponentArmDisarm),
			Action:       ActionDisarm,
			AckResult:    UnacknowledgedResult,
			AckAccounted: 1,
			AckPoolMask:  deniedBit, // Round 8 persisted result mask ONLY (no ackPoolWrapHeldBit)
			IssuedMs:     tBase.Add(20 * time.Millisecond).UnixMilli(),
			UpdatedMs:    tBase.Add(30 * time.Millisecond).UnixMilli(),
		}

		now := tBase.Add(100 * time.Millisecond)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    legacyStore,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService legacy upgrade: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19992")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 20, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		// Direct post-upgrade Disarm #3 + cmd2's delayed ACCEPTED (no prior idle ACK to set surplusAckTainted)
		// MUST stay PhaseCommitUnknown while Armed because legacyWrapHeld recovered wrapAckCredit = 1!
		now = now.Add(5 * time.Millisecond)
		disarm3Direct, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand disarm3Direct: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    21,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peer, now), now)
		if ent3Direct, _ := legacyStore.get(disarm3Direct.CommandID); ent3Direct.PhaseState != PhaseCommitUnknown || ent3Direct.AckResult != UnacknowledgedResult {
			t.Fatalf("legacy Round 8 upgrade falsely confirmed disarm3Direct while armed: %+v", ent3Direct)
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceMultiWrapDisarmedHeartbeatOverrideLossAndUncertainOrderGuard(t *testing.T) {
	t.Run("two_wraps_old_disarmed_hb_never_confirms_timed_out_disarm", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 23, 20, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19993")

		// Cycle 0: seq=1 disarmed, seq=2 armed, telemetry through wrap #1.
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		seq := uint8(2)
		for i := 0; i < 254; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{Sequence: seq, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}

		// Cycle 1: seq=1 armed, telemetry through wrap #2.
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		seq = 1
		for i := 0; i < 255; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{Sequence: seq, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}

		now = now.Add(5 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}
		now = now.Add(2 * time.Second)
		svc.Advance(context.Background(), now)

		// Cycle 2: delayed copy of Cycle 0 (seq=1, Disarmed) arrives -> Disarm MUST stay PhaseCommitUnknown!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		svc.Advance(context.Background(), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm falsely confirmed on two-wrap old (seq=1, Disarmed): %+v", ent)
		}

		// Corroborated seq=2 disarmed HB reconciles Disarm!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm after corroborated seq=2 disarmed HB = %+v, want PhaseConfirmed", ent)
		}
	})

	t.Run("override_after_199_lost_packets_and_post_wrap_seq1_revokes_teleop_immediately", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 23, 25, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19994")

		for s := 1; s <= 220; s++ {
			now = now.Add(2 * time.Millisecond)
			if s == 1 || s == 164 || s == 220 {
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, uint8(s), true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			} else {
				_ = tracker.IngestFrame(mavcodec.Frame{Sequence: uint8(s), SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
			}
		}
		tgt, _ := tracker.ActuationTarget(1)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s1-loss199",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   2.0,
			Engage:               true,
			ObservedMs:           tgt.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt.ObservationRev,
			AuthorityIncarnation: tgt.AuthorityIncarnation,
			AuthorityEpoch:       tgt.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("SubmitTeleop engage: %v", err)
		}

		// Lose 199 packets; receive override at seq=164 (and 165) -> lease MUST be revoked immediately on seq=164!
		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 164, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s1-loss199",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      2.0,
		}); err == nil {
			t.Fatal("expected active teleop lease to be revoked immediately on seq=164 override after 199 lost packets")
		}

		// Post-wrap seq=1 override after earlier seq=1 override + seq=2 companion also revokes teleop immediately!
		tracker2, _ := mavlink.NewTracker("boxbotix", "bbops")
		svcWrap, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker2,
			Sender:   &fakeFrameSender{},
			Store:    newFakeCommandStore(),
			Now:      func() time.Time { return now },
		})
		deliverTrackerOutcome(svcWrap, tracker2.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svcWrap, tracker2.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		seq := uint8(2)
		for i := 0; i < 254; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker2.IngestFrame(mavcodec.Frame{Sequence: seq, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}
		tgt2, _ := tracker2.ActuationTarget(1)
		if _, err := svcWrap.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s1-wrap1",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.5,
			Engage:               true,
			ObservedMs:           tgt2.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt2.ObservationRev,
			AuthorityIncarnation: tgt2.AuthorityIncarnation,
			AuthorityEpoch:       tgt2.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("SubmitTeleop engage post-recovery: %v", err)
		}

		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcWrap, tracker2.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)
		if _, err := svcWrap.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s1-wrap1",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      1.5,
		}); err == nil {
			t.Fatal("expected active teleop lease to be revoked immediately on post-wrap seq=1 override")
		}
	})

	t.Run("unconfirmed_gap_override_preempts_without_confirming_timed_out_sitl_override_toggle", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 27, 23, 30, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:19995")

		// Admit companion-teleop at seq=9, submit ActionSITLCRSFOverride (Param1=1), and advance past ACK deadline -> PhaseCommitUnknown.
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 9, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		sitlToggle, err := svc.SubmitCommand(context.Background(), CommandRequest{
			SysID:  1,
			Action: ActionSITLCRSFOverride,
			Param1: 1,
		})
		if err != nil {
			t.Fatalf("SubmitCommand ActionSITLCRSFOverride: %v", err)
		}
		now = now.Add(2 * time.Second)
		svc.Advance(context.Background(), now)

		// Fresh companion-teleop HB at seq=10 advances HeartbeatOrder=2 after timeout without matching DirectCRSFOverride.
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		// Unconfirmed-gap override HB at seq=80 (delta=70) preempts immediately with HeartbeatOrder=0 -> MUST NOT confirm sitlToggle!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 80, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)
		svc.Advance(context.Background(), now)
		if snap, _ := tracker.Snapshot(1); snap.Authority != uxsvocab.AuthorityDirectCRSFOverride {
			t.Fatalf("Snapshot(1).Authority = %q, want immediate direct-crsf-override preemption", snap.Authority)
		}
		if ent, _ := store.get(sitlToggle.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("ActionSITLCRSFOverride falsely confirmed on unconfirmed-gap seq=80 override: %+v", ent)
		}

		// Corroborating packet at seq=81 confirms gap order -> reconciles ActionSITLCRSFOverride to PhaseConfirmed!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
			Sequence:    81,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message:     mavcodec.AttitudeQuaternion{Q1: 1},
		}, peer, now), now)
		svc.Advance(context.Background(), now)
		if ent, _ := store.get(sitlToggle.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("ActionSITLCRSFOverride after corroborated seq=80..81 = %+v, want PhaseConfirmed", ent)
		}

		// Subcase B: baselinePacketOrder == 0 + packet-ordered target with uncertain HeartbeatOrder == 0 (PacketOrder > 0).
		fakeTargets := newFakeTargetSource()
		tSub := now.Add(20 * time.Millisecond)
		fakeTargets.set(mavlink.ActuationTarget{
			SysID:                   1,
			Domain:                  uxsvocab.DomainRover,
			Armed:                   true,
			CompanionMode:           uxsvocab.ModeTeleopVelocity,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: tSub.UnixNano(),
			HeartbeatSeq:            1,
			HeartbeatOrder:          0,
			PacketOrder:             0,
		})
		subStore := newFakeCommandStore()
		svcSub, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  fakeTargets,
			Sender:   &fakeFrameSender{},
			Store:    subStore,
			Now:      func() time.Time { return tSub },
		})
		if err != nil {
			t.Fatalf("NewService sub: %v", err)
		}
		tSub = tSub.Add(5 * time.Millisecond)
		disarmSub, err := svcSub.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand disarmSub: %v", err)
		}
		tSub = tSub.Add(2 * time.Second)
		svcSub.Advance(context.Background(), tSub)

		// Uncertain-order heartbeat (HeartbeatOrder == 0) on a packet-ordered target (PacketOrder == 5) MUST NOT confirm disarmSub!
		tSub = tSub.Add(10 * time.Millisecond)
		fakeTargets.set(mavlink.ActuationTarget{
			SysID:                   1,
			Domain:                  uxsvocab.DomainRover,
			Armed:                   false,
			CompanionMode:           uxsvocab.ModeDisarmed,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: tSub.UnixNano(),
			HeartbeatSeq:            2,
			HeartbeatOrder:          0,
			PacketOrder:             5,
		})
		svcSub.Advance(context.Background(), tSub)
		if entSub, _ := subStore.get(disarmSub.CommandID); entSub.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm falsely confirmed on uncertain HeartbeatOrder=0 with baselinePacketOrder=0 and PacketOrder=5: %+v", entSub)
		}

		// Once corroborated (HeartbeatOrder == 5 > 0), Advance reconciles disarmSub to PhaseConfirmed.
		tSub = tSub.Add(10 * time.Millisecond)
		fakeTargets.set(mavlink.ActuationTarget{
			SysID:                   1,
			Domain:                  uxsvocab.DomainRover,
			Armed:                   false,
			CompanionMode:           uxsvocab.ModeDisarmed,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: tSub.UnixNano(),
			HeartbeatSeq:            2,
			HeartbeatOrder:          5,
			PacketOrder:             6,
		})
		svcSub.Advance(context.Background(), tSub)
		if entSub, _ := subStore.get(disarmSub.CommandID); entSub.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm after HeartbeatOrder=5 corroboration = %+v, want PhaseConfirmed", entSub)
		}
	})
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceWrapHeldTeleopHoleLossRepeatHeartbeatIdleDupAckFormatV2AndSessionStop(t *testing.T) {
	t.Run("wrap_held_disarmed_refuses_nonzero_teleop_renewal_immediately_while_holding_disarm_reconciliation", func(t *testing.T) {
		tracker, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker: %v", err)
		}
		store := newFakeCommandStore()
		sender := &fakeFrameSender{}
		now := time.Date(2026, 11, 28, 1, 10, 0, 0, time.UTC)
		svc, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   sender,
			Store:    store,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		peer := netip.MustParseAddrPort("127.0.0.1:20101")

		// Cycle 0: seq=1 Disarmed, seq=2 Armed, telemetry through wrap to seq=0 Armed.
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		seq := uint8(2)
		for i := 0; i < 254; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			if seq == 0 {
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 0, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			} else {
				_ = tracker.IngestFrame(mavcodec.Frame{Sequence: seq, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
			}
		}

		// Engage teleop lease at seq=0 Armed AND have a timed-out Disarm in PhaseCommitUnknown.
		tgt0, _ := tracker.ActuationTarget(1)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s-wrap-disarm",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   2.0,
			Engage:               true,
			ObservedMs:           tgt0.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt0.ObservationRev,
			AuthorityIncarnation: tgt0.AuthorityIncarnation,
			AuthorityEpoch:       tgt0.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("SubmitTeleop engage: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		disarmCmd, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}
		now = now.Add(150 * time.Millisecond)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-wrap-disarm",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      2.0,
		}); err != nil {
			t.Fatalf("SubmitTeleop renewal before timeout: %v", err)
		}
		// Advance Disarm past ACK timeout while keeping teleop lease renewed every 150ms.
		for i := 0; i < 12; i++ {
			now = now.Add(150 * time.Millisecond)
			svc.Advance(context.Background(), now)
			if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:   1,
				Session: "s-wrap-disarm",
				Seq:     uint64(3 + i),
				Frame:   FrameLocalNED,
				Vx:      2.0,
			}); err != nil {
				t.Fatalf("SubmitTeleop keepalive %d: %v", i, err)
			}
		}
		if ent, _ := store.get(disarmCmd.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm phase before post-wrap seq=1 disarmed = %q, want %q", ent.PhaseState, PhaseCommitUnknown)
		}

		// Post-wrap seq=1 Disarmed arrives (matches Cycle 0 seq=1 Disarmed):
		// 1) Nonzero teleop renewal MUST be refused immediately (409 vehicle_disarmed) and lease revoked!
		// 2) Timed-out Disarm MUST remain PhaseCommitUnknown until corroborated!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		svc.Advance(context.Background(), now)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-wrap-disarm",
			Seq:     20,
			Frame:   FrameLocalNED,
			Vx:      2.0,
		}); err == nil {
			t.Fatal("expected nonzero teleop renewal to be refused immediately on post-wrap seq=1 disarmed heartbeat")
		}
		if ent, _ := store.get(disarmCmd.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm falsely reconciled on uncorroborated post-wrap seq=1 disarmed: %+v", ent)
		}

		// Corroborating seq=2 Disarmed reconciles Disarm to PhaseConfirmed.
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent, _ := store.get(disarmCmd.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm after corroborating seq=2 disarmed = %+v, want PhaseConfirmed", ent)
		}
	})

	t.Run("loss_199_over_prior_holes_and_matching_prior_override_slot_stops_teleop_immediately", func(t *testing.T) {
		peer := netip.MustParseAddrPort("127.0.0.1:20102")
		now := time.Date(2026, 11, 28, 1, 15, 0, 0, time.UTC)

		// Subcase A: seq=164 and seq=165 were omitted holes in Cycle 0; lose 199 packets -> seq=164 override revokes teleop immediately.
		trackerA, _ := mavlink.NewTracker("boxbotix", "bbops")
		svcA, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerA,
			Sender:   &fakeFrameSender{},
			Store:    newFakeCommandStore(),
			Now:      func() time.Time { return now },
		})
		for s := 1; s <= 220; s++ {
			if s == 164 || s == 165 {
				continue
			}
			now = now.Add(2 * time.Millisecond)
			if s == 1 || s == 220 {
				deliverTrackerOutcome(svcA, trackerA.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, uint8(s), true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			} else {
				_ = trackerA.IngestFrame(mavcodec.Frame{Sequence: uint8(s), SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
			}
		}
		tgtA, _ := trackerA.ActuationTarget(1)
		if _, err := svcA.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s-hole-199",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.8,
			Engage:               true,
			ObservedMs:           tgtA.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgtA.ObservationRev,
			AuthorityIncarnation: tgtA.AuthorityIncarnation,
			AuthorityEpoch:       tgtA.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("svcA engage: %v", err)
		}
		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcA, trackerA.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 164, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)
		if _, err := svcA.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-hole-199",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      1.8,
		}); err == nil {
			t.Fatal("expected svcA teleop renewal to fail immediately on seq=164 override over Cycle 0 hole")
		}

		// Subcase B: seq=164 WAS DirectCRSFOverride in Cycle 0 before seq=220 restored CompanionTeleop;
		// after 199 lost packets, seq=164 override matches recentHbFrame[164] -> renewal MUST still fail immediately with zero #84 egress!
		trackerB, _ := mavlink.NewTracker("boxbotix", "bbops")
		senderB := &fakeFrameSender{}
		svcB, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerB,
			Sender:   senderB,
			Store:    newFakeCommandStore(),
			Now:      func() time.Time { return now },
		})
		for s := 1; s <= 220; s++ {
			now = now.Add(2 * time.Millisecond)
			switch s {
			case 1, 220:
				deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, uint8(s), true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			case 164:
				deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 164, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)
			default:
				_ = trackerB.IngestFrame(mavcodec.Frame{Sequence: uint8(s), SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
			}
		}
		tgtB, _ := trackerB.ActuationTarget(1)
		if _, err := svcB.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s-dupslot-199",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.8,
			Engage:               true,
			ObservedMs:           tgtB.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgtB.ObservationRev,
			AuthorityIncarnation: tgtB.AuthorityIncarnation,
			AuthorityEpoch:       tgtB.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("svcB engage: %v", err)
		}
		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 164, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)
		sentBefore := senderB.countByMsgID(mavcodec.MsgIDSetPositionTargetLocalNED)
		if _, err := svcB.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-dupslot-199",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      1.8,
		}); err == nil {
			t.Fatal("expected svcB teleop renewal to fail immediately on seq=164 override matching Cycle 0 slot")
		}
		if got := senderB.countByMsgID(mavcodec.MsgIDSetPositionTargetLocalNED); got != sentBefore {
			t.Fatalf("svcB emitted #84 frame during uncertain authority preemption: got %d, want %d", got, sentBefore)
		}
	})

	t.Run("two_historical_disarmed_heartbeats_never_confirm_timed_out_disarm_after_wrap", func(t *testing.T) {
		tracker, _ := mavlink.NewTracker("boxbotix", "bbops")
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 28, 1, 20, 0, 0, time.UTC)
		svc, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		peer := netip.MustParseAddrPort("127.0.0.1:20103")

		// Cycle 0: seq=1 Disarmed, seq=2 Disarmed, seq=3 Armed, telemetry through wrap.
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 3, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		seq := uint8(3)
		for i := 0; i < 253; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{Sequence: seq, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}

		now = now.Add(5 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}
		now = now.Add(2 * time.Second)
		svc.Advance(context.Background(), now)

		// Delayed historical copies (seq=1, Disarmed) AND (seq=2, Disarmed) arrive consecutively -> Disarm MUST stay PhaseCommitUnknown!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		svc.Advance(context.Background(), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm falsely confirmed by two historical disarmed copies (seq=1,2): %+v", ent)
		}

		// Genuine fresh Disarmed at seq=3 confirms Disarm!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 3, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("Disarm after genuine seq=3 disarmed = %+v, want PhaseConfirmed", ent)
		}
	})

	t.Run("no_restart_post_wrap_copied_denial_never_consumes_unacked_slot_for_delayed_accepted", func(t *testing.T) {
		for _, idleFirst := range []bool{true, false} {
			tracker, _ := mavlink.NewTracker("boxbotix", "bbops")
			store := newFakeCommandStore()
			now := time.Date(2026, 11, 28, 1, 25, 0, 0, time.UTC)
			svc, _ := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   &fakeFrameSender{},
				Store:    store,
				Now:      func() time.Time { return now },
			})
			peer := netip.MustParseAddrPort("127.0.0.1:20104")

			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

			// 1) Disarm A is denied at seq=2.
			now = now.Add(5 * time.Millisecond)
			disarmA, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand Disarm A: %v", err)
			}
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
				Sequence:    2,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdComponentArmDisarm,
					Result:  mavcodec.MAVResultDenied,
				},
			}, peer, now), now)
			if entA, _ := store.get(disarmA.CommandID); entA.PhaseState != PhaseRejected {
				t.Fatalf("Disarm A = %+v, want PhaseRejected", entA)
			}

			// 2) Disarm B times out and is heartbeat-confirmed at seq=3 while its ACCEPTED (seq=4) is delayed on UDP.
			now = now.Add(5 * time.Millisecond)
			disarmB, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand Disarm B: %v", err)
			}
			now = now.Add(2 * time.Second)
			svc.Advance(context.Background(), now)
			now = now.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 3, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			if entB, _ := store.get(disarmB.CommandID); entB.PhaseState != PhaseConfirmed {
				t.Fatalf("Disarm B = %+v, want PhaseConfirmed", entB)
			}

			// 3) Vehicle rearms at seq=5 and 8-bit MAVLink seq wraps cleanly.
			now = now.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 5, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			seq := uint8(5)
			for i := 0; i < 252; i++ {
				seq++
				now = now.Add(2 * time.Millisecond)
				_ = tracker.IngestFrame(mavcodec.Frame{Sequence: seq, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
			}
			// seq is now 1 in new cycle.

			var disarmC CommandResult
			if idleFirst {
				// Copied DENIED from Disarm A arrives while IDLE, then Disarm C is submitted, then B's delayed ACCEPTED arrives.
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
					Sequence:    2,
					SystemID:    1,
					ComponentID: mavcodec.CompIDAutopilot1,
					Message: mavcodec.CommandAck{
						Command: mavcodec.MAVCmdComponentArmDisarm,
						Result:  mavcodec.MAVResultDenied,
					},
				}, peer, now), now)
				now = now.Add(5 * time.Millisecond)
				disarmC, err = svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
				if err != nil {
					t.Fatalf("SubmitCommand Disarm C (idleFirst): %v", err)
				}
			} else {
				// Disarm C is submitted FIRST, then copied DENIED from Disarm A arrives while C is in flight, then B's delayed ACCEPTED arrives.
				now = now.Add(5 * time.Millisecond)
				disarmC, err = svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
				if err != nil {
					t.Fatalf("SubmitCommand Disarm C (inFlightFirst): %v", err)
				}
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
					Sequence:    2,
					SystemID:    1,
					ComponentID: mavcodec.CompIDAutopilot1,
					Message: mavcodec.CommandAck{
						Command: mavcodec.MAVCmdComponentArmDisarm,
						Result:  mavcodec.MAVResultDenied,
					},
				}, peer, now), now)
			}

			// Now B's delayed ACCEPTED arrives at seq=4 while vehicle is STILL Armed -> Disarm C MUST stay PhaseCommitUnknown!
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{
				Sequence:    4,
				SystemID:    1,
				ComponentID: mavcodec.CompIDAutopilot1,
				Message: mavcodec.CommandAck{
					Command: mavcodec.MAVCmdComponentArmDisarm,
					Result:  mavcodec.MAVResultAccepted,
				},
			}, peer, now), now)
			if entC, _ := store.get(disarmC.CommandID); entC.PhaseState != PhaseCommitUnknown || entC.AckResult != UnacknowledgedResult {
				t.Fatalf("idleFirst=%v: Disarm C falsely confirmed while Armed on B's delayed ACCEPTED: %+v", idleFirst, entC)
			}
		}
	})

	t.Run("lossless_idle_denial_recovery_rejects_post_restart_failed_command_cleanly", func(t *testing.T) {
		tracker1, _ := mavlink.NewTracker("boxbotix", "bbops")
		sharedStore := newFakeCommandStore()
		now := time.Date(2026, 11, 28, 1, 30, 0, 0, time.UTC)
		svc1, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker1,
			Sender:   &fakeFrameSender{},
			Store:    sharedStore,
			Now:      func() time.Time { return now },
		})
		peer := netip.MustParseAddrPort("127.0.0.1:20105")
		deliverTrackerOutcome(svc1, tracker1.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		// cmd0: Disarm denied cleanly at seq=2.
		now = now.Add(5 * time.Millisecond)
		cmd0, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("cmd0: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc1, tracker1.IngestFrame(mavcodec.Frame{
			Sequence:    2,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)
		if ent0, _ := sharedStore.get(cmd0.CommandID); ent0.PhaseState != PhaseRejected {
			t.Fatalf("cmd0 = %+v, want PhaseRejected", ent0)
		}

		// cmd1: Disarm times out, reconciles via Disarmed HB at seq=3, then receives its late DENIED at seq=4 while idle (lossless, no wrap).
		now = now.Add(5 * time.Millisecond)
		cmd1, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("cmd1: %v", err)
		}
		now = now.Add(2 * time.Second)
		svc1.Advance(context.Background(), now)
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc1, tracker1.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 3, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc1, tracker1.IngestFrame(mavcodec.Frame{
			Sequence:    4,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultDenied,
			},
		}, peer, now), now)
		if ent1, _ := sharedStore.get(cmd1.CommandID); ent1.AckAccounted != 1 || (ent1.AckPoolMask&ackPoolWrapHeldBit) != 0 || (ent1.AckPoolMask&ackPoolFormatV2Bit) == 0 {
			t.Fatalf("cmd1 persisted metadata = %+v, want AckAccounted=1 formatV2=1 wrapHeld=0", ent1)
		}

		// Restart onto svc2 with the same store; vehicle is Armed; cmd2 receives explicit MAV_RESULT_FAILED -> MUST be PhaseRejected!
		now = now.Add(100 * time.Millisecond)
		tracker2, _ := mavlink.NewTracker("boxbotix", "bbops")
		svc2, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker2,
			Sender:   &fakeFrameSender{},
			Store:    sharedStore,
			Now:      func() time.Time { return now },
		})
		deliverTrackerOutcome(svc2, tracker2.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		cmd2, err := svc2.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("cmd2: %v", err)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc2, tracker2.IngestFrame(mavcodec.Frame{
			Sequence:    11,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message: mavcodec.CommandAck{
				Command: mavcodec.MAVCmdComponentArmDisarm,
				Result:  mavcodec.MAVResultFailed,
			},
		}, peer, now), now)
		if ent2, _ := sharedStore.get(cmd2.CommandID); ent2.PhaseState != PhaseRejected || ent2.AckResult != int(mavcodec.MAVResultFailed) {
			t.Fatalf("cmd2 after lossless idle denial + restart + FAILED = %+v, want PhaseRejected with MAVResultFailed", ent2)
		}
	})

	t.Run("session_bound_stop_at_484ms_terminates_live_server_lease_while_preserving_replacement_lease", func(t *testing.T) {
		tracker, _ := mavlink.NewTracker("boxbotix", "bbops")
		sender := &fakeFrameSender{}
		now := time.Date(2026, 11, 28, 1, 35, 0, 0, time.UTC)
		svc, _ := NewService(Options{
			Org:            "boxbotix",
			Platform:       "bbops",
			Targets:        tracker,
			Sender:         sender,
			Store:          newFakeCommandStore(),
			TeleopLeaseTTL: 240 * time.Millisecond,
			Now:            func() time.Time { return now },
		})
		peer := netip.MustParseAddrPort("127.0.0.1:20106")
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		tgt, _ := tracker.ActuationTarget(1)

		// Engage s-orig at 0ms, renew nonzero at 230ms and 455ms (extending server lease to 695ms).
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s-orig",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   2.5,
			Engage:               true,
			ObservedMs:           tgt.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt.ObservationRev,
			AuthorityIncarnation: tgt.AuthorityIncarnation,
			AuthorityEpoch:       tgt.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("engage s-orig: %v", err)
		}
		now = now.Add(230 * time.Millisecond)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{SysID: 1, Session: "s-orig", Seq: 2, Frame: FrameLocalNED, Vx: 2.5}); err != nil {
			t.Fatalf("renew 230ms: %v", err)
		}
		now = now.Add(225 * time.Millisecond) // 455ms
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{SysID: 1, Session: "s-orig", Seq: 3, Frame: FrameLocalNED, Vx: 2.5}); err != nil {
			t.Fatalf("renew 455ms: %v", err)
		}

		// Completed Disengage gesture at 484ms sends Stop for s-orig -> immediately terminates s-orig and emits zero #84!
		now = now.Add(29 * time.Millisecond) // 484ms
		beforeZero := sender.countByMsgID(mavcodec.MsgIDSetPositionTargetLocalNED)
		stopRes, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-orig",
			Frame:   FrameLocalNED,
			Stop:    true,
		})
		if err != nil || stopRes.LeaseActive {
			t.Fatalf("Stop s-orig at 484ms = (%+v, %v), want LeaseActive=false", stopRes, err)
		}
		if got := sender.countByMsgID(mavcodec.MsgIDSetPositionTargetLocalNED); got != beforeZero+1 {
			t.Fatalf("expected immediate zero-velocity #84 on Stop at 484ms, count = %d want %d", got, beforeZero+1)
		}

		// Engage replacement session s-next; a late Stop for old s-orig MUST NOT disturb s-next!
		now = now.Add(5 * time.Millisecond)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s-next",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.2,
			Engage:               true,
			ObservedMs:           tgt.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt.ObservationRev,
			AuthorityIncarnation: tgt.AuthorityIncarnation,
			AuthorityEpoch:       tgt.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("engage s-next: %v", err)
		}
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-orig",
			Frame:   FrameLocalNED,
			Stop:    true,
		}); err != nil {
			t.Fatalf("late Stop s-orig: %v", err)
		}
		if renewNext, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-next",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      1.2,
		}); err != nil || !renewNext.LeaseActive {
			t.Fatalf("s-next renewal after late s-orig Stop = (%+v, %v), want LeaseActive=true", renewNext, err)
		}
	})
}

type barrierTargetSource struct {
	inner     *mavlink.Tracker
	mu        sync.Mutex
	armPause  bool
	pausedCh  chan struct{}
	releaseCh chan struct{}
}

func (b *barrierTargetSource) ActuationTarget(sysid uint8) (mavlink.ActuationTarget, bool) {
	tgt, ok := b.inner.ActuationTarget(sysid)
	b.mu.Lock()
	shouldPause := b.armPause && ok && tgt.TeleopUncertain
	if shouldPause {
		b.armPause = false
	}
	pausedCh := b.pausedCh
	releaseCh := b.releaseCh
	b.mu.Unlock()
	if shouldPause {
		close(pausedCh)
		<-releaseCh
	}
	return tgt, ok
}

func (b *barrierTargetSource) ResolveObservation(sysid uint8, obsRev, authorityEpoch uint64, observedMs int64) (uint64, bool) {
	return b.inner.ResolveObservation(sysid, obsRev, authorityEpoch, observedMs)
}

// spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
// spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
func TestServiceMotionSuspensionPersistenceCadenceRecoveryDiscreteRestrictionsAndReplacementLease(t *testing.T) {
	t.Run("finding1_motion_suspension_survives_delayed_attitude_and_copied_armed_heartbeat_before_renewal", func(t *testing.T) {
		peer := netip.MustParseAddrPort("127.0.0.1:20201")
		now := time.Date(2026, 11, 28, 3, 0, 0, 0, time.UTC)

		// Subcase A: unconfirmed-gap disarmed HB -> delayed attitude arrives BEFORE next renewal -> renewal refused!
		trackerA, _ := mavlink.NewTracker("boxbotix", "bbops")
		senderA := &fakeFrameSender{}
		svcA, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerA,
			Sender:   senderA,
			Store:    newFakeCommandStore(),
			Now:      func() time.Time { return now },
		})
		deliverTrackerOutcome(svcA, trackerA.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		for seq := uint8(2); seq <= 10; seq++ {
			now = now.Add(5 * time.Millisecond)
			_ = trackerA.IngestFrame(mavcodec.Frame{Sequence: seq, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}
		tgtA, _ := trackerA.ActuationTarget(1)
		if _, err := svcA.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s-gap-attitude",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   2.0,
			Engage:               true,
			ObservedMs:           tgtA.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgtA.ObservationRev,
			AuthorityIncarnation: tgtA.AuthorityIncarnation,
			AuthorityEpoch:       tgtA.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("svcA engage: %v", err)
		}

		// Disarmed heartbeat opens gap (seq=91), then delayed attitude (seq=11) arrives BEFORE renewal.
		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcA, trackerA.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 91, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svcA, trackerA.IngestFrame(mavcodec.Frame{Sequence: 11, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)

		var ce *ControlError
		if _, err := svcA.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-gap-attitude",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      2.0,
		}); !errors.As(err, &ce) || ce.Code != "vehicle_disarmed" {
			t.Fatalf("svcA renewal after delayed attitude got err=%v, want 409 vehicle_disarmed", err)
		}

		// Subcase B: post-wrap disarmed HB -> copied earlier armed HB arrives BEFORE next renewal -> renewal refused!
		trackerB, _ := mavlink.NewTracker("boxbotix", "bbops")
		senderB := &fakeFrameSender{}
		svcB, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerB,
			Sender:   senderB,
			Store:    newFakeCommandStore(),
			Now:      func() time.Time { return now },
		})
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		seq := uint8(2)
		for i := 0; i < 254; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = trackerB.IngestFrame(mavcodec.Frame{Sequence: seq, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}
		tgtB, _ := trackerB.ActuationTarget(1)
		if _, err := svcB.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s-wrap-copied-armed",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   2.0,
			Engage:               true,
			ObservedMs:           tgtB.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgtB.ObservationRev,
			AuthorityIncarnation: tgtB.AuthorityIncarnation,
			AuthorityEpoch:       tgtB.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("svcB engage: %v", err)
		}

		// Genuine disarmed HB at seq=1 suspends motion; copied Cycle 0 armed HB at seq=2 arrives BEFORE renewal.
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		ce = nil
		if _, err := svcB.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "s-wrap-copied-armed",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      2.0,
		}); !errors.As(err, &ce) || ce.Code != "vehicle_disarmed" {
			t.Fatalf("svcB renewal after copied Cycle 0 armed HB got err=%v, want 409 vehicle_disarmed", err)
		}
	})

	t.Run("finding2_regular_1hz_hb_31hz_kinematics_reconciles_timed_out_disarm_while_holding_delayed_copies", func(t *testing.T) {
		tracker, _ := mavlink.NewTracker("boxbotix", "bbops")
		store := newFakeCommandStore()
		now := time.Date(2026, 11, 28, 3, 10, 0, 0, time.UTC)
		svc, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   &fakeFrameSender{},
			Store:    store,
			Now:      func() time.Time { return now },
		})
		peer := netip.MustParseAddrPort("127.0.0.1:20202")

		var seq uint8
		emitSecond := func(armed bool, mode mavcodec.CompanionMode) {
			seq++
			now = now.Add(31 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, seq, armed, mode, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			for k := 0; k < 31; k++ {
				seq++
				now = now.Add(31 * time.Millisecond)
				_ = tracker.IngestFrame(mavcodec.Frame{Sequence: seq, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
			}
			svc.Advance(context.Background(), now)
		}

		for i := 0; i < 8; i++ {
			emitSecond(false, mavcodec.ModeDisarmed)
		}
		for i := 0; i < 8; i++ {
			emitSecond(true, mavcodec.ModeTeleopVelocity)
		}

		now = now.Add(5 * time.Millisecond)
		disarmCmd, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}
		// Advance 2 seconds of kinematics so Disarm times out to commit_unknown while staying on cadence.
		for sec := 0; sec < 2; sec++ {
			emitSecond(true, mavcodec.ModeTeleopVelocity)
		}
		now = now.Add(100 * time.Millisecond)
		svc.Advance(context.Background(), now)
		if ent, _ := store.get(disarmCmd.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm after timeout = %+v, want PhaseCommitUnknown", ent)
		}

		// Deliver 64 consecutive genuine disarmed heartbeats + 31 kinematics/sec:
		// delayed copy count 1..2 stays held (commit_unknown), and sustained stream reconciles well before 64s!
		for sec := 1; sec <= 64; sec++ {
			emitSecond(false, mavcodec.ModeDisarmed)
			ent, _ := store.get(disarmCmd.CommandID)
			if sec <= 2 && ent.PhaseState != PhaseCommitUnknown {
				t.Fatalf("Disarm confirmed too early on held heartbeat #%d: %+v", sec, ent)
			}
		}
		snap, _ := tracker.Snapshot(1)
		ent, _ := store.get(disarmCmd.CommandID)
		if snap.Armed || ent.PhaseState != PhaseConfirmed {
			t.Fatalf("after 64s genuine disarmed cadence: Armed=%v cmd=%+v, want Armed=false PhaseConfirmed", snap.Armed, ent)
		}
	})

	t.Run("finding3_repeated_override_preempts_pending_hold_blocks_arm_preserves_emergency_disarm_and_blocks_sitl_on_uncertain_supervisory", func(t *testing.T) {
		peer := netip.MustParseAddrPort("127.0.0.1:20203")
		now := time.Date(2026, 11, 28, 3, 20, 0, 0, time.UTC)

		// 3A) Repeated DirectCRSFOverride slot (seq=164 -> seq=220 teleop -> lose 199 -> seq=164 override):
		// pending Hold MUST transition to PhasePreempted and subsequent Arm MUST be rejected with zero outbound #76!
		trackerCRSF, _ := mavlink.NewTracker("boxbotix", "bbops")
		senderCRSF := &fakeFrameSender{}
		storeCRSF := newFakeCommandStore()
		svcCRSF, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerCRSF,
			Sender:   senderCRSF,
			Store:    storeCRSF,
			Now:      func() time.Time { return now },
		})
		for s := 1; s <= 220; s++ {
			now = now.Add(2 * time.Millisecond)
			switch s {
			case 1, 220:
				deliverTrackerOutcome(svcCRSF, trackerCRSF.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, uint8(s), true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			case 164:
				deliverTrackerOutcome(svcCRSF, trackerCRSF.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 164, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)
			default:
				_ = trackerCRSF.IngestFrame(mavcodec.Frame{Sequence: uint8(s), SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
			}
		}

		now = now.Add(5 * time.Millisecond)
		holdCmd, err := svcCRSF.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		if err != nil {
			t.Fatalf("SubmitCommand Hold: %v", err)
		}
		cmdLongBeforeOverride := senderCRSF.countByMsgID(mavcodec.MsgIDCommandLong)

		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcCRSF, trackerCRSF.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 164, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride), peer, now), now)

		if holdEnt, _ := storeCRSF.get(holdCmd.CommandID); holdEnt.PhaseState != PhasePreempted {
			t.Fatalf("pending Hold after repeated-slot DirectCRSFOverride = %+v, want PhasePreempted", holdEnt)
		}
		var ce *ControlError
		if _, err := svcCRSF.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm}); !errors.As(err, &ce) || ce.Code != "authority_preempted" {
			t.Fatalf("SubmitCommand Arm during repeated-slot DirectCRSFOverride got err=%v, want 409 authority_preempted", err)
		}
		if got := senderCRSF.countByMsgID(mavcodec.MsgIDCommandLong); got != cmdLongBeforeOverride {
			t.Fatalf("CommandLong count after rejected Arm = %d, want %d (zero new #76 frames)", got, cmdLongBeforeOverride)
		}

		// 3B) Repeated MCUFailsafe slot preempts pending Hold and rejects Hold/Arm while preserving emergency Disarm!
		trackerMCU, _ := mavlink.NewTracker("boxbotix", "bbops")
		senderMCU := &fakeFrameSender{}
		storeMCU := newFakeCommandStore()
		svcMCU, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerMCU,
			Sender:   senderMCU,
			Store:    storeMCU,
			Now:      func() time.Time { return now },
		})
		for s := 1; s <= 220; s++ {
			now = now.Add(2 * time.Millisecond)
			switch s {
			case 1, 220:
				deliverTrackerOutcome(svcMCU, trackerMCU.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, uint8(s), true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			case 164:
				deliverTrackerOutcome(svcMCU, trackerMCU.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 164, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityMCUFailsafe), peer, now), now)
			default:
				_ = trackerMCU.IngestFrame(mavcodec.Frame{Sequence: uint8(s), SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
			}
		}
		holdMCU, err := svcMCU.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition})
		if err != nil {
			t.Fatalf("svcMCU Hold: %v", err)
		}
		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcMCU, trackerMCU.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 164, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityMCUFailsafe), peer, now), now)
		if holdEnt, _ := storeMCU.get(holdMCU.CommandID); holdEnt.PhaseState != PhasePreempted {
			t.Fatalf("pending Hold after repeated-slot MCUFailsafe = %+v, want PhasePreempted", holdEnt)
		}
		beforeMCUReject := senderMCU.countByMsgID(mavcodec.MsgIDCommandLong)
		ce = nil
		if _, err := svcMCU.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionArm}); !errors.As(err, &ce) || ce.Code != "authority_preempted" {
			t.Fatalf("svcMCU Arm got err=%v, want authority_preempted", err)
		}
		if got := senderMCU.countByMsgID(mavcodec.MsgIDCommandLong); got != beforeMCUReject {
			t.Fatalf("svcMCU CommandLong count after rejected Arm = %d, want %d", got, beforeMCUReject)
		}
		if _, err := svcMCU.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm}); err != nil {
			t.Fatalf("emergency Disarm under repeated-slot MCUFailsafe failed: %v", err)
		}
		if got := senderMCU.countByMsgID(mavcodec.MsgIDCommandLong); got != beforeMCUReject+1 {
			t.Fatalf("emergency Disarm CommandLong count = %d, want %d", got, beforeMCUReject+1)
		}

		// 3C) SITL-local heartbeat followed by supervisory-link heartbeat of uncertain order:
		// direct validateCommandTarget AND SubmitCommand reject SITL-only actions with 403 sitl_only_action!
		trackerSup, _ := mavlink.NewTracker("boxbotix", "bbops")
		senderSup := &fakeFrameSender{}
		svcSup, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerSup,
			Sender:   senderSup,
			Store:    newFakeCommandStore(),
			Now:      func() time.Time { return now },
		})
		deliverTrackerOutcome(svcSup, trackerSup.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcSup, trackerSup.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 90, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierMeshSupervisory), peer, now), now)

		supTarget, supOK := trackerSup.ActuationTarget(1)
		ce = nil
		if vErr := validateCommandTarget(1, ActionSITLCRSFOverride, true, supTarget, supOK); !errors.As(vErr, &ce) || ce.Code != "sitl_only_action" {
			t.Fatalf("validateCommandTarget on uncertain supervisory link = %v, want 403 sitl_only_action", vErr)
		}
		ce = nil
		if _, err := svcSup.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); !errors.As(err, &ce) || ce.Code != "sitl_only_action" {
			t.Fatalf("SubmitCommand SITL action on uncertain supervisory link = %v, want 403 sitl_only_action", err)
		}
		if got := senderSup.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
			t.Fatalf("SITL action on uncertain supervisory link emitted %d #76 frames, want 0", got)
		}
	})

	t.Run("finding4_delayed_uncertain_renewal_never_stops_newer_healthy_replacement_session", func(t *testing.T) {
		tracker, _ := mavlink.NewTracker("boxbotix", "bbops")
		barrier := &barrierTargetSource{
			inner:     tracker,
			pausedCh:  make(chan struct{}),
			releaseCh: make(chan struct{}),
		}
		sender := &fakeFrameSender{}
		var muNow sync.Mutex
		curNow := time.Date(2026, 11, 28, 3, 30, 0, 0, time.UTC)
		getNow := func() time.Time {
			muNow.Lock()
			defer muNow.Unlock()
			return curNow
		}
		addNow := func(d time.Duration) time.Time {
			muNow.Lock()
			curNow = curNow.Add(d)
			out := curNow
			muNow.Unlock()
			return out
		}

		svc, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  barrier,
			Sender:   sender,
			Store:    newFakeCommandStore(),
			Now:      getNow,
		})
		peer := netip.MustParseAddrPort("127.0.0.1:20204")
		t0 := getNow()
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, t0), t0)

		tgt1, _ := tracker.ActuationTarget(1)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "session-old",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.5,
			Engage:               true,
			ObservedMs:           tgt1.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt1.ObservationRev,
			AuthorityIncarnation: tgt1.AuthorityIncarnation,
			AuthorityEpoch:       tgt1.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("engage session-old: %v", err)
		}

		// Uncertain disarmed heartbeat arrives at seq=90; arm barrier so old renewal pauses right after sampling uncertain target.
		tUncertain := addNow(20 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 90, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, tUncertain), tUncertain)
		barrier.mu.Lock()
		barrier.armPause = true
		barrier.mu.Unlock()

		oldErrCh := make(chan error, 1)
		go func() {
			_, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:   1,
				Session: "session-old",
				Seq:     2,
				Frame:   FrameLocalNED,
				Vx:      1.5,
			})
			oldErrCh <- err
		}()
		<-barrier.pausedCh

		// Healthy post-disarm armed heartbeat arrives at seq=91 (>90) and replacement session-new engages before session-old resumes.
		tHealthy := addNow(15 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 91, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, tHealthy), tHealthy)
		tgt2, _ := tracker.ActuationTarget(1)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "session-new",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   2.2,
			Engage:               true,
			ObservedMs:           tgt2.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt2.ObservationRev,
			AuthorityIncarnation: tgt2.AuthorityIncarnation,
			AuthorityEpoch:       tgt2.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("engage session-new: %v", err)
		}

		// Resume delayed session-old renewal: it MUST remain refused while session-new stays active and can renew!
		close(barrier.releaseCh)
		var ce *ControlError
		if oldErr := <-oldErrCh; !errors.As(oldErr, &ce) || ce.Code != "vehicle_disarmed" {
			t.Fatalf("resumed stale renewal err = %v, want 409 vehicle_disarmed", oldErr)
		}
		if !svc.HasActiveLease(1) {
			t.Fatal("delayed uncertain renewal deleted active replacement lease")
		}
		_ = addNow(20 * time.Millisecond)
		if renewRes, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "session-new",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      2.2,
		}); err != nil || !renewRes.LeaseActive {
			t.Fatalf("replacement session-new renewal after delayed old renewal = (%+v, %v), want LeaseActive=true", renewRes, err)
		}
	})

	t.Run("ackpoolmask_vocab_documents_result_range_and_reserved_accounting_bits", func(t *testing.T) {
		pack := ControlPackSpec()
		found := false
		for _, pred := range pack.Predicates {
			if pred.Name == PredicateCommandAckPoolMask {
				found = true
				if !strings.Contains(pred.Description, "bits 0..27") || !strings.Contains(pred.Description, "bit 30") {
					t.Fatalf("PredicateCommandAckPoolMask description = %q, want result-bit range and reserved accounting bits", pred.Description)
				}
			}
		}
		if !found {
			t.Fatal("PredicateCommandAckPoolMask missing from ControlPackSpec")
		}
	})

	t.Run("delayed_pre_disarm_armed_heartbeat_keeps_motion_blocked_while_post_disarm_82_restores_eligibility", func(t *testing.T) {
		// Case A: Lease active at seq=10 -> genuine disarmed HB at seq=81 suspends motion ->
		// delayed pre-disarm armed HB at seq=11 MUST keep old session's nonzero renewal blocked!
		now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
		tracker, _ := mavlink.NewTracker("boxbotix", "bbops")
		sender := &fakeFrameSender{}
		svc, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   sender,
			Store:    newFakeCommandStore(),
			Now:      func() time.Time { return now },
		})
		peer := netip.MustParseAddrPort("127.0.0.1:20301")

		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		tgt10, _ := tracker.ActuationTarget(1)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "sess-pre-disarm",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.4,
			Engage:               true,
			ObservedMs:           tgt10.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt10.ObservationRev,
			AuthorityIncarnation: tgt10.AuthorityIncarnation,
			AuthorityEpoch:       tgt10.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("engage at seq=10: %v", err)
		}

		// Genuine disarmed heartbeat at seq=81 suspends motion.
		now = now.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 81, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		// Delayed pre-disarm armed heartbeat at seq=11 arrives late for the first time.
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 11, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

		countNonZero := func(s *fakeFrameSender) int {
			n := 0
			for _, rec := range s.snapshot() {
				if sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED); ok &&
					(sp.Vx != 0 || sp.Vy != 0 || sp.Vz != 0 || sp.YawRate != 0) {
					n++
				}
			}
			return n
		}
		beforeRenewNonzero := countNonZero(sender)
		var ce *ControlError
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "sess-pre-disarm",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      1.4,
		}); !errors.As(err, &ce) || ce.Code != "vehicle_disarmed" {
			t.Fatalf("nonzero renewal after delayed pre-disarm seq=11 got err=%v, want 409 vehicle_disarmed", err)
		}
		if got := countNonZero(sender); got != beforeRenewNonzero {
			t.Fatalf("delayed pre-disarm seq=11 allowed nonzero #84 renewal (got %d, want %d)", got, beforeRenewNonzero)
		}

		// Control case: Lease active at seq=10 -> genuine disarmed HB at seq=81 -> genuinely post-disarm armed HB at seq=82
		// restores fresh Engage eligibility while revoking the pre-disarm session!
		nowCtrl := time.Date(2026, 11, 28, 4, 1, 0, 0, time.UTC)
		trackerCtrl, _ := mavlink.NewTracker("boxbotix", "bbops")
		senderCtrl := &fakeFrameSender{}
		svcCtrl, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerCtrl,
			Sender:   senderCtrl,
			Store:    newFakeCommandStore(),
			Now:      func() time.Time { return nowCtrl },
		})
		deliverTrackerOutcome(svcCtrl, trackerCtrl.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, nowCtrl), nowCtrl)
		tgtCtrl10, _ := trackerCtrl.ActuationTarget(1)
		if _, err := svcCtrl.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "sess-ctrl-old",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.2,
			Engage:               true,
			ObservedMs:           tgtCtrl10.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgtCtrl10.ObservationRev,
			AuthorityIncarnation: tgtCtrl10.AuthorityIncarnation,
			AuthorityEpoch:       tgtCtrl10.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("control engage at seq=10: %v", err)
		}
		nowCtrl = nowCtrl.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcCtrl, trackerCtrl.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 81, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, nowCtrl), nowCtrl)
		nowCtrl = nowCtrl.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcCtrl, trackerCtrl.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 82, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, nowCtrl), nowCtrl)

		// Old pre-disarm session stays revoked, while fresh Engage at post-disarm seq=82 succeeds!
		ce = nil
		if _, err := svcCtrl.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "sess-ctrl-old",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      1.2,
		}); !errors.As(err, &ce) {
			t.Fatal("expected pre-disarm session renewal after disarm+re-arm to be rejected")
		}
		tgtCtrl82, _ := trackerCtrl.ActuationTarget(1)
		if res, err := svcCtrl.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "sess-ctrl-fresh",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.9,
			Engage:               true,
			ObservedMs:           tgtCtrl82.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgtCtrl82.ObservationRev,
			AuthorityIncarnation: tgtCtrl82.AuthorityIncarnation,
			AuthorityEpoch:       tgtCtrl82.AuthorityEpoch,
		}); err != nil || !res.LeaseActive {
			t.Fatalf("fresh engage after post-disarm seq=82 = (%+v, %v), want LeaseActive=true", res, err)
		}
	})

	t.Run("four_historical_disarmed_heartbeats_leave_timed_out_disarm_indeterminate_until_fresh_evidence", func(t *testing.T) {
		now := time.Date(2026, 11, 28, 4, 2, 0, 0, time.UTC)
		tracker, _ := mavlink.NewTracker("boxbotix", "bbops")
		sender := &fakeFrameSender{}
		store := newFakeCommandStore()
		svc, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   sender,
			Store:    store,
			Now:      func() time.Time { return now },
		})
		peer := netip.MustParseAddrPort("127.0.0.1:20302")

		// Vehicle reports Disarmed 4 times (seq=1..4), then Armed (seq=5), then wraps within 512 packets.
		for s := uint8(1); s <= 4; s++ {
			now = now.Add(5 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, s, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		}
		now = now.Add(5 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 5, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		seq := uint8(5)
		for i := 0; i < 251; i++ {
			seq++
			now = now.Add(2 * time.Millisecond)
			_ = tracker.IngestFrame(mavcodec.Frame{Sequence: seq, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
		}

		// Issue Disarm and let it time out to PhaseCommitUnknown.
		now = now.Add(5 * time.Millisecond)
		disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm: %v", err)
		}
		now = now.Add(2 * time.Second)
		svc.Advance(context.Background(), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("timed-out Disarm phase = %q, want %q", ent.PhaseState, PhaseCommitUnknown)
		}

		// Delayed copies of all 4 pre-arm disarmed heartbeats (seq=1..4) inside the 512-packet reorder window
		// MUST ALL leave Disarm in PhaseCommitUnknown (including the 4th copy!).
		for copySeq := uint8(1); copySeq <= 4; copySeq++ {
			now = now.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, copySeq, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			svc.Advance(context.Background(), now)
			if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
				t.Fatalf("historical disarmed copy #%d (seq=%d) falsely confirmed Disarm: %+v", copySeq, copySeq, ent)
			}
		}

		// Comparison: genuinely fresh disarmed heartbeat at seq=6 (never disarmed in prior cycle) confirms Disarm!
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 6, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
		svc.Advance(context.Background(), now)
		if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("fresh unseen disarmed heartbeat seq=6 phase = %q, want %q", ent.PhaseState, PhaseConfirmed)
		}
	})

	t.Run("all_non_sitl_link_tiers_block_sitl_only_commands_with_zero_frames_during_gap", func(t *testing.T) {
		tiers := []struct {
			name     string
			tierByte mavcodec.LinkTier
		}{
			{name: "direct_bench", tierByte: mavcodec.LinkTierDirectBench},
			{name: "mesh_high_rate", tierByte: mavcodec.LinkTierMeshHighRate},
			{name: "mesh_supervisory", tierByte: mavcodec.LinkTierMeshSupervisory},
		}
		for _, tc := range tiers {
			t.Run(tc.name, func(t *testing.T) {
				now := time.Date(2026, 11, 28, 4, 3, 0, 0, time.UTC)
				tracker, _ := mavlink.NewTracker("boxbotix", "bbops")
				sender := &fakeFrameSender{}
				svc, _ := NewService(Options{
					Org:      "boxbotix",
					Platform: "bbops",
					Targets:  tracker,
					Sender:   sender,
					Store:    newFakeCommandStore(),
					Now:      func() time.Time { return now },
				})
				peer := netip.MustParseAddrPort("127.0.0.1:20303")

				// Confirmed SITL-local status at seq=1, followed by armed companion-teleop gap HB at seq=90 with non-SITL tier.
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
				now = now.Add(20 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 90, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, tc.tierByte), peer, now), now)

				tgt, ok := tracker.ActuationTarget(1)
				var ce *ControlError
				if vErr := validateCommandTarget(1, ActionSITLCRSFOverride, true, tgt, ok); !errors.As(vErr, &ce) || ce.Code != "sitl_only_action" {
					t.Fatalf("%s: validateCommandTarget = %v, want 403 sitl_only_action (target=%+v)", tc.name, vErr, tgt)
				}
				ce = nil
				if _, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); !errors.As(err, &ce) || ce.Code != "sitl_only_action" {
					t.Fatalf("%s: SubmitCommand SITL action = %v, want 403 sitl_only_action", tc.name, err)
				}
				if got := sender.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
					t.Fatalf("%s: SITL-only dispatch emitted %d #76 frames during uncertain tier, want 0", tc.name, got)
				}
			})
		}
	})

	t.Run("older_second_gap_pair_keeps_teleop_and_sitl_only_commands_blocked_until_newer_corroboration", func(t *testing.T) {
		countNonZero := func(s *fakeFrameSender) int {
			n := 0
			for _, rec := range s.snapshot() {
				if sp, ok := rec.frame.Message.(mavcodec.SetPositionTargetLocalNED); ok &&
					(sp.Vx != 0 || sp.Vy != 0 || sp.Vz != 0 || sp.YawRate != 0) {
					n++
				}
			}
			return n
		}

		// Case A (teleop): Active lease at seq=10 -> disarmed HB at seq=160 -> delayed attitude at seq=11 ->
		// older armed+SITL gap pair at seq=80/81 MUST keep existing session renewal AND fresh engage blocked.
		now := time.Date(2026, 11, 28, 4, 10, 0, 0, time.UTC)
		tracker, _ := mavlink.NewTracker("boxbotix", "bbops")
		sender := &fakeFrameSender{}
		svc, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  tracker,
			Sender:   sender,
			Store:    newFakeCommandStore(),
			Now:      func() time.Time { return now },
		})
		peer := netip.MustParseAddrPort("127.0.0.1:20310")

		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
		tgt10, _ := tracker.ActuationTarget(1)
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "sess-gap-active",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.3,
			Engage:               true,
			ObservedMs:           tgt10.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt10.ObservationRev,
			AuthorityIncarnation: tgt10.AuthorityIncarnation,
			AuthorityEpoch:       tgt10.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("initial engage at seq=10: %v", err)
		}

		now = now.Add(15 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 160, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{Sequence: 11, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 80, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
		now = now.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{Sequence: 81, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)

		beforeNonZero := countNonZero(sender)
		var ce *ControlError
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "sess-gap-active",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      1.3,
		}); !errors.As(err, &ce) || ce.Code != "vehicle_disarmed" {
			t.Fatalf("renewal after older gap pair 80/81 = %v, want 409 vehicle_disarmed", err)
		}
		tgt81, _ := tracker.ActuationTarget(1)
		ce = nil
		if _, err := svc.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "sess-gap-fresh-81",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.1,
			Engage:               true,
			ObservedMs:           tgt81.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt81.ObservationRev,
			AuthorityIncarnation: tgt81.AuthorityIncarnation,
			AuthorityEpoch:       tgt81.AuthorityEpoch,
		}); !errors.As(err, &ce) || ce.Code != "vehicle_disarmed" {
			t.Fatalf("fresh engage after older gap pair 80/81 = %v, want 409 vehicle_disarmed", err)
		}
		if got := countNonZero(sender); got != beforeNonZero {
			t.Fatalf("older gap pair 80/81 emitted nonzero #84 frames: got %d, want %d", got, beforeNonZero)
		}

		// Positive control (teleop): identical packet history 10 -> 160 -> 11 -> 80 -> 81 followed by genuinely
		// newer armed+SITL pair 161/162 restores motion eligibility for both an active session and fresh engage.
		nowCtrl := time.Date(2026, 11, 28, 4, 11, 0, 0, time.UTC)
		trackerCtrl, _ := mavlink.NewTracker("boxbotix", "bbops")
		senderCtrl := &fakeFrameSender{}
		svcCtrl, _ := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerCtrl,
			Sender:   senderCtrl,
			Store:    newFakeCommandStore(),
			Now:      func() time.Time { return nowCtrl },
		})
		deliverTrackerOutcome(svcCtrl, trackerCtrl.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, nowCtrl), nowCtrl)
		tgtCtrl10, _ := trackerCtrl.ActuationTarget(1)
		if _, err := svcCtrl.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "sess-gap-ctrl",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.3,
			Engage:               true,
			ObservedMs:           tgtCtrl10.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgtCtrl10.ObservationRev,
			AuthorityIncarnation: tgtCtrl10.AuthorityIncarnation,
			AuthorityEpoch:       tgtCtrl10.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("ctrl engage at seq=10: %v", err)
		}
		nowCtrl = nowCtrl.Add(15 * time.Millisecond)
		deliverTrackerOutcome(svcCtrl, trackerCtrl.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 160, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, nowCtrl), nowCtrl)
		nowCtrl = nowCtrl.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcCtrl, trackerCtrl.IngestFrame(mavcodec.Frame{Sequence: 11, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, nowCtrl), nowCtrl)
		nowCtrl = nowCtrl.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcCtrl, trackerCtrl.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 80, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, nowCtrl), nowCtrl)
		nowCtrl = nowCtrl.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcCtrl, trackerCtrl.IngestFrame(mavcodec.Frame{Sequence: 81, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, nowCtrl), nowCtrl)
		nowCtrl = nowCtrl.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcCtrl, trackerCtrl.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 161, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, nowCtrl), nowCtrl)
		nowCtrl = nowCtrl.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcCtrl, trackerCtrl.IngestFrame(mavcodec.Frame{Sequence: 162, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, nowCtrl), nowCtrl)

		beforeCtrlRenew := countNonZero(senderCtrl)
		if res, err := svcCtrl.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:   1,
			Session: "sess-gap-ctrl",
			Seq:     2,
			Frame:   FrameLocalNED,
			Vx:      1.6,
		}); err != nil || !res.LeaseActive || countNonZero(senderCtrl) != beforeCtrlRenew+1 {
			t.Fatalf("ctrl renewal after newer gap pair 161/162 = (%+v, %v), nonzero=%d want %d",
				res, err, countNonZero(senderCtrl), beforeCtrlRenew+1)
		}

		// Case B (tier restriction): Confirmed sitl-local at seq=10 -> pending non-SITL tier at seq=160 ->
		// delayed attitude at seq=11 -> older sitl-local pair at seq=80/81 MUST keep SITL-only actions blocked;
		// genuinely newer sitl-local pair at seq=161/162 restores SITL-only eligibility.
		for _, tierByte := range []mavcodec.LinkTier{
			mavcodec.LinkTierDirectBench,
			mavcodec.LinkTierMeshHighRate,
			mavcodec.LinkTierMeshSupervisory,
		} {
			nowTier := time.Date(2026, 11, 28, 4, 12, 0, 0, time.UTC)
			trackerTier, _ := mavlink.NewTracker("boxbotix", "bbops")
			senderTier := &fakeFrameSender{}
			svcTier, _ := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  trackerTier,
				Sender:   senderTier,
				Store:    newFakeCommandStore(),
				Now:      func() time.Time { return nowTier },
			})
			deliverTrackerOutcome(svcTier, trackerTier.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, nowTier), nowTier)
			nowTier = nowTier.Add(15 * time.Millisecond)
			deliverTrackerOutcome(svcTier, trackerTier.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 160, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, tierByte), peer, nowTier), nowTier)
			nowTier = nowTier.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svcTier, trackerTier.IngestFrame(mavcodec.Frame{Sequence: 11, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, nowTier), nowTier)
			nowTier = nowTier.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svcTier, trackerTier.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 80, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, nowTier), nowTier)
			nowTier = nowTier.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svcTier, trackerTier.IngestFrame(mavcodec.Frame{Sequence: 81, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, nowTier), nowTier)

			tgtTier81, okTier81 := trackerTier.ActuationTarget(1)
			ce = nil
			if vErr := validateCommandTarget(1, ActionSITLCRSFOverride, true, tgtTier81, okTier81); !errors.As(vErr, &ce) || ce.Code != "sitl_only_action" {
				t.Fatalf("tier=%d validateCommandTarget after older 80/81 = %v, want 403 sitl_only_action (target=%+v)", tierByte, vErr, tgtTier81)
			}
			ce = nil
			if _, err := svcTier.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); !errors.As(err, &ce) || ce.Code != "sitl_only_action" {
				t.Fatalf("tier=%d SubmitCommand after older 80/81 = %v, want 403 sitl_only_action", tierByte, err)
			}
			if got := senderTier.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
				t.Fatalf("tier=%d older 80/81 allowed %d #76 frames, want 0", tierByte, got)
			}

			nowTier = nowTier.Add(15 * time.Millisecond)
			deliverTrackerOutcome(svcTier, trackerTier.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 161, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, nowTier), nowTier)
			nowTier = nowTier.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svcTier, trackerTier.IngestFrame(mavcodec.Frame{Sequence: 162, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, nowTier), nowTier)
			if _, err := svcTier.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); err != nil {
				t.Fatalf("tier=%d SubmitCommand after newer 161/162: %v", tierByte, err)
			}
			if got := senderTier.countByMsgID(mavcodec.MsgIDCommandLong); got != 1 {
				t.Fatalf("tier=%d newer 161/162 #76 count = %d, want 1", tierByte, got)
			}
		}
	})

	t.Run("uncorroborated_preempt_heartbeat_across_non_sitl_tiers_blocks_sitl_only_commands_until_corroborated", func(t *testing.T) {
		tiers := []struct {
			name     string
			tierByte mavcodec.LinkTier
		}{
			{name: "direct_bench", tierByte: mavcodec.LinkTierDirectBench},
			{name: "mesh_high_rate", tierByte: mavcodec.LinkTierMeshHighRate},
			{name: "mesh_supervisory", tierByte: mavcodec.LinkTierMeshSupervisory},
		}
		authorities := []struct {
			name string
			auth mavcodec.ControlAuthority
		}{
			{name: "crsf_override", auth: mavcodec.AuthorityDirectCRSFOverride},
			{name: "mcu_failsafe", auth: mavcodec.AuthorityMCUFailsafe},
		}
		for _, tc := range tiers {
			for _, ac := range authorities {
				t.Run(tc.name+"_"+ac.name, func(t *testing.T) {
					now := time.Date(2026, 11, 28, 4, 15, 0, 0, time.UTC)
					tracker, _ := mavlink.NewTracker("boxbotix", "bbops")
					sender := &fakeFrameSender{}
					svc, _ := NewService(Options{
						Org:      "boxbotix",
						Platform: "bbops",
						Targets:  tracker,
						Sender:   sender,
						Store:    newFakeCommandStore(),
						Now:      func() time.Time { return now },
					})
					peer := netip.MustParseAddrPort("127.0.0.1:20311")

					deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, tc.tierByte), peer, now), now)
					now = now.Add(20 * time.Millisecond)
					deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 90, true, mavcodec.ModeHoldPosition, ac.auth, mavcodec.LinkTierSITLLocal), peer, now), now)

					tgt, ok := tracker.ActuationTarget(1)
					if !ok || tgt.HeartbeatOrder != 0 {
						t.Fatalf("expected uncorroborated HeartbeatOrder=0, got %+v (ok=%v)", tgt, ok)
					}
					var ce *ControlError
					if vErr := validateCommandTarget(1, ActionHoldPosition, false, tgt, ok); !errors.As(vErr, &ce) || ce.Code != "authority_preempted" {
						t.Fatalf("validateCommandTarget regular action = %v, want 409 authority_preempted", vErr)
					}
					ce = nil
					if vErr := validateCommandTarget(1, ActionSITLCRSFOverride, true, tgt, ok); !errors.As(vErr, &ce) || ce.Code != "sitl_only_action" {
						t.Fatalf("validateCommandTarget SITL action = %v, want 403 sitl_only_action (target=%+v)", vErr, tgt)
					}
					ce = nil
					if _, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); !errors.As(err, &ce) || ce.Code != "sitl_only_action" {
						t.Fatalf("SubmitCommand SITL action during uncorroborated preempt = %v, want 403 sitl_only_action", err)
					}
					if got := sender.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
						t.Fatalf("uncorroborated preempt emitted %d #76 frames, want 0", got)
					}

					// Corroborate seq=90 with consecutive packet seq=91 -> SITL-only action now permitted.
					now = now.Add(15 * time.Millisecond)
					deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{Sequence: 91, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)
					if _, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); err != nil {
						t.Fatalf("SubmitCommand SITL action after corroboration at seq=91: %v", err)
					}
					if got := sender.countByMsgID(mavcodec.MsgIDCommandLong); got != 1 {
						t.Fatalf("corroborated SITL action #76 count = %d, want 1", got)
					}
				})
			}
		}
	})

	t.Run("non_heartbeat_link_restore_blocks_sitl_only_commands_and_teleop_until_current_link_heartbeat", func(t *testing.T) {
		runRestoredHistory := func(t *testing.T) (*mavlink.Tracker, *Service, *fakeFrameSender, netip.AddrPort, *time.Time) {
			t.Helper()
			now := time.Date(2026, 11, 28, 4, 20, 0, 0, time.UTC)
			tracker, err := mavlink.NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
			if err != nil {
				t.Fatalf("NewTrackerWithTimeouts: %v", err)
			}
			sender := &fakeFrameSender{}
			svc, _ := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   sender,
				Store:    newFakeCommandStore(),
				Now:      func() time.Time { return now },
			})
			peer := netip.MustParseAddrPort("127.0.0.1:20312")

			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
			now = now.Add(20 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 80, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, now), now)

			now = now.Add(3100 * time.Millisecond)
			lostEvents := tracker.EvaluateSilence(now)
			if len(lostEvents) != 1 || lostEvents[0].Kind != mavlink.EventKindLinkLost {
				t.Fatalf("EvaluateSilence events = %+v, want single link_lost", lostEvents)
			}
			for _, ev := range lostEvents {
				svc.OnTransitionEvent(ev)
			}

			now = now.Add(20 * time.Millisecond)
			restoreOut := tracker.IngestFrame(mavcodec.Frame{Sequence: 81, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now)
			if len(restoreOut.Events) != 1 || restoreOut.Events[0].Kind != mavlink.EventKindLinkRestored {
				t.Fatalf("attitude restore events = %+v, want single link_restored", restoreOut.Events)
			}
			deliverTrackerOutcome(svc, restoreOut, now)
			return tracker, svc, sender, peer, &now
		}

		// Branch A: Non-heartbeat link restoration rejects SITL-only command and teleop engage, and
		// a subsequent direct-bench current-link heartbeat continues to reject SITL-only commands.
		trackerBench, svcBench, senderBench, peerBench, nowBench := runRestoredHistory(t)
		tgtRestored, okRestored := trackerBench.ActuationTarget(1)
		var ce *ControlError
		if vErr := validateCommandTarget(1, ActionSITLCRSFOverride, true, tgtRestored, okRestored); !errors.As(vErr, &ce) || ce.Code != "sitl_only_action" {
			t.Fatalf("validateCommandTarget after attitude link_restored = %v, want 403 sitl_only_action (target=%+v)", vErr, tgtRestored)
		}
		ce = nil
		if _, err := svcBench.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); !errors.As(err, &ce) || ce.Code != "sitl_only_action" {
			t.Fatalf("SubmitCommand SITL action after attitude link_restored = %v, want 403 sitl_only_action", err)
		}
		ce = nil
		if _, err := svcBench.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "sess-after-att-restore",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.2,
			Engage:               true,
			ObservedMs:           tgtRestored.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgtRestored.ObservationRev,
			AuthorityIncarnation: tgtRestored.AuthorityIncarnation,
			AuthorityEpoch:       tgtRestored.AuthorityEpoch,
		}); !errors.As(err, &ce) {
			t.Fatal("expected SubmitTeleop Engage after attitude-only link_restored to be rejected until current-link heartbeat")
		}
		if got := senderBench.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
			t.Fatalf("attitude-only link_restored emitted %d #76 frames, want 0", got)
		}

		*nowBench = nowBench.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcBench, trackerBench.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 82, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peerBench, *nowBench), *nowBench)
		ce = nil
		if _, err := svcBench.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); !errors.As(err, &ce) || ce.Code != "sitl_only_action" {
			t.Fatalf("SubmitCommand SITL action after fresh direct-bench heartbeat = %v, want 403 sitl_only_action", err)
		}
		if got := senderBench.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
			t.Fatalf("fresh direct-bench heartbeat emitted %d #76 frames, want 0", got)
		}

		// Branch B: Same pre-heartbeat restoration history, followed by a fresh sitl-local heartbeat,
		// confirms sitl-local on the restored link and permits the SITL-only command.
		trackerSITL, svcSITL, senderSITL, peerSITL, nowSITL := runRestoredHistory(t)
		*nowSITL = nowSITL.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcSITL, trackerSITL.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 82, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerSITL, *nowSITL), *nowSITL)
		if _, err := svcSITL.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); err != nil {
			t.Fatalf("SubmitCommand SITL action after fresh sitl-local heartbeat: %v", err)
		}
		if got := senderSITL.countByMsgID(mavcodec.MsgIDCommandLong); got != 1 {
			t.Fatalf("fresh sitl-local heartbeat #76 count = %d, want 1", got)
		}
	})

	t.Run("pre_command_held_disarmed_heartbeat_promoted_after_timeout_cannot_confirm_disarm", func(t *testing.T) {
		newTimedOutDisarmWithPreCmdGapHB := func(t *testing.T, withPreCmdGapHB bool) (*mavlink.Tracker, *Service, *fakeCommandStore, netip.AddrPort, string, *time.Time) {
			t.Helper()
			now := time.Date(2026, 11, 28, 5, 0, 0, 0, time.UTC)
			tracker, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker: %v", err)
			}
			store := newFakeCommandStore()
			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   &fakeFrameSender{},
				Store:    store,
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			peer := netip.MustParseAddrPort("127.0.0.1:20401")
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now), now)

			if withPreCmdGapHB {
				// Disarmed gap HB arrives 10ms BEFORE Disarm is submitted and stays held in pendingGap.
				now = now.Add(10 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 80, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now), now)
			}

			now = now.Add(10 * time.Millisecond)
			disarmRes, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand Disarm: %v", err)
			}
			now = now.Add(2 * time.Second)
			svc.Advance(context.Background(), now)
			if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
				t.Fatalf("Disarm at timeout = %+v, want PhaseCommitUnknown", ent)
			}
			return tracker, svc, store, peer, disarmRes.CommandID, &now
		}

		// Branch A: AttitudeQuaternion after timeout promotes the pre-command held disarmed HB (seq=80),
		// which MUST NOT reconcile Disarm to PhaseConfirmed.
		trPre, svcPre, storePre, peerPre, cmdPre, nowPre := newTimedOutDisarmWithPreCmdGapHB(t, true)
		*nowPre = nowPre.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcPre, trPre.IngestFrame(mavcodec.Frame{
			Sequence:    81,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message:     mavcodec.AttitudeQuaternion{Q1: 1},
		}, peerPre, *nowPre), *nowPre)
		svcPre.Advance(context.Background(), *nowPre)
		if ent, _ := storePre.get(cmdPre); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm after deferred promotion of pre-command disarmed HB = %+v, want PhaseCommitUnknown", ent)
		}

		// Branch B: Attitude-only control without any post-timeout heartbeat also stays PhaseCommitUnknown.
		trAtt, svcAtt, storeAtt, peerAtt, cmdAtt, nowAtt := newTimedOutDisarmWithPreCmdGapHB(t, false)
		*nowAtt = nowAtt.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcAtt, trAtt.IngestFrame(mavcodec.Frame{
			Sequence:    2,
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Message:     mavcodec.AttitudeQuaternion{Q1: 1},
		}, peerAtt, *nowAtt), *nowAtt)
		svcAtt.Advance(context.Background(), *nowAtt)
		if ent, _ := storeAtt.get(cmdAtt); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("Disarm after attitude-only frame = %+v, want PhaseCommitUnknown", ent)
		}

		// Branch C: Genuinely new disarmed heartbeat received AFTER timeout reconciles Disarm to PhaseConfirmed.
		*nowPre = nowPre.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcPre, trPre.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 82, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peerPre, *nowPre), *nowPre)
		svcPre.Advance(context.Background(), *nowPre)
		if ent, _ := storePre.get(cmdPre); ent.PhaseState != PhaseConfirmed || ent.AckResult != UnacknowledgedResult {
			t.Fatalf("Disarm after post-timeout disarmed HB = %+v, want PhaseConfirmed AckResult=-1", ent)
		}
	})

	t.Run("pre_loss_sitl_heartbeat_copy_after_reconnect_cannot_grant_sitl_only_command_permission", func(t *testing.T) {
		newLostBenchHarness := func(t *testing.T) (*mavlink.Tracker, *Service, *fakeFrameSender, netip.AddrPort, *time.Time) {
			t.Helper()
			now := time.Date(2026, 11, 28, 5, 10, 0, 0, time.UTC)
			tracker, err := mavlink.NewTrackerWithTimeouts("boxbotix", "bbops", 3*time.Second, 10*time.Second)
			if err != nil {
				t.Fatalf("NewTrackerWithTimeouts: %v", err)
			}
			sender := &fakeFrameSender{}
			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   sender,
				Store:    newFakeCommandStore(),
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			peer := netip.MustParseAddrPort("127.0.0.1:20402")
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
			now = now.Add(20 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 11, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, now), now)

			now = now.Add(3100 * time.Millisecond)
			for _, ev := range tracker.EvaluateSilence(now) {
				svc.OnTransitionEvent(ev)
			}
			return tracker, svc, sender, peer, &now
		}

		// Branch A: Direct reconnect with exact copy of pre-loss (seq=10, sitl-local) on same endpoint.
		trDirect, svcDirect, senderDirect, peerDirect, nowDirect := newLostBenchHarness(t)
		*nowDirect = nowDirect.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcDirect, trDirect.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerDirect, *nowDirect), *nowDirect)
		var ce *ControlError
		if _, err := svcDirect.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); !errors.As(err, &ce) || ce.Code != "sitl_only_action" {
			t.Fatalf("SubmitCommand SITL action after direct pre-loss SITL HB replay = %v, want 403 sitl_only_action", err)
		}
		if got := senderDirect.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
			t.Fatalf("direct pre-loss SITL HB replay emitted %d #76 frames, want 0", got)
		}

		// Branch B: AttitudeQuaternion restores link first, then exact copy of pre-loss (seq=10, sitl-local) arrives.
		trAtt, svcAtt, senderAtt, peerAtt, nowAtt := newLostBenchHarness(t)
		*nowAtt = nowAtt.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcAtt, trAtt.IngestFrame(mavcodec.Frame{Sequence: 9, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peerAtt, *nowAtt), *nowAtt)
		*nowAtt = nowAtt.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcAtt, trAtt.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerAtt, *nowAtt), *nowAtt)
		ce = nil
		if _, err := svcAtt.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); !errors.As(err, &ce) || ce.Code != "sitl_only_action" {
			t.Fatalf("SubmitCommand SITL action after post-attitude pre-loss SITL HB replay = %v, want 403 sitl_only_action", err)
		}
		if got := senderAtt.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
			t.Fatalf("post-attitude pre-loss SITL HB replay emitted %d #76 frames, want 0", got)
		}

		// Branch C: Fresh direct-bench heartbeat (seq=12) confirms direct-bench and keeps SITL action blocked.
		*nowDirect = nowDirect.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcDirect, trDirect.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 12, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peerDirect, *nowDirect), *nowDirect)
		ce = nil
		if _, err := svcDirect.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); !errors.As(err, &ce) || ce.Code != "sitl_only_action" {
			t.Fatalf("SubmitCommand SITL action after fresh direct-bench HB = %v, want 403 sitl_only_action", err)
		}

		// Branch D: Fresh sitl-local heartbeat (seq=12) confirms sitl-local on restored link and permits SITL action.
		*nowAtt = nowAtt.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcAtt, trAtt.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 12, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerAtt, *nowAtt), *nowAtt)
		if _, err := svcAtt.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride, Param1: 1}); err != nil {
			t.Fatalf("SubmitCommand SITL action after fresh sitl-local HB: %v", err)
		}
		if got := senderAtt.countByMsgID(mavcodec.MsgIDCommandLong); got != 1 {
			t.Fatalf("fresh sitl-local HB #76 count = %d, want 1", got)
		}
	})

	t.Run("unconfirmed_disarmed_mcu_failsafe_heartbeat_retains_hold_restriction_until_corroborated", func(t *testing.T) {
		newArmedFailsafeHarness := func(t *testing.T) (*mavlink.Tracker, *Service, *fakeFrameSender, netip.AddrPort, *time.Time) {
			t.Helper()
			now := time.Date(2026, 11, 28, 5, 20, 0, 0, time.UTC)
			tracker, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker: %v", err)
			}
			sender := &fakeFrameSender{}
			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   sender,
				Store:    newFakeCommandStore(),
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			peer := netip.MustParseAddrPort("127.0.0.1:20403")
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityMCUFailsafe), peer, now), now)
			return tracker, svc, sender, peer, &now
		}

		// Branch A: Confirmed armed mcu-failsafe refuses Hold -> unconfirmed gap disarmed mcu-failsafe (seq=80)
		// STILL refuses Hold while retaining emergency Disarm eligibility, until seq=81 corroborates disarm.
		trGap, svcGap, senderGap, peerGap, nowGap := newArmedFailsafeHarness(t)
		var ce *ControlError
		if _, err := svcGap.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition}); !errors.As(err, &ce) || ce.Code != "authority_preempted" {
			t.Fatalf("Hold on confirmed armed mcu-failsafe = %v, want 409 authority_preempted", err)
		}

		*nowGap = nowGap.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcGap, trGap.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 80, false, mavcodec.ModeDisarmed, mavcodec.AuthorityMCUFailsafe), peerGap, *nowGap), *nowGap)
		tgtUncorroborated, ok := trGap.ActuationTarget(1)
		if !ok || tgtUncorroborated.HeartbeatOrder != 0 {
			t.Fatalf("target after unconfirmed disarmed mcu-failsafe HB = %+v (ok=%v), want HeartbeatOrder=0", tgtUncorroborated, ok)
		}
		ce = nil
		if _, err := svcGap.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition}); !errors.As(err, &ce) || ce.Code != "authority_preempted" {
			t.Fatalf("Hold after unconfirmed disarmed mcu-failsafe HB = %v, want 409 authority_preempted", err)
		}
		if vErr := validateCommandTarget(1, ActionDisarm, false, tgtUncorroborated, ok); vErr != nil {
			t.Fatalf("emergency Disarm eligibility after unconfirmed disarmed mcu-failsafe HB = %v, want nil", vErr)
		}
		if got := senderGap.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
			t.Fatalf("unconfirmed disarmed mcu-failsafe HB emitted %d #76 frames, want 0", got)
		}

		// Corroborate seq=80 with AttitudeQuaternion(seq=81) -> Armed=false confirmed, Hold now eligible.
		*nowGap = nowGap.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcGap, trGap.IngestFrame(mavcodec.Frame{Sequence: 81, SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peerGap, *nowGap), *nowGap)
		if _, err := svcGap.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition}); err != nil {
			t.Fatalf("Hold after corroborated disarmed mcu-failsafe HB: %v", err)
		}
		if got := senderGap.countByMsgID(mavcodec.MsgIDCommandLong); got != 1 {
			t.Fatalf("corroborated disarmed mcu-failsafe HB #76 count = %d, want 1", got)
		}

		// Branch B: Uncertain mcu-failsafe HB still reporting Armed=true keeps Hold blocked.
		trArmedGap, svcArmedGap, _, peerArmedGap, nowArmedGap := newArmedFailsafeHarness(t)
		*nowArmedGap = nowArmedGap.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcArmedGap, trArmedGap.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 80, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityMCUFailsafe), peerArmedGap, *nowArmedGap), *nowArmedGap)
		ce = nil
		if _, err := svcArmedGap.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition}); !errors.As(err, &ce) || ce.Code != "authority_preempted" {
			t.Fatalf("Hold on uncertain armed mcu-failsafe HB = %v, want 409 authority_preempted", err)
		}

		// Branch C: Ordered disarmed mcu-failsafe HB (seq=2 after seq=1) commits immediately and allows Hold.
		trOrdered, svcOrdered, senderOrdered, peerOrdered, nowOrdered := newArmedFailsafeHarness(t)
		*nowOrdered = nowOrdered.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcOrdered, trOrdered.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 2, false, mavcodec.ModeDisarmed, mavcodec.AuthorityMCUFailsafe), peerOrdered, *nowOrdered), *nowOrdered)
		if _, err := svcOrdered.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionHoldPosition}); err != nil {
			t.Fatalf("Hold after ordered disarmed mcu-failsafe HB: %v", err)
		}
		if got := senderOrdered.countByMsgID(mavcodec.MsgIDCommandLong); got != 1 {
			t.Fatalf("ordered disarmed mcu-failsafe HB #76 count = %d, want 1", got)
		}
	})

	t.Run("stale_out_of_order_sitl_heartbeat_before_loss_blocks_sitl_command_on_reconnect_copy", func(t *testing.T) {
		for _, orderedPreLoss := range []bool{false, true} {
			tracker, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker: %v", err)
			}
			sender := &fakeFrameSender{}
			now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   sender,
				Store:    newFakeCommandStore(),
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			peer := netip.MustParseAddrPort("127.0.0.1:20410")

			if orderedPreLoss {
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
				now = now.Add(20 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 11, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, now), now)
			} else {
				// Out-of-order: seq=11 direct-bench arrives first, then older seq=10 sitl-local arrives stale!
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 11, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, now), now)
				now = now.Add(20 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
			}

			now = now.Add(3100 * time.Millisecond)
			for _, ev := range tracker.EvaluateSilence(now) {
				svc.OnTransitionEvent(ev)
			}

			// Reconnect carries an identical copy of pre-loss (seq=10, sitl-local): MUST NOT grant ActionSITLCRSFOverride!
			now = now.Add(20 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)

			var ce *ControlError
			if _, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride}); !errors.As(err, &ce) || ce.Status != 403 || ce.Code != "sitl_only_action" {
				t.Fatalf("orderedPreLoss=%v: ActionSITLCRSFOverride after stale seq=10 sitl-local replay = %v, want 403 sitl_only_action", orderedPreLoss, err)
			}
			if got := sender.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
				t.Fatalf("orderedPreLoss=%v: emitted %d #76 frames on stale replay, want 0", orderedPreLoss, got)
			}

			// Positive control: fresh current-link sitl-local HB (seq=12) confirms sitl-local and allows ActionSITLCRSFOverride.
			now = now.Add(20 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 12, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
			if _, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride}); err != nil {
				t.Fatalf("orderedPreLoss=%v: ActionSITLCRSFOverride after fresh seq=12 sitl-local: %v", orderedPreLoss, err)
			}
			if got := sender.countByMsgID(mavcodec.MsgIDCommandLong); got != 1 {
				t.Fatalf("orderedPreLoss=%v: fresh seq=12 #76 count = %d, want 1", orderedPreLoss, got)
			}
		}
	})

	t.Run("sustained_1hz_post_loss_heartbeats_restore_teleop_engage_before_769_packets_while_1024_single_copies_refuse_engage", func(t *testing.T) {
		newBenchHarness := func(t *testing.T, preLossCount int) (*mavlink.Tracker, *Service, *fakeFrameSender, netip.AddrPort, *time.Time) {
			t.Helper()
			tracker, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker: %v", err)
			}
			sender := &fakeFrameSender{}
			now := time.Date(2026, 11, 28, 4, 30, 0, 0, time.UTC)
			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   sender,
				Store:    newFakeCommandStore(),
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			peer := netip.MustParseAddrPort("127.0.0.1:20411")
			for i := 0; i < preLossCount; i++ {
				now = now.Add(time.Second)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, uint8(i), true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, now), now)
			}
			now = now.Add(3100 * time.Millisecond)
			for _, ev := range tracker.EvaluateSilence(now) {
				svc.OnTransitionEvent(ev)
			}
			return tracker, svc, sender, peer, &now
		}

		// Control A: Full 256-slot history -> 1 Hz consecutive heartbeats refuse engage at 512 and allow engage at 513 (well before 769).
		trFull, svcFull, senderFull, peerFull, nowFull := newBenchHarness(t, 256)
		for pkt := 1; pkt <= 513; pkt++ {
			*nowFull = nowFull.Add(time.Second)
			seq := uint8((pkt - 1) % 256)
			deliverTrackerOutcome(svcFull, trFull.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, seq, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peerFull, *nowFull), *nowFull)
			if pkt == 512 {
				tgt512, _ := trFull.ActuationTarget(1)
				var ce *ControlError
				if _, err := svcFull.SubmitTeleop(context.Background(), TeleopRequest{
					SysID:                1,
					Session:              "s-full-512",
					Seq:                  1,
					Frame:                FrameLocalNED,
					Vx:                   1.0,
					Engage:               true,
					ObservedMs:           tgt512.HeartbeatObservedUnixNs / int64(time.Millisecond),
					ObservationRev:       tgt512.ObservationRev,
					AuthorityIncarnation: tgt512.AuthorityIncarnation,
					AuthorityEpoch:       tgt512.AuthorityEpoch,
				}); !errors.As(err, &ce) || ce.Status != 409 || ce.Code != "authority_not_teleop" {
					t.Fatalf("pkt 512 teleop engage = %v, want 409 authority_not_teleop", err)
				}
			}
		}
		tgt513, _ := trFull.ActuationTarget(1)
		if _, err := svcFull.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s-full-513",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.0,
			Engage:               true,
			ObservedMs:           tgt513.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt513.ObservationRev,
			AuthorityIncarnation: tgt513.AuthorityIncarnation,
			AuthorityEpoch:       tgt513.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("pkt 513 teleop engage after 1 Hz stream: %v", err)
		}
		if got := senderFull.countByMsgID(mavcodec.MsgIDSetPositionTargetLocalNED); got != 1 {
			t.Fatalf("pkt 513 #84 count = %d, want 1", got)
		}

		// Control B: Partial 10-slot history -> refuses engage at packet 10 (seq=9) and allows engage at packet 11 (seq=10).
		trPartial, svcPartial, senderPartial, peerPartial, nowPartial := newBenchHarness(t, 10)
		for pkt := 1; pkt <= 11; pkt++ {
			*nowPartial = nowPartial.Add(time.Second)
			seq := uint8(pkt - 1)
			deliverTrackerOutcome(svcPartial, trPartial.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, seq, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peerPartial, *nowPartial), *nowPartial)
			if pkt == 10 {
				tgt10, _ := trPartial.ActuationTarget(1)
				var ce *ControlError
				if _, err := svcPartial.SubmitTeleop(context.Background(), TeleopRequest{
					SysID:                1,
					Session:              "s-part-10",
					Seq:                  1,
					Frame:                FrameLocalNED,
					Vx:                   1.0,
					Engage:               true,
					ObservedMs:           tgt10.HeartbeatObservedUnixNs / int64(time.Millisecond),
					ObservationRev:       tgt10.ObservationRev,
					AuthorityIncarnation: tgt10.AuthorityIncarnation,
					AuthorityEpoch:       tgt10.AuthorityEpoch,
				}); !errors.As(err, &ce) || ce.Status != 409 || ce.Code != "authority_not_teleop" {
					t.Fatalf("partial pkt 10 teleop engage = %v, want 409 authority_not_teleop", err)
				}
			}
		}
		tgt11, _ := trPartial.ActuationTarget(1)
		if _, err := svcPartial.SubmitTeleop(context.Background(), TeleopRequest{
			SysID:                1,
			Session:              "s-part-11",
			Seq:                  1,
			Frame:                FrameLocalNED,
			Vx:                   1.0,
			Engage:               true,
			ObservedMs:           tgt11.HeartbeatObservedUnixNs / int64(time.Millisecond),
			ObservationRev:       tgt11.ObservationRev,
			AuthorityIncarnation: tgt11.AuthorityIncarnation,
			AuthorityEpoch:       tgt11.AuthorityEpoch,
		}); err != nil {
			t.Fatalf("partial pkt 11 teleop engage: %v", err)
		}
		if got := senderPartial.countByMsgID(mavcodec.MsgIDSetPositionTargetLocalNED); got != 1 {
			t.Fatalf("partial pkt 11 #84 count = %d, want 1", got)
		}

		// Control C: 1,024 copies of a single old heartbeat (seq=5) still refuse teleop engage across both direct-bench and sitl-local.
		for _, tier := range []mavcodec.LinkTier{mavcodec.LinkTierDirectBench, mavcodec.LinkTierSITLLocal} {
			trDup, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker dup: %v", err)
			}
			senderDup := &fakeFrameSender{}
			nowDup := time.Date(2026, 11, 28, 4, 40, 0, 0, time.UTC)
			svcDup, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  trDup,
				Sender:   senderDup,
				Store:    newFakeCommandStore(),
				Now:      func() time.Time { return nowDup },
			})
			if err != nil {
				t.Fatalf("NewService dup: %v", err)
			}
			peerDup := netip.MustParseAddrPort("127.0.0.1:20411")
			for i := 0; i < 10; i++ {
				nowDup = nowDup.Add(time.Second)
				deliverTrackerOutcome(svcDup, trDup.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, uint8(i), true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, tier), peerDup, nowDup), nowDup)
			}
			nowDup = nowDup.Add(3100 * time.Millisecond)
			for _, ev := range trDup.EvaluateSilence(nowDup) {
				svcDup.OnTransitionEvent(ev)
			}
			for copyIdx := 1; copyIdx <= 1024; copyIdx++ {
				nowDup = nowDup.Add(time.Second)
				deliverTrackerOutcome(svcDup, trDup.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 5, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, tier), peerDup, nowDup), nowDup)
			}
			tgtDup, _ := trDup.ActuationTarget(1)
			var ceDup *ControlError
			if _, err := svcDup.SubmitTeleop(context.Background(), TeleopRequest{
				SysID:                1,
				Session:              "s-dup-1024",
				Seq:                  1,
				Frame:                FrameLocalNED,
				Vx:                   1.0,
				Engage:               true,
				ObservedMs:           tgtDup.HeartbeatObservedUnixNs / int64(time.Millisecond),
				ObservationRev:       tgtDup.ObservationRev,
				AuthorityIncarnation: tgtDup.AuthorityIncarnation,
				AuthorityEpoch:       tgtDup.AuthorityEpoch,
			}); !errors.As(err, &ceDup) || ceDup.Status != 409 || ceDup.Code != "authority_not_teleop" {
				t.Fatalf("tier=%v 1024 single-packet copies teleop engage = %v, want 409 authority_not_teleop", tier, err)
			}
			if got := senderDup.countByMsgID(mavcodec.MsgIDSetPositionTargetLocalNED); got != 0 {
				t.Fatalf("tier=%v 1024 single-packet copies #84 count = %d, want 0", tier, got)
			}
		}
	})

	t.Run("backward_utc_clock_after_restart_with_empty_tracker_reconciles_disarm_via_receipt_revisions_while_preserving_timestamp_fallback", func(t *testing.T) {
		peer := netip.MustParseAddrPort("127.0.0.1:20412")
		t0 := time.Date(2026, 11, 28, 5, 0, 0, 0, time.UTC)

		// Branch A (backward clock -1m on restart) and Branch B (forward clock +1m control):
		for _, clockSkew := range []time.Duration{-1 * time.Minute, 1 * time.Minute} {
			tr1, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker 1: %v", err)
			}
			store := newFakeCommandStore()
			now1 := t0
			svc1, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tr1,
				Sender:   &fakeFrameSender{},
				Store:    store,
				Now:      func() time.Time { return now1 },
			})
			if err != nil {
				t.Fatalf("NewService 1: %v", err)
			}
			deliverTrackerOutcome(svc1, tr1.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop), peer, now1), now1)
			now1 = now1.Add(10 * time.Millisecond)
			disarmRes, err := svc1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
			if err != nil {
				t.Fatalf("SubmitCommand Disarm: %v", err)
			}

			// Simulate process restart with empty Tracker and UTC clock skewed by clockSkew.
			tr2, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker 2: %v", err)
			}
			now2 := t0.Add(clockSkew)
			svc2, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tr2,
				Sender:   &fakeFrameSender{},
				Store:    store,
				Now:      func() time.Time { return now2 },
			})
			if err != nil {
				t.Fatalf("NewService 2: %v", err)
			}
			svc2.Advance(context.Background(), now2)
			if ent, _ := store.get(disarmRes.CommandID); ent.PhaseState != PhaseCommitUnknown {
				t.Fatalf("skew=%v: recovered Disarm phase before heartbeats = %q, want %q", clockSkew, ent.PhaseState, PhaseCommitUnknown)
			}

			// Current boot receives 3 disarmed heartbeats while UTC clock is still behind t0 (when clockSkew < 0).
			for seq := uint8(1); seq <= 3; seq++ {
				now2 = now2.Add(time.Second)
				deliverTrackerOutcome(svc2, tr2.IngestFrame(makeTestVehicleHeartbeatFrame(t, 1, seq, false, mavcodec.ModeDisarmed, mavcodec.AuthorityCompanionTeleop), peer, now2), now2)
			}
			svc2.Advance(context.Background(), now2)
			entAfter, ok := store.get(disarmRes.CommandID)
			if !ok || entAfter.PhaseState != PhaseConfirmed || entAfter.AckResult != -1 {
				t.Fatalf("skew=%v: Disarm after 3 current-boot disarmed heartbeats = %+v (ok=%v), want PhaseConfirmed AckResult=-1",
					clockSkew, entAfter, ok)
			}
		}

		// Branch C: TargetSource without receipt revisions (fakeTargetSource, HeartbeatReceiptRev == 0)
		// preserves the HeartbeatObservedUnixNs > unknownAtUnixNs timestamp fallback!
		fake1 := newFakeTargetSource()
		nowC1 := t0
		fake1.set(mavlink.ActuationTarget{
			SysID:                   1,
			Domain:                  uxsvocab.DomainQuadcopter,
			Armed:                   true,
			CompanionMode:           uxsvocab.ModeTeleopVelocity,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: nowC1.UnixNano(),
			HeartbeatSeq:            1,
		})
		storeC := newFakeCommandStore()
		svcC1, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  fake1,
			Sender:   &fakeFrameSender{},
			Store:    storeC,
			Now:      func() time.Time { return nowC1 },
		})
		if err != nil {
			t.Fatalf("NewService C1: %v", err)
		}
		nowC1 = nowC1.Add(10 * time.Millisecond)
		disarmC, err := svcC1.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionDisarm})
		if err != nil {
			t.Fatalf("SubmitCommand Disarm C: %v", err)
		}

		fake2 := newFakeTargetSource()
		nowC2 := t0.Add(10 * time.Second)
		svcC2, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  fake2,
			Sender:   &fakeFrameSender{},
			Store:    storeC,
			Now:      func() time.Time { return nowC2 },
		})
		if err != nil {
			t.Fatalf("NewService C2: %v", err)
		}
		svcC2.Advance(context.Background(), nowC2)

		// Without receipt revisions, a disarmed snapshot whose HeartbeatObservedUnixNs <= nowC2 stays PhaseCommitUnknown.
		fake2.set(mavlink.ActuationTarget{
			SysID:                   1,
			Domain:                  uxsvocab.DomainQuadcopter,
			Armed:                   false,
			CompanionMode:           uxsvocab.ModeDisarmed,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: t0.UnixNano(),
			HeartbeatSeq:            1,
		})
		svcC2.Advance(context.Background(), nowC2)
		if entStale, _ := storeC.get(disarmC.CommandID); entStale.PhaseState != PhaseCommitUnknown {
			t.Fatalf("provider without receipt revisions falsely confirmed on stale timestamp: %+v", entStale)
		}

		// Once HeartbeatObservedUnixNs > nowC2, timestamp fallback reconciles Disarm to PhaseConfirmed.
		nowC2 = nowC2.Add(time.Second)
		fake2.set(mavlink.ActuationTarget{
			SysID:                   1,
			Domain:                  uxsvocab.DomainQuadcopter,
			Armed:                   false,
			CompanionMode:           uxsvocab.ModeDisarmed,
			Authority:               uxsvocab.AuthorityCompanionTeleop,
			LinkTier:                uxsvocab.LinkTierSITLLocal,
			PeerAddr:                peer,
			HeartbeatObservedUnixNs: nowC2.UnixNano(),
			HeartbeatSeq:            2,
		})
		svcC2.Advance(context.Background(), nowC2)
		if entFresh, _ := storeC.get(disarmC.CommandID); entFresh.PhaseState != PhaseConfirmed {
			t.Fatalf("provider without receipt revisions after fresh timestamp = %+v, want PhaseConfirmed", entFresh)
		}
	})

	t.Run("recent_out_of_order_sitl_heartbeat_over_expired_history_slot_blocks_sitl_command_after_reconnect", func(t *testing.T) {
		for _, mode := range []string{
			"expired_slot_out_of_order",
			"recent_slot_out_of_order",
			"empty_slot_out_of_order",
			"expired_slot_in_order",
		} {
			tracker, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("%s: NewTracker: %v", mode, err)
			}
			sender := &fakeFrameSender{}
			now := time.Date(2026, 11, 28, 6, 0, 0, 0, time.UTC)
			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   sender,
				Store:    newFakeCommandStore(),
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("%s: NewService: %v", mode, err)
			}
			peer := netip.MustParseAddrPort("127.0.0.1:20511")

			switch mode {
			case "expired_slot_out_of_order":
				// Initial (seq=10, sitl-local), followed by >512 telemetry packets (~3 wraps), skipping seq=10 on the final wrap
				// so (seq=11, direct-bench) lands first, then recent out-of-order (seq=10, sitl-local) arrives right before link_lost.
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
				curSeq := uint8(10)
				for step := 1; step <= 767; step++ {
					now = now.Add(5 * time.Millisecond)
					if step == 767 {
						curSeq = 11
						deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, curSeq, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, now), now)
					} else {
						curSeq++
						deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: curSeq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)
					}
				}
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
			case "recent_slot_out_of_order":
				// Initial (seq=10, sitl-local), 1 wrap (~255 telemetry packets) to (seq=11, direct-bench), then out-of-order (seq=10, sitl-local).
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
				curSeq := uint8(10)
				for step := 1; step <= 255; step++ {
					now = now.Add(5 * time.Millisecond)
					if step == 255 {
						curSeq = 11
						deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, curSeq, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, now), now)
					} else {
						curSeq++
						deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: curSeq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)
					}
				}
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
				// Advance 300 more telemetry packets (~557 total > 1+512, <= 257+512) so only refreshing curLinkLife at wrap #2 holds seq=10 across link_lost.
				for extra := 0; extra < 300; extra++ {
					curSeq++
					now = now.Add(5 * time.Millisecond)
					deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: curSeq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)
				}
			case "empty_slot_out_of_order":
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 11, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, now), now)
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
			case "expired_slot_in_order":
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
				curSeq := uint8(10)
				for step := 1; step <= 767; step++ {
					curSeq++
					now = now.Add(5 * time.Millisecond)
					deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: curSeq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)
				}
				// curSeq == 9; send in-order (seq=10, sitl-local) then (seq=11, direct-bench).
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 11, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, now), now)
			}

			now = now.Add(3100 * time.Millisecond)
			for _, ev := range tracker.EvaluateSilence(now) {
				svc.OnTransitionEvent(ev)
			}

			// Reconnect carries a copy of recent pre-loss (seq=10, sitl-local): MUST NOT grant ActionSITLCRSFOverride!
			now = now.Add(20 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)

			var ce *ControlError
			if _, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride}); !errors.As(err, &ce) || ce.Status != 403 || ce.Code != "sitl_only_action" {
				t.Fatalf("%s: ActionSITLCRSFOverride after stale seq=10 sitl-local replay = %v, want 403 sitl_only_action", mode, err)
			}
			if got := sender.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
				t.Fatalf("%s: emitted %d #76 frames on stale replay, want 0", mode, got)
			}

			// Positive control: fresh unseen (seq=12, sitl-local) confirms sitl-local and permits ActionSITLCRSFOverride.
			now = now.Add(20 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 12, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
			if _, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride}); err != nil {
				t.Fatalf("%s: ActionSITLCRSFOverride after fresh seq=12 sitl-local: %v", mode, err)
			}
			if got := sender.countByMsgID(mavcodec.MsgIDCommandLong); got != 1 {
				t.Fatalf("%s: fresh seq=12 #76 count = %d, want 1", mode, got)
			}
		}
	})

	t.Run("sitl_override_reconciles_across_link_loss_on_new_link_epoch_without_waiting_for_old_packet_position", func(t *testing.T) {
		ctx := context.Background()

		// Subcase A: ActionSITLCRSFOverride times out at PacketOrder=150 before link_lost, then reconnects on LinkEpoch=2
		// and reconciles on confirmed matching heartbeats at PacketOrder=1..5 (<< 150).
		trackerA, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker A: %v", err)
		}
		storeA := newFakeCommandStore()
		senderA := &fakeFrameSender{}
		nowA := time.Date(2026, 11, 28, 7, 0, 0, 0, time.UTC)
		svcA, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerA,
			Sender:   senderA,
			Store:    storeA,
			Now:      func() time.Time { return nowA },
		})
		if err != nil {
			t.Fatalf("NewService A: %v", err)
		}
		peerA := netip.MustParseAddrPort("127.0.0.1:20521")

		deliverTrackerOutcome(svcA, trackerA.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerA, nowA), nowA)
		for seq := uint8(2); seq <= 150; seq++ {
			nowA = nowA.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svcA, trackerA.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peerA, nowA), nowA)
		}
		if tgtPre, _ := trackerA.ActuationTarget(1); tgtPre.PacketOrder != 150 || tgtPre.LinkEpoch != 1 {
			t.Fatalf("pre-command target A = %+v, want LinkEpoch=1 PacketOrder=150", tgtPre)
		}

		nowA = nowA.Add(10 * time.Millisecond)
		cmdA, err := svcA.SubmitCommand(ctx, CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride})
		if err != nil {
			t.Fatalf("SubmitCommand ActionSITLCRSFOverride A: %v", err)
		}

		// Timeout to PhaseCommitUnknown while still on LinkEpoch=1 at PacketOrder=150.
		nowA = nowA.Add(2 * time.Second)
		svcA.Advance(ctx, nowA)
		if ent, _ := storeA.get(cmdA.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("after timeout A = %+v, want PhaseCommitUnknown", ent)
		}

		// Link loss resets Tracker packet order to 0; ActionSITLCRSFOverride stays PhaseCommitUnknown across link_lost.
		nowA = nowA.Add(3100 * time.Millisecond)
		for _, ev := range trackerA.EvaluateSilence(nowA) {
			svcA.OnTransitionEvent(ev)
		}
		svcA.Advance(ctx, nowA)
		if ent, _ := storeA.get(cmdA.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("after link_lost A = %+v, want PhaseCommitUnknown", ent)
		}

		// Reconnect on LinkEpoch=2 with 5 confirmed direct-crsf-override heartbeats (PacketOrder=1..5 << 150).
		for seq := uint8(1); seq <= 5; seq++ {
			nowA = nowA.Add(time.Second)
			deliverTrackerOutcome(svcA, trackerA.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, seq, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerA, nowA), nowA)
			svcA.Advance(ctx, nowA)
		}
		if tgtPost, _ := trackerA.ActuationTarget(1); tgtPost.LinkEpoch <= 1 || tgtPost.PacketOrder != 5 || tgtPost.CurrentLinkHbUnconfirmed {
			t.Fatalf("post-reconnect target A = %+v, want confirmed LinkEpoch>1 PacketOrder=5", tgtPost)
		}
		if ent, _ := storeA.get(cmdA.CommandID); ent.PhaseState != PhaseConfirmed || ent.AckResult != UnacknowledgedResult {
			t.Fatalf("after 5 confirmed direct-crsf-override heartbeats on LinkEpoch=2 (PacketOrder=5 << 150) = %+v, want PhaseConfirmed AckResult=-1", ent)
		}

		// Subcase B: Negative controls on new link epoch — neither a held pre-loss replay nor an uncorroborated gap
		// (HeartbeatOrder == 0 on an already-confirmed new link epoch) reconciles ActionSITLCRSFOverride prematurely.
		trackerB, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker B: %v", err)
		}
		storeB := newFakeCommandStore()
		nowB := time.Date(2026, 11, 28, 7, 10, 0, 0, time.UTC)
		svcB, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerB,
			Sender:   &fakeFrameSender{},
			Store:    storeB,
			Now:      func() time.Time { return nowB },
		})
		if err != nil {
			t.Fatalf("NewService B: %v", err)
		}
		peerB := netip.MustParseAddrPort("127.0.0.1:20522")

		// Pre-loss history includes (seq=10, direct-crsf-override, sitl-local), followed by (seq=11, companion-teleop, sitl-local) + 139 frames.
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerB, nowB), nowB)
		nowB = nowB.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 11, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerB, nowB), nowB)
		for seq := uint8(12); seq <= 150; seq++ {
			nowB = nowB.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svcB, trackerB.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peerB, nowB), nowB)
		}
		nowB = nowB.Add(10 * time.Millisecond)
		cmdB, err := svcB.SubmitCommand(ctx, CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride})
		if err != nil {
			t.Fatalf("SubmitCommand ActionSITLCRSFOverride B: %v", err)
		}
		nowB = nowB.Add(2 * time.Second)
		svcB.Advance(ctx, nowB)
		nowB = nowB.Add(3100 * time.Millisecond)
		for _, ev := range trackerB.EvaluateSilence(nowB) {
			svcB.OnTransitionEvent(ev)
		}

		// Negative control B1: held pre-loss replay of (seq=10, direct-crsf-override) after reconnect MUST NOT reconcile!
		nowB = nowB.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerB, nowB), nowB)
		svcB.Advance(ctx, nowB)
		if ent, _ := storeB.get(cmdB.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("held pre-loss seq=10 direct-crsf-override falsely reconciled ActionSITLCRSFOverride: %+v", ent)
		}

		// Confirm LinkEpoch=2 with non-matching (seq=20, companion-teleop).
		nowB = nowB.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 20, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerB, nowB), nowB)
		svcB.Advance(ctx, nowB)
		if ent, _ := storeB.get(cmdB.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("non-matching seq=20 companion-teleop falsely reconciled ActionSITLCRSFOverride: %+v", ent)
		}

		// Negative control B2: uncorroborated gap (seq=100, delta=80 > 64) on confirmed LinkEpoch=2 sets Authority=direct-crsf-override
		// with HeartbeatOrder==0 — MUST NOT reconcile until corroborated!
		nowB = nowB.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 100, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerB, nowB), nowB)
		if tgtGap, _ := trackerB.ActuationTarget(1); tgtGap.CurrentLinkHbUnconfirmed || tgtGap.HeartbeatOrder != 0 || tgtGap.Authority != uxsvocab.AuthorityDirectCRSFOverride {
			t.Fatalf("expected uncorroborated gap HeartbeatOrder=0 on confirmed LinkEpoch=2, got %+v", tgtGap)
		}
		svcB.Advance(ctx, nowB)
		if ent, _ := storeB.get(cmdB.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("uncorroborated gap HeartbeatOrder=0 falsely reconciled ActionSITLCRSFOverride: %+v", ent)
		}

		// Corroborating next packet (seq=101, PacketOrder << 150) promotes HeartbeatOrder > 0 and reconciles to PhaseConfirmed.
		nowB = nowB.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 101, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerB, nowB), nowB)
		svcB.Advance(ctx, nowB)
		if ent, _ := storeB.get(cmdB.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("after corroborating seq=101 on LinkEpoch=2 = %+v, want PhaseConfirmed", ent)
		}

		// Subcase C: Link loss + reconnect onto LinkEpoch=2 (PacketOrder=2 << 150) BEFORE CommandAckTimeout expires.
		trackerC, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker C: %v", err)
		}
		storeC := newFakeCommandStore()
		nowC := time.Date(2026, 11, 28, 7, 20, 0, 0, time.UTC)
		svcC, err := NewService(Options{
			Org:               "boxbotix",
			Platform:          "bbops",
			Targets:           trackerC,
			Sender:            &fakeFrameSender{},
			Store:             storeC,
			CommandAckTimeout: 5 * time.Second,
			Now:               func() time.Time { return nowC },
		})
		if err != nil {
			t.Fatalf("NewService C: %v", err)
		}
		peerC := netip.MustParseAddrPort("127.0.0.1:20523")

		deliverTrackerOutcome(svcC, trackerC.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerC, nowC), nowC)
		for seq := uint8(2); seq <= 150; seq++ {
			nowC = nowC.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svcC, trackerC.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peerC, nowC), nowC)
		}
		nowC = nowC.Add(10 * time.Millisecond)
		cmdC, err := svcC.SubmitCommand(ctx, CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride})
		if err != nil {
			t.Fatalf("SubmitCommand ActionSITLCRSFOverride C: %v", err)
		}

		// Link loss occurs at +3.1s (< 5s CommandAckTimeout), followed by reconnect on new link epoch reaching PacketOrder=10 (skipping seq=9).
		nowC = nowC.Add(3100 * time.Millisecond)
		for _, ev := range trackerC.EvaluateSilence(nowC) {
			svcC.OnTransitionEvent(ev)
		}
		nowC = nowC.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcC, trackerC.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerC, nowC), nowC)
		for _, seq := range []uint8{2, 3, 4, 5, 6, 7, 8, 10} {
			nowC = nowC.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svcC, trackerC.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peerC, nowC), nowC)
		}

		// Advance past 5s CommandAckTimeout while on new link epoch at PacketOrder=10 (<< 150).
		nowC = nowC.Add(2 * time.Second)
		svcC.Advance(ctx, nowC)
		if ent, _ := storeC.get(cmdC.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("after timeout on new link epoch C = %+v, want PhaseCommitUnknown", ent)
		}

		// Delayed pre-timeout out-of-order heartbeat (seq=9, HeartbeatOrder=9 <= 10) MUST NOT reconcile!
		nowC = nowC.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcC, trackerC.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 9, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerC, nowC), nowC)
		svcC.Advance(ctx, nowC)
		if ent, _ := storeC.get(cmdC.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("delayed pre-timeout seq=9 (HeartbeatOrder=9 <= 10) falsely reconciled ActionSITLCRSFOverride: %+v", ent)
		}

		// Fresh post-timeout confirmed heartbeat (seq=11, HeartbeatOrder=11 > 10, << 150) reconciles immediately.
		nowC = nowC.Add(time.Second)
		deliverTrackerOutcome(svcC, trackerC.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 11, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerC, nowC), nowC)
		svcC.Advance(ctx, nowC)
		if ent, _ := storeC.get(cmdC.CommandID); ent.PhaseState != PhaseConfirmed {
			t.Fatalf("after seq=11 direct-crsf-override at PacketOrder=11 on new link epoch = %+v, want PhaseConfirmed", ent)
		}
	})

	t.Run("window_crossing_sitl_heartbeat_after_packet_loss_blocks_sitl_command_for_stale_and_uncorroborated_histories", func(t *testing.T) {
		for idx, mode := range []string{"uncorroborated_after_bench_12", "stale_mono_after_bench_12", "one_pos_late_after_bench_11"} {
			tracker, err := mavlink.NewTracker("boxbotix", "bbops")
			if err != nil {
				t.Fatalf("NewTracker (%s): %v", mode, err)
			}
			sender := &fakeFrameSender{}
			now := time.Date(2026, 11, 28, 8, idx, 0, 0, time.UTC)
			svc, err := NewService(Options{
				Org:      "boxbotix",
				Platform: "bbops",
				Targets:  tracker,
				Sender:   sender,
				Store:    newFakeCommandStore(),
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewService (%s): %v", mode, err)
			}
			peer := netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", 20600+idx))

			// Initial (seq=10, sitl-local) at pos=1.
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
			curSeq := uint8(10)
			// Wrap 1: 256 sequence steps back to seq=10, dropping 2 packets (seq=50, 51) -> lifetimePacketPos = 255.
			for step := 1; step <= 256; step++ {
				curSeq++
				if curSeq == 50 || curSeq == 51 {
					continue
				}
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: curSeq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)
			}
			// Wrap 2 up to seq=9: 255 sequence steps, dropping 2 packets (seq=200, 201) -> lifetimePacketPos = 508.
			for step := 1; step <= 255; step++ {
				curSeq++
				if curSeq == 200 || curSeq == 201 {
					continue
				}
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: curSeq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)
			}

			// Cycle 3: skip seq=10 initially so candPos <= 513 (older cycle-1 entry still inside 512-packet window!).
			var benchTs time.Time
			if mode == "one_pos_late_after_bench_11" {
				now = now.Add(5 * time.Millisecond)
				benchTs = now
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 11, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, benchTs), benchTs)
				curSeq = 11
			} else {
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: 11, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)
				now = now.Add(5 * time.Millisecond)
				benchTs = now
				deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 12, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierDirectBench), peer, benchTs), benchTs)
				curSeq = 12
			}

			obsTs := now.Add(5 * time.Millisecond)
			if mode == "stale_mono_after_bench_12" {
				obsTs = benchTs.Add(-2 * time.Millisecond)
			} else {
				now = obsTs
			}
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, obsTs), obsTs)

			// 10 ordinary telemetry packets age out the cycle-1 pos=1 entry (lifetimePacketPos > 513).
			for i := 0; i < 10; i++ {
				curSeq++
				now = now.Add(5 * time.Millisecond)
				deliverTrackerOutcome(svc, tracker.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: curSeq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peer, now), now)
			}

			now = now.Add(3100 * time.Millisecond)
			for _, ev := range tracker.EvaluateSilence(now) {
				svc.OnTransitionEvent(ev)
			}

			// Reconnect copy of (seq=10, sitl-local) MUST NOT confirm sitl-local or allow ActionSITLCRSFOverride!
			now = now.Add(20 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 10, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)

			var ce *ControlError
			if _, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride}); !errors.As(err, &ce) || ce.Status != 403 || ce.Code != "sitl_only_action" {
				t.Fatalf("%s: ActionSITLCRSFOverride after window-crossing seq=10 sitl-local replay = %v, want 403 sitl_only_action", mode, err)
			}
			if got := sender.countByMsgID(mavcodec.MsgIDCommandLong); got != 0 {
				t.Fatalf("%s: emitted %d #76 frames on stale replay, want 0", mode, got)
			}

			// Positive control: fresh unseen (seq=25, sitl-local) confirms sitl-local and dispatches ActionSITLCRSFOverride.
			now = now.Add(20 * time.Millisecond)
			deliverTrackerOutcome(svc, tracker.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 25, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peer, now), now)
			if _, err := svc.SubmitCommand(context.Background(), CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride}); err != nil {
				t.Fatalf("%s: ActionSITLCRSFOverride after fresh seq=25 sitl-local: %v", mode, err)
			}
			if got := sender.countByMsgID(mavcodec.MsgIDCommandLong); got != 1 {
				t.Fatalf("%s: fresh seq=25 #76 count = %d, want 1", mode, got)
			}
		}
	})

	t.Run("replacement_peer_or_non_sitl_tier_never_confirms_prior_simulator_command_across_reconnect", func(t *testing.T) {
		ctx := context.Background()

		// Subcase A: Simulator A (peerA, sitl-local) dispatches ActionSITLCRSFOverride -> times out -> link_lost ->
		// direct-bench peer B (peerB != peerA) admitted under sysid=1 with hardware override (direct-crsf-override).
		trackerA, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker A: %v", err)
		}
		storeA := newFakeCommandStore()
		nowA := time.Date(2026, 11, 28, 8, 30, 0, 0, time.UTC)
		svcA, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerA,
			Sender:   &fakeFrameSender{},
			Store:    storeA,
			Now:      func() time.Time { return nowA },
		})
		if err != nil {
			t.Fatalf("NewService A: %v", err)
		}
		peerSimA := netip.MustParseAddrPort("127.0.0.1:20611")
		peerBenchB := netip.MustParseAddrPort("127.0.0.1:20612")

		deliverTrackerOutcome(svcA, trackerA.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerSimA, nowA), nowA)
		for seq := uint8(2); seq <= 150; seq++ {
			nowA = nowA.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svcA, trackerA.IngestFrame(mavcodec.Frame{SystemID: 1, ComponentID: mavcodec.CompIDAutopilot1, Sequence: seq, Message: mavcodec.AttitudeQuaternion{Q1: 1}}, peerSimA, nowA), nowA)
		}
		nowA = nowA.Add(10 * time.Millisecond)
		cmdSimA, err := svcA.SubmitCommand(ctx, CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride})
		if err != nil {
			t.Fatalf("SubmitCommand SimA: %v", err)
		}
		nowA = nowA.Add(2 * time.Second)
		svcA.Advance(ctx, nowA)
		if ent, _ := storeA.get(cmdSimA.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("after timeout SimA = %+v, want PhaseCommitUnknown", ent)
		}

		// Link loss on Simulator A, then replacement direct-bench peer B admitted with direct-crsf-override!
		nowA = nowA.Add(3100 * time.Millisecond)
		for _, ev := range trackerA.EvaluateSilence(nowA) {
			svcA.OnTransitionEvent(ev)
		}
		nowA = nowA.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcA, trackerA.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierDirectBench), peerBenchB, nowA), nowA)
		svcA.Advance(ctx, nowA)
		if ent, _ := storeA.get(cmdSimA.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("direct-bench replacement peer B at PacketOrder=1 falsely confirmed Simulator A command: %+v", ent)
		}
		// Even past PacketOrder > 150 AND with #77 COMMAND_ACK from peer B, Simulator A's command MUST stay PhaseCommitUnknown!
		for seq := uint8(2); seq <= 160; seq++ {
			nowA = nowA.Add(10 * time.Millisecond)
			deliverTrackerOutcome(svcA, trackerA.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, seq, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierDirectBench), peerBenchB, nowA), nowA)
		}
		nowA = nowA.Add(10 * time.Millisecond)
		deliverTrackerOutcome(svcA, trackerA.IngestFrame(mavcodec.Frame{
			SystemID:    1,
			ComponentID: mavcodec.CompIDAutopilot1,
			Sequence:    161,
			Message: mavcodec.CommandAck{
				Command: cmdSimA.CmdID,
				Result:  mavcodec.MAVResultAccepted,
			},
		}, peerBenchB, nowA), nowA)
		svcA.Advance(ctx, nowA)
		if ent, _ := storeA.get(cmdSimA.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("direct-bench replacement peer B past PacketOrder=160 / COMMAND_ACK falsely confirmed Simulator A command: %+v", ent)
		}

		// Subcase B: Replacement sitl-local Simulator B at peerSimB != peerSimA (including peerSimA -> peerSimB -> peerSimA)
		// NEVER confirms Simulator A's prior command.
		trackerB, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker B: %v", err)
		}
		storeB := newFakeCommandStore()
		nowB := time.Date(2026, 11, 28, 8, 40, 0, 0, time.UTC)
		svcB, err := NewService(Options{
			Org:               "boxbotix",
			Platform:          "bbops",
			Targets:           trackerB,
			Sender:            &fakeFrameSender{},
			Store:             storeB,
			CommandAckTimeout: 5 * time.Second,
			Now:               func() time.Time { return nowB },
		})
		if err != nil {
			t.Fatalf("NewService B: %v", err)
		}
		peerSimB := netip.MustParseAddrPort("127.0.0.1:20613")

		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerSimA, nowB), nowB)
		nowB = nowB.Add(10 * time.Millisecond)
		cmdSimB, err := svcB.SubmitCommand(ctx, CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride})
		if err != nil {
			t.Fatalf("SubmitCommand SimB: %v", err)
		}
		// Link loss BEFORE timeout, then replacement sitl-local peerSimB admitted with Authority=direct-crsf-override.
		nowB = nowB.Add(3100 * time.Millisecond)
		for _, ev := range trackerB.EvaluateSilence(nowB) {
			svcB.OnTransitionEvent(ev)
		}
		nowB = nowB.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerSimB, nowB), nowB)
		nowB = nowB.Add(2 * time.Second)
		svcB.Advance(ctx, nowB)
		nowB = nowB.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 2, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerSimB, nowB), nowB)
		svcB.Advance(ctx, nowB)
		if ent, _ := storeB.get(cmdSimB.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("replacement sitl-local peerSimB falsely confirmed peerSimA command: %+v", ent)
		}
		// Even if peerSimB later goes silent and peerSimA is re-admitted, targetDisplaced stays latched!
		nowB = nowB.Add(3100 * time.Millisecond)
		for _, ev := range trackerB.EvaluateSilence(nowB) {
			svcB.OnTransitionEvent(ev)
		}
		nowB = nowB.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcB, trackerB.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 25, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerSimA, nowB), nowB)
		svcB.Advance(ctx, nowB)
		if ent, _ := storeB.get(cmdSimB.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("re-admitted peerSimA after peerSimB displacement falsely confirmed displaced command: %+v", ent)
		}

		// Subcase C: Same UDP peer reused by direct-bench hardware after link_lost NEVER confirms ActionSITLCRSFOverride.
		trackerC, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker C: %v", err)
		}
		storeC := newFakeCommandStore()
		nowC := time.Date(2026, 11, 28, 8, 50, 0, 0, time.UTC)
		svcC, err := NewService(Options{
			Org:      "boxbotix",
			Platform: "bbops",
			Targets:  trackerC,
			Sender:   &fakeFrameSender{},
			Store:    storeC,
			Now:      func() time.Time { return nowC },
		})
		if err != nil {
			t.Fatalf("NewService C: %v", err)
		}
		peerSame := netip.MustParseAddrPort("127.0.0.1:20614")

		deliverTrackerOutcome(svcC, trackerC.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerSame, nowC), nowC)
		nowC = nowC.Add(10 * time.Millisecond)
		cmdSame, err := svcC.SubmitCommand(ctx, CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride})
		if err != nil {
			t.Fatalf("SubmitCommand SamePeer: %v", err)
		}
		nowC = nowC.Add(2 * time.Second)
		svcC.Advance(ctx, nowC)
		nowC = nowC.Add(3100 * time.Millisecond)
		for _, ev := range trackerC.EvaluateSilence(nowC) {
			svcC.OnTransitionEvent(ev)
		}
		nowC = nowC.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcC, trackerC.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 25, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierDirectBench), peerSame, nowC), nowC)
		nowC = nowC.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcC, trackerC.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 26, true, mavcodec.ModeHoldPosition, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierDirectBench), peerSame, nowC), nowC)
		svcC.Advance(ctx, nowC)
		if ent, _ := storeC.get(cmdSame.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("same-peer direct-bench hardware falsely confirmed ActionSITLCRSFOverride: %+v", ent)
		}
		// Even if peerSame later emits sitl-local after confirmed direct-bench takeover, targetDisplaced remains latched!
		nowC = nowC.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcC, trackerC.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 27, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerSame, nowC), nowC)
		svcC.Advance(ctx, nowC)
		if ent, _ := storeC.get(cmdSame.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("post-bench-takeover sitl-local frame falsely confirmed displaced ActionSITLCRSFOverride: %+v", ent)
		}

		// Subcase D: Brief replacement peerSimB takeover with companion-teleop WHILE still PhasePending (< timeout),
		// followed by peerSimB silence and peerSimA reconnect with direct-crsf-override -> OnTransitionEvent latched targetDisplaced!
		trackerD, err := mavlink.NewTracker("boxbotix", "bbops")
		if err != nil {
			t.Fatalf("NewTracker D: %v", err)
		}
		storeD := newFakeCommandStore()
		nowD := time.Date(2026, 11, 28, 8, 55, 0, 0, time.UTC)
		svcD, err := NewService(Options{
			Org:               "boxbotix",
			Platform:          "bbops",
			Targets:           trackerD,
			Sender:            &fakeFrameSender{},
			Store:             storeD,
			CommandAckTimeout: 10 * time.Second,
			Now:               func() time.Time { return nowD },
		})
		if err != nil {
			t.Fatalf("NewService D: %v", err)
		}
		deliverTrackerOutcome(svcD, trackerD.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerSimA, nowD), nowD)
		nowD = nowD.Add(10 * time.Millisecond)
		cmdD, err := svcD.SubmitCommand(ctx, CommandRequest{SysID: 1, Action: ActionSITLCRSFOverride})
		if err != nil {
			t.Fatalf("SubmitCommand D: %v", err)
		}
		nowD = nowD.Add(3100 * time.Millisecond)
		for _, ev := range trackerD.EvaluateSilence(nowD) {
			svcD.OnTransitionEvent(ev)
		}
		nowD = nowD.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcD, trackerD.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 1, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityCompanionTeleop, mavcodec.LinkTierSITLLocal), peerSimB, nowD), nowD)
		nowD = nowD.Add(3100 * time.Millisecond)
		for _, ev := range trackerD.EvaluateSilence(nowD) {
			svcD.OnTransitionEvent(ev)
		}
		nowD = nowD.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcD, trackerD.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 30, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerSimA, nowD), nowD)
		nowD = nowD.Add(5 * time.Second)
		svcD.Advance(ctx, nowD)
		nowD = nowD.Add(20 * time.Millisecond)
		deliverTrackerOutcome(svcD, trackerD.IngestFrame(makeTestVehicleHeartbeatFrameWithTier(t, 1, 31, true, mavcodec.ModeTeleopVelocity, mavcodec.AuthorityDirectCRSFOverride, mavcodec.LinkTierSITLLocal), peerSimA, nowD), nowD)
		svcD.Advance(ctx, nowD)
		if ent, _ := storeD.get(cmdD.CommandID); ent.PhaseState != PhaseCommitUnknown {
			t.Fatalf("transient pre-timeout peerSimB takeover failed to latch targetDisplaced: %+v", ent)
		}
	})
}
