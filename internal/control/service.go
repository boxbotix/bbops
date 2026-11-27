package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boxbotix/bbops/internal/mavcodec"
	"github.com/boxbotix/bbops/internal/mavlink"
	"github.com/boxbotix/bbops/internal/uxsvocab"
	"github.com/c360studio/semengine/pkg/lifecycle"
)

const (
	// DefaultTeleopLeaseTTL expires silent #84 browser velocity streams within < 250ms,
	// strictly ahead of bbnav CommandMux's 300ms companion deadman clamp.
	DefaultTeleopLeaseTTL = 200 * time.Millisecond

	// DefaultTickInterval drives lease-expiry and command-timeout checks.
	DefaultTickInterval = 25 * time.Millisecond

	// DefaultGCSHeartbeatInterval is the 1 Hz outbound MAV_TYPE_GCS liveness cadence.
	DefaultGCSHeartbeatInterval = time.Second

	// DefaultCommandAckTimeout is the non-retrying #77 COMMAND_ACK wait bound on standard/SITL links.
	DefaultCommandAckTimeout = 1500 * time.Millisecond

	// DefaultSupervisoryAckTimeout is the non-retrying #77 COMMAND_ACK wait bound on mesh-supervisory links.
	DefaultSupervisoryAckTimeout = 3500 * time.Millisecond
)

// TargetSource provides atomic per-sysid pinned peer and safety snapshots (`OQ1 Option A`).
type TargetSource interface {
	ActuationTarget(sysid uint8) (mavlink.ActuationTarget, bool)
}

// FrameSender transmits a single MAVLink v2 frame to a pinned peer UDP endpoint.
type FrameSender interface {
	SendFrame(frame mavcodec.Frame, peer netip.AddrPort) error
}

// CommandStore persists discrete CommandEntity creations and revision-fenced CAS transitions.
type CommandStore interface {
	Create(ctx context.Context, cmd *CommandEntity) error
	Transition(ctx context.Context, entityID, newPhase, note string, source lifecycle.TransitionSource, ackResult int, detail string, updatedMs int64) error
}

// LifecycleStore adapts semengine/pkg/lifecycle.Manager to CommandStore (`OQ2 Option A`).
type LifecycleStore struct {
	mgr *lifecycle.Manager
}

// NewLifecycleStore registers the uxs-command workflow on mgr and returns a CAS-backed CommandStore.
func NewLifecycleStore(mgr *lifecycle.Manager) (*LifecycleStore, error) {
	if err := RegisterWorkflow(mgr); err != nil {
		return nil, err
	}
	return &LifecycleStore{mgr: mgr}, nil
}

// Create persists a newly dispatched pending CommandEntity into ENTITY_STATES.
func (s *LifecycleStore) Create(ctx context.Context, cmd *CommandEntity) error {
	if cmd == nil {
		return errors.New("control: nil CommandEntity")
	}
	cp := *cmd
	return s.mgr.Create(ctx, &cp)
}

// Transition executes a revision-fenced CAS phase transition and triple patch on entityID.
func (s *LifecycleStore) Transition(
	ctx context.Context,
	entityID, newPhase, note string,
	source lifecycle.TransitionSource,
	ackResult int,
	detail string,
	updatedMs int64,
) error {
	return s.mgr.TransitionWith(ctx, WorkflowName, entityID, newPhase, source, note, func(p lifecycle.Participant) error {
		ce, ok := p.(*CommandEntity)
		if !ok {
			return fmt.Errorf("control: unexpected participant type %T", p)
		}
		ce.AckResult = ackResult
		ce.Detail = detail
		ce.UpdatedMs = updatedMs
		return nil
	})
}

// ControlError classifies deterministic safety-gate, validation, and transport refusals.
type ControlError struct {
	Status  int
	Code    string
	Message string
}

