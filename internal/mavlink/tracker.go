package mavlink

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/boxbotix/bbops/internal/mavcodec"
	"github.com/boxbotix/bbops/internal/uxsvocab"
	"github.com/c360studio/semengine/message"
)

const (
	minVehicleSysID uint8 = 1
	maxVehicleSysID uint8 = 254

	minLatDegE7 int32 = -900_000_000
	maxLatDegE7 int32 = 900_000_000
	minLonDegE7 int32 = -1_800_000_000
	maxLonDegE7 int32 = 1_800_000_000

	maxValidHdgCdeg  uint16 = 35999
	unknownHdgUint16 uint16 = 65535

	minQuatNormSq = 1e-6

	// TransitionEvent Kind discriminators published on bbops.telemetry.events.<sysid>.
	EventKindHeartbeatEdge     = "heartbeat_edge"
	EventKindFailsafeIncrement = "failsafe_increment"
	EventKindStatusText        = "statustext"
	EventKindLinkLost          = "link_lost"
	EventKindLinkRestored      = "link_restored"

	// DefaultLinkTimeout is the default silence threshold for bench and high-rate links (`3s`).
	DefaultLinkTimeout = DefaultLinkTimeoutMs * time.Millisecond
	// DefaultSupervisoryTimeout is the default silence threshold for mesh-supervisory links (`10s`).
	DefaultSupervisoryTimeout = DefaultSupervisoryTimeoutMs * time.Millisecond
)

// TelemetryFrame is the compact Lane 1 kinematic JSON payload published uncoalesced
// (`at-most-once` core NATS) onto `bbops.telemetry.<sysid>` for WebSocket #1 (`D2 Option A`).
type TelemetryFrame struct {
	SysID        uint8      `json:"sysid"`
	Domain       string     `json:"domain"`
	Seq          uint8      `json:"seq"`
	MsgID        uint32     `json:"msg_id"`
	QuatWXYZ     [4]float64 `json:"quat_wxyz"`
	HasAttitude  bool       `json:"has_attitude"`
	BodyRates    [3]float64 `json:"body_rates"`
	NEDXYZ       [3]float64 `json:"ned_xyz"`
	HasLocalPos  bool       `json:"has_local_pos"`
	NEDVel       [3]float64 `json:"ned_vel"`
	LatLonAlt    [3]float64 `json:"lat_lon_alt"`
	HasGlobalPos bool       `json:"has_global_pos"`
	HdgDeg       float64    `json:"hdg_deg"`
	HasHeading   bool       `json:"has_heading"`
	Timestamp    int64      `json:"timestamp"`
}

// StatusTextEvent carries #253 STATUSTEXT metadata inside a Lane 3 TransitionEvent.
type StatusTextEvent struct {
	Severity uint8  `json:"severity"`
	Text     string `json:"text"`
	ID       uint16 `json:"id"`
	ChunkSeq uint8  `json:"chunk_seq"`
}

// TransitionEvent is the Lane 3 edge payload published immediately onto
// `bbops.telemetry.events.<sysid>` whenever consecutive HEARTBEAT fields flip,
// #1 SYS_STATUS failsafes increment, #253 STATUSTEXT arrives, or per-sysid link
// freshness crosses silence/restoration boundaries (`link_lost` / `link_restored`).
type TransitionEvent struct {
	SysID          uint8                `json:"sysid"`
	EntityID       string               `json:"entity_id"`
	Domain         string               `json:"domain"`
	Seq            uint8                `json:"seq"`
	MsgID          uint32               `json:"msg_id"`
	Kind           string               `json:"kind"`
	Reasons        []string             `json:"reasons"`
	Armed          bool                 `json:"armed"`
	Authority      string               `json:"authority"`
	PrevAuthority  string               `json:"prev_authority,omitempty"`
	AuthorityEpoch uint64               `json:"authority_epoch,omitempty"`
	CompanionMode  string               `json:"companion_mode"`
	LinkTier       string               `json:"link_tier"`
	Flags          uxsvocab.HealthFlags `json:"flags"`
	Failsafes      int                  `json:"failsafes"`
	StatusText     *StatusTextEvent     `json:"status_text,omitempty"`
	Timestamp      int64                `json:"timestamp"`
}

// SnapshotBatchItem pairs a serialized BaseMessage(VehicleSnapshot) with its
// monotonic per-sysid state revision so confirmed revisions are not redundantly
// re-upserted while silent or unchanged.
type SnapshotBatchItem struct {
	SysID   uint8
	Rev     uint64
	Payload []byte
}

// ActuationTarget captures the pinned UDP endpoint and safety state of an
// admitted UXS vehicle atomically under the Tracker lock (`OQ1 Option A`).
type ActuationTarget struct {
	SysID                   uint8
	EntityID                string
	PeerAddr                netip.AddrPort
	Domain                  string
	Armed                   bool
	Authority               string
	AuthorityEpoch          uint64
	CompanionMode           string
	LinkTier                string
	LinkLost                bool
	HeartbeatObservedUnixNs int64
}

// IngestOutcome describes the deterministic side-effects produced by processing
// one inbound UDP datagram in Tracker.IngestDatagram.
type IngestOutcome struct {
	Accepted      bool
	CountedError  bool
	SysID         uint8
	Telemetry     *TelemetryFrame
	Events        []TransitionEvent
	CommandAck    *mavcodec.CommandAck
	CommandAckSeq uint8
	UrgentSKGSync bool
}

type ackFrameSlot struct {
	active      bool
	ack         mavcodec.CommandAck
	packetOrder uint64
}

