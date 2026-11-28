package mavlink

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boxbotix/bbops/internal/mavcodec"
	"github.com/boxbotix/bbops/internal/uxsvocab"
	"github.com/c360studio/semengine/message"
)

var trackerIncarnationCounter atomic.Uint64

func newTrackerIncarnation() string {
	seq := trackerIncarnationCounter.Add(1)
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "t" + strconv.FormatUint(seq, 10)
	}
	return "t" + hex.EncodeToString(raw[:]) + "-" + strconv.FormatUint(seq, 10)
}

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
	SysID                uint8                `json:"sysid"`
	EntityID             string               `json:"entity_id"`
	Domain               string               `json:"domain"`
	Seq                  uint8                `json:"seq"`
	MsgID                uint32               `json:"msg_id"`
	Kind                 string               `json:"kind"`
	Reasons              []string             `json:"reasons"`
	Armed                bool                 `json:"armed"`
	Authority            string               `json:"authority"`
	PrevAuthority        string               `json:"prev_authority,omitempty"`
	AuthorityIncarnation string               `json:"authority_incarnation,omitempty"`
	AuthorityEpoch       uint64               `json:"authority_epoch,omitempty"`
	ObservationRev       uint64               `json:"observation_rev,omitempty"`
	CompanionMode        string               `json:"companion_mode"`
	LinkTier             string               `json:"link_tier"`
	Flags                uxsvocab.HealthFlags `json:"flags"`
	Failsafes            int                  `json:"failsafes"`
	StatusText           *StatusTextEvent     `json:"status_text,omitempty"`
	Timestamp            int64                `json:"timestamp"`
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
	SysID                     uint8
	EntityID                  string
	PeerAddr                  netip.AddrPort
	Domain                    string
	Armed                     bool
	Authority                 string
	AuthorityIncarnation      string
	AuthorityEpoch            uint64
	ObservationRev            uint64
	HeartbeatSeq              uint64
	HeartbeatOrder            uint64
	HeartbeatReceiptRev       uint64
	LatestHeartbeatReceiptRev uint64
	LinkEpoch                 uint64
	PacketOrder               uint64
	LastAckOrder              uint64
	LastAckSeq                uint64
	LastAckUncertain          bool
	CompanionMode             string
	LinkTier                  string
	LinkLost                  bool
	CurrentLinkHbUnconfirmed  bool
	TeleopUncertain           bool
	TeleopUncertainReason     string
	UncertainAuthority        string
	UncertainLinkTier         string
	UncertainArmed            bool
	HeartbeatObservedUnixNs   int64
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

type hbStateKey struct {
	domain        string
	armed         bool
	authority     string
	companionMode string
	linkTier      string
	flags         uxsvocab.HealthFlags
}

func hbStateKeyOf(snap uxsvocab.VehicleSnapshot) hbStateKey {
	return hbStateKey{
		domain:        snap.Domain,
		armed:         snap.Armed,
		authority:     snap.Authority,
		companionMode: snap.CompanionMode,
		linkTier:      snap.LinkTier,
		flags:         snap.Flags,
	}
}

type hbStateEntry struct {
	state      hbStateKey
	firstOrder uint64
	lastOrder  uint64
}

type hbFrameSlot struct {
	active      bool
	packetOrder uint64
	entries     []hbStateEntry
}

type linkHbHistoryEntry struct {
	state         hbStateKey
	priorLinkLife uint64
	curLinkLife   uint64
	curLinkOrder  uint64
	curLinkEpoch  uint64
}

const (
	maxHbSlotStates = 16
	// maxHistoricalHbReorderPackets bounds how long a previously confirmed (seq, state)
	// pair can hold a matching post-wrap heartbeat as a suspected delayed UDP copy (two
	// full 256-packet MAVLink v2 sequence cycles).
	maxHistoricalHbReorderPackets = 512
)

type authorityObsRecord struct {
	rev   uint64
	epoch uint64
	obsMs int64
}

const (
	maxTrackedObsMs   = 4096
	ambiguousObsEpoch = ^uint64(0)
)

type vehicleSession struct {
	snap                            uxsvocab.VehicleSnapshot
	rev                             uint64
	authorityEpoch                  uint64
	obsRev                          uint64
	heartbeatSeq                    uint64
	hbReceiptSeq                    uint64
	snapHbReceiptSeq                uint64
	lastHbMonoAt                    time.Time
	lastHbOrder                     uint64
	snapHbOrder                     uint64
	currentLinkHbConfirmed          bool
	teleopUncertainReason           string
	uncertainAuthority              string
	uncertainLinkTier               string
	uncertainArmed                  bool
	uncertainMinOrder               uint64
	pendingNonSITLLinkTier          string
	pendingNonSITLMinOrder          uint64
	obsHistory                      [256]authorityObsRecord
	obsIdx                          int
	obsMsEpoch                      map[int64]uint64
	obsMsOrder                      [maxTrackedObsMs]int64
	obsMsValid                      [maxTrackedObsMs]bool
	obsMsIdx                        int
	linkEpoch                       uint64
	lifetimePacketPos               uint64
	hasLifeSeq                      bool
	lastLifeSeq                     uint8
	hasLifePendingGap               bool
	lifePendingGapSeq               uint8
	hasSeqStream                    bool
	lastStreamSeq                   uint8
	seqStreamPos                    uint64
	hasPendingGap                   bool
	pendingGapSeq                   uint8
	pendingGapOrder                 uint64
	pendingGapHasAck                bool
	pendingGapAck                   mavcodec.CommandAck
	pendingGapHasHb                 bool
	pendingGapHbApplied             bool
	pendingGapPriorRestrictionOrder uint64
	pendingGapHbReceiptSeq          uint64
	pendingGapHbVh                  mavcodec.VehicleHeartbeat
	pendingGapHbWall                time.Time
	pendingGapHbMono                time.Time
	lastAckOrder                    uint64
	lastAckSeq                      uint64
	lastAckUncertain                bool
	seenAckFrames                   [256]ackFrameSlot
	seenHbFrames                    [256]hbFrameSlot
	seenLinkHbHistory               [256][]linkHbHistoryEntry
	seenSeqOrder                    [256]uint64
	lastPacketAt                    time.Time
	lastSeq                         uint8
	lastMsgID                       uint32
	linkLost                        bool
	hasSysStatus                    bool
	hasAttitude                     bool
	hasLocalPos                     bool
	hasGlobalPos                    bool
	hasHeading                      bool
	peerAddr                        netip.AddrPort
}

func isPreemptAuthority(auth string) bool {
	return auth == uxsvocab.AuthorityDirectCRSFOverride || auth == uxsvocab.AuthorityMCUFailsafe
}

func nonTeleopUncertainReason(next uxsvocab.VehicleSnapshot) string {
	if isPreemptAuthority(next.Authority) {
		return "authority_preempted"
	}
	if next.LinkTier == uxsvocab.LinkTierMeshSupervisory {
		return "supervisory_link_only"
	}
	if !next.Armed {
		return "vehicle_disarmed"
	}
	if next.Authority != uxsvocab.AuthorityCompanionTeleop {
		return "authority_not_teleop"
	}
	return ""
}

func (s *vehicleSession) recordHbSlot(seq uint8, hbOrder uint64, state hbStateKey) {
	slot := &s.seenHbFrames[seq]
	slot.active = true
	if hbOrder > slot.packetOrder {
		slot.packetOrder = hbOrder
	}
	for i := range slot.entries {
		if slot.entries[i].state == state {
			if hbOrder < slot.entries[i].firstOrder {
				slot.entries[i].firstOrder = hbOrder
			}
			if hbOrder > slot.entries[i].lastOrder {
				slot.entries[i].lastOrder = hbOrder
			}
			return
		}
	}
	if len(slot.entries) >= maxHbSlotStates {
		oldestIdx := 0
		for i := 1; i < len(slot.entries); i++ {
			if slot.entries[i].lastOrder < slot.entries[oldestIdx].lastOrder {
				oldestIdx = i
			}
		}
		slot.entries[oldestIdx] = hbStateEntry{
			state:      state,
			firstOrder: hbOrder,
			lastOrder:  hbOrder,
		}
		return
	}
	slot.entries = append(slot.entries, hbStateEntry{
		state:      state,
		firstOrder: hbOrder,
		lastOrder:  hbOrder,
	})
}

