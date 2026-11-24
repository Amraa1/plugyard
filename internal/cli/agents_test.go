package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// useGuide builds the guide in, as the main package does.
func useGuide(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "llms.txt"))
	if err != nil {
		t.Fatal(err)
	}
	old := AgentGuide
	AgentGuide = string(data)
	t.Cleanup(func() { AgentGuide = old })
	return AgentGuide
}

// firstSession returns the commands of the guide's first session, each
// split into its arguments, without the leading "plugyard".
func firstSession(t *testing.T, guide string) [][]string {
	t.Helper()
	_, rest, found := strings.Cut(guide, "## A first session")
	_, rest, fenced := strings.Cut(rest, "```sh\n")
	block, _, closed := strings.Cut(rest, "```")
	if !found || !fenced || !closed {
		t.Fatal("the guide has no first session in a sh block")
	}
	var commands [][]string
	for _, line := range strings.Split(strings.TrimSpace(block), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "plugyard" {
			t.Fatalf("the first session has a line that is not a plugyard command: %q", line)
		}
		for i, f := range fields {
			fields[i] = strings.Trim(f, "'")
		}
		commands = append(commands, fields[1:])
	}
	return commands
}

func TestAgentsPrintsTheGuide(t *testing.T) {
	guide := useGuide(t)
	if out := mustRun(t, "agents"); out != guide {
		t.Errorf("agents printed something other than llms.txt:\n%s", out)
	}

	// The help of the root command, and only that, ends by naming it.
	for _, args := range [][]string{{}, {"--help"}, {"help"}} {
		if out := mustRun(t, args...); !strings.HasSuffix(out, "\n\n"+agentsHint+"\n") {
			t.Errorf("plugyard %v does not end with the line for AI agents:\n%s", args, out)
		}
	}
	if out := mustRun(t, "ps", "--help"); strings.Contains(out, "AI agents") {
		t.Errorf("the help of a command has the line for AI agents:\n%s", out)
	}

	// Every command the guide names exists.
	root := newRoot()
	for _, line := range strings.Split(guide, "\n") {
		for _, part := range strings.Split(line, "`") {
			fields := strings.Fields(part)
			if len(fields) < 2 || fields[0] != "plugyard" || strings.HasPrefix(fields[1], "<") {
				continue
			}
			if cmd, _, err := root.Find(fields[1:2]); err != nil || cmd == root {
				t.Errorf("the guide names %q, which is not a command", "plugyard "+fields[1])
			}
		}
	}

	// A build without the guide says where it is.
	AgentGuide = ""
	if code, _, stderr := run("agents"); code != ExitFailure || !strings.Contains(stderr, "llms.txt") {
		t.Errorf("agents without a guide: exit %d, stderr %q", code, stderr)
	}
}

// The first session of the guide is run as it is written, so that what an
// agent is told to do keeps working.
func TestTheFirstSessionOfTheGuideWorks(t *testing.T) {
	session := firstSession(t, useGuide(t))
	useTempDataDir(t)
	t.Setenv(asCLIEnv, "1")
	t.Chdir(t.TempDir())
	csms := csmstest.Start(t)
	t.Cleanup(func() { run("site", "down") })

	var logs string
	for _, args := range session {
		for i, a := range args {
			if a == "ws://localhost:9000/ocpp" {
				args[i] = csms.URL()
			}
		}
		code, stdout, stderr := run(args...)
		if code != ExitOK {
			t.Fatalf("plugyard %s: exit %d, stdout %q, stderr %q", strings.Join(args, " "), code, stdout, stderr)
		}
		if args[0] == "logs" {
			logs = stdout
		}
	}

	// What the guide says about a log line holds.
	started := false
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var entry struct {
			ChargePointID string            `json:"chargePointId"`
			Kind          string            `json:"kind"`
			Message       []json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.ChargePointID != "CP-1" {
			t.Fatalf("a log line is not what the guide says: %v\n%s", err, line)
		}
		if entry.Kind == "sent" && len(entry.Message) == 4 && string(entry.Message[2]) == `"StartTransaction"` {
			started = true
		}
	}
	if !started {
		t.Errorf("the log has no StartTransaction that was sent:\n%s", logs)
	}
	csms.WaitForCalls(t, "CP-1", v16.ActionStopTransaction, 1)
	if _, stdout, _ := run("ps"); strings.Contains(stdout, "CP-1") {
		t.Errorf("chargers are still listed after the session:\n%s", stdout)
	}
}

// fakeSkillsTool stands in for Node.js, and records how the skills tool
// was run.
type fakeSkillsTool struct {
	missing bool
	fail    error
	dirs    []string
	args    [][]string
}

func (f *fakeSkillsTool) use(t *testing.T, terminal bool) {
	t.Helper()
	oldFind, oldRun, oldTerminal := findNpx, runSkillsTool, stdinIsTerminal
	findNpx = func() (string, error) {
		if f.missing {
			return "", errors.New("not found")
		}
		return "npx", nil
	}
	runSkillsTool = func(_ context.Context, _, dir string, args []string, out, _ io.Writer) error {
		f.dirs, f.args = append(f.dirs, dir), append(f.args, args)
		io.WriteString(out, "the skills tool ran\n")
		return f.fail
	}
	stdinIsTerminal = func() bool { return terminal }
	t.Cleanup(func() { findNpx, runSkillsTool, stdinIsTerminal = oldFind, oldRun, oldTerminal })
}