type vehicleSession struct {
	snap           uxsvocab.VehicleSnapshot
	rev            uint64
	authorityEpoch uint64
	rxPacketOrder  uint64
	seenAckFrames  [256]ackFrameSlot
	lastPacketAt   time.Time
	lastSeq        uint8
	lastMsgID      uint32
	linkLost       bool
	hasSysStatus   bool
	hasAttitude    bool
	hasLocalPos    bool
	hasGlobalPos   bool
	hasHeading     bool
	peerAddr       netip.AddrPort
}

func normalizeAddrPort(peer netip.AddrPort) netip.AddrPort {
	if !peer.IsValid() {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port())
}

func (s *vehicleSession) recordAcceptedPacket(seq uint8, msgID uint32, peer netip.AddrPort, monoSample time.Time) {
	if s.linkLost {
		s.authorityEpoch++
		s.seenAckFrames = [256]ackFrameSlot{}
	}
	if msgID != mavcodec.MsgIDCommandAck {
		s.rxPacketOrder++
	}
	if s.lastPacketAt.IsZero() || !monoSample.Before(s.lastPacketAt) {
		s.lastPacketAt = monoSample
	}
	s.lastSeq = seq
	s.lastMsgID = msgID
	s.linkLost = false
	normPeer := normalizeAddrPort(peer)
	if normPeer.IsValid() {
		s.peerAddr = normPeer
	}
}

func observedUTC(ts time.Time) time.Time {
	utc := ts.UTC()
	if utc.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return utc
}

func monoOrEpoch(ts time.Time) time.Time {
	if ts.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return ts
}

func newLinkTransitionEvent(snap uxsvocab.VehicleSnapshot, authorityEpoch uint64, seq uint8, msgID uint32, kind, reason string, ts time.Time) TransitionEvent {
	return TransitionEvent{
		SysID:          snap.SysID,
		EntityID:       snap.EntityID(),
		Domain:         snap.Domain,
		Seq:            seq,
		MsgID:          msgID,
		Kind:           kind,
		Reasons:        []string{reason},
		Armed:          snap.Armed,
		Authority:      snap.Authority,
		AuthorityEpoch: authorityEpoch,
		CompanionMode:  snap.CompanionMode,
		LinkTier:       snap.LinkTier,
		Flags:          snap.Flags,
		Failsafes:      snap.Failsafes,
		Timestamp:      ts.UnixMilli(),
	}
}

// Tracker maintains per-sysid (1..=254, compid == 1) vehicle state, pinned peer
// UDP endpoints (`OQ1 Option A`), link-loss freshness, packet sanitization,
// `#77 COMMAND_ACK` correlation, and 3-lane split decisions.
type Tracker struct {
	org                string
	platform           string
	linkTimeout        time.Duration
	supervisoryTimeout time.Duration
	allowedPeers       []netip.Prefix

	mu       sync.RWMutex
	sessions map[uint8]*vehicleSession
}

// NewTracker constructs a per-sysid MAVLink v2 state tracker with default silence timeouts.
func NewTracker(org, platform string) (*Tracker, error) {
	return NewTrackerWithTimeouts(org, platform, DefaultLinkTimeout, DefaultSupervisoryTimeout)
}

// NewTrackerWithTimeouts constructs a per-sysid MAVLink v2 state tracker with explicit
// bench/high-rate and mesh-supervisory silence thresholds.
func NewTrackerWithTimeouts(org, platform string, linkTimeout, supervisoryTimeout time.Duration) (*Tracker, error) {
	return NewTrackerWithOptions(org, platform, linkTimeout, supervisoryTimeout, nil)
}

// NewTrackerWithOptions constructs a per-sysid MAVLink v2 state tracker with explicit
// silence thresholds and optional CIDR/IP allowed_peers admission filters.
func NewTrackerWithOptions(
	org, platform string,
	linkTimeout, supervisoryTimeout time.Duration,
	allowedPeers []netip.Prefix,
) (*Tracker, error) {
	if _, err := uxsvocab.VehicleEntityID(org, platform, 1); err != nil {
		return nil, fmt.Errorf("mavlink: invalid platform identity (%q, %q): %w", org, platform, err)
	}
	if linkTimeout <= 0 {
		return nil, fmt.Errorf("mavlink: linkTimeout (%v) must be > 0", linkTimeout)
	}
	if supervisoryTimeout < linkTimeout {
		return nil, fmt.Errorf("mavlink: supervisoryTimeout (%v) must be >= linkTimeout (%v)", supervisoryTimeout, linkTimeout)
	}
	var prefixes []netip.Prefix
	if len(allowedPeers) > 0 {
		prefixes = make([]netip.Prefix, len(allowedPeers))
		for i, pfx := range allowedPeers {
			unmapped := pfx.Addr().Unmap()
			bits := pfx.Bits()
			if pfx.Addr().Is4In6() && bits >= 96 {
				bits -= 96
			}
			prefixes[i] = netip.PrefixFrom(unmapped, bits).Masked()
		}
	}
	return &Tracker{
		org:                org,
		platform:           platform,
		linkTimeout:        linkTimeout,
		supervisoryTimeout: supervisoryTimeout,
		allowedPeers:       prefixes,
		sessions:           make(map[uint8]*vehicleSession),
	}, nil
}

func (t *Tracker) peerAllowed(peer netip.AddrPort) bool {
	if len(t.allowedPeers) == 0 {
		return true
	}
	if !peer.IsValid() {
		return false
	}
	addr := peer.Addr().Unmap()
	for _, pfx := range t.allowedPeers {
		if pfx.Contains(addr) {
			return true
		}
	}
	return false
}

