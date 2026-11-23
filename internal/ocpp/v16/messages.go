// Package v16 defines the OCPP 1.6 messages plugyard exchanges with a CSMS.
package v16

import (
	"encoding/json"
	"time"
)

// Subprotocol is the WebSocket subprotocol name of OCPP 1.6J.
const Subprotocol = "ocpp1.6"

// Actions.
const (
	ActionBootNotification   = "BootNotification"
	ActionHeartbeat          = "Heartbeat"
	ActionStatusNotification = "StatusNotification"
)

// Registration status values in a BootNotification answer.
const (
	RegistrationAccepted = "Accepted"
	RegistrationPending  = "Pending"
	RegistrationRejected = "Rejected"
)

// Connector status values.
const (
	StatusAvailable     = "Available"
	StatusPreparing     = "Preparing"
	StatusCharging      = "Charging"
	StatusSuspendedEVSE = "SuspendedEVSE"
	StatusSuspendedEV   = "SuspendedEV"
	StatusFinishing     = "Finishing"
	StatusReserved      = "Reserved"
	StatusUnavailable   = "Unavailable"
	StatusFaulted       = "Faulted"
)

// ErrorCodeNoError is the error code of a connector that has no fault.
const ErrorCodeNoError = "NoError"

// Time is a timestamp in the form OCPP 1.6 uses: RFC 3339 in UTC with
// millisecond precision.
type Time struct{ time.Time }

const timeLayout = "2006-01-02T15:04:05.000Z"

func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Format(timeLayout))
}

func (t *Time) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

type BootNotificationRequest struct {
	ChargePointVendor string `json:"chargePointVendor"`
	ChargePointModel  string `json:"chargePointModel"`
	FirmwareVersion   string `json:"firmwareVersion,omitempty"`
}

type BootNotificationResponse struct {
	Status      string `json:"status"`
	CurrentTime Time   `json:"currentTime"`
	Interval    int    `json:"interval"`
}

type HeartbeatRequest struct{}

type HeartbeatResponse struct {
	CurrentTime Time `json:"currentTime"`
}

type StatusNotificationRequest struct {
	ConnectorID int    `json:"connectorId"`
	ErrorCode   string `json:"errorCode"`
	Status      string `json:"status"`
	Timestamp   *Time  `json:"timestamp,omitempty"`
}

type StatusNotificationResponse struct{}
