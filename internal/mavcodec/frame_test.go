package mavcodec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"
)

// crc16MCRF4XX computes ITU-T X.25 / MCRF4XX CRC-16 independently from gomavlib
// as a clean-room test oracle for MAVLink v2 wire bytes.
func crc16MCRF4XX(body []byte, crcExtra byte) uint16 {
	crc := uint16(0xFFFF)
	accumulate := func(b byte) {
		tmp := b ^ byte(crc&0xFF)
		tmp ^= tmp << 4
		crc = (crc >> 8) ^ (uint16(tmp) << 8) ^ (uint16(tmp) << 3) ^ (uint16(tmp) >> 4)
	}
	for _, b := range body {
		accumulate(b)
	}
	accumulate(crcExtra)
	return crc
}

// spec: mavlink-ingest / Clean-room common.xml MAVLink v2 frame codec and zero-GPLv3 dependency guard
func TestFrameCodecAllNineMessagesAndWireOracle(t *testing.T) {
	hbMsg, err := EncodeVehicleHeartbeat(VehicleHeartbeat{
		Armed:        true,
		SystemStatus: 4,
		Custom: CustomMode{
			LinkTier:         LinkTierMeshHighRate,
			HealthFlags:      HealthFlags{ClockSyncLocked: true, CRSFLinkHealthy: true, CompanionTrunkHealthy: true},
			Domain:           DomainSurfaceBoat,
			CompanionMode:    ModeTeleopVelocity,
			ControlAuthority: AuthorityCompanionTeleop,
		},
	})
	if err != nil {
		t.Fatalf("EncodeVehicleHeartbeat: %v", err)
	}

	// Independent MAVLink v2 wire-byte oracle for #0 HEARTBEAT (CRC_EXTRA = 50, wire field order: custom_mode u32, type u8, autopilot u8, base_mode u8, system_status u8, mavlink_version u8).
	var wantHBPayload [9]byte
	binary.LittleEndian.PutUint32(wantHBPayload[0:4], hbMsg.CustomMode)
	wantHBPayload[4] = hbMsg.Type
	wantHBPayload[5] = hbMsg.Autopilot
	wantHBPayload[6] = hbMsg.BaseMode
	wantHBPayload[7] = hbMsg.SystemStatus
	wantHBPayload[8] = hbMsg.MAVLinkVersion

	var wantHBFrame []byte
	wantHBFrame = append(wantHBFrame,
		0xFD,    // V2 magic
		9,       // payload length (mavlink_version = 3 is non-zero, so no V2 truncation)
		0,       // incompat_flags
		0,       // compat_flags
		17,      // seq
		42,      // sysid
		1,       // compid
		0, 0, 0, // msgid = 0 (24-bit LE)
	)
	wantHBFrame = append(wantHBFrame, wantHBPayload[:]...)
	hbCRC := crc16MCRF4XX(wantHBFrame[1:], 50)
	wantHBFrame = binary.LittleEndian.AppendUint16(wantHBFrame, hbCRC)

	gotHBBytes, err := EncodeFrame(Frame{
		Sequence:    17,
		SystemID:    42,
		ComponentID: CompIDAutopilot1,
		Message:     hbMsg,
	})
	if err != nil {
		t.Fatalf("EncodeFrame(Heartbeat): %v", err)
	}
	if !bytes.Equal(gotHBBytes, wantHBFrame) {
		t.Fatalf("Heartbeat wire bytes = %x, want oracle %x", gotHBBytes, wantHBFrame)
	}

	cases := []struct {
		name string
		in   Frame
	}{
		{
			name: "#0 HEARTBEAT",
			in: Frame{
				Sequence:    0,
				SystemID:    1,
				ComponentID: CompIDAutopilot1,
				Message:     hbMsg,
			},
		},
		{
			name: "#1 SYS_STATUS (including v2 extended sensor bitmaps)",
			in: Frame{
				Sequence:    11,
				SystemID:    2,
				ComponentID: CompIDAutopilot1,
				Message: SysStatus{
					SensorsPresent:         0x0000FC1F,
					SensorsEnabled:         0x0000FC1F,
					SensorsHealth:          0x0000FC1F,
					Load:                   985,
					VoltageBattery:         16800,
					CurrentBattery:         1420,
					BatteryRemaining:       84,
					DropRateComm:           0,
					ErrorsComm:             3,
					ErrorsCount1:           1,
					ErrorsCount2:           1390,
					ErrorsCount3:           740,
					ErrorsCount4:           0,
					SensorsPresentExtended: 0x00000005,
					SensorsEnabledExtended: 0x00000005,
					SensorsHealthExtended:  0x00000001,
				},
			},
		},
		{
			name: "#31 ATTITUDE_QUATERNION (including v2 repr_offset_q)",
			in: Frame{
				Sequence:    31,
				SystemID:    12,
				ComponentID: CompIDAutopilot1,
				Message: AttitudeQuaternion{
					TimeBootMs:  124500,
					Q1:          0.70710677,
					Q2:          0.01,
					Q3:          -0.02,
					Q4:          0.70710677,
					Rollspeed:   0.05,
					Pitchspeed:  -0.03,
					Yawspeed:    0.22,
					ReprOffsetQ: [4]float32{1, 0, 0, 0},
				},
			},
		},
		{
			name: "#32 LOCAL_POSITION_NED (USV crab-angle drift)",
			in: Frame{
				Sequence:    32,
				SystemID:    77,
				ComponentID: CompIDAutopilot1,
				Message: LocalPositionNED{
					TimeBootMs: 124550,
					X:          48.25,
					Y:          -19.5,
					Z:          0.12,
					Vx:         3.1,
					Vy:         -0.85,
					Vz:         0.0,
				},
			},
		},
		{
			name: "#33 GLOBAL_POSITION_INT",
			in: Frame{
				Sequence:    33,
				SystemID:    108,
				ComponentID: CompIDAutopilot1,
				Message: GlobalPositionInt{
					TimeBootMs:  124600,
					Lat:         327157360,
					Lon:         -1171610870,
					Alt:         14250,
					RelativeAlt: 1200,
					Vx:          310,
					Vy:          -85,
					Vz:          0,
					Hdg:         27450,
				},
			},
		},
		{
			name: "#76 COMMAND_LONG",
			in: Frame{
				Sequence:    76,
				SystemID:    254,
				ComponentID: CompIDAutopilot1,
				Message: CommandLong{
					TargetSystem:    12,
					TargetComponent: CompIDAutopilot1,
					Command:         400, // MAV_CMD_COMPONENT_ARM_DISARM
					Confirmation:    0,
					Param1:          1.0,
					Param2:          0,
					Param3:          0,
					Param4:          0,
					Param5:          0,
					Param6:          0,
					Param7:          0,
				},
			},
		},
		{
			name: "#77 COMMAND_ACK (including v2 progress/result_param2/target IDs)",
			in: Frame{
				Sequence:    77,
				SystemID:    12,
				ComponentID: CompIDAutopilot1,
				Message: CommandAck{
					Command:         400,
					Result:          0, // MAV_RESULT_ACCEPTED
					Progress:        100,
					ResultParam2:    -7,
					TargetSystem:    254,
					TargetComponent: CompIDAutopilot1,
				},
			},
		},
		{
			name: "#84 SET_POSITION_TARGET_LOCAL_NED (type_mask 0x07C7 velocity + yaw_rate)",
			in: Frame{
				Sequence:    84,
				SystemID:    254,
				ComponentID: CompIDAutopilot1,
				Message: SetPositionTargetLocalNED{
					TimeBootMs:      124650,
					TargetSystem:    77,
					TargetComponent: CompIDAutopilot1,
					CoordinateFrame: 1, // MAV_FRAME_LOCAL_NED
					TypeMask:        0x07C7,
					X:               0,
					Y:               0,
					Z:               0,
					Vx:              2.5,
					Vy:              -0.4,
					Vz:              0.0,
					Afx:             0,
					Afy:             0,
					Afz:             0,
					Yaw:             0,
					YawRate:         0.35,
				},
			},
		},
		{
			name: "#253 STATUSTEXT (50-byte exact boundary + chunk metadata)",
			in: Frame{
				Sequence:    253,
				SystemID:    254,
				ComponentID: CompIDAutopilot1,
				Message: StatusText{
					Severity: 4, // MAV_SEVERITY_WARNING
					Text:     strings.Repeat("X", MaxStatusTextBytes),
					ID:       9,
					ChunkSeq: 1,
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := EncodeFrame(tc.in)
			if err != nil {
				t.Fatalf("EncodeFrame: %v", err)
			}
			if len(wire) < 12 || wire[0] != V2MagicByte {
				t.Fatalf("wire frame invalid header: %x", wire)
			}

			decoded, err := DecodeFrame(wire)
			if err != nil {
				t.Fatalf("DecodeFrame: %v", err)
			}
			if !framesEqualBitwise(decoded, tc.in) {
				t.Fatalf("round-trip mismatch:\ngot:  %+v\nwant: %+v", decoded, tc.in)
			}
		})
	}
}

// spec: mavlink-ingest / Clean-room common.xml MAVLink v2 frame codec and zero-GPLv3 dependency guard
func TestFrameCodecErrorPaths(t *testing.T) {
	validWire, err := EncodeFrame(Frame{
		Sequence:    5,
		SystemID:    1,
		ComponentID: CompIDAutopilot1,
		Message: LocalPositionNED{
			TimeBootMs: 500,
			X:          1.0,
			Y:          2.0,
			Z:          -3.0,
		},
	})
	if err != nil {
		t.Fatalf("EncodeFrame baseline: %v", err)
	}

	// 1. Empty slice.
	if _, err := DecodeFrame(nil); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("DecodeFrame(nil) err = %v, want ErrInvalidFrame", err)
	}

	// 2. Legacy MAVLink v1 (0xFE) magic and arbitrary non-v2 magic.
	v1Packet := []byte{V1MagicByte, 9, 0, 1, 1, 0, 0, 0, 0, 0, 2, 0, 1, 4, 3, 0x00, 0x00}
	if _, err := DecodeFrame(v1Packet); !errors.Is(err, ErrNonV2Frame) {
		t.Fatalf("DecodeFrame(v1 0xFE) err = %v, want ErrNonV2Frame", err)
	}
	if _, err := DecodeFrame([]byte{0x42, 0x00}); !errors.Is(err, ErrNonV2Frame) {
		t.Fatalf("DecodeFrame(0x42) err = %v, want ErrNonV2Frame", err)
	}

	// 3. Truncated packets at every cut point.
	for cut := 1; cut < len(validWire); cut++ {
		if _, err := DecodeFrame(validWire[:cut]); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("DecodeFrame(truncated len=%d) err = %v, want ErrInvalidFrame", cut, err)
		}
	}

	// 4. Corrupted X.25 CRC byte.
	badCRC := bytes.Clone(validWire)
	badCRC[len(badCRC)-1] ^= 0xFF
	if _, err := DecodeFrame(badCRC); !errors.Is(err, ErrBadChecksum) {
		t.Fatalf("DecodeFrame(corrupted CRC) err = %v, want ErrBadChecksum", err)
	}

	// 5. Trailing unread bytes after valid frame.
	withTrailing := append(bytes.Clone(validWire), 0x99)
	if _, err := DecodeFrame(withTrailing); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("DecodeFrame(trailing byte) err = %v, want ErrInvalidFrame", err)
	}

	// 6. Unverified V2 signed incompatibility flag (0x01).
	signedPacket := []byte{
		V2MagicByte, 0, 0x01, 0, 1, 1, 1, 0, 0, 0,
		0x00, 0x00, // checksum
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // 13-byte signature trailer
	}
	if _, err := DecodeFrame(signedPacket); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("DecodeFrame(signed flag 0x01) err = %v, want ErrInvalidFrame", err)
	}

	// 7. Zero sysid or compid in V2 header.
	zeroSysID := bytes.Clone(validWire)
	zeroSysID[5] = 0
	if _, err := DecodeFrame(zeroSysID); err == nil {
		t.Fatal("DecodeFrame(sysid=0) succeeded, want error")
	}

	// 8. Unsupported message IDs (#24 GPS_RAW_INT, #30 ATTITUDE, #75 COMMAND_INT, #9999).
	for _, msgID := range []uint32{24, 30, 75, 9999} {
		pkt := []byte{
			V2MagicByte,
			2,    // payload len
			0, 0, // flags
			1, 1, 1, // seq, sysid, compid
			byte(msgID), byte(msgID >> 8), byte(msgID >> 16),
			0xAA, 0xBB, // payload
			0x12, 0x34, // checksum
		}
		_, err := DecodeFrame(pkt)
		if !errors.Is(err, ErrUnsupportedMessage) {
			t.Fatalf("DecodeFrame(unsupported #%d) err = %v, want ErrUnsupportedMessage", msgID, err)
		}
		var decErr *DecodeError
		if !errors.As(err, &decErr) || decErr.MessageID != msgID {
			t.Fatalf("DecodeFrame(unsupported #%d) DecodeError = %+v, want MessageID=%d", msgID, decErr, msgID)
		}
	}

	// 9. EncodeFrame validation errors.
	encodeRejects := []Frame{
		{SystemID: 0, ComponentID: 1, Message: Heartbeat{}},
		{SystemID: 1, ComponentID: 0, Message: Heartbeat{}},
		{SystemID: 1, ComponentID: 1, Message: nil},
		{SystemID: 1, ComponentID: 1, Message: StatusText{Severity: 3, Text: strings.Repeat("A", MaxStatusTextBytes+1)}},
		{SystemID: 1, ComponentID: 1, Message: StatusText{Severity: 3, Text: "bad\x00nul"}},
	}
	for i, bad := range encodeRejects {
		if _, err := EncodeFrame(bad); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("EncodeFrame(invalid[%d]) err = %v, want ErrInvalidFrame", i, err)
		}
	}
}

