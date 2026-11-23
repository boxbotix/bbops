// Package uxsvocab defines the bbops UXS consumer vocabulary pack, ADR-102
// vehicle entity identity builder, and constant-predicate Graphable snapshot
// payload registered with SemEngine.
package uxsvocab

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"

	semtypes "github.com/c360studio/semengine/pkg/types"
	"github.com/c360studio/semengine/vocabulary"
)

//go:embed uxs_pack.json
var embeddedPackJSON []byte

const (
	// PackName is the declarative vocabulary pack identifier registered with SemEngine.
	PackName = "bbops-uxs"

	// EntityTypeVehicle is the canonical <domain>.<type> key for controllable UXS vehicles.
	EntityTypeVehicle = "uxs.vehicle"

	// PredicateDomainKind records the UXS operating domain ("quadcopter" | "rover" | "surface-boat").
	PredicateDomainKind = "uxs.domain.kind"
	// PredicateAuthorityState records the active CommandMux control authority state.
	PredicateAuthorityState = "uxs.authority.state"
	// PredicateModeCompanion records the companion operating mode.
	PredicateModeCompanion = "uxs.mode.companion"
	// PredicateLinkTier records the active telemetry/actuation link tier.
	PredicateLinkTier = "uxs.link.tier"

	// PredicateHealthArmed records whether vehicle propulsion actuators are armed.
	PredicateHealthArmed = "uxs.health.armed"
	// PredicateHealthFlags records structured 0xB1 link/trunk health flags (json).
	PredicateHealthFlags = "uxs.health.flags"
	// PredicateHealthFailsafes records cumulative MCU failsafe count.
	PredicateHealthFailsafes = "uxs.health.failsafes"
	// PredicateHealthCRCErrors records cumulative companion-MCU CRC error count.
	PredicateHealthCRCErrors = "uxs.health.crcerrors"
	// PredicateHealthLoopDtUs records latest MCU loop period in microseconds.
	PredicateHealthLoopDtUs = "uxs.health.loopdtus"
	// PredicateHealthMaxLoopDtUs records peak MCU loop period in microseconds.
	PredicateHealthMaxLoopDtUs = "uxs.health.maxloopdtus"
	// PredicateHealthRTTUs records latest companion-MCU ping RTT in microseconds.
	PredicateHealthRTTUs = "uxs.health.rttus"

	// PredicateAttitudeQuaternion records body-to-NED attitude quaternion [qw, qx, qy, qz] (json).
	PredicateAttitudeQuaternion = "uxs.attitude.quaternion"
	// PredicateAttitudeRates records body angular rates [p, q, r] in rad/s (json).
	PredicateAttitudeRates = "uxs.attitude.rates"

	// PredicateKinematicsNED records local NED position [x, y, z] in meters (json).
	PredicateKinematicsNED = "uxs.kinematics.ned"
	// PredicateKinematicsVx records local North velocity in m/s.
	PredicateKinematicsVx = "uxs.kinematics.vx"
	// PredicateKinematicsVy records local East velocity in m/s.
	PredicateKinematicsVy = "uxs.kinematics.vy"
	// PredicateKinematicsVz records local Down velocity in m/s.
	PredicateKinematicsVz = "uxs.kinematics.vz"
	// PredicateKinematicsHeadingDeg records vehicle heading in degrees [0, 360].
	PredicateKinematicsHeadingDeg = "uxs.kinematics.headingdeg"
)

const (
	// DomainQuadcopter identifies an aerial quadcopter UXS.
	DomainQuadcopter = "quadcopter"
	// DomainRover identifies a ground rover UXS.
	DomainRover = "rover"
	// DomainSurfaceBoat identifies an uncrewed surface vessel (USV) UXS.
	DomainSurfaceBoat = "surface-boat"

	// AuthorityDirectCRSFOverride indicates hardware RC pilot override holds actuator authority.
	AuthorityDirectCRSFOverride = "direct-crsf-override"
	// AuthorityCompanionTeleop indicates operator teleop via companion holds authority.
	AuthorityCompanionTeleop = "companion-teleop"
	// AuthorityCompanionAutonomous indicates onboard autonomous planner holds authority.
	AuthorityCompanionAutonomous = "companion-autonomous"
	// AuthorityMCUFailsafe indicates MCU hardware failsafe holds authority.
	AuthorityMCUFailsafe = "mcu-failsafe"

	// ModeDisarmed indicates companion actuation is disarmed.
	ModeDisarmed = "disarmed"
	// ModeTeleopVelocity indicates companion velocity teleoperation mode.
	ModeTeleopVelocity = "teleop-velocity"
	// ModeAutonomousVelocity indicates companion autonomous velocity mode.
	ModeAutonomousVelocity = "autonomous-velocity"
	// ModeHoldPosition indicates companion position/station-keeping hold mode.
	ModeHoldPosition = "hold-position"

	// LinkTierSITLLocal indicates local loopback SITL simulation link.
	LinkTierSITLLocal = "sitl-local"
	// LinkTierDirectBench indicates direct wired/bench link.
	LinkTierDirectBench = "direct-bench"
	// LinkTierMeshHighRate indicates Wi-Fi/HaLow high-rate mesh link (10-30 Hz teleop).
	LinkTierMeshHighRate = "mesh-high-rate"
	// LinkTierMeshSupervisory indicates US915 LoRa 1 Hz supervisory-only mesh link.
	LinkTierMeshSupervisory = "mesh-supervisory"
)

