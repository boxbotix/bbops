package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	DefaultTeleopLeaseTTL = 240 * time.Millisecond

	// DefaultTickInterval drives lease-expiry and command-timeout checks.
	DefaultTickInterval = 25 * time.Millisecond

	// DefaultGCSHeartbeatInterval is the 1 Hz outbound MAV_TYPE_GCS liveness cadence.
	DefaultGCSHeartbeatInterval = time.Second

	// DefaultCommandAckTimeout is the non-retrying #77 COMMAND_ACK wait bound on standard/SITL links.
	DefaultCommandAckTimeout = 1500 * time.Millisecond

	// DefaultSupervisoryAckTimeout is the non-retrying #77 COMMAND_ACK wait bound on mesh-supervisory links.
	DefaultSupervisoryAckTimeout = 3500 * time.Millisecond

	maxStoppedSessionsPerVehicle = 64
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

// RecoverableCommandStore extends CommandStore with authoritative reads and full-history
// enumeration for restart recovery, ACK-ambiguity preservation, and transient-persistence verification.
type RecoverableCommandStore interface {
	CommandStore
	Get(ctx context.Context, entityID string) (*CommandEntity, error)
	ListCommands(ctx context.Context) ([]CommandEntity, error)
	ListNonTerminal(ctx context.Context) ([]CommandEntity, error)
}

// LifecycleStore adapts semengine/pkg/lifecycle.Manager to RecoverableCommandStore (`OQ2 Option A`).
type LifecycleStore struct {
	mgr *lifecycle.Manager
}

var _ RecoverableCommandStore = (*LifecycleStore)(nil)

// NewLifecycleStore registers the uxs-command workflow on mgr and returns a CAS-backed LifecycleStore.
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

// Get reads the projected CommandEntity for entityID from ENTITY_STATES.
func (s *LifecycleStore) Get(ctx context.Context, entityID string) (*CommandEntity, error) {
	p, err := s.mgr.Get(ctx, WorkflowName, entityID)
	if err != nil {
		return nil, err
	}
	ce, ok := p.(*CommandEntity)
	if !ok {
		return nil, fmt.Errorf("control: unexpected participant type %T", p)
	}
	cp := *ce
	return &cp, nil
}

// ListCommands enumerates all CommandEntity records (active and terminal) in ENTITY_STATES.
func (s *LifecycleStore) ListCommands(ctx context.Context) ([]CommandEntity, error) {
	items, err := s.mgr.List(ctx, WorkflowName, lifecycle.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]CommandEntity, 0, len(items))
	for _, p := range items {
		ce, ok := p.(*CommandEntity)
		if !ok || ce == nil {
			continue
		}
		out = append(out, *ce)
	}
	return out, nil
}

// ListNonTerminal enumerates non-terminal CommandEntity records (`pending`, `commit_unknown`) in ENTITY_STATES.
func (s *LifecycleStore) ListNonTerminal(ctx context.Context) ([]CommandEntity, error) {
	items, err := s.mgr.List(ctx, WorkflowName, lifecycle.ListOptions{Active: true})
	if err != nil {
		return nil, err
	}
	out := make([]CommandEntity, 0, len(items))
	for _, p := range items {
		ce, ok := p.(*CommandEntity)
		if !ok || ce == nil {
			continue
		}
		out = append(out, *ce)
	}
	return out, nil
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
	Session string  `json:"session,omitempty"`
	Seq     uint64  `json:"seq,omitempty"`
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
	session    string
	lastSeq    uint64
	expiresAt  time.Time
}

type stoppedSessionRing struct {
	order []string
	set   map[string]struct{}
}

func (r *stoppedSessionRing) add(session string) {
	if session == "" {
		return
	}
	if r.set == nil {
		r.set = make(map[string]struct{})
	}
	if _, exists := r.set[session]; exists {
		return
	}
	if len(r.order) >= maxStoppedSessionsPerVehicle {
		evict := r.order[0]
		r.order = r.order[1:]
		delete(r.set, evict)
	}
	r.order = append(r.order, session)
	r.set[session] = struct{}{}
}

func (r *stoppedSessionRing) contains(session string) bool {
	if session == "" || r == nil || r.set == nil {
		return false
	}
	_, ok := r.set[session]
	return ok
}

type cmdKey struct {
	sysid uint8
	cmdID uint16
}

type cmdDispatchState uint8

const (
	dispatchReserved cmdDispatchState = iota
	dispatchSending
	dispatchSent
)

type earlyCommandAck struct {
	ack        mavcodec.CommandAck
	observedAt time.Time
}

type inFlightCommand struct {
	entityID        string
	orderSeq        uint64
	sysid           uint8
	cmdID           uint16
	action          string
	sitlOnly        bool
	dispatch        cmdDispatchState
	abortedNote     string
	abortedDetail   string
	phase           string
	detail          string
	ackAmbiguous    bool
	ackDeadline     time.Time
	unknownAtUnixNs int64
	earlyAcks       []earlyCommandAck
}

type pendingCommandWrite struct {
	entityID  string
	phase     string
	note      string
	source    lifecycle.TransitionSource
	ackResult int
	detail    string
	updatedMs int64
}

// Service enforces UXS safety gates, non-retrying #76 -> #77 discrete actuation,
// lease-bound #84 velocity teleop, 1 Hz GCS heartbeats, and instant preemption.
type Service struct {
	org                   string
	platform              string
	bootNonce             string
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
	recovered  atomic.Bool

	// Lock hierarchy: egressMu[sysid] -> mu; recoverMu -> mu; persistMu -> mu.
	// Never acquire egressMu, recoverMu, or persistMu while holding mu, and never call
	// ensureRecovered or flushPendingWrites while holding egressMu[sysid].
	egressMu   [256]sync.Mutex
	expiring   [256]atomic.Bool
	gcsSending [256]atomic.Bool
	recoverMu  sync.Mutex
	persistMu  sync.Mutex

	mu               sync.Mutex
	authEpoch        map[uint8]uint64
	leases           map[uint8]*teleopLease
	stoppedSessions  map[uint8]*stoppedSessionRing
	preemptLatched   map[uint8]bool
	inFlight         map[string]*inFlightCommand
	unackedCount     map[cmdKey]int
	dispatchedInBoot map[cmdKey]bool
	pendingWrites    map[string]pendingCommandWrite
	lastGCSHB        map[uint8]time.Time

	leaseWake   chan struct{}
	persistWake chan struct{}

	runMu     sync.Mutex
	runCancel context.CancelFunc
	leaseWG   sync.WaitGroup
	cmdWG     sync.WaitGroup
}

func newBootNonce() string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatUint(uint64(time.Now().UnixNano()), 16)
	}
	return hex.EncodeToString(raw[:])
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

	svc := &Service{
		org:                   org,
		platform:              platform,
		bootNonce:             newBootNonce(),
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
		authEpoch:             make(map[uint8]uint64),
		leases:                make(map[uint8]*teleopLease),
		stoppedSessions:       make(map[uint8]*stoppedSessionRing),
		preemptLatched:        make(map[uint8]bool),
		inFlight:              make(map[string]*inFlightCommand),
		unackedCount:          make(map[cmdKey]int),
		dispatchedInBoot:      make(map[cmdKey]bool),
		pendingWrites:         make(map[string]pendingCommandWrite),
		lastGCSHB:             make(map[uint8]time.Time),
		leaseWake:             make(chan struct{}, 1),
		persistWake:           make(chan struct{}, 1),
	}
	if _, ok := opts.Store.(RecoverableCommandStore); !ok {
		svc.recovered.Store(true)
	}
	return svc, nil
}

