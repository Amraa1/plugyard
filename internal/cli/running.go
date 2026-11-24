package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/api"
	"github.com/Amraa1/plugyard/internal/store"
)

// liveSite is a site that has a process, and which of its chargers are
// powered on.
type liveSite struct {
	name    string
	powered []string
}

// running is what holds the program open: the sites that have a process,
// and the API. "plugyard update" and "plugyard uninstall" stop them first,
// and update starts them again.
type running struct {
	sites []liveSite
	api   *api.Info
	// transactions counts the transactions that stopping the sites ends.
	transactions int
}

// findRunning looks for sites and an API that are running.
func findRunning(ctx context.Context, dataDir string) (running, error) {
	var r running
	w, err := loadWorld(ctx, "")
	if err != nil {
		return r, err
	}
	defer w.close()
	for _, client := range w.clients {
		site := liveSite{name: client.Info.Site}
		for _, t := range w.targets {
			if t.state.Site != site.name || !t.state.Powered {
				continue
			}
			site.powered = append(site.powered, t.state.ChargePointID)
			for _, k := range t.state.Connectors {
				if k.TransactionID != 0 {
					r.transactions++
				}
			}
		}
		r.sites = append(r.sites, site)
	}
	if info, err := api.Probe(ctx, dataDir); err == nil {
		r.api = &info
	}
	return r, nil
}

func (r running) any() bool { return len(r.sites) > 0 || r.api != nil }

// describe names what is running, as a sentence lists it.
func (r running) describe() string {
	var things []string
	for _, s := range r.sites {
		things = append(things, fmt.Sprintf("site %s (%s)", s.name, countChargers(len(s.powered))))
	}
	if r.api != nil {
		things = append(things, "the API at "+r.api.URL())
	}
	return joinWords(things)
}

// ending says what stopping costs, or nothing when it costs nothing.
func (r running) ending() string {
	switch r.transactions {
	case 0:
		return ""
	case 1:
		return " 1 transaction ends with StopTransaction."
	default:
		return fmt.Sprintf(" %d transactions end with StopTransaction.", r.transactions)
	}
}

// stop shuts down every site, in an orderly way, and the API.
func (r running) stop(ctx context.Context, dataDir string, out io.Writer) error {
	for _, s := range r.sites {
		if _, err := shutDownSite(ctx, dataDir, s.name); err != nil {
			return err
		}
		fmt.Fprintf(out, "Site %s is stopped\n", s.name)
	}
	if r.api != nil {
		if err := api.Shutdown(ctx, dataDir); err != nil {
			return fmt.Errorf("the API did not stop: %w", err)
		}
		fmt.Fprintf(out, "The API at %s is stopped\n", r.api.URL())
	}
	return nil
}

// start brings back what stop took down, in the background: every site
// with the chargers that were powered on, and the API on its address. It
// carries on after a failure, and returns what did not come back.
func (r running) start(cmd *cobra.Command, dataDir string, out io.Writer) []string {
	ctx := cmd.Context()
	var failed []string
	for _, s := range r.sites {
		err := func() error {
			st, err := store.OpenExisting(dataDir, s.name)
			if err != nil {
				return err
			}
			// The same chargers as before, not every one of the site.
			err = st.SetPower(s.powered)
			st.Close()
			if err != nil {
				return err
			}
			_, err = startBackground(ctx, dataDir, s.name)
			return err
		}()
		if err != nil {
			failed = append(failed, fmt.Sprintf("site %s did not start again: %v. Start it with \"plugyard site start -s %s\"", s.name, strings.TrimSpace(err.Error()), s.name))
			continue
		}
		fmt.Fprintf(out, "Site %s is running again, in the background, with %s\n", s.name, countChargers(len(s.powered)))
	}
	if r.api != nil {
		args := []string{"api", "up", "--listen", r.api.Listen}
		if r.api.TLSCert != "" {
			args = append(args, "--tls-cert", r.api.TLSCert, "--tls-key", r.api.TLSKey)
		}
		if info, err := startAPIBackground(cmd, dataDir, args); err != nil {
			failed = append(failed, fmt.Sprintf("the API did not start again: %v. Start it with \"plugyard %s -d\"", strings.TrimSpace(err.Error()), strings.Join(args, " ")))
		} else {
			fmt.Fprintf(out, "The API is running again, in the background, at %s\n", info.URL())
		}
	}
	return failed
}