func (s *vehicleSession) wasHbStateSeenInPriorCycle(seq uint8, hbOrder uint64, state hbStateKey) bool {
	slot := &s.seenHbFrames[seq]
	if !slot.active {
		return false
	}
	for i := range slot.entries {
		if slot.entries[i].state == state &&
			hbOrder >= slot.entries[i].firstOrder+256 &&
			hbOrder <= slot.entries[i].lastOrder+maxHistoricalHbReorderPackets {
			return true
		}
	}
	return false
}

func (s *vehicleSession) projectedLifetimePos(seq uint8) uint64 {
	pos := s.lifetimePacketPos
	if s.linkLost || !s.hasLifeSeq {
		if pos == 0 || seq != s.lastSeq {
			return pos + 1
		}
		return pos
	}
	if s.hasLifePendingGap {
		if seq == s.lifePendingGapSeq {
			if pos == 0 {
				return 1
			}
			return pos
		}
		gapStep := uint8(seq - s.lifePendingGapSeq)
		if gapStep >= 1 && gapStep <= 64 {
			return pos + 2
		}
	}
	delta := uint8(seq - s.lastLifeSeq)
	if delta >= 1 && delta <= 64 {
		return pos + 1
	}
	if pos == 0 {
		return 1
	}
	return pos
}

func (s *vehicleSession) advanceLifetimeSeq(seq uint8) {
	if !s.hasLifeSeq {
		s.hasLifeSeq = true
		s.lastLifeSeq = seq
		s.hasLifePendingGap = false
		if s.lifetimePacketPos == 0 || seq != s.lastSeq {
			s.lifetimePacketPos++
		}
		return
	}
	if s.hasLifePendingGap {
		if seq == s.lifePendingGapSeq {
			return
		}
		gapStep := uint8(seq - s.lifePendingGapSeq)
		if gapStep >= 1 && gapStep <= 64 {
			s.hasLifePendingGap = false
			s.lastLifeSeq = seq
			s.lifetimePacketPos += 2
			return
		}
		s.hasLifePendingGap = false
	}
	delta := uint8(seq - s.lastLifeSeq)
	if delta == 0 {
		return
	}
	if delta <= 64 {
		s.lastLifeSeq = seq
		s.lifetimePacketPos++
		return
	}
	if delta <= 255-maxBackwardTelemetryReorder {
		s.hasLifePendingGap = true
		s.lifePendingGapSeq = seq
		return
	}
}

const hbHistoryCycleOffset = 256

func (s *vehicleSession) recordLinkHbHistory(seq uint8, state hbStateKey) {
	s.recordLinkHbHistoryAtOrder(seq, s.seenSeqOrder[seq], state)
}

func (s *vehicleSession) recordLinkHbHistoryAtOrder(seq uint8, streamOrder uint64, state hbStateKey) {
	cycleOrder := s.unconfirmedHbCycleOrder(seq)
	if streamOrder > 0 {
		cycleOrder = hbHistoryCycleOffset + streamOrder
	}
	s.recordLinkHbHistoryAtCycleOrder(seq, cycleOrder, s.lifetimePacketPos, state)
}

func (s *vehicleSession) recordLinkHbHistoryAtCycleOrder(seq uint8, cycleOrder, pos uint64, state hbStateKey) {
	if pos == 0 {
		pos = 1
	}
	entries := s.seenLinkHbHistory[seq]
	for i := range entries {
		if entries[i].state == state {
			if entries[i].curLinkEpoch < s.linkEpoch {
				entries[i].priorLinkLife = entries[i].curLinkLife
				entries[i].curLinkEpoch = s.linkEpoch
				entries[i].curLinkLife = 0
				entries[i].curLinkOrder = 0
			}
			if pos > entries[i].curLinkLife {
				entries[i].curLinkLife = pos
			}
			if cycleOrder > entries[i].curLinkOrder {
				entries[i].curLinkOrder = cycleOrder
			}
			return
		}
	}
	if len(entries) >= maxHbSlotStates {
		oldestIdx := 0
		oldestLife := max(entries[0].priorLinkLife, entries[0].curLinkLife)
		for i := 1; i < len(entries); i++ {
			candLife := max(entries[i].priorLinkLife, entries[i].curLinkLife)
			if candLife < oldestLife {
				oldestIdx = i
				oldestLife = candLife
			}
		}
		entries[oldestIdx] = linkHbHistoryEntry{
			state:        state,
			curLinkLife:  pos,
			curLinkOrder: cycleOrder,
			curLinkEpoch: s.linkEpoch,
		}
		return
	}
	s.seenLinkHbHistory[seq] = append(entries, linkHbHistoryEntry{
		state:        state,
		curLinkLife:  pos,
		curLinkOrder: cycleOrder,
		curLinkEpoch: s.linkEpoch,
	})
}

func (s *vehicleSession) unconfirmedHbCycleOrder(seq uint8) uint64 {
	if !s.hasSeqStream {
		return hbHistoryCycleOffset + 1
	}
	headOrder := hbHistoryCycleOffset + s.seqStreamPos
	delta := uint8(seq - s.lastStreamSeq)
	if delta == 0 {
		return headOrder
	}
	if delta <= 64 {
		return headOrder + uint64(delta)
	}
	back := uint64(uint8(s.lastStreamSeq - seq))
	return headOrder - back
}

func (s *vehicleSession) recordUnconfirmedLinkHbObservation(seq uint8, peer netip.AddrPort, state hbStateKey, candPos uint64) {
	if candPos == 0 {
		candPos = s.lifetimePacketPos
		if candPos == 0 {
			candPos = 1
		}
	}
	if !s.currentLinkHbConfirmed && s.wasHbStateSeenBeforeCurrentLinkAt(seq, peer, state, candPos) {
		return
	}
	obsCycleOrder := s.unconfirmedHbCycleOrder(seq)
	entries := s.seenLinkHbHistory[seq]
	for i := range entries {
		if entries[i].state != state || entries[i].curLinkEpoch != s.linkEpoch {
			continue
		}
		if candPos <= entries[i].curLinkLife+maxHistoricalHbReorderPackets &&
			obsCycleOrder < entries[i].curLinkOrder+256 {
			return
		}
		break
	}
	s.recordLinkHbHistoryAtCycleOrder(seq, obsCycleOrder, candPos, state)
}

func (s *vehicleSession) wasHbStateSeenBeforeCurrentLinkAt(seq uint8, peer netip.AddrPort, state hbStateKey, candPos uint64) bool {
	normPeer := normalizeAddrPort(peer)
	if !matchesPinnedPeer(s, normPeer) {
		return false
	}
	entries := s.seenLinkHbHistory[seq]
	for i := range entries {
		if entries[i].state != state {
			continue
		}
		priorLife := entries[i].priorLinkLife
		if entries[i].curLinkEpoch > 0 && entries[i].curLinkEpoch < s.linkEpoch && entries[i].curLinkLife > priorLife {
			priorLife = entries[i].curLinkLife
		}
		if priorLife == 0 {
			return false
		}
		if candPos == 0 {
			candPos = 1
		}
		return candPos <= priorLife+maxHistoricalHbReorderPackets
	}
	return false
}

func (s *vehicleSession) shouldHoldPreLossHeartbeatAt(seq uint8, peer netip.AddrPort, prev, next uxsvocab.VehicleSnapshot, candPos uint64) bool {
	nextKey := hbStateKeyOf(next)
	if !s.wasHbStateSeenBeforeCurrentLinkAt(seq, peer, nextKey, candPos) {
		return false
	}
	if s.currentLinkHbConfirmed &&
		s.teleopUncertainReason == "" &&
		s.pendingNonSITLLinkTier == "" &&
		hbStateKeyOf(prev) == nextKey &&
		s.snapHbOrder > 0 {
		return false
	}
	return true
}

func (s *vehicleSession) clearPendingGap() {
	s.hasPendingGap = false
	s.pendingGapOrder = 0
	s.pendingGapHasAck = false
	s.pendingGapHasHb = false
	s.pendingGapHbApplied = false
	s.pendingGapPriorRestrictionOrder = 0
	s.pendingGapHbReceiptSeq = 0
	s.pendingGapHbVh = mavcodec.VehicleHeartbeat{}
	s.pendingGapHbWall = time.Time{}
	s.pendingGapHbMono = time.Time{}
}

func (s *vehicleSession) setTeleopUncertain(reason string, next uxsvocab.VehicleSnapshot, minOrder uint64) {
	if minOrder == 0 {
		minOrder = s.seqStreamPos
	}
	if s.teleopUncertainReason == "" || minOrder >= s.uncertainMinOrder {
		s.teleopUncertainReason = reason
		s.uncertainAuthority = next.Authority
		s.uncertainLinkTier = next.LinkTier
		s.uncertainArmed = next.Armed
	}
	if minOrder > s.uncertainMinOrder {
		s.uncertainMinOrder = minOrder
	}
}

