package scenario

import (
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/control"
)

const commute = `
name: commute
steps:
  - at: 0s
    charger: "FAST-*"
    do: plug
    ev: longrange
    soc: 30%
  - at: 5s
    charger: "FAST-*"
    do: tap
    tag: 04A1B2C3
  - { at: 10m, charger: FAST-003, do: fault, code: GroundFailure, connector: 0, info: leak }
  - { at: 10m, charger: FAST-004, do: offline, for: 30s }
  - { at: 20m, charger: FAST-005, do: kill }
  - { at: 30m, charger: "FAST-*", do: unplug, connector: 2 }
`

func TestParse(t *testing.T) {
	s, err := Parse([]byte(commute))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Name != "commute" || len(s.Steps) != 6 || s.Duration() != 30*time.Minute {
		t.Fatalf("scenario = %+v", s)
	}

	plug := s.Steps[0]
	if plug.Number != 1 || plug.At != 0 || plug.Charger != "FAST-*" || plug.Do != control.ActionPlug ||
		plug.Request.Action != control.ActionPlug || plug.Request.Connector != 1 || plug.Request.EVPreset != "longrange" ||
		plug.Request.EV.SoC == nil || *plug.Request.EV.SoC != 0.30 || plug.Power() {
		t.Errorf("plug step = %+v", plug)
	}
	if tap := s.Steps[1]; tap.At != 5*time.Second || tap.Request.Tag != "04A1B2C3" {
		t.Errorf("tap step = %+v", tap)
	}
	if fault := s.Steps[2]; fault.Request.Fault.Code != "GroundFailure" || fault.Request.Fault.Info != "leak" || fault.Request.Connector != 0 {
		t.Errorf("fault step = %+v", fault)
	}
	if offline := s.Steps[3]; offline.Request.For != 30*time.Second {
		t.Errorf("offline step = %+v", offline)
	}
	if kill := s.Steps[4]; !kill.Power() || kill.Do != control.OpKill {
		t.Errorf("kill step = %+v", kill)
	}

	for i, want := range []string{"plug --ev longrange", "tap 04A1B2C3", "fault GroundFailure -n 0", "offline --for 30s", "kill", "unplug -n 2"} {
		if got := s.Steps[i].Describe(); got != want {
			t.Errorf("step %d describes itself as %q, want %q", i+1, got, want)
		}
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`
steps:
  - { charger: CP-1, do: plug }
  - { at: soon, charger: CP-1, do: plug }
  - { at: 10s, do: tap }
  - { at: 20s, charger: "CP-[", do: explode }
  - { at: 5s, charger: CP-1, do: unplug }
  - { at: 30s, charger: CP-1, do: fault, code: Exploded }
  - { at: 30s, charger: CP-1, do: unplug, tag: T1, for: 5s }
  - { at: 30s, charger: CP-1, do: plug, connector: 0, soc: lots }
  - { at: 30s, charger: CP-1, do: offline, for: never, connector: 2 }
  - { at: 30s, charger: CP-1, do: tap, tag: 123456789012345678901 }
  - { at: -5s, charger: CP-1, do: stop }
`))
	if err == nil {
		t.Fatal("Parse succeeded")
	}
	for _, want := range []string{
		"name is required",
		"step 1: at is required",
		`step 2: at "soon" is not a time`,
		"step 3: charger is required",
		"step 3: tag is required for tap",
		`step 4: charger "CP-[" is not a valid pattern`,
		`step 4: do "explode" is unknown: want one of plug, unplug, tap, fault, clear, offline, online, reboot, stop, start, restart, kill`,
		"step 5: at 5s is earlier than the step before it (20s); write the steps in time order",
		`step 6: code "Exploded" is unknown`,
		"step 7: tag does not apply to unplug",
		"step 7: for does not apply to unplug",
		"step 8: connector must be 1 or more",
		"step 8: soc:",
		`step 9: for "never" is not a time above zero`,
		"step 9: connector does not apply to offline",
		"step 10: tag is longer than the 20 characters",
		"step 11: at cannot be negative",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is missing %q:\n%v", want, err)
		}
	}
}

func TestParseRejectsWhatItDoesNotKnow(t *testing.T) {
	for text, want := range map[string]string{
		"":                              "the file is empty",
		"name: x\n":                     "steps: at least one step is required",
		"name: x\nsteps: []\nloop: 3\n": "loop",
		"name: x\nsteps:\n  - { at: 0s, charger: CP-1, do: plug, when: later }\n": "when",
	} {
		if _, err := Parse([]byte(text)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error = %v, want one containing %q", text, err, want)
		}
	}
}
