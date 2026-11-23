package mavcodec

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"github.com/bluenviron/gomavlib/v3/pkg/dialect"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"
	"github.com/bluenviron/gomavlib/v3/pkg/frame"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
)

const (
	// V2MagicByte is the MAVLink v2 start-of-frame marker (0xFD).
	V2MagicByte byte = 0xFD
	// V1MagicByte is the legacy MAVLink v1 start-of-frame marker (0xFE, rejected by bbops).
	V1MagicByte byte = 0xFE

	// Contract message IDs from MIT minimal.xml + common.xml.
	MsgIDHeartbeat                 uint32 = 0
	MsgIDSysStatus                 uint32 = 1
	MsgIDAttitudeQuaternion        uint32 = 31
	MsgIDLocalPositionNED          uint32 = 32
	MsgIDGlobalPositionInt         uint32 = 33
	MsgIDCommandLong               uint32 = 76
	MsgIDCommandAck                uint32 = 77
	MsgIDSetPositionTargetLocalNED uint32 = 84
	MsgIDStatusText                uint32 = 253
)

var contractDialectRW = mustInitContractDialect()

func mustInitContractDialect() *dialect.ReadWriter {
	rw := &dialect.ReadWriter{
		Dialect: &dialect.Dialect{
			Version: 3,
			Messages: []message.Message{
				&common.MessageHeartbeat{},
				&common.MessageSysStatus{},
				&common.MessageAttitudeQuaternion{},
				&common.MessageLocalPositionNed{},
				&common.MessageGlobalPositionInt{},
				&common.MessageCommandLong{},
				&common.MessageCommandAck{},
				&common.MessageSetPositionTargetLocalNed{},
				&common.MessageStatustext{},
			},
		},
	}
	if err := rw.Initialize(); err != nil {
		panic(fmt.Sprintf("mavcodec: initialize common.xml contract dialect: %v", err))
	}
	return rw
}

// Message is the sealed interface implemented by the 9 bbops common.xml MAVLink
// v2 contract message structs.
type Message interface {
	MessageID() uint32
	isContractMessage()
}

// Frame represents a single unsigned MAVLink v2 (0xFD) packet header and typed
// contract message payload.
type Frame struct {
	Sequence    uint8
	SystemID    uint8
	ComponentID uint8
	Message     Message
}

// Heartbeat represents MIT minimal.xml #0 HEARTBEAT.
type Heartbeat struct {
	Type           uint8
	Autopilot      uint8
	BaseMode       uint8
	CustomMode     uint32
	SystemStatus   uint8
	MAVLinkVersion uint8
}

func (Heartbeat) MessageID() uint32  { return MsgIDHeartbeat }
func (Heartbeat) isContractMessage() {}

// SysStatus represents MIT common.xml #1 SYS_STATUS.
type SysStatus struct {
	SensorsPresent         uint32
	SensorsEnabled         uint32
	SensorsHealth          uint32
	Load                   uint16
	VoltageBattery         uint16
	CurrentBattery         int16
	BatteryRemaining       int8
	DropRateComm           uint16
	ErrorsComm             uint16
	ErrorsCount1           uint16
	ErrorsCount2           uint16
	ErrorsCount3           uint16
	ErrorsCount4           uint16
	SensorsPresentExtended uint32
	SensorsEnabledExtended uint32
	SensorsHealthExtended  uint32
}

func (SysStatus) MessageID() uint32  { return MsgIDSysStatus }
func (SysStatus) isContractMessage() {}

// AttitudeQuaternion represents MIT common.xml #31 ATTITUDE_QUATERNION.
type AttitudeQuaternion struct {
	TimeBootMs  uint32
	Q1          float32
	Q2          float32
	Q3          float32
	Q4          float32
	Rollspeed   float32
	Pitchspeed  float32
	Yawspeed    float32
	ReprOffsetQ [4]float32
}

func (AttitudeQuaternion) MessageID() uint32  { return MsgIDAttitudeQuaternion }
func (AttitudeQuaternion) isContractMessage() {}

