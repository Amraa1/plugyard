package cli

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	code, stdout, stderr := run("version")

	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %q)", code, ExitOK, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	for _, want := range []string{
		"Version:  dev",
		"OCPP:     1.6J",
		"OS/Arch:  " + runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestNoArgsPrintsHelp(t *testing.T) {
	code, stdout, _ := run()

	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "version") {
		t.Errorf("help does not list the version command:\n%s", stdout)
	}
}

func TestUsageErrorsExitWithTwo(t *testing.T) {
	cases := map[string][]string{
		"unknown command":     {"nosuchcommand"},
		"unknown flag":        {"version", "--nosuchflag"},
		"unexpected argument": {"version", "extra"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := run(args...)

			if code != ExitUsage {
				t.Errorf("exit code = %d, want %d", code, ExitUsage)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.HasPrefix(stderr, "plugyard: ") {
				t.Errorf("stderr = %q, want it to start with %q", stderr, "plugyard: ")
			}
		})
	}
}
