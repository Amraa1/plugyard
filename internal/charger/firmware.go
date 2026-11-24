package charger

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/Amraa1/plugyard/internal/ocpp/ocppj"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// The Firmware Management profile. The charger never fetches or sends a
// file: it walks through the statuses a real charger reports, so that a CSMS
// can be tested without a file server.

// What Fail can make fail.
const (
	FailDownload = "download"
	FailInstall  = "install"
	FailUpload   = "upload"
)

// Failures lists what Fail accepts.
var Failures = []string{FailDownload, FailInstall, FailUpload}

// The steps of a firmware update and of a diagnostics upload.
const (
	stepWaiting    = iota // for the retrieve date, or for the next attempt
	stepTransfer          // downloading or uploading
	stepDownloaded        // waiting for the transactions to end
	stepInstalling
)

// transfer is a firmware update or a diagnostics upload under way.
type transfer struct {
	location string
	step     int
	// until is when the current step ends.
	until time.Time
	// attempts counts the downloads or uploads started so far.
	attempts      int
	retries       int
	retryInterval time.Duration
}

// newTransfer starts a transfer that waits until start.
func newTransfer(location string, start time.Time, retries, retryInterval *int) *transfer {
	t := &transfer{location: location, step: stepWaiting, until: start}
	if retries != nil && *retries > 0 {
		t.retries = *retries
	}
	if retryInterval != nil && *retryInterval > 0 {
		t.retryInterval = time.Duration(*retryInterval) * time.Second
	}
	return t
}

// retry reports whether a failed download or upload is tried again, and if
// so sets it up.
func (t *transfer) retry(now time.Time) bool {
	if t.attempts > t.retries {
		return false
	}
	t.step, t.until = stepWaiting, now.Add(t.retryInterval)
	return true
}

// scaled shortens a duration by the speed of the site.
func (c *Charger) scaled(d time.Duration) time.Duration {
	return time.Duration(float64(d) / c.cfg.Speed)
}

// notify queues a FirmwareStatusNotification or a
// DiagnosticsStatusNotification. The caller holds c.mu.
func (c *Charger) notify(action, status string) {
	c.enqueue(&outMsg{action: action, req: &v16.TransferStatusNotification{Status: status}, volatile: true})
}

// firmwareVersion returns the firmware the charger runs: the one of its
// model until an update has installed another. The caller holds c.mu, or the
// charger is not running yet.
func (c *Charger) firmwareVersion() string {
	if c.installed != "" {
		return c.installed
	}
	return c.cfg.Model.Firmware
}

// versionFrom names the firmware at a location after its file:
// ".../fw-2.4.1.bin" is "fw-2.4.1". It returns "" for a location with no
// file name.
func versionFrom(location string) string {
	name := location
	if u, err := url.Parse(location); err == nil && u.Host != "" {
		name = u.Path
	}
	name = path.Base(strings.TrimRight(name, "/"))
	if name == "." || name == "/" || name == "" {
		return ""
	}
	// Extensions such as .bin and .tar.gz go; the ".1" of "2.4.1" stays.
	for {
		ext := path.Ext(name)
		if len(ext) < 2 || len(ext) > 5 || ext == name || !unicode.IsLetter(rune(ext[1])) || strings.ContainsFunc(ext[1:], func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			break
		}
		name = strings.TrimSuffix(name, ext)
	}
	if len(name) > 50 {
		name = name[:50]
	}
	return name
}

