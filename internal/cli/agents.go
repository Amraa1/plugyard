package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// AgentGuide is the guide "plugyard agents" prints: llms.txt at the root of
// the repository, which the main package builds in.
var AgentGuide string

// agentsHint is the last line of "plugyard --help". An AI agent reads the
// help first, and this sends it to the guide.
const agentsHint = `AI agents: run "plugyard agents" for a guide written for you.`

// Where the skill for AI agents lives, and the command of the skills tool
// (https://skills.sh) that installs it into a project.
const (
	skillSource = "Amraa1/plugyard"
	skillURL    = "https://github.com/Amraa1/plugyard/tree/main/skills/plugyard"
)

func newAgents() *cobra.Command {
	return &cobra.Command{
		Use:   "agents",
		Short: "Print a guide for AI agents that drive plugyard",
		Long: "Print a guide written for AI agents, such as coding assistants, that\n" +
			"run plugyard for a user: the rules that keep a session from hanging, a\n" +
			"complete first session, how to read the output, and where to look next.\n" +
			"It matches the installed version.\n\n" +
			"\"plugyard init --agents\" installs a skill into a project, so that an\n" +
			"agent working there knows plugyard and this guide.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if AgentGuide == "" {
				return errors.New("this build of plugyard has no guide built in; read https://github.com/Amraa1/plugyard/blob/main/llms.txt")
			}
			_, err := io.WriteString(cmd.OutOrStdout(), AgentGuide)
			return err
		},
	}
}

// These are variables so that tests can stand in for Node.js.
var (
	// findNpx reports where npx is, the program of Node.js that runs a
	// package without installing it.
	findNpx = func() (string, error) { return exec.LookPath("npx") }

	// runSkillsTool runs the skills tool in a folder, on the terminal of
	// the user, who it may ask which agents to install the skill for.
	runSkillsTool = func(ctx context.Context, npx, dir string, args []string, out, errOut io.Writer) error {
		tool := exec.CommandContext(ctx, npx, args...)
		tool.Dir = dir
		tool.Stdin, tool.Stdout, tool.Stderr = os.Stdin, out, errOut
		return tool.Run()
	}
)

// skillsArgs is what npx is given to install the skill. With nobody to
// answer, neither npx nor the skills tool may ask.
func skillsArgs(interactive bool) []string {
	if interactive {
		return []string{"skills", "add", skillSource}
	}
	return []string{"--yes", "skills", "add", skillSource, "--yes"}
}

// errNoNpx says what to do on a machine without Node.js.
func errNoNpx() error {
	return fmt.Errorf("npx was not found, and it is what installs the skill; nothing was written. "+
		"Install Node.js (https://nodejs.org) and run this again, or copy the skill by hand from %s", skillURL)
}

// installSkill installs the skill for AI agents into a folder.
func installSkill(ctx context.Context, npx, dir string, out, errOut io.Writer) error {
	args := skillsArgs(stdinIsTerminal())
	fmt.Fprintf(out, "Running: npx")
	for _, a := range args {
		fmt.Fprintf(out, " %s", a)
	}
	fmt.Fprintln(out)
	if err := runSkillsTool(ctx, npx, dir, args, out, errOut); err != nil {
		return fmt.Errorf("the skill was not installed: %w. Run \"npx skills add %s\" yourself to see why", err, skillSource)
	}
	return nil
}