func matchesPinnedPeer(sess *vehicleSession, normPeer netip.AddrPort) bool {
	if !sess.peerAddr.IsValid() {
		return true
	}
	return normPeer == sess.peerAddr
}

// IngestDatagram decodes a single MAVLink v2 (0xFD) UDP datagram, validates
// sender identity (`sysid` in `1..=254`, `compid == 1`) and payload invariants,
// updates in-memory vehicle state atomically per `sysid`, and returns the
// resulting Lane 1 / Lane 3 outputs without performing network I/O.
func (t *Tracker) IngestDatagram(datagram []byte, peer netip.AddrPort, now time.Time) IngestOutcome {
	frame, err := mavcodec.DecodeFrame(datagram)
	if err != nil {
		return IngestOutcome{CountedError: true}
	}
	return t.IngestFrame(frame, peer, now)
}

// IngestFrame processes an already-decoded MAVLink v2 Frame, preserving any Go
// monotonic clock reading on now for silence intervals while converting separately
// to UTC for published timestamps.
func (t *Tracker) IngestFrame(frame mavcodec.Frame, peer netip.AddrPort, now time.Time) IngestOutcome {
	return t.ingestFrameAt(frame, peer, observedUTC(now), monoOrEpoch(now))
}

func (t *Tracker) ingestFrameAt(frame mavcodec.Frame, peer netip.AddrPort, wallUTC, monoSample time.Time) IngestOutcome {
	if frame.Message == nil {
		return IngestOutcome{CountedError: true}
	}

	// Peer GCS heartbeats (commonly sysid=255 or MAV_TYPE_GCS=6 with custom_mode=0)
	// are benign peer traffic on shared UDP links and are ignored without counting errors.
	if hb, isHB := frame.Message.(mavcodec.Heartbeat); isHB {
		vh, err := mavcodec.DecodeHeartbeat(hb)
		if err != nil {
			if errors.Is(err, mavcodec.ErrNotBBStackHeartbeat) {
				return IngestOutcome{}
			}
			return IngestOutcome{CountedError: true}
		}
		if frame.SystemID < minVehicleSysID || frame.SystemID > maxVehicleSysID || frame.ComponentID != mavcodec.CompIDAutopilot1 {
			return IngestOutcome{CountedError: true}
		}
		return t.applyHeartbeat(frame.SystemID, frame.Sequence, vh, peer, wallUTC, monoSample)
	}

	if frame.SystemID < minVehicleSysID || frame.SystemID > maxVehicleSysID || frame.ComponentID != mavcodec.CompIDAutopilot1 {
		return IngestOutcome{CountedError: true}
	}

	switch msg := frame.Message.(type) {
	case mavcodec.SysStatus:
		return t.applySysStatus(frame.SystemID, frame.Sequence, msg, peer, wallUTC, monoSample)
	case mavcodec.AttitudeQuaternion:
		return t.applyAttitudeQuaternion(frame.SystemID, frame.Sequence, msg, peer, wallUTC, monoSample)
	case mavcodec.LocalPositionNED:
		return t.applyLocalPositionNED(frame.SystemID, frame.Sequence, msg, peer, wallUTC, monoSample)
	case mavcodec.GlobalPositionInt:
		return t.applyGlobalPositionInt(frame.SystemID, frame.Sequence, msg, peer, wallUTC, monoSample)
	case mavcodec.CommandAck:
		return t.applyCommandAck(frame.SystemID, frame.Sequence, msg, peer, wallUTC, monoSample)
	case mavcodec.StatusText:
		return t.applyStatusText(frame.SystemID, frame.Sequence, msg, peer, wallUTC, monoSample)
	default:
		// Outbound GCS actuation messages (#76, #84) are ignored cleanly if echoed on shared UDP links.
		return IngestOutcome{}
	}
}

