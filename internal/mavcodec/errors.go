// Package mavcodec implements the clean-room bbstack 0xB1 HEARTBEAT.custom_mode
// bitfield codec and MIT minimal.xml/common.xml MAVLink v2 (0xFD) frame codec.
package mavcodec

import (
	"errors"
	"fmt"
)

var (
	// ErrNotBBStackHeartbeat indicates a HEARTBEAT or custom_mode word whose
	// high byte [31:24] is not 0xB1 (for example, a peer GCS heartbeat).
	ErrNotBBStackHeartbeat = errors.New("mavcodec: non-0xB1 heartbeat")

	// ErrInvalidCustomMode indicates an out-of-range enum nibble or byte inside
	// a 0xB1 custom_mode word (or an invalid CustomMode struct on encode).
	ErrInvalidCustomMode = errors.New("mavcodec: invalid 0xB1 custom_mode field")

	// ErrInvalidHeartbeatEnvelope indicates a 0xB1 HEARTBEAT frame failing strict
	// envelope invariants (CUSTOM_MODE_ENABLED flag, MAV_AUTOPILOT_GENERIC, or
	// HEARTBEAT.type matching VehicleDomain).
	ErrInvalidHeartbeatEnvelope = errors.New("mavcodec: invalid 0xB1 heartbeat envelope")

	// ErrNonV2Frame indicates a packet that does not start with MAVLink v2 magic 0xFD.
	ErrNonV2Frame = errors.New("mavcodec: non-MAVLink-v2 frame")

	// ErrBadChecksum indicates an X.25 CRC mismatch on a MAVLink v2 frame.
	ErrBadChecksum = errors.New("mavcodec: checksum mismatch")

	// ErrUnsupportedMessage indicates a MAVLink message ID outside the 9-message
	// bbops common.xml edge contract.
	ErrUnsupportedMessage = errors.New("mavcodec: unsupported message id")

	// ErrInvalidFrame indicates malformed framing, truncated or trailing bytes,
	// unverified incompatibility flags, zero sender IDs, or out-of-bounds fields.
	ErrInvalidFrame = errors.New("mavcodec: invalid frame")
)

// DecodeError is the structured error type returned by all mavcodec pack/unpack
// and frame encode/decode operations. Callers can match sentinel categories via
// errors.Is(err, Err...) or inspect fields via errors.As(err, &decErr).
type DecodeError struct {
	Kind      error
	MessageID uint32
	Detail    string
	Cause     error
}

// Error implements error.
func (e *DecodeError) Error() string {
	if e == nil {
		return ""
	}
	base := "mavcodec error"
	if e.Kind != nil {
		base = e.Kind.Error()
	}
	if e.Detail != "" && e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", base, e.Detail, e.Cause)
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s", base, e.Detail)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", base, e.Cause)
	}
	return base
}

// Unwrap returns wrapped sentinel and underlying errors for errors.Is / errors.As.
func (e *DecodeError) Unwrap() []error {
	if e == nil {
		return nil
	}
	if e.Kind != nil && e.Cause != nil {
		return []error{e.Kind, e.Cause}
	}
	if e.Kind != nil {
		return []error{e.Kind}
	}
	if e.Cause != nil {
		return []error{e.Cause}
	}
	return nil
}

func newDecodeError(kind error, detail string) error {
	return &DecodeError{Kind: kind, Detail: detail}
}

func wrapDecodeError(kind error, msgID uint32, detail string, cause error) error {
	return &DecodeError{Kind: kind, MessageID: msgID, Detail: detail, Cause: cause}
}