// LocalPositionNED represents MIT common.xml #32 LOCAL_POSITION_NED.
type LocalPositionNED struct {
	TimeBootMs uint32
	X          float32
	Y          float32
	Z          float32
	Vx         float32
	Vy         float32
	Vz         float32
}

func (LocalPositionNED) MessageID() uint32  { return MsgIDLocalPositionNED }
func (LocalPositionNED) isContractMessage() {}

// GlobalPositionInt represents MIT common.xml #33 GLOBAL_POSITION_INT.
type GlobalPositionInt struct {
	TimeBootMs  uint32
	Lat         int32
	Lon         int32
	Alt         int32
	RelativeAlt int32
	Vx          int16
	Vy          int16
	Vz          int16
	Hdg         uint16
}

func (GlobalPositionInt) MessageID() uint32  { return MsgIDGlobalPositionInt }
func (GlobalPositionInt) isContractMessage() {}

// CommandLong represents MIT common.xml #76 COMMAND_LONG.
type CommandLong struct {
	TargetSystem    uint8
	TargetComponent uint8
	Command         uint16
	Confirmation    uint8
	Param1          float32
	Param2          float32
	Param3          float32
	Param4          float32
	Param5          float32
	Param6          float32
	Param7          float32
}

func (CommandLong) MessageID() uint32  { return MsgIDCommandLong }
func (CommandLong) isContractMessage() {}

// CommandAck represents MIT common.xml #77 COMMAND_ACK.
type CommandAck struct {
	Command         uint16
	Result          uint8
	Progress        uint8
	ResultParam2    int32
	TargetSystem    uint8
	TargetComponent uint8
}

func (CommandAck) MessageID() uint32  { return MsgIDCommandAck }
func (CommandAck) isContractMessage() {}

// SetPositionTargetLocalNED represents MIT common.xml #84 SET_POSITION_TARGET_LOCAL_NED.
type SetPositionTargetLocalNED struct {
	TimeBootMs      uint32
	TargetSystem    uint8
	TargetComponent uint8
	CoordinateFrame uint8
	TypeMask        uint16
	X               float32
	Y               float32
	Z               float32
	Vx              float32
	Vy              float32
	Vz              float32
	Afx             float32
	Afy             float32
	Afz             float32
	Yaw             float32
	YawRate         float32
}

func (SetPositionTargetLocalNED) MessageID() uint32  { return MsgIDSetPositionTargetLocalNED }
func (SetPositionTargetLocalNED) isContractMessage() {}

// StatusText represents MIT common.xml #253 STATUSTEXT.
type StatusText struct {
	Severity uint8
	Text     string
	ID       uint16
	ChunkSeq uint8
}

func (StatusText) MessageID() uint32  { return MsgIDStatusText }
func (StatusText) isContractMessage() {}

// EncodeFrame encodes f into a single MAVLink v2 (0xFD) wire packet using the
// restricted 9-message common.xml dialect.
func EncodeFrame(f Frame) ([]byte, error) {
	if f.SystemID == 0 {
		return nil, newDecodeError(ErrInvalidFrame, "system_id must be in 1..255")
	}
	if f.ComponentID == 0 {
		return nil, newDecodeError(ErrInvalidFrame, "component_id must be in 1..255")
	}
	if f.Message == nil {
		return nil, newDecodeError(ErrInvalidFrame, "nil message")
	}

	wireMsg, err := toWireMessage(f.Message)
	if err != nil {
		return nil, err
	}
	msgRW := contractDialectRW.GetMessage(wireMsg.GetID())
	if msgRW == nil {
		return nil, &DecodeError{
			Kind:      ErrUnsupportedMessage,
			MessageID: wireMsg.GetID(),
			Detail:    fmt.Sprintf("message #%d not in contract dialect", wireMsg.GetID()),
		}
	}

	rawMsg := msgRW.Write(wireMsg, true)
	v2 := &frame.V2Frame{
		SequenceNumber: f.Sequence,
		SystemID:       f.SystemID,
		ComponentID:    f.ComponentID,
		Message:        rawMsg,
	}
	v2.Checksum = v2.GenerateChecksum(msgRW.CRCExtra())

	var buf bytes.Buffer
	w := &frame.Writer{
		ByteWriter: &buf,
		DialectRW:  contractDialectRW,
	}
	if err := w.Initialize(); err != nil {
		return nil, wrapDecodeError(ErrInvalidFrame, wireMsg.GetID(), "initialize frame writer", err)
	}
	if err := w.Write(v2); err != nil {
		return nil, wrapDecodeError(ErrInvalidFrame, wireMsg.GetID(), "write v2 frame", err)
	}
	return buf.Bytes(), nil
}