// Start launches decoupled real-time lease-deadman scheduling and background command persistence loops.
func (s *Service) Start(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	s.ensureRecovered(ctx)

	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.runCancel != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.runCancel = cancel

	leaseTick := s.tickInterval
	if leaseTick > 10*time.Millisecond {
		leaseTick = 10 * time.Millisecond
	}

	// Real-time deadman & GCS heartbeat loop: strictly decoupled from CommandStore I/O.
	s.leaseWG.Add(1)
	go func() {
		defer s.leaseWG.Done()
		ticker := time.NewTicker(leaseTick)
		defer ticker.Stop()
		var timer *time.Timer
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()

		for {
			var timerCh <-chan time.Time
			if nextExpiry, ok := s.nextLeaseExpiry(); ok {
				wait := nextExpiry.Sub(s.nowFn())
				if wait < 0 {
					wait = 0
				}
				if timer == nil {
					timer = time.NewTimer(wait)
				} else {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(wait)
				}
				timerCh = timer.C
			}

			select {
			case <-runCtx.Done():
				return
			case <-s.leaseWake:
				s.advanceRealtime(s.nowFn(), true)
			case <-timerCh:
				s.advanceRealtime(s.nowFn(), true)
			case <-ticker.C:
				s.advanceRealtime(s.nowFn(), true)
			}
		}
	}()

	// Command timeout, heartbeat reconciliation, and durable transition retry loop.
	s.cmdWG.Add(1)
	go func() {
		defer s.cmdWG.Done()
		ticker := time.NewTicker(s.tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-s.persistWake:
				s.flushPendingWrites(runCtx)
			case <-ticker.C:
				s.advanceCommands(runCtx, s.nowFn())
			}
		}
	}()

	return nil
}

// Stop cancels background loops and waits for completion.
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
		s.leaseWG.Wait()
		s.cmdWG.Wait()
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

func (s *Service) signalLeaseWake() {
	select {
	case s.leaseWake <- struct{}{}:
	default:
	}
}

func (s *Service) signalPersistWake() {
	select {
	case s.persistWake <- struct{}{}:
	default:
	}
}

func (s *Service) nextLeaseExpiry() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var earliest time.Time
	for sysid, lease := range s.leases {
		if s.expiring[sysid].Load() {
			continue
		}
		if earliest.IsZero() || lease.expiresAt.Before(earliest) {
			earliest = lease.expiresAt
		}
	}
	return earliest, !earliest.IsZero()
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

func narrowFiniteFloat32(v float64) (float32, bool) {
	if !isFiniteFloat(v) {
		return 0, false
	}
	f32 := float32(v)
	if math.IsNaN(float64(f32)) || math.IsInf(float64(f32), 0) {
		return 0, false
	}
	return f32, true
}

func isSITLOnlyAction(action string) bool {
	switch action {
	case ActionSITLCompanionBlackout, ActionSITLCRSFOverride, ActionSITLCrossCurrent:
		return true
	default:
		return false
	}
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
	p1, ok1 := narrowFiniteFloat32(req.Param1)
	p2, ok2 := narrowFiniteFloat32(req.Param2)
	p3, ok3 := narrowFiniteFloat32(req.Param3)
	if !ok1 || !ok2 || !ok3 {
		return resolvedCommandSpec{}, &ControlError{
			Status:  http.StatusBadRequest,
			Code:    "non_finite_value",
			Message: "command parameters must be finite float32 numbers",
		}
	}
	_ = p3
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
		durMs := p1
		if durMs <= 0 {
			durMs = 350
		}
		base.Command = mavcodec.MAVCmdUser1
		base.Param1 = 1
		base.Param2 = durMs
		return resolvedCommandSpec{msg: base, sitlOnly: true}, nil
	case ActionSITLCRSFOverride:
		base.Command = mavcodec.MAVCmdUser1
		base.Param1 = 2
		base.Param2 = p1
		return resolvedCommandSpec{msg: base, sitlOnly: true}, nil
	case ActionSITLCrossCurrent:
		base.Command = mavcodec.MAVCmdUser1
		base.Param1 = 3
		base.Param2 = p1
		base.Param3 = p2
		return resolvedCommandSpec{msg: base, sitlOnly: true}, nil
	default:
		return resolvedCommandSpec{}, &ControlError{
			Status:  http.StatusBadRequest,
			Code:    "unsupported_action",
			Message: fmt.Sprintf("unsupported discrete command action %q", req.Action),
		}
	}
}

func (s *Service) isCurrentBootEntityID(entityID string) bool {
	return s.bootNonce != "" && strings.Contains(entityID, "n"+s.bootNonce+"c")
}

func parseCommandInstance(entityID string) (nano int64, nonce string, seq uint64, ok bool) {
	inst := entityID
	if dot := strings.LastIndexByte(entityID, '.'); dot >= 0 {
		inst = entityID[dot+1:]
	}
	if len(inst) < 7 || inst[0] != 's' {
		return 0, "", 0, false
	}
	tIdx := strings.IndexByte(inst, 't')
	cIdx := strings.LastIndexByte(inst, 'c')
	if tIdx <= 1 || cIdx <= tIdx+2 || cIdx >= len(inst)-1 {
		return 0, "", 0, false
	}
	nIdx := strings.LastIndexByte(inst[:cIdx], 'n')
	if nIdx <= tIdx+1 || nIdx >= cIdx-1 {
		return 0, "", 0, false
	}
	if _, err := strconv.ParseUint(inst[1:tIdx], 10, 16); err != nil {
		return 0, "", 0, false
	}
	parsedNano, err := strconv.ParseInt(inst[tIdx+1:nIdx], 36, 64)
	if err != nil {
		return 0, "", 0, false
	}
	parsedSeq, err := strconv.ParseUint(inst[cIdx+1:], 10, 64)
	if err != nil {
		return 0, "", 0, false
	}
	return parsedNano, inst[nIdx+1 : cIdx], parsedSeq, true
}

func isNewerRecoveredEntity(candidate, incumbent CommandEntity) bool {
	candNano, candNonce, candSeq, candOK := parseCommandInstance(candidate.ID)
	incNano, incNonce, incSeq, incOK := parseCommandInstance(incumbent.ID)
	if candOK && incOK && candNonce == incNonce && candSeq != incSeq {
		return candSeq > incSeq
	}
	if candidate.IssuedMs != incumbent.IssuedMs {
		return candidate.IssuedMs > incumbent.IssuedMs
	}
	if candOK && incOK && candNano != incNano {
		return candNano > incNano
	}
	if candidate.UpdatedMs != incumbent.UpdatedMs {
		return candidate.UpdatedMs > incumbent.UpdatedMs
	}
	return candidate.ID > incumbent.ID
}