// VehicleDomainDelegation declares bbops's entity-ID minting delegation for
// SemEngine ADR-102 corpus auditing.
var VehicleDomainDelegation = semtypes.EntityDomainDelegation{
	Producer: "bbops",
	Domain:   "uxs",
	Type:     "vehicle",
}

// UXSPredicates returns a fresh copy of the 18 canonical uxs.* predicates in
// deterministic snapshot emission order.
func UXSPredicates() []string {
	return []string{
		PredicateDomainKind,
		PredicateAuthorityState,
		PredicateModeCompanion,
		PredicateLinkTier,
		PredicateHealthArmed,
		PredicateHealthFlags,
		PredicateHealthFailsafes,
		PredicateHealthCRCErrors,
		PredicateHealthLoopDtUs,
		PredicateHealthMaxLoopDtUs,
		PredicateHealthRTTUs,
		PredicateAttitudeQuaternion,
		PredicateAttitudeRates,
		PredicateKinematicsNED,
		PredicateKinematicsVx,
		PredicateKinematicsVy,
		PredicateKinematicsVz,
		PredicateKinematicsHeadingDeg,
	}
}

// SnapshotPredicates returns a fresh copy of all 22 constant producer predicates
// emitted on every VehicleSnapshot (18 uxs.* predicates + 3 geo.location.* + dc.terms.title).
func SnapshotPredicates() []string {
	preds := make([]string, 0, 22)
	preds = append(preds, UXSPredicates()...)
	preds = append(preds,
		vocabulary.GeoLocationLatitude,
		vocabulary.GeoLocationLongitude,
		vocabulary.GeoLocationAltitude,
		vocabulary.DCTermsTitle,
	)
	return preds
}

// LoadPack parses and validates the embedded uxs_pack.json without mutating
// global SemEngine vocabulary registries.
func LoadPack() (vocabulary.PackSpec, error) {
	return loadPackBytes(embeddedPackJSON)
}

func loadPackBytes(raw []byte) (vocabulary.PackSpec, error) {
	pack, err := vocabulary.LoadPackJSON(bytes.NewReader(raw))
	if err != nil {
		return vocabulary.PackSpec{}, fmt.Errorf("uxsvocab: %w", err)
	}
	if err := pack.Validate(); err != nil {
		return vocabulary.PackSpec{}, fmt.Errorf("uxsvocab: validate pack %q: %w", pack.Name, err)
	}
	return pack, nil
}

var (
	registerOnce sync.Once
	registerErr  error
)

// Register loads and atomically registers the bbops UXS vocabulary pack into
// SemEngine's process-global vocabulary registry once. Subsequent calls return
// the initial registration outcome.
func Register() error {
	registerOnce.Do(func() {
		pack, err := LoadPack()
		if err != nil {
			registerErr = err
			return
		}
		if err := vocabulary.RegisterPack(pack); err != nil {
			registerErr = fmt.Errorf("uxsvocab: register pack %q: %w", pack.Name, err)
		}
	})
	return registerErr
}

// IsValidDomain reports whether v is one of the 3 canonical UXS domain strings.
func IsValidDomain(v string) bool {
	switch v {
	case DomainQuadcopter, DomainRover, DomainSurfaceBoat:
		return true
	default:
		return false
	}
}

// IsValidAuthority reports whether v is one of the 4 canonical CommandMux authority strings.
func IsValidAuthority(v string) bool {
	switch v {
	case AuthorityDirectCRSFOverride, AuthorityCompanionTeleop, AuthorityCompanionAutonomous, AuthorityMCUFailsafe:
		return true
	default:
		return false
	}
}

// IsValidCompanionMode reports whether v is one of the 4 canonical companion mode strings.
func IsValidCompanionMode(v string) bool {
	switch v {
	case ModeDisarmed, ModeTeleopVelocity, ModeAutonomousVelocity, ModeHoldPosition:
		return true
	default:
		return false
	}
}

// IsValidLinkTier reports whether v is one of the 4 canonical link tier strings.
func IsValidLinkTier(v string) bool {
	switch v {
	case LinkTierSITLLocal, LinkTierDirectBench, LinkTierMeshHighRate, LinkTierMeshSupervisory:
		return true
	default:
		return false
	}
}