// DecodeFrame decodes a single MAVLink v2 (0xFD) packet from data into one of
// the 9 common.xml contract messages, returning a typed *DecodeError without
// panicking on malformed frames, v1 frames, bad CRCs, trailing bytes, or
// unsupported message IDs.
func DecodeFrame(data []byte) (Frame, error) {
	if len(data) == 0 {
		return Frame{}, newDecodeError(ErrInvalidFrame, "empty frame")
	}
	if data[0] != V2MagicByte {
		return Frame{}, newDecodeError(ErrNonV2Frame, fmt.Sprintf("magic byte 0x%02X != 0x%02X", data[0], V2MagicByte))
	}
	if len(data) < 12 {
		return Frame{}, newDecodeError(ErrInvalidFrame, fmt.Sprintf("truncated v2 frame length %d < 12", len(data)))
	}
	if data[2] != 0 {
		return Frame{}, newDecodeError(ErrInvalidFrame, fmt.Sprintf("unsupported v2 incompatibility flag 0x%02X", data[2]))
	}

	byteReader := bytes.NewReader(data)
	bufReader := bufio.NewReader(byteReader)
	r := &frame.Reader{
		BufByteReader: bufReader,
		DialectRW:     contractDialectRW,
	}
	if err := r.Initialize(); err != nil {
		return Frame{}, wrapDecodeError(ErrInvalidFrame, 0, "initialize frame reader", err)
	}

	fr, err := r.Read()
	if err != nil {
		if strings.Contains(err.Error(), "wrong checksum") {
			return Frame{}, wrapDecodeError(ErrBadChecksum, 0, "x25 crc verification failed", err)
		}
		return Frame{}, wrapDecodeError(ErrInvalidFrame, 0, "read v2 frame", err)
	}
	if bufReader.Buffered() != 0 || byteReader.Len() != 0 {
		return Frame{}, newDecodeError(ErrInvalidFrame, "unexpected trailing bytes after v2 frame")
	}

	v2, ok := fr.(*frame.V2Frame)
	if !ok {
		return Frame{}, newDecodeError(ErrNonV2Frame, fmt.Sprintf("decoded %T, expected *frame.V2Frame", fr))
	}
	if v2.SystemID == 0 || v2.ComponentID == 0 {
		return Frame{}, newDecodeError(ErrInvalidFrame, fmt.Sprintf("invalid sender header sysid=%d compid=%d", v2.SystemID, v2.ComponentID))
	}

	domainMsg, err := fromWireMessage(v2.Message)
	if err != nil {
		return Frame{}, err
	}
	if data[1] == 0 {
		return Frame{}, &DecodeError{
			Kind:      ErrInvalidFrame,
			MessageID: domainMsg.MessageID(),
			Detail:    fmt.Sprintf("empty v2 payload for non-empty message #%d", domainMsg.MessageID()),
		}
	}
	return Frame{
		Sequence:    v2.SequenceNumber,
		SystemID:    v2.SystemID,
		ComponentID: v2.ComponentID,
		Message:     domainMsg,
	}, nil
}

