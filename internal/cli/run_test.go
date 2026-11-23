package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

const testChargerfile = `
vendor: Acme
model: Wallbox
firmware: "1.0"
connectors:
  - { id: 1, type: Type2, maxPower: 7.4kW }
`

func writeChargerfile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Chargerfile")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// syncBuffer is a bytes.Buffer that is safe to read while a command writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunBringsAChargerOnlineUntilInterrupted(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeChargerfile(t, testChargerfile)

	ctx, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	var stdout, stderr syncBuffer
	exit := make(chan int, 1)
	go func() {
		exit <- RunContext(ctx, []string{"run", path, "--csms", csms.URL(), "--id", "CP-1"}, &stdout, &stderr)
	}()

	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 1)
	interrupt() // what Ctrl+C does

	select {
	case code := <-exit:
		if code != ExitOK {
			t.Errorf("exit code = %d, want %d (stderr: %q)", code, ExitOK, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after being interrupted")
	}

	if ends := csms.WaitDisconnects(t, "CP-1", 1); !ends[0].Clean {
		t.Error("the connection was dropped, want a normal WebSocket close")
	}

	out := stdout.String()
	for _, want := range []string{
		"CP-1 | connected to " + csms.URL() + "/CP-1",
		`CP-1 > [2,`,
		`"BootNotification",{"chargePointVendor":"Acme","chargePointModel":"Wallbox","firmwareVersion":"1.0"}]`,
		`CP-1 < [3,`,
		"CP-1 | accepted by the CSMS",
		"CP-1 | powered off",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestRunUsageErrors(t *testing.T) {
	valid := writeChargerfile(t, testChargerfile)
	invalid := writeChargerfile(t, "vendor: Acme\n")

	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no Chargerfile", []string{"run", "--csms", "ws://localhost/ocpp", "--id", "CP-1"}, "accepts 1 arg"},
		{"no --csms", []string{"run", valid, "--id", "CP-1"}, `"csms"`},
		{"no --id", []string{"run", valid, "--csms", "ws://localhost/ocpp"}, `"id"`},
		{"missing file", []string{"run", filepath.Join(t.TempDir(), "nope"), "--csms", "ws://localhost/ocpp", "--id", "CP-1"}, "nope"},
		{"invalid Chargerfile", []string{"run", invalid, "--csms", "ws://localhost/ocpp", "--id", "CP-1"}, "model is required"},
		{"http URL", []string{"run", valid, "--csms", "http://localhost/ocpp", "--id", "CP-1"}, "ws:// or wss://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := run(tc.args...)
			if code != ExitUsage {
				t.Errorf("exit code = %d, want %d", code, ExitUsage)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.wantErr)
			}
		})
	}
}