func (t *Tracker) applyHeartbeat(sysid, seq uint8, vh mavcodec.VehicleHeartbeat, peer netip.AddrPort, wallUTC, monoSample time.Time) IngestOutcome {
	if !t.peerAllowed(peer) {
		return IngestOutcome{CountedError: true}
	}
	domainStr, err := vh.Custom.Domain.VocabString()
	if err != nil {
		return IngestOutcome{CountedError: true}
	}
	authStr, err := vh.Custom.ControlAuthority.VocabString()
	if err != nil {
		return IngestOutcome{CountedError: true}
	}
	modeStr, err := vh.Custom.CompanionMode.VocabString()
	if err != nil {
		return IngestOutcome{CountedError: true}
	}
	tierStr, err := vh.Custom.LinkTier.VocabString()
	if err != nil {
		return IngestOutcome{CountedError: true}
	}
	normPeer := normalizeAddrPort(peer)

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		snap, initErr := uxsvocab.NewVehicleSnapshot(t.org, t.platform, sysid, domainStr)
		if initErr != nil {
			return IngestOutcome{CountedError: true}
		}
		obsNs := unixNanoOrZero(wallUTC)
		snap.Armed = vh.Armed
		snap.Authority = authStr
		snap.CompanionMode = modeStr
		snap.LinkTier = tierStr
		snap.Flags = vh.Custom.HealthFlags
		snap.PoseMetadata = true
		snap.HeartbeatObservedUnixNs = obsNs
		snap.SysStatusObservedUnixNs = obsNs
		snap.AttitudeObservedUnixNs = obsNs
		snap.LocalPosObservedUnixNs = obsNs
		snap.GlobalPosObservedUnixNs = obsNs
		snap.HeadingObservedUnixNs = obsNs
		snap.ObservedAt = wallUTC
		if valErr := snap.Validate(); valErr != nil {
			return IngestOutcome{CountedError: true}
		}
		newSess := &vehicleSession{
			snap:           *snap,
			rev:            1,
			authorityEpoch: 1,
		}
		newSess.recordAcceptedPacket(seq, mavcodec.MsgIDHeartbeat, normPeer, monoSample)
		t.sessions[sysid] = newSess
		return IngestOutcome{
			Accepted: true,
			SysID:    sysid,
		}
	}

	if !sess.linkLost && !matchesPinnedPeer(sess, normPeer) {
		return IngestOutcome{CountedError: true}
	}

	wasLost := sess.linkLost
	prev := sess.snap
	next := prev
	next.Domain = domainStr
	next.Armed = vh.Armed
	next.Authority = authStr
	next.CompanionMode = modeStr
	next.LinkTier = tierStr
	next.Flags = vh.Custom.HealthFlags
	next.HeartbeatObservedUnixNs = unixNanoOrZero(wallUTC)
	next.ObservedAt = wallUTC
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	// shortcut: edge transition alerts evaluated in internal/mavlink, migrate declarative alert rules to processor/rule when semengine alpha.9 lands
	reasons := diffHeartbeatReasons(prev, next)

	sess.snap = next
	sess.rev++
	if !wasLost && (prev.Authority != next.Authority || prev.Armed != next.Armed || prev.LinkTier != next.LinkTier) {
		sess.authorityEpoch++
	}
	sess.recordAcceptedPacket(seq, mavcodec.MsgIDHeartbeat, normPeer, monoSample)

	out := IngestOutcome{
		Accepted: true,
		SysID:    sysid,
	}
	if wasLost {
		out.Events = append(out.Events, newLinkTransitionEvent(next, sess.authorityEpoch, seq, mavcodec.MsgIDHeartbeat, EventKindLinkRestored, "link_restored", wallUTC))
		out.UrgentSKGSync = true
	}
	if len(reasons) > 0 {
		prevAuth := ""
		if prev.Authority != next.Authority {
			prevAuth = prev.Authority
		}
		out.Events = append(out.Events, TransitionEvent{
			SysID:          sysid,
			EntityID:       next.EntityID(),
			Domain:         next.Domain,
			Seq:            seq,
			MsgID:          mavcodec.MsgIDHeartbeat,
			Kind:           EventKindHeartbeatEdge,
			Reasons:        reasons,
			Armed:          next.Armed,
			Authority:      next.Authority,
			PrevAuthority:  prevAuth,
			AuthorityEpoch: sess.authorityEpoch,
			CompanionMode:  next.CompanionMode,
			LinkTier:       next.LinkTier,
			Flags:          next.Flags,
			Failsafes:      next.Failsafes,
			Timestamp:      wallUTC.UnixMilli(),
		})
		out.UrgentSKGSync = true
	}
	return out
}

func diffHeartbeatReasons(prev, next uxsvocab.VehicleSnapshot) []string {
	var reasons []string
	if prev.Domain != next.Domain {
		reasons = append(reasons, "domain_changed")
	}
	if prev.Armed != next.Armed {
		reasons = append(reasons, "armed_changed")
	}
	if prev.Authority != next.Authority {
		reasons = append(reasons, "authority_changed")
	}
	if prev.CompanionMode != next.CompanionMode {
		reasons = append(reasons, "companion_mode_changed")
	}
	if prev.LinkTier != next.LinkTier {
		reasons = append(reasons, "link_tier_changed")
	}
	if prev.Flags != next.Flags {
		reasons = append(reasons, "health_flags_changed")
	}
	return reasons
}

func (t *Tracker) applySysStatus(sysid, seq uint8, msg mavcodec.SysStatus, peer netip.AddrPort, wallUTC, monoSample time.Time) IngestOutcome {
	if !t.peerAllowed(peer) {
		return IngestOutcome{CountedError: true}
	}
	normPeer := normalizeAddrPort(peer)

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}
	if !matchesPinnedPeer(sess, normPeer) {
		return IngestOutcome{CountedError: true}
	}

	wasLost := sess.linkLost
	hadSysStatus := sess.hasSysStatus
	prevFailsafes := sess.snap.Failsafes
	next := sess.snap
	next.LoopDtUs = int(msg.Load)
	next.CRCErrors = int(msg.ErrorsComm)
	next.Failsafes = int(msg.ErrorsCount1)
	next.MaxLoopDtUs = int(msg.ErrorsCount2)
	next.RTTUs = int(msg.ErrorsCount3)
	next.PoseMetadata = true
	next.HasSysStatus = true
	next.SysStatusObservedUnixNs = unixNanoOrZero(wallUTC)
	next.ObservedAt = wallUTC
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	sess.snap = next
	sess.hasSysStatus = true
	sess.rev++
	sess.recordAcceptedPacket(seq, mavcodec.MsgIDSysStatus, peer, monoSample)

	out := IngestOutcome{
		Accepted: true,
		SysID:    sysid,
	}
	if wasLost {
		out.Events = append(out.Events, newLinkTransitionEvent(next, sess.authorityEpoch, seq, mavcodec.MsgIDSysStatus, EventKindLinkRestored, "link_restored", wallUTC))
		out.UrgentSKGSync = true
	}
	if hadSysStatus && next.Failsafes > prevFailsafes {
		out.Events = append(out.Events, TransitionEvent{
			SysID:          sysid,
			EntityID:       next.EntityID(),
			Domain:         next.Domain,
			Seq:            seq,
			MsgID:          mavcodec.MsgIDSysStatus,
			Kind:           EventKindFailsafeIncrement,
			Reasons:        []string{"failsafes_incremented"},
			Armed:          next.Armed,
			Authority:      next.Authority,
			AuthorityEpoch: sess.authorityEpoch,
			CompanionMode:  next.CompanionMode,
			LinkTier:       next.LinkTier,
			Flags:          next.Flags,
			Failsafes:      next.Failsafes,
			Timestamp:      wallUTC.UnixMilli(),
		})
		out.UrgentSKGSync = true
	}
	return out
}

