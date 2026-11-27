package mavcodec

import (
	"fmt"

	"github.com/boxbotix/bbops/internal/uxsvocab"
)

const (
	// BBStackMagic is byte [31:24] of the bbstack Edge Contract v1 custom_mode word.
	BBStackMagic uint8 = 0xB1

	// Standard MIT minimal.xml / common.xml constants used at the 0xB1 edge boundary.
	MAVTypeQuadrotor          uint8 = 2
	MAVTypeGCS                uint8 = 6
	MAVTypeGroundRover        uint8 = 10
	MAVTypeSurfaceBoat        uint8 = 11
	AutopilotGeneric          uint8 = 0
	ModeFlagCustomModeEnabled uint8 = 1
	ModeFlagSafetyArmed       uint8 = 128
	MAVStateActive            uint8 = 4
	CompIDAutopilot1          uint8 = 1
	CompIDMissionPlanner      uint8 = 190
	GCSSystemID               uint8 = 255
	MAVLinkVersionV2          uint8 = 3
	MaxStatusTextBytes        int   = 50

	// Standard MIT common.xml #84 SET_POSITION_TARGET_LOCAL_NED frame & velocity-only mask.
	MAVFrameLocalNED                   uint8  = 1
	MAVFrameBodyNED                    uint8  = 8
	PositionTargetTypeMaskVelocityOnly uint16 = 0x07C7

	// Standard MIT common.xml #76 COMMAND_LONG command IDs and #77 COMMAND_ACK results.
	MAVCmdNavLoiterUnlim     uint16 = 17
	MAVCmdDoSetMode          uint16 = 176
	MAVCmdComponentArmDisarm uint16 = 400
	MAVCmdUser1              uint16 = 31010

	MAVResultAccepted            uint8 = 0
	MAVResultTemporarilyRejected uint8 = 1
	MAVResultDenied              uint8 = 2
	MAVResultUnsupported         uint8 = 3
	MAVResultFailed              uint8 = 4
	MAVResultInProgress          uint8 = 5
	MAVResultCancelled           uint8 = 6
)

const (
	flagBitClockSyncLocked       uint8 = 1 << 0 // bit 16 in custom_mode
	flagBitTeleopDeadmanClamped  uint8 = 1 << 1 // bit 17 in custom_mode
	flagBitCRSFLinkHealthy       uint8 = 1 << 2 // bit 18 in custom_mode
	flagBitCompanionTrunkHealthy uint8 = 1 << 3 // bit 19 in custom_mode
)

// LinkTier encodes bits [23:20] of the 0xB1 HEARTBEAT.custom_mode word.
type LinkTier uint8

const (
	LinkTierSITLLocal       LinkTier = 0
	LinkTierDirectBench     LinkTier = 1
	LinkTierMeshHighRate    LinkTier = 2
	LinkTierMeshSupervisory LinkTier = 3
)

// Valid reports whether t is a defined 0xB1 LinkTier value (0..3).
func (t LinkTier) Valid() bool {
	return t <= LinkTierMeshSupervisory
}

// VocabString maps LinkTier onto its canonical uxsvocab kebab-case string.
func (t LinkTier) VocabString() (string, error) {
	switch t {
	case LinkTierSITLLocal:
		return uxsvocab.LinkTierSITLLocal, nil
	case LinkTierDirectBench:
		return uxsvocab.LinkTierDirectBench, nil
	case LinkTierMeshHighRate:
		return uxsvocab.LinkTierMeshHighRate, nil
	case LinkTierMeshSupervisory:
		return uxsvocab.LinkTierMeshSupervisory, nil
	default:
		return "", newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("link tier %d out of range [0, 3]", t))
	}
}

// VehicleDomain encodes bits [15:12] of the 0xB1 HEARTBEAT.custom_mode word.
type VehicleDomain uint8

const (
	DomainQuadcopter  VehicleDomain = 0
	DomainRover       VehicleDomain = 1
	DomainSurfaceBoat VehicleDomain = 2
)