func (s *vehicleSession) clearTeleopUncertain() {
	s.teleopUncertainReason = ""
	s.uncertainAuthority = ""
	s.uncertainLinkTier = ""
	s.uncertainArmed = false
	s.uncertainMinOrder = 0
}

func (s *vehicleSession) setPendingNonSITLLinkTier(linkTier string, minOrder uint64) {
	if linkTier == "" || linkTier == uxsvocab.LinkTierSITLLocal {
		return
	}
	if minOrder == 0 {
		minOrder = s.seqStreamPos
	}
	if s.pendingNonSITLLinkTier == "" || minOrder >= s.pendingNonSITLMinOrder {
		s.pendingNonSITLLinkTier = linkTier
	}
	if minOrder > s.pendingNonSITLMinOrder {
		s.pendingNonSITLMinOrder = minOrder
	}
}

func (s *vehicleSession) clearPendingNonSITLLinkTier() {
	s.pendingNonSITLLinkTier = ""
	s.pendingNonSITLMinOrder = 0
}

func (s *vehicleSession) activeRestrictionMinOrder() uint64 {
	if s.pendingNonSITLMinOrder > s.uncertainMinOrder {
		return s.pendingNonSITLMinOrder
	}
	return s.uncertainMinOrder
}

func (s *vehicleSession) shouldHoldWrapHeartbeat(seq uint8, hbOrder uint64, prev, next uxsvocab.VehicleSnapshot) bool {
	curKey := hbStateKeyOf(prev)
	nextKey := hbStateKeyOf(next)
	if !s.wasHbStateSeenInPriorCycle(seq, hbOrder, nextKey) {
		return false
	}
	if s.teleopUncertainReason == "" && s.pendingNonSITLLinkTier == "" && curKey == nextKey && s.snapHbOrder > 0 {
		return false
	}
	return true
}

func normalizeAddrPort(peer netip.AddrPort) netip.AddrPort {
	if !peer.IsValid() {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port())
}

const maxBackwardTelemetryReorder = 8

func (s *vehicleSession) isUnconfirmedGapDelta(seq, delta uint8, isHeartbeat bool) bool {
	if delta >= 65 && delta <= 255-maxBackwardTelemetryReorder {
		return true
	}
	if delta > 255-maxBackwardTelemetryReorder && delta <= 254 {
		back := uint64(256 - uint16(delta))
		if s.seqStreamPos <= back || s.seenSeqOrder[seq] == s.seqStreamPos-back {
			return true
		}
		if isHeartbeat && s.seqStreamPos-back <= s.lastHbOrder {
			return true
		}
	}
	return false
}

func (s *vehicleSession) evaluateHeartbeatSeq(seq uint8) (hbOrder uint64, confirmedOrder, staleOrDup bool) {
	if !s.hasSeqStream {
		return 1, true, false
	}
	prevHb := s.seenHbFrames[seq]
	dupHbAt := func(pos uint64) bool {
		return prevHb.active && (pos <= prevHb.packetOrder || pos-prevHb.packetOrder < 256)
	}
	if s.hasPendingGap && seq == s.pendingGapSeq {
		if s.pendingGapHasHb {
			return 0, false, true
		}
		pos := s.pendingGapOrder
		if dupHbAt(pos) {
			return pos, false, true
		}
		return pos, false, false
	}
	delta := uint8(seq - s.lastStreamSeq)
	if delta == 0 {
		pos := s.seqStreamPos
		if dupHbAt(pos) || pos < s.lastHbOrder {
			return pos, false, true
		}
		return pos, true, false
	}
	if delta <= 64 {
		pos := s.seqStreamPos + uint64(delta)
		if dupHbAt(pos) || pos <= s.lastHbOrder {
			return pos, false, true
		}
		return pos, true, false
	}
	if s.isUnconfirmedGapDelta(seq, delta, true) {
		pos := s.seqStreamPos + uint64(delta)
		if dupHbAt(pos) {
			return pos, false, true
		}
		return pos, false, false
	}
	back := uint64(uint8(s.lastStreamSeq - seq))
	if s.seqStreamPos <= back {
		return 0, false, true
	}
	pos := s.seqStreamPos - back
	if s.seenSeqOrder[seq] == pos || dupHbAt(pos) || pos <= s.lastHbOrder {
		return pos, false, true
	}
	return pos, true, false
}

func (s *vehicleSession) advanceTelemetrySeq(seq uint8, isHeartbeat bool) {
	if !s.hasSeqStream {
		s.hasSeqStream = true
		s.seqStreamPos = 1
		s.lastStreamSeq = seq
		s.seenSeqOrder[seq] = 1
		s.clearPendingGap()
		return
	}
	delta := uint8(seq - s.lastStreamSeq)
	if s.hasPendingGap && seq != s.pendingGapSeq {
		s.clearPendingGap()
	}
	if delta == 0 {
		return
	}
	if delta <= 64 {
		s.seqStreamPos += uint64(delta)
		s.lastStreamSeq = seq
		s.seenSeqOrder[seq] = s.seqStreamPos
		return
	}
	if s.isUnconfirmedGapDelta(seq, delta, isHeartbeat) {
		s.hasPendingGap = true
		s.pendingGapSeq = seq
		s.pendingGapOrder = s.seqStreamPos + uint64(delta)
		s.pendingGapHasAck = false
		s.pendingGapHasHb = false
		s.pendingGapHbApplied = false
		s.pendingGapPriorRestrictionOrder = 0
		s.pendingGapHbReceiptSeq = 0
		s.pendingGapHbVh = mavcodec.VehicleHeartbeat{}
		s.pendingGapHbWall = time.Time{}
		s.pendingGapHbMono = time.Time{}
		return
	}
	back := uint64(uint8(s.lastStreamSeq - seq))
	if s.seqStreamPos > back && s.seenSeqOrder[seq] == 0 {
		s.seenSeqOrder[seq] = s.seqStreamPos - back
	}
}

func (s *vehicleSession) evaluateCommandAckSeq(seq uint8, msg mavcodec.CommandAck) (candPos uint64, advanceHead, setGap, recordAckSlot, isDup, uncertain bool) {
	prevSlot := s.seenAckFrames[seq]
	if !s.hasSeqStream {
		return 1, true, false, true, false, false
	}
	if s.hasPendingGap && seq == s.pendingGapSeq {
		fwdPos := s.pendingGapOrder
		if s.pendingGapHasAck && s.pendingGapAck == msg {
			return fwdPos, false, false, false, true, false
		}
		if prevSlot.active && prevSlot.ack == msg {
			if fwdPos <= prevSlot.packetOrder || fwdPos-prevSlot.packetOrder < 256 {
				return fwdPos, false, false, false, true, false
			}
			return fwdPos, false, true, false, false, true
		}
		return fwdPos, false, true, false, false, false
	}
	delta := uint8(seq - s.lastStreamSeq)
	if delta == 0 {
		pos := s.seqStreamPos
		dup := prevSlot.active && prevSlot.ack == msg &&
			(pos <= prevSlot.packetOrder || pos-prevSlot.packetOrder < 256)
		return pos, false, false, true, dup, false
	}
	if delta <= 64 {
		pos := s.seqStreamPos + uint64(delta)
		dup := prevSlot.active && prevSlot.ack == msg &&
			(pos <= prevSlot.packetOrder || pos-prevSlot.packetOrder < 256)
		return pos, true, false, true, dup, false
	}
	if delta <= 128 {
		fwdPos := s.seqStreamPos + uint64(delta)
		if prevSlot.active && prevSlot.ack == msg {
			if fwdPos <= prevSlot.packetOrder || fwdPos-prevSlot.packetOrder < 256 {
				return fwdPos, false, false, false, true, false
			}
			return fwdPos, false, true, false, false, true
		}
		return fwdPos, false, true, false, false, false
	}
	back := uint64(uint8(s.lastStreamSeq - seq))
	pos := uint64(1)
	if s.seqStreamPos > back {
		pos = s.seqStreamPos - back
	}
	dup := prevSlot.active && prevSlot.ack == msg &&
		(pos <= prevSlot.packetOrder || pos-prevSlot.packetOrder < 256)
	return pos, false, false, true, dup, false
}