func (t *Tracker) applyAttitudeQuaternion(sysid, seq uint8, msg mavcodec.AttitudeQuaternion, peer netip.AddrPort, wallUTC, monoSample time.Time) IngestOutcome {
	if !t.peerAllowed(peer) {
		return IngestOutcome{CountedError: true}
	}
	rawQ := [4]float64{float64(msg.Q1), float64(msg.Q2), float64(msg.Q3), float64(msg.Q4)}
	rates := [3]float64{float64(msg.Rollspeed), float64(msg.Pitchspeed), float64(msg.Yawspeed)}
	for _, v := range rawQ {
		if !isFinite(v) {
			return IngestOutcome{CountedError: true}
		}
	}
	for _, v := range rates {
		if !isFinite(v) {
			return IngestOutcome{CountedError: true}
		}
	}
	normSq := rawQ[0]*rawQ[0] + rawQ[1]*rawQ[1] + rawQ[2]*rawQ[2] + rawQ[3]*rawQ[3]
	if !isFinite(normSq) || normSq < minQuatNormSq {
		return IngestOutcome{CountedError: true}
	}
	invNorm := 1.0 / math.Sqrt(normSq)
	unitQ := [4]float64{
		rawQ[0] * invNorm,
		rawQ[1] * invNorm,
		rawQ[2] * invNorm,
		rawQ[3] * invNorm,
	}
	hdgDeg := QuaternionYawDeg(unitQ)
	normPeer := normalizeAddrPort(peer)

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}
	if !matchesPinnedPeer(sess, normPeer) {
		return IngestOutcome{CountedError: true}
	}

	wasLost := sess.linkLost
	obsNs := unixNanoOrZero(wallUTC)
	next := sess.snap
	next.Quaternion = unitQ
	next.BodyRates = rates
	next.HeadingDeg = hdgDeg
	next.PoseMetadata = true
	next.HasAttitude = true
	next.AttitudeObservedUnixNs = obsNs
	next.HasHeading = true
	next.HeadingObservedUnixNs = obsNs
	next.ObservedAt = wallUTC
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	sess.snap = next
	sess.hasAttitude = true
	sess.hasHeading = true
	sess.rev++
	sess.recordAcceptedPacket(seq, mavcodec.MsgIDAttitudeQuaternion, normPeer, monoSample)

	tf := buildTelemetryFrame(sess, seq, mavcodec.MsgIDAttitudeQuaternion, wallUTC)
	out := IngestOutcome{
		Accepted:  true,
		SysID:     sysid,
		Telemetry: &tf,
	}
	if wasLost {
		out.Events = []TransitionEvent{newLinkTransitionEvent(next, sess.authorityEpoch, seq, mavcodec.MsgIDAttitudeQuaternion, EventKindLinkRestored, "link_restored", wallUTC)}
		out.UrgentSKGSync = true
	}
	return out
}

func (t *Tracker) applyLocalPositionNED(sysid, seq uint8, msg mavcodec.LocalPositionNED, peer netip.AddrPort, wallUTC, monoSample time.Time) IngestOutcome {
	if !t.peerAllowed(peer) {
		return IngestOutcome{CountedError: true}
	}
	ned := [3]float64{float64(msg.X), float64(msg.Y), float64(msg.Z)}
	vx, vy, vz := float64(msg.Vx), float64(msg.Vy), float64(msg.Vz)
	for _, v := range [6]float64{ned[0], ned[1], ned[2], vx, vy, vz} {
		if !isFinite(v) {
			return IngestOutcome{CountedError: true}
		}
	}
	normPeer := normalizeAddrPort(peer)

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}
	if !matchesPinnedPeer(sess, normPeer) {
		return IngestOutcome{CountedError: true}
	}

	wasLost := sess.linkLost
	obsNs := unixNanoOrZero(wallUTC)
	next := sess.snap
	next.NED = ned
	next.Vx = vx
	next.Vy = vy
	next.Vz = vz
	next.PoseMetadata = true
	next.HasLocalPos = true
	next.LocalPosObservedUnixNs = obsNs
	next.ObservedAt = wallUTC
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	sess.snap = next
	sess.hasLocalPos = true
	sess.rev++
	sess.recordAcceptedPacket(seq, mavcodec.MsgIDLocalPositionNED, normPeer, monoSample)

	tf := buildTelemetryFrame(sess, seq, mavcodec.MsgIDLocalPositionNED, wallUTC)
	out := IngestOutcome{
		Accepted:  true,
		SysID:     sysid,
		Telemetry: &tf,
	}
	if wasLost {
		out.Events = []TransitionEvent{newLinkTransitionEvent(next, sess.authorityEpoch, seq, mavcodec.MsgIDLocalPositionNED, EventKindLinkRestored, "link_restored", wallUTC)}
		out.UrgentSKGSync = true
	}
	return out
}