// Valid reports whether d is a defined 0xB1 VehicleDomain value (0..2).
func (d VehicleDomain) Valid() bool {
	return d <= DomainSurfaceBoat
}

// VocabString maps VehicleDomain onto its canonical uxsvocab kebab-case string.
func (d VehicleDomain) VocabString() (string, error) {
	switch d {
	case DomainQuadcopter:
		return uxsvocab.DomainQuadcopter, nil
	case DomainRover:
		return uxsvocab.DomainRover, nil
	case DomainSurfaceBoat:
		return uxsvocab.DomainSurfaceBoat, nil
	default:
		return "", newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("vehicle domain %d out of range [0, 2]", d))
	}
}

// MAVType returns the standard minimal.xml MAV_TYPE required for VehicleDomain.
func (d VehicleDomain) MAVType() (uint8, error) {
	switch d {
	case DomainQuadcopter:
		return MAVTypeQuadrotor, nil
	case DomainRover:
		return MAVTypeGroundRover, nil
	case DomainSurfaceBoat:
		return MAVTypeSurfaceBoat, nil
	default:
		return 0, newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("vehicle domain %d out of range [0, 2]", d))
	}
}

// CompanionMode encodes bits [11:8] of the 0xB1 HEARTBEAT.custom_mode word.
type CompanionMode uint8

const (
	ModeDisarmed           CompanionMode = 0
	ModeTeleopVelocity     CompanionMode = 1
	ModeAutonomousVelocity CompanionMode = 2
	ModeHoldPosition       CompanionMode = 3
)

// Valid reports whether m is a defined 0xB1 CompanionMode value (0..3).
func (m CompanionMode) Valid() bool {
	return m <= ModeHoldPosition
}

// VocabString maps CompanionMode onto its canonical uxsvocab kebab-case string.
func (m CompanionMode) VocabString() (string, error) {
	switch m {
	case ModeDisarmed:
		return uxsvocab.ModeDisarmed, nil
	case ModeTeleopVelocity:
		return uxsvocab.ModeTeleopVelocity, nil
	case ModeAutonomousVelocity:
		return uxsvocab.ModeAutonomousVelocity, nil
	case ModeHoldPosition:
		return uxsvocab.ModeHoldPosition, nil
	default:
		return "", newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("companion mode %d out of range [0, 3]", m))
	}
}

// ControlAuthority encodes bits [7:0] of the 0xB1 HEARTBEAT.custom_mode word.
type ControlAuthority uint8

const (
	AuthorityDirectCRSFOverride  ControlAuthority = 0
	AuthorityCompanionTeleop     ControlAuthority = 1
	AuthorityCompanionAutonomous ControlAuthority = 2
	AuthorityMCUFailsafe         ControlAuthority = 3
)

// Valid reports whether a is a defined 0xB1 ControlAuthority value (0..3).
func (a ControlAuthority) Valid() bool {
	return a <= AuthorityMCUFailsafe
}

// VocabString maps ControlAuthority onto its canonical uxsvocab kebab-case string.
func (a ControlAuthority) VocabString() (string, error) {
	switch a {
	case AuthorityDirectCRSFOverride:
		return uxsvocab.AuthorityDirectCRSFOverride, nil
	case AuthorityCompanionTeleop:
		return uxsvocab.AuthorityCompanionTeleop, nil
	case AuthorityCompanionAutonomous:
		return uxsvocab.AuthorityCompanionAutonomous, nil
	case AuthorityMCUFailsafe:
		return uxsvocab.AuthorityMCUFailsafe, nil
	default:
		return "", newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("control authority %d out of range [0, 3]", a))
	}
}

// HealthFlags aliases uxsvocab.HealthFlags so decoded 0xB1 flags flow directly
// into SKG VehicleSnapshot structs without field-by-field copying.
type HealthFlags = uxsvocab.HealthFlags

