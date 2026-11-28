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
	"slices"
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

// ObservationResolver optionally resolves a vehicle observation (optional obsRev,
// authorityEpoch, and UTC observedMs) to a host-monotonic observation revision (`*mavlink.Tracker`).
type ObservationResolver interface {
	ResolveObservation(sysid uint8, obsRev, authorityEpoch uint64, observedMs int64) (uint64, bool)
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

// RecoverableCommandStore extends CommandStore with authoritative reads, owner-neutral
// consumed-ACK pool updates, and full-history enumeration for restart recovery and ambiguity preservation.
type RecoverableCommandStore interface {
	CommandStore
	RecordConsumedAck(ctx context.Context, entityID string, ackAccounted, ackPoolMask int) error
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
		if ackResult >= 0 {
			ce.AckAccounted = 1
			ce.AckPoolMask |= ackPoolFormatV2Bit
			if ackResult < 27 {
				ce.AckPoolMask |= 1 << uint(ackResult)
			}
		}
		ce.Detail = detail
		ce.UpdatedMs = updatedMs
		return nil
	})
}

// RecordConsumedAck executes a revision-fenced CAS triple update recording owner-neutral
// #77 COMMAND_ACK pool accounting on an already-created CommandEntity without mutating
// its PhaseState, UpdatedMs transition timestamp, or attributing AckResult to a specific command.
func (s *LifecycleStore) RecordConsumedAck(ctx context.Context, entityID string, ackAccounted, ackPoolMask int) error {
	return s.mgr.UpdateFromOperator(ctx, WorkflowName, entityID, map[string]any{
		"ack_accounted": ackAccounted,
		"ack_pool_mask": ackPoolMask,
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
	SysID                uint8   `json:"sysid"`
	Session              string  `json:"session,omitempty"`
	Seq                  uint64  `json:"seq,omitempty"`
	Frame                string  `json:"frame,omitempty"`
	Vx                   float64 `json:"vx"`
	Vy                   float64 `json:"vy"`
	Vz                   float64 `json:"vz"`
	YawRate              float64 `json:"yaw_rate"`
	Engage               bool    `json:"engage,omitempty"`
	Stop                 bool    `json:"stop,omitempty"`
	ObservedMs           int64   `json:"observed_ms,omitempty"`
	ObservationRev       uint64  `json:"observation_rev,omitempty"`
	AuthorityIncarnation string  `json:"authority_incarnation,omitempty"`
	AuthorityEpoch       uint64  `json:"authority_epoch,omitempty"`
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
	peer               netip.AddrPort
	coordFrame         uint8
	session            string
	lastSeq            uint64
	engagedObsRev      uint64
	engagedObsMs       int64
	engagedEpoch       uint64
	engagedIncarnation string
	expiresAt          time.Time
}

type preemptRecoveryState struct {
	latched             bool
	preemptedAtMs       int64
	preemptEpoch        uint64
	preemptObsRev       uint64
	recoveryReady       bool
	recoveryObservedMs  int64
	recoveryObsRev      uint64
	recoveryEpoch       uint64
	recoveryIncarnation string
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
	peer  netip.AddrPort
}

type cmdDispatchState uint8

const (
	dispatchReserved cmdDispatchState = iota
	dispatchSending
	dispatchSent
)

type earlyCommandAck struct {
	peer       netip.AddrPort
	seq        uint8
	hasSeq     bool
	uncertain  bool
	ack        mavcodec.CommandAck
	observedAt time.Time
}

type serviceAckSlot struct {
	active      bool
	ack         mavcodec.CommandAck
	streamOrder uint64
}

type directSeqTracker struct {
	hasSeq           bool
	lastSeq          uint8
	pos              uint64
	hasPendingGap    bool
	pendingGapSeq    uint8
	pendingGapDelta  uint8
	pendingGapHasAck bool
	pendingGapAck    mavcodec.CommandAck
}

func (d *directSeqTracker) evaluate(seq uint8, msg mavcodec.CommandAck, baseOrder uint64, slots *[256]serviceAckSlot) (candOrder uint64, recordSlot, isDup, uncertain bool) {
	prev := slots[seq]
	if !d.hasSeq {
		d.hasSeq = true
		d.lastSeq = seq
		d.pos = 1
		d.hasPendingGap = false
		d.pendingGapHasAck = false
		return baseOrder + 1, true, false, false
	}
	if d.hasPendingGap {
		gapStep := uint8(seq - d.pendingGapSeq)
		if gapStep >= 1 && gapStep <= 64 {
			gapRel := d.pos + uint64(d.pendingGapDelta)
			if d.pendingGapHasAck {
				slots[d.pendingGapSeq] = serviceAckSlot{
					active:      true,
					ack:         d.pendingGapAck,
					streamOrder: baseOrder + gapRel,
				}
			}
			nextRel := gapRel + uint64(gapStep)
			cand := baseOrder + nextRel
			dup := prev.active && prev.ack == msg &&
				(cand <= prev.streamOrder || cand-prev.streamOrder < 256)
			d.hasPendingGap = false
			d.pendingGapHasAck = false
			if !dup {
				d.pos = nextRel
				d.lastSeq = seq
			}
			return cand, !dup, dup, false
		}
		if gapStep == 0 {
			fwdOrder := baseOrder + d.pos + uint64(d.pendingGapDelta)
			if d.pendingGapHasAck && d.pendingGapAck == msg {
				return fwdOrder, false, true, false
			}
			if prev.active && prev.ack == msg {
				if fwdOrder <= prev.streamOrder || fwdOrder-prev.streamOrder < 256 {
					return fwdOrder, false, true, false
				}
				d.pendingGapHasAck = true
				d.pendingGapAck = msg
				return fwdOrder, false, false, true
			}
			d.pendingGapHasAck = true
			d.pendingGapAck = msg
			return fwdOrder, false, false, false
		}
		d.hasPendingGap = false
		d.pendingGapHasAck = false
	}
	delta := uint8(seq - d.lastSeq)
	if delta == 0 {
		cand := baseOrder + d.pos
		dup := prev.active && prev.ack == msg &&
			(cand <= prev.streamOrder || cand-prev.streamOrder < 256)
		return cand, !dup, dup, false
	}
	if delta <= 64 {
		nextRel := d.pos + uint64(delta)
		cand := baseOrder + nextRel
		dup := prev.active && prev.ack == msg &&
			(cand <= prev.streamOrder || cand-prev.streamOrder < 256)
		if !dup {
			d.pos = nextRel
			d.lastSeq = seq
		}
		return cand, !dup, dup, false
	}
	if delta <= 128 {
		fwdOrder := baseOrder + d.pos + uint64(delta)
		if prev.active && prev.ack == msg {
			if fwdOrder <= prev.streamOrder || fwdOrder-prev.streamOrder < 256 {
				return fwdOrder, false, true, false
			}
			d.hasPendingGap = true
			d.pendingGapSeq = seq
			d.pendingGapDelta = delta
			d.pendingGapHasAck = true
			d.pendingGapAck = msg
			return fwdOrder, false, false, true
		}
		d.hasPendingGap = true
		d.pendingGapSeq = seq
		d.pendingGapDelta = delta
		d.pendingGapHasAck = true
		d.pendingGapAck = msg
		return fwdOrder, false, false, false
	}
	back := uint64(uint8(d.lastSeq - seq))
	rel := uint64(1)
	if d.pos > back {
		rel = d.pos - back
	}
	cand := baseOrder + rel
	dup := prev.active && prev.ack == msg &&
		(cand <= prev.streamOrder || cand-prev.streamOrder < 256)
	return cand, !dup, dup, false
}

type commandInvokeSpec struct {
	action     string
	param1Bits uint32
	param2Bits uint32
	param3Bits uint32
}

const (
	ackPoolTargetDisplacedBit = 1 << 27
	ackPoolWrapHeldBit        = 1 << 28
	ackPoolWrapSettledBit     = 1 << 29
	ackPoolFormatV2Bit        = 1 << 30
	ackPoolResultMask         = (1 << 27) - 1
)

func poolMaskForCmd(mask uint64, accounted, wrapHeld, displaced bool) int {
	pm := int(mask) & ackPoolResultMask
	if accounted || wrapHeld || displaced {
		pm |= ackPoolFormatV2Bit
	}
	if wrapHeld {
		pm |= ackPoolWrapHeldBit
	}
	if displaced {
		pm |= ackPoolTargetDisplacedBit
	}
	return pm
}

type ackMetaState struct {
	ackAccounted int
	ackPoolMask  int
}

type inFlightCommand struct {
	entityID            string
	orderSeq            uint64
	sysid               uint8
	cmdID               uint16
	action              string
	invokeSpec          commandInvokeSpec
	issuedPeer          netip.AddrPort
	issuedTargetID      string
	issuedDomain        string
	issuedIncarnation   string
	issuedLinkTier      string
	issuedAuthority     string
	fromRecovery        bool
	priorUnackedAtIssue bool
	sitlOnly            bool
	targetDisplaced     bool
	dispatch            cmdDispatchState
	abortedNote         string
	abortedDetail       string
	phase               string
	detail              string
	ackAccounted        bool
	ackAmbiguous        bool
	wrapReceiptHeld     bool
	ackDeadline         time.Time
	issuedHbObsNs       int64
	issuedHbSeq         uint64
	issuedHbReceiptRev  uint64
	issuedLinkEpoch     uint64
	issuedPacketOrder   uint64
	unknownAtUnixNs     int64
	unknownHbSeq        uint64
	unknownHbReceiptRev uint64
	unknownLinkEpoch    uint64
	unknownPacketOrder  uint64
	earlyAcks           []earlyCommandAck
}

func (c *inFlightCommand) poolKey() cmdKey {
	return cmdKey{
		sysid: c.sysid,
		cmdID: c.cmdID,
		peer:  c.issuedPeer,
	}
}

type pendingCommandWrite struct {
	entityID     string
	ackOnly      bool
	phase        string
	note         string
	source       lifecycle.TransitionSource
	ackResult    int
	ackAccounted int
	ackPoolMask  int
	detail       string
	updatedMs    int64
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

	mu                      sync.Mutex
	authEpoch               map[uint8]uint64
	leases                  map[uint8]*teleopLease
	stoppedSessions         map[uint8]*stoppedSessionRing
	preemptState            map[uint8]*preemptRecoveryState
	minEngageObsRev         map[uint8]uint64
	minEngageObsMs          map[uint8]int64
	lastObsIncarnation      map[uint8]string
	inFlight                map[string]*inFlightCommand
	unackedCount            map[cmdKey]int
	wrapAckCredit           map[cmdKey]int
	wrapCreditIDs           map[cmdKey][]string
	unackedDetachedIDs      map[cmdKey][]string
	ambiguousSlotIDs        map[cmdKey][]string
	persistedAckMeta        map[string]ackMetaState
	seenAckFrames           map[uint8]*[256]serviceAckSlot
	directAckSeq            map[uint8]directSeqTracker
	lastSeenTrackerAckOrder map[uint8]uint64
	lastSeenTrackerAckSeq   map[uint8]uint64
	lastAckIncarnation      map[uint8]string
	lastAckLinkEpoch        map[uint8]uint64
	lastAckPeer             map[uint8]netip.AddrPort
	seenAckResultMask       map[cmdKey]uint64
	seenAckResultMaskBySeq  map[cmdKey]*[256]uint64
	preResetAckResultMask   map[cmdKey]uint64
	surplusAckTainted       map[cmdKey]bool
	dispatchedInBoot        map[cmdKey]bool
	pendingWrites           map[string]pendingCommandWrite
	lastGCSHB               map[uint8]time.Time

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
		org:                     org,
		platform:                platform,
		bootNonce:               newBootNonce(),
		targets:                 opts.Targets,
		sender:                  opts.Sender,
		store:                   opts.Store,
		logger:                  logger,
		nowFn:                   nowFn,
		bootTime:                nowFn(),
		teleopTTL:               teleopTTL,
		tickInterval:            tickInterval,
		gcsHeartbeatInterval:    gcsHB,
		commandAckTimeout:       ackTimeout,
		supervisoryAckTimeout:   supAckTimeout,
		authEpoch:               make(map[uint8]uint64),
		leases:                  make(map[uint8]*teleopLease),
		stoppedSessions:         make(map[uint8]*stoppedSessionRing),
		preemptState:            make(map[uint8]*preemptRecoveryState),
		minEngageObsRev:         make(map[uint8]uint64),
		minEngageObsMs:          make(map[uint8]int64),
		lastObsIncarnation:      make(map[uint8]string),
		inFlight:                make(map[string]*inFlightCommand),
		unackedCount:            make(map[cmdKey]int),
		wrapAckCredit:           make(map[cmdKey]int),
		wrapCreditIDs:           make(map[cmdKey][]string),
		unackedDetachedIDs:      make(map[cmdKey][]string),
		ambiguousSlotIDs:        make(map[cmdKey][]string),
		persistedAckMeta:        make(map[string]ackMetaState),
		seenAckFrames:           make(map[uint8]*[256]serviceAckSlot),
		directAckSeq:            make(map[uint8]directSeqTracker),
		lastSeenTrackerAckOrder: make(map[uint8]uint64),
		lastSeenTrackerAckSeq:   make(map[uint8]uint64),
		lastAckIncarnation:      make(map[uint8]string),
		lastAckLinkEpoch:        make(map[uint8]uint64),
		lastAckPeer:             make(map[uint8]netip.AddrPort),
		seenAckResultMask:       make(map[cmdKey]uint64),
		seenAckResultMaskBySeq:  make(map[cmdKey]*[256]uint64),
		preResetAckResultMask:   make(map[cmdKey]uint64),
		surplusAckTainted:       make(map[cmdKey]bool),
		dispatchedInBoot:        make(map[cmdKey]bool),
		pendingWrites:           make(map[string]pendingCommandWrite),
		lastGCSHB:               make(map[uint8]time.Time),
		leaseWake:               make(chan struct{}, 1),
		persistWake:             make(chan struct{}, 1),
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
		cePeer := parseEntityPeer(ce.Peer)
		key := cmdKey{sysid: uint8(ce.SysID), cmdID: uint16(ce.CmdID), peer: cePeer}
		if cur, exists := latestSentByKey[key]; !exists || isNewerRecoveredEntity(ce, cur) {
			latestSentByKey[key] = ce
		}
	}
	slices.SortFunc(preRestartSent, func(a, b CommandEntity) int {
		if a.ID == b.ID {
			return 0
		}
		if isNewerRecoveredEntity(a, b) {
			return 1
		}
		return -1
	})

	concreteResultMask := make(map[cmdKey]uint64)

	s.mu.Lock()
	for _, ce := range preRestartSent {
		recPeer := parseEntityPeer(ce.Peer)
		key := cmdKey{sysid: uint8(ce.SysID), cmdID: uint16(ce.CmdID), peer: recPeer}
		priorConcreteMask := concreteResultMask[key]
		var mask uint64
		if ce.AckResult >= 0 && ce.AckResult < 64 {
			bit := uint64(1) << uint(ce.AckResult)
			mask |= bit
			concreteResultMask[key] |= bit
		}
		if ce.AckPoolMask > 0 {
			mask |= uint64(ce.AckPoolMask & ackPoolResultMask)
		}
		if mask != 0 {
			s.seenAckResultMask[key] |= mask
			s.preResetAckResultMask[key] |= mask
		}
		isCurrentFormat := (ce.AckPoolMask & (ackPoolFormatV2Bit | ackPoolTargetDisplacedBit | ackPoolWrapHeldBit | ackPoolWrapSettledBit)) != 0
		wasDisplaced := (ce.AckPoolMask & ackPoolTargetDisplacedBit) != 0
		legacyWrapHeld := !isCurrentFormat &&
			ce.AckResult < 0 &&
			ce.AckAccounted > 0 &&
			mask != 0 &&
			(mask&^priorConcreteMask) == 0
		wrapHeld := ce.AckResult < 0 &&
			(ce.AckPoolMask&ackPoolWrapSettledBit) == 0 &&
			((ce.AckPoolMask&ackPoolWrapHeldBit) != 0 || legacyWrapHeld)
		accounted := ce.AckAccounted > 0 || ce.AckResult >= 0
		persistedMask := ce.AckPoolMask | (int(mask) & ackPoolResultMask)
		s.persistedAckMeta[ce.ID] = ackMetaState{
			ackAccounted: max(ce.AckAccounted, boolToInt(accounted)),
			ackPoolMask:  persistedMask,
		}
		if accounted && ce.AckResult < 0 {
			s.ambiguousSlotIDs[key] = appendBoundedIDs(s.ambiguousSlotIDs[key], ce.ID)
		}
		isUnacked := (ce.AckResult == UnacknowledgedResult || ce.PhaseState == PhasePending) && !accounted
		if isUnacked {
			s.unackedCount[key]++
			for _, curCmd := range s.inFlight {
				if curCmd.sysid == key.sysid && curCmd.cmdID == key.cmdID &&
					(!key.peer.IsValid() || curCmd.issuedPeer == key.peer) {
					curCmd.ackAmbiguous = true
				}
			}
		}
		if ce.PhaseState == PhasePending {
			s.enqueueWriteLocked(pendingCommandWrite{
				entityID:     ce.ID,
				phase:        PhaseCommitUnknown,
				note:         "recovered_indeterminate_after_restart",
				source:       lifecycle.TransitionSourceComponent,
				ackResult:    UnacknowledgedResult,
				ackAccounted: ce.AckAccounted,
				ackPoolMask:  persistedMask,
				detail:       CommitUnknownDetail,
				updatedMs:    updatedMs,
			})
		}
		dispatchedForPool := s.dispatchedInBoot[key]
		if !recPeer.IsValid() {
			for k, v := range s.dispatchedInBoot {
				if v && k.sysid == key.sysid && k.cmdID == key.cmdID {
					dispatchedForPool = true
					break
				}
			}
		}
		if !recPeer.IsValid() || wasDisplaced || dispatchedForPool || latestSentByKey[key].ID != ce.ID || commandTransitions.IsTerminal(ce.PhaseState) {
			if isUnacked {
				s.unackedDetachedIDs[key] = append(s.unackedDetachedIDs[key], ce.ID)
			}
			if wrapHeld {
				s.wrapAckCredit[key]++
				s.wrapCreditIDs[key] = append(s.wrapCreditIDs[key], ce.ID)
				for _, curCmd := range s.inFlight {
					if curCmd.sysid == key.sysid && curCmd.cmdID == key.cmdID &&
						(!key.peer.IsValid() || curCmd.issuedPeer == key.peer) {
						curCmd.ackAmbiguous = true
					}
				}
			}
			continue
		}
		if _, exists := s.inFlight[ce.ID]; exists {
			continue
		}
		unknownNs := bootNs
		if ceUpdatedNs := ce.UpdatedMs * int64(time.Millisecond); ceUpdatedNs > unknownNs {
			unknownNs = ceUpdatedNs
		}
		var unknownHbSeq, unknownHbReceiptRev, unknownLinkEpoch, unknownPacketOrder uint64
		recDomain := strings.TrimSpace(ce.Domain)
		recLinkTier := strings.TrimSpace(ce.LinkTier)
		recTarget, recOK := s.targets.ActuationTarget(uint8(ce.SysID))
		if recOK {
			if recTarget.HeartbeatObservedUnixNs > unknownNs {
				unknownNs = recTarget.HeartbeatObservedUnixNs
			}
			unknownHbSeq = recTarget.HeartbeatSeq
			unknownHbReceiptRev = latestTargetHbReceiptRev(recTarget)
			if normalizeControlPeer(recTarget.PeerAddr) == recPeer {
				unknownLinkEpoch = recTarget.LinkEpoch
				unknownPacketOrder = recTarget.PacketOrder
			}
		}
		seq := s.cmdCounter.Add(1)
		recCmd := &inFlightCommand{
			entityID:            ce.ID,
			orderSeq:            seq,
			sysid:               uint8(ce.SysID),
			cmdID:               uint16(ce.CmdID),
			action:              ce.Action,
			invokeSpec:          commandInvokeSpec{action: ce.Action},
			issuedPeer:          recPeer,
			issuedTargetID:      ce.Target,
			issuedDomain:        recDomain,
			issuedLinkTier:      recLinkTier,
			fromRecovery:        true,
			priorUnackedAtIssue: true,
			sitlOnly:            isSITLOnlyAction(ce.Action),
			dispatch:            dispatchSent,
			phase:               PhaseCommitUnknown,
			detail:              CommitUnknownDetail,
			ackAccounted:        accounted,
			ackAmbiguous:        true,
			wrapReceiptHeld:     wrapHeld,
			ackDeadline:         now,
			unknownAtUnixNs:     unknownNs,
			unknownHbSeq:        unknownHbSeq,
			unknownHbReceiptRev: unknownHbReceiptRev,
			unknownLinkEpoch:    unknownLinkEpoch,
			unknownPacketOrder:  unknownPacketOrder,
		}
		if recOK {
			_ = observeCommandTargetContinuity(recCmd, recTarget, true)
			if recCmd.targetDisplaced {
				_ = s.persistDisplacedCommandLocked(recCmd)
				if isUnacked {
					s.unackedDetachedIDs[key] = append(s.unackedDetachedIDs[key], ce.ID)
				}
				if wrapHeld {
					recCmd.wrapReceiptHeld = false
					s.wrapAckCredit[key]++
					s.wrapCreditIDs[key] = append(s.wrapCreditIDs[key], ce.ID)
					for _, curCmd := range s.inFlight {
						if curCmd.sysid == key.sysid && curCmd.cmdID == key.cmdID &&
							(!key.peer.IsValid() || curCmd.issuedPeer == key.peer) {
							curCmd.ackAmbiguous = true
						}
					}
				}
				continue
			}
		}
		s.inFlight[ce.ID] = recCmd
	}
	hasWrites := len(s.pendingWrites) > 0
	s.recovered.Store(true)
	s.mu.Unlock()

	if hasWrites {
		s.flushPendingWrites(ctx)
	}
}

func poolMaskInt(mask uint64) int {
	return (int(mask) & ackPoolResultMask) | ackPoolFormatV2Bit
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func appendBoundedIDs(ids []string, id string) []string {
	if id == "" || slices.Contains(ids, id) {
		return ids
	}
	if len(ids) >= maxStoppedSessionsPerVehicle {
		ids = ids[1:]
	}
	return append(ids, id)
}

func (s *Service) persistDisplacedCommandLocked(cmd *inFlightCommand) bool {
	if cmd == nil || !cmd.targetDisplaced || cmd.dispatch != dispatchSent {
		return false
	}
	key := cmd.poolKey()
	mask := poolMaskForCmd(s.seenAckResultMask[key], cmd.ackAccounted, cmd.wrapReceiptHeld, true)
	return s.enqueueConsumedAckLocked(cmd.entityID, boolToInt(cmd.ackAccounted), mask)
}

func (s *Service) enqueueConsumedAckLocked(entityID string, ackAccounted, ackPoolMask int) bool {
	if entityID == "" {
		return false
	}
	curMeta := s.persistedAckMeta[entityID]
	if curMeta.ackAccounted >= ackAccounted && (curMeta.ackPoolMask&ackPoolMask) == ackPoolMask {
		return false
	}
	s.enqueueWriteLocked(pendingCommandWrite{
		entityID:     entityID,
		ackOnly:      true,
		ackAccounted: ackAccounted,
		ackPoolMask:  ackPoolMask,
	})
	return true
}

func (s *Service) enqueueWriteLocked(w pendingCommandWrite) {
	if curMeta, ok := s.persistedAckMeta[w.entityID]; ok {
		if curMeta.ackAccounted > w.ackAccounted {
			w.ackAccounted = curMeta.ackAccounted
		}
		w.ackPoolMask |= curMeta.ackPoolMask
	}
	if prev, exists := s.pendingWrites[w.entityID]; exists {
		if w.ackOnly {
			if w.ackAccounted > prev.ackAccounted {
				prev.ackAccounted = w.ackAccounted
			}
			prev.ackPoolMask |= w.ackPoolMask
			s.pendingWrites[w.entityID] = prev
			return
		}
		if !prev.ackOnly && commandTransitions.IsTerminal(prev.phase) {
			return
		}
		if prev.ackAccounted > w.ackAccounted {
			w.ackAccounted = prev.ackAccounted
		}
		w.ackPoolMask |= prev.ackPoolMask
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

	recStore, hasReader := s.store.(RecoverableCommandStore)
	for pass := 0; pass < 2; pass++ {
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

		needsFollowupPass := false
		for _, w := range batch {
			if ctx.Err() != nil {
				return
			}
			if w.ackOnly {
				if !hasReader {
					s.mu.Lock()
					if cur, exists := s.pendingWrites[w.entityID]; exists && cur.ackOnly &&
						cur.ackAccounted == w.ackAccounted && cur.ackPoolMask == w.ackPoolMask {
						delete(s.pendingWrites, w.entityID)
					}
					s.mu.Unlock()
					continue
				}
				err := recStore.RecordConsumedAck(ctx, w.entityID, w.ackAccounted, w.ackPoolMask)
				verified := err == nil
				if !verified && ctx.Err() == nil {
					if got, getErr := recStore.Get(ctx, w.entityID); getErr == nil && got != nil &&
						got.AckAccounted >= w.ackAccounted && (got.AckPoolMask&w.ackPoolMask) == w.ackPoolMask {
						verified = true
					}
				}
				if verified {
					s.mu.Lock()
					meta := s.persistedAckMeta[w.entityID]
					if w.ackAccounted > meta.ackAccounted {
						meta.ackAccounted = w.ackAccounted
					}
					meta.ackPoolMask |= w.ackPoolMask
					s.persistedAckMeta[w.entityID] = meta
					if cur, exists := s.pendingWrites[w.entityID]; exists && cur.ackOnly &&
						cur.ackAccounted <= w.ackAccounted && (cur.ackPoolMask&^w.ackPoolMask) == 0 {
						delete(s.pendingWrites, w.entityID)
					}
					s.mu.Unlock()
					continue
				}
				s.logger.Warn("control: transient consumed-ack write failed; retaining for retry",
					slog.String("command_id", w.entityID),
					slog.Any("error", err),
				)
				continue
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
				if w.ackResult >= 0 {
					meta := s.persistedAckMeta[w.entityID]
					meta.ackAccounted = 1
					meta.ackPoolMask |= ackPoolFormatV2Bit
					if w.ackResult < 27 {
						meta.ackPoolMask |= 1 << uint(w.ackResult)
					}
					s.persistedAckMeta[w.entityID] = meta
				}
				if cur, exists := s.pendingWrites[w.entityID]; exists && !cur.ackOnly && cur.phase == w.phase {
					curMeta := s.persistedAckMeta[w.entityID]
					needPoolPatch := hasReader && cur.ackResult < 0 &&
						(cur.ackAccounted > curMeta.ackAccounted || (cur.ackPoolMask&^curMeta.ackPoolMask) != 0)
					if !needPoolPatch {
						delete(s.pendingWrites, w.entityID)
					} else {
						s.pendingWrites[w.entityID] = pendingCommandWrite{
							entityID:     w.entityID,
							ackOnly:      true,
							ackAccounted: cur.ackAccounted,
							ackPoolMask:  cur.ackPoolMask,
						}
						needsFollowupPass = true
					}
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
		if !needsFollowupPass {
			return
		}
	}
}

func (s *Service) isPreemptLatchedLocked(sysid uint8) bool {
	st := s.preemptState[sysid]
	return st != nil && st.latched
}

// IsPreempted reports whether sysid currently holds an unacknowledged preemption latch.
func (s *Service) IsPreempted(sysid uint8) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isPreemptLatchedLocked(sysid)
}

// HasActiveLease reports whether sysid currently has a live #84 velocity teleop lease.
func (s *Service) HasActiveLease(sysid uint8) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.leases[sysid]
	return ok && s.nowFn().Before(lease.expiresAt)
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
	if target.CurrentLinkHbUnconfirmed {
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "authority_not_teleop",
			Message: fmt.Sprintf("vehicle sysid %d velocity teleop suspended pending current-link vehicle heartbeat", sysid),
		}
	}
	if target.TeleopUncertain {
		code := target.TeleopUncertainReason
		status := http.StatusConflict
		switch code {
		case "supervisory_link_only", "supervisory_link_tier":
			code = "supervisory_link_tier"
			status = http.StatusForbidden
		case "vehicle_disarmed", "authority_preempted":
		default:
			code = "authority_not_teleop"
		}
		return &ControlError{
			Status:  status,
			Code:    code,
			Message: fmt.Sprintf("vehicle sysid %d velocity teleop suspended pending corroborated vehicle heartbeat (%s)", sysid, code),
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
	if sitlOnly {
		if target.LinkTier != uxsvocab.LinkTierSITLLocal {
			return &ControlError{
				Status:  http.StatusForbidden,
				Code:    "sitl_only_action",
				Message: fmt.Sprintf("action %q is restricted to sitl-local vehicles (current link_tier=%q)", actionSlug, target.LinkTier),
			}
		}
		if target.CurrentLinkHbUnconfirmed {
			return &ControlError{
				Status:  http.StatusForbidden,
				Code:    "sitl_only_action",
				Message: fmt.Sprintf("action %q is restricted to sitl-local vehicles (current-link heartbeat required)", actionSlug),
			}
		}
		if (target.UncertainLinkTier != "" && target.UncertainLinkTier != uxsvocab.LinkTierSITLLocal) ||
			(target.TeleopUncertain &&
				(target.TeleopUncertainReason == "supervisory_link_only" ||
					target.TeleopUncertainReason == "supervisory_link_tier")) {
			effTier := target.UncertainLinkTier
			if effTier == "" {
				effTier = uxsvocab.LinkTierMeshSupervisory
			}
			return &ControlError{
				Status:  http.StatusForbidden,
				Code:    "sitl_only_action",
				Message: fmt.Sprintf("action %q is restricted to sitl-local vehicles (uncertain link_tier=%q)", actionSlug, effTier),
			}
		}
	}
	if !sitlOnly {
		if target.Authority == uxsvocab.AuthorityDirectCRSFOverride ||
			(target.TeleopUncertain && target.UncertainAuthority == uxsvocab.AuthorityDirectCRSFOverride) {
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
		if target.TeleopUncertain && target.UncertainAuthority == uxsvocab.AuthorityMCUFailsafe &&
			(target.Armed || target.UncertainArmed) && actionSlug != ActionDisarm {
			return &ControlError{
				Status:  http.StatusConflict,
				Code:    "authority_preempted",
				Message: "MCU hardware failsafe (mcu-failsafe) holds actuator authority",
			}
		}
		if target.TeleopUncertain && target.UncertainAuthority == "" &&
			target.TeleopUncertainReason == "authority_preempted" && actionSlug != ActionDisarm {
			return &ControlError{
				Status:  http.StatusConflict,
				Code:    "authority_preempted",
				Message: "actuator authority is uncertain (authority_preempted)",
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

func (s *Service) syncObsIncarnationLocked(sysid uint8, inc string) {
	if inc == "" {
		return
	}
	prev := s.lastObsIncarnation[sysid]
	if prev != "" && prev != inc {
		delete(s.minEngageObsRev, sysid)
		delete(s.minEngageObsMs, sysid)
		if st := s.preemptState[sysid]; st != nil {
			st.preemptedAtMs = 0
			st.preemptEpoch = 0
			st.preemptObsRev = 0
			st.recoveryReady = false
			st.recoveryObservedMs = 0
			st.recoveryObsRev = 0
			st.recoveryEpoch = 0
			st.recoveryIncarnation = ""
		}
	}
	s.lastObsIncarnation[sysid] = inc
}

func (s *Service) ensurePreemptRecoveryObservedLocked(sysid uint8, target mavlink.ActuationTarget, ok bool) {
	st := s.preemptState[sysid]
	if st == nil || !ok || target.LinkLost || target.CurrentLinkHbUnconfirmed || target.TeleopUncertain ||
		target.LinkTier == uxsvocab.LinkTierMeshSupervisory ||
		!target.Armed || target.Authority != uxsvocab.AuthorityCompanionTeleop {
		return
	}
	_, hasResolver := s.targets.(ObservationResolver)
	useObsRev := hasResolver && target.ObservationRev > 0
	if !st.recoveryReady {
		recMs := target.HeartbeatObservedUnixNs / int64(time.Millisecond)
		if !useObsRev && recMs <= st.preemptedAtMs {
			recMs = st.preemptedAtMs + 1
		}
		st.recoveryReady = true
		st.recoveryObservedMs = recMs
		if useObsRev {
			st.recoveryObsRev = target.ObservationRev
			if target.ObservationRev > s.minEngageObsRev[sysid] {
				s.minEngageObsRev[sysid] = target.ObservationRev
			}
		} else if recMs > s.minEngageObsMs[sysid] {
			s.minEngageObsMs[sysid] = recMs
		}
		if target.AuthorityEpoch > 0 {
			st.recoveryEpoch = target.AuthorityEpoch
		}
		if target.AuthorityIncarnation != "" {
			st.recoveryIncarnation = target.AuthorityIncarnation
		}
		return
	}
	if target.AuthorityIncarnation != "" && st.recoveryIncarnation != target.AuthorityIncarnation {
		st.recoveryIncarnation = target.AuthorityIncarnation
		st.recoveryEpoch = target.AuthorityEpoch
		if useObsRev {
			st.recoveryObsRev = target.ObservationRev
		}
	}
}

func (s *Service) validateEngageObservationLocked(sysid uint8, session string, req TeleopRequest, target mavlink.ActuationTarget, ok bool, now time.Time) (uint64, bool, error) {
	if !req.Engage {
		return 0, false, nil
	}
	curInc := ""
	if ok {
		curInc = target.AuthorityIncarnation
	}
	s.syncObsIncarnationLocked(sysid, curInc)

	st := s.preemptState[sysid]
	rejectPreempt := func(msg string) error {
		s.recordStoppedSessionLocked(sysid, session)
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "preemption_latched",
			Message: msg,
		}
	}
	rejectNotEngaged := func(msg string) error {
		s.recordStoppedSessionLocked(sysid, session)
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "teleop_not_engaged",
			Message: msg,
		}
	}

	if req.AuthorityIncarnation != "" && curInc != "" && req.AuthorityIncarnation != curInc {
		if st != nil {
			return 0, false, rejectPreempt(fmt.Sprintf("vehicle sysid %d teleop authority incarnation mismatch (%q != %q)", sysid, req.AuthorityIncarnation, curInc))
		}
		return 0, false, rejectNotEngaged(fmt.Sprintf("vehicle sysid %d authority incarnation changed (%q -> %q) prior to engage", sysid, req.AuthorityIncarnation, curInc))
	}

	resolver, hasResolver := s.targets.(ObservationResolver)
	useObsRev := hasResolver && ok && target.ObservationRev > 0
	hasObsInput := req.ObservationRev > 0 || req.ObservedMs > 0
	var reqObsRev uint64
	if useObsRev && hasObsInput {
		resolvedRev, matched := resolver.ResolveObservation(sysid, req.ObservationRev, req.AuthorityEpoch, req.ObservedMs)
		if !matched {
			if st != nil {
				isPreOverride := st.latched ||
					(st.preemptEpoch > 0 && req.AuthorityEpoch > 0 && req.AuthorityEpoch <= st.preemptEpoch) ||
					(st.preemptedAtMs > 0 && req.ObservationRev == 0 && req.ObservedMs > 0 && req.ObservedMs <= st.preemptedAtMs)
				if !isPreOverride && req.ObservedMs > 0 && st.preemptObsRev > 0 {
					if anyRev, anyOK := resolver.ResolveObservation(sysid, 0, 0, req.ObservedMs); anyOK && anyRev <= st.preemptObsRev {
						isPreOverride = true
					}
				}
				if isPreOverride {
					return 0, true, rejectPreempt(fmt.Sprintf("vehicle sysid %d teleop observation does not match current post-recovery state", sysid))
				}
			}
			return 0, true, rejectNotEngaged(fmt.Sprintf("vehicle sysid %d teleop observation does not match current vehicle state", sysid))
		}
		reqObsRev = resolvedRev
	}

	if st != nil {
		s.ensurePreemptRecoveryObservedLocked(sysid, target, ok)
		rejectStaleForState := func(msg string) error {
			if st.latched ||
				(st.preemptEpoch > 0 && req.AuthorityEpoch > 0 && req.AuthorityEpoch <= st.preemptEpoch) ||
				(useObsRev && st.preemptObsRev > 0 && reqObsRev > 0 && reqObsRev <= st.preemptObsRev) ||
				(!useObsRev && st.preemptedAtMs > 0 && req.ObservedMs > 0 && req.ObservedMs <= st.preemptedAtMs) {
				return rejectPreempt(msg)
			}
			return rejectNotEngaged(msg)
		}
		if !st.recoveryReady {
			return 0, useObsRev, rejectPreempt(fmt.Sprintf("vehicle sysid %d teleop was preempted — re-engage requires current post-recovery authority observation", sysid))
		}
		hasValidEpoch := false
		if req.AuthorityEpoch > 0 {
			if st.recoveryEpoch == 0 || req.AuthorityEpoch < st.recoveryEpoch {
				return 0, useObsRev, rejectStaleForState(fmt.Sprintf("vehicle sysid %d teleop authority epoch %d precedes recovery epoch %d", sysid, req.AuthorityEpoch, st.recoveryEpoch))
			}
			if ok && target.AuthorityEpoch > 0 && req.AuthorityEpoch != target.AuthorityEpoch {
				return 0, useObsRev, rejectStaleForState(fmt.Sprintf("vehicle sysid %d teleop authority epoch %d does not match current vehicle epoch %d", sysid, req.AuthorityEpoch, target.AuthorityEpoch))
			}
			if curInc != "" && req.AuthorityIncarnation == "" {
				if !hasObsInput {
					return 0, useObsRev, rejectStaleForState(fmt.Sprintf("vehicle sysid %d teleop re-engage requires authority_incarnation or post-recovery observation", sysid))
				}
			} else {
				hasValidEpoch = true
			}
		}
		hasValidObs := false
		if useObsRev {
			if reqObsRev > 0 {
				if st.recoveryObsRev == 0 || reqObsRev < st.recoveryObsRev || (st.preemptObsRev > 0 && reqObsRev <= st.preemptObsRev) {
					return 0, true, rejectStaleForState(fmt.Sprintf("vehicle sysid %d teleop observation_rev %d precedes post-recovery floor %d", sysid, reqObsRev, st.recoveryObsRev))
				}
				hasValidObs = true
			}
		} else if req.ObservedMs > 0 {
			if st.recoveryObservedMs == 0 || req.ObservedMs < st.recoveryObservedMs || req.ObservedMs <= st.preemptedAtMs {
				return 0, false, rejectStaleForState(fmt.Sprintf("vehicle sysid %d teleop was preempted — observed_ms %d precedes post-recovery floor %d", sysid, req.ObservedMs, st.recoveryObservedMs))
			}
			hasValidObs = true
		}
		if !hasValidEpoch && !hasValidObs {
			return 0, useObsRev, rejectStaleForState(fmt.Sprintf("vehicle sysid %d teleop was preempted — re-engage requires current post-recovery authority observation", sysid))
		}
	} else if ok && req.AuthorityEpoch > 0 {
		if target.AuthorityEpoch > 0 && req.AuthorityEpoch != target.AuthorityEpoch {
			return 0, useObsRev, rejectNotEngaged(fmt.Sprintf("vehicle sysid %d authority epoch mismatch (%d vs %d) prior to engage", sysid, req.AuthorityEpoch, target.AuthorityEpoch))
		}
		if curInc != "" && req.AuthorityIncarnation == "" && !hasObsInput {
			return 0, useObsRev, rejectNotEngaged(fmt.Sprintf("vehicle sysid %d authority_epoch requires authority_incarnation or observed_ms", sysid))
		}
	}

	curLease := s.leases[sysid]
	if curLease != nil && !now.Before(curLease.expiresAt) {
		curLease = nil
	}
	if useObsRev {
		minRev := s.minEngageObsRev[sysid]
		if reqObsRev > 0 && minRev > 0 && reqObsRev < minRev {
			s.recordStoppedSessionLocked(sysid, session)
			return 0, true, &ControlError{
				Status:  http.StatusConflict,
				Code:    "teleop_session_superseded",
				Message: fmt.Sprintf("teleop engage observation_rev %d is older than active floor %d on vehicle sysid %d", reqObsRev, minRev, sysid),
			}
		}
		if curLease != nil && curLease.session != "" && curLease.session != session {
			if reqObsRev == 0 && curLease.engagedObsRev > 0 {
				s.recordStoppedSessionLocked(sysid, session)
				return 0, true, &ControlError{
					Status:  http.StatusConflict,
					Code:    "teleop_session_superseded",
					Message: fmt.Sprintf("teleop engage without observation cannot evict newer session %q on vehicle sysid %d", curLease.session, sysid),
				}
			}
			if req.AuthorityEpoch == 0 && curLease.engagedEpoch > 0 && reqObsRev < curLease.engagedObsRev {
				s.recordStoppedSessionLocked(sysid, session)
				return 0, true, &ControlError{
					Status:  http.StatusConflict,
					Code:    "teleop_session_superseded",
					Message: fmt.Sprintf("teleop engage cannot evict newer epoch-fenced session %q on vehicle sysid %d", curLease.session, sysid),
				}
			}
		}
		return reqObsRev, true, nil
	}

	minObs := s.minEngageObsMs[sysid]
	if req.ObservedMs > 0 && minObs > 0 && req.ObservedMs < minObs {
		s.recordStoppedSessionLocked(sysid, session)
		return 0, false, &ControlError{
			Status:  http.StatusConflict,
			Code:    "teleop_session_superseded",
			Message: fmt.Sprintf("teleop engage observed_ms %d is older than active floor %d on vehicle sysid %d", req.ObservedMs, minObs, sysid),
		}
	}
	if curLease != nil && curLease.session != "" && curLease.session != session {
		if req.ObservedMs <= 0 && curLease.engagedObsMs > 0 {
			s.recordStoppedSessionLocked(sysid, session)
			return 0, false, &ControlError{
				Status:  http.StatusConflict,
				Code:    "teleop_session_superseded",
				Message: fmt.Sprintf("teleop engage without observed_ms cannot evict newer session %q on vehicle sysid %d", curLease.session, sysid),
			}
		}
		if req.AuthorityEpoch == 0 && curLease.engagedEpoch > 0 && req.ObservedMs < curLease.engagedObsMs {
			s.recordStoppedSessionLocked(sysid, session)
			return 0, false, &ControlError{
				Status:  http.StatusConflict,
				Code:    "teleop_session_superseded",
				Message: fmt.Sprintf("teleop engage cannot evict newer epoch-fenced session %q on vehicle sysid %d", curLease.session, sysid),
			}
		}
	}
	return 0, false, nil
}

func (s *Service) checkTeleopSessionLocked(sysid uint8, session string, seq uint64, engage bool, now time.Time) error {
	if session != "" && s.stoppedSessions[sysid].contains(session) {
		if s.isPreemptLatchedLocked(sysid) {
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
	if s.isPreemptLatchedLocked(sysid) && !engage {
		return &ControlError{
			Status:  http.StatusConflict,
			Code:    "preemption_latched",
			Message: fmt.Sprintf("vehicle sysid %d teleop was preempted — explicit re-engage required", sysid),
		}
	}
	cur := s.leases[sysid]
	if cur != nil && !now.Before(cur.expiresAt) {
		cur = nil
	}
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

func (s *Service) revokeUncertainLease(sysid uint8, session string, engage bool, fallbackTarget mavlink.ActuationTarget) {
	s.egressMu[sysid].Lock()
	defer s.egressMu[sysid].Unlock()

	var zeroPeer netip.AddrPort
	var zeroFrame uint8
	var shouldZero bool
	s.mu.Lock()
	liveTarget, liveOK := s.targets.ActuationTarget(sysid)
	liveErr := validateTeleopTarget(sysid, liveTarget, liveOK)
	if prev, had := s.leases[sysid]; had {
		sameSession := (session != "" && prev.session == session) || (session == "" && prev.session == "")
		newerObs := (fallbackTarget.ObservationRev > 0 && prev.engagedObsRev > fallbackTarget.ObservationRev) ||
			(fallbackTarget.HeartbeatObservedUnixNs > 0 && prev.engagedObsMs > fallbackTarget.HeartbeatObservedUnixNs/int64(time.Millisecond))
		if liveErr != nil || (!engage && sameSession && !newerObs) {
			zeroPeer = prev.peer
			zeroFrame = prev.coordFrame
			s.recordStoppedSessionLocked(sysid, prev.session)
			delete(s.leases, sysid)
			s.authEpoch[sysid]++
			shouldZero = true
		}
	}
	if session != "" {
		s.recordStoppedSessionLocked(sysid, session)
	}
	s.mu.Unlock()

	if shouldZero {
		target := liveTarget
		if !liveOK {
			target = fallbackTarget
		}
		if !target.LinkLost && target.PeerAddr.IsValid() &&
			target.PeerAddr == zeroPeer &&
			target.LinkTier != uxsvocab.LinkTierMeshSupervisory &&
			target.Authority == uxsvocab.AuthorityCompanionTeleop &&
			target.TeleopUncertainReason != "authority_preempted" &&
			target.TeleopUncertainReason != "supervisory_link_only" {
			_ = s.sendZeroVelocity(sysid, zeroFrame, target.PeerAddr, s.nowFn())
		}
	}
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
				target.Authority == uxsvocab.AuthorityCompanionTeleop &&
				target.TeleopUncertainReason != "authority_preempted" &&
				target.TeleopUncertainReason != "supervisory_link_only" {
				_ = s.sendZeroVelocity(req.SysID, releaseFrame, target.PeerAddr, s.nowFn())
			}
		}
		return TeleopResult{SysID: req.SysID, LeaseActive: false}, nil
	}

	initialTarget, ok := s.targets.ActuationTarget(req.SysID)
	if err := validateTeleopTarget(req.SysID, initialTarget, ok); err != nil {
		if ok && initialTarget.TeleopUncertain {
			s.revokeUncertainLease(req.SysID, session, req.Engage, initialTarget)
		}
		return TeleopResult{}, err
	}

	now := s.nowFn()
	s.mu.Lock()
	if session != "" && s.stoppedSessions[req.SysID].contains(session) {
		err := s.checkTeleopSessionLocked(req.SysID, session, req.Seq, req.Engage, now)
		s.mu.Unlock()
		return TeleopResult{}, err
	}
	if s.isPreemptLatchedLocked(req.SysID) && !req.Engage {
		s.mu.Unlock()
		return TeleopResult{}, &ControlError{
			Status:  http.StatusConflict,
			Code:    "preemption_latched",
			Message: fmt.Sprintf("vehicle sysid %d teleop was preempted — explicit re-engage required", req.SysID),
		}
	}
	if _, _, err := s.validateEngageObservationLocked(req.SysID, session, req, initialTarget, ok, now); err != nil {
		s.mu.Unlock()
		return TeleopResult{}, err
	}
	startEpoch := s.authEpoch[req.SysID]
	lastHB, seenHB := s.lastGCSHB[req.SysID]
	hasLiveLease := false
	if cur := s.leases[req.SysID]; cur != nil && now.Before(cur.expiresAt) {
		hasLiveLease = true
	}
	shouldSendGCSHB := (req.Engage || hasLiveLease) &&
		(!seenHB || now.Before(lastHB) || !now.Before(lastHB.Add(s.gcsHeartbeatInterval)))
	s.mu.Unlock()

	if shouldSendGCSHB {
		_ = s.sendGCSHeartbeat(initialTarget.PeerAddr)
	}

	// Per-vehicle serialized egress: enforce overdue lease deadman expiry under the same
	// per-vehicle lock as the expiry worker, validate target, pinned peer, authority epoch,
	// post-recovery authority observation, and session/sequence ordering, transmit #84 while
	// holding egressMu[sysid], and commit the lease only once SendFrame succeeds.
	s.egressMu[req.SysID].Lock()

	s.mu.Lock()
	now = s.nowFn()
	inlineExpired := false
	if prev := s.leases[req.SysID]; prev != nil && !now.Before(prev.expiresAt) {
		expPeer := prev.peer
		expFrame := prev.coordFrame
		s.recordStoppedSessionLocked(req.SysID, prev.session)
		delete(s.leases, req.SysID)
		s.authEpoch[req.SysID]++
		inlineExpired = true
		if s.authEpoch[req.SysID] == startEpoch+1 && !s.isPreemptLatchedLocked(req.SysID) {
			startEpoch = s.authEpoch[req.SysID]
		}
		s.mu.Unlock()
		if target, tOK := s.targets.ActuationTarget(req.SysID); tOK &&
			!target.LinkLost && target.PeerAddr.IsValid() &&
			target.PeerAddr == expPeer &&
			target.LinkTier != uxsvocab.LinkTierMeshSupervisory &&
			target.Authority == uxsvocab.AuthorityCompanionTeleop &&
			target.TeleopUncertainReason != "authority_preempted" &&
			target.TeleopUncertainReason != "supervisory_link_only" {
			_ = s.sendZeroVelocity(req.SysID, expFrame, target.PeerAddr, now)
		}
		s.mu.Lock()
		now = s.nowFn()
	}
	_ = inlineExpired
	expiresAt := now.Add(s.teleopTTL)
	curTarget, curOK := s.targets.ActuationTarget(req.SysID)
	if err := validateTeleopTarget(req.SysID, curTarget, curOK); err != nil {
		var zeroPeer netip.AddrPort
		var zeroFrame uint8
		var shouldZero bool
		if curOK && curTarget.TeleopUncertain {
			if prev, had := s.leases[req.SysID]; had {
				zeroPeer = prev.peer
				zeroFrame = prev.coordFrame
				s.recordStoppedSessionLocked(req.SysID, prev.session)
				delete(s.leases, req.SysID)
				s.authEpoch[req.SysID]++
				shouldZero = true
			}
			if req.Engage && session != "" {
				s.recordStoppedSessionLocked(req.SysID, session)
			}
		}
		s.mu.Unlock()
		if shouldZero &&
			!curTarget.LinkLost && curTarget.PeerAddr.IsValid() &&
			curTarget.PeerAddr == zeroPeer &&
			curTarget.LinkTier != uxsvocab.LinkTierMeshSupervisory &&
			curTarget.Authority == uxsvocab.AuthorityCompanionTeleop &&
			curTarget.TeleopUncertainReason != "authority_preempted" &&
			curTarget.TeleopUncertainReason != "supervisory_link_only" {
			_ = s.sendZeroVelocity(req.SysID, zeroFrame, curTarget.PeerAddr, now)
		}
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
		latched := s.isPreemptLatchedLocked(req.SysID)
		if req.Engage {
			s.recordStoppedSessionLocked(req.SysID, session)
		}
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
	resolvedObsRev, useObsRev, obsErr := s.validateEngageObservationLocked(req.SysID, session, req, curTarget, curOK, now)
	if obsErr != nil {
		s.mu.Unlock()
		s.egressMu[req.SysID].Unlock()
		return TeleopResult{}, obsErr
	}
	if err := s.checkTeleopSessionLocked(req.SysID, session, req.Seq, req.Engage, now); err != nil {
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
	var leaseObsRev uint64
	var leaseObsMs int64
	var leaseEpoch uint64
	var leaseInc string
	if prev := s.leases[req.SysID]; prev != nil {
		if prev.session != "" && prev.session != effectiveSession {
			s.recordStoppedSessionLocked(req.SysID, prev.session)
		}
		if !req.Engage {
			leaseObsRev = prev.engagedObsRev
			leaseObsMs = prev.engagedObsMs
			leaseEpoch = prev.engagedEpoch
			leaseInc = prev.engagedIncarnation
		}
	}
	if req.Engage {
		if st := s.preemptState[req.SysID]; st != nil {
			st.latched = false
		}
		if useObsRev {
			if resolvedObsRev > s.minEngageObsRev[req.SysID] {
				s.minEngageObsRev[req.SysID] = resolvedObsRev
			}
			leaseObsRev = resolvedObsRev
		} else if req.ObservedMs > s.minEngageObsMs[req.SysID] {
			s.minEngageObsMs[req.SysID] = req.ObservedMs
		}
		leaseObsMs = req.ObservedMs
		leaseEpoch = req.AuthorityEpoch
		leaseInc = req.AuthorityIncarnation
	}
	s.leases[req.SysID] = &teleopLease{
		peer:               sendPeer,
		coordFrame:         coordFrame,
		session:            effectiveSession,
		lastSeq:            effectiveSeq,
		engagedObsRev:      leaseObsRev,
		engagedObsMs:       leaseObsMs,
		engagedEpoch:       leaseEpoch,
		engagedIncarnation: leaseInc,
		expiresAt:          expiresAt,
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
	shouldSendGCSHB := !seenHB || now.Before(lastHB) || !now.Before(lastHB.Add(s.gcsHeartbeatInterval))
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
	if normalizeControlPeer(curTarget.PeerAddr) != normalizeControlPeer(initialTarget.PeerAddr) {
		s.mu.Unlock()
		return CommandResult{}, &ControlError{
			Status:  http.StatusConflict,
			Code:    "vehicle_peer_changed",
			Message: fmt.Sprintf("vehicle sysid %d pinned peer changed prior to command registration", req.SysID),
		}
	}
	normPeer := normalizeControlPeer(curTarget.PeerAddr)

	for existingID, existing := range s.inFlight {
		if existing.sysid != req.SysID || existing.cmdID != spec.msg.Command {
			continue
		}
		if existing.targetDisplaced || (existing.issuedPeer.IsValid() && existing.issuedPeer != normPeer) {
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
		invokeSpec: commandInvokeSpec{
			action:     actionSlug,
			param1Bits: math.Float32bits(spec.msg.Param1),
			param2Bits: math.Float32bits(spec.msg.Param2),
			param3Bits: math.Float32bits(spec.msg.Param3),
		},
		issuedPeer:        normPeer,
		issuedTargetID:    curTarget.EntityID,
		issuedDomain:      curTarget.Domain,
		issuedIncarnation: curTarget.AuthorityIncarnation,
		issuedLinkTier:    curTarget.LinkTier,
		sitlOnly:          spec.sitlOnly,
		dispatch:          dispatchReserved,
		phase:             PhasePending,
		detail:            "Awaiting COMMAND_ACK",
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
		Peer:       normPeer.String(),
		Domain:     curTarget.Domain,
		LinkTier:   curTarget.LinkTier,
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
	peerChanged := targetErr == nil && (normalizeControlPeer(sendTarget.PeerAddr) != normPeer ||
		(entity.Target != "" && sendTarget.EntityID != entity.Target) ||
		(entity.Domain != "" && sendTarget.Domain != entity.Domain))
	epochRevoked := !spec.sitlOnly && s.authEpoch[req.SysID] != startEpoch &&
		(s.isPreemptLatchedLocked(req.SysID) || inflight.abortedNote != "")

	if inflight.abortedNote == "" && targetErr == nil && !peerChanged && !epochRevoked {
		for _, other := range s.inFlight {
			if other.entityID == entityID || other.sysid != req.SysID || other.cmdID != spec.msg.Command {
				continue
			}
			if other.targetDisplaced || (other.issuedPeer.IsValid() && other.issuedPeer != normPeer) {
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
	preSendHbReceiptRev := latestTargetHbReceiptRev(sendTarget)
	inflight.issuedPeer = normPeer
	inflight.issuedTargetID = sendTarget.EntityID
	inflight.issuedDomain = sendTarget.Domain
	inflight.issuedIncarnation = sendTarget.AuthorityIncarnation
	inflight.issuedLinkTier = sendTarget.LinkTier
	inflight.issuedAuthority = sendTarget.Authority
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
			s.processDedupedCommandAck(req.SysID, ea.peer, ea.seq, ea.hasSeq, ea.uncertain, ea.ack, ea.observedAt)
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
	hbBaselineReceiptRev := preSendHbReceiptRev
	linkEpochBaseline := sendTarget.LinkEpoch
	packetOrderBaseline := sendTarget.PacketOrder
	var hbBaselineSeq uint64
	if curT, ok := s.targets.ActuationTarget(req.SysID); ok {
		if curT.HeartbeatObservedUnixNs > hbBaselineNs {
			hbBaselineNs = curT.HeartbeatObservedUnixNs
		}
		hbBaselineSeq = curT.HeartbeatSeq
		if curRev := latestTargetHbReceiptRev(curT); curRev > hbBaselineReceiptRev {
			hbBaselineReceiptRev = curRev
		}
		linkEpochBaseline = curT.LinkEpoch
		packetOrderBaseline = curT.PacketOrder
	}
	key := cmdKey{sysid: req.SysID, cmdID: spec.msg.Command, peer: normPeer}
	legacyKey := cmdKey{sysid: req.SysID, cmdID: spec.msg.Command}
	supersededWrapHeld := false
	for otherID, other := range s.inFlight {
		if otherID == entityID || other.sysid != req.SysID || other.dispatch != dispatchSent {
			continue
		}
		_ = observeCommandTargetContinuity(other, sendTarget, true)
		if s.persistDisplacedCommandLocked(other) {
			hasSupersededWrite = true
		}
		if other.cmdID != spec.msg.Command {
			continue
		}
		otherKey := other.poolKey()
		if otherKey != key {
			continue
		}
		delete(s.inFlight, otherID)
		wasWrapHeld := other.wrapReceiptHeld
		if wasWrapHeld {
			other.wrapReceiptHeld = false
			s.wrapAckCredit[otherKey]++
			s.wrapCreditIDs[otherKey] = append(s.wrapCreditIDs[otherKey], other.entityID)
			supersededWrapHeld = true
		}
		if !other.ackAccounted {
			s.unackedDetachedIDs[otherKey] = append(s.unackedDetachedIDs[otherKey], other.entityID)
		}
		if other.phase == PhasePending {
			other.phase = PhaseCommitUnknown
			other.detail = CommitUnknownDetail
			s.enqueueWriteLocked(pendingCommandWrite{
				entityID:     other.entityID,
				phase:        PhaseCommitUnknown,
				note:         "superseded_by_emergency_disarm",
				source:       lifecycle.TransitionSourceComponent,
				ackResult:    UnacknowledgedResult,
				ackAccounted: boolToInt(other.ackAccounted),
				ackPoolMask:  poolMaskForCmd(s.seenAckResultMask[otherKey], other.ackAccounted, wasWrapHeld, other.targetDisplaced),
				detail:       CommitUnknownDetail,
				updatedMs:    dispatchNow.UnixMilli(),
			})
			hasSupersededWrite = true
		}
	}
	priorUnacked := s.unackedCount[key] > 0 || s.wrapAckCredit[key] > 0 || supersededWrapHeld ||
		s.unackedCount[legacyKey] > 0 || s.wrapAckCredit[legacyKey] > 0
	s.unackedCount[key]++
	s.dispatchedInBoot[key] = true
	tainted := s.surplusAckTainted[key] || s.surplusAckTainted[legacyKey]
	inflight.dispatch = dispatchSent
	inflight.priorUnackedAtIssue = !s.recovered.Load() || priorUnacked || tainted
	inflight.ackAmbiguous = !s.recovered.Load() || priorUnacked || tainted
	inflight.ackDeadline = dispatchNow.Add(ackWait)
	inflight.issuedHbObsNs = hbBaselineNs
	inflight.issuedHbSeq = hbBaselineSeq
	inflight.issuedHbReceiptRev = hbBaselineReceiptRev
	inflight.issuedLinkEpoch = linkEpochBaseline
	inflight.issuedPacketOrder = packetOrderBaseline
	inflight.unknownAtUnixNs = hbBaselineNs
	inflight.unknownHbSeq = hbBaselineSeq
	inflight.unknownHbReceiptRev = hbBaselineReceiptRev
	inflight.unknownLinkEpoch = linkEpochBaseline
	inflight.unknownPacketOrder = packetOrderBaseline
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
		s.processDedupedCommandAck(req.SysID, ea.peer, ea.seq, ea.hasSeq, ea.uncertain, ea.ack, ea.observedAt)
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

func (s *Service) snapshotPreResetAckHistoryLocked(sysid uint8) {
	for k, mask := range s.seenAckResultMask {
		if k.sysid == sysid && mask != 0 {
			s.preResetAckResultMask[k] |= mask
		}
	}
	for k := range s.seenAckResultMaskBySeq {
		if k.sysid == sysid {
			delete(s.seenAckResultMaskBySeq, k)
		}
	}
	delete(s.seenAckFrames, sysid)
	delete(s.directAckSeq, sysid)
	delete(s.lastSeenTrackerAckOrder, sysid)
	delete(s.lastSeenTrackerAckSeq, sysid)
}

func latestTargetHbReceiptRev(target mavlink.ActuationTarget) uint64 {
	if target.LatestHeartbeatReceiptRev > target.HeartbeatReceiptRev {
		return target.LatestHeartbeatReceiptRev
	}
	return target.HeartbeatReceiptRev
}

func hasFreshHeartbeatSince(
	target mavlink.ActuationTarget,
	baselineNs int64,
	baselineHbSeq, baselineHbReceiptRev, baselineLinkEpoch, baselinePacketOrder uint64,
) bool {
	if target.CurrentLinkHbUnconfirmed {
		return false
	}
	hasReceiptRev := target.HeartbeatReceiptRev > 0 || target.LatestHeartbeatReceiptRev > 0 || baselineHbReceiptRev > 0
	if hasReceiptRev && target.HeartbeatReceiptRev <= baselineHbReceiptRev {
		return false
	}
	if target.HeartbeatSeq > 0 {
		if target.HeartbeatSeq <= baselineHbSeq {
			return false
		}
		if !hasReceiptRev && baselineHbSeq == 0 && target.HeartbeatObservedUnixNs <= baselineNs {
			return false
		}
		if target.HeartbeatOrder > 0 || baselinePacketOrder > 0 || target.PacketOrder > 0 {
			if baselineLinkEpoch > 0 && target.LinkEpoch > baselineLinkEpoch {
				return target.HeartbeatOrder > 0
			}
			return target.HeartbeatOrder > baselinePacketOrder
		}
		if hasReceiptRev {
			return target.HeartbeatReceiptRev > baselineHbReceiptRev
		}
		return target.HeartbeatObservedUnixNs > baselineNs
	}
	if hasReceiptRev {
		return target.HeartbeatReceiptRev > baselineHbReceiptRev
	}
	return target.HeartbeatObservedUnixNs > baselineNs
}

// OnCommandAck correlates a sequence-less inbound #77 COMMAND_ACK against any dispatched
// discrete command for (sysid, ack.Command). When frame sequence identity is unavailable and
// an earlier command on the same key already observed the same MAV_RESULT code, correlation
// conservatively requires fresh #0 HEARTBEAT vehicle-state evidence.
func (s *Service) OnCommandAck(sysid uint8, ack mavcodec.CommandAck, observedAt time.Time) {
	s.handleCommandAck(sysid, 0, false, ack, observedAt)
}

// OnCommandAckFrame correlates an inbound #77 COMMAND_ACK carrying its MAVLink v2 frame sequence,
// suppressing repeated (sysid, seq, ack) deliveries within the current 256-packet window.
func (s *Service) OnCommandAckFrame(sysid, seq uint8, ack mavcodec.CommandAck, observedAt time.Time) {
	s.handleCommandAck(sysid, seq, true, ack, observedAt)
}

func (s *Service) handleCommandAck(sysid, seq uint8, hasSeq bool, ack mavcodec.CommandAck, observedAt time.Time) {
	if ack.Result == mavcodec.MAVResultInProgress {
		return
	}
	s.ensureRecovered(context.Background())
	if observedAt.IsZero() {
		observedAt = s.nowFn()
	}
	obsUTC := observedAt.UTC()

	target, targetOK := s.targets.ActuationTarget(sysid)
	var ackPeer netip.AddrPort
	if targetOK && target.PeerAddr.IsValid() {
		ackPeer = normalizeControlPeer(target.PeerAddr)
	}
	var ackOrderUncertain bool
	s.mu.Lock()
	if targetOK {
		prevInc := s.lastAckIncarnation[sysid]
		prevLinkEpoch := s.lastAckLinkEpoch[sysid]
		prevAckPeer := s.lastAckPeer[sysid]
		if (prevInc != "" && target.AuthorityIncarnation != "" && prevInc != target.AuthorityIncarnation) ||
			(prevLinkEpoch != 0 && target.LinkEpoch != 0 && prevLinkEpoch != target.LinkEpoch) ||
			(prevAckPeer.IsValid() && ackPeer.IsValid() && prevAckPeer != ackPeer) {
			s.snapshotPreResetAckHistoryLocked(sysid)
		}
		if target.AuthorityIncarnation != "" {
			s.lastAckIncarnation[sysid] = target.AuthorityIncarnation
		}
		if target.LinkEpoch != 0 {
			s.lastAckLinkEpoch[sysid] = target.LinkEpoch
		}
		if ackPeer.IsValid() {
			s.lastAckPeer[sysid] = ackPeer
		}
	}
	if hasSeq {
		slots := s.seenAckFrames[sysid]
		if slots == nil {
			slots = &[256]serviceAckSlot{}
			s.seenAckFrames[sysid] = slots
		}
		fromTracker := targetOK &&
			target.LastAckSeq > 0 &&
			target.LastAckSeq != s.lastSeenTrackerAckSeq[sysid]
		if fromTracker {
			candidateOrder := target.LastAckOrder
			ackOrderUncertain = target.LastAckUncertain
			prev := slots[seq]
			if !ackOrderUncertain && prev.active && prev.ack == ack &&
				(candidateOrder <= prev.streamOrder || candidateOrder-prev.streamOrder < 256) {
				s.mu.Unlock()
				return
			}
			s.lastSeenTrackerAckSeq[sysid] = target.LastAckSeq
			s.lastSeenTrackerAckOrder[sysid] = target.LastAckOrder
			if !ackOrderUncertain {
				slots[seq] = serviceAckSlot{
					active:      true,
					ack:         ack,
					streamOrder: candidateOrder,
				}
			}
		} else {
			var baseOrder uint64
			if targetOK && target.PacketOrder > 0 {
				baseOrder = target.PacketOrder
			}
			nextDirect := s.directAckSeq[sysid]
			candOrder, recordSlot, isDup, uncertain := nextDirect.evaluate(seq, ack, baseOrder, slots)
			if isDup {
				s.mu.Unlock()
				return
			}
			ackOrderUncertain = uncertain
			s.directAckSeq[sysid] = nextDirect
			if recordSlot {
				slots[seq] = serviceAckSlot{
					active:      true,
					ack:         ack,
					streamOrder: candOrder,
				}
			}
		}
	}
	s.mu.Unlock()

	s.processDedupedCommandAck(sysid, ackPeer, seq, hasSeq, ackOrderUncertain, ack, obsUTC)
}

func (s *Service) processDedupedCommandAck(sysid uint8, ackPeer netip.AddrPort, seq uint8, hasSeq, ackOrderUncertain bool, ack mavcodec.CommandAck, obsUTC time.Time) {
	updatedMs := obsUTC.UnixMilli()
	obsNs := obsUTC.UnixNano()

	s.mu.Lock()
	target, targetOK := s.targets.ActuationTarget(sysid)
	if !ackPeer.IsValid() && targetOK && target.PeerAddr.IsValid() {
		ackPeer = normalizeControlPeer(target.PeerAddr)
	}
	key := cmdKey{sysid: sysid, cmdID: ack.Command, peer: ackPeer}
	legacyKey := cmdKey{sysid: sysid, cmdID: ack.Command}
	hasLegacyPool := ackPeer.IsValid()

	for _, cmd := range s.inFlight {
		if cmd.dispatch == dispatchSending && cmd.sysid == sysid && cmd.cmdID == ack.Command &&
			(!ackPeer.IsValid() || cmd.issuedPeer == ackPeer) {
			cmd.earlyAcks = append(cmd.earlyAcks, earlyCommandAck{
				peer:       ackPeer,
				seq:        seq,
				hasSeq:     hasSeq,
				uncertain:  ackOrderUncertain,
				ack:        ack,
				observedAt: obsUTC,
			})
			s.mu.Unlock()
			return
		}
	}

	var matches []*inFlightCommand
	for _, cmd := range s.inFlight {
		if cmd.dispatch != dispatchSent || cmd.sysid != sysid || cmd.cmdID != ack.Command {
			continue
		}
		if cmd.phase != PhasePending && cmd.phase != PhaseCommitUnknown {
			continue
		}
		if targetOK && target.PeerAddr.IsValid() {
			_ = observeCommandTargetContinuity(cmd, target, true)
			_ = s.persistDisplacedCommandLocked(cmd)
		}
		if cmd.targetDisplaced {
			continue
		}
		if ackPeer.IsValid() && cmd.issuedPeer != ackPeer {
			continue
		}
		matches = append(matches, cmd)
	}
	if len(matches) > 1 {
		slices.SortFunc(matches, func(a, b *inFlightCommand) int {
			if a.orderSeq < b.orderSeq {
				return -1
			}
			if a.orderSeq > b.orderSeq {
				return 1
			}
			return 0
		})
	}

	combinedSeenMask := s.seenAckResultMask[key]
	combinedPreResetMask := s.preResetAckResultMask[key]
	legacyUnacked := s.unackedCount[legacyKey]
	legacyWrapCredit := s.wrapAckCredit[legacyKey]
	legacyTainted := s.surplusAckTainted[legacyKey]
	if hasLegacyPool {
		combinedSeenMask |= s.seenAckResultMask[legacyKey]
		combinedPreResetMask |= s.preResetAckResultMask[legacyKey]
	}

	seenSameResultBefore := ack.Result < 64 && (combinedSeenMask&(uint64(1)<<ack.Result)) != 0
	seenBeforeSeqReset := ack.Result < 64 && (combinedPreResetMask&(uint64(1)<<ack.Result)) != 0
	seqMasks := s.seenAckResultMaskBySeq[key]
	seenSameSeqResultBefore := hasSeq && seqMasks != nil && ack.Result < 64 && (seqMasks[seq]&(uint64(1)<<ack.Result)) != 0
	couldBeAccountedDuplicate := seenBeforeSeqReset ||
		ackOrderUncertain ||
		(hasSeq && seenSameSeqResultBefore) ||
		(!hasSeq && seenSameResultBefore)
	if ack.Result < 64 {
		bit := uint64(1) << ack.Result
		s.seenAckResultMask[key] |= bit
		combinedSeenMask |= bit
		if hasSeq {
			if seqMasks == nil {
				seqMasks = &[256]uint64{}
				s.seenAckResultMaskBySeq[key] = seqMasks
			}
			seqMasks[seq] |= bit
		}
	}
	poolMask := poolMaskInt(combinedSeenMask)

	slotPoolMask := func(pool cmdKey, accID string) int {
		if slices.Contains(s.wrapCreditIDs[pool], accID) {
			return poolMask | ackPoolWrapHeldBit
		}
		if inflightCmd, ok := s.inFlight[accID]; ok && inflightCmd.wrapReceiptHeld {
			return poolMask | ackPoolWrapHeldBit
		}
		return poolMask
	}
	settledWrapMask := poolMask | ackPoolWrapHeldBit | ackPoolWrapSettledBit

	var priorUnacked, peerWrapCredit int
	if ackPeer.IsValid() {
		priorUnacked = s.unackedCount[key]
		peerWrapCredit = s.wrapAckCredit[key]
	}
	hasLegacyUncertainty := legacyUnacked > 0 || legacyWrapCredit > 0 || legacyTainted
	if len(matches) == 0 {
		if s.recovered.Load() && ackPeer.IsValid() {
			if s.unackedCount[key] > 0 {
				s.unackedCount[key]--
				if couldBeAccountedDuplicate {
					s.wrapAckCredit[key]++
				}
				if ids := s.unackedDetachedIDs[key]; len(ids) > 0 {
					detachedID := ids[0]
					s.unackedDetachedIDs[key] = ids[1:]
					if couldBeAccountedDuplicate {
						s.wrapCreditIDs[key] = append(s.wrapCreditIDs[key], detachedID)
					}
					s.ambiguousSlotIDs[key] = appendBoundedIDs(s.ambiguousSlotIDs[key], detachedID)
				}
			} else if s.wrapAckCredit[key] > 0 {
				if !couldBeAccountedDuplicate {
					s.wrapAckCredit[key]--
					if ids := s.wrapCreditIDs[key]; len(ids) > 0 {
						creditID := ids[0]
						s.wrapCreditIDs[key] = ids[1:]
						_ = s.enqueueConsumedAckLocked(creditID, 1, settledWrapMask)
					}
				}
			} else {
				s.surplusAckTainted[key] = true
			}
			for _, accID := range s.ambiguousSlotIDs[key] {
				_ = s.enqueueConsumedAckLocked(accID, 1, slotPoolMask(key, accID))
			}
		} else if s.recovered.Load() {
			s.surplusAckTainted[legacyKey] = true
		}
		hasWrites := len(s.pendingWrites) > 0
		s.mu.Unlock()
		if hasWrites {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			s.flushPendingWrites(ctx)
			s.signalPersistWake()
		}
		return
	}

	anyTargetDisplaced := false
	for _, cmd := range matches {
		if targetOK && !canReconcileCommandOnTarget(cmd, target, true) {
			anyTargetDisplaced = true
		} else if cmd.targetDisplaced {
			anyTargetDisplaced = true
		}
	}
	wrapAmbiguous := false
	if len(matches) == 1 && hasSeq {
		hasPostDispatchMatchingHB := targetOK &&
			!target.LinkLost &&
			!ackOrderUncertain &&
			canReconcileCommandOnTarget(matches[0], target, targetOK) &&
			hasFreshHeartbeatSince(target, matches[0].issuedHbObsNs, matches[0].issuedHbSeq, matches[0].issuedHbReceiptRev, matches[0].issuedLinkEpoch, matches[0].issuedPacketOrder) &&
			matchesExpectedCommandState(matches[0], target)
		if ackOrderUncertain {
			wrapAmbiguous = true
		} else if seenSameSeqResultBefore {
			if ack.Result == mavcodec.MAVResultAccepted {
				if !hasPostDispatchMatchingHB {
					wrapAmbiguous = true
				}
			} else {
				wrapAmbiguous = true
			}
		}
	}

	holdingWrapReceipt := len(matches) == 1 && matches[0].wrapReceiptHeld
	nonWrapAmbiguous := !s.recovered.Load() ||
		!ackPeer.IsValid() ||
		anyTargetDisplaced ||
		priorUnacked > 1 ||
		peerWrapCredit > 0 ||
		hasLegacyUncertainty ||
		len(matches) > 1 ||
		matches[0].ackAmbiguous ||
		s.surplusAckTainted[key] ||
		seenBeforeSeqReset ||
		(!hasSeq && seenSameResultBefore)
	ambiguous := nonWrapAmbiguous || wrapAmbiguous

	if ambiguous {
		wroteAny := false
		markAllAmbiguous := nonWrapAmbiguous
		if anyTargetDisplaced || !ackPeer.IsValid() {
			markAllAmbiguous = true
		} else if s.recovered.Load() && peerWrapCredit > 0 && !holdingWrapReceipt {
			if !couldBeAccountedDuplicate {
				s.wrapAckCredit[key]--
				if ids := s.wrapCreditIDs[key]; len(ids) > 0 {
					creditID := ids[0]
					s.wrapCreditIDs[key] = ids[1:]
					if s.enqueueConsumedAckLocked(creditID, 1, settledWrapMask) {
						wroteAny = true
					}
				}
			}
			markAllAmbiguous = true
		} else if s.recovered.Load() && priorUnacked > 0 {
			if !couldBeAccountedDuplicate {
				s.unackedCount[key]--
				if ids := s.unackedDetachedIDs[key]; len(ids) > 0 {
					detachedID := ids[0]
					s.unackedDetachedIDs[key] = ids[1:]
					s.ambiguousSlotIDs[key] = appendBoundedIDs(s.ambiguousSlotIDs[key], detachedID)
				} else {
					for _, cmd := range matches {
						if !cmd.ackAccounted {
							cmd.ackAccounted = true
							s.ambiguousSlotIDs[key] = appendBoundedIDs(s.ambiguousSlotIDs[key], cmd.entityID)
							break
						}
					}
				}
			} else if ids := s.unackedDetachedIDs[key]; len(ids) > 0 {
				s.unackedCount[key]--
				s.wrapAckCredit[key]++
				detachedID := ids[0]
				s.unackedDetachedIDs[key] = ids[1:]
				s.wrapCreditIDs[key] = append(s.wrapCreditIDs[key], detachedID)
				s.ambiguousSlotIDs[key] = appendBoundedIDs(s.ambiguousSlotIDs[key], detachedID)
				markAllAmbiguous = true
			} else if !hasLegacyUncertainty && priorUnacked == 1 && len(matches) == 1 && !holdingWrapReceipt && !matches[0].ackAccounted {
				s.unackedCount[key] = 0
				matches[0].ackAccounted = true
				matches[0].wrapReceiptHeld = true
				s.ambiguousSlotIDs[key] = appendBoundedIDs(s.ambiguousSlotIDs[key], matches[0].entityID)
			} else {
				markAllAmbiguous = true
			}
		} else if s.recovered.Load() && priorUnacked == 0 {
			if holdingWrapReceipt {
				if !couldBeAccountedDuplicate {
					matches[0].wrapReceiptHeld = false
					if s.enqueueConsumedAckLocked(matches[0].entityID, 1, settledWrapMask) {
						wroteAny = true
					}
				}
				markAllAmbiguous = true
			} else {
				s.surplusAckTainted[key] = true
				markAllAmbiguous = true
			}
		}
		hbFloor := obsNs
		var hbFloorSeq, hbFloorReceiptRev, linkEpochFloor, packetOrderFloor uint64
		if targetOK {
			if target.HeartbeatObservedUnixNs > hbFloor {
				hbFloor = target.HeartbeatObservedUnixNs
			}
			hbFloorSeq = target.HeartbeatSeq
			hbFloorReceiptRev = latestTargetHbReceiptRev(target)
			linkEpochFloor = target.LinkEpoch
			packetOrderFloor = target.PacketOrder
		}
		for _, cmd := range matches {
			if markAllAmbiguous {
				cmd.ackAmbiguous = true
			}
			targetApplicable := observeCommandTargetContinuity(cmd, target, targetOK)
			_ = s.persistDisplacedCommandLocked(cmd)
			if hbFloor > cmd.unknownAtUnixNs {
				cmd.unknownAtUnixNs = hbFloor
			}
			if hbFloorSeq > cmd.unknownHbSeq {
				cmd.unknownHbSeq = hbFloorSeq
			}
			if hbFloorReceiptRev > cmd.unknownHbReceiptRev {
				cmd.unknownHbReceiptRev = hbFloorReceiptRev
			}
			if targetApplicable && linkEpochFloor > cmd.unknownLinkEpoch {
				cmd.unknownLinkEpoch = linkEpochFloor
				cmd.unknownPacketOrder = packetOrderFloor
			} else if linkEpochFloor == cmd.unknownLinkEpoch && packetOrderFloor > cmd.unknownPacketOrder {
				cmd.unknownPacketOrder = packetOrderFloor
			}
			if cmd.phase == PhasePending {
				cmd.phase = PhaseCommitUnknown
				cmd.detail = CommitUnknownDetail
				cmdMask := poolMaskForCmd(combinedSeenMask, cmd.ackAccounted, cmd.wrapReceiptHeld, cmd.targetDisplaced)
				s.enqueueWriteLocked(pendingCommandWrite{
					entityID:     cmd.entityID,
					phase:        PhaseCommitUnknown,
					note:         "command_ack_ambiguous",
					source:       lifecycle.TransitionSourceComponent,
					ackResult:    UnacknowledgedResult,
					ackAccounted: boolToInt(cmd.ackAccounted),
					ackPoolMask:  cmdMask,
					detail:       CommitUnknownDetail,
					updatedMs:    updatedMs,
				})
				wroteAny = true
			}
		}
		if s.recovered.Load() && ackPeer.IsValid() {
			for _, accID := range s.ambiguousSlotIDs[key] {
				if s.enqueueConsumedAckLocked(accID, 1, slotPoolMask(key, accID)) {
					wroteAny = true
				}
			}
		}
		hasWrites := wroteAny || len(s.pendingWrites) > 0
		s.mu.Unlock()
		if hasWrites {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			s.flushPendingWrites(ctx)
			s.signalPersistWake()
		}
		return
	}

	matched := matches[0]
	s.unackedCount[key] = 0
	wasWrapHeld := matched.wrapReceiptHeld
	matched.wrapReceiptHeld = false
	matched.ackAccounted = true
	finalPoolMask := poolMask
	if wasWrapHeld {
		finalPoolMask = settledWrapMask
		s.ambiguousSlotIDs[key] = slices.DeleteFunc(s.ambiguousSlotIDs[key], func(id string) bool {
			return id == matched.entityID
		})
	}
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
		entityID:     matched.entityID,
		phase:        nextPhase,
		note:         note,
		source:       lifecycle.TransitionSourceComponent,
		ackResult:    int(ack.Result),
		ackAccounted: 1,
		ackPoolMask:  finalPoolMask,
		detail:       detail,
		updatedMs:    updatedMs,
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
	targetForObs, targetForObsOK := s.targets.ActuationTarget(ev.SysID)
	obsInc := ev.AuthorityIncarnation
	if obsInc == "" && targetForObsOK {
		obsInc = targetForObs.AuthorityIncarnation
	}
	s.syncObsIncarnationLocked(ev.SysID, obsInc)
	_, hasResolver := s.targets.(ObservationResolver)
	evObsRev := ev.ObservationRev
	if evObsRev == 0 && targetForObsOK && targetForObs.ObservationRev > 0 {
		evObsRev = targetForObs.ObservationRev
	}
	useObsRev := hasResolver && evObsRev > 0
	if targetForObsOK {
		for _, cmd := range s.inFlight {
			if cmd.sysid == ev.SysID && cmd.dispatch == dispatchSent {
				_ = observeCommandTargetContinuity(cmd, targetForObs, true)
				if s.persistDisplacedCommandLocked(cmd) {
					wroteAny = true
				}
			}
		}
	}

	if ev.Kind == mavlink.EventKindLinkLost || ev.Kind == mavlink.EventKindLinkRestored {
		normObsPeer := normalizeControlPeer(targetForObs.PeerAddr)
		prevAckPeer := s.lastAckPeer[ev.SysID]
		alreadySnapshotted := ev.Kind == mavlink.EventKindLinkRestored && targetForObsOK &&
			targetForObs.LinkEpoch != 0 && s.lastAckLinkEpoch[ev.SysID] == targetForObs.LinkEpoch &&
			(!prevAckPeer.IsValid() || !normObsPeer.IsValid() || prevAckPeer == normObsPeer)
		if !alreadySnapshotted {
			s.snapshotPreResetAckHistoryLocked(ev.SysID)
		}
		if targetForObsOK {
			if targetForObs.AuthorityIncarnation != "" {
				s.lastAckIncarnation[ev.SysID] = targetForObs.AuthorityIncarnation
			}
			if targetForObs.LinkEpoch != 0 {
				s.lastAckLinkEpoch[ev.SysID] = targetForObs.LinkEpoch
			}
			if normObsPeer.IsValid() {
				s.lastAckPeer[ev.SysID] = normObsPeer
			}
		}
	}
	if isPreempt {
		s.authEpoch[ev.SysID]++
		if prev, had := s.leases[ev.SysID]; had {
			s.recordStoppedSessionLocked(ev.SysID, prev.session)
			delete(s.leases, ev.SysID)
		}
		st := s.preemptState[ev.SysID]
		if st == nil {
			st = &preemptRecoveryState{}
			s.preemptState[ev.SysID] = st
		}
		preemptMs := updatedMs
		if !useObsRev && st.preemptedAtMs > preemptMs {
			preemptMs = st.preemptedAtMs
		}
		st.latched = true
		st.preemptedAtMs = preemptMs
		if ev.AuthorityEpoch > st.preemptEpoch {
			st.preemptEpoch = ev.AuthorityEpoch
		}
		if targetForObsOK && targetForObs.AuthorityEpoch > st.preemptEpoch {
			st.preemptEpoch = targetForObs.AuthorityEpoch
		}
		if useObsRev {
			if evObsRev > st.preemptObsRev {
				st.preemptObsRev = evObsRev
			}
			if evObsRev > s.minEngageObsRev[ev.SysID] {
				s.minEngageObsRev[ev.SysID] = evObsRev
			}
		} else if preemptMs > s.minEngageObsMs[ev.SysID] {
			s.minEngageObsMs[ev.SysID] = preemptMs
		}
		st.recoveryReady = false
		st.recoveryObservedMs = 0
		st.recoveryObsRev = 0
		st.recoveryEpoch = 0
		st.recoveryIncarnation = ""

		preemptNote := "authority_preempted"
		preemptDetail := fmt.Sprintf("Preempted by %s", ev.Authority)
		if ev.Kind == mavlink.EventKindLinkLost {
			preemptNote = "link_lost"
			preemptDetail = "Preempted by vehicle link loss"
		}
		for id, cmd := range s.inFlight {
			if cmd.sysid == ev.SysID && !cmd.sitlOnly && !cmd.targetDisplaced {
				delete(s.inFlight, id)
				if cmd.dispatch == dispatchReserved {
					cmd.abortedNote = preemptNote
					cmd.abortedDetail = preemptDetail
					continue
				}
				key := cmd.poolKey()
				wasWrapHeld := cmd.wrapReceiptHeld
				if wasWrapHeld {
					cmd.wrapReceiptHeld = false
					s.wrapAckCredit[key]++
					s.wrapCreditIDs[key] = append(s.wrapCreditIDs[key], cmd.entityID)
				}
				if cmd.dispatch == dispatchSent && !cmd.ackAccounted {
					s.unackedDetachedIDs[key] = append(s.unackedDetachedIDs[key], cmd.entityID)
				} else if cmd.dispatch == dispatchSent && cmd.ackAccounted {
					s.ambiguousSlotIDs[key] = appendBoundedIDs(s.ambiguousSlotIDs[key], cmd.entityID)
				}
				cmd.phase = PhasePreempted
				cmd.detail = preemptDetail
				s.enqueueWriteLocked(pendingCommandWrite{
					entityID:     cmd.entityID,
					phase:        PhasePreempted,
					note:         preemptNote,
					source:       lifecycle.TransitionSourceComponent,
					ackResult:    UnacknowledgedResult,
					ackAccounted: boolToInt(cmd.ackAccounted),
					ackPoolMask:  poolMaskForCmd(s.seenAckResultMask[key], cmd.ackAccounted, wasWrapHeld, cmd.targetDisplaced),
					detail:       preemptDetail,
					updatedMs:    updatedMs,
				})
				wroteAny = true
			}
		}
	} else {
		st := s.preemptState[ev.SysID]
		if ev.LinkTier == uxsvocab.LinkTierMeshSupervisory ||
			!ev.Armed ||
			ev.Authority != uxsvocab.AuthorityCompanionTeleop {
			if prev, had := s.leases[ev.SysID]; had {
				s.authEpoch[ev.SysID]++
				s.recordStoppedSessionLocked(ev.SysID, prev.session)
				delete(s.leases, ev.SysID)
			}
			if useObsRev && evObsRev > s.minEngageObsRev[ev.SysID] {
				s.minEngageObsRev[ev.SysID] = evObsRev
			}
			if st != nil {
				st.recoveryReady = false
				st.recoveryObservedMs = 0
				st.recoveryObsRev = 0
				st.recoveryEpoch = 0
				st.recoveryIncarnation = ""
			}
		} else if st != nil && isHeartbeatEvidence &&
			(!targetForObsOK || (!targetForObs.CurrentLinkHbUnconfirmed && !targetForObs.TeleopUncertain)) {
			if !st.recoveryReady {
				recMs := updatedMs
				recEpoch := ev.AuthorityEpoch
				recInc := ev.AuthorityIncarnation
				if targetForObsOK {
					if hbMs := targetForObs.HeartbeatObservedUnixNs / int64(time.Millisecond); !useObsRev && hbMs > recMs {
						recMs = hbMs
					} else if useObsRev && targetForObs.HeartbeatObservedUnixNs > 0 {
						recMs = targetForObs.HeartbeatObservedUnixNs / int64(time.Millisecond)
					}
					if recEpoch == 0 && targetForObs.AuthorityEpoch > 0 {
						recEpoch = targetForObs.AuthorityEpoch
					}
					if recInc == "" && targetForObs.AuthorityIncarnation != "" {
						recInc = targetForObs.AuthorityIncarnation
					}
				}
				if !useObsRev && recMs <= st.preemptedAtMs {
					recMs = st.preemptedAtMs + 1
				}
				st.recoveryReady = true
				st.recoveryObservedMs = recMs
				st.recoveryEpoch = recEpoch
				st.recoveryIncarnation = recInc
				if useObsRev {
					st.recoveryObsRev = evObsRev
					if evObsRev > s.minEngageObsRev[ev.SysID] {
						s.minEngageObsRev[ev.SysID] = evObsRev
					}
				} else if recMs > s.minEngageObsMs[ev.SysID] {
					s.minEngageObsMs[ev.SysID] = recMs
				}
			} else if ev.AuthorityIncarnation != "" && st.recoveryIncarnation != ev.AuthorityIncarnation {
				st.recoveryIncarnation = ev.AuthorityIncarnation
				st.recoveryEpoch = ev.AuthorityEpoch
				if useObsRev {
					st.recoveryObsRev = evObsRev
				}
			}
		}
	}
	if isHeartbeatEvidence && targetForObsOK && !targetForObs.LinkLost {
		for id, cmd := range s.inFlight {
			if cmd.dispatch == dispatchSent &&
				cmd.sysid == ev.SysID &&
				cmd.phase == PhaseCommitUnknown {
				targetApplicable := observeCommandTargetContinuity(cmd, targetForObs, targetForObsOK)
				if s.persistDisplacedCommandLocked(cmd) {
					wroteAny = true
				}
				if targetApplicable &&
					hasFreshHeartbeatSince(targetForObs, cmd.unknownAtUnixNs, cmd.unknownHbSeq, cmd.unknownHbReceiptRev, cmd.unknownLinkEpoch, cmd.unknownPacketOrder) &&
					matchesExpectedCommandState(cmd, targetForObs) {
					cmd.phase = PhaseConfirmed
					cmd.detail = "Confirmed via vehicle HEARTBEAT state reconciliation"
					delete(s.inFlight, id)
					key := cmd.poolKey()
					wasWrapHeld := cmd.wrapReceiptHeld
					if wasWrapHeld {
						cmd.wrapReceiptHeld = false
						s.wrapAckCredit[key]++
						s.wrapCreditIDs[key] = append(s.wrapCreditIDs[key], cmd.entityID)
					}
					if !cmd.ackAccounted {
						s.unackedDetachedIDs[key] = append(s.unackedDetachedIDs[key], cmd.entityID)
					} else {
						s.ambiguousSlotIDs[key] = appendBoundedIDs(s.ambiguousSlotIDs[key], cmd.entityID)
					}
					s.enqueueWriteLocked(pendingCommandWrite{
						entityID:     cmd.entityID,
						phase:        PhaseConfirmed,
						note:         "reconciled_by_heartbeat",
						source:       lifecycle.TransitionSourceComponent,
						ackResult:    UnacknowledgedResult,
						ackAccounted: boolToInt(cmd.ackAccounted),
						ackPoolMask:  poolMaskForCmd(s.seenAckResultMask[key], cmd.ackAccounted, wasWrapHeld, cmd.targetDisplaced),
						detail:       cmd.detail,
						updatedMs:    updatedMs,
					})
					wroteAny = true
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
			target.Authority == uxsvocab.AuthorityCompanionTeleop &&
			target.TeleopUncertainReason != "authority_preempted" &&
			target.TeleopUncertainReason != "supervisory_link_only" {
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
		if !seen || now.Before(lastHB) || !now.Before(lastHB.Add(s.gcsHeartbeatInterval)) {
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
		key := cmd.poolKey()
		if cmd.phase == PhasePending && !now.Before(cmd.ackDeadline) {
			cmd.phase = PhaseCommitUnknown
			cmd.detail = CommitUnknownDetail
			hbFloor := nowNs
			if target, ok := s.targets.ActuationTarget(cmd.sysid); ok {
				targetApplicable := observeCommandTargetContinuity(cmd, target, true)
				if target.HeartbeatObservedUnixNs > hbFloor {
					hbFloor = target.HeartbeatObservedUnixNs
				}
				if target.HeartbeatSeq > cmd.unknownHbSeq {
					cmd.unknownHbSeq = target.HeartbeatSeq
				}
				if curRev := latestTargetHbReceiptRev(target); curRev > cmd.unknownHbReceiptRev {
					cmd.unknownHbReceiptRev = curRev
				}
				if targetApplicable && target.LinkEpoch > cmd.unknownLinkEpoch {
					cmd.unknownLinkEpoch = target.LinkEpoch
					cmd.unknownPacketOrder = target.PacketOrder
				} else if target.LinkEpoch == cmd.unknownLinkEpoch && target.PacketOrder > cmd.unknownPacketOrder {
					cmd.unknownPacketOrder = target.PacketOrder
				}
			}
			if hbFloor > cmd.unknownAtUnixNs {
				cmd.unknownAtUnixNs = hbFloor
			}
			s.enqueueWriteLocked(pendingCommandWrite{
				entityID:     cmd.entityID,
				phase:        PhaseCommitUnknown,
				note:         "command_ack_timeout",
				source:       lifecycle.TransitionSourceComponent,
				ackResult:    UnacknowledgedResult,
				ackAccounted: boolToInt(cmd.ackAccounted),
				ackPoolMask:  poolMaskForCmd(s.seenAckResultMask[key], cmd.ackAccounted, cmd.wrapReceiptHeld, cmd.targetDisplaced),
				detail:       CommitUnknownDetail,
				updatedMs:    updatedMs,
			})
			continue
		}
		if cmd.phase == PhaseCommitUnknown {
			if target, ok := s.targets.ActuationTarget(cmd.sysid); ok {
				targetApplicable := observeCommandTargetContinuity(cmd, target, true)
				_ = s.persistDisplacedCommandLocked(cmd)
				if !target.LinkLost &&
					targetApplicable &&
					hasFreshHeartbeatSince(target, cmd.unknownAtUnixNs, cmd.unknownHbSeq, cmd.unknownHbReceiptRev, cmd.unknownLinkEpoch, cmd.unknownPacketOrder) &&
					matchesExpectedCommandState(cmd, target) {
					cmd.phase = PhaseConfirmed
					cmd.detail = "Confirmed via vehicle HEARTBEAT state reconciliation"
					delete(s.inFlight, id)
					wasWrapHeld := cmd.wrapReceiptHeld
					if wasWrapHeld {
						cmd.wrapReceiptHeld = false
						s.wrapAckCredit[key]++
						s.wrapCreditIDs[key] = append(s.wrapCreditIDs[key], cmd.entityID)
					}
					if !cmd.ackAccounted {
						s.unackedDetachedIDs[key] = append(s.unackedDetachedIDs[key], cmd.entityID)
					} else {
						s.ambiguousSlotIDs[key] = appendBoundedIDs(s.ambiguousSlotIDs[key], cmd.entityID)
					}
					s.enqueueWriteLocked(pendingCommandWrite{
						entityID:     cmd.entityID,
						phase:        PhaseConfirmed,
						note:         "reconciled_by_heartbeat",
						source:       lifecycle.TransitionSourceComponent,
						ackResult:    UnacknowledgedResult,
						ackAccounted: boolToInt(cmd.ackAccounted),
						ackPoolMask:  poolMaskForCmd(s.seenAckResultMask[key], cmd.ackAccounted, wasWrapHeld, cmd.targetDisplaced),
						detail:       cmd.detail,
						updatedMs:    updatedMs,
					})
					continue
				}
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

func normalizeControlPeer(peer netip.AddrPort) netip.AddrPort {
	if !peer.IsValid() {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port())
}

func parseEntityPeer(raw string) netip.AddrPort {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return netip.AddrPort{}
	}
	parsed, err := netip.ParseAddrPort(trimmed)
	if err != nil || !parsed.IsValid() {
		return netip.AddrPort{}
	}
	return normalizeControlPeer(parsed)
}

func isApplicableCommandLinkTier(cmd *inFlightCommand, target mavlink.ActuationTarget) bool {
	if cmd == nil || !uxsvocab.IsValidLinkTier(target.LinkTier) {
		return false
	}
	if cmd.sitlOnly {
		if target.LinkTier != uxsvocab.LinkTierSITLLocal || target.CurrentLinkHbUnconfirmed {
			return false
		}
		if (target.UncertainLinkTier != "" && target.UncertainLinkTier != uxsvocab.LinkTierSITLLocal) ||
			(target.TeleopUncertain &&
				(target.TeleopUncertainReason == "supervisory_link_only" ||
					target.TeleopUncertainReason == "supervisory_link_tier")) {
			return false
		}
	}
	return true
}

func observeCommandTargetContinuity(cmd *inFlightCommand, target mavlink.ActuationTarget, ok bool) bool {
	if cmd == nil || !ok || !target.PeerAddr.IsValid() {
		return false
	}
	curPeer := normalizeControlPeer(target.PeerAddr)
	if !cmd.issuedPeer.IsValid() {
		cmd.targetDisplaced = true
		return false
	}
	if curPeer != cmd.issuedPeer {
		cmd.targetDisplaced = true
	}
	if cmd.issuedTargetID != "" && target.EntityID != "" && target.EntityID != cmd.issuedTargetID {
		cmd.targetDisplaced = true
	}
	if cmd.issuedDomain != "" && target.Domain != "" && target.Domain != cmd.issuedDomain {
		cmd.targetDisplaced = true
	} else if !cmd.fromRecovery && cmd.issuedDomain == "" && target.Domain != "" {
		cmd.issuedDomain = target.Domain
	}
	if cmd.issuedIncarnation != "" && target.AuthorityIncarnation != "" && target.AuthorityIncarnation != cmd.issuedIncarnation {
		cmd.targetDisplaced = true
	}
	if cmd.sitlOnly && cmd.fromRecovery &&
		cmd.issuedLinkTier != "" && cmd.issuedLinkTier != uxsvocab.LinkTierSITLLocal {
		cmd.targetDisplaced = true
	}
	if cmd.sitlOnly && !target.LinkLost && !target.CurrentLinkHbUnconfirmed &&
		target.LinkTier != "" && target.LinkTier != uxsvocab.LinkTierSITLLocal {
		cmd.targetDisplaced = true
	}
	if !cmd.targetDisplaced && cmd.issuedIncarnation == "" && target.AuthorityIncarnation != "" {
		cmd.issuedIncarnation = target.AuthorityIncarnation
	}
	return !cmd.targetDisplaced
}

func canReconcileCommandOnTarget(cmd *inFlightCommand, target mavlink.ActuationTarget, ok bool) bool {
	if !observeCommandTargetContinuity(cmd, target, ok) {
		return false
	}
	return isApplicableCommandLinkTier(cmd, target)
}

func matchesExpectedCommandState(cmd *inFlightCommand, target mavlink.ActuationTarget) bool {
	if cmd == nil || !canReconcileCommandOnTarget(cmd, target, true) {
		return false
	}
	if cmd.action == ActionSITLCRSFOverride {
		if cmd.invokeSpec.param1Bits != math.Float32bits(2) {
			return false
		}
		param2 := float64(math.Float32frombits(cmd.invokeSpec.param2Bits))
		if math.IsNaN(param2) {
			return false
		}
		if param2 >= 0.5 {
			return target.Authority == uxsvocab.AuthorityDirectCRSFOverride
		}
		if param2 <= -0.5 {
			return target.Authority == uxsvocab.AuthorityCompanionTeleop ||
				target.Authority == uxsvocab.AuthorityCompanionAutonomous
		}
		if cmd.issuedAuthority == "" || cmd.priorUnackedAtIssue {
			return false
		}
		if cmd.issuedAuthority == uxsvocab.AuthorityDirectCRSFOverride {
			return target.Authority == uxsvocab.AuthorityCompanionTeleop ||
				target.Authority == uxsvocab.AuthorityCompanionAutonomous
		}
		return target.Authority == uxsvocab.AuthorityDirectCRSFOverride
	}
	return matchesExpectedVehicleState(cmd.action, target.Armed, target.CompanionMode, target.Authority)
}

func matchesExpectedVehicleState(action string, armed bool, companionMode, authority string) bool {
	switch action {
	case ActionArm:
		return armed
	case ActionDisarm:
		return !armed
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
