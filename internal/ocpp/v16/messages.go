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
	Info        string `json:"info,omitempty"`
	Status      string `json:"status"`
	Timestamp   *Time  `json:"timestamp,omitempty"`
	// VendorErrorCode is a vendor's own code for the fault.
	VendorErrorCode string `json:"vendorErrorCode,omitempty"`
}

type StatusNotificationResponse struct{}

// Actions of a transaction.
const (
	ActionAuthorize        = "Authorize"
	ActionStartTransaction = "StartTransaction"
	ActionStopTransaction  = "StopTransaction"
	ActionMeterValues      = "MeterValues"
)

// AuthorizationAccepted is the idTagInfo status of a tag that may charge.
const AuthorizationAccepted = "Accepted"

// Reasons for stopping a transaction.
const (
	ReasonLocal          = "Local"
	ReasonEVDisconnected = "EVDisconnected"
	ReasonRemote         = "Remote"
	ReasonDeAuthorized   = "DeAuthorized"
	ReasonPowerLoss      = "PowerLoss"
	ReasonOther          = "Other"
)

// Measurands plugyard can report.
const (
	MeasurandEnergy = "Energy.Active.Import.Register"
	MeasurandPower  = "Power.Active.Import"
	MeasurandSoC    = "SoC"
)

// Reading contexts.
const (
	ContextSamplePeriodic   = "Sample.Periodic"
	ContextTransactionBegin = "Transaction.Begin"
	ContextTransactionEnd   = "Transaction.End"
)

type IDTagInfo struct {
	Status      string `json:"status"`
	ParentIDTag string `json:"parentIdTag,omitempty"`
	ExpiryDate  *Time  `json:"expiryDate,omitempty"`
}

type AuthorizeRequest struct {
	IDTag string `json:"idTag"`
}

type AuthorizeResponse struct {
	IDTagInfo IDTagInfo `json:"idTagInfo"`
}

type StartTransactionRequest struct {
	ConnectorID int    `json:"connectorId"`
	IDTag       string `json:"idTag"`
	MeterStart  int    `json:"meterStart"`
	Timestamp   Time   `json:"timestamp"`
}

type StartTransactionResponse struct {
	IDTagInfo     IDTagInfo `json:"idTagInfo"`
	TransactionID int       `json:"transactionId"`
}

type StopTransactionRequest struct {
	IDTag           string       `json:"idTag,omitempty"`
	MeterStop       int          `json:"meterStop"`
	Timestamp       Time         `json:"timestamp"`
	TransactionID   int          `json:"transactionId"`
	Reason          string       `json:"reason,omitempty"`
	TransactionData []MeterValue `json:"transactionData,omitempty"`
}

type StopTransactionResponse struct {
	IDTagInfo *IDTagInfo `json:"idTagInfo,omitempty"`
}

type MeterValuesRequest struct {
	ConnectorID   int          `json:"connectorId"`
	TransactionID *int         `json:"transactionId,omitempty"`
	MeterValue    []MeterValue `json:"meterValue"`
}

type MeterValuesResponse struct{}

type MeterValue struct {
	Timestamp    Time           `json:"timestamp"`
	SampledValue []SampledValue `json:"sampledValue"`
}

type SampledValue struct {
	Value     string `json:"value"`
	Context   string `json:"context,omitempty"`
	Measurand string `json:"measurand,omitempty"`
	Location  string `json:"location,omitempty"`
	Unit      string `json:"unit,omitempty"`
}

// More reasons for stopping a transaction.
const (
	ReasonHardReset     = "HardReset"
	ReasonSoftReset     = "SoftReset"
	ReasonReboot        = "Reboot"
	ReasonUnlockCommand = "UnlockCommand"
)

// ContextTrigger is the reading context of a meter value the CSMS asked for.
const ContextTrigger = "Trigger"

// ErrorCodes are the fault codes a StatusNotification can carry, besides
// NoError.
var ErrorCodes = []string{
	"ConnectorLockFailure", "EVCommunicationError", "GroundFailure", "HighTemperature",
	"InternalError", "LocalListConflict", "OtherError", "OverCurrentFailure",
	"OverVoltage", "PowerMeterFailure", "PowerSwitchFailure", "ReaderFailure",
	"ResetFailure", "UnderVoltage", "WeakSignal",
}

