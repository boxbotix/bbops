package uxsvocab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/c360studio/semengine/graph"
	"github.com/c360studio/semengine/message"
	"github.com/c360studio/semengine/payloadregistry"
	"github.com/c360studio/semengine/vocabulary"
)

const (
	// PayloadDomain is the message.Type domain for UXS vehicle SKG snapshots.
	PayloadDomain = "uxs"
	// PayloadCategory is the message.Type category for UXS vehicle SKG snapshots.
	PayloadCategory = "vehicle"
	// PayloadVersion is the message.Type schema version for UXS vehicle SKG snapshots.
	PayloadVersion = "v1"
	// TripleSource identifies bbops's MAVLink SKG adapter as the triple producer.
	TripleSource = "bbops-mavlink-ingest"
)

// VehiclePayloadType is the canonical `uxs.vehicle.v1` message schema registered
// in SemEngine's PayloadRegistry.
var VehiclePayloadType = message.Type{
	Domain:   PayloadDomain,
	Category: PayloadCategory,
	Version:  PayloadVersion,
}

// HealthFlags holds the 4 structured boolean flags unpacked from bits [19:16]
// of the bbstack 0xB1 HEARTBEAT.custom_mode word. Stored directly as a native
// struct in Triple.Object (`D2 Option A`) so WS #2 (ENTITY_STATES) and GraphQL
// serialize structured JSON objects without double-encoding.
type HealthFlags struct {
	ClockSyncLocked       bool `json:"clock_sync_locked"`
	TeleopDeadmanClamped  bool `json:"teleop_deadman_clamped"`
	CRSFLinkHealthy       bool `json:"crsf_link_healthy"`
	CompanionTrunkHealthy bool `json:"companion_trunk_healthy"`
}

// VehicleSnapshot is the constant-predicate Graphable SKG entity payload for a
// controllable UXS vehicle. Every snapshot emits the exact same 22 producer
// predicates so `processor/graph-index` skips PREDICATE_INDEX rewrites after
// initial entity birth, while `processor/graph-ingest` stamps
// `entity.indexing.profile = "signal"` from the payload registration.
type VehicleSnapshot struct {
	Org      string `json:"org"`
	Platform string `json:"platform"`
	SysID    uint8  `json:"sysid"`
	Title    string `json:"title"`

	Domain        string `json:"domain"`
	Authority     string `json:"authority"`
	CompanionMode string `json:"companion_mode"`
	LinkTier      string `json:"link_tier"`

	Armed       bool        `json:"armed"`
	Flags       HealthFlags `json:"flags"`
	Failsafes   int         `json:"failsafes"`
	CRCErrors   int         `json:"crc_errors"`
	LoopDtUs    int         `json:"loop_dt_us"`
	MaxLoopDtUs int         `json:"max_loop_dt_us"`
	RTTUs       int         `json:"rtt_us"`

	Quaternion [4]float64 `json:"quaternion"`
	BodyRates  [3]float64 `json:"body_rates"`
	NED        [3]float64 `json:"ned"`
	Vx         float64    `json:"vx"`
	Vy         float64    `json:"vy"`
	Vz         float64    `json:"vz"`
	HeadingDeg float64    `json:"heading_deg"`

	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Altitude  float64 `json:"altitude"`

	ObservedAt time.Time `json:"observed_at"`
}

var (
	_ message.Payload          = (*VehicleSnapshot)(nil)
	_ message.IndexingProfiler = (*VehicleSnapshot)(nil)
	_ graph.Graphable          = (*VehicleSnapshot)(nil)
)

// NewVehicleSnapshot constructs a VehicleSnapshot initialized with neutral
// finite defaults (identity quaternion [1, 0, 0, 0], zero kinematics/GPS/counters,
// disarmed mode, mcu-failsafe authority, sitl-local link tier) so snapshots
// emitted before SYS_STATUS (#1) or GLOBAL_POSITION_INT (#33) arrive still
// carry the full 22-predicate set.
func NewVehicleSnapshot(org, platform string, sysid uint8, domain string) (*VehicleSnapshot, error) {
	if _, err := VehicleEntityID(org, platform, sysid); err != nil {
		return nil, err
	}
	if !IsValidDomain(domain) {
		return nil, fmt.Errorf("uxsvocab: invalid vehicle domain %q", domain)
	}
	return &VehicleSnapshot{
		Org:           org,
		Platform:      platform,
		SysID:         sysid,
		Title:         defaultVehicleTitle(sysid),
		Domain:        domain,
		Authority:     AuthorityMCUFailsafe,
		CompanionMode: ModeDisarmed,
		LinkTier:      LinkTierSITLLocal,
		Quaternion:    [4]float64{1, 0, 0, 0},
	}, nil
}

