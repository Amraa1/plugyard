package cli

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/api"
	"github.com/Amraa1/plugyard/schemas"
)

// The guide is the first thing an agent reads, and what it does not name an
// agent does not look for. These tests fail when a command, an action or a
// setting of a file is added and the guide is not told.

// codeOf returns what the guide writes as code: the text between backticks,
// and the lines of its code blocks.
func codeOf(guide string) []string {
	var code []string
	for i, block := range strings.Split(guide, "```") {
		if i%2 == 1 {
			code = append(code, strings.Split(block, "\n")...)
			continue
		}
		for j, span := range strings.Split(block, "`") {
			if j%2 == 1 {
				code = append(code, span)
			}
		}
	}
	return code
}

// named reports whether the guide writes word as code, alone or among others.
func named(code []string, word string) bool {
	whole := regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(word) + `($|[^A-Za-z0-9_-])`)
	for _, c := range code {
		if whole.MatchString(c) {
			return true
		}
	}
	return false
}

func TestTheGuideNamesEveryCommand(t *testing.T) {
	code := codeOf(useGuide(t))

	var check func(cmd *cobra.Command, path string)
	check = func(cmd *cobra.Command, path string) {
		for _, sub := range cmd.Commands() {
			if sub.Hidden || sub.Name() == "help" {
				continue
			}
			name := strings.TrimSpace(path + " " + sub.Name())
			if !named(code, name) {
				t.Errorf("the guide does not name the command %q", "plugyard "+name)
			}
			// The charger suite repeats the short commands under one name.
			if sub.Name() != "charger" {
				check(sub, name)
			}
		}
	}
	check(newRoot(), "")
}

func TestTheGuideNamesEveryAction(t *testing.T) {
	code := codeOf(useGuide(t))
	if len(api.Actions) == 0 {
		t.Fatal("there are no actions to look for")
	}
	for _, action := range api.Actions {
		if !named(code, action) {
			t.Errorf("the guide does not name the action %q", action)
		}
	}
}

// settings returns the name of every setting a schema describes, at any depth.
func settings(t *testing.T, kind string) []string {
	t.Helper()
	data, err := schemas.Get(kind)
	if err != nil {
		t.Fatal(err)
	}
	var schema any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("the %s schema: %v", kind, err)
	}
	seen := map[string]bool{}
	var names []string
	var walk func(node any)
	walk = func(node any) {
		switch node := node.(type) {
		case map[string]any:
			if properties, ok := node["properties"].(map[string]any); ok {
				for name := range properties {
					if !seen[name] {
						seen[name] = true
						names = append(names, name)
					}
				}
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(schema)
	if len(names) == 0 {
		t.Fatalf("the %s schema describes no settings", kind)
	}
	return names
}

func TestTheGuideNamesEverySettingOfTheFiles(t *testing.T) {
	code := codeOf(useGuide(t))
	for _, kind := range schemas.Kinds() {
		for _, name := range settings(t, kind) {
			if !named(code, name) {
				t.Errorf("the guide does not name %q, a setting of the %s file", name, kind)
			}
		}
	}
}

func TestAMistakeNamesTheHelpOfItsCommand(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		// A misspelled command is answered with the ones it comes close to.
		{[]string{"exce", "CP-1", "plug"}, `unknown command "exce" for "plugyard"; did you mean "exec"?` + "\nRun 'plugyard --help' for usage.\n"},
		{[]string{"site", "upp"}, `unknown command "upp" for "plugyard site"; did you mean "up"?` + "\nRun 'plugyard site --help' for usage.\n"},
		{[]string{"nosuchcommand"}, `unknown command "nosuchcommand" for "plugyard"` + "\nRun 'plugyard --help' for usage.\n"},
		// The help that is named is the one of the command that was refused.
		{[]string{"site", "up", "--detatch"}, "unknown flag: --detatch\nRun 'plugyard site up --help' for usage.\n"},
		{[]string{"exec", "CP-1", "tap"}, "Run 'plugyard exec --help' for usage.\n"},
	}
	for _, c := range cases {
		code, _, stderr := run(c.args...)
		if code != ExitUsage || !strings.HasSuffix(stderr, c.want) {
			t.Errorf("plugyard %s: exit %d, stderr %q, want it to end with %q", strings.Join(c.args, " "), code, stderr, c.want)
		}
	}
}