func (s *Service) ensureRecovered(ctx context.Context) {
	if s.recovered.Load() {
		return
	}
	recStore, ok := s.store.(RecoverableCommandStore)
	if !ok {
		s.recovered.Store(true)
		return
	}

	s.recoverMu.Lock()
	defer s.recoverMu.Unlock()
	if s.recovered.Load() {
		return
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
	}

	allCmds, err := recStore.ListCommands(ctx)
	if err != nil {
		s.logger.Debug("control: deferred command history recovery", slog.Any("error", err))
		return
	}

	now := s.nowFn().UTC()
	bootNs := s.bootTime.UTC().UnixNano()
	updatedMs := now.UnixMilli()

	preRestartSent := make([]CommandEntity, 0, len(allCmds))
	latestSentByKey := make(map[cmdKey]CommandEntity)
	for _, ce := range allCmds {
		if ce.ID == "" || ce.SysID < 1 || ce.SysID > 254 || s.isCurrentBootEntityID(ce.ID) {
			continue
		}
		if ce.AckResult == UnsentAckResult {
			continue
		}
		preRestartSent = append(preRestartSent, ce)
		key := cmdKey{sysid: uint8(ce.SysID), cmdID: uint16(ce.CmdID)}
		if cur, exists := latestSentByKey[key]; !exists || isNewerRecoveredEntity(ce, cur) {
			latestSentByKey[key] = ce
		}
	}

	s.mu.Lock()
	for _, ce := range preRestartSent {
		key := cmdKey{sysid: uint8(ce.SysID), cmdID: uint16(ce.CmdID)}
		if ce.AckResult == UnacknowledgedResult || ce.PhaseState == PhasePending {
			s.unackedCount[key]++
			for _, curCmd := range s.inFlight {
				if curCmd.sysid == key.sysid && curCmd.cmdID == key.cmdID {
					curCmd.ackAmbiguous = true
				}
			}
		}
		if ce.PhaseState == PhasePending {
			s.enqueueWriteLocked(pendingCommandWrite{
				entityID:  ce.ID,
				phase:     PhaseCommitUnknown,
				note:      "recovered_indeterminate_after_restart",
				source:    lifecycle.TransitionSourceComponent,
				ackResult: UnacknowledgedResult,
				detail:    CommitUnknownDetail,
				updatedMs: updatedMs,
			})
		}
		if s.dispatchedInBoot[key] || latestSentByKey[key].ID != ce.ID || commandTransitions.IsTerminal(ce.PhaseState) {
			continue
		}
		if _, exists := s.inFlight[ce.ID]; exists {
			continue
		}
		unknownNs := bootNs
		if ceUpdatedNs := ce.UpdatedMs * int64(time.Millisecond); ceUpdatedNs > unknownNs {
			unknownNs = ceUpdatedNs
		}
		if target, ok := s.targets.ActuationTarget(uint8(ce.SysID)); ok && target.HeartbeatObservedUnixNs > unknownNs {
			unknownNs = target.HeartbeatObservedUnixNs
		}
		seq := s.cmdCounter.Add(1)
		s.inFlight[ce.ID] = &inFlightCommand{
			entityID:        ce.ID,
			orderSeq:        seq,
			sysid:           uint8(ce.SysID),
			cmdID:           uint16(ce.CmdID),
			action:          ce.Action,
			sitlOnly:        isSITLOnlyAction(ce.Action),
			dispatch:        dispatchSent,
			phase:           PhaseCommitUnknown,
			detail:          CommitUnknownDetail,
			ackAmbiguous:    true,
			ackDeadline:     now,
			unknownAtUnixNs: unknownNs,
		}
	}
	hasWrites := len(s.pendingWrites) > 0
	s.recovered.Store(true)
	s.mu.Unlock()

	if hasWrites {
		s.flushPendingWrites(ctx)
	}
}

func (s *Service) enqueueWriteLocked(w pendingCommandWrite) {
	if prev, exists := s.pendingWrites[w.entityID]; exists {
		if commandTransitions.IsTerminal(prev.phase) {
			return
		}
	}
	s.pendingWrites[w.entityID] = w
}

func (s *Service) flushPendingWrites(ctx context.Context) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
	}

	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.Lock()
	if len(s.pendingWrites) == 0 {
		s.mu.Unlock()
		return
	}
	batch := make([]pendingCommandWrite, 0, len(s.pendingWrites))
	for _, w := range s.pendingWrites {
		batch = append(batch, w)
	}
	s.mu.Unlock()

	recStore, hasReader := s.store.(RecoverableCommandStore)
	for _, w := range batch {
		if ctx.Err() != nil {
			return
		}
		err := s.store.Transition(
			ctx,
			w.entityID,
			w.phase,
			w.note,
			w.source,
			w.ackResult,
			w.detail,
			w.updatedMs,
		)
		verified := err == nil
		if !verified && hasReader && ctx.Err() == nil {
			if got, getErr := recStore.Get(ctx, w.entityID); getErr == nil && got != nil {
				if got.PhaseState == w.phase || commandTransitions.IsTerminal(got.PhaseState) {
					verified = true
				}
			}
		}
		if verified {
			s.mu.Lock()
			if cur, exists := s.pendingWrites[w.entityID]; exists && cur.phase == w.phase {
				delete(s.pendingWrites, w.entityID)
			}
			s.mu.Unlock()
			continue
		}
		s.logger.Warn("control: transient command state write failed; retaining for retry",
			slog.String("command_id", w.entityID),
			slog.String("phase", w.phase),
			slog.Any("error", err),
		)
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

func validateTeleopTarget(sysid uint8, target mavlink.ActuationTarget, ok bool) error {
	if !ok || !target.PeerAddr.IsValid() {
		return &ControlError{
			Status:  http.StatusNotFound,
			Code:    "vehicle_not_admitted",
			Message: fmt.Sprintf("vehicle sysid %d is not admitted", sysid),
		}
	}
	if target.LinkLost {
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "vehicle_link_lost",
			Message: fmt.Sprintf("vehicle sysid %d link is silent (LINK LOST)", sysid),
		}
	}
	if target.LinkTier == uxsvocab.LinkTierMeshSupervisory {
		return &ControlError{
			Status:  http.StatusForbidden,
			Code:    "supervisory_link_tier",
			Message: "continuous #84 velocity teleop is prohibited on mesh-supervisory links",
		}
	}
	if !target.Armed {
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "vehicle_disarmed",
			Message: fmt.Sprintf("vehicle sysid %d is disarmed", sysid),
		}
	}
	if target.Authority != uxsvocab.AuthorityCompanionTeleop {
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "authority_not_teleop",
			Message: fmt.Sprintf("vehicle sysid %d authority is %q (companion-teleop required)", sysid, target.Authority),
		}
	}
	return nil
}

func validateCommandTarget(sysid uint8, actionSlug string, sitlOnly bool, target mavlink.ActuationTarget, ok bool) error {
	if !ok || !target.PeerAddr.IsValid() {
		return &ControlError{
			Status:  http.StatusNotFound,
			Code:    "vehicle_not_admitted",
			Message: fmt.Sprintf("vehicle sysid %d is not admitted", sysid),
		}
	}
	if target.LinkLost {
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "vehicle_link_lost",
			Message: fmt.Sprintf("vehicle sysid %d link is silent (LINK LOST)", sysid),
		}
	}
	if sitlOnly && target.LinkTier != uxsvocab.LinkTierSITLLocal {
		return &ControlError{
			Status:  http.StatusForbidden,
			Code:    "sitl_only_action",
			Message: fmt.Sprintf("action %q is restricted to sitl-local vehicles (current link_tier=%q)", actionSlug, target.LinkTier),
		}
	}
	if !sitlOnly {
		if target.Authority == uxsvocab.AuthorityDirectCRSFOverride {
			return &ControlError{
				Status:  http.StatusConflict,
				Code:    "authority_preempted",
				Message: "hardware RC pilot override (direct-crsf-override) holds actuator authority",
			}
		}
		if target.Authority == uxsvocab.AuthorityMCUFailsafe && target.Armed && actionSlug != ActionDisarm {
			return &ControlError{
				Status:  http.StatusConflict,
				Code:    "authority_preempted",
				Message: "MCU hardware failsafe (mcu-failsafe) holds actuator authority",
			}
		}
	}
	return nil
}