func (t *Tracker) applyGlobalPositionInt(sysid, seq uint8, msg mavcodec.GlobalPositionInt, peer netip.AddrPort, wallUTC, monoSample time.Time) IngestOutcome {
	if !t.peerAllowed(peer) {
		return IngestOutcome{CountedError: true}
	}
	if msg.Lat < minLatDegE7 || msg.Lat > maxLatDegE7 {
		return IngestOutcome{CountedError: true}
	}
	if msg.Lon < minLonDegE7 || msg.Lon > maxLonDegE7 {
		return IngestOutcome{CountedError: true}
	}
	if msg.Hdg > maxValidHdgCdeg && msg.Hdg != unknownHdgUint16 {
		return IngestOutcome{CountedError: true}
	}

	lat := float64(msg.Lat) / 1e7
	lon := float64(msg.Lon) / 1e7
	alt := float64(msg.Alt) / 1000.0
	normPeer := normalizeAddrPort(peer)

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}
	if !matchesPinnedPeer(sess, normPeer) {
		return IngestOutcome{CountedError: true}
	}

	wasLost := sess.linkLost
	obsNs := unixNanoOrZero(wallUTC)
	next := sess.snap
	next.Latitude = lat
	next.Longitude = lon
	next.Altitude = alt
	next.PoseMetadata = true
	next.HasGlobalPos = true
	next.GlobalPosObservedUnixNs = obsNs
	hasFallbackHdg := !sess.hasAttitude && msg.Hdg <= maxValidHdgCdeg
	if !sess.hasAttitude {
		if hasFallbackHdg {
			next.HeadingDeg = float64(msg.Hdg) / 100.0
			next.HasHeading = true
			next.HeadingObservedUnixNs = obsNs
		} else {
			next.HeadingDeg = 0
			next.HasHeading = false
			next.HeadingObservedUnixNs = obsNs
		}
	}
	next.ObservedAt = wallUTC
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	sess.snap = next
	sess.hasGlobalPos = true
	if !sess.hasAttitude {
		sess.hasHeading = hasFallbackHdg
	}
	sess.rev++
	sess.recordAcceptedPacket(seq, mavcodec.MsgIDGlobalPositionInt, normPeer, monoSample)

	tf := buildTelemetryFrame(sess, seq, mavcodec.MsgIDGlobalPositionInt, wallUTC)
	out := IngestOutcome{
		Accepted:  true,
		SysID:     sysid,
		Telemetry: &tf,
	}
	if wasLost {
		out.Events = []TransitionEvent{newLinkTransitionEvent(next, sess.authorityEpoch, seq, mavcodec.MsgIDGlobalPositionInt, EventKindLinkRestored, "link_restored", wallUTC)}
		out.UrgentSKGSync = true
	}
	return out
}

func (t *Tracker) applyCommandAck(sysid, seq uint8, msg mavcodec.CommandAck, peer netip.AddrPort, wallUTC, monoSample time.Time) IngestOutcome {
	if !t.peerAllowed(peer) {
		return IngestOutcome{CountedError: true}
	}
	if msg.TargetSystem != 0 && msg.TargetSystem != mavcodec.GCSSystemID {
		return IngestOutcome{}
	}
	normPeer := normalizeAddrPort(peer)

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}
	if !matchesPinnedPeer(sess, normPeer) {
		return IngestOutcome{CountedError: true}
	}

	wasLost := sess.linkLost
	prevSlot := sess.seenAckFrames[seq]
	isDupAck := !wasLost && prevSlot.active && prevSlot.ack == msg && sess.rxPacketOrder-prevSlot.packetOrder < 256

	var events []TransitionEvent
	urgent := false
	next := sess.snap
	if wasLost {
		next.ObservedAt = wallUTC
		if err := next.Validate(); err != nil {
			return IngestOutcome{CountedError: true}
		}
		sess.snap = next
		sess.rev++
		urgent = true
	}
	sess.recordAcceptedPacket(seq, mavcodec.MsgIDCommandAck, normPeer, monoSample)
	if wasLost {
		events = append(events, newLinkTransitionEvent(next, sess.authorityEpoch, seq, mavcodec.MsgIDCommandAck, EventKindLinkRestored, "link_restored", wallUTC))
	}
	if isDupAck {
		return IngestOutcome{
			Accepted:      true,
			SysID:         sysid,
			Events:        events,
			UrgentSKGSync: urgent,
		}
	}
	sess.rxPacketOrder++
	sess.seenAckFrames[seq] = ackFrameSlot{
		active:      true,
		ack:         msg,
		packetOrder: sess.rxPacketOrder,
	}
	ackCopy := msg
	return IngestOutcome{
		Accepted:      true,
		SysID:         sysid,
		Events:        events,
		CommandAck:    &ackCopy,
		CommandAckSeq: seq,
		UrgentSKGSync: urgent,
	}
}

func (t *Tracker) applyStatusText(sysid, seq uint8, msg mavcodec.StatusText, peer netip.AddrPort, wallUTC, monoSample time.Time) IngestOutcome {
	if !t.peerAllowed(peer) {
		return IngestOutcome{CountedError: true}
	}
	normPeer := normalizeAddrPort(peer)

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}
	if !matchesPinnedPeer(sess, normPeer) {
		return IngestOutcome{CountedError: true}
	}

	wasLost := sess.linkLost
	snap := sess.snap
	var events []TransitionEvent
	urgent := false

	if wasLost {
		next := sess.snap
		next.ObservedAt = wallUTC
		if err := next.Validate(); err != nil {
			return IngestOutcome{CountedError: true}
		}
		sess.snap = next
		sess.rev++
		snap = next
		urgent = true
	}
	sess.recordAcceptedPacket(seq, mavcodec.MsgIDStatusText, normPeer, monoSample)
	if wasLost {
		events = append(events, newLinkTransitionEvent(snap, sess.authorityEpoch, seq, mavcodec.MsgIDStatusText, EventKindLinkRestored, "link_restored", wallUTC))
	}

	cleanText := strings.TrimRight(msg.Text, "\x00")
	events = append(events, TransitionEvent{
		SysID:          sysid,
		EntityID:       snap.EntityID(),
		Domain:         snap.Domain,
		Seq:            seq,
		MsgID:          mavcodec.MsgIDStatusText,
		Kind:           EventKindStatusText,
		Reasons:        []string{"statustext"},
		Armed:          snap.Armed,
		Authority:      snap.Authority,
		AuthorityEpoch: sess.authorityEpoch,
		CompanionMode:  snap.CompanionMode,
		LinkTier:       snap.LinkTier,
		Flags:          snap.Flags,
		Failsafes:      snap.Failsafes,
		StatusText: &StatusTextEvent{
			Severity: msg.Severity,
			Text:     cleanText,
			ID:       msg.ID,
			ChunkSeq: msg.ChunkSeq,
		},
		Timestamp: wallUTC.UnixMilli(),
	})

	return IngestOutcome{
		Accepted:      true,
		SysID:         sysid,
		Events:        events,
		UrgentSKGSync: urgent,
	}
}