func derefMessage(msg Message) (Message, error) {
	switch m := msg.(type) {
	case *Heartbeat:
		if m == nil {
			return nil, newDecodeError(ErrInvalidFrame, "typed nil *Heartbeat message")
		}
		return *m, nil
	case *SysStatus:
		if m == nil {
			return nil, newDecodeError(ErrInvalidFrame, "typed nil *SysStatus message")
		}
		return *m, nil
	case *AttitudeQuaternion:
		if m == nil {
			return nil, newDecodeError(ErrInvalidFrame, "typed nil *AttitudeQuaternion message")
		}
		return *m, nil
	case *LocalPositionNED:
		if m == nil {
			return nil, newDecodeError(ErrInvalidFrame, "typed nil *LocalPositionNED message")
		}
		return *m, nil
	case *GlobalPositionInt:
		if m == nil {
			return nil, newDecodeError(ErrInvalidFrame, "typed nil *GlobalPositionInt message")
		}
		return *m, nil
	case *CommandLong:
		if m == nil {
			return nil, newDecodeError(ErrInvalidFrame, "typed nil *CommandLong message")
		}
		return *m, nil
	case *CommandAck:
		if m == nil {
			return nil, newDecodeError(ErrInvalidFrame, "typed nil *CommandAck message")
		}
		return *m, nil
	case *SetPositionTargetLocalNED:
		if m == nil {
			return nil, newDecodeError(ErrInvalidFrame, "typed nil *SetPositionTargetLocalNED message")
		}
		return *m, nil
	case *StatusText:
		if m == nil {
			return nil, newDecodeError(ErrInvalidFrame, "typed nil *StatusText message")
		}
		return *m, nil
	default:
		return msg, nil
	}
}