func (s *Service) recordStoppedSessionLocked(sysid uint8, session string) {
	if session == "" {
		return
	}
	ring := s.stoppedSessions[sysid]
	if ring == nil {
		ring = &stoppedSessionRing{}
		s.stoppedSessions[sysid] = ring
	}
	ring.add(session)
}

func (s *Service) checkTeleopSessionLocked(sysid uint8, session string, seq uint64, engage bool) error {
	if session != "" && s.stoppedSessions[sysid].contains(session) {
		if s.preemptLatched[sysid] {
			return &ControlError{
				Status:  http.StatusConflict,
				Code:    "preemption_latched",
				Message: fmt.Sprintf("vehicle sysid %d teleop session %q was preempted — explicit re-engage required", sysid, session),
			}
		}
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "teleop_session_stopped",
			Message: fmt.Sprintf("teleop session %q on vehicle sysid %d has already disengaged", session, sysid),
		}
	}
	if s.preemptLatched[sysid] && !engage {
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "preemption_latched",
			Message: fmt.Sprintf("vehicle sysid %d teleop was preempted — explicit re-engage required", sysid),
		}
	}
	cur := s.leases[sysid]
	if !engage {
		if cur == nil {
			return &ControlError{
				Status:  http.StatusConflict,
				Code:    "teleop_not_engaged",
				Message: fmt.Sprintf("vehicle sysid %d has no active teleop lease — explicit engage required", sysid),
			}
		}
		if session != "" && cur.session != "" && session != cur.session {
			return &ControlError{
				Status:  http.StatusConflict,
				Code:    "teleop_session_superseded",
				Message: fmt.Sprintf("teleop session %q was superseded on vehicle sysid %d", session, sysid),
			}
		}
	}
	if cur != nil && seq > 0 && cur.lastSeq > 0 && session == cur.session && seq <= cur.lastSeq {
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "teleop_seq_superseded",
			Message: fmt.Sprintf("teleop sequence %d is older than latest %d on vehicle sysid %d", seq, cur.lastSeq, sysid),
		}
	}
	return nil
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
	vx32, okVx := narrowFiniteFloat32(req.Vx)
	vy32, okVy := narrowFiniteFloat32(req.Vy)
	vz32, okVz := narrowFiniteFloat32(req.Vz)
	yaw32, okYaw := narrowFiniteFloat32(req.YawRate)
	if !okVx || !okVy || !okVz || !okYaw {
		return TeleopResult{}, &ControlError{
			Status:  http.StatusBadRequest,
			Code:    "non_finite_value",
			Message: "teleop velocity and yaw_rate values must be finite float32 numbers",
		}
	}
	coordFrame, err := resolveCoordinateFrame(req.Frame)
	if err != nil {
		return TeleopResult{}, err
	}
	session := strings.TrimSpace(req.Session)

	if req.Stop {
		var releasePeer netip.AddrPort
		var releaseFrame uint8
		var shouldZero bool

		s.egressMu[req.SysID].Lock()
		defer s.egressMu[req.SysID].Unlock()

		s.mu.Lock()
		s.recordStoppedSessionLocked(req.SysID, session)
		prev, hadLease := s.leases[req.SysID]
		if hadLease && (session == "" || prev.session == "" || prev.session == session) {
			delete(s.leases, req.SysID)
			s.authEpoch[req.SysID]++
			releaseFrame = prev.coordFrame
			releasePeer = prev.peer
			shouldZero = true
		}
		s.mu.Unlock()

		if shouldZero {
			if target, ok := s.targets.ActuationTarget(req.SysID); ok &&
				!target.LinkLost && target.PeerAddr.IsValid() &&
				target.PeerAddr == releasePeer &&
				target.LinkTier != uxsvocab.LinkTierMeshSupervisory &&
				target.Authority == uxsvocab.AuthorityCompanionTeleop {
				_ = s.sendZeroVelocity(req.SysID, releaseFrame, target.PeerAddr, s.nowFn())
			}
		}
		return TeleopResult{SysID: req.SysID, LeaseActive: false}, nil
	}

	initialTarget, ok := s.targets.ActuationTarget(req.SysID)
	if err := validateTeleopTarget(req.SysID, initialTarget, ok); err != nil {
		return TeleopResult{}, err
	}

	now := s.nowFn()
	s.mu.Lock()
	if session != "" && s.stoppedSessions[req.SysID].contains(session) {
		err := s.checkTeleopSessionLocked(req.SysID, session, req.Seq, req.Engage)
		s.mu.Unlock()
		return TeleopResult{}, err
	}
	if s.preemptLatched[req.SysID] && !req.Engage {
		s.mu.Unlock()
		return TeleopResult{}, &ControlError{
			Status:  http.StatusConflict,
			Code:    "preemption_latched",
			Message: fmt.Sprintf("vehicle sysid %d teleop was preempted — explicit re-engage required", req.SysID),
		}
	}
	startEpoch := s.authEpoch[req.SysID]
	lastHB, seenHB := s.lastGCSHB[req.SysID]
	shouldSendGCSHB := (req.Engage || s.leases[req.SysID] != nil) &&
		(!seenHB || !now.Before(lastHB.Add(s.gcsHeartbeatInterval)))
	s.mu.Unlock()

	if shouldSendGCSHB {
		_ = s.sendGCSHeartbeat(initialTarget.PeerAddr)
	}

	// Per-vehicle serialized egress: validate target, pinned peer, authority epoch, and
	// session/sequence ordering, transmit #84 while holding egressMu[sysid], and commit the
	// lease only once SendFrame succeeds so failed renewals preserve any active deadline.
	s.egressMu[req.SysID].Lock()

	s.mu.Lock()
	now = s.nowFn()
	expiresAt := now.Add(s.teleopTTL)
	curTarget, curOK := s.targets.ActuationTarget(req.SysID)
	if err := validateTeleopTarget(req.SysID, curTarget, curOK); err != nil {
		s.mu.Unlock()
		s.egressMu[req.SysID].Unlock()
		return TeleopResult{}, err
	}
	if curTarget.PeerAddr != initialTarget.PeerAddr {
		s.mu.Unlock()
		s.egressMu[req.SysID].Unlock()
		return TeleopResult{}, &ControlError{
			Status:  http.StatusConflict,
			Code:    "vehicle_peer_changed",
			Message: fmt.Sprintf("vehicle sysid %d pinned peer changed prior to transmit", req.SysID),
		}
	}
	if s.authEpoch[req.SysID] != startEpoch {
		latched := s.preemptLatched[req.SysID]
		s.mu.Unlock()
		s.egressMu[req.SysID].Unlock()
		if latched {
			return TeleopResult{}, &ControlError{
				Status:  http.StatusConflict,
				Code:    "preemption_latched",
				Message: fmt.Sprintf("vehicle sysid %d teleop was preempted prior to transmit — explicit re-engage required", req.SysID),
			}
		}
		return TeleopResult{}, &ControlError{
			Status:  http.StatusConflict,
			Code:    "teleop_not_engaged",
			Message: fmt.Sprintf("vehicle sysid %d teleop lease was revoked prior to transmit", req.SysID),
		}
	}
	if err := s.checkTeleopSessionLocked(req.SysID, session, req.Seq, req.Engage); err != nil {
		s.mu.Unlock()
		s.egressMu[req.SysID].Unlock()
		return TeleopResult{}, err
	}
	effectiveSession := session
	effectiveSeq := req.Seq
	if prev := s.leases[req.SysID]; prev != nil && !req.Engage {
		if effectiveSession == "" {
			effectiveSession = prev.session
		}
		if effectiveSeq == 0 {
			effectiveSeq = prev.lastSeq
		}
	}
	sendPeer := curTarget.PeerAddr
	s.mu.Unlock()

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
			Vx:              vx32,
			Vy:              vy32,
			Vz:              vz32,
			YawRate:         yaw32,
		},
	}, sendPeer); err != nil {
		s.egressMu[req.SysID].Unlock()
		return TeleopResult{}, &ControlError{
			Status:  http.StatusServiceUnavailable,
			Code:    "transport_unavailable",
			Message: err.Error(),
		}
	}

	s.mu.Lock()
	if prev := s.leases[req.SysID]; prev != nil && prev.session != "" && prev.session != effectiveSession {
		s.recordStoppedSessionLocked(req.SysID, prev.session)
	}
	if req.Engage {
		delete(s.preemptLatched, req.SysID)
	}
	s.leases[req.SysID] = &teleopLease{
		peer:       sendPeer,
		coordFrame: coordFrame,
		session:    effectiveSession,
		lastSeq:    effectiveSeq,
		expiresAt:  expiresAt,
	}
	if shouldSendGCSHB {
		s.lastGCSHB[req.SysID] = now
	}
	s.mu.Unlock()
	s.egressMu[req.SysID].Unlock()

	s.signalLeaseWake()

	return TeleopResult{
		SysID:       req.SysID,
		LeaseActive: true,
		ExpiresMs:   expiresAt.UTC().UnixMilli(),
	}, nil
}

