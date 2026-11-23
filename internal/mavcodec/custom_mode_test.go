package mavcodec

import (
	"errors"
	"testing"

	"github.com/boxbotix/bbops/internal/uxsvocab"
)

func referenceEncodeCustomMode(tier, flags, domain, mode, auth uint8) uint32 {
	return uint32(0xB1)<<24 |
		uint32(tier&0x0F)<<20 |
		uint32(flags&0x0F)<<16 |
		uint32(domain&0x0F)<<12 |
		uint32(mode&0x0F)<<8 |
		uint32(auth)
}

func referenceIsValidCustomMode(word uint32) bool {
	if uint8(word>>24) != 0xB1 {
		return false
	}
	if ((word >> 20) & 0x0F) > 3 {
		return false
	}
	if ((word >> 12) & 0x0F) > 2 {
		return false
	}
	if ((word >> 8) & 0x0F) > 3 {
		return false
	}
	if (word & 0xFF) > 3 {
		return false
	}
	return true
}

// spec: mavlink-ingest / bbstack 0xB1 HEARTBEAT custom_mode bitfield codec
func TestCustomModeExhaustiveRoundTripAndVocabMapping(t *testing.T) {
	count := 0
	for tier := uint8(0); tier <= 3; tier++ {
		for flags := uint8(0); flags <= 0x0F; flags++ {
			for domain := uint8(0); domain <= 2; domain++ {
				for mode := uint8(0); mode <= 3; mode++ {
					for auth := uint8(0); auth <= 3; auth++ {
						count++
						in := CustomMode{
							LinkTier: LinkTier(tier),
							HealthFlags: HealthFlags{
								ClockSyncLocked:       flags&0x01 != 0,
								TeleopDeadmanClamped:  flags&0x02 != 0,
								CRSFLinkHealthy:       flags&0x04 != 0,
								CompanionTrunkHealthy: flags&0x08 != 0,
							},
							Domain:           VehicleDomain(domain),
							CompanionMode:    CompanionMode(mode),
							ControlAuthority: ControlAuthority(auth),
						}

						gotWord, err := EncodeCustomMode(in)
						if err != nil {
							t.Fatalf("EncodeCustomMode(%+v): %v", in, err)
						}
						wantWord := referenceEncodeCustomMode(tier, flags, domain, mode, auth)
						if gotWord != wantWord {
							t.Fatalf("EncodeCustomMode(%+v) = 0x%08X, want 0x%08X", in, gotWord, wantWord)
						}
						if uint8(gotWord>>24) != BBStackMagic {
							t.Fatalf("high byte = 0x%02X, want 0x%02X", uint8(gotWord>>24), BBStackMagic)
						}

						out, err := DecodeCustomMode(gotWord)
						if err != nil {
							t.Fatalf("DecodeCustomMode(0x%08X): %v", gotWord, err)
						}
						if out != in {
							t.Fatalf("DecodeCustomMode(0x%08X) = %+v, want %+v", gotWord, out, in)
						}

						domainStr, err := out.Domain.VocabString()
						if err != nil || !uxsvocab.IsValidDomain(domainStr) {
							t.Fatalf("Domain(%d).VocabString() = (%q, %v)", domain, domainStr, err)
						}
						tierStr, err := out.LinkTier.VocabString()
						if err != nil || !uxsvocab.IsValidLinkTier(tierStr) {
							t.Fatalf("LinkTier(%d).VocabString() = (%q, %v)", tier, tierStr, err)
						}
						modeStr, err := out.CompanionMode.VocabString()
						if err != nil || !uxsvocab.IsValidCompanionMode(modeStr) {
							t.Fatalf("CompanionMode(%d).VocabString() = (%q, %v)", mode, modeStr, err)
						}
						authStr, err := out.ControlAuthority.VocabString()
						if err != nil || !uxsvocab.IsValidAuthority(authStr) {
							t.Fatalf("ControlAuthority(%d).VocabString() = (%q, %v)", auth, authStr, err)
						}
					}
				}
			}
		}
	}
	if count != 3072 {
		t.Fatalf("tested %d combinations, want exhaustive 3072", count)
	}
}