func toWireMessage(msg Message) (message.Message, error) {
	valMsg, err := derefMessage(msg)
	if err != nil {
		return nil, err
	}
	switch m := valMsg.(type) {
	case Heartbeat:
		return &common.MessageHeartbeat{
			Type:           common.MAV_TYPE(m.Type),
			Autopilot:      common.MAV_AUTOPILOT(m.Autopilot),
			BaseMode:       common.MAV_MODE_FLAG(m.BaseMode),
			CustomMode:     m.CustomMode,
			SystemStatus:   common.MAV_STATE(m.SystemStatus),
			MavlinkVersion: m.MAVLinkVersion,
		}, nil
	case SysStatus:
		return &common.MessageSysStatus{
			OnboardControlSensorsPresent:         common.MAV_SYS_STATUS_SENSOR(m.SensorsPresent),
			OnboardControlSensorsEnabled:         common.MAV_SYS_STATUS_SENSOR(m.SensorsEnabled),
			OnboardControlSensorsHealth:          common.MAV_SYS_STATUS_SENSOR(m.SensorsHealth),
			Load:                                 m.Load,
			VoltageBattery:                       m.VoltageBattery,
			CurrentBattery:                       m.CurrentBattery,
			BatteryRemaining:                     m.BatteryRemaining,
			DropRateComm:                         m.DropRateComm,
			ErrorsComm:                           m.ErrorsComm,
			ErrorsCount1:                         m.ErrorsCount1,
			ErrorsCount2:                         m.ErrorsCount2,
			ErrorsCount3:                         m.ErrorsCount3,
			ErrorsCount4:                         m.ErrorsCount4,
			OnboardControlSensorsPresentExtended: common.MAV_SYS_STATUS_SENSOR_EXTENDED(m.SensorsPresentExtended),
			OnboardControlSensorsEnabledExtended: common.MAV_SYS_STATUS_SENSOR_EXTENDED(m.SensorsEnabledExtended),
			OnboardControlSensorsHealthExtended:  common.MAV_SYS_STATUS_SENSOR_EXTENDED(m.SensorsHealthExtended),
		}, nil
	case AttitudeQuaternion:
		return &common.MessageAttitudeQuaternion{
			TimeBootMs:  m.TimeBootMs,
			Q1:          m.Q1,
			Q2:          m.Q2,
			Q3:          m.Q3,
			Q4:          m.Q4,
			Rollspeed:   m.Rollspeed,
			Pitchspeed:  m.Pitchspeed,
			Yawspeed:    m.Yawspeed,
			ReprOffsetQ: m.ReprOffsetQ,
		}, nil
	case LocalPositionNED:
		return &common.MessageLocalPositionNed{
			TimeBootMs: m.TimeBootMs,
			X:          m.X,
			Y:          m.Y,
			Z:          m.Z,
			Vx:         m.Vx,
			Vy:         m.Vy,
			Vz:         m.Vz,
		}, nil
	case GlobalPositionInt:
		return &common.MessageGlobalPositionInt{
			TimeBootMs:  m.TimeBootMs,
			Lat:         m.Lat,
			Lon:         m.Lon,
			Alt:         m.Alt,
			RelativeAlt: m.RelativeAlt,
			Vx:          m.Vx,
			Vy:          m.Vy,
			Vz:          m.Vz,
			Hdg:         m.Hdg,
		}, nil
	case CommandLong:
		return &common.MessageCommandLong{
			TargetSystem:    m.TargetSystem,
			TargetComponent: m.TargetComponent,
			Command:         common.MAV_CMD(m.Command),
			Confirmation:    m.Confirmation,
			Param1:          m.Param1,
			Param2:          m.Param2,
			Param3:          m.Param3,
			Param4:          m.Param4,
			Param5:          m.Param5,
			Param6:          m.Param6,
			Param7:          m.Param7,
		}, nil
	case CommandAck:
		return &common.MessageCommandAck{
			Command:         common.MAV_CMD(m.Command),
			Result:          common.MAV_RESULT(m.Result),
			Progress:        m.Progress,
			ResultParam2:    m.ResultParam2,
			TargetSystem:    m.TargetSystem,
			TargetComponent: m.TargetComponent,
		}, nil
	case SetPositionTargetLocalNED:
		return &common.MessageSetPositionTargetLocalNed{
			TimeBootMs:      m.TimeBootMs,
			TargetSystem:    m.TargetSystem,
			TargetComponent: m.TargetComponent,
			CoordinateFrame: common.MAV_FRAME(m.CoordinateFrame),
			TypeMask:        common.POSITION_TARGET_TYPEMASK(m.TypeMask),
			X:               m.X,
			Y:               m.Y,
			Z:               m.Z,
			Vx:              m.Vx,
			Vy:              m.Vy,
			Vz:              m.Vz,
			Afx:             m.Afx,
			Afy:             m.Afy,
			Afz:             m.Afz,
			Yaw:             m.Yaw,
			YawRate:         m.YawRate,
		}, nil
	case StatusText:
		if len(m.Text) > MaxStatusTextBytes {
			return nil, &DecodeError{
				Kind:      ErrInvalidFrame,
				MessageID: MsgIDStatusText,
				Detail:    fmt.Sprintf("statustext length %d exceeds %d bytes", len(m.Text), MaxStatusTextBytes),
			}
		}
		if strings.IndexByte(m.Text, 0) >= 0 {
			return nil, &DecodeError{
				Kind:      ErrInvalidFrame,
				MessageID: MsgIDStatusText,
				Detail:    "statustext must not contain embedded NUL bytes",
			}
		}
		return &common.MessageStatustext{
			Severity: common.MAV_SEVERITY(m.Severity),
			Text:     m.Text,
			Id:       m.ID,
			ChunkSeq: m.ChunkSeq,
		}, nil
	default:
		return nil, newDecodeError(ErrUnsupportedMessage, fmt.Sprintf("unhandled message type %T", msg))
	}
}

