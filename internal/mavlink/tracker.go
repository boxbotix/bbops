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
// #1 SYS_STATUS failsafes increment, or #253 STATUSTEXT arrives.
type TransitionEvent struct {
	SysID         uint8                `json:"sysid"`
	EntityID      string               `json:"entity_id"`
	Domain        string               `json:"domain"`
	Seq           uint8                `json:"seq"`
	MsgID         uint32               `json:"msg_id"`
	Kind          string               `json:"kind"`
	Reasons       []string             `json:"reasons"`
	Armed         bool                 `json:"armed"`
	Authority     string               `json:"authority"`
	PrevAuthority string               `json:"prev_authority,omitempty"`
	CompanionMode string               `json:"companion_mode"`
	LinkTier      string               `json:"link_tier"`
	Flags         uxsvocab.HealthFlags `json:"flags"`
	Failsafes     int                  `json:"failsafes"`
	StatusText    *StatusTextEvent     `json:"status_text,omitempty"`
	Timestamp     int64                `json:"timestamp"`
}

// SnapshotBatchItem pairs a serialized BaseMessage(VehicleSnapshot) with its
// monotonic per-sysid state revision so older periodic batches never overwrite
// a newer out-of-band Lane 3 edge snapshot.
type SnapshotBatchItem struct {
	SysID   uint8
	Rev     uint64
	Payload []byte
}

// IngestOutcome describes the deterministic side-effects produced by processing
// one inbound UDP datagram in Tracker.IngestDatagram.
type IngestOutcome struct {
	Accepted      bool
	CountedError  bool
	SysID         uint8
	Telemetry     *TelemetryFrame
	Event         *TransitionEvent
	UrgentSKGSync bool
}

type vehicleSession struct {
	snap         uxsvocab.VehicleSnapshot
	rev          uint64
	hasSysStatus bool
	hasAttitude  bool
	hasLocalPos  bool
	hasGlobalPos bool
	peerAddr     netip.AddrPort
}

// Tracker maintains per-sysid (1..=254, compid == 1) vehicle state, peer UDP
// endpoints (`D4 Option A`), packet sanitization, and 3-lane split decisions.
type Tracker struct {
	org      string
	platform string

	mu       sync.RWMutex
	sessions map[uint8]*vehicleSession
}