func (e *ControlError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// CommandRequest is the inbound operator command payload for POST /control/command.
type CommandRequest struct {
	SysID  uint8   `json:"sysid"`
	Action string  `json:"action"`
	Param1 float64 `json:"param1,omitempty"`
	Param2 float64 `json:"param2,omitempty"`
	Param3 float64 `json:"param3,omitempty"`
}

// CommandResult is returned synchronously by SubmitCommand once the single non-retrying
// #76 COMMAND_LONG datagram is sent and the pending CommandEntity is created.
type CommandResult struct {
	CommandID string `json:"command_id"`
	SysID     uint8  `json:"sysid"`
	CmdID     uint16 `json:"cmd_id"`
	Action    string `json:"action"`
	Phase     string `json:"phase"`
	Detail    string `json:"detail"`
	IssuedMs  int64  `json:"issued_ms"`
}

// TeleopRequest is the inbound 10-30 Hz velocity setpoint or stop payload for POST /control/teleop.
type TeleopRequest struct {
	SysID   uint8   `json:"sysid"`
	Frame   string  `json:"frame,omitempty"`
	Vx      float64 `json:"vx"`
	Vy      float64 `json:"vy"`
	Vz      float64 `json:"vz"`
	YawRate float64 `json:"yaw_rate"`
	Engage  bool    `json:"engage,omitempty"`
	Stop    bool    `json:"stop,omitempty"`
}

// TeleopResult reports lease state after processing a POST /control/teleop request.
type TeleopResult struct {
	SysID       uint8 `json:"sysid"`
	LeaseActive bool  `json:"lease_active"`
	ExpiresMs   int64 `json:"expires_ms,omitempty"`
}

// Options configures a control.Service instance.
type Options struct {
	Org                   string
	Platform              string
	Targets               TargetSource
	Sender                FrameSender
	Store                 CommandStore
	Logger                *slog.Logger
	Now                   func() time.Time
	TeleopLeaseTTL        time.Duration
	TickInterval          time.Duration
	GCSHeartbeatInterval  time.Duration
	CommandAckTimeout     time.Duration
	SupervisoryAckTimeout time.Duration
}

type teleopLease struct {
	peer       netip.AddrPort
	coordFrame uint8
	expiresAt  time.Time
}

type inFlightCommand struct {
	entityID        string
	sysid           uint8
	cmdID           uint16
	action          string
	sitlOnly        bool
	phase           string
	ackDeadline     time.Time
	unknownAtUnixNs int64
}

// Service enforces UXS safety gates, non-retrying #76 -> #77 discrete actuation,
// lease-bound #84 velocity teleop, 1 Hz GCS heartbeats, and instant preemption.
type Service struct {
	org                   string
	platform              string
	targets               TargetSource
	sender                FrameSender
	store                 CommandStore
	logger                *slog.Logger
	nowFn                 func() time.Time
	bootTime              time.Time
	teleopTTL             time.Duration
	tickInterval          time.Duration
	gcsHeartbeatInterval  time.Duration
	commandAckTimeout     time.Duration
	supervisoryAckTimeout time.Duration

	seqCounter atomic.Uint32
	cmdCounter atomic.Uint64

	mu             sync.Mutex
	leases         map[uint8]*teleopLease
	preemptLatched map[uint8]bool
	inFlight       map[string]*inFlightCommand
	lastGCSHB      map[uint8]time.Time

	runMu     sync.Mutex
	runCancel context.CancelFunc
	wg        sync.WaitGroup
}

// NewService validates opts and constructs an unstarted control.Service.
func NewService(opts Options) (*Service, error) {
	if err := RegisterVocab(); err != nil {
		return nil, err
	}
	org := strings.TrimSpace(opts.Org)
	if org == "" {
		org = "boxbotix"
	}
	platform := strings.TrimSpace(opts.Platform)
	if platform == "" {
		platform = "bbops"
	}
	if _, err := CommandEntityID(org, platform, "initcheck"); err != nil {
		return nil, err
	}
	if opts.Targets == nil {
		return nil, errors.New("control: Targets is required")
	}
	if opts.Sender == nil {
		return nil, errors.New("control: Sender is required")
	}
	if opts.Store == nil {
		return nil, errors.New("control: Store is required")
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	teleopTTL := opts.TeleopLeaseTTL
	if teleopTTL <= 0 {
		teleopTTL = DefaultTeleopLeaseTTL
	}
	if teleopTTL >= 250*time.Millisecond {
		return nil, fmt.Errorf("control: TeleopLeaseTTL (%v) must be < 250ms", teleopTTL)
	}
	tickInterval := opts.TickInterval
	if tickInterval <= 0 {
		tickInterval = DefaultTickInterval
	}
	gcsHB := opts.GCSHeartbeatInterval
	if gcsHB <= 0 {
		gcsHB = DefaultGCSHeartbeatInterval
	}
	ackTimeout := opts.CommandAckTimeout
	if ackTimeout <= 0 {
		ackTimeout = DefaultCommandAckTimeout
	}
	supAckTimeout := opts.SupervisoryAckTimeout
	if supAckTimeout <= 0 {
		supAckTimeout = DefaultSupervisoryAckTimeout
	}

	return &Service{
		org:                   org,
		platform:              platform,
		targets:               opts.Targets,
		sender:                opts.Sender,
		store:                 opts.Store,
		logger:                logger,
		nowFn:                 nowFn,
		bootTime:              nowFn(),
		teleopTTL:             teleopTTL,
		tickInterval:          tickInterval,
		gcsHeartbeatInterval:  gcsHB,
		commandAckTimeout:     ackTimeout,
		supervisoryAckTimeout: supAckTimeout,
		leases:                make(map[uint8]*teleopLease),
		preemptLatched:        make(map[uint8]bool),
		inFlight:              make(map[string]*inFlightCommand),
		lastGCSHB:             make(map[uint8]time.Time),
	}, nil
}

// Start launches the background lease-deadman, command-ACK timeout, and 1 Hz GCS heartbeat loop.
func (s *Service) Start(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.runCancel != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.runCancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				s.Advance(runCtx, s.nowFn())
			}
		}
	}()
	return nil
}

