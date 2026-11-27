package cli

import (
	"github.com/spf13/cobra"
)

// newCharger is the suite of charger commands. As in Docker, where
// "docker container ls" is "docker ps", each command of the suite is one
// that also exists by its short name, made by the same function, so the
// two cannot behave differently.
func newCharger() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "charger",
		Short: "Manage chargers: the charger commands as a suite",
		Long: "The commands that act on chargers, gathered under one name beside\n" +
			"\"plugyard site\". Each one is the same as the short command: \"plugyard charger ls\"\n" +
			"is \"plugyard ps\", \"plugyard charger exec\" is \"plugyard exec\", and so on.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return unknownCommand(cmd, args[0])
			}
			return cmd.Help()
		},
	}

	// "ls" is what a suite calls its listing, as "site ls" does.
	ls := newPs()
	ls.Use = "ls"
	ls.Short = "List running chargers (the same as \"plugyard ps\")"

	cmd.AddCommand(ls, newExec(), newInspect(), newLogs(), newRun(), newStats(), newWait())
	cmd.AddCommand(newPowerCommands()...)
	return cmd
}