// NewTracker constructs a per-sysid MAVLink v2 state tracker scoped to org and platform.
func NewTracker(org, platform string) (*Tracker, error) {
	if _, err := uxsvocab.VehicleEntityID(org, platform, 1); err != nil {
		return nil, fmt.Errorf("mavlink: invalid platform identity (%q, %q): %w", org, platform, err)
	}
	return &Tracker{
		org:      org,
		platform: platform,
		sessions: make(map[uint8]*vehicleSession),
	}, nil
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

// IngestFrame processes an already-decoded MAVLink v2 Frame.
func (t *Tracker) IngestFrame(frame mavcodec.Frame, peer netip.AddrPort, now time.Time) IngestOutcome {
	if frame.Message == nil {
		return IngestOutcome{CountedError: true}
	}
	observedAt := now.UTC()
	if observedAt.IsZero() {
		observedAt = time.Unix(0, 0).UTC()
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
		return t.applyHeartbeat(frame.SystemID, frame.Sequence, vh, peer, observedAt)
	}

	if frame.SystemID < minVehicleSysID || frame.SystemID > maxVehicleSysID || frame.ComponentID != mavcodec.CompIDAutopilot1 {
		return IngestOutcome{CountedError: true}
	}

	switch msg := frame.Message.(type) {
	case mavcodec.SysStatus:
		return t.applySysStatus(frame.SystemID, frame.Sequence, msg, peer, observedAt)
	case mavcodec.AttitudeQuaternion:
		return t.applyAttitudeQuaternion(frame.SystemID, frame.Sequence, msg, peer, observedAt)
	case mavcodec.LocalPositionNED:
		return t.applyLocalPositionNED(frame.SystemID, frame.Sequence, msg, peer, observedAt)
	case mavcodec.GlobalPositionInt:
		return t.applyGlobalPositionInt(frame.SystemID, frame.Sequence, msg, peer, observedAt)
	case mavcodec.StatusText:
		return t.applyStatusText(frame.SystemID, frame.Sequence, msg, peer, observedAt)
	default:
		// Outbound/actuation messages (#76, #77, #84) are outside M1.2 telemetry ingest scope.
		return IngestOutcome{}
	}
}

func (t *Tracker) applyHeartbeat(sysid, seq uint8, vh mavcodec.VehicleHeartbeat, peer netip.AddrPort, now time.Time) IngestOutcome {
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

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		snap, initErr := uxsvocab.NewVehicleSnapshot(t.org, t.platform, sysid, domainStr)
		if initErr != nil {
			return IngestOutcome{CountedError: true}
		}
		snap.Armed = vh.Armed
		snap.Authority = authStr
		snap.CompanionMode = modeStr
		snap.LinkTier = tierStr
		snap.Flags = vh.Custom.HealthFlags
		snap.ObservedAt = now
		if valErr := snap.Validate(); valErr != nil {
			return IngestOutcome{CountedError: true}
		}
		newSess := &vehicleSession{
			snap: *snap,
			rev:  1,
		}
		if peer.IsValid() {
			newSess.peerAddr = peer
		}
		t.sessions[sysid] = newSess
		return IngestOutcome{
			Accepted: true,
			SysID:    sysid,
		}
	}

	prev := sess.snap
	next := prev
	next.Domain = domainStr
	next.Armed = vh.Armed
	next.Authority = authStr
	next.CompanionMode = modeStr
	next.LinkTier = tierStr
	next.Flags = vh.Custom.HealthFlags
	next.ObservedAt = now
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	// shortcut: edge transition alerts evaluated in internal/mavlink, migrate declarative alert rules to processor/rule when semengine alpha.9 lands
	reasons := diffHeartbeatReasons(prev, next)

	sess.snap = next
	sess.rev++
	if peer.IsValid() {
		sess.peerAddr = peer
	}

	out := IngestOutcome{
		Accepted: true,
		SysID:    sysid,
	}
	if len(reasons) > 0 {
		prevAuth := ""
		if prev.Authority != next.Authority {
			prevAuth = prev.Authority
		}
		out.Event = &TransitionEvent{
			SysID:         sysid,
			EntityID:      next.EntityID(),
			Domain:        next.Domain,
			Seq:           seq,
			MsgID:         mavcodec.MsgIDHeartbeat,
			Kind:          EventKindHeartbeatEdge,
			Reasons:       reasons,
			Armed:         next.Armed,
			Authority:     next.Authority,
			PrevAuthority: prevAuth,
			CompanionMode: next.CompanionMode,
			LinkTier:      next.LinkTier,
			Flags:         next.Flags,
			Failsafes:     next.Failsafes,
			Timestamp:     now.UnixMilli(),
		}
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

func (t *Tracker) applySysStatus(sysid, seq uint8, msg mavcodec.SysStatus, peer netip.AddrPort, now time.Time) IngestOutcome {
	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}

	hadSysStatus := sess.hasSysStatus
	prevFailsafes := sess.snap.Failsafes
	next := sess.snap
	next.LoopDtUs = int(msg.Load)
	next.CRCErrors = int(msg.ErrorsComm)
	next.Failsafes = int(msg.ErrorsCount1)
	next.MaxLoopDtUs = int(msg.ErrorsCount2)
	next.RTTUs = int(msg.ErrorsCount3)
	next.ObservedAt = now
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	sess.snap = next
	sess.hasSysStatus = true
	sess.rev++
	if peer.IsValid() {
		sess.peerAddr = peer
	}

	out := IngestOutcome{
		Accepted: true,
		SysID:    sysid,
	}
	if hadSysStatus && next.Failsafes > prevFailsafes {
		out.Event = &TransitionEvent{
			SysID:         sysid,
			EntityID:      next.EntityID(),
			Domain:        next.Domain,
			Seq:           seq,
			MsgID:         mavcodec.MsgIDSysStatus,
			Kind:          EventKindFailsafeIncrement,
			Reasons:       []string{"failsafes_incremented"},
			Armed:         next.Armed,
			Authority:     next.Authority,
			CompanionMode: next.CompanionMode,
			LinkTier:      next.LinkTier,
			Flags:         next.Flags,
			Failsafes:     next.Failsafes,
			Timestamp:     now.UnixMilli(),
		}
		out.UrgentSKGSync = true
	}
	return out
}

func (t *Tracker) applyAttitudeQuaternion(sysid, seq uint8, msg mavcodec.AttitudeQuaternion, peer netip.AddrPort, now time.Time) IngestOutcome {
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

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}

	next := sess.snap
	next.Quaternion = unitQ
	next.BodyRates = rates
	next.HeadingDeg = hdgDeg
	next.ObservedAt = now
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	sess.snap = next
	sess.hasAttitude = true
	sess.rev++
	if peer.IsValid() {
		sess.peerAddr = peer
	}

	tf := buildTelemetryFrame(sess, seq, mavcodec.MsgIDAttitudeQuaternion, now)
	return IngestOutcome{
		Accepted:  true,
		SysID:     sysid,
		Telemetry: &tf,
	}
}