func defaultVehicleTitle(sysid uint8) string {
	return fmt.Sprintf("UXS-%d", sysid)
}

// Schema implements message.Payload.
func (s *VehicleSnapshot) Schema() message.Type {
	return VehiclePayloadType
}

// IndexingProfile implements message.IndexingProfiler so both Graphable
// extraction and PayloadRegistry agree on the "signal" telemetry profile.
func (s *VehicleSnapshot) IndexingProfile() string {
	return vocabulary.IndexingProfileSignal
}

// Validate checks entity identity, closed enum strings, non-negative health
// counters, and finite float invariants across all kinematic and geodetic fields.
func (s *VehicleSnapshot) Validate() error {
	if s == nil {
		return errors.New("uxsvocab: nil vehicle snapshot")
	}
	if _, err := VehicleEntityID(s.Org, s.Platform, s.SysID); err != nil {
		return err
	}
	if !IsValidDomain(s.Domain) {
		return fmt.Errorf("uxsvocab: invalid domain %q", s.Domain)
	}
	if !IsValidAuthority(s.Authority) {
		return fmt.Errorf("uxsvocab: invalid authority %q", s.Authority)
	}
	if !IsValidCompanionMode(s.CompanionMode) {
		return fmt.Errorf("uxsvocab: invalid companion mode %q", s.CompanionMode)
	}
	if !IsValidLinkTier(s.LinkTier) {
		return fmt.Errorf("uxsvocab: invalid link tier %q", s.LinkTier)
	}
	if s.Failsafes < 0 || s.CRCErrors < 0 || s.LoopDtUs < 0 || s.MaxLoopDtUs < 0 || s.RTTUs < 0 {
		return errors.New("uxsvocab: health counters and durations must be non-negative")
	}

	for i, v := range s.Quaternion {
		if !isFinite(v) {
			return fmt.Errorf("uxsvocab: non-finite quaternion[%d]", i)
		}
	}
	for i, v := range s.BodyRates {
		if !isFinite(v) {
			return fmt.Errorf("uxsvocab: non-finite body_rates[%d]", i)
		}
	}
	for i, v := range s.NED {
		if !isFinite(v) {
			return fmt.Errorf("uxsvocab: non-finite ned[%d]", i)
		}
	}
	namedFloats := []struct {
		name string
		val  float64
	}{
		{name: "vx", val: s.Vx},
		{name: "vy", val: s.Vy},
		{name: "vz", val: s.Vz},
		{name: "heading_deg", val: s.HeadingDeg},
		{name: "latitude", val: s.Latitude},
		{name: "longitude", val: s.Longitude},
		{name: "altitude", val: s.Altitude},
	}
	for _, f := range namedFloats {
		if !isFinite(f.val) {
			return fmt.Errorf("uxsvocab: non-finite %s", f.name)
		}
	}
	if s.HeadingDeg < 0 || s.HeadingDeg > 360 {
		return fmt.Errorf("uxsvocab: heading_deg %v out of [0, 360]", s.HeadingDeg)
	}
	if s.Latitude < -90 || s.Latitude > 90 {
		return fmt.Errorf("uxsvocab: latitude %v out of [-90, 90]", s.Latitude)
	}
	if s.Longitude < -180 || s.Longitude > 180 {
		return fmt.Errorf("uxsvocab: longitude %v out of [-180, 180]", s.Longitude)
	}
	return nil
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// MarshalJSON implements json.Marshaler for message.Payload.
func (s *VehicleSnapshot) MarshalJSON() ([]byte, error) {
	type alias VehicleSnapshot
	return json.Marshal((*alias)(s))
}

// UnmarshalJSON implements json.Unmarshaler for message.Payload, validating
// exact vector lengths for quaternion (4), body_rates (3), and ned (3) before
// copying into fixed arrays so encoding/json never zero-pads or truncates.
func (s *VehicleSnapshot) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("uxsvocab: null vehicle snapshot")
	}
	type alias VehicleSnapshot
	var base alias
	aux := struct {
		*alias
		Quaternion []float64 `json:"quaternion"`
		BodyRates  []float64 `json:"body_rates"`
		NED        []float64 `json:"ned"`
	}{
		alias: &base,
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.Quaternion) != len(s.Quaternion) {
		return fmt.Errorf("uxsvocab: quaternion array length %d, want %d", len(aux.Quaternion), len(s.Quaternion))
	}
	if len(aux.BodyRates) != len(s.BodyRates) {
		return fmt.Errorf("uxsvocab: body_rates array length %d, want %d", len(aux.BodyRates), len(s.BodyRates))
	}
	if len(aux.NED) != len(s.NED) {
		return fmt.Errorf("uxsvocab: ned array length %d, want %d", len(aux.NED), len(s.NED))
	}
	next := VehicleSnapshot(base)
	copy(next.Quaternion[:], aux.Quaternion)
	copy(next.BodyRates[:], aux.BodyRates)
	copy(next.NED[:], aux.NED)
	*s = next
	return nil
}