// Stop cancels the background ticker and waits for completion.
func (s *Service) Stop(ctx context.Context) error {
	s.runMu.Lock()
	cancel := s.runCancel
	s.runCancel = nil
	s.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) nextSeq() uint8 {
	return uint8(s.seqCounter.Add(1) - 1)
}

func isValidVehicleSysID(sysid uint8) bool {
	return sysid >= 1 && sysid <= 254
}

func isFiniteFloat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func resolveCoordinateFrame(frame string) (uint8, error) {
	switch strings.ToLower(strings.TrimSpace(frame)) {
	case "", FrameLocalNED:
		return mavcodec.MAVFrameLocalNED, nil
	case FrameBodyNED:
		return mavcodec.MAVFrameBodyNED, nil
	default:
		return 0, &ControlError{
			Status:  http.StatusBadRequest,
			Code:    "invalid_frame",
			Message: fmt.Sprintf("unsupported velocity coordinate frame %q (expected %q or %q)", frame, FrameLocalNED, FrameBodyNED),
		}
	}
}

type resolvedCommandSpec struct {
	msg      mavcodec.CommandLong
	sitlOnly bool
}

func resolveActionSpec(req CommandRequest) (resolvedCommandSpec, error) {
	if !isFiniteFloat(req.Param1) || !isFiniteFloat(req.Param2) || !isFiniteFloat(req.Param3) {
		return resolvedCommandSpec{}, &ControlError{
			Status:  http.StatusBadRequest,
			Code:    "non_finite_value",
			Message: "command parameters must be finite numbers",
		}
	}
	base := mavcodec.CommandLong{
		TargetSystem:    req.SysID,
		TargetComponent: mavcodec.CompIDAutopilot1,
		Confirmation:    0,
	}
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case ActionArm:
		base.Command = mavcodec.MAVCmdComponentArmDisarm
		base.Param1 = 1
		return resolvedCommandSpec{msg: base}, nil
	case ActionDisarm:
		base.Command = mavcodec.MAVCmdComponentArmDisarm
		base.Param1 = 0
		return resolvedCommandSpec{msg: base}, nil
	case ActionHoldPosition:
		base.Command = mavcodec.MAVCmdNavLoiterUnlim
		return resolvedCommandSpec{msg: base}, nil
	case ActionModeTeleop:
		base.Command = mavcodec.MAVCmdDoSetMode
		base.Param1 = float32(mavcodec.ModeFlagCustomModeEnabled)
		base.Param2 = float32(mavcodec.ModeTeleopVelocity)
		return resolvedCommandSpec{msg: base}, nil
	case ActionModeAutonomous:
		base.Command = mavcodec.MAVCmdDoSetMode
		base.Param1 = float32(mavcodec.ModeFlagCustomModeEnabled)
		base.Param2 = float32(mavcodec.ModeAutonomousVelocity)
		return resolvedCommandSpec{msg: base}, nil
	case ActionSITLCompanionBlackout:
		durMs := req.Param1
		if durMs <= 0 {
			durMs = 350
		}
		base.Command = mavcodec.MAVCmdUser1
		base.Param1 = 1
		base.Param2 = float32(durMs)
		return resolvedCommandSpec{msg: base, sitlOnly: true}, nil
	case ActionSITLCRSFOverride:
		base.Command = mavcodec.MAVCmdUser1
		base.Param1 = 2
		base.Param2 = float32(req.Param1)
		return resolvedCommandSpec{msg: base, sitlOnly: true}, nil
	case ActionSITLCrossCurrent:
		base.Command = mavcodec.MAVCmdUser1
		base.Param1 = 3
		base.Param2 = float32(req.Param1)
		base.Param3 = float32(req.Param2)
		return resolvedCommandSpec{msg: base, sitlOnly: true}, nil
	default:
		return resolvedCommandSpec{}, &ControlError{
			Status:  http.StatusBadRequest,
			Code:    "unsupported_action",
			Message: fmt.Sprintf("unsupported discrete command action %q", req.Action),
		}
	}
}

// IsPreempted reports whether sysid currently holds an unacknowledged preemption latch.
func (s *Service) IsPreempted(sysid uint8) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preemptLatched[sysid]
}

// HasActiveLease reports whether sysid currently has a live #84 velocity teleop lease.
func (s *Service) HasActiveLease(sysid uint8) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.leases[sysid]
	return ok
}

