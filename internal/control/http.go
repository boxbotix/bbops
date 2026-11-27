package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const (
	// CommandFrontDoorPath is the same-origin HTTP POST endpoint for discrete #76 commands.
	CommandFrontDoorPath = "/control/command"

	// TeleopFrontDoorPath is the same-origin HTTP POST endpoint for 10-30 Hz #84 velocity setpoints.
	TeleopFrontDoorPath = "/control/teleop"

	// MaxControlBodyBytes caps inbound JSON request bodies on /control/* at 16 KiB.
	MaxControlBodyBytes = 16 * 1024
)

// Envelope is the deterministic JSON response wrapper returned by /control/command and /control/teleop.
type Envelope[T any] struct {
	OK     bool         `json:"ok"`
	Result *T           `json:"result,omitempty"`
	Error  *ErrorDetail `json:"error,omitempty"`
}

// ErrorDetail encodes a structured refusal or transport error on /control/*.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ServeCommand handles POST /control/command after host middleware validates
// HTTP method, same-origin Origin header, and runtime readiness.
func (s *Service) ServeCommand(w http.ResponseWriter, r *http.Request, shutdownCtx context.Context) {
	var req CommandRequest
	if !decodeStrictBody(w, r, &req) {
		return
	}

	reqCtx, cleanup := bindShutdownContext(r.Context(), shutdownCtx)
	defer cleanup()

	res, err := s.SubmitCommand(reqCtx, req)
	if err != nil {
		writeControlFailure[CommandResult](w, err)
		return
	}
	writeControlJSON(w, http.StatusOK, Envelope[CommandResult]{
		OK:     true,
		Result: &res,
	})
}

// ServeTeleop handles POST /control/teleop after host middleware validates
// HTTP method, same-origin Origin header, and runtime readiness.
func (s *Service) ServeTeleop(w http.ResponseWriter, r *http.Request, shutdownCtx context.Context) {
	var req TeleopRequest
	if !decodeStrictBody(w, r, &req) {
		return
	}

	reqCtx, cleanup := bindShutdownContext(r.Context(), shutdownCtx)
	defer cleanup()

	res, err := s.SubmitTeleop(reqCtx, req)
	if err != nil {
		writeControlFailure[TeleopResult](w, err)
		return
	}
	writeControlJSON(w, http.StatusOK, Envelope[TeleopResult]{
		OK:     true,
		Result: &res,
	})
}

func bindShutdownContext(base, shutdownCtx context.Context) (context.Context, func()) {
	if shutdownCtx == nil {
		return base, func() {}
	}
	reqCtx, cancel := context.WithCancel(base)
	stopCancel := context.AfterFunc(shutdownCtx, cancel)
	return reqCtx, func() {
		stopCancel()
		cancel()
	}
}

func decodeStrictBody(w http.ResponseWriter, r *http.Request, target any) bool {
	limited := http.MaxBytesReader(w, r.Body, MaxControlBodyBytes)
	defer func() { _ = limited.Close() }()

	raw, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeControlError[struct{}](w, http.StatusBadRequest, "request_too_large", "request body exceeds 16 KiB limit")
			return false
		}
		writeControlError[struct{}](w, http.StatusBadRequest, "bad_request", "failed to read request body")
		return false
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		writeControlError[struct{}](w, http.StatusBadRequest, "invalid_json", "expected JSON object")
		return false
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeControlError[struct{}](w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeControlError[struct{}](w, http.StatusBadRequest, "invalid_json", "unexpected trailing JSON content")
		return false
	}
	return true
}

func writeControlFailure[T any](w http.ResponseWriter, err error) {
	var ce *ControlError
	if errors.As(err, &ce) {
		status := ce.Status
		if status == 0 {
			status = http.StatusBadRequest
		}
		writeControlError[T](w, status, ce.Code, ce.Message)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeControlError[T](w, http.StatusGatewayTimeout, "timeout", "control operation timed out")
		return
	}
	writeControlError[T](w, http.StatusInternalServerError, "internal_error", err.Error())
}

func writeControlError[T any](w http.ResponseWriter, status int, code, message string) {
	writeControlJSON(w, status, Envelope[T]{
		OK: false,
		Error: &ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

func writeControlJSON[T any](w http.ResponseWriter, status int, payload Envelope[T]) {
	encoded, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}