// spec: mavlink-ingest / Clean-room common.xml MAVLink v2 frame codec and zero-GPLv3 dependency guard
func FuzzDecodeFrame(f *testing.F) {
	seedMessages := []Message{
		Heartbeat{Type: MAVTypeQuadrotor, Autopilot: AutopilotGeneric, BaseMode: ModeFlagCustomModeEnabled, CustomMode: 0xB1000000, MAVLinkVersion: MAVLinkVersionV2},
		SysStatus{Load: 500, VoltageBattery: 14800, ErrorsComm: 1},
		AttitudeQuaternion{TimeBootMs: 10, Q1: 1},
		LocalPositionNED{TimeBootMs: 20, X: 1, Vy: -2},
		GlobalPositionInt{TimeBootMs: 30, Lat: 100, Lon: -200, Hdg: 9000},
		CommandLong{TargetSystem: 1, TargetComponent: 1, Command: 400, Param1: 1},
		CommandAck{Command: 400, Result: 0, Progress: 100},
		SetPositionTargetLocalNED{TargetSystem: 1, TargetComponent: 1, CoordinateFrame: 1, TypeMask: 0x07C7, Vx: 1.5},
		StatusText{Severity: 6, Text: "bbnav trunk locked"},
	}
	for i, msg := range seedMessages {
		wire, err := EncodeFrame(Frame{Sequence: uint8(i), SystemID: 1, ComponentID: 1, Message: msg})
		if err != nil {
			f.Fatalf("seed EncodeFrame(%d): %v", i, err)
		}
		f.Add(wire)
	}
	f.Add([]byte{})
	f.Add([]byte{0xFE, 0x09, 0x00, 0x01, 0x01, 0x00})
	f.Add([]byte{0xFD, 0x01, 0x00, 0x00, 0x01, 0x01, 0x01, 0x18, 0x00, 0x00, 0x01, 0x00, 0x00})

	f.Fuzz(func(t *testing.T, raw []byte) {
		decoded, err := DecodeFrame(raw)
		if err != nil {
			var decErr *DecodeError
			if !errors.As(err, &decErr) {
				t.Fatalf("DecodeFrame error %T is not *DecodeError: %v", err, err)
			}
			return
		}
		reencoded, err := EncodeFrame(decoded)
		if err != nil {
			t.Fatalf("EncodeFrame(%+v) failed on decoded frame: %v", decoded, err)
		}
		redecoded, err := DecodeFrame(reencoded)
		if err != nil {
			t.Fatalf("DecodeFrame(reencoded) failed: %v", err)
		}
		if !framesEqualBitwise(redecoded, decoded) {
			t.Fatalf("round-trip mismatch:\ngot:  %+v\nwant: %+v", redecoded, decoded)
		}
	})
}