// SubmitTeleop validates safety gates and forwards an at-most-once #84 velocity setpoint
// (or explicit zero-velocity release on Stop) to the pinned peer UDP endpoint.
func (s *Service) SubmitTeleop(_ context.Context, req TeleopRequest) (TeleopResult, error) {
	if !isValidVehicleSysID(req.SysID) {
		return TeleopResult{}, &ControlError{
			Status:  http.StatusBadRequest,
			Code:    "invalid_sysid",
			Message: fmt.Sprintf("sysid %d must be in range [1..254]", req.SysID),
		}
	}
	if !isFiniteFloat(req.Vx) || !isFiniteFloat(req.Vy) || !isFiniteFloat(req.Vz) || !isFiniteFloat(req.YawRate) {
		return TeleopResult{}, &ControlError{
			Status:  http.StatusBadRequest,
			Code:    "non_finite_value",
			Message: "teleop velocity and yaw_rate values must be finite numbers",
		}
	}
	coordFrame, err := resolveCoordinateFrame(req.Frame)
	if err != nil {
		return TeleopResult{}, err
	}

	now := s.nowFn()
	if req.Stop {
		var releasePeer netip.AddrPort
		var releaseFrame uint8
		s.mu.Lock()
		prev, hadLease := s.leases[req.SysID]
		if hadLease {
			delete(s.leases, req.SysID)
			releaseFrame = prev.coordFrame
		}
		s.mu.Unlock()

		if hadLease {
			if target, ok := s.targets.ActuationTarget(req.SysID); ok &&
				!target.LinkLost && target.PeerAddr.IsValid() &&
				target.LinkTier != uxsvocab.LinkTierMeshSupervisory &&
				target.Authority == uxsvocab.AuthorityCompanionTeleop {
				releasePeer = target.PeerAddr
			}
		}
		if releasePeer.IsValid() {
			_ = s.sendZeroVelocity(req.SysID, releaseFrame, releasePeer, now)
		}
		return TeleopResult{SysID: req.SysID, LeaseActive: false}, nil
	}

	target, ok := s.targets.ActuationTarget(req.SysID)
	if !ok || !target.PeerAddr.IsValid() {
		return TeleopResult{}, &ControlError{
			Status:  http.StatusNotFound,
			Code:    "vehicle_not_admitted",
			Message: fmt.Sprintf("vehicle sysid %d is not admitted", req.SysID),
		}
	}
	if target.LinkLost {
		return TeleopResult{}, &ControlError{
			Status:  http.StatusConflict,
			Code:    "vehicle_link_lost",
			Message: fmt.Sprintf("vehicle sysid %d link is silent (LINK LOST)", req.SysID),
		}
	}
	if target.LinkTier == uxsvocab.LinkTierMeshSupervisory {
		return TeleopResult{}, &ControlError{
			Status:  http.StatusForbidden,
			Code:    "supervisory_link_tier",
			Message: "continuous #84 velocity teleop is prohibited on mesh-supervisory links",
		}
	}
	if !target.Armed {
		return TeleopResult{}, &ControlError{
			Status:  http.StatusConflict,
			Code:    "vehicle_disarmed",
			Message: fmt.Sprintf("vehicle sysid %d is disarmed", req.SysID),
		}
	}
	if target.Authority != uxsvocab.AuthorityCompanionTeleop {
		return TeleopResult{}, &ControlError{
			Status:  http.StatusConflict,
			Code:    "authority_not_teleop",
			Message: fmt.Sprintf("vehicle sysid %d authority is %q (companion-teleop required)", req.SysID, target.Authority),
		}
	}

	expiresAt := now.Add(s.teleopTTL)
	var shouldSendGCSHB bool

	s.mu.Lock()
	if s.preemptLatched[req.SysID] {
		if !req.Engage {
			s.mu.Unlock()
			return TeleopResult{}, &ControlError{
				Status:  http.StatusConflict,
				Code:    "preemption_latched",
				Message: fmt.Sprintf("vehicle sysid %d teleop was preempted — explicit re-engage required", req.SysID),
			}
		}
		delete(s.preemptLatched, req.SysID)
	}
	s.leases[req.SysID] = &teleopLease{
		peer:       target.PeerAddr,
		coordFrame: coordFrame,
		expiresAt:  expiresAt,
	}
	if lastHB, seen := s.lastGCSHB[req.SysID]; !seen || !now.Before(lastHB.Add(s.gcsHeartbeatInterval)) {
		s.lastGCSHB[req.SysID] = now
		shouldSendGCSHB = true
	}
	s.mu.Unlock()

	if shouldSendGCSHB {
		_ = s.sendGCSHeartbeat(target.PeerAddr)
	}

	if err := s.sender.SendFrame(mavcodec.Frame{
		Sequence:    s.nextSeq(),
		SystemID:    mavcodec.GCSSystemID,
		ComponentID: mavcodec.CompIDMissionPlanner,
		Message: mavcodec.SetPositionTargetLocalNED{
			TimeBootMs:      s.bootMillis(now),
			TargetSystem:    req.SysID,
			TargetComponent: mavcodec.CompIDAutopilot1,
			CoordinateFrame: coordFrame,
			TypeMask:        mavcodec.PositionTargetTypeMaskVelocityOnly,
			Vx:              float32(req.Vx),
			Vy:              float32(req.Vy),
			Vz:              float32(req.Vz),
			YawRate:         float32(req.YawRate),
		},
	}, target.PeerAddr); err != nil {
		return TeleopResult{}, &ControlError{
			Status:  http.StatusServiceUnavailable,
			Code:    "transport_unavailable",
			Message: err.Error(),
		}
	}

	return TeleopResult{
		SysID:       req.SysID,
		LeaseActive: true,
		ExpiresMs:   expiresAt.UTC().UnixMilli(),
	}, nil
}

