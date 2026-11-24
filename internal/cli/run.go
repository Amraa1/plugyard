package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/datadir"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/names"
	"github.com/Amraa1/plugyard/internal/store"
)

// passwordEnv is read when --password is not given, so that the password
// need not appear on the command line.
const passwordEnv = "PLUGYARD_CSMS_PASSWORD"

func newRun() *cobra.Command {
	var (
		csmsURL            string
		id                 string
		password           string
		insecureSkipVerify bool
		speed              float64
		name               string
		remove             bool
		yes                bool
		detach             bool
	)

	cmd := &cobra.Command{
		Use:   "run <Chargerfile> --csms <url> --id <charge point ID>",
		Short: "Start one charger without a site file",
		Long: "Start one charger from a Chargerfile and keep it running in the foreground,\n" +
			"printing its events and OCPP messages. Press Ctrl+C to shut it down. With\n" +
			"-d the charger runs in the background and the command returns at once;\n" +
			"\"plugyard stop\" shuts it down.\n\n" +
			"The charger's state is saved: running the same charge point ID again brings\n" +
			"back the same charger, with its meter and configuration, and \"plugyard rm\"\n" +
			"wipes it. With --rm nothing is saved.\n\n" +
			"The charge point ID is appended to the CSMS URL: with --csms ws://host/ocpp\n" +
			"and --id CP-1 the charger connects to ws://host/ocpp/CP-1.",
		Example: "  plugyard run Chargerfile --csms ws://localhost:9000/ocpp --id CP-1\n" +
			"  plugyard run Chargerfile --csms ws://localhost:9000/ocpp --id CP-1 -d",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if detach && remove {
				return &UsageError{Err: errors.New("--rm saves nothing, and a charger in the background runs from its saved state; use one of --detach and --rm")}
			}
			m, err := model.Load(args[0])
			if err != nil {
				return &UsageError{Err: err}
			}

			if password == "" {
				password = os.Getenv(passwordEnv)
			}

			csms := charger.CSMS{URL: csmsURL, Password: password, InsecureSkipVerify: insecureSkipVerify}
			if _, err := charger.New(charger.Config{ID: id, Model: m, CSMS: csms, Speed: speed}); err != nil {
				return &UsageError{Err: err}
			}
			if name != "" {
				if err := names.Validate(name); err != nil {
					return &UsageError{Err: fmt.Errorf("--name: %w", err)}
				}
			}

			// A charger started with run lives in an implicit site named
			// after its charge point ID. Serving the site's socket is what
			// lets ps and exec reach it from another terminal, and its saved
			// state is what brings the same charger back on the next run.
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			meta := store.Meta{Name: id, Implicit: true, Speed: speed}
			want := []desired{{
				ID:   id,
				Name: name,
				Spec: store.Spec{
					Model:              m,
					CSMSURL:            csmsURL,
					InsecureSkipVerify: insecureSkipVerify,
					HasPassword:        password != "",
				},
			}}
			st, err := prepare(cmd.Context(), cmd, dir, meta, want, yes, remove)
			if err != nil {
				return err
			}
			if detach {
				st.Close()
				// The background copy reads the password from the
				// environment, since it is never saved.
				if password != "" {
					os.Setenv(passwordEnv, password)
				}
				info, err := startBackground(cmd.Context(), dir, id)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "Charger %s is running in the background (process %d)\n", id, info.PID)
				fmt.Fprintf(out, "Follow its log with: plugyard logs %s -f\n", id)
				fmt.Fprintf(out, "Shut it down with:   plugyard stop %s\n", id)
				return nil
			}
			return serveSaved(cmd.Context(), dir, st, cmd.OutOrStdout(), !remove, func(string) string { return password })
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&csmsURL, "csms", "", "CSMS URL, ws:// or wss:// (required)")
	flags.StringVar(&id, "id", "", "charge point ID (required)")
	flags.StringVar(&name, "name", "", "name for the charger, unique on this machine (default: one is made up)")
	flags.BoolVarP(&detach, "detach", "d", false, "run the charger in the background")
	flags.BoolVar(&remove, "rm", false, "save nothing: the charger leaves no trace when it stops")
	flags.BoolVarP(&yes, "yes", "y", false, "reset saved state that no longer fits the Chargerfile, without asking")
	flags.Float64Var(&speed, "speed", 1, "multiply the rate of EV charging and energy accumulation")
	flags.StringVar(&password, "password", "", "password for HTTP Basic authentication; defaults to $"+passwordEnv)
	flags.BoolVar(&insecureSkipVerify, "insecure-skip-verify", false, "do not verify the CSMS's TLS certificate")
	_ = cmd.MarkFlagRequired("csms")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

// newLogPrinter returns a charger log function that writes one line per
// entry to w:
//
//	2026-01-02T15:04:05.000+01:00 CP-1 | connected to ws://...
//	2026-01-02T15:04:05.010+01:00 CP-1 > [2,"...","BootNotification",{...}]
//	2026-01-02T15:04:05.020+01:00 CP-1 < [3,"...",{...}]
//
// On a terminal each charge point ID gets a color of its own, so that the
// interleaved logs of many chargers can be told apart.
func newLogPrinter(w io.Writer) func(charger.Entry) {
	var mu sync.Mutex
	p := newPalette(w)
	return func(e charger.Entry) {
		marker := "|"
		switch e.Kind {
		case charger.MessageSent:
			marker = ">"
		case charger.MessageReceived:
			marker = "<"
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "%s %s %s %s\n", e.Time.Format("2006-01-02T15:04:05.000Z07:00"), p.id(e.ChargePointID), marker, e.Text)
	}
}