// spec: mavlink-ingest / bbstack 0xB1 HEARTBEAT custom_mode bitfield codec
func TestCustomModeAndHeartbeatEnvelopeRejection(t *testing.T) {
	// Non-0xB1 magic words (including GCS custom_mode = 0).
	for _, word := range []uint32{0x00000000, 0xB0000000, 0xB2000000, 0xFF120101} {
		_, err := DecodeCustomMode(word)
		if !errors.Is(err, ErrNotBBStackHeartbeat) {
			t.Fatalf("DecodeCustomMode(0x%08X) err = %v, want ErrNotBBStackHeartbeat", word, err)
		}
		var decErr *DecodeError
		if !errors.As(err, &decErr) {
			t.Fatalf("DecodeCustomMode(0x%08X) err %T is not *DecodeError", word, err)
		}
	}

	// Out-of-range enum nibbles/bytes inside 0xB1 words.
	invalidWords := []struct {
		name string
		word uint32
	}{
		{name: "link tier 4", word: referenceEncodeCustomMode(4, 0, 0, 0, 0)},
		{name: "link tier 15", word: referenceEncodeCustomMode(15, 0, 0, 0, 0)},
		{name: "domain 3", word: referenceEncodeCustomMode(0, 0, 3, 0, 0)},
		{name: "domain 15", word: referenceEncodeCustomMode(0, 0, 15, 0, 0)},
		{name: "companion mode 4", word: referenceEncodeCustomMode(0, 0, 0, 4, 0)},
		{name: "companion mode 15", word: referenceEncodeCustomMode(0, 0, 0, 15, 0)},
		{name: "authority 4", word: referenceEncodeCustomMode(0, 0, 0, 0, 4)},
		{name: "authority 0x10 upper nibble set", word: referenceEncodeCustomMode(0, 0, 0, 0, 0x10)},
		{name: "authority 255", word: referenceEncodeCustomMode(0, 0, 0, 0, 255)},
	}
	for _, tc := range invalidWords {
		if _, err := DecodeCustomMode(tc.word); !errors.Is(err, ErrInvalidCustomMode) {
			t.Fatalf("%s (0x%08X): err = %v, want ErrInvalidCustomMode", tc.name, tc.word, err)
		}
	}

	// Out-of-range enum values on EncodeCustomMode and VocabString/MAVType.
	invalidEncode := []CustomMode{
		{LinkTier: 4},
		{Domain: 3},
		{CompanionMode: 4},
		{ControlAuthority: 4},
	}
	for _, cm := range invalidEncode {
		if _, err := EncodeCustomMode(cm); !errors.Is(err, ErrInvalidCustomMode) {
			t.Fatalf("EncodeCustomMode(%+v) err = %v, want ErrInvalidCustomMode", cm, err)
		}
	}
	if _, err := LinkTier(9).VocabString(); !errors.Is(err, ErrInvalidCustomMode) {
		t.Fatalf("LinkTier(9).VocabString() err = %v", err)
	}
	if _, err := VehicleDomain(9).VocabString(); !errors.Is(err, ErrInvalidCustomMode) {
		t.Fatalf("VehicleDomain(9).VocabString() err = %v", err)
	}
	if _, err := VehicleDomain(9).MAVType(); !errors.Is(err, ErrInvalidCustomMode) {
		t.Fatalf("VehicleDomain(9).MAVType() err = %v", err)
	}
	if _, err := CompanionMode(9).VocabString(); !errors.Is(err, ErrInvalidCustomMode) {
		t.Fatalf("CompanionMode(9).VocabString() err = %v", err)
	}
	if _, err := ControlAuthority(9).VocabString(); !errors.Is(err, ErrInvalidCustomMode) {
		t.Fatalf("ControlAuthority(9).VocabString() err = %v", err)
	}

	// Strict HEARTBEAT envelope (`D4 Option A`) round-trip across armed/disarmed and all 3 domains.
	for _, domain := range []VehicleDomain{DomainQuadcopter, DomainRover, DomainSurfaceBoat} {
		for _, armed := range []bool{false, true} {
			in := VehicleHeartbeat{
				Armed:        armed,
				SystemStatus: 4,
				Custom: CustomMode{
					LinkTier:         LinkTierMeshHighRate,
					HealthFlags:      HealthFlags{ClockSyncLocked: true, CompanionTrunkHealthy: true},
					Domain:           domain,
					CompanionMode:    ModeTeleopVelocity,
					ControlAuthority: AuthorityCompanionTeleop,
				},
			}
			hb, err := EncodeVehicleHeartbeat(in)
			if err != nil {
				t.Fatalf("EncodeVehicleHeartbeat(%+v): %v", in, err)
			}
			got, err := DecodeHeartbeat(hb)
			if err != nil {
				t.Fatalf("DecodeHeartbeat(%+v): %v", hb, err)
			}
			if got.Armed != armed || got.Custom != in.Custom || got.MAVLinkVersion != MAVLinkVersionV2 {
				t.Fatalf("DecodeHeartbeat = %+v, want armed=%v custom=%+v", got, armed, in.Custom)
			}
		}
	}

	// Peer GCS HEARTBEAT (#0) returns ErrNotBBStackHeartbeat cleanly.
	gcsHB := Heartbeat{
		Type:           MAVTypeGCS,
		Autopilot:      8,
		BaseMode:       0,
		CustomMode:     0,
		MAVLinkVersion: MAVLinkVersionV2,
	}
	if _, err := DecodeHeartbeat(gcsHB); !errors.Is(err, ErrNotBBStackHeartbeat) {
		t.Fatalf("DecodeHeartbeat(GCS) err = %v, want ErrNotBBStackHeartbeat", err)
	}

	validUSV, err := EncodeVehicleHeartbeat(VehicleHeartbeat{
		Armed: true,
		Custom: CustomMode{
			LinkTier:         LinkTierSITLLocal,
			Domain:           DomainSurfaceBoat,
			CompanionMode:    ModeHoldPosition,
			ControlAuthority: AuthorityCompanionAutonomous,
		},
	})
	if err != nil {
		t.Fatalf("EncodeVehicleHeartbeat(USV): %v", err)
	}

	envelopeRejects := []struct {
		name   string
		mutate func(*Heartbeat)
	}{
		{
			name:   "missing CUSTOM_MODE_ENABLED",
			mutate: func(h *Heartbeat) { h.BaseMode &^= ModeFlagCustomModeEnabled },
		},
		{
			name:   "non-generic autopilot (ArduPilot=3)",
			mutate: func(h *Heartbeat) { h.Autopilot = 3 },
		},
		{
			name:   "MAV_TYPE_QUADROTOR on SurfaceBoat custom_mode",
			mutate: func(h *Heartbeat) { h.Type = MAVTypeQuadrotor },
		},
		{
			name:   "MAV_TYPE_GENERIC (0) on SurfaceBoat custom_mode",
			mutate: func(h *Heartbeat) { h.Type = 0 },
		},
	}
	for _, tc := range envelopeRejects {
		hb := validUSV
		tc.mutate(&hb)
		if _, err := DecodeHeartbeat(hb); !errors.Is(err, ErrInvalidHeartbeatEnvelope) {
			t.Fatalf("%s: DecodeHeartbeat err = %v, want ErrInvalidHeartbeatEnvelope", tc.name, err)
		}
	}
}