func (s *vehicleSession) recordAuthorityObservation(wallUTC time.Time) {
	ms := wallUTC.UnixMilli()
	s.obsRev++
	s.obsHistory[s.obsIdx] = authorityObsRecord{
		rev:   s.obsRev,
		epoch: s.authorityEpoch,
		obsMs: ms,
	}
	s.obsIdx = (s.obsIdx + 1) % len(s.obsHistory)
	if s.obsMsEpoch == nil {
		s.obsMsEpoch = make(map[int64]uint64, 64)
	}
	if prevEpoch, seen := s.obsMsEpoch[ms]; !seen {
		if s.obsMsValid[s.obsMsIdx] {
			oldMs := s.obsMsOrder[s.obsMsIdx]
			if s.obsMsEpoch[oldMs] != ambiguousObsEpoch {
				delete(s.obsMsEpoch, oldMs)
			}
		}
		s.obsMsOrder[s.obsMsIdx] = ms
		s.obsMsValid[s.obsMsIdx] = true
		s.obsMsIdx = (s.obsMsIdx + 1) % len(s.obsMsOrder)
		s.obsMsEpoch[ms] = s.authorityEpoch
	} else if prevEpoch != s.authorityEpoch {
		s.obsMsEpoch[ms] = ambiguousObsEpoch
	}
}