// EntityID implements graph.Graphable, returning `<org>.<platform>.robotics.uxs.vehicle.<sysid>`
// or the empty string if identity inputs are invalid.
func (s *VehicleSnapshot) EntityID() string {
	if s == nil {
		return ""
	}
	id, err := VehicleEntityID(s.Org, s.Platform, s.SysID)
	if err != nil {
		return ""
	}
	return id
}

// Triples implements graph.Graphable, emitting the constant 22-predicate producer
// set in deterministic order without stamping reserved graph.SourceIndexingProfile.
func (s *VehicleSnapshot) Triples() []message.Triple {
	entityID := s.EntityID()
	if entityID == "" {
		return nil
	}
	ts := s.ObservedAt
	if ts.IsZero() {
		ts = time.Unix(0, 0).UTC()
	} else {
		ts = ts.UTC()
	}
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = defaultVehicleTitle(s.SysID)
	}

	triple := func(predicate string, obj any) message.Triple {
		return message.Triple{
			Subject:    entityID,
			Predicate:  predicate,
			Object:     obj,
			Source:     TripleSource,
			Timestamp:  ts,
			Confidence: 1.0,
		}
	}

	return []message.Triple{
		triple(PredicateDomainKind, s.Domain),
		triple(PredicateAuthorityState, s.Authority),
		triple(PredicateModeCompanion, s.CompanionMode),
		triple(PredicateLinkTier, s.LinkTier),
		triple(PredicateHealthArmed, s.Armed),
		triple(PredicateHealthFlags, s.Flags),
		triple(PredicateHealthFailsafes, s.Failsafes),
		triple(PredicateHealthCRCErrors, s.CRCErrors),
		triple(PredicateHealthLoopDtUs, s.LoopDtUs),
		triple(PredicateHealthMaxLoopDtUs, s.MaxLoopDtUs),
		triple(PredicateHealthRTTUs, s.RTTUs),
		triple(PredicateAttitudeQuaternion, s.Quaternion),
		triple(PredicateAttitudeRates, s.BodyRates),
		triple(PredicateKinematicsNED, s.NED),
		triple(PredicateKinematicsVx, s.Vx),
		triple(PredicateKinematicsVy, s.Vy),
		triple(PredicateKinematicsVz, s.Vz),
		triple(PredicateKinematicsHeadingDeg, s.HeadingDeg),
		triple(vocabulary.GeoLocationLatitude, s.Latitude),
		triple(vocabulary.GeoLocationLongitude, s.Longitude),
		triple(vocabulary.GeoLocationAltitude, s.Altitude),
		triple(vocabulary.DCTermsTitle, title),
	}
}

// RegisterPayloads registers the `uxs.vehicle.v1` VehicleSnapshot payload with
// `IndexingProfileSignal` ("signal") so `processor/graph-ingest` stamps
// `entity.indexing.profile = "signal"` at entity birth.
func RegisterPayloads(reg *payloadregistry.Registry) error {
	if reg == nil {
		return errors.New("uxsvocab: nil payload registry")
	}
	return reg.Register(&payloadregistry.Registration{
		Factory:         func() any { return &VehicleSnapshot{} },
		Domain:          PayloadDomain,
		Category:        PayloadCategory,
		Version:         PayloadVersion,
		Description:     "Constant-predicate UXS vehicle live SKG snapshot",
		IndexingProfile: vocabulary.IndexingProfileSignal,
	})
}