// updateFirmware answers UpdateFirmware.
func (c *Charger) updateFirmware(req v16.UpdateFirmwareRequest) *ocppj.CallError {
	if strings.TrimSpace(req.Location) == "" {
		return &ocppj.CallError{Code: ocppj.ErrPropertyConstraintViolation, Description: "UpdateFirmware needs a location"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.firmware; old != nil {
		if old.step == stepInstalling {
			c.event("firmware: the CSMS asked for an update from %s, which is ignored: an installation is under way", req.Location)
			return nil
		}
		c.event("firmware: the update from %s is replaced", old.location)
		c.setUpdating(false)
	}
	// The answer must reach the CSMS before the first status does.
	start := time.Now().Add(actDelay)
	if req.RetrieveDate.After(start) {
		start = req.RetrieveDate.Time
		c.event("firmware: the CSMS asked for an update from %s, to be fetched at %s", req.Location, start.UTC().Format(time.RFC3339))
	} else {
		c.event("firmware: the CSMS asked for an update from %s", req.Location)
	}
	c.firmware = newTransfer(req.Location, start, req.Retries, req.RetryInterval)
	return nil
}

// getDiagnostics answers GetDiagnostics with the name of the file the
// charger says it will upload.
func (c *Charger) getDiagnostics(req v16.GetDiagnosticsRequest) (v16.GetDiagnosticsResponse, *ocppj.CallError) {
	if strings.TrimSpace(req.Location) == "" {
		return v16.GetDiagnosticsResponse{}, &ocppj.CallError{Code: ocppj.ErrPropertyConstraintViolation, Description: "GetDiagnostics needs a location"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	name := fmt.Sprintf("diagnostics-%s-%s.log", c.cfg.ID, now.UTC().Format("20060102T150405Z"))
	if c.diagnostics != nil {
		c.event("diagnostics: the upload to %s is replaced", c.diagnostics.location)
	}
	c.event("diagnostics: the CSMS asked for %s to be uploaded to %s", name, req.Location)
	c.diagnostics = newTransfer(req.Location, now.Add(actDelay), req.Retries, req.RetryInterval)
	return v16.GetDiagnosticsResponse{FileName: name}, nil
}

// setUpdating makes the connectors unavailable for the time an installation
// needs them free, or gives them back. The caller holds c.mu.
func (c *Charger) setUpdating(updating bool) {
	for _, k := range c.connectors {
		k.updating = updating
		// A tag accepted before its EV arrived starts nothing any more.
		if updating && k.pending != nil {
			k.pending.timer.Stop()
			k.pending = nil
		}
		c.reportStatus(k)
	}
}

// stepTransfers moves a firmware update and a diagnostics upload along. The
// caller holds c.mu.
func (c *Charger) stepTransfers(now time.Time) {
	c.stepFirmware(now)
	c.stepDiagnostics(now)
}

func (c *Charger) stepFirmware(now time.Time) {
	t := c.firmware
	if t == nil || (t.step != stepDownloaded && now.Before(t.until)) {
		return
	}
	status := func(s string) { c.notify(v16.ActionFirmwareStatusNotification, s) }

	switch t.step {
	case stepWaiting:
		t.step, t.until = stepTransfer, now.Add(c.scaled(c.cfg.Timing.Download))
		t.attempts++
		if t.attempts == 1 {
			c.event("firmware: downloading %s", t.location)
			status(v16.FirmwareDownloading)
		} else {
			c.event("firmware: downloading again (attempt %d of %d)", t.attempts, t.retries+1)
		}

	case stepTransfer:
		if c.failing[FailDownload] {
			if t.retry(now) {
				c.event("firmware: the download failed; trying again in %s", t.retryInterval)
				return
			}
			delete(c.failing, FailDownload)
			c.firmware = nil
			c.event("firmware: the download failed")
			status(v16.FirmwareDownloadFailed)
			return
		}
		t.step = stepDownloaded
		status(v16.FirmwareDownloaded)
		// No new transaction may start; the ones running are left to end.
		c.setUpdating(true)
		if n := c.transactions(); n > 0 {
			c.event("firmware: downloaded; waiting for %d transaction(s) to end before installing", n)
		} else {
			c.event("firmware: downloaded")
		}

	case stepDownloaded:
		if c.transactions() > 0 {
			return
		}
		t.step, t.until = stepInstalling, now.Add(c.scaled(c.cfg.Timing.Install))
		c.event("firmware: installing")
		status(v16.FirmwareInstalling)

	case stepInstalling:
		c.firmware = nil
		c.setUpdating(false)
		if c.failing[FailInstall] {
			delete(c.failing, FailInstall)
			c.event("firmware: the installation failed; the charger keeps firmware %s", c.firmwareVersion())
			status(v16.FirmwareInstallationFailed)
			return
		}
		if version := versionFrom(t.location); version != "" {
			c.installed = version
			c.dirty = true
		}
		c.event("firmware: %s installed; rebooting", c.firmwareVersion())
		status(v16.FirmwareInstalled)
		c.after(func() { c.reboot(v16.ReasonReboot, false) })
	}
}

func (c *Charger) stepDiagnostics(now time.Time) {
	t := c.diagnostics
	if t == nil || now.Before(t.until) {
		return
	}
	status := func(s string) { c.notify(v16.ActionDiagnosticsStatusNotification, s) }

	switch t.step {
	case stepWaiting:
		t.step, t.until = stepTransfer, now.Add(c.scaled(c.cfg.Timing.Upload))
		t.attempts++
		if t.attempts == 1 {
			c.event("diagnostics: uploading to %s", t.location)
			status(v16.DiagnosticsUploading)
		} else {
			c.event("diagnostics: uploading again (attempt %d of %d)", t.attempts, t.retries+1)
		}

	case stepTransfer:
		if c.failing[FailUpload] {
			if t.retry(now) {
				c.event("diagnostics: the upload failed; trying again in %s", t.retryInterval)
				return
			}
			delete(c.failing, FailUpload)
			c.diagnostics = nil
			c.event("diagnostics: the upload failed")
			status(v16.DiagnosticsUploadFailed)
			return
		}
		c.diagnostics = nil
		c.event("diagnostics: uploaded")
		status(v16.DiagnosticsUploaded)
	}
}

// transactions counts the transactions running. The caller holds c.mu.
func (c *Charger) transactions() int {
	n := 0
	for _, k := range c.connectors {
		if k.tx != nil {
			n++
		}
	}
	return n
}

// firmwareStatus and diagnosticsStatus return what a TriggerMessage is
// answered with: where the transfer stands, or Idle. The caller holds c.mu.
func (c *Charger) firmwareStatus() string {
	t := c.firmware
	switch {
	case t == nil || t.attempts == 0:
		return v16.FirmwareIdle
	case t.step == stepDownloaded:
		return v16.FirmwareDownloaded
	case t.step == stepInstalling:
		return v16.FirmwareInstalling
	default:
		return v16.FirmwareDownloading
	}
}

func (c *Charger) diagnosticsStatus() string {
	if t := c.diagnostics; t == nil || t.attempts == 0 {
		return v16.DiagnosticsIdle
	}
	return v16.DiagnosticsUploading
}

// Fail makes the next firmware download, firmware installation or
// diagnostics upload fail, as a broken link or a bad file would. It stays
// set until that step has failed, or the charger is powered off.
func (c *Charger) Fail(what string) (string, error) {
	var outcome string
	switch what {
	case FailDownload:
		outcome = "the next firmware download will fail"
	case FailInstall:
		outcome = "the next firmware installation will fail"
	case FailUpload:
		outcome = "the next diagnostics upload will fail"
	default:
		return "", fmt.Errorf("unknown step %q: want %s", what, strings.Join(Failures, ", "))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.off {
		return "", errOff
	}
	if c.failing[what] {
		return "", errors.New(outcome + " already")
	}
	c.failing[what] = true
	c.event("%s", outcome)
	return outcome, nil
}