// SubmitCommand validates safety gates, persists a pending CommandEntity via lifecycle CAS,
// fences against concurrent preemption/peer revocation, and transmits a single non-retrying
// #76 COMMAND_LONG (`Confirmation = 0`) datagram.
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

	s.ensureRecovered(ctx)

	initialTarget, ok := s.targets.ActuationTarget(req.SysID)
	if err := validateCommandTarget(req.SysID, actionSlug, spec.sitlOnly, initialTarget, ok); err != nil {
		return CommandResult{}, err
	}

	now := s.nowFn().UTC()
	s.mu.Lock()
	lastHB, seenHB := s.lastGCSHB[req.SysID]
	shouldSendGCSHB := !seenHB || !now.Before(lastHB.Add(s.gcsHeartbeatInterval))
	s.mu.Unlock()

	if shouldSendGCSHB {
		_ = s.sendGCSHeartbeat(initialTarget.PeerAddr)
	}

	now = s.nowFn().UTC()
	ackWait := s.commandAckTimeout
	if initialTarget.LinkTier == uxsvocab.LinkTierMeshSupervisory {
		ackWait = s.supervisoryAckTimeout
	}

	cmdSeq := s.cmdCounter.Add(1)
	instance := fmt.Sprintf("s%dt%sn%sc%d", req.SysID, strconv.FormatInt(now.UnixNano(), 36), s.bootNonce, cmdSeq)
	entityID, err := CommandEntityID(s.org, s.platform, instance)
	if err != nil {
		return CommandResult{}, err
	}

	s.mu.Lock()
	curTarget, curOK := s.targets.ActuationTarget(req.SysID)
	if err := validateCommandTarget(req.SysID, actionSlug, spec.sitlOnly, curTarget, curOK); err != nil {
		s.mu.Unlock()
		return CommandResult{}, err
	}
	if curTarget.PeerAddr != initialTarget.PeerAddr {
		s.mu.Unlock()
		return CommandResult{}, &ControlError{
			Status:  http.StatusConflict,
			Code:    "vehicle_peer_changed",
			Message: fmt.Sprintf("vehicle sysid %d pinned peer changed prior to command registration", req.SysID),
		}
	}

	for existingID, existing := range s.inFlight {
		if existing.sysid != req.SysID || existing.cmdID != spec.msg.Command {
			continue
		}
		if existing.dispatch == dispatchReserved || existing.phase == PhasePending {
			// Emergency Disarm MUST remain available even if Arm (MAV_CMD 400) is reserved, sending, or pending.
			if actionSlug == ActionDisarm && existing.action != ActionDisarm {
				if existing.dispatch == dispatchReserved {
					existing.abortedNote = "superseded_before_send"
					existing.abortedDetail = "Superseded by Disarm prior to UDP command transmit"
					delete(s.inFlight, existingID)
				}
				continue
			}
			s.mu.Unlock()
			return CommandResult{}, &ControlError{
				Status:  http.StatusConflict,
				Code:    "command_in_flight",
				Message: fmt.Sprintf("command %d is already pending on vehicle sysid %d", spec.msg.Command, req.SysID),
			}
		}
	}

	startEpoch := s.authEpoch[req.SysID]
	pinnedPeer := curTarget.PeerAddr
	inflight := &inFlightCommand{
		entityID: entityID,
		orderSeq: cmdSeq,
		sysid:    req.SysID,
		cmdID:    spec.msg.Command,
		action:   actionSlug,
		sitlOnly: spec.sitlOnly,
		dispatch: dispatchReserved,
		phase:    PhasePending,
		detail:   "Awaiting COMMAND_ACK",
	}
	s.inFlight[entityID] = inflight
	s.mu.Unlock()

	entity := &CommandEntity{
		ID:         entityID,
		PhaseState: PhasePending,
		Target:     curTarget.EntityID,
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
		delete(s.pendingWrites, entityID)
		s.mu.Unlock()
		return CommandResult{}, fmt.Errorf("control: create command entity: %w", err)
	}

	// Per-vehicle serialized egress: verify no preemption, link loss, authority revocation,
	// or peer re-pinning occurred while Create was committing, transmit #76 while holding
	// egressMu[sysid], and start ACK eligibility + timeout strictly at dispatch.
	s.egressMu[req.SysID].Lock()

	postCreateMs := s.nowFn().UTC().UnixMilli()
	s.mu.Lock()
	sendTarget, sendOK := s.targets.ActuationTarget(req.SysID)
	targetErr := validateCommandTarget(req.SysID, actionSlug, spec.sitlOnly, sendTarget, sendOK)
	peerChanged := targetErr == nil && sendTarget.PeerAddr != pinnedPeer
	epochRevoked := !spec.sitlOnly && s.authEpoch[req.SysID] != startEpoch &&
		(s.preemptLatched[req.SysID] || inflight.abortedNote != "")

	if inflight.abortedNote == "" && targetErr == nil && !peerChanged && !epochRevoked {
		for _, other := range s.inFlight {
			if other.entityID == entityID || other.sysid != req.SysID || other.cmdID != spec.msg.Command {
				continue
			}
			if other.dispatch != dispatchReserved && other.phase == PhasePending && (actionSlug != ActionDisarm || other.action == ActionDisarm) {
				targetErr = &ControlError{
					Status:  http.StatusConflict,
					Code:    "command_in_flight",
					Message: fmt.Sprintf("command %d is already pending on vehicle sysid %d", spec.msg.Command, req.SysID),
				}
				break
			}
		}
	}

	if inflight.abortedNote != "" || epochRevoked || targetErr != nil || peerChanged {
		delete(s.inFlight, entityID)
		abortPhase := PhasePreempted
		abortNote := "preempted_before_send"
		abortDetail := "Preempted prior to UDP command transmit"
		if inflight.abortedNote != "" {
			abortNote = inflight.abortedNote
			abortDetail = inflight.abortedDetail
		}
		retErr := targetErr
		if retErr == nil {
			if peerChanged {
				abortPhase = PhaseRejected
				abortNote = "peer_changed_before_send"
				abortDetail = "Pinned vehicle peer changed prior to UDP command transmit"
				retErr = &ControlError{
					Status:  http.StatusConflict,
					Code:    "vehicle_peer_changed",
					Message: fmt.Sprintf("vehicle sysid %d pinned peer changed prior to transmit", req.SysID),
				}
			} else if inflight.abortedNote == "link_lost" {
				retErr = &ControlError{
					Status:  http.StatusConflict,
					Code:    "vehicle_link_lost",
					Message: fmt.Sprintf("vehicle sysid %d link is silent (LINK LOST)", req.SysID),
				}
			} else {
				retErr = &ControlError{
					Status:  http.StatusConflict,
					Code:    "authority_preempted",
					Message: "actuator authority was preempted prior to UDP command transmit",
				}
			}
		} else {
			var ce *ControlError
			if errors.As(retErr, &ce) {
				switch ce.Code {
				case "sitl_only_action", "command_in_flight":
					abortPhase = PhaseRejected
					abortNote = ce.Code + "_before_send"
					abortDetail = ce.Message
				case "vehicle_link_lost":
					abortNote = "link_lost_before_send"
					abortDetail = "Preempted by vehicle link loss prior to transmit"
				}
			}
		}
		s.enqueueWriteLocked(pendingCommandWrite{
			entityID:  entityID,
			phase:     abortPhase,
			note:      abortNote,
			source:    lifecycle.TransitionSourceComponent,
			ackResult: UnsentAckResult,
			detail:    abortDetail,
			updatedMs: postCreateMs,
		})
		s.mu.Unlock()
		s.egressMu[req.SysID].Unlock()
		s.flushPendingWrites(ctx)
		s.signalPersistWake()
		return CommandResult{}, retErr
	}
	inflight.dispatch = dispatchSending
	s.mu.Unlock()

	if err := s.sender.SendFrame(mavcodec.Frame{
		Sequence:    s.nextSeq(),
		SystemID:    mavcodec.GCSSystemID,
		ComponentID: mavcodec.CompIDMissionPlanner,
		Message:     spec.msg,
	}, pinnedPeer); err != nil {
		s.mu.Lock()
		earlyAcks := inflight.earlyAcks
		inflight.earlyAcks = nil
		delete(s.inFlight, entityID)
		s.enqueueWriteLocked(pendingCommandWrite{
			entityID:  entityID,
			phase:     PhaseRejected,
			note:      "udp_send_failed",
			source:    lifecycle.TransitionSourceComponent,
			ackResult: UnsentAckResult,
			detail:    "UDP command transmit failed",
			updatedMs: s.nowFn().UTC().UnixMilli(),
		})
		s.mu.Unlock()
		s.egressMu[req.SysID].Unlock()
		s.flushPendingWrites(ctx)
		s.signalPersistWake()
		for _, ea := range earlyAcks {
			s.OnCommandAck(req.SysID, ea.ack, ea.observedAt)
		}
		return CommandResult{}, &ControlError{
			Status:  http.StatusServiceUnavailable,
			Code:    "transport_unavailable",
			Message: err.Error(),
		}
	}

	dispatchNow := s.nowFn().UTC()
	hasSupersededWrite := false
	s.mu.Lock()
	hbBaselineNs := dispatchNow.UnixNano()
	if curT, ok := s.targets.ActuationTarget(req.SysID); ok && curT.HeartbeatObservedUnixNs > hbBaselineNs {
		hbBaselineNs = curT.HeartbeatObservedUnixNs
	}
	key := cmdKey{sysid: req.SysID, cmdID: spec.msg.Command}
	for otherID, other := range s.inFlight {
		if otherID == entityID || other.sysid != req.SysID || other.cmdID != spec.msg.Command || other.dispatch != dispatchSent {
			continue
		}
		delete(s.inFlight, otherID)
		if other.phase == PhasePending {
			other.phase = PhaseCommitUnknown
			other.detail = CommitUnknownDetail
			s.enqueueWriteLocked(pendingCommandWrite{
				entityID:  other.entityID,
				phase:     PhaseCommitUnknown,
				note:      "superseded_by_emergency_disarm",
				source:    lifecycle.TransitionSourceComponent,
				ackResult: UnacknowledgedResult,
				detail:    CommitUnknownDetail,
				updatedMs: dispatchNow.UnixMilli(),
			})
			hasSupersededWrite = true
		}
	}
	priorUnacked := s.unackedCount[key] > 0
	s.unackedCount[key]++
	s.dispatchedInBoot[key] = true
	inflight.dispatch = dispatchSent
	inflight.ackAmbiguous = !s.recovered.Load() || priorUnacked
	inflight.ackDeadline = dispatchNow.Add(ackWait)
	inflight.unknownAtUnixNs = hbBaselineNs
	earlyAcks := inflight.earlyAcks
	inflight.earlyAcks = nil
	if shouldSendGCSHB {
		s.lastGCSHB[req.SysID] = dispatchNow
	}
	s.mu.Unlock()
	s.egressMu[req.SysID].Unlock()

	if hasSupersededWrite {
		s.flushPendingWrites(ctx)
		s.signalPersistWake()
	}
	for _, ea := range earlyAcks {
		s.OnCommandAck(req.SysID, ea.ack, ea.observedAt)
	}
	s.signalLeaseWake()

	resPhase := PhasePending
	resDetail := entity.Detail
	if len(earlyAcks) > 0 {
		s.mu.Lock()
		resPhase = inflight.phase
		resDetail = inflight.detail
		s.mu.Unlock()
	}

	return CommandResult{
		CommandID: entityID,
		SysID:     req.SysID,
		CmdID:     spec.msg.Command,
		Action:    actionSlug,
		Phase:     resPhase,
		Detail:    resDetail,
		IssuedMs:  entity.IssuedMs,
	}, nil
}