func framesEqualBitwise(a, b Frame) bool {
	if a.Sequence != b.Sequence || a.SystemID != b.SystemID || a.ComponentID != b.ComponentID {
		return false
	}
	switch ma := a.Message.(type) {
	case Heartbeat:
		mb, ok := b.Message.(Heartbeat)
		return ok && ma == mb
	case SysStatus:
		mb, ok := b.Message.(SysStatus)
		return ok && ma == mb
	case AttitudeQuaternion:
		mb, ok := b.Message.(AttitudeQuaternion)
		return ok && ma.TimeBootMs == mb.TimeBootMs &&
			f32Eq(ma.Q1, mb.Q1) && f32Eq(ma.Q2, mb.Q2) && f32Eq(ma.Q3, mb.Q3) && f32Eq(ma.Q4, mb.Q4) &&
			f32Eq(ma.Rollspeed, mb.Rollspeed) && f32Eq(ma.Pitchspeed, mb.Pitchspeed) && f32Eq(ma.Yawspeed, mb.Yawspeed) &&
			f32Eq(ma.ReprOffsetQ[0], mb.ReprOffsetQ[0]) && f32Eq(ma.ReprOffsetQ[1], mb.ReprOffsetQ[1]) &&
			f32Eq(ma.ReprOffsetQ[2], mb.ReprOffsetQ[2]) && f32Eq(ma.ReprOffsetQ[3], mb.ReprOffsetQ[3])
	case LocalPositionNED:
		mb, ok := b.Message.(LocalPositionNED)
		return ok && ma.TimeBootMs == mb.TimeBootMs &&
			f32Eq(ma.X, mb.X) && f32Eq(ma.Y, mb.Y) && f32Eq(ma.Z, mb.Z) &&
			f32Eq(ma.Vx, mb.Vx) && f32Eq(ma.Vy, mb.Vy) && f32Eq(ma.Vz, mb.Vz)
	case GlobalPositionInt:
		mb, ok := b.Message.(GlobalPositionInt)
		return ok && ma == mb
	case CommandLong:
		mb, ok := b.Message.(CommandLong)
		return ok && ma.TargetSystem == mb.TargetSystem && ma.TargetComponent == mb.TargetComponent &&
			ma.Command == mb.Command && ma.Confirmation == mb.Confirmation &&
			f32Eq(ma.Param1, mb.Param1) && f32Eq(ma.Param2, mb.Param2) && f32Eq(ma.Param3, mb.Param3) &&
			f32Eq(ma.Param4, mb.Param4) && f32Eq(ma.Param5, mb.Param5) && f32Eq(ma.Param6, mb.Param6) &&
			f32Eq(ma.Param7, mb.Param7)
	case CommandAck:
		mb, ok := b.Message.(CommandAck)
		return ok && ma == mb
	case SetPositionTargetLocalNED:
		mb, ok := b.Message.(SetPositionTargetLocalNED)
		return ok && ma.TimeBootMs == mb.TimeBootMs && ma.TargetSystem == mb.TargetSystem &&
			ma.TargetComponent == mb.TargetComponent && ma.CoordinateFrame == mb.CoordinateFrame &&
			ma.TypeMask == mb.TypeMask &&
			f32Eq(ma.X, mb.X) && f32Eq(ma.Y, mb.Y) && f32Eq(ma.Z, mb.Z) &&
			f32Eq(ma.Vx, mb.Vx) && f32Eq(ma.Vy, mb.Vy) && f32Eq(ma.Vz, mb.Vz) &&
			f32Eq(ma.Afx, mb.Afx) && f32Eq(ma.Afy, mb.Afy) && f32Eq(ma.Afz, mb.Afz) &&
			f32Eq(ma.Yaw, mb.Yaw) && f32Eq(ma.YawRate, mb.YawRate)
	case StatusText:
		mb, ok := b.Message.(StatusText)
		return ok && ma == mb
	default:
		return false
	}
}

func f32Eq(a, b float32) bool {
	if math.IsNaN(float64(a)) && math.IsNaN(float64(b)) {
		return true
	}
	return math.Float32bits(a) == math.Float32bits(b)
}
