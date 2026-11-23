// Package ocppj implements OCPP-J: OCPP messages framed as JSON arrays over a
// WebSocket connection. It knows nothing about individual OCPP actions, so
// every OCPP version that uses this framing can sit on top of it.
package ocppj

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// MessageType is the first element of an OCPP-J frame.
type MessageType int

const (
	TypeCall       MessageType = 2
	TypeCallResult MessageType = 3
	TypeCallError  MessageType = 4
)

// Error codes defined by OCPP-J 1.6.
const (
	ErrNotImplemented                = "NotImplemented"
	ErrNotSupported                  = "NotSupported"
	ErrInternalError                 = "InternalError"
	ErrProtocolError                 = "ProtocolError"
	ErrSecurityError                 = "SecurityError"
	ErrFormationViolation            = "FormationViolation"
	ErrPropertyConstraintViolation   = "PropertyConstraintViolation"
	ErrOccurrenceConstraintViolation = "OccurrenceConstraintViolation"
	ErrTypeConstraintViolation       = "TypeConstraintViolation"
	ErrGenericError                  = "GenericError"
)

// Message is one decoded OCPP-J frame.
//
//	Call:       [2, "<id>", "<action>", {payload}]
//	CallResult: [3, "<id>", {payload}]
//	CallError:  [4, "<id>", "<code>", "<description>", {details}]
type Message struct {
	Type MessageType
	ID   string

	Action  string          // Call
	Payload json.RawMessage // Call and CallResult

	ErrorCode        string          // CallError
	ErrorDescription string          // CallError
	ErrorDetails     json.RawMessage // CallError
}

// CallError is an error answer to a call. It is returned by Conn.Call when
// the other side answers with an error, and returned by a Handler to answer
// with one.
type CallError struct {
	Code        string
	Description string
	Details     json.RawMessage
}

func (e *CallError) Error() string {
	if e.Description == "" {
		return e.Code
	}
	return e.Code + ": " + e.Description
}

var emptyObject = json.RawMessage(`{}`)

// Encode returns the wire form of m.
func (m Message) Encode() ([]byte, error) {
	payload := m.Payload
	if len(payload) == 0 {
		payload = emptyObject
	}

	var frame []any
	switch m.Type {
	case TypeCall:
		frame = []any{m.Type, m.ID, m.Action, payload}
	case TypeCallResult:
		frame = []any{m.Type, m.ID, payload}
	case TypeCallError:
		details := m.ErrorDetails
		if len(details) == 0 {
			details = emptyObject
		}
		frame = []any{m.Type, m.ID, m.ErrorCode, m.ErrorDescription, details}
	default:
		return nil, fmt.Errorf("ocppj: unknown message type %d", m.Type)
	}
	return json.Marshal(frame)
}

// Decode reads one OCPP-J frame.
func Decode(data []byte) (Message, error) {
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil {
		return Message{}, errors.New("ocppj: frame is not a JSON array")
	}
	if len(parts) < 3 {
		return Message{}, fmt.Errorf("ocppj: frame has %d elements, want at least 3", len(parts))
	}

	var m Message
	if err := json.Unmarshal(parts[0], &m.Type); err != nil {
		return Message{}, errors.New("ocppj: message type is not a number")
	}
	if err := json.Unmarshal(parts[1], &m.ID); err != nil {
		return Message{}, errors.New("ocppj: message ID is not a string")
	}

	switch m.Type {
	case TypeCall:
		if len(parts) != 4 {
			return m, fmt.Errorf("ocppj: call has %d elements, want 4", len(parts))
		}
		if err := json.Unmarshal(parts[2], &m.Action); err != nil {
			return m, errors.New("ocppj: action is not a string")
		}
		if !isObject(parts[3]) {
			return m, errors.New("ocppj: call payload is not a JSON object")
		}
		m.Payload = parts[3]

	case TypeCallResult:
		if len(parts) != 3 {
			return m, fmt.Errorf("ocppj: call result has %d elements, want 3", len(parts))
		}
		if !isObject(parts[2]) {
			return m, errors.New("ocppj: call result payload is not a JSON object")
		}
		m.Payload = parts[2]

	case TypeCallError:
		if len(parts) != 5 {
			return m, fmt.Errorf("ocppj: call error has %d elements, want 5", len(parts))
		}
		if err := json.Unmarshal(parts[2], &m.ErrorCode); err != nil {
			return m, errors.New("ocppj: error code is not a string")
		}
		if err := json.Unmarshal(parts[3], &m.ErrorDescription); err != nil {
			return m, errors.New("ocppj: error description is not a string")
		}
		m.ErrorDetails = parts[4]

	default:
		return m, fmt.Errorf("ocppj: unknown message type %d", m.Type)
	}
	return m, nil
}

func isObject(raw json.RawMessage) bool {
	return bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{"))
}
