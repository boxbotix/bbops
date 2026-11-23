package uxsvocab

import (
	"fmt"
	"strconv"
	"strings"

	semtypes "github.com/c360studio/semengine/pkg/types"
)

const (
	// MinSysID is the lowest valid MAVLink vehicle system ID accepted by bbops.
	MinSysID uint8 = 1
	// MaxSysID is the highest valid MAVLink vehicle system ID (255 is reserved for GCS).
	MaxSysID uint8 = 254

	vehicleSystemSegment = "robotics"
	vehicleDomainSegment = "uxs"
	vehicleTypeSegment   = "vehicle"
)

// ValidateSysID checks that sysid falls in the controllable vehicle range 1..=254.
func ValidateSysID(sysid uint8) error {
	if sysid < MinSysID || sysid > MaxSysID {
		return fmt.Errorf("uxsvocab: sysid %d out of vehicle range [%d, %d]", sysid, MinSysID, MaxSysID)
	}
	return nil
}

// VehicleEntityID constructs and validates the canonical 6-part SemEngine ADR-102
// entity ID `<org>.<platform>.robotics.uxs.vehicle.<sysid>` (<= 256 bytes).
func VehicleEntityID(org, platform string, sysid uint8) (string, error) {
	if err := ValidateSysID(sysid); err != nil {
		return "", err
	}
	if org == "" || strings.Contains(org, ".") {
		return "", fmt.Errorf("uxsvocab: invalid org segment %q", org)
	}
	if platform == "" || strings.Contains(platform, ".") {
		return "", fmt.Errorf("uxsvocab: invalid platform segment %q", platform)
	}

	instance := strconv.Itoa(int(sysid))
	entityID := org + "." + platform + "." + vehicleSystemSegment + "." + vehicleDomainSegment + "." + vehicleTypeSegment + "." + instance
	if len(entityID) > semtypes.MaxEntityIDBytes {
		return "", fmt.Errorf("uxsvocab: entity ID length %d exceeds %d bytes", len(entityID), semtypes.MaxEntityIDBytes)
	}
	if _, err := semtypes.ParseEntityID(entityID); err != nil {
		return "", fmt.Errorf("uxsvocab: invalid entity ID %q: %w", entityID, err)
	}
	return entityID, nil
}