// Actions the CSMS starts.
const (
	ActionRemoteStartTransaction = "RemoteStartTransaction"
	ActionRemoteStopTransaction  = "RemoteStopTransaction"
	ActionReset                  = "Reset"
	ActionGetConfiguration       = "GetConfiguration"
	ActionChangeConfiguration    = "ChangeConfiguration"
	ActionChangeAvailability     = "ChangeAvailability"
	ActionUnlockConnector        = "UnlockConnector"
	ActionClearCache             = "ClearCache"
	ActionDataTransfer           = "DataTransfer"
	ActionTriggerMessage         = "TriggerMessage"
)

// UnsupportedActions belong to the feature profiles plugyard does not
// implement: Local Auth List Management and Reservation.
var UnsupportedActions = []string{
	"GetLocalListVersion", "SendLocalList",
	"ReserveNow", "CancelReservation",
}

// Actions of the Firmware Management profile.
const (
	ActionUpdateFirmware                = "UpdateFirmware"
	ActionGetDiagnostics                = "GetDiagnostics"
	ActionFirmwareStatusNotification    = "FirmwareStatusNotification"
	ActionDiagnosticsStatusNotification = "DiagnosticsStatusNotification"
)

// Statuses of a firmware update and of a diagnostics upload. Idle is used by
// both, and only in answer to a TriggerMessage.
const (
	FirmwareIdle               = "Idle"
	FirmwareDownloading        = "Downloading"
	FirmwareDownloaded         = "Downloaded"
	FirmwareDownloadFailed     = "DownloadFailed"
	FirmwareInstalling         = "Installing"
	FirmwareInstalled          = "Installed"
	FirmwareInstallationFailed = "InstallationFailed"

	DiagnosticsIdle         = "Idle"
	DiagnosticsUploading    = "Uploading"
	DiagnosticsUploaded     = "Uploaded"
	DiagnosticsUploadFailed = "UploadFailed"
)

type UpdateFirmwareRequest struct {
	Location      string `json:"location"`
	Retries       *int   `json:"retries,omitempty"`
	RetrieveDate  Time   `json:"retrieveDate"`
	RetryInterval *int   `json:"retryInterval,omitempty"`
}

type GetDiagnosticsRequest struct {
	Location      string `json:"location"`
	Retries       *int   `json:"retries,omitempty"`
	RetryInterval *int   `json:"retryInterval,omitempty"`
	StartTime     *Time  `json:"startTime,omitempty"`
	StopTime      *Time  `json:"stopTime,omitempty"`
}

type GetDiagnosticsResponse struct {
	// FileName is empty when the charger has no diagnostics to send.
	FileName string `json:"fileName,omitempty"`
}

// TransferStatusNotification is both FirmwareStatusNotification and
// DiagnosticsStatusNotification: each carries only a status.
type TransferStatusNotification struct {
	Status string `json:"status"`
}

// Answer statuses.
const (
	StatusAccepted       = "Accepted"
	StatusRejected       = "Rejected"
	StatusScheduled      = "Scheduled"
	StatusNotSupported   = "NotSupported"
	StatusNotImplemented = "NotImplemented"
	StatusRebootRequired = "RebootRequired"
	StatusUnlocked       = "Unlocked"
	StatusUnknownVendor  = "UnknownVendorId"
)

// Reset types and availability types.
const (
	ResetHard = "Hard"
	ResetSoft = "Soft"

	AvailabilityOperative   = "Operative"
	AvailabilityInoperative = "Inoperative"
)

// StatusAnswer is the answer to every request that is answered with only a
// status.
type StatusAnswer struct {
	Status string `json:"status"`
}

type RemoteStartTransactionRequest struct {
	ConnectorID *int   `json:"connectorId,omitempty"`
	IDTag       string `json:"idTag"`
	// ChargingProfile, when given, is a TxProfile for the transaction the
	// request starts.
	ChargingProfile *ChargingProfile `json:"chargingProfile,omitempty"`
}

type RemoteStopTransactionRequest struct {
	TransactionID int `json:"transactionId"`
}

type ResetRequest struct {
	Type string `json:"type"`
}

type GetConfigurationRequest struct {
	Key []string `json:"key,omitempty"`
}

type KeyValue struct {
	Key      string `json:"key"`
	Readonly bool   `json:"readonly"`
	Value    string `json:"value"`
}

type GetConfigurationResponse struct {
	ConfigurationKey []KeyValue `json:"configurationKey,omitempty"`
	UnknownKey       []string   `json:"unknownKey,omitempty"`
}

type ChangeConfigurationRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type ChangeAvailabilityRequest struct {
	ConnectorID int    `json:"connectorId"`
	Type        string `json:"type"`
}

type UnlockConnectorRequest struct {
	ConnectorID int `json:"connectorId"`
}

type DataTransferRequest struct {
	VendorID  string `json:"vendorId"`
	MessageID string `json:"messageId,omitempty"`
	Data      string `json:"data,omitempty"`
}

type DataTransferResponse struct {
	Status string `json:"status"`
	Data   string `json:"data,omitempty"`
}

type TriggerMessageRequest struct {
	RequestedMessage string `json:"requestedMessage"`
	ConnectorID      *int   `json:"connectorId,omitempty"`
}

// Actions of the Smart Charging profile.
const (
	ActionSetChargingProfile   = "SetChargingProfile"
	ActionClearChargingProfile = "ClearChargingProfile"
	ActionGetCompositeSchedule = "GetCompositeSchedule"
)

// More measurands plugyard can report: what a connector offers the EV.
const (
	MeasurandPowerOffered   = "Power.Offered"
	MeasurandCurrentOffered = "Current.Offered"
)

// Measurands lists every measurand plugyard can report.
var Measurands = []string{MeasurandEnergy, MeasurandPower, MeasurandSoC, MeasurandPowerOffered, MeasurandCurrentOffered}

// Charging profile purposes, kinds, recurrency kinds and rate units.
const (
	PurposeChargePointMax = "ChargePointMaxProfile"
	PurposeTxDefault      = "TxDefaultProfile"
	PurposeTx             = "TxProfile"

	KindAbsolute  = "Absolute"
	KindRecurring = "Recurring"
	KindRelative  = "Relative"

	RecurrencyDaily  = "Daily"
	RecurrencyWeekly = "Weekly"

	UnitAmps  = "A"
	UnitWatts = "W"
)

// StatusUnknown answers a ClearChargingProfile that matched no profile.
const StatusUnknown = "Unknown"

type ChargingProfile struct {
	ChargingProfileID      int              `json:"chargingProfileId"`
	TransactionID          *int             `json:"transactionId,omitempty"`
	StackLevel             int              `json:"stackLevel"`
	ChargingProfilePurpose string           `json:"chargingProfilePurpose"`
	ChargingProfileKind    string           `json:"chargingProfileKind"`
	RecurrencyKind         string           `json:"recurrencyKind,omitempty"`
	ValidFrom              *Time            `json:"validFrom,omitempty"`
	ValidTo                *Time            `json:"validTo,omitempty"`
	ChargingSchedule       ChargingSchedule `json:"chargingSchedule"`
}

type ChargingSchedule struct {
	// Duration is the length of the schedule in seconds.
	Duration               *int                     `json:"duration,omitempty"`
	StartSchedule          *Time                    `json:"startSchedule,omitempty"`
	ChargingRateUnit       string                   `json:"chargingRateUnit"`
	ChargingSchedulePeriod []ChargingSchedulePeriod `json:"chargingSchedulePeriod"`
	MinChargingRate        *float64                 `json:"minChargingRate,omitempty"`
}

type ChargingSchedulePeriod struct {
	// StartPeriod is the start of the period, in seconds from the start of
	// the schedule.
	StartPeriod  int     `json:"startPeriod"`
	Limit        float64 `json:"limit"`
	NumberPhases *int    `json:"numberPhases,omitempty"`
}

type SetChargingProfileRequest struct {
	ConnectorID        int             `json:"connectorId"`
	CsChargingProfiles ChargingProfile `json:"csChargingProfiles"`
}

type ClearChargingProfileRequest struct {
	ID                     *int   `json:"id,omitempty"`
	ConnectorID            *int   `json:"connectorId,omitempty"`
	ChargingProfilePurpose string `json:"chargingProfilePurpose,omitempty"`
	StackLevel             *int   `json:"stackLevel,omitempty"`
}

type GetCompositeScheduleRequest struct {
	ConnectorID      int    `json:"connectorId"`
	Duration         int    `json:"duration"`
	ChargingRateUnit string `json:"chargingRateUnit,omitempty"`
}

type GetCompositeScheduleResponse struct {
	Status           string            `json:"status"`
	ConnectorID      *int              `json:"connectorId,omitempty"`
	ScheduleStart    *Time             `json:"scheduleStart,omitempty"`
	ChargingSchedule *ChargingSchedule `json:"chargingSchedule,omitempty"`
}