func buildTelemetryFrame(sess *vehicleSession, seq uint8, msgID uint32, now time.Time) TelemetryFrame {
	return TelemetryFrame{
		SysID:        sess.snap.SysID,
		Domain:       sess.snap.Domain,
		Seq:          seq,
		MsgID:        msgID,
		QuatWXYZ:     sess.snap.Quaternion,
		HasAttitude:  sess.hasAttitude,
		BodyRates:    sess.snap.BodyRates,
		NEDXYZ:       sess.snap.NED,
		HasLocalPos:  sess.hasLocalPos,
		NEDVel:       [3]float64{sess.snap.Vx, sess.snap.Vy, sess.snap.Vz},
		LatLonAlt:    [3]float64{sess.snap.Latitude, sess.snap.Longitude, sess.snap.Altitude},
		HasGlobalPos: sess.hasGlobalPos,
		HdgDeg:       sess.snap.HeadingDeg,
		HasHeading:   sess.hasHeading,
		Timestamp:    now.UnixMilli(),
	}
}

// QuaternionYawDeg computes the ZYX Euler nose/bow heading angle in [0, 360) degrees
// clockwise from North in the NED frame from a unit Hamilton quaternion [w, x, y, z]
// (Fossen, Handbook of Marine Craft Hydrodynamics and Motion Control, §2.2.2).
func QuaternionYawDeg(unitWXYZ [4]float64) float64 {
	w, x, y, z := unitWXYZ[0], unitWXYZ[1], unitWXYZ[2], unitWXYZ[3]
	yawRad := math.Atan2(2.0*(w*z+x*y), 1.0-2.0*(y*y+z*z))
	deg := math.Mod(yawRad*(180.0/math.Pi), 360.0)
	if deg < 0 {
		deg += 360.0
	}
	if deg >= 360.0 || deg == 0 {
		return 0
	}
	return deg
}

// Snapshot returns a value copy of the current VehicleSnapshot for sysid if admitted.
func (t *Tracker) Snapshot(sysid uint8) (uxsvocab.VehicleSnapshot, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	sess, ok := t.sessions[sysid]
	if !ok {
		return uxsvocab.VehicleSnapshot{}, false
	}
	return sess.snap, true
}

func (t *Tracker) lastPacketSample(sysid uint8) (time.Time, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	sess, ok := t.sessions[sysid]
	if !ok {
		return time.Time{}, false
	}
	return sess.lastPacketAt, true
}

// IsLinkLost reports whether an admitted sysid is currently marked link-lost.
func (t *Tracker) IsLinkLost(sysid uint8) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	sess, ok := t.sessions[sysid]
	return ok && sess.linkLost
}

// EvaluateSilence inspects all admitted sessions against their LinkTier silence
// threshold (`OQ1 Option A`) at now, preserving any Go monotonic clock reading on now
// for elapsed-time comparisons while converting separately to UTC for published
// timestamps, and returning one `link_lost` TransitionEvent per newly crossed session
// in ascending sysid order without mutating VehicleSnapshot timestamps (`OQ2 Option A`).
func (t *Tracker) EvaluateSilence(now time.Time) []TransitionEvent {
	return t.evaluateSilenceAt(observedUTC(now), monoOrEpoch(now))
}

func (t *Tracker) admittedSysIDs() []uint8 {
	t.mu.RLock()
	out := make([]uint8, 0, len(t.sessions))
	for sysid := range t.sessions {
		out = append(out, sysid)
	}
	t.mu.RUnlock()
	slices.Sort(out)
	return out
}

func (t *Tracker) evaluateSilenceSysID(sysid uint8, now time.Time) (TransitionEvent, bool) {
	wallUTC := observedUTC(now)
	monoSample := monoOrEpoch(now)

	t.mu.Lock()
	defer t.mu.Unlock()
	sess, ok := t.sessions[sysid]
	if !ok {
		return TransitionEvent{}, false
	}
	return t.evaluateSessionSilence(sess, wallUTC, monoSample)
}

func (t *Tracker) evaluateSilenceAt(wallUTC, monoSample time.Time) []TransitionEvent {
	t.mu.Lock()
	defer t.mu.Unlock()

	var events []TransitionEvent
	for _, sess := range t.sessions {
		if ev, lost := t.evaluateSessionSilence(sess, wallUTC, monoSample); lost {
			events = append(events, ev)
		}
	}
	slices.SortFunc(events, func(a, b TransitionEvent) int {
		return int(a.SysID) - int(b.SysID)
	})
	return events
}