// SubmitCommand validates safety gates, persists a pending CommandEntity via lifecycle CAS,
// and transmits a single non-retrying #76 COMMAND_LONG (`Confirmation = 0`) datagram.
func (s *Service) SubmitCommand(ctx context.Context, req CommandRequest) (CommandResult, error) {
	if !isValidVehicleSysID(req.SysID) {
		return CommandResult{}, &ControlError{
			Status:  http.StatusBadRequest,
			Code:    "invalid_sysid",
			Message: fmt.Sprintf("sysid %d must be in range [1..254]", req.SysID),
		}
	}
	actionSlug := strings.ToLower(strings.TrimSpace(req.Action))
	req.Action = actionSlug
	spec, err := resolveActionSpec(req)
	if err != nil {
		return CommandResult{}, err
	}

	target, ok := s.targets.ActuationTarget(req.SysID)
	if !ok || !target.PeerAddr.IsValid() {
		return CommandResult{}, &ControlError{
			Status:  http.StatusNotFound,
			Code:    "vehicle_not_admitted",
			Message: fmt.Sprintf("vehicle sysid %d is not admitted", req.SysID),
		}
	}
	if target.LinkLost {
		return CommandResult{}, &ControlError{
			Status:  http.StatusConflict,
			Code:    "vehicle_link_lost",
			Message: fmt.Sprintf("vehicle sysid %d link is silent (LINK LOST)", req.SysID),
		}
	}
	if spec.sitlOnly && target.LinkTier != uxsvocab.LinkTierSITLLocal {
		return CommandResult{}, &ControlError{
			Status:  http.StatusForbidden,
			Code:    "sitl_only_action",
			Message: fmt.Sprintf("action %q is restricted to sitl-local vehicles (current link_tier=%q)", actionSlug, target.LinkTier),
		}
	}
	if !spec.sitlOnly {
		if target.Authority == uxsvocab.AuthorityDirectCRSFOverride {
			return CommandResult{}, &ControlError{
				Status:  http.StatusConflict,
				Code:    "authority_preempted",
				Message: "hardware RC pilot override (direct-crsf-override) holds actuator authority",
			}
		}
		if target.Authority == uxsvocab.AuthorityMCUFailsafe && target.Armed && actionSlug != ActionDisarm {
			return CommandResult{}, &ControlError{
				Status:  http.StatusConflict,
				Code:    "authority_preempted",
				Message: "MCU hardware failsafe (mcu-failsafe) holds actuator authority",
			}
		}
	}

	now := s.nowFn().UTC()
	ackWait := s.commandAckTimeout
	if target.LinkTier == uxsvocab.LinkTierMeshSupervisory {
		ackWait = s.supervisoryAckTimeout
	}

	cmdSeq := s.cmdCounter.Add(1)
	instance := fmt.Sprintf("s%dc%d", req.SysID, cmdSeq)
	entityID, err := CommandEntityID(s.org, s.platform, instance)
	if err != nil {
		return CommandResult{}, err
	}

	var shouldSendGCSHB bool
	s.mu.Lock()
	for id, existing := range s.inFlight {
		if existing.sysid == req.SysID && existing.cmdID == spec.msg.Command {
			if existing.phase == PhasePending {
				s.mu.Unlock()
				return CommandResult{}, &ControlError{
					Status:  http.StatusConflict,
					Code:    "command_in_flight",
					Message: fmt.Sprintf("command %d is already pending on vehicle sysid %d", spec.msg.Command, req.SysID),
				}
			}
			delete(s.inFlight, id)
		}
	}
	inflight := &inFlightCommand{
		entityID:    entityID,
		sysid:       req.SysID,
		cmdID:       spec.msg.Command,
		action:      actionSlug,
		sitlOnly:    spec.sitlOnly,
		phase:       PhasePending,
		ackDeadline: now.Add(ackWait),
	}
	s.inFlight[entityID] = inflight
	if lastHB, seen := s.lastGCSHB[req.SysID]; !seen || !now.Before(lastHB.Add(s.gcsHeartbeatInterval)) {
		s.lastGCSHB[req.SysID] = now
		shouldSendGCSHB = true
	}
	s.mu.Unlock()

	entity := &CommandEntity{
		ID:         entityID,
		PhaseState: PhasePending,
		Target:     target.EntityID,
		SysID:      int(req.SysID),
		CmdID:      int(spec.msg.Command),
		Action:     actionSlug,
		AckResult:  UnacknowledgedResult,
		Detail:     "Awaiting COMMAND_ACK",
		IssuedMs:   now.UnixMilli(),
		UpdatedMs:  now.UnixMilli(),
	}
	if err := s.store.Create(ctx, entity); err != nil {
		s.mu.Lock()
		delete(s.inFlight, entityID)
		s.mu.Unlock()
		return CommandResult{}, fmt.Errorf("control: create command entity: %w", err)
	}

	if shouldSendGCSHB {
		_ = s.sendGCSHeartbeat(target.PeerAddr)
	}

	if err := s.sender.SendFrame(mavcodec.Frame{
		Sequence:    s.nextSeq(),
		SystemID:    mavcodec.GCSSystemID,
		ComponentID: mavcodec.CompIDMissionPlanner,
		Message:     spec.msg,
	}, target.PeerAddr); err != nil {
		s.mu.Lock()
		delete(s.inFlight, entityID)
		s.mu.Unlock()
		_ = s.store.Transition(
			ctx,
			entityID,
			PhaseRejected,
			"udp_send_failed",
			lifecycle.TransitionSourceComponent,
			int(mavcodec.MAVResultFailed),
			"UDP command transmit failed",
			now.UnixMilli(),
		)
		return CommandResult{}, &ControlError{
			Status:  http.StatusServiceUnavailable,
			Code:    "transport_unavailable",
			Message: err.Error(),
		}
	}

	return CommandResult{
		CommandID: entityID,
		SysID:     req.SysID,
		CmdID:     spec.msg.Command,
		Action:    actionSlug,
		Phase:     PhasePending,
		Detail:    entity.Detail,
		IssuedMs:  entity.IssuedMs,
	}, nil
}