func TestInitAgents(t *testing.T) {
	// Plain init says that the skill exists.
	t.Chdir(t.TempDir())
	if out := mustRun(t, "init"); !strings.Contains(out, "  plugyard init --agents               add a skill, so an AI agent working here knows plugyard\n") {
		t.Errorf("init does not offer the skill:\n%s", out)
	}

	// In a folder that has the files, only the skill is installed.
	tool := &fakeSkillsTool{}
	tool.use(t, true)
	if err := os.WriteFile("site.yaml", []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, "init", "--agents")
	want := "site.yaml, Chargerfile and scenario.yaml are here already and left alone.\n" +
		"Running: npx skills add Amraa1/plugyard\nthe skills tool ran\n"
	if out != want {
		t.Errorf("init --agents over existing files printed:\n%s\nwant:\n%s", out, want)
	}
	if data, _ := os.ReadFile("site.yaml"); string(data) != "mine" {
		t.Error("init --agents overwrote site.yaml")
	}
	if len(tool.dirs) != 1 || tool.dirs[0] != "." || strings.Join(tool.args[0], " ") != "skills add Amraa1/plugyard" {
		t.Errorf("the skills tool was run in %v with %v", tool.dirs, tool.args)
	}

	// In an empty folder the files come first, then the skill. With
	// nobody to answer, nothing may ask.
	tool = &fakeSkillsTool{}
	tool.use(t, false)
	dir := filepath.Join(t.TempDir(), "my-site")
	out = mustRun(t, "init", dir, "--agents")
	want = "Wrote site.yaml, Chargerfile and scenario.yaml.\n" +
		"Running: npx --yes skills add Amraa1/plugyard --yes\nthe skills tool ran\n\nNext:\n  cd " + dir + "\n  plugyard site up"
	if !strings.HasPrefix(out, want) || strings.Contains(out, "init --agents") {
		t.Errorf("init --agents printed:\n%s\nwant it to start with:\n%s", out, want)
	}
	if len(tool.dirs) != 1 || tool.dirs[0] != dir || !isThere(filepath.Join(dir, "site.yaml")) {
		t.Errorf("the skills tool was run in %v; site.yaml written: %v", tool.dirs, isThere(filepath.Join(dir, "site.yaml")))
	}

	// Without Node.js nothing is written.
	tool = &fakeSkillsTool{missing: true}
	tool.use(t, true)
	dir = filepath.Join(t.TempDir(), "other")
	code, stdout, stderr := run("init", dir, "--agents")
	if code != ExitFailure || stdout != "" || !strings.Contains(stderr, "npx was not found") ||
		!strings.Contains(stderr, "nothing was written") || !strings.Contains(stderr, skillURL) {
		t.Errorf("init --agents without npx: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if isThere(dir) || len(tool.dirs) != 0 {
		t.Error("init --agents without npx wrote something")
	}

	// A skills tool that fails is reported.
	tool = &fakeSkillsTool{fail: errors.New("exit status 1")}
	tool.use(t, true)
	code, _, stderr = run("init", dir, "--agents")
	if code != ExitFailure || !strings.Contains(stderr, `the skill was not installed: exit status 1. Run "npx skills add Amraa1/plugyard" yourself`) {
		t.Errorf("init --agents with a failing skills tool: exit %d, stderr %q", code, stderr)
	}
}

func TestRunDetached(t *testing.T) {
	useTempDataDir(t)
	t.Setenv(asCLIEnv, "1")
	csms := csmstest.Start(t)
	path := writeChargerfile(t, testChargerfile)
	t.Cleanup(func() { run("stop", "CP-1") })

	code, stdout, stderr := run("run", path, "--csms", csms.URL(), "--id", "CP-1", "--rm", "-d")
	if code != ExitUsage || !strings.Contains(stderr, "use one of --detach and --rm") {
		t.Errorf("run --rm -d: exit %d, stderr %q", code, stderr)
	}

	// It returns at once, and the charger runs on.
	code, stdout, stderr = run("run", path, "--csms", csms.URL(), "--id", "CP-1", "--name", "cp-1", "-d")
	if code != ExitOK || !strings.HasPrefix(stdout, "Charger CP-1 is running in the background (process ") ||
		!strings.HasSuffix(stdout, ")\nFollow its log with: plugyard logs CP-1 -f\nShut it down with:   plugyard stop CP-1\n") {
		t.Fatalf("run -d: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	mustRun(t, "wait", "CP-1", "online", "--timeout", "10s")
	if row := psRow(t, "CP-1"); row != "CP-1 cp-1 CP-1 online 1:Available" {
		t.Errorf("ps row = %q", row)
	}
	if logs := mustRun(t, "logs", "CP-1"); !strings.Contains(logs, "BootNotification") {
		t.Errorf("the charger in the background has no log:\n%s", logs)
	}

	// A second one of the same ID is refused, and stop ends it.
	code, _, stderr = run("run", path, "--csms", csms.URL(), "--id", "CP-1", "-d")
	if code != ExitFailure || !strings.Contains(stderr, "already running") {
		t.Errorf("a second run -d: exit %d, stderr %q", code, stderr)
	}
	mustRun(t, "stop", "CP-1")
	mustRun(t, "wait", "CP-1", "stopped", "--timeout", "10s")
	csms.WaitDisconnects(t, "CP-1", 1)
}

func isThere(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