// CustomMode represents the unpacked 32-bit bbstack 0xB1 HEARTBEAT.custom_mode word:
//
//	[31:24] = 0xB1 (BBStackMagic)
//	[23:20] = LinkTier (0..3)
//	[19:16] = HealthFlags (bit16=clock_sync_locked, bit17=teleop_deadman_clamped, bit18=crsf_link_healthy, bit19=companion_trunk_healthy)
//	[15:12] = VehicleDomain (0..2)
//	[11:8]  = CompanionMode (0..3)
//	[7:0]   = ControlAuthority (0..3)
type CustomMode struct {
	LinkTier         LinkTier
	HealthFlags      HealthFlags
	Domain           VehicleDomain
	CompanionMode    CompanionMode
	ControlAuthority ControlAuthority
}

// EncodeCustomMode packs cm into a deterministic 32-bit 0xB1 custom_mode word,
// returning an error if any enum field is outside its defined range.
func EncodeCustomMode(cm CustomMode) (uint32, error) {
	if !cm.LinkTier.Valid() {
		return 0, newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("link tier %d out of range [0, 3]", cm.LinkTier))
	}
	if !cm.Domain.Valid() {
		return 0, newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("vehicle domain %d out of range [0, 2]", cm.Domain))
	}
	if !cm.CompanionMode.Valid() {
		return 0, newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("companion mode %d out of range [0, 3]", cm.CompanionMode))
	}
	if !cm.ControlAuthority.Valid() {
		return 0, newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("control authority %d out of range [0, 3]", cm.ControlAuthority))
	}

	var flagsNibble uint8
	if cm.HealthFlags.ClockSyncLocked {
		flagsNibble |= flagBitClockSyncLocked
	}
	if cm.HealthFlags.TeleopDeadmanClamped {
		flagsNibble |= flagBitTeleopDeadmanClamped
	}
	if cm.HealthFlags.CRSFLinkHealthy {
		flagsNibble |= flagBitCRSFLinkHealthy
	}
	if cm.HealthFlags.CompanionTrunkHealthy {
		flagsNibble |= flagBitCompanionTrunkHealthy
	}

	word := uint32(BBStackMagic)<<24 |
		uint32(cm.LinkTier&0x0F)<<20 |
		uint32(flagsNibble&0x0F)<<16 |
		uint32(cm.Domain&0x0F)<<12 |
		uint32(cm.CompanionMode&0x0F)<<8 |
		uint32(cm.ControlAuthority)
	return word, nil
}

// DecodeCustomMode unpacks a 32-bit HEARTBEAT.custom_mode word under the 0xB1
// contract, returning a typed *DecodeError without panicking on non-0xB1 magic
// bytes or out-of-range enum nibbles/bytes.
func DecodeCustomMode(word uint32) (CustomMode, error) {
	magic := uint8(word >> 24)
	if magic != BBStackMagic {
		return CustomMode{}, newDecodeError(
			ErrNotBBStackHeartbeat,
			fmt.Sprintf("custom_mode magic 0x%02X != 0x%02X", magic, BBStackMagic),
		)
	}

	linkTier := LinkTier((word >> 20) & 0x0F)
	if !linkTier.Valid() {
		return CustomMode{}, newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("link tier nibble %d out of range [0, 3]", linkTier))
	}

	flagsNibble := uint8((word >> 16) & 0x0F)
	flags := HealthFlags{
		ClockSyncLocked:       flagsNibble&flagBitClockSyncLocked != 0,
		TeleopDeadmanClamped:  flagsNibble&flagBitTeleopDeadmanClamped != 0,
		CRSFLinkHealthy:       flagsNibble&flagBitCRSFLinkHealthy != 0,
		CompanionTrunkHealthy: flagsNibble&flagBitCompanionTrunkHealthy != 0,
	}

	domain := VehicleDomain((word >> 12) & 0x0F)
	if !domain.Valid() {
		return CustomMode{}, newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("vehicle domain nibble %d out of range [0, 2]", domain))
	}

	mode := CompanionMode((word >> 8) & 0x0F)
	if !mode.Valid() {
		return CustomMode{}, newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("companion mode nibble %d out of range [0, 3]", mode))
	}

	authority := ControlAuthority(word & 0xFF)
	if !authority.Valid() {
		return CustomMode{}, newDecodeError(ErrInvalidCustomMode, fmt.Sprintf("control authority byte %d out of range [0, 3]", authority))
	}

	return CustomMode{
		LinkTier:         linkTier,
		HealthFlags:      flags,
		Domain:           domain,
		CompanionMode:    mode,
		ControlAuthority: authority,
	}, nil
}