func (s *vehicleSession) recordAcceptedPacket(seq uint8, msgID uint32, peer netip.AddrPort, monoSample time.Time) {
	normPeer := normalizeAddrPort(peer)
	wasLost := s.linkLost
	if wasLost {
		if normPeer.IsValid() && s.peerAddr.IsValid() && normPeer != s.peerAddr {
			s.seenLinkHbHistory = [256][]linkHbHistoryEntry{}
		}
		s.authorityEpoch++
		s.linkEpoch++
		s.lastHbMonoAt = time.Time{}
		s.lastHbOrder = 0
		s.snapHbOrder = 0
		s.snapHbReceiptSeq = 0
		s.currentLinkHbConfirmed = false
		s.clearTeleopUncertain()
		s.clearPendingNonSITLLinkTier()
		s.hasLifeSeq = false
		s.hasLifePendingGap = false
		s.hasSeqStream = false
		s.seqStreamPos = 0
		s.clearPendingGap()
		s.lastAckOrder = 0
		s.lastAckSeq = 0
		s.lastAckUncertain = false
		s.seenAckFrames = [256]ackFrameSlot{}
		s.seenHbFrames = [256]hbFrameSlot{}
		s.seenSeqOrder = [256]uint64{}
	}
	if msgID != mavcodec.MsgIDCommandAck {
		s.advanceLifetimeSeq(seq)
		s.advanceTelemetrySeq(seq, msgID == mavcodec.MsgIDHeartbeat)
	} else if wasLost {
		s.lifetimePacketPos++
	}
	if s.lastPacketAt.IsZero() || !monoSample.Before(s.lastPacketAt) {
		s.lastPacketAt = monoSample
	}
	s.lastSeq = seq
	s.lastMsgID = msgID
	s.linkLost = false
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

func newLinkTransitionEvent(snap uxsvocab.VehicleSnapshot, incarnation string, authorityEpoch, obsRev uint64, seq uint8, msgID uint32, kind, reason string, ts time.Time) TransitionEvent {
	return TransitionEvent{
		SysID:                snap.SysID,
		EntityID:             snap.EntityID(),
		Domain:               snap.Domain,
		Seq:                  seq,
		MsgID:                msgID,
		Kind:                 kind,
		Reasons:              []string{reason},
		Armed:                snap.Armed,
		Authority:            snap.Authority,
		AuthorityIncarnation: incarnation,
		AuthorityEpoch:       authorityEpoch,
		ObservationRev:       obsRev,
		CompanionMode:        snap.CompanionMode,
		LinkTier:             snap.LinkTier,
		Flags:                snap.Flags,
		Failsafes:            snap.Failsafes,
		Timestamp:            ts.UnixMilli(),
	}
}

// Tracker maintains per-sysid (1..=254, compid == 1) vehicle state, pinned peer
// UDP endpoints (`OQ1 Option A`), link-loss freshness, packet sanitization,
// `#77 COMMAND_ACK` correlation, and 3-lane split decisions.
type Tracker struct {
	org                string
	platform           string
	incarnation        string
	linkTimeout        time.Duration
	supervisoryTimeout time.Duration
	allowedPeers       []netip.Prefix

	mu       sync.RWMutex
	sessions map[uint8]*vehicleSession
}

// Incarnation returns the unique per-instance tracker incarnation identifier.
func (t *Tracker) Incarnation() string {
	return t.incarnation
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
		incarnation:        newTrackerIncarnation(),
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

// IngestFrameWithClock processes an already-decoded MAVLink v2 Frame using wallUTC
// for published UTC timestamps and monoSample for monotonic silence intervals.
func (t *Tracker) IngestFrameWithClock(frame mavcodec.Frame, peer netip.AddrPort, wallUTC, monoSample time.Time) IngestOutcome {
	return t.ingestFrameAt(frame, peer, observedUTC(wallUTC), monoOrEpoch(monoSample))
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

func applyHeartbeatFields(base uxsvocab.VehicleSnapshot, vh mavcodec.VehicleHeartbeat, wallUTC time.Time) (uxsvocab.VehicleSnapshot, bool) {
	domainStr, err := vh.Custom.Domain.VocabString()
	if err != nil {
		return uxsvocab.VehicleSnapshot{}, false
	}
	authStr, err := vh.Custom.ControlAuthority.VocabString()
	if err != nil {
		return uxsvocab.VehicleSnapshot{}, false
	}
	modeStr, err := vh.Custom.CompanionMode.VocabString()
	if err != nil {
		return uxsvocab.VehicleSnapshot{}, false
	}
	tierStr, err := vh.Custom.LinkTier.VocabString()
	if err != nil {
		return uxsvocab.VehicleSnapshot{}, false
	}
	next := base
	next.Domain = domainStr
	next.Armed = vh.Armed
	next.Authority = authStr
	next.CompanionMode = modeStr
	next.LinkTier = tierStr
	next.Flags = vh.Custom.HealthFlags
	next.HeartbeatObservedUnixNs = unixNanoOrZero(wallUTC)
	next.ObservedAt = wallUTC
	if err := next.Validate(); err != nil {
		return uxsvocab.VehicleSnapshot{}, false
	}
	return next, true
}

func (t *Tracker) commitConfirmedHeartbeatLocked(
	sess *vehicleSession,
	sysid, seq uint8,
	prev, next uxsvocab.VehicleSnapshot,
	hbOrder uint64,
	receiptSeq uint64,
	wasLost bool,
	wallUTC, monoSample time.Time,
) ([]TransitionEvent, bool) {
	sess.currentLinkHbConfirmed = true
	sess.clearTeleopUncertain()
	sess.clearPendingNonSITLLinkTier()
	// shortcut: edge transition alerts evaluated in internal/mavlink, migrate declarative alert rules to processor/rule when semengine alpha.9 lands
	reasons := diffHeartbeatReasons(prev, next)

	sess.snap = next
	sess.rev++
	sess.heartbeatSeq++
	if !wasLost && (prev.Authority != next.Authority || prev.Armed != next.Armed || prev.LinkTier != next.LinkTier) {
		sess.authorityEpoch++
	}
	if sess.lastHbMonoAt.IsZero() || !monoSample.Before(sess.lastHbMonoAt) || wasLost {
		sess.lastHbMonoAt = monoSample
	}
	if wasLost {
		hbOrder = 1
		sess.lastHbOrder = 1
	} else if hbOrder > sess.lastHbOrder {
		sess.lastHbOrder = hbOrder
	}
	sess.snapHbOrder = hbOrder
	sess.snapHbReceiptSeq = receiptSeq
	sess.seenSeqOrder[seq] = hbOrder
	nextKey := hbStateKeyOf(next)
	sess.recordHbSlot(seq, hbOrder, nextKey)
	sess.recordLinkHbHistory(seq, nextKey)
	sess.recordAuthorityObservation(wallUTC)

	var events []TransitionEvent
	urgent := false
	if wasLost {
		events = append(events, newLinkTransitionEvent(next, t.incarnation, sess.authorityEpoch, sess.obsRev, seq, mavcodec.MsgIDHeartbeat, EventKindLinkRestored, "link_restored", wallUTC))
		urgent = true
	}
	if len(reasons) > 0 {
		prevAuth := ""
		if prev.Authority != next.Authority {
			prevAuth = prev.Authority
		}
		events = append(events, TransitionEvent{
			SysID:                sysid,
			EntityID:             next.EntityID(),
			Domain:               next.Domain,
			Seq:                  seq,
			MsgID:                mavcodec.MsgIDHeartbeat,
			Kind:                 EventKindHeartbeatEdge,
			Reasons:              reasons,
			Armed:                next.Armed,
			Authority:            next.Authority,
			PrevAuthority:        prevAuth,
			AuthorityIncarnation: t.incarnation,
			AuthorityEpoch:       sess.authorityEpoch,
			ObservationRev:       sess.obsRev,
			CompanionMode:        next.CompanionMode,
			LinkTier:             next.LinkTier,
			Flags:                next.Flags,
			Failsafes:            next.Failsafes,
			Timestamp:            wallUTC.UnixMilli(),
		})
		urgent = true
	}
	return events, urgent
}

func (t *Tracker) applyUncorroboratedPreemptLocked(
	sess *vehicleSession,
	sysid, seq uint8,
	prev, next uxsvocab.VehicleSnapshot,
	hbOrder uint64,
	receiptSeq uint64,
	wasLost bool,
	wallUTC, monoSample time.Time,
) ([]TransitionEvent, bool) {
	if isPreemptAuthority(prev.Authority) &&
		(sess.teleopUncertainReason != "" || sess.pendingNonSITLLinkTier != "") &&
		hbOrder < sess.activeRestrictionMinOrder() {
		if next.LinkTier != uxsvocab.LinkTierSITLLocal {
			sess.setPendingNonSITLLinkTier(next.LinkTier, hbOrder)
		}
		if next.Armed {
			sess.uncertainArmed = true
		}
		return nil, false
	}
	effectiveNext := next
	effectiveNext.LinkTier = prev.LinkTier
	if prev.Armed {
		effectiveNext.Armed = true
	}
	if next.LinkTier != uxsvocab.LinkTierSITLLocal {
		sess.setPendingNonSITLLinkTier(next.LinkTier, hbOrder)
	}
	sess.setTeleopUncertain("authority_preempted", effectiveNext, hbOrder)

	reasons := diffHeartbeatReasons(prev, effectiveNext)
	sess.snap = effectiveNext
	sess.rev++
	sess.heartbeatSeq++
	if !wasLost && (prev.Authority != effectiveNext.Authority || prev.Armed != effectiveNext.Armed || prev.LinkTier != effectiveNext.LinkTier) {
		sess.authorityEpoch++
	}
	if sess.lastHbMonoAt.IsZero() || !monoSample.Before(sess.lastHbMonoAt) || wasLost {
		sess.lastHbMonoAt = monoSample
	}
	sess.snapHbOrder = 0
	sess.snapHbReceiptSeq = receiptSeq
	sess.recordAuthorityObservation(wallUTC)
	if len(reasons) == 0 {
		return nil, false
	}
	prevAuth := ""
	if prev.Authority != effectiveNext.Authority {
		prevAuth = prev.Authority
	}
	return []TransitionEvent{{
		SysID:                sysid,
		EntityID:             effectiveNext.EntityID(),
		Domain:               effectiveNext.Domain,
		Seq:                  seq,
		MsgID:                mavcodec.MsgIDHeartbeat,
		Kind:                 EventKindHeartbeatEdge,
		Reasons:              reasons,
		Armed:                effectiveNext.Armed,
		Authority:            effectiveNext.Authority,
		PrevAuthority:        prevAuth,
		AuthorityIncarnation: t.incarnation,
		AuthorityEpoch:       sess.authorityEpoch,
		ObservationRev:       sess.obsRev,
		CompanionMode:        effectiveNext.CompanionMode,
		LinkTier:             effectiveNext.LinkTier,
		Flags:                effectiveNext.Flags,
		Failsafes:            effectiveNext.Failsafes,
		Timestamp:            wallUTC.UnixMilli(),
	}}, true
}

func (t *Tracker) resolvePendingGapBeforePacketLocked(sess *vehicleSession, sysid, seq uint8) ([]TransitionEvent, bool) {
	if sess.linkLost || !sess.hasSeqStream || !sess.hasPendingGap {
		return nil, false
	}
	if seq == sess.pendingGapSeq {
		return nil, false
	}
	normalDelta := uint8(seq - sess.lastStreamSeq)
	if normalDelta >= 1 && normalDelta <= 64 {
		sess.clearPendingGap()
		return nil, false
	}
	gapStep := uint8(seq - sess.pendingGapSeq)
	if gapStep >= 1 && gapStep <= 64 {
		gapSeq := sess.pendingGapSeq
		gapOrder := sess.pendingGapOrder
		hadAck := sess.pendingGapHasAck
		gapAck := sess.pendingGapAck
		hadHb := sess.pendingGapHasHb
		hadHbApplied := sess.pendingGapHbApplied
		priorRestrictionOrder := sess.pendingGapPriorRestrictionOrder
		gapHbReceiptSeq := sess.pendingGapHbReceiptSeq
		gapHbVh := sess.pendingGapHbVh
		gapHbWall := sess.pendingGapHbWall
		gapHbMono := sess.pendingGapHbMono
		sess.clearPendingGap()

		sess.seenSeqOrder[gapSeq] = gapOrder
		if hadAck {
			sess.seenAckFrames[gapSeq] = ackFrameSlot{
				active:      true,
				ack:         gapAck,
				packetOrder: gapOrder,
			}
		}
		sess.seqStreamPos = gapOrder
		sess.lastStreamSeq = gapSeq
		if hadHb {
			gapCandLife := sess.lifetimePacketPos + 1
			gapRestricted := gapOrder <= sess.lastHbOrder ||
				gapOrder <= priorRestrictionOrder ||
				gapOrder < sess.activeRestrictionMinOrder()
			if hadHbApplied {
				if appliedNext, ok := applyHeartbeatFields(sess.snap, gapHbVh, gapHbWall); ok {
					appliedKey := hbStateKeyOf(appliedNext)
					if sess.currentLinkHbConfirmed || !sess.wasHbStateSeenBeforeCurrentLinkAt(gapSeq, sess.peerAddr, appliedKey, gapCandLife) {
						sess.recordLinkHbHistoryAtOrder(gapSeq, gapOrder, appliedKey)
					}
					if !gapRestricted &&
						!sess.wasHbStateSeenInPriorCycle(gapSeq, gapOrder, appliedKey) &&
						!sess.shouldHoldPreLossHeartbeatAt(gapSeq, sess.peerAddr, sess.snap, appliedNext, gapCandLife) {
						if diff := diffHeartbeatReasons(sess.snap, appliedNext); len(diff) > 0 {
							return t.commitConfirmedHeartbeatLocked(sess, sysid, gapSeq, sess.snap, appliedNext, gapOrder, gapHbReceiptSeq, false, gapHbWall, gapHbMono)
						}
						sess.currentLinkHbConfirmed = true
						if gapOrder > sess.lastHbOrder {
							sess.lastHbOrder = gapOrder
						}
						sess.snapHbOrder = gapOrder
						sess.snapHbReceiptSeq = gapHbReceiptSeq
						sess.recordHbSlot(gapSeq, gapOrder, appliedKey)
						sess.recordLinkHbHistory(gapSeq, appliedKey)
						sess.clearTeleopUncertain()
						sess.clearPendingNonSITLLinkTier()
					}
				}
				return nil, false
			}
			prev := sess.snap
			next, ok := applyHeartbeatFields(prev, gapHbVh, gapHbWall)
			if ok {
				nextKey := hbStateKeyOf(next)
				if sess.currentLinkHbConfirmed || !sess.wasHbStateSeenBeforeCurrentLinkAt(gapSeq, sess.peerAddr, nextKey, gapCandLife) {
					sess.recordLinkHbHistoryAtOrder(gapSeq, gapOrder, nextKey)
				}
				if gapRestricted {
					if isPreemptAuthority(next.Authority) {
						return t.applyUncorroboratedPreemptLocked(sess, sysid, gapSeq, prev, next, gapOrder, gapHbReceiptSeq, false, gapHbWall, gapHbMono)
					}
					if next.LinkTier != uxsvocab.LinkTierSITLLocal {
						sess.setPendingNonSITLLinkTier(next.LinkTier, gapOrder)
					}
					if reason := nonTeleopUncertainReason(next); reason != "" {
						sess.setTeleopUncertain(reason, next, gapOrder)
					}
					return nil, false
				}
				if !sess.shouldHoldWrapHeartbeat(gapSeq, gapOrder, prev, next) &&
					!sess.shouldHoldPreLossHeartbeatAt(gapSeq, sess.peerAddr, prev, next, gapCandLife) {
					return t.commitConfirmedHeartbeatLocked(sess, sysid, gapSeq, prev, next, gapOrder, gapHbReceiptSeq, false, gapHbWall, gapHbMono)
				}
				if isPreemptAuthority(next.Authority) {
					return t.applyUncorroboratedPreemptLocked(sess, sysid, gapSeq, prev, next, gapOrder, gapHbReceiptSeq, false, gapHbWall, gapHbMono)
				}
				if next.LinkTier != uxsvocab.LinkTierSITLLocal {
					sess.setPendingNonSITLLinkTier(next.LinkTier, gapOrder)
				}
				if reason := nonTeleopUncertainReason(next); reason != "" {
					sess.setTeleopUncertain(reason, next, gapOrder)
				}
			}
		}
		return nil, false
	}
	sess.clearPendingGap()
	return nil, false
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
			snap:                   *snap,
			rev:                    1,
			authorityEpoch:         1,
			heartbeatSeq:           1,
			hbReceiptSeq:           1,
			snapHbReceiptSeq:       1,
			lastHbMonoAt:           monoSample,
			lastHbOrder:            1,
			snapHbOrder:            1,
			currentLinkHbConfirmed: true,
			linkEpoch:              1,
		}
		initKey := hbStateKeyOf(*snap)
		newSess.recordHbSlot(seq, 1, initKey)
		newSess.recordAcceptedPacket(seq, mavcodec.MsgIDHeartbeat, normPeer, monoSample)
		newSess.recordLinkHbHistory(seq, initKey)
		newSess.recordAuthorityObservation(wallUTC)
		t.sessions[sysid] = newSess
		return IngestOutcome{
			Accepted: true,
			SysID:    sysid,
		}
	}

	if !sess.linkLost && !matchesPinnedPeer(sess, normPeer) {
		return IngestOutcome{CountedError: true}
	}
	candNext, ok := applyHeartbeatFields(sess.snap, vh, wallUTC)
	if !ok {
		return IngestOutcome{CountedError: true}
	}
	candKey := hbStateKeyOf(candNext)

	wasLost := sess.linkLost
	var gapEvents []TransitionEvent
	gapUrgent := false
	var hbOrder uint64
	var receiptSeq uint64
	if !wasLost {
		if !sess.lastHbMonoAt.IsZero() && monoSample.Before(sess.lastHbMonoAt) {
			sess.recordUnconfirmedLinkHbObservation(seq, normPeer, candKey, 0)
			return IngestOutcome{
				Accepted: true,
				SysID:    sysid,
			}
		}
		gapEvents, gapUrgent = t.resolvePendingGapBeforePacketLocked(sess, sysid, seq)
		var confirmedOrder, staleOrDup bool
		hbOrder, confirmedOrder, staleOrDup = sess.evaluateHeartbeatSeq(seq)
		if staleOrDup {
			sess.recordUnconfirmedLinkHbObservation(seq, normPeer, candKey, 0)
			return IngestOutcome{
				Accepted:      true,
				SysID:         sysid,
				Events:        gapEvents,
				UrgentSKGSync: gapUrgent,
			}
		}
		sess.hbReceiptSeq++
		receiptSeq = sess.hbReceiptSeq
		if !confirmedOrder {
			priorRestrictionOrder := sess.activeRestrictionMinOrder()
			candLifePos := sess.projectedLifetimePos(seq)
			sess.recordAcceptedPacket(seq, mavcodec.MsgIDHeartbeat, normPeer, monoSample)
			var preemptEvents []TransitionEvent
			preemptUrgent := false
			prev := sess.snap
			next, ok := applyHeartbeatFields(prev, vh, wallUTC)
			appliedPreempt := false
			if ok {
				nextKey := hbStateKeyOf(next)
				sess.recordUnconfirmedLinkHbObservation(seq, normPeer, nextKey, max(candLifePos, sess.lifetimePacketPos))
				if isPreemptAuthority(next.Authority) {
					preemptEvents, preemptUrgent = t.applyUncorroboratedPreemptLocked(sess, sysid, seq, prev, next, hbOrder, receiptSeq, false, wallUTC, monoSample)
					appliedPreempt = true
				} else {
					if next.LinkTier != uxsvocab.LinkTierSITLLocal {
						sess.setPendingNonSITLLinkTier(next.LinkTier, hbOrder)
					}
					if reason := nonTeleopUncertainReason(next); reason != "" {
						sess.setTeleopUncertain(reason, next, hbOrder)
					}
				}
			}
			if sess.hasPendingGap && sess.pendingGapSeq == seq {
				sess.pendingGapHasHb = true
				sess.pendingGapHbApplied = appliedPreempt
				sess.pendingGapPriorRestrictionOrder = priorRestrictionOrder
				sess.pendingGapHbReceiptSeq = receiptSeq
				sess.pendingGapHbVh = vh
				sess.pendingGapHbWall = wallUTC
				sess.pendingGapHbMono = monoSample
			}
			return IngestOutcome{
				Accepted:      true,
				SysID:         sysid,
				Events:        append(gapEvents, preemptEvents...),
				UrgentSKGSync: gapUrgent || preemptUrgent,
			}
		}
	} else {
		sess.hbReceiptSeq++
		receiptSeq = sess.hbReceiptSeq
	}

	prev := sess.snap
	next, ok := applyHeartbeatFields(prev, vh, wallUTC)
	if !ok {
		return IngestOutcome{CountedError: true}
	}
	candLifePos := sess.projectedLifetimePos(seq)
	if wasLost && sess.shouldHoldPreLossHeartbeatAt(seq, normPeer, prev, next, candLifePos) {
		sess.recordAcceptedPacket(seq, mavcodec.MsgIDHeartbeat, normPeer, monoSample)
		var holdEvents []TransitionEvent
		if isPreemptAuthority(next.Authority) {
			preemptEvents, _ := t.applyUncorroboratedPreemptLocked(sess, sysid, seq, prev, next, 1, receiptSeq, true, wallUTC, monoSample)
			restoreEv := newLinkTransitionEvent(sess.snap, t.incarnation, sess.authorityEpoch, sess.obsRev, seq, mavcodec.MsgIDHeartbeat, EventKindLinkRestored, "link_restored", wallUTC)
			holdEvents = append(holdEvents, restoreEv)
			holdEvents = append(holdEvents, preemptEvents...)
			return IngestOutcome{
				Accepted:      true,
				SysID:         sysid,
				Events:        holdEvents,
				UrgentSKGSync: true,
			}
		}
		restoredSnap := sess.snap
		restoredSnap.ObservedAt = wallUTC
		if err := restoredSnap.Validate(); err != nil {
			return IngestOutcome{CountedError: true}
		}
		sess.snap = restoredSnap
		sess.rev++
		if next.LinkTier != uxsvocab.LinkTierSITLLocal {
			sess.setPendingNonSITLLinkTier(next.LinkTier, 1)
		}
		if reason := nonTeleopUncertainReason(next); reason != "" {
			sess.setTeleopUncertain(reason, next, 1)
		}
		sess.recordAuthorityObservation(wallUTC)
		holdEvents = append(holdEvents, newLinkTransitionEvent(restoredSnap, t.incarnation, sess.authorityEpoch, sess.obsRev, seq, mavcodec.MsgIDHeartbeat, EventKindLinkRestored, "link_restored", wallUTC))
		return IngestOutcome{
			Accepted:      true,
			SysID:         sysid,
			Events:        holdEvents,
			UrgentSKGSync: true,
		}
	}
	if !wasLost &&
		(sess.teleopUncertainReason != "" || sess.pendingNonSITLLinkTier != "") &&
		hbOrder <= sess.activeRestrictionMinOrder() {
		nextKey := hbStateKeyOf(next)
		if isPreemptAuthority(next.Authority) {
			sess.recordAcceptedPacket(seq, mavcodec.MsgIDHeartbeat, normPeer, monoSample)
			if sess.currentLinkHbConfirmed || !sess.wasHbStateSeenBeforeCurrentLinkAt(seq, normPeer, nextKey, candLifePos) {
				sess.recordLinkHbHistoryAtOrder(seq, hbOrder, nextKey)
			}
			preemptEvents, preemptUrgent := t.applyUncorroboratedPreemptLocked(sess, sysid, seq, prev, next, hbOrder, receiptSeq, false, wallUTC, monoSample)
			return IngestOutcome{
				Accepted:      true,
				SysID:         sysid,
				Events:        append(gapEvents, preemptEvents...),
				UrgentSKGSync: gapUrgent || preemptUrgent,
			}
		}
		sess.advanceLifetimeSeq(seq)
		if sess.currentLinkHbConfirmed || !sess.wasHbStateSeenBeforeCurrentLinkAt(seq, normPeer, nextKey, candLifePos) {
			sess.recordLinkHbHistoryAtOrder(seq, hbOrder, nextKey)
		}
		if monoSample.After(sess.lastPacketAt) {
			sess.lastPacketAt = monoSample
		}
		if next.LinkTier != uxsvocab.LinkTierSITLLocal {
			sess.setPendingNonSITLLinkTier(next.LinkTier, hbOrder)
		}
		if reason := nonTeleopUncertainReason(next); reason != "" {
			sess.setTeleopUncertain(reason, next, hbOrder)
		}
		return IngestOutcome{
			Accepted:      true,
			SysID:         sysid,
			Events:        gapEvents,
			UrgentSKGSync: gapUrgent,
		}
	}
	if !wasLost &&
		(sess.shouldHoldWrapHeartbeat(seq, hbOrder, prev, next) ||
			sess.shouldHoldPreLossHeartbeatAt(seq, normPeer, prev, next, candLifePos)) {
		nextKey := hbStateKeyOf(next)
		if isPreemptAuthority(next.Authority) {
			sess.recordAcceptedPacket(seq, mavcodec.MsgIDHeartbeat, normPeer, monoSample)
			if sess.currentLinkHbConfirmed || !sess.wasHbStateSeenBeforeCurrentLinkAt(seq, normPeer, nextKey, candLifePos) {
				sess.recordLinkHbHistoryAtOrder(seq, hbOrder, nextKey)
			}
			preemptEvents, preemptUrgent := t.applyUncorroboratedPreemptLocked(sess, sysid, seq, prev, next, hbOrder, receiptSeq, false, wallUTC, monoSample)
			return IngestOutcome{
				Accepted:      true,
				SysID:         sysid,
				Events:        append(gapEvents, preemptEvents...),
				UrgentSKGSync: gapUrgent || preemptUrgent,
			}
		}
		sess.advanceLifetimeSeq(seq)
		if !sess.currentLinkHbConfirmed &&
			(!sess.hasSeqStream || (uint8(seq-sess.lastStreamSeq) >= 1 && uint8(seq-sess.lastStreamSeq) <= maxBackwardTelemetryReorder)) {
			sess.advanceTelemetrySeq(seq, true)
		}
		if sess.currentLinkHbConfirmed || !sess.wasHbStateSeenBeforeCurrentLinkAt(seq, normPeer, nextKey, candLifePos) {
			sess.recordLinkHbHistoryAtOrder(seq, hbOrder, nextKey)
		}
		if monoSample.After(sess.lastPacketAt) {
			sess.lastPacketAt = monoSample
		}
		if next.LinkTier != uxsvocab.LinkTierSITLLocal {
			sess.setPendingNonSITLLinkTier(next.LinkTier, hbOrder)
		}
		if reason := nonTeleopUncertainReason(next); reason != "" {
			sess.setTeleopUncertain(reason, next, hbOrder)
		}
		return IngestOutcome{
			Accepted:      true,
			SysID:         sysid,
			Events:        gapEvents,
			UrgentSKGSync: gapUrgent,
		}
	}

	sess.recordAcceptedPacket(seq, mavcodec.MsgIDHeartbeat, normPeer, monoSample)
	hbEvents, hbUrgent := t.commitConfirmedHeartbeatLocked(sess, sysid, seq, prev, next, hbOrder, receiptSeq, wasLost, wallUTC, monoSample)
	return IngestOutcome{
		Accepted:      true,
		SysID:         sysid,
		Events:        append(gapEvents, hbEvents...),
		UrgentSKGSync: gapUrgent || hbUrgent,
	}
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
	var gapEvents []TransitionEvent
	gapUrgent := false
	if !wasLost {
		gapEvents, gapUrgent = t.resolvePendingGapBeforePacketLocked(sess, sysid, seq)
	}
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
	failsafeInc := hadSysStatus && next.Failsafes > prevFailsafes
	if wasLost || failsafeInc {
		sess.recordAuthorityObservation(wallUTC)
	}

	out := IngestOutcome{
		Accepted:      true,
		SysID:         sysid,
		Events:        gapEvents,
		UrgentSKGSync: gapUrgent,
	}
	if wasLost {
		out.Events = append(out.Events, newLinkTransitionEvent(next, t.incarnation, sess.authorityEpoch, sess.obsRev, seq, mavcodec.MsgIDSysStatus, EventKindLinkRestored, "link_restored", wallUTC))
		out.UrgentSKGSync = true
	}
	if failsafeInc {
		out.Events = append(out.Events, TransitionEvent{
			SysID:                sysid,
			EntityID:             next.EntityID(),
			Domain:               next.Domain,
			Seq:                  seq,
			MsgID:                mavcodec.MsgIDSysStatus,
			Kind:                 EventKindFailsafeIncrement,
			Reasons:              []string{"failsafes_incremented"},
			Armed:                next.Armed,
			Authority:            next.Authority,
			AuthorityIncarnation: t.incarnation,
			AuthorityEpoch:       sess.authorityEpoch,
			ObservationRev:       sess.obsRev,
			CompanionMode:        next.CompanionMode,
			LinkTier:             next.LinkTier,
			Flags:                next.Flags,
			Failsafes:            next.Failsafes,
			Timestamp:            wallUTC.UnixMilli(),
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
	var gapEvents []TransitionEvent
	gapUrgent := false
	if !wasLost {
		gapEvents, gapUrgent = t.resolvePendingGapBeforePacketLocked(sess, sysid, seq)
	}
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
		Accepted:      true,
		SysID:         sysid,
		Telemetry:     &tf,
		Events:        gapEvents,
		UrgentSKGSync: gapUrgent,
	}
	if wasLost {
		sess.recordAuthorityObservation(wallUTC)
		out.Events = append(out.Events, newLinkTransitionEvent(next, t.incarnation, sess.authorityEpoch, sess.obsRev, seq, mavcodec.MsgIDAttitudeQuaternion, EventKindLinkRestored, "link_restored", wallUTC))
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
	var gapEvents []TransitionEvent
	gapUrgent := false
	if !wasLost {
		gapEvents, gapUrgent = t.resolvePendingGapBeforePacketLocked(sess, sysid, seq)
	}
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
		Accepted:      true,
		SysID:         sysid,
		Telemetry:     &tf,
		Events:        gapEvents,
		UrgentSKGSync: gapUrgent,
	}
	if wasLost {
		sess.recordAuthorityObservation(wallUTC)
		out.Events = append(out.Events, newLinkTransitionEvent(next, t.incarnation, sess.authorityEpoch, sess.obsRev, seq, mavcodec.MsgIDLocalPositionNED, EventKindLinkRestored, "link_restored", wallUTC))
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
	var gapEvents []TransitionEvent
	gapUrgent := false
	if !wasLost {
		gapEvents, gapUrgent = t.resolvePendingGapBeforePacketLocked(sess, sysid, seq)
	}
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
		Accepted:      true,
		SysID:         sysid,
		Telemetry:     &tf,
		Events:        gapEvents,
		UrgentSKGSync: gapUrgent,
	}
	if wasLost {
		sess.recordAuthorityObservation(wallUTC)
		out.Events = append(out.Events, newLinkTransitionEvent(next, t.incarnation, sess.authorityEpoch, sess.obsRev, seq, mavcodec.MsgIDGlobalPositionInt, EventKindLinkRestored, "link_restored", wallUTC))
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
	var events []TransitionEvent
	urgent := false
	var candPos uint64
	var advanceHead, setGap, recordAckSlot, isDupAck, ackUncertain bool
	if !wasLost {
		events, urgent = t.resolvePendingGapBeforePacketLocked(sess, sysid, seq)
		candPos, advanceHead, setGap, recordAckSlot, isDupAck, ackUncertain = sess.evaluateCommandAckSeq(seq, msg)
	}

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
		candPos = 1
		advanceHead = true
		setGap = false
		recordAckSlot = true
		ackUncertain = false
		sess.recordAuthorityObservation(wallUTC)
		events = append(events, newLinkTransitionEvent(next, t.incarnation, sess.authorityEpoch, sess.obsRev, seq, mavcodec.MsgIDCommandAck, EventKindLinkRestored, "link_restored", wallUTC))
	}
	if isDupAck {
		return IngestOutcome{
			Accepted:      true,
			SysID:         sysid,
			Events:        events,
			UrgentSKGSync: urgent,
		}
	}
	if setGap {
		if !sess.hasPendingGap {
			sess.hasPendingGap = true
			sess.pendingGapSeq = seq
			sess.pendingGapOrder = sess.seqStreamPos + uint64(uint8(seq-sess.lastStreamSeq))
			sess.pendingGapHasHb = false
			sess.pendingGapHbApplied = false
			sess.pendingGapPriorRestrictionOrder = 0
			sess.pendingGapHbReceiptSeq = 0
			sess.pendingGapHbVh = mavcodec.VehicleHeartbeat{}
			sess.pendingGapHbWall = time.Time{}
			sess.pendingGapHbMono = time.Time{}
		}
		sess.pendingGapHasAck = true
		sess.pendingGapAck = msg
	}
	if advanceHead {
		sess.hasSeqStream = true
		sess.seqStreamPos = candPos
		sess.lastStreamSeq = seq
		sess.seenSeqOrder[seq] = candPos
	}
	sess.lastAckOrder = candPos
	sess.lastAckSeq++
	sess.lastAckUncertain = ackUncertain
	if recordAckSlot {
		if !ackUncertain {
			sess.seenSeqOrder[seq] = candPos
		}
		sess.seenAckFrames[seq] = ackFrameSlot{
			active:      true,
			ack:         msg,
			packetOrder: candPos,
		}
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
	var events []TransitionEvent
	urgent := false
	if !wasLost {
		events, urgent = t.resolvePendingGapBeforePacketLocked(sess, sysid, seq)
	}
	snap := sess.snap

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
		sess.recordAuthorityObservation(wallUTC)
		events = append(events, newLinkTransitionEvent(snap, t.incarnation, sess.authorityEpoch, sess.obsRev, seq, mavcodec.MsgIDStatusText, EventKindLinkRestored, "link_restored", wallUTC))
	}

	cleanText := strings.TrimRight(msg.Text, "\x00")
	events = append(events, TransitionEvent{
		SysID:                sysid,
		EntityID:             snap.EntityID(),
		Domain:               snap.Domain,
		Seq:                  seq,
		MsgID:                mavcodec.MsgIDStatusText,
		Kind:                 EventKindStatusText,
		Reasons:              []string{"statustext"},
		Armed:                snap.Armed,
		Authority:            snap.Authority,
		AuthorityIncarnation: t.incarnation,
		AuthorityEpoch:       sess.authorityEpoch,
		ObservationRev:       sess.obsRev,
		CompanionMode:        snap.CompanionMode,
		LinkTier:             snap.LinkTier,
		Flags:                snap.Flags,
		Failsafes:            snap.Failsafes,
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

func (t *Tracker) evaluateSilenceSysIDAt(sysid uint8, wallUTC, monoSample time.Time) (TransitionEvent, bool) {
	wUTC := observedUTC(wallUTC)
	mSample := monoOrEpoch(monoSample)

	t.mu.Lock()
	defer t.mu.Unlock()
	sess, ok := t.sessions[sysid]
	if !ok {
		return TransitionEvent{}, false
	}
	return t.evaluateSessionSilence(sess, wUTC, mSample)
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
	sess.linkEpoch++
	sess.lastHbMonoAt = time.Time{}
	sess.lastHbOrder = 0
	sess.snapHbOrder = 0
	sess.snapHbReceiptSeq = 0
	sess.currentLinkHbConfirmed = false
	sess.clearTeleopUncertain()
	sess.clearPendingNonSITLLinkTier()
	sess.hasLifeSeq = false
	sess.hasLifePendingGap = false
	sess.hasSeqStream = false
	sess.seqStreamPos = 0
	sess.clearPendingGap()
	sess.lastAckOrder = 0
	sess.lastAckSeq = 0
	sess.lastAckUncertain = false
	sess.seenAckFrames = [256]ackFrameSlot{}
	sess.seenHbFrames = [256]hbFrameSlot{}
	sess.seenSeqOrder = [256]uint64{}
	sess.recordAuthorityObservation(wallUTC)
	return newLinkTransitionEvent(
		sess.snap,
		t.incarnation,
		sess.authorityEpoch,
		sess.obsRev,
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
	uncertainTier := sess.pendingNonSITLLinkTier
	if uncertainTier == "" {
		uncertainTier = sess.uncertainLinkTier
	}
	return ActuationTarget{
		SysID:                     sysid,
		EntityID:                  sess.snap.EntityID(),
		Domain:                    sess.snap.Domain,
		PeerAddr:                  sess.peerAddr,
		Armed:                     sess.snap.Armed,
		Authority:                 sess.snap.Authority,
		AuthorityIncarnation:      t.incarnation,
		AuthorityEpoch:            sess.authorityEpoch,
		ObservationRev:            sess.obsRev,
		HeartbeatSeq:              sess.heartbeatSeq,
		HeartbeatOrder:            sess.snapHbOrder,
		HeartbeatReceiptRev:       sess.snapHbReceiptSeq,
		LatestHeartbeatReceiptRev: sess.hbReceiptSeq,
		LinkEpoch:                 sess.linkEpoch,
		PacketOrder:               sess.seqStreamPos,
		LastAckOrder:              sess.lastAckOrder,
		LastAckSeq:                sess.lastAckSeq,
		LastAckUncertain:          sess.lastAckUncertain,
		CompanionMode:             sess.snap.CompanionMode,
		LinkTier:                  sess.snap.LinkTier,
		LinkLost:                  sess.linkLost,
		CurrentLinkHbUnconfirmed:  !sess.currentLinkHbConfirmed,
		TeleopUncertain:           sess.teleopUncertainReason != "",
		TeleopUncertainReason:     sess.teleopUncertainReason,
		UncertainAuthority:        sess.uncertainAuthority,
		UncertainLinkTier:         uncertainTier,
		UncertainArmed:            sess.uncertainArmed,
		HeartbeatObservedUnixNs:   sess.snap.HeartbeatObservedUnixNs,
	}, true
}

// ResolveObservation resolves an inbound operator observation (optional obsRev,
// authorityEpoch, and UTC observedMs) to the matching host-monotonic observation
// revision recorded for sysid. Timestamp-only lookups (obsRev == 0) whose
// observedMs was shared across an authority-epoch boundary are rejected as
// ambiguous so re-engagement requires an explicit current observation revision.
func (t *Tracker) ResolveObservation(sysid uint8, obsRev, authorityEpoch uint64, observedMs int64) (uint64, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	sess, ok := t.sessions[sysid]
	if !ok || sess.obsRev == 0 {
		return 0, false
	}
	if obsRev > sess.obsRev {
		return 0, false
	}
	n := len(sess.obsHistory)
	if obsRev > 0 {
		for i := 1; i <= n; i++ {
			idx := (sess.obsIdx - i + n) % n
			entry := sess.obsHistory[idx]
			if entry.rev != obsRev {
				continue
			}
			if (authorityEpoch == 0 || entry.epoch == authorityEpoch) &&
				(observedMs <= 0 || entry.obsMs == observedMs) {
				return entry.rev, true
			}
			return 0, false
		}
		if obsRev == sess.obsRev &&
			(authorityEpoch == 0 || sess.authorityEpoch == authorityEpoch) &&
			(observedMs <= 0 || sess.snap.HeartbeatObservedUnixNs/int64(time.Millisecond) == observedMs) {
			return sess.obsRev, true
		}
		return 0, false
	}
	if observedMs <= 0 {
		return 0, false
	}
	epochState, seenMs := sess.obsMsEpoch[observedMs]
	if !seenMs || epochState == ambiguousObsEpoch {
		return 0, false
	}
	if authorityEpoch > 0 && authorityEpoch != epochState {
		return 0, false
	}
	for i := 1; i <= n; i++ {
		idx := (sess.obsIdx - i + n) % n
		entry := sess.obsHistory[idx]
		if entry.rev == 0 {
			continue
		}
		if entry.epoch == epochState && entry.obsMs == observedMs {
			return entry.rev, true
		}
	}
	if sess.authorityEpoch == epochState &&
		sess.snap.HeartbeatObservedUnixNs/int64(time.Millisecond) == observedMs {
		return sess.obsRev, true
	}
	return 0, false
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