// OnCommandAck correlates an inbound #77 COMMAND_ACK from a pinned vehicle peer
// against any dispatched discrete command for (sysid, ack.Command).
func (s *Service) OnCommandAck(sysid uint8, ack mavcodec.CommandAck, observedAt time.Time) {
	if ack.Result == mavcodec.MAVResultInProgress {
		return
	}
	if observedAt.IsZero() {
		observedAt = s.nowFn()
	}
	obsUTC := observedAt.UTC()
	updatedMs := obsUTC.UnixMilli()
	obsNs := obsUTC.UnixNano()
	key := cmdKey{sysid: sysid, cmdID: ack.Command}

	s.mu.Lock()
	for _, cmd := range s.inFlight {
		if cmd.dispatch == dispatchSending && cmd.sysid == sysid && cmd.cmdID == ack.Command {
			cmd.earlyAcks = append(cmd.earlyAcks, earlyCommandAck{
				ack:        ack,
				observedAt: obsUTC,
			})
			s.mu.Unlock()
			return
		}
	}

	var matches []*inFlightCommand
	for _, cmd := range s.inFlight {
		if cmd.dispatch == dispatchSent && cmd.sysid == sysid && cmd.cmdID == ack.Command &&
			(cmd.phase == PhasePending || cmd.phase == PhaseCommitUnknown) {
			matches = append(matches, cmd)
		}
	}
	priorUnacked := s.unackedCount[key]
	if len(matches) == 0 {
		if s.recovered.Load() && priorUnacked > 0 {
			s.unackedCount[key] = priorUnacked - 1
		}
		s.mu.Unlock()
		return
	}

	ambiguous := !s.recovered.Load() || priorUnacked > 1 || len(matches) > 1 || matches[0].ackAmbiguous
	if s.recovered.Load() && priorUnacked > 0 {
		s.unackedCount[key] = priorUnacked - 1
	}
	if ambiguous {
		hbFloor := obsNs
		if target, ok := s.targets.ActuationTarget(sysid); ok && target.HeartbeatObservedUnixNs > hbFloor {
			hbFloor = target.HeartbeatObservedUnixNs
		}
		wroteAny := false
		for _, cmd := range matches {
			cmd.ackAmbiguous = true
			if hbFloor > cmd.unknownAtUnixNs {
				cmd.unknownAtUnixNs = hbFloor
			}
			if cmd.phase == PhasePending {
				cmd.phase = PhaseCommitUnknown
				cmd.detail = CommitUnknownDetail
				s.enqueueWriteLocked(pendingCommandWrite{
					entityID:  cmd.entityID,
					phase:     PhaseCommitUnknown,
					note:      "command_ack_ambiguous",
					source:    lifecycle.TransitionSourceComponent,
					ackResult: UnacknowledgedResult,
					detail:    CommitUnknownDetail,
					updatedMs: updatedMs,
				})
				wroteAny = true
			}
		}
		s.mu.Unlock()
		if wroteAny {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			s.flushPendingWrites(ctx)
			s.signalPersistWake()
		}
		return
	}

	matched := matches[0]
	s.unackedCount[key] = 0
	nextPhase := PhaseRejected
	note := "command_ack_rejected"
	detail := formatAckResultDetail(ack.Result)
	if ack.Result == mavcodec.MAVResultAccepted {
		nextPhase = PhaseConfirmed
		note = "command_ack_accepted"
	}
	matched.phase = nextPhase
	matched.detail = detail
	delete(s.inFlight, matched.entityID)
	s.enqueueWriteLocked(pendingCommandWrite{
		entityID:  matched.entityID,
		phase:     nextPhase,
		note:      note,
		source:    lifecycle.TransitionSourceComponent,
		ackResult: int(ack.Result),
		detail:    detail,
		updatedMs: updatedMs,
	})
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s.flushPendingWrites(ctx)
	s.signalPersistWake()
}