func (t *Tracker) applyLocalPositionNED(sysid, seq uint8, msg mavcodec.LocalPositionNED, peer netip.AddrPort, now time.Time) IngestOutcome {
	ned := [3]float64{float64(msg.X), float64(msg.Y), float64(msg.Z)}
	vx, vy, vz := float64(msg.Vx), float64(msg.Vy), float64(msg.Vz)
	for _, v := range [6]float64{ned[0], ned[1], ned[2], vx, vy, vz} {
		if !isFinite(v) {
			return IngestOutcome{CountedError: true}
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}

	next := sess.snap
	next.NED = ned
	next.Vx = vx
	next.Vy = vy
	next.Vz = vz
	next.ObservedAt = now
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	sess.snap = next
	sess.hasLocalPos = true
	sess.rev++
	if peer.IsValid() {
		sess.peerAddr = peer
	}

	tf := buildTelemetryFrame(sess, seq, mavcodec.MsgIDLocalPositionNED, now)
	return IngestOutcome{
		Accepted:  true,
		SysID:     sysid,
		Telemetry: &tf,
	}
}

func (t *Tracker) applyGlobalPositionInt(sysid, seq uint8, msg mavcodec.GlobalPositionInt, peer netip.AddrPort, now time.Time) IngestOutcome {
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

	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}

	next := sess.snap
	next.Latitude = lat
	next.Longitude = lon
	next.Altitude = alt
	if !sess.hasAttitude && msg.Hdg <= maxValidHdgCdeg {
		next.HeadingDeg = float64(msg.Hdg) / 100.0
	}
	next.ObservedAt = now
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	sess.snap = next
	sess.hasGlobalPos = true
	sess.rev++
	if peer.IsValid() {
		sess.peerAddr = peer
	}

	tf := buildTelemetryFrame(sess, seq, mavcodec.MsgIDGlobalPositionInt, now)
	return IngestOutcome{
		Accepted:  true,
		SysID:     sysid,
		Telemetry: &tf,
	}
}

func (t *Tracker) applyStatusText(sysid, seq uint8, msg mavcodec.StatusText, peer netip.AddrPort, now time.Time) IngestOutcome {
	t.mu.Lock()
	defer t.mu.Unlock()

	sess, exists := t.sessions[sysid]
	if !exists {
		return IngestOutcome{}
	}

	next := sess.snap
	next.ObservedAt = now
	if err := next.Validate(); err != nil {
		return IngestOutcome{CountedError: true}
	}

	sess.snap = next
	sess.rev++
	if peer.IsValid() {
		sess.peerAddr = peer
	}

	cleanText := strings.TrimRight(msg.Text, "\x00")
	return IngestOutcome{
		Accepted: true,
		SysID:    sysid,
		Event: &TransitionEvent{
			SysID:         sysid,
			EntityID:      next.EntityID(),
			Domain:        next.Domain,
			Seq:           seq,
			MsgID:         mavcodec.MsgIDStatusText,
			Kind:          EventKindStatusText,
			Reasons:       []string{"statustext"},
			Armed:         next.Armed,
			Authority:     next.Authority,
			CompanionMode: next.CompanionMode,
			LinkTier:      next.LinkTier,
			Flags:         next.Flags,
			Failsafes:     next.Failsafes,
			StatusText: &StatusTextEvent{
				Severity: msg.Severity,
				Text:     cleanText,
				ID:       msg.ID,
				ChunkSeq: msg.ChunkSeq,
			},
			Timestamp: now.UnixMilli(),
		},
		UrgentSKGSync: true,
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

// BuildPeriodicBatch marshals BaseMessage(VehicleSnapshot) items for every currently
// admitted sysid in ascending sysid order.
func (t *Tracker) BuildPeriodicBatch() ([]SnapshotBatchItem, error) {
	t.mu.RLock()
	sysids := make([]uint8, 0, len(t.sessions))
	for sysid := range t.sessions {
		sysids = append(sysids, sysid)
	}
	slices.Sort(sysids)
	type captured struct {
		snap uxsvocab.VehicleSnapshot
		rev  uint64
	}
	snaps := make([]captured, 0, len(sysids))
	for _, sysid := range sysids {
		sess := t.sessions[sysid]
		snaps = append(snaps, captured{snap: sess.snap, rev: sess.rev})
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

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