// VehicleHeartbeat holds the validated state decoded from a strict 0xB1 vehicle
// HEARTBEAT (#0) frame (`D4 Option A`).
type VehicleHeartbeat struct {
	Armed          bool
	BaseMode       uint8
	SystemStatus   uint8
	MAVLinkVersion uint8
	Custom         CustomMode
}

// DecodeHeartbeat validates a HEARTBEAT (#0) payload against the strict 0xB1
// vehicle envelope (`D4 Option A`):
//  1. custom_mode[31:24] == 0xB1 (non-0xB1 returns ErrNotBBStackHeartbeat)
//  2. base_mode carries MAV_MODE_FLAG_CUSTOM_MODE_ENABLED
//  3. autopilot == MAV_AUTOPILOT_GENERIC (0)
//  4. custom_mode enum fields are valid
//  5. HEARTBEAT.type matches CustomMode.Domain (2<->Quadcopter, 10<->Rover, 11<->SurfaceBoat)
func DecodeHeartbeat(hb Heartbeat) (VehicleHeartbeat, error) {
	cm, err := DecodeCustomMode(hb.CustomMode)
	if err != nil {
		return VehicleHeartbeat{}, err
	}
	if hb.BaseMode&ModeFlagCustomModeEnabled == 0 {
		return VehicleHeartbeat{}, newDecodeError(
			ErrInvalidHeartbeatEnvelope,
			fmt.Sprintf("base_mode 0x%02X missing MAV_MODE_FLAG_CUSTOM_MODE_ENABLED", hb.BaseMode),
		)
	}
	if hb.Autopilot != AutopilotGeneric {
		return VehicleHeartbeat{}, newDecodeError(
			ErrInvalidHeartbeatEnvelope,
			fmt.Sprintf("autopilot %d != MAV_AUTOPILOT_GENERIC (%d)", hb.Autopilot, AutopilotGeneric),
		)
	}
	wantType, err := cm.Domain.MAVType()
	if err != nil {
		return VehicleHeartbeat{}, err
	}
	if hb.Type != wantType {
		return VehicleHeartbeat{}, newDecodeError(
			ErrInvalidHeartbeatEnvelope,
			fmt.Sprintf("heartbeat type %d does not match domain %d (expected MAV_TYPE %d)", hb.Type, cm.Domain, wantType),
		)
	}

	return VehicleHeartbeat{
		Armed:          hb.BaseMode&ModeFlagSafetyArmed != 0,
		BaseMode:       hb.BaseMode,
		SystemStatus:   hb.SystemStatus,
		MAVLinkVersion: hb.MAVLinkVersion,
		Custom:         cm,
	}, nil
}

// EncodeVehicleHeartbeat builds a canonical 0xB1 vehicle Heartbeat (#0) payload.
func EncodeVehicleHeartbeat(vh VehicleHeartbeat) (Heartbeat, error) {
	word, err := EncodeCustomMode(vh.Custom)
	if err != nil {
		return Heartbeat{}, err
	}
	mavType, err := vh.Custom.Domain.MAVType()
	if err != nil {
		return Heartbeat{}, err
	}
	baseMode := vh.BaseMode | ModeFlagCustomModeEnabled
	if vh.Armed {
		baseMode |= ModeFlagSafetyArmed
	} else {
		baseMode &^= ModeFlagSafetyArmed
	}
	ver := vh.MAVLinkVersion
	if ver == 0 {
		ver = MAVLinkVersionV2
	}
	return Heartbeat{
		Type:           mavType,
		Autopilot:      AutopilotGeneric,
		BaseMode:       baseMode,
		CustomMode:     word,
		SystemStatus:   vh.SystemStatus,
		MAVLinkVersion: ver,
	}, nil
}
