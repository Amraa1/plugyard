package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/csmstest"
)

func TestWaitForAState(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	startRun(t, csms, "CP-2")

	expect := func(want string, args ...string) {
		t.Helper()
		code, stdout, stderr := run(args...)
		if code != ExitOK || stdout != want {
			t.Errorf("%v: exit %d, stdout %q, stderr %q\nwant %q", args, code, stdout, stderr, want)
		}
	}

	// A state the chargers are in already is reached at once, whatever the
	// case it is written in.
	expect("CP-1: connector 1 is Available\n", "wait", "CP-1", "Available")
	expect("CP-1: connector 1 is Available\n", "wait", "cp-1", "available", "-n", "1")
	expect("CP-1: online\nCP-2: online\n", "wait", "CP-*", "online")
	// A charger named twice is waited for once.
	expect("CP-1: online\nCP-2: online\n", "wait", "CP-1", "CP-*", "ONLINE")

	// A state that comes later: wait returns when it does.
	type result struct {
		code           int
		stdout, stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, stdout, stderr := run("wait", "CP-1", "CP-2", "Charging", "--timeout", "10s")
		done <- result{code, stdout, stderr}
	}()
	mustRun(t, "exec", "CP-*", "plug")
	mustRun(t, "exec", "CP-1", "tap", "TAG-1")
	select {
	case r := <-done:
		t.Fatalf("wait returned before CP-2 was charging: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	case <-time.After(300 * time.Millisecond):
	}
	mustRun(t, "exec", "CP-2", "tap", "TAG-2")
	select {
	case r := <-done:
		if r.code != ExitOK || r.stdout != "CP-1: connector 1 is Charging\nCP-2: connector 1 is Charging\n" {
			t.Errorf("wait for Charging: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not return once both chargers were charging")
	}

	// A state that does not come: exit 1, and only the chargers that are
	// not in it are named.
	mustRun(t, "exec", "CP-2", "tap", "TAG-2")
	expect("CP-2: connector 1 is Finishing\n", "wait", "CP-2", "Finishing")
	start := time.Now()
	code, stdout, stderr := run("wait", "CP-*", "Charging", "--timeout", "300ms")
	if code != ExitFailure || stdout != "" || stderr != "CP-2: connector 1 is Finishing, not Charging, after 300ms\n" {
		t.Errorf("wait that times out: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if took := time.Since(start); took < 300*time.Millisecond || took > 5*time.Second {
		t.Errorf("a wait with a timeout of 300ms took %s", took)
	}

	// Connection states.
	mustRun(t, "exec", "CP-1", "offline")
	expect("CP-1: offline\n", "wait", "CP-1", "offline")
	code, _, stderr = run("wait", "CP-*", "online", "--timeout", "200ms")
	if code != ExitFailure || stderr != "CP-1: offline, not online, after 200ms\n" {
		t.Errorf("wait for online while offline: exit %d, stderr %q", code, stderr)
	}
	mustRun(t, "exec", "CP-1", "online")
	expect("CP-1: online\n", "wait", "CP-1", "online", "--timeout", "10s")

	// A stopped charger is "stopped", and has no connector status.
	mustRun(t, "stop", "CP-2")
	expect("CP-2: stopped\n", "wait", "CP-2", "stopped", "--timeout", "10s")
	code, _, stderr = run("wait", "CP-2", "Available", "--timeout", "200ms")
	if code != ExitFailure || stderr != "CP-2: stopped, not Available, after 200ms\n" {
		t.Errorf("wait for a connector of a stopped charger: exit %d, stderr %q", code, stderr)
	}
}

func TestWaitUsageErrors(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"wait", "CP-1"}, "requires at least 2 arg(s)"},
		{[]string{"wait", "CP-1", "Sleeping"}, `unknown state "Sleeping": want a connector status (Available, `},
		{[]string{"wait", "CP-1", "Reserved"}, `unknown state "Reserved"`},
		{[]string{"wait", "CP-9", "Available"}, `no charger matches "CP-9"`},
		{[]string{"wait", "CP-1", "FAST-*", "Available"}, `no charger matches "FAST-*"`},
		{[]string{"wait", "CP-[", "Available"}, `invalid charger pattern "CP-["`},
		{[]string{"wait", "CP-1", "Available", "-n", "2"}, "charger CP-1 has no connector 2"},
		{[]string{"wait", "CP-1", "Available", "-n", "0"}, "the connector number must be 1 or more"},
		{[]string{"wait", "CP-1", "online", "-n", "1"}, "online is a state of the whole charger, so it takes no --connector"},
		{[]string{"wait", "CP-1", "online", "--timeout", "-1s"}, "--timeout must be a time such as 30s"},
		{[]string{"wait", "CP-1", "online", "-s", "nowhere"}, `no site is named "nowhere"`},
	} {
		code, stdout, stderr := run(tc.args...)
		wantCode := ExitUsage
		if strings.HasPrefix(tc.want, "no site") {
			wantCode = ExitFailure
		}
		if code != wantCode || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q\nwant exit %d and %q", tc.args, code, stdout, stderr, wantCode, tc.want)
		}
	}
}