// OnTransitionEvent reacts synchronously to Lane 3 authority, LinkTier, and link-loss edges,
// revoking #84 velocity leases, latching preemption, preempting pending discrete commands,
// and reconciling commit_unknown commands only on fresh post-timeout #0 HEARTBEAT state evidence.
func (s *Service) OnTransitionEvent(ev mavlink.TransitionEvent) {
	isPreempt := ev.Kind == mavlink.EventKindLinkLost ||
		ev.Authority == uxsvocab.AuthorityDirectCRSFOverride ||
		ev.Authority == uxsvocab.AuthorityMCUFailsafe

	updatedMs := ev.Timestamp
	if updatedMs <= 0 {
		updatedMs = s.nowFn().UTC().UnixMilli()
	}

	isHeartbeatEvidence := ev.MsgID == mavcodec.MsgIDHeartbeat &&
		(ev.Kind == mavlink.EventKindHeartbeatEdge || ev.Kind == mavlink.EventKindLinkRestored)

	if isValidVehicleSysID(ev.SysID) {
		s.egressMu[ev.SysID].Lock()
	}
	s.mu.Lock()
	wroteAny := false
	if isPreempt {
		s.authEpoch[ev.SysID]++
		if prev, had := s.leases[ev.SysID]; had {
			s.recordStoppedSessionLocked(ev.SysID, prev.session)
			delete(s.leases, ev.SysID)
		}
		s.preemptLatched[ev.SysID] = true

		preemptNote := "authority_preempted"
		preemptDetail := fmt.Sprintf("Preempted by %s", ev.Authority)
		if ev.Kind == mavlink.EventKindLinkLost {
			preemptNote = "link_lost"
			preemptDetail = "Preempted by vehicle link loss"
		}
		for id, cmd := range s.inFlight {
			if cmd.sysid == ev.SysID && !cmd.sitlOnly {
				delete(s.inFlight, id)
				if cmd.dispatch == dispatchReserved {
					cmd.abortedNote = preemptNote
					cmd.abortedDetail = preemptDetail
					continue
				}
				cmd.phase = PhasePreempted
				cmd.detail = preemptDetail
				s.enqueueWriteLocked(pendingCommandWrite{
					entityID:  cmd.entityID,
					phase:     PhasePreempted,
					note:      preemptNote,
					source:    lifecycle.TransitionSourceComponent,
					ackResult: UnacknowledgedResult,
					detail:    preemptDetail,
					updatedMs: updatedMs,
				})
				wroteAny = true
			}
		}
	} else {
		if ev.LinkTier == uxsvocab.LinkTierMeshSupervisory ||
			!ev.Armed ||
			ev.Authority != uxsvocab.AuthorityCompanionTeleop {
			if prev, had := s.leases[ev.SysID]; had {
				s.authEpoch[ev.SysID]++
				s.recordStoppedSessionLocked(ev.SysID, prev.session)
				delete(s.leases, ev.SysID)
			}
		}
		if isHeartbeatEvidence {
			if target, ok := s.targets.ActuationTarget(ev.SysID); ok && !target.LinkLost {
				for id, cmd := range s.inFlight {
					if cmd.dispatch == dispatchSent &&
						cmd.sysid == ev.SysID &&
						cmd.phase == PhaseCommitUnknown &&
						target.HeartbeatObservedUnixNs > cmd.unknownAtUnixNs &&
						matchesExpectedVehicleState(cmd.action, target.Armed, target.CompanionMode, target.Authority) {
						cmd.phase = PhaseConfirmed
						cmd.detail = "Confirmed via vehicle HEARTBEAT state reconciliation"
						delete(s.inFlight, id)
						s.enqueueWriteLocked(pendingCommandWrite{
							entityID:  cmd.entityID,
							phase:     PhaseConfirmed,
							note:      "reconciled_by_heartbeat",
							source:    lifecycle.TransitionSourceComponent,
							ackResult: UnacknowledgedResult,
							detail:    cmd.detail,
							updatedMs: updatedMs,
						})
						wroteAny = true
					}
				}
			}
		}
	}
	s.mu.Unlock()
	if isValidVehicleSysID(ev.SysID) {
		s.egressMu[ev.SysID].Unlock()
	}

	if wroteAny {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.flushPendingWrites(ctx)
		s.signalPersistWake()
	}
}