// OnCommandAck correlates an inbound #77 COMMAND_ACK from a pinned vehicle peer
// against any in-flight discrete command for (sysid, ack.Command).
func (s *Service) OnCommandAck(sysid uint8, ack mavcodec.CommandAck, observedAt time.Time) {
	if ack.Result == mavcodec.MAVResultInProgress {
		return
	}
	if observedAt.IsZero() {
		observedAt = s.nowFn()
	}
	updatedMs := observedAt.UTC().UnixMilli()

	var matched *inFlightCommand
	var matchedID string
	s.mu.Lock()
	for id, cmd := range s.inFlight {
		if cmd.sysid == sysid && cmd.cmdID == ack.Command {
			if matched == nil || (matched.phase != PhasePending && cmd.phase == PhasePending) {
				matched = cmd
				matchedID = id
			}
		}
	}
	if matched != nil {
		delete(s.inFlight, matchedID)
	}
	s.mu.Unlock()
	if matched == nil {
		return
	}

	nextPhase := PhaseRejected
	note := "command_ack_rejected"
	detail := formatAckResultDetail(ack.Result)
	if ack.Result == mavcodec.MAVResultAccepted {
		nextPhase = PhaseConfirmed
		note = "command_ack_accepted"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.store.Transition(
		ctx,
		matched.entityID,
		nextPhase,
		note,
		lifecycle.TransitionSourceComponent,
		int(ack.Result),
		detail,
		updatedMs,
	); err != nil {
		s.logger.Warn("control: failed to transition command on COMMAND_ACK",
			slog.String("command_id", matched.entityID),
			slog.String("phase", nextPhase),
			slog.Any("error", err),
		)
	}
}

// OnTransitionEvent reacts synchronously to Lane 3 authority, LinkTier, and link-loss edges,
// revoking #84 velocity leases, latching preemption, preempting pending discrete commands,
// and reconciling commit_unknown commands when vehicle state edges confirm execution.
func (s *Service) OnTransitionEvent(ev mavlink.TransitionEvent) {
	isPreempt := ev.Kind == mavlink.EventKindLinkLost ||
		ev.Authority == uxsvocab.AuthorityDirectCRSFOverride ||
		ev.Authority == uxsvocab.AuthorityMCUFailsafe

	updatedMs := ev.Timestamp
	if updatedMs <= 0 {
		updatedMs = s.nowFn().UTC().UnixMilli()
	}

	var toPreempt []*inFlightCommand
	var toConfirm []*inFlightCommand

	s.mu.Lock()
	if isPreempt {
		delete(s.leases, ev.SysID)
		s.preemptLatched[ev.SysID] = true
		for id, cmd := range s.inFlight {
			if cmd.sysid == ev.SysID && !cmd.sitlOnly {
				toPreempt = append(toPreempt, cmd)
				delete(s.inFlight, id)
			}
		}
	} else {
		if ev.LinkTier == uxsvocab.LinkTierMeshSupervisory ||
			!ev.Armed ||
			ev.Authority != uxsvocab.AuthorityCompanionTeleop {
			delete(s.leases, ev.SysID)
		}
		for id, cmd := range s.inFlight {
			if cmd.sysid == ev.SysID && cmd.phase == PhaseCommitUnknown &&
				matchesExpectedVehicleState(cmd.action, ev.Armed, ev.CompanionMode, ev.Authority) {
				toConfirm = append(toConfirm, cmd)
				delete(s.inFlight, id)
			}
		}
	}
	s.mu.Unlock()

	if len(toPreempt) == 0 && len(toConfirm) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	preemptNote := "authority_preempted"
	preemptDetail := fmt.Sprintf("Preempted by %s", ev.Authority)
	if ev.Kind == mavlink.EventKindLinkLost {
		preemptNote = "link_lost"
		preemptDetail = "Preempted by vehicle link loss"
	}
	for _, cmd := range toPreempt {
		if err := s.store.Transition(
			ctx,
			cmd.entityID,
			PhasePreempted,
			preemptNote,
			lifecycle.TransitionSourceComponent,
			UnacknowledgedResult,
			preemptDetail,
			updatedMs,
		); err != nil {
			s.logger.Warn("control: failed to mark command preempted",
				slog.String("command_id", cmd.entityID),
				slog.Any("error", err),
			)
		}
	}

	for _, cmd := range toConfirm {
		if err := s.store.Transition(
			ctx,
			cmd.entityID,
			PhaseConfirmed,
			"reconciled_by_heartbeat",
			lifecycle.TransitionSourceComponent,
			UnacknowledgedResult,
			"Confirmed via vehicle HEARTBEAT state reconciliation",
			updatedMs,
		); err != nil {
			s.logger.Warn("control: failed to reconcile commit_unknown command",
				slog.String("command_id", cmd.entityID),
				slog.Any("error", err),
			)
		}
	}
}

// Advance evaluates #84 lease expiry (< 250ms deadman), non-retrying #76 -> commit_unknown
// timeouts, steady-heartbeat reconciliation, and 1 Hz outbound GCS #0 HEARTBEAT liveness at now.
func (s *Service) Advance(ctx context.Context, now time.Time) {
	if ctx == nil {
		ctx = context.Background()
	}
	type expiredLease struct {
		sysid      uint8
		peer       netip.AddrPort
		coordFrame uint8
	}

	var expired []expiredLease
	var timedOut []*inFlightCommand
	var reconciled []*inFlightCommand
	activeSysIDs := make(map[uint8]struct{})

	s.mu.Lock()
	for sysid, lease := range s.leases {
		if !now.Before(lease.expiresAt) {
			expired = append(expired, expiredLease{
				sysid:      sysid,
				peer:       lease.peer,
				coordFrame: lease.coordFrame,
			})
			delete(s.leases, sysid)
			continue
		}
		activeSysIDs[sysid] = struct{}{}
	}

	for id, cmd := range s.inFlight {
		if cmd.phase == PhasePending && !now.Before(cmd.ackDeadline) {
			cmd.phase = PhaseCommitUnknown
			cmd.unknownAtUnixNs = now.UTC().UnixNano()
			timedOut = append(timedOut, cmd)
			activeSysIDs[cmd.sysid] = struct{}{}
			continue
		}
		if cmd.phase == PhaseCommitUnknown {
			if target, ok := s.targets.ActuationTarget(cmd.sysid); ok && !target.LinkLost &&
				target.HeartbeatObservedUnixNs > cmd.unknownAtUnixNs &&
				matchesExpectedVehicleState(cmd.action, target.Armed, target.CompanionMode, target.Authority) {
				reconciled = append(reconciled, cmd)
				delete(s.inFlight, id)
				continue
			}
		}
		activeSysIDs[cmd.sysid] = struct{}{}
	}

	var gcsPeers []netip.AddrPort
	for sysid := range activeSysIDs {
		lastHB, seen := s.lastGCSHB[sysid]
		if !seen || !now.Before(lastHB.Add(s.gcsHeartbeatInterval)) {
			if target, ok := s.targets.ActuationTarget(sysid); ok && !target.LinkLost && target.PeerAddr.IsValid() {
				s.lastGCSHB[sysid] = now
				gcsPeers = append(gcsPeers, target.PeerAddr)
			}
		}
	}
	for sysid := range s.lastGCSHB {
		if _, active := activeSysIDs[sysid]; !active {
			delete(s.lastGCSHB, sysid)
		}
	}
	s.mu.Unlock()

	for _, exp := range expired {
		if target, ok := s.targets.ActuationTarget(exp.sysid); ok &&
			!target.LinkLost && target.PeerAddr.IsValid() &&
			target.LinkTier != uxsvocab.LinkTierMeshSupervisory &&
			target.Authority == uxsvocab.AuthorityCompanionTeleop {
			_ = s.sendZeroVelocity(exp.sysid, exp.coordFrame, target.PeerAddr, now)
		}
	}

	updatedMs := now.UTC().UnixMilli()
	for _, cmd := range timedOut {
		if err := s.store.Transition(
			ctx,
			cmd.entityID,
			PhaseCommitUnknown,
			"command_ack_timeout",
			lifecycle.TransitionSourceComponent,
			UnacknowledgedResult,
			CommitUnknownDetail,
			updatedMs,
		); err != nil {
			s.logger.Warn("control: failed to transition command to commit_unknown",
				slog.String("command_id", cmd.entityID),
				slog.Any("error", err),
			)
		}
	}

	for _, cmd := range reconciled {
		if err := s.store.Transition(
			ctx,
			cmd.entityID,
			PhaseConfirmed,
			"reconciled_by_heartbeat",
			lifecycle.TransitionSourceComponent,
			UnacknowledgedResult,
			"Confirmed via vehicle HEARTBEAT state reconciliation",
			updatedMs,
		); err != nil {
			s.logger.Warn("control: failed to reconcile commit_unknown command",
				slog.String("command_id", cmd.entityID),
				slog.Any("error", err),
			)
		}
	}

	for _, peer := range gcsPeers {
		_ = s.sendGCSHeartbeat(peer)
	}
}

func (s *Service) sendZeroVelocity(sysid, coordFrame uint8, peer netip.AddrPort, now time.Time) error {
	return s.sender.SendFrame(mavcodec.Frame{
		Sequence:    s.nextSeq(),
		SystemID:    mavcodec.GCSSystemID,
		ComponentID: mavcodec.CompIDMissionPlanner,
		Message: mavcodec.SetPositionTargetLocalNED{
			TimeBootMs:      s.bootMillis(now),
			TargetSystem:    sysid,
			TargetComponent: mavcodec.CompIDAutopilot1,
			CoordinateFrame: coordFrame,
			TypeMask:        mavcodec.PositionTargetTypeMaskVelocityOnly,
			Vx:              0,
			Vy:              0,
			Vz:              0,
			YawRate:         0,
		},
	}, peer)
}

func (s *Service) sendGCSHeartbeat(peer netip.AddrPort) error {
	return s.sender.SendFrame(mavcodec.Frame{
		Sequence:    s.nextSeq(),
		SystemID:    mavcodec.GCSSystemID,
		ComponentID: mavcodec.CompIDMissionPlanner,
		Message: mavcodec.Heartbeat{
			CustomMode:     0,
			Type:           mavcodec.MAVTypeGCS,
			Autopilot:      mavcodec.AutopilotGeneric,
			BaseMode:       0,
			SystemStatus:   mavcodec.MAVStateActive,
			MAVLinkVersion: mavcodec.MAVLinkVersionV2,
		},
	}, peer)
}

func (s *Service) bootMillis(now time.Time) uint32 {
	ms := now.Sub(s.bootTime).Milliseconds()
	if ms < 0 {
		return 0
	}
	return uint32(ms)
}

func matchesExpectedVehicleState(action string, armed bool, companionMode, authority string) bool {
	switch action {
	case ActionArm:
		return armed
	case ActionDisarm:
		return !armed || companionMode == uxsvocab.ModeDisarmed
	case ActionHoldPosition:
		return companionMode == uxsvocab.ModeHoldPosition
	case ActionModeTeleop:
		return companionMode == uxsvocab.ModeTeleopVelocity && authority == uxsvocab.AuthorityCompanionTeleop
	case ActionModeAutonomous:
		return companionMode == uxsvocab.ModeAutonomousVelocity
	default:
		return false
	}
}

func formatAckResultDetail(result uint8) string {
	switch result {
	case mavcodec.MAVResultAccepted:
		return "Accepted by vehicle"
	case mavcodec.MAVResultTemporarilyRejected:
		return "Temporarily rejected by vehicle"
	case mavcodec.MAVResultDenied:
		return "Denied by vehicle"
	case mavcodec.MAVResultUnsupported:
		return "Unsupported command"
	case mavcodec.MAVResultFailed:
		return "Execution failed on vehicle"
	case mavcodec.MAVResultCancelled:
		return "Cancelled by vehicle"
	default:
		return "Rejected by vehicle (result=" + strconv.Itoa(int(result)) + ")"
	}
}
