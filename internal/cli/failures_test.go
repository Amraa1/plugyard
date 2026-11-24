package cli

import (
	"strings"
	"testing"

	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

func TestFailureActionsFromTheCommandLine(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1", "--speed", "360")

	expect := func(want string, args ...string) {
		t.Helper()
		code, stdout, stderr := run(args...)
		if code != ExitOK || stdout != want {
			t.Errorf("%v: exit %d, stdout %q, stderr %q\nwant %q", args, code, stdout, stderr, want)
		}
	}

	run("exec", "CP-1", "plug")
	run("exec", "CP-1", "tap", "TAG-1")
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)

	// A fault ends the transaction and shows in ps with its code.
	expect("CP-1  connector 1  ok  fault GroundFailure reported, transaction 1 stopped\n",
		"exec", "CP-1", "fault", "GroundFailure", "--info", "leak on L2", "--vendor-code", "E-42")
	var stop v16.StopTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStopTransaction, 1)[0].Decode(t, &stop)
	if stop.Reason != v16.ReasonOther {
		t.Errorf("StopTransaction reason = %s, want Other", stop.Reason)
	}
	if row := psRow(t, "CP-1"); row != "CP-1 cp-1 CP-1 online 1:Faulted(GroundFailure)" {
		t.Errorf("ps row = %q", row)
	}
	code, stdout, _ := run("exec", "CP-1", "tap", "TAG-1")
	if code != ExitFailure || !strings.Contains(stdout, "failed  the connector has a fault (GroundFailure); clear it first") {
		t.Errorf("tap on a faulted connector: exit %d, stdout %q", code, stdout)
	}
	expect("CP-1  connector 1  ok  fault GroundFailure cleared, connector 1 is Finishing\n", "exec", "CP-1", "clear")

	// -n 0 is the charger as a whole, listed in ps as connector 0.
	expect("CP-1  ok  fault InternalError reported\n", "exec", "CP-1", "fault", "InternalError", "-n", "0")
	if row := psRow(t, "CP-1"); row != "CP-1 cp-1 CP-1 online 0:Faulted(InternalError) 1:Finishing" {
		t.Errorf("ps row = %q", row)
	}
	expect("CP-1  ok  fault InternalError cleared, the charger is Available\n", "exec", "CP-1", "clear", "-n", "0")
	code, stdout, _ = run("exec", "CP-1", "clear")
	if code != ExitFailure || !strings.Contains(stdout, "failed  connector 1 has no fault") {
		t.Errorf("clear with no fault: exit %d, stdout %q", code, stdout)
	}

	// offline --for comes back by itself.
	expect("CP-1  ok  offline for 200ms\n", "exec", "CP-1", "offline", "--for", "200ms")
	csms.WaitDisconnects(t, "CP-1", 1)
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 2)
	eventually(t, "the charger is online again", func() bool { return strings.Contains(psRow(t, "CP-1"), " online ") })

	// offline stays away until online.
	expect("CP-1  ok  offline\n", "exec", "CP-1", "offline")
	csms.WaitDisconnects(t, "CP-1", 2)
	if row := psRow(t, "CP-1"); !strings.Contains(row, " offline ") {
		t.Errorf("ps row while offline = %q", row)
	}
	expect("CP-1  ok  online, connecting\n", "exec", "CP-1", "online")
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 3)
	eventually(t, "the charger is online again", func() bool { return strings.Contains(psRow(t, "CP-1"), " online ") })

	expect("CP-1  ok  rebooting\n", "exec", "CP-1", "reboot")
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 4)
}

func TestFailureActionUsageErrors(t *testing.T) {
	useTempDataDir(t)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"exec", "CP-1", "fault", "Exploded"}, `unknown error code "Exploded": OCPP 1.6 has ConnectorLockFailure, EVCommunicationError, GroundFailure`},
		{[]string{"exec", "CP-1", "fault"}, "fault takes exactly one argument, the error code: ConnectorLockFailure"},
		{[]string{"exec", "CP-1", "offline", "-n", "2"}, "offline acts on the whole charger, so it takes no --connector"},
		{[]string{"exec", "CP-1", "reboot", "-n", "1"}, "reboot acts on the whole charger"},
		{[]string{"exec", "CP-1", "plug", "-n", "0"}, "the connector number must be 1 or more"},
		{[]string{"exec", "CP-1", "online", "--for", "5s"}, "--for only applies to offline"},
		{[]string{"exec", "CP-1", "offline", "--for", "0s"}, "--for must be a time above zero"},
		{[]string{"exec", "CP-1", "clear", "--info", "x"}, "--info and --vendor-code only apply to fault"},
		{[]string{"exec", "CP-1", "explode"}, `unknown action "explode": want plug, unplug, tap, fault, clear, offline, online or reboot`},
	} {
		code, _, stderr := run(tc.args...)
		if code != ExitUsage || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q\nwant exit %d and %q", tc.args, code, stderr, ExitUsage, tc.want)
		}
	}

	// The help tells people the failure actions exist.
	_, help, _ := run("exec", "--help")
	for _, want := range []string{"Actions for testing what goes wrong", "fault <errorCode>", "GroundFailure", "WeakSignal", "offline --for 30s"} {
		if !strings.Contains(help, want) {
			t.Errorf("exec --help is missing %q", want)
		}
	}
}