// spec: mavlink-ingest / bbstack 0xB1 HEARTBEAT custom_mode bitfield codec
func FuzzDecodeCustomMode(f *testing.F) {
	seeds := []uint32{
		0x00000000,
		0xFFFFFFFF,
		0xB1000000,
		0xB13F2303,
		0xB1400000,
		0xB1003000,
		0xB1000400,
		0xB1000004,
		0xB1000010,
		0xB0000000,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, word uint32) {
		cm, err := DecodeCustomMode(word)
		wantValid := referenceIsValidCustomMode(word)
		if wantValid && err != nil {
			t.Fatalf("DecodeCustomMode(0x%08X) failed unexpectedly: %v", word, err)
		}
		if !wantValid && err == nil {
			t.Fatalf("DecodeCustomMode(0x%08X) succeeded with %+v, want error", word, cm)
		}
		if err != nil {
			var decErr *DecodeError
			if !errors.As(err, &decErr) {
				t.Fatalf("DecodeCustomMode(0x%08X) err %T is not *DecodeError", word, err)
			}
			return
		}
		reencoded, encErr := EncodeCustomMode(cm)
		if encErr != nil {
			t.Fatalf("EncodeCustomMode(%+v) failed after decode: %v", cm, encErr)
		}
		if reencoded != word {
			t.Fatalf("EncodeCustomMode(DecodeCustomMode(0x%08X)) = 0x%08X", word, reencoded)
		}
	})
}