func (t *Tracker) evaluateSessionSilence(sess *vehicleSession, wallUTC, monoSample time.Time) (TransitionEvent, bool) {
	if sess.linkLost {
		return TransitionEvent{}, false
	}
	threshold := t.linkTimeout
	if sess.snap.LinkTier == uxsvocab.LinkTierMeshSupervisory {
		threshold = t.supervisoryTimeout
	}
	if monoSample.Sub(sess.lastPacketAt) < threshold {
		return TransitionEvent{}, false
	}
	sess.linkLost = true
	sess.authorityEpoch++
	sess.seenAckFrames = [256]ackFrameSlot{}
	return newLinkTransitionEvent(
		sess.snap,
		sess.authorityEpoch,
		sess.lastSeq,
		sess.lastMsgID,
		EventKindLinkLost,
		"silence_timeout",
		wallUTC,
	), true
}

// PeerAddr returns the latest validated UDP endpoint recorded for an admitted sysid.
func (t *Tracker) PeerAddr(sysid uint8) (netip.AddrPort, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	sess, ok := t.sessions[sysid]
	if !ok || !sess.peerAddr.IsValid() {
		return netip.AddrPort{}, false
	}
	return sess.peerAddr, true
}

// Peers returns a snapshot map of all admitted sysids that have a recorded UDP peer endpoint.
func (t *Tracker) Peers() map[uint8]netip.AddrPort {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[uint8]netip.AddrPort, len(t.sessions))
	for sysid, sess := range t.sessions {
		if sess.peerAddr.IsValid() {
			out[sysid] = sess.peerAddr
		}
	}
	return out
}

// ActuationTarget returns the pinned UDP endpoint and current safety snapshot
// for sysid atomically under the Tracker lock (`OQ1 Option A`).
func (t *Tracker) ActuationTarget(sysid uint8) (ActuationTarget, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	sess, ok := t.sessions[sysid]
	if !ok || !sess.peerAddr.IsValid() {
		return ActuationTarget{}, false
	}
	return ActuationTarget{
		SysID:                   sysid,
		EntityID:                sess.snap.EntityID(),
		PeerAddr:                sess.peerAddr,
		Domain:                  sess.snap.Domain,
		Armed:                   sess.snap.Armed,
		Authority:               sess.snap.Authority,
		AuthorityEpoch:          sess.authorityEpoch,
		CompanionMode:           sess.snap.CompanionMode,
		LinkTier:                sess.snap.LinkTier,
		LinkLost:                sess.linkLost,
		HeartbeatObservedUnixNs: sess.snap.HeartbeatObservedUnixNs,
	}, true
}

// BuildPeriodicBatch marshals BaseMessage(VehicleSnapshot) items at flush time in ascending
// sysid order for admitted sessions whose revision exceeds confirmedRevs[sysid] (`OQ3 Option A`).
func (t *Tracker) BuildPeriodicBatch(confirmedRevs map[uint8]uint64) ([]SnapshotBatchItem, error) {
	type captured struct {
		snap uxsvocab.VehicleSnapshot
		rev  uint64
	}

	t.mu.RLock()
	snaps := make([]captured, 0, len(t.sessions))
	for sysid, sess := range t.sessions {
		if sess.rev <= confirmedRevs[sysid] {
			continue
		}
		snaps = append(snaps, captured{snap: sess.snap, rev: sess.rev})
	}
	t.mu.RUnlock()

	if len(snaps) == 0 {
		return nil, nil
	}
	slices.SortFunc(snaps, func(a, b captured) int {
		return int(a.snap.SysID) - int(b.snap.SysID)
	})

	items := make([]SnapshotBatchItem, 0, len(snaps))
	for _, c := range snaps {
		raw, err := marshalSnapshotBaseMessage(c.snap)
		if err != nil {
			return nil, err
		}
		items = append(items, SnapshotBatchItem{
			SysID:   c.snap.SysID,
			Rev:     c.rev,
			Payload: raw,
		})
	}
	return items, nil
}

// BuildUrgentBatch marshals BaseMessage(VehicleSnapshot) items for the requested
// sysids in ascending sysid order, skipping any sysid not currently admitted.
func (t *Tracker) BuildUrgentBatch(sysids []uint8) ([]SnapshotBatchItem, error) {
	if len(sysids) == 0 {
		return nil, nil
	}
	sorted := slices.Clone(sysids)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	type captured struct {
		snap uxsvocab.VehicleSnapshot
		rev  uint64
	}
	t.mu.RLock()
	snaps := make([]captured, 0, len(sorted))
	for _, sysid := range sorted {
		if sess, ok := t.sessions[sysid]; ok {
			snaps = append(snaps, captured{snap: sess.snap, rev: sess.rev})
		}
	}
	t.mu.RUnlock()

	items := make([]SnapshotBatchItem, 0, len(snaps))
	for _, c := range snaps {
		raw, err := marshalSnapshotBaseMessage(c.snap)
		if err != nil {
			return nil, err
		}
		items = append(items, SnapshotBatchItem{
			SysID:   c.snap.SysID,
			Rev:     c.rev,
			Payload: raw,
		})
	}
	return items, nil
}

func marshalSnapshotBaseMessage(snap uxsvocab.VehicleSnapshot) ([]byte, error) {
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	msg := message.NewBaseMessage(snap.Schema(), &snap, uxsvocab.TripleSource)
	return json.Marshal(msg)
}

func unixNanoOrZero(ts time.Time) int64 {
	if ts.IsZero() {
		return 0
	}
	if ns := ts.UTC().UnixNano(); ns > 0 {
		return ns
	}
	return 0
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