type gcsHeartbeatTarget struct {
	sysid uint8
	peer  netip.AddrPort
}

func (s *Service) expireVehicleLease(sysid uint8, now time.Time) {
	s.egressMu[sysid].Lock()
	defer s.egressMu[sysid].Unlock()

	var expPeer netip.AddrPort
	var expFrame uint8
	var shouldZero bool
	s.mu.Lock()
	if lease, ok := s.leases[sysid]; ok && !now.Before(lease.expiresAt) {
		expPeer = lease.peer
		expFrame = lease.coordFrame
		s.recordStoppedSessionLocked(sysid, lease.session)
		delete(s.leases, sysid)
		s.authEpoch[sysid]++
		shouldZero = true
	}
	s.mu.Unlock()

	if shouldZero {
		if target, ok := s.targets.ActuationTarget(sysid); ok &&
			!target.LinkLost && target.PeerAddr.IsValid() &&
			target.PeerAddr == expPeer &&
			target.LinkTier != uxsvocab.LinkTierMeshSupervisory &&
			target.Authority == uxsvocab.AuthorityCompanionTeleop {
			_ = s.sendZeroVelocity(sysid, expFrame, target.PeerAddr, now)
		}
	}
}

// advanceRealtime evaluates < 250ms #84 lease deadman expiry and 1 Hz outbound GCS #0 HEARTBEAT
// liveness without touching CommandStore persistence. When async is true (background real-time
// scheduler), per-vehicle egress is isolated so a blocked writer on one vehicle cannot starve
// another vehicle's deadman deadline.
func (s *Service) advanceRealtime(now time.Time, async bool) {
	var expiredSysIDs []uint8
	activeSysIDs := make(map[uint8]struct{})

	s.mu.Lock()
	for sysid, lease := range s.leases {
		if !now.Before(lease.expiresAt) {
			if !async || !s.expiring[sysid].Load() {
				expiredSysIDs = append(expiredSysIDs, sysid)
			}
			continue
		}
		activeSysIDs[sysid] = struct{}{}
	}
	for _, cmd := range s.inFlight {
		if cmd.dispatch == dispatchSent && cmd.phase == PhasePending && now.Before(cmd.ackDeadline) {
			activeSysIDs[cmd.sysid] = struct{}{}
		}
	}

	var gcsTargets []gcsHeartbeatTarget
	for sysid := range activeSysIDs {
		if async && s.gcsSending[sysid].Load() {
			continue
		}
		lastHB, seen := s.lastGCSHB[sysid]
		if !seen || !now.Before(lastHB.Add(s.gcsHeartbeatInterval)) {
			if target, ok := s.targets.ActuationTarget(sysid); ok && !target.LinkLost && target.PeerAddr.IsValid() {
				s.lastGCSHB[sysid] = now
				gcsTargets = append(gcsTargets, gcsHeartbeatTarget{
					sysid: sysid,
					peer:  target.PeerAddr,
				})
			}
		}
	}
	for sysid := range s.lastGCSHB {
		if _, active := activeSysIDs[sysid]; !active {
			delete(s.lastGCSHB, sysid)
		}
	}
	s.mu.Unlock()

	if !async {
		for _, sysid := range expiredSysIDs {
			s.expireVehicleLease(sysid, now)
		}
		for _, gt := range gcsTargets {
			_ = s.sendGCSHeartbeat(gt.peer)
		}
		return
	}

	for _, sysid := range expiredSysIDs {
		if s.expiring[sysid].CompareAndSwap(false, true) {
			s.leaseWG.Add(1)
			go func(id uint8, expNow time.Time) {
				defer s.leaseWG.Done()
				defer func() {
					s.expiring[id].Store(false)
					s.signalLeaseWake()
				}()
				checkNow := s.nowFn()
				if expNow.After(checkNow) {
					checkNow = expNow
				}
				s.expireVehicleLease(id, checkNow)
			}(sysid, now)
		}
	}

	for _, gt := range gcsTargets {
		if s.gcsSending[gt.sysid].CompareAndSwap(false, true) {
			s.leaseWG.Add(1)
			go func(id uint8, peer netip.AddrPort) {
				defer s.leaseWG.Done()
				defer s.gcsSending[id].Store(false)
				_ = s.sendGCSHeartbeat(peer)
			}(gt.sysid, gt.peer)
		}
	}
}

func (s *Service) advanceCommands(ctx context.Context, now time.Time) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.ensureRecovered(ctx)

	nowUTC := now.UTC()
	nowNs := nowUTC.UnixNano()
	updatedMs := nowUTC.UnixMilli()

	s.mu.Lock()
	for id, cmd := range s.inFlight {
		if cmd.dispatch != dispatchSent {
			continue
		}
		if cmd.phase == PhasePending && !now.Before(cmd.ackDeadline) {
			cmd.phase = PhaseCommitUnknown
			cmd.detail = CommitUnknownDetail
			hbFloor := nowNs
			if target, ok := s.targets.ActuationTarget(cmd.sysid); ok && target.HeartbeatObservedUnixNs > hbFloor {
				hbFloor = target.HeartbeatObservedUnixNs
			}
			if hbFloor > cmd.unknownAtUnixNs {
				cmd.unknownAtUnixNs = hbFloor
			}
			s.enqueueWriteLocked(pendingCommandWrite{
				entityID:  cmd.entityID,
				phase:     PhaseCommitUnknown,
				note:      "command_ack_timeout",
				source:    lifecycle.TransitionSourceComponent,
				ackResult: UnacknowledgedResult,
				detail:    CommitUnknownDetail,
				updatedMs: updatedMs,
			})
			continue
		}
		if cmd.phase == PhaseCommitUnknown {
			if target, ok := s.targets.ActuationTarget(cmd.sysid); ok && !target.LinkLost &&
				target.HeartbeatObservedUnixNs > cmd.unknownAtUnixNs &&
				matchesExpectedVehicleState(cmd.action, target.Armed, target.CompanionMode, target.Authority) {
				cmd.phase = PhaseConfirmed
				cmd.detail = "Confirmed via vehicle HEARTBEAT state reconciliation"
				delete(s.inFlight, id)
				s.enqueueWriteLocked(pendingCommandWrite{
					entityID:  cmd.entityID,
					phase:     PhaseConfirmed,
					note:      "reconciled_by_heartbeat",
					source:    lifecycle.TransitionSourceComponent,
					ackResult: UnacknowledgedResult,
					detail:    cmd.detail,
					updatedMs: updatedMs,
				})
				continue
			}
		}
	}
	hasPendingWrites := len(s.pendingWrites) > 0
	s.mu.Unlock()

	if hasPendingWrites {
		s.flushPendingWrites(ctx)
	}
}

// Advance evaluates #84 lease expiry (< 250ms deadman), 1 Hz outbound GCS #0 HEARTBEAT liveness,
// non-retrying #76 -> commit_unknown timeouts, fresh-heartbeat reconciliation, and retained store writes.
func (s *Service) Advance(ctx context.Context, now time.Time) {
	s.advanceRealtime(now, false)
	s.advanceCommands(ctx, now)
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
