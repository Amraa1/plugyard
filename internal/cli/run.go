package cli

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/datadir"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/site"
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
	)

	cmd := &cobra.Command{
		Use:   "run <Chargerfile> --csms <url> --id <charge point ID>",
		Short: "Start one charger without a site file",
		Long: "Start one charger from a Chargerfile and keep it running in the foreground,\n" +
			"printing its events and OCPP messages. Press Ctrl+C to shut it down.\n\n" +
			"The charge point ID is appended to the CSMS URL: with --csms ws://host/ocpp\n" +
			"and --id CP-1 the charger connects to ws://host/ocpp/CP-1.",
		Example: "  plugyard run Chargerfile --csms ws://localhost:9000/ocpp --id CP-1",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := model.Load(args[0])
			if err != nil {
				return &UsageError{Err: err}
			}

			if password == "" {
				password = os.Getenv(passwordEnv)
			}

			c, err := charger.New(charger.Config{
				ID:    id,
				Model: m,
				CSMS: charger.CSMS{
					URL:                csmsURL,
					Password:           password,
					InsecureSkipVerify: insecureSkipVerify,
				},
				Speed: speed,
				Log:   newLogPrinter(cmd.OutOrStdout()),
			})
			if err != nil {
				return &UsageError{Err: err}
			}

			// A charger started with run lives in an implicit site named
			// after its charge point ID. Serving the site's socket is what
			// lets ps and exec reach it from another terminal.
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			server, err := control.Serve(dir, site.New(id, c))
			if err != nil {
				return err
			}
			defer server.Close()

			return c.Run(cmd.Context())
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&csmsURL, "csms", "", "CSMS URL, ws:// or wss:// (required)")
	flags.StringVar(&id, "id", "", "charge point ID (required)")
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
func newLogPrinter(w io.Writer) func(charger.Entry) {
	var mu sync.Mutex
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
		fmt.Fprintf(w, "%s %s %s %s\n", e.Time.Format("2006-01-02T15:04:05.000Z07:00"), e.ChargePointID, marker, e.Text)
	}
}