func fromWireMessage(msg message.Message) (Message, error) {
	switch m := msg.(type) {
	case *common.MessageHeartbeat:
		return Heartbeat{
			Type:           uint8(m.Type),
			Autopilot:      uint8(m.Autopilot),
			BaseMode:       uint8(m.BaseMode),
			CustomMode:     m.CustomMode,
			SystemStatus:   uint8(m.SystemStatus),
			MAVLinkVersion: m.MavlinkVersion,
		}, nil
	case *common.MessageSysStatus:
		return SysStatus{
			SensorsPresent:         uint32(m.OnboardControlSensorsPresent),
			SensorsEnabled:         uint32(m.OnboardControlSensorsEnabled),
			SensorsHealth:          uint32(m.OnboardControlSensorsHealth),
			Load:                   m.Load,
			VoltageBattery:         m.VoltageBattery,
			CurrentBattery:         m.CurrentBattery,
			BatteryRemaining:       m.BatteryRemaining,
			DropRateComm:           m.DropRateComm,
			ErrorsComm:             m.ErrorsComm,
			ErrorsCount1:           m.ErrorsCount1,
			ErrorsCount2:           m.ErrorsCount2,
			ErrorsCount3:           m.ErrorsCount3,
			ErrorsCount4:           m.ErrorsCount4,
			SensorsPresentExtended: uint32(m.OnboardControlSensorsPresentExtended),
			SensorsEnabledExtended: uint32(m.OnboardControlSensorsEnabledExtended),
			SensorsHealthExtended:  uint32(m.OnboardControlSensorsHealthExtended),
		}, nil
	case *common.MessageAttitudeQuaternion:
		return AttitudeQuaternion{
			TimeBootMs:  m.TimeBootMs,
			Q1:          m.Q1,
			Q2:          m.Q2,
			Q3:          m.Q3,
			Q4:          m.Q4,
			Rollspeed:   m.Rollspeed,
			Pitchspeed:  m.Pitchspeed,
			Yawspeed:    m.Yawspeed,
			ReprOffsetQ: m.ReprOffsetQ,
		}, nil
	case *common.MessageLocalPositionNed:
		return LocalPositionNED{
			TimeBootMs: m.TimeBootMs,
			X:          m.X,
			Y:          m.Y,
			Z:          m.Z,
			Vx:         m.Vx,
			Vy:         m.Vy,
			Vz:         m.Vz,
		}, nil
	case *common.MessageGlobalPositionInt:
		return GlobalPositionInt{
			TimeBootMs:  m.TimeBootMs,
			Lat:         m.Lat,
			Lon:         m.Lon,
			Alt:         m.Alt,
			RelativeAlt: m.RelativeAlt,
			Vx:          m.Vx,
			Vy:          m.Vy,
			Vz:          m.Vz,
			Hdg:         m.Hdg,
		}, nil
	case *common.MessageCommandLong:
		return CommandLong{
			TargetSystem:    m.TargetSystem,
			TargetComponent: m.TargetComponent,
			Command:         uint16(m.Command),
			Confirmation:    m.Confirmation,
			Param1:          m.Param1,
			Param2:          m.Param2,
			Param3:          m.Param3,
			Param4:          m.Param4,
			Param5:          m.Param5,
			Param6:          m.Param6,
			Param7:          m.Param7,
		}, nil
	case *common.MessageCommandAck:
		return CommandAck{
			Command:         uint16(m.Command),
			Result:          uint8(m.Result),
			Progress:        m.Progress,
			ResultParam2:    m.ResultParam2,
			TargetSystem:    m.TargetSystem,
			TargetComponent: m.TargetComponent,
		}, nil
	case *common.MessageSetPositionTargetLocalNed:
		return SetPositionTargetLocalNED{
			TimeBootMs:      m.TimeBootMs,
			TargetSystem:    m.TargetSystem,
			TargetComponent: m.TargetComponent,
			CoordinateFrame: uint8(m.CoordinateFrame),
			TypeMask:        uint16(m.TypeMask),
			X:               m.X,
			Y:               m.Y,
			Z:               m.Z,
			Vx:              m.Vx,
			Vy:              m.Vy,
			Vz:              m.Vz,
			Afx:             m.Afx,
			Afy:             m.Afy,
			Afz:             m.Afz,
			Yaw:             m.Yaw,
			YawRate:         m.YawRate,
		}, nil
	case *common.MessageStatustext:
		return StatusText{
			Severity: uint8(m.Severity),
			Text:     m.Text,
			ID:       m.Id,
			ChunkSeq: m.ChunkSeq,
		}, nil
	case *message.MessageRaw:
		return nil, &DecodeError{
			Kind:      ErrUnsupportedMessage,
			MessageID: m.ID,
			Detail:    fmt.Sprintf("message #%d outside 9-message common.xml contract", m.ID),
		}
	default:
		return nil, newDecodeError(ErrUnsupportedMessage, fmt.Sprintf("unexpected wire message %T", msg))
	}
}
