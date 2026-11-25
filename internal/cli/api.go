package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/api"
	"github.com/Amraa1/plugyard/internal/buildinfo"
	"github.com/Amraa1/plugyard/internal/datadir"
)

func newAPI() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "api",
		Short: "Serve the HTTP API, for callers with no shell on this machine",
		Long: "The API lets someone who has no shell on this machine act on chargers:\n" +
			"plug in, unplug, tap, fault, go offline, and read their state. It serves\n" +
			"every charger on the machine, whichever site it belongs to, as \"plugyard\n" +
			"exec\" and \"plugyard ps\" do.\n\n" +
			"It listens on this machine only unless told otherwise, and listening\n" +
			"beyond it needs a token. Everything that is not an action on a charger,\n" +
			"such as starting sites, stays on the command line.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return &UsageError{Err: fmt.Errorf("unknown command %q for \"plugyard api\"", args[0])}
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newAPIUp(), newAPIDown(), newAPIStatus(), newAPILogs(), newAPISpec(), newAPIToken())
	return cmd
}

// isRoot reports whether plugyard runs as the superuser. It is a variable
// so that tests can say so.
var isRoot = func() bool { return os.Geteuid() == 0 }

func newAPIUp() *cobra.Command {
	var (
		listen  string
		detach  bool
		tlsCert string
		tlsKey  string
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start serving the API",
		Long: "Start serving the API and print every call as it comes in, until Ctrl+C.\n" +
			"With -d it runs in the background and the command returns.\n\n" +
			"It listens on " + api.DefaultListen + ", which only this machine can reach. Calls\n" +
			"from this machine need no token as long as none exists. To listen on\n" +
			"another address, make a token first with \"plugyard api token create\":\n" +
			"without one, plugyard refuses.\n\n" +
			"--tls-cert and --tls-key serve HTTPS. Without them a token travels\n" +
			"unencrypted, which is only safe on this machine or behind a reverse proxy\n" +
			"that adds encryption.\n\n" +
			"Sites may come and go while the API runs: it serves whatever chargers\n" +
			"there are at the moment of a call.\n\n" +
			"AI agents use the same reading and actions as tools, over the Model\n" +
			"Context Protocol (MCP), at the address of the API followed by " + api.MCPPath + ",\n" +
			"with the same tokens.",
		Example: "  plugyard api up\n" +
			"  plugyard api up -d\n" +
			"  plugyard api up -d --listen 0.0.0.0:7070",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			if (tlsCert == "") != (tlsKey == "") {
				return &UsageError{Err: errors.New("--tls-cert and --tls-key go together: give both or neither")}
			}

			if detach {
				if running, err := api.Probe(ctx, dir); err == nil {
					return fmt.Errorf("%w, at %s (process %d)", api.ErrAlreadyRunning, running.URL(), running.PID)
				}
				args := []string{"api", "up", "--listen", listen}
				if tlsCert != "" {
					// The background copy starts in another folder.
					for _, file := range []*string{&tlsCert, &tlsKey} {
						if *file, err = filepath.Abs(*file); err != nil {
							return err
						}
					}
					args = append(args, "--tls-cert", tlsCert, "--tls-key", tlsKey)
				}
				info, err := startAPIBackground(cmd, dir, args)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "The API is running in the background at %s (process %d)\n", info.URL(), info.PID)
				fmt.Fprintf(out, "AI agents use:      %s%s\n", info.URL(), api.MCPPath)
				fmt.Fprintln(out, "See its calls with: plugyard api logs")
				fmt.Fprintln(out, "Stop it with:       plugyard api down")
				return nil
			}

			server, err := api.Serve(api.Config{
				DataDir: dir,
				Listen:  listen,
				TLSCert: tlsCert,
				TLSKey:  tlsKey,
				Backend: apiBackend{},
				Version: buildinfo.Version(),
				Out:     out,
			})
			if err != nil {
				return err
			}
			defer server.Close()

			announceAPI(cmd, server, dir)

			select {
			case <-ctx.Done():
			case <-server.Stopped():
				fmt.Fprintln(out, "The API was asked to stop")
			case <-server.Done():
				return errors.New("the API stopped serving")
			}
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&listen, "listen", api.DefaultListen, "the address to listen on")
	flags.BoolVarP(&detach, "detach", "d", false, "run the API in the background")
	flags.StringVar(&tlsCert, "tls-cert", "", "the certificate file to serve HTTPS with")
	flags.StringVar(&tlsKey, "tls-key", "", "the key file of that certificate")
	return cmd
}

// announceAPI says where an API that has just started listens, who may
// call it, and what is unsafe about how it runs.
func announceAPI(cmd *cobra.Command, server *api.Server, dir string) {
	out, info := cmd.OutOrStdout(), server.Info()
	tokens, _ := api.LoadTokens(dir)
	fmt.Fprintf(out, "The API is listening at %s\n", info.URL())
	fmt.Fprintf(out, "AI agents use its tools at %s%s\n", info.URL(), api.MCPPath)
	switch {
	case len(tokens) > 0:
		fmt.Fprintf(out, "Every call needs a token (%s)\n", countTokens(len(tokens)))
	default:
		fmt.Fprintln(out, "Calls from this machine need no token. Other machines cannot reach it.")
	}
	warnings := cmd.ErrOrStderr()
	if !server.Local() && !info.TLS {
		fmt.Fprintln(warnings, "warning: tokens travel unencrypted. Put a reverse proxy with HTTPS in front, or give --tls-cert and --tls-key.")
	}
	if isRoot() {
		fmt.Fprintln(warnings, "warning: the API runs as root. Run it as a user of its own, so that a flaw in it reaches no further than plugyard's data.")
	}
}

func countTokens(n int) string {
	if n == 1 {
		return "1 token"
	}
	return strconv.Itoa(n) + " tokens"
}

// startAPIBackground starts a background copy of plugyard that serves the
// API, and waits until it answers.
func startAPIBackground(cmd *cobra.Command, dir string, args []string) (api.Info, error) {
	var none api.Info
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return none, err
	}
	// What the background copy prints, which is only what goes wrong with
	// it, goes to a file that starts empty each time.
	outPath := filepath.Join(dir, "api.out")
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return none, err
	}
	defer outFile.Close()

	exe, err := os.Executable()
	if err != nil {
		return none, err
	}
	child := exec.Command(exe, args...)
	child.Stdout, child.Stderr = outFile, outFile
	child.Dir = dir
	setDetached(child)
	if err := child.Start(); err != nil {
		return none, fmt.Errorf("cannot start the background process: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	ctx := cmd.Context()
	deadline := time.After(detachTimeout)
	for {
		if info, err := api.Probe(ctx, dir); err == nil {
			return info, nil
		}
		select {
		case <-exited:
			return none, fmt.Errorf("the API did not start:\n%s", strings.TrimSpace(tailFrom(outPath, 0, 20)))
		case <-deadline:
			return none, fmt.Errorf("the API did not report as running within %s; see %s", detachTimeout, outPath)
		case <-ctx.Done():
			return none, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func newAPIDown() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop serving the API",
		Long: "Stop the API, whether it runs in the background or in the foreground of\n" +
			"another terminal. Chargers and sites are not touched.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			info, err := api.Probe(cmd.Context(), dir)
			if err != nil {
				return errors.New("the API is not running")
			}
			if err := api.Shutdown(cmd.Context(), dir); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "The API at %s is stopped\n", info.URL())
			return nil
		},
	}
}

// apiStatus is what "plugyard api status" reports.
type apiStatus struct {
	Running bool `json:"running"`
	// URL is where the API listens; empty when it is not running.
	URL string `json:"url,omitempty"`
	// MCPURL is where AI agents reach the tools of the API.
	MCPURL  string     `json:"mcpUrl,omitempty"`
	PID     int        `json:"pid,omitempty"`
	Since   *time.Time `json:"since,omitempty"`
	TLS     bool       `json:"tls"`
	Version string     `json:"version,omitempty"`
	// Tokens is how many tokens exist.
	Tokens int `json:"tokens"`
	// TokenNeeded says whether a call has to carry a token.
	TokenNeeded bool `json:"tokenNeeded"`
}

func newAPIStatus() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Say whether the API is running, and where",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			tokens, err := api.LoadTokens(dir)
			if err != nil {
				return err
			}
			status := apiStatus{Tokens: len(tokens), TokenNeeded: len(tokens) > 0}
			if info, err := api.Probe(cmd.Context(), dir); err == nil {
				status.Running, status.URL, status.PID, status.TLS, status.Version = true, info.URL(), info.PID, info.TLS, info.Version
				status.MCPURL = info.URL() + api.MCPPath
				status.Since = &info.Since
			}

			out := cmd.OutOrStdout()
			if format == formatJSON {
				return json.NewEncoder(out).Encode(status)
			}
			if !status.Running {
				fmt.Fprintf(out, "The API is not running. Start it with: plugyard api up -d\n")
				return nil
			}
			need := "calls from this machine need no token"
			if status.TokenNeeded {
				need = "every call needs a token (" + countTokens(status.Tokens) + ")"
			}
			encrypted := "not encrypted (HTTP)"
			if status.TLS {
				encrypted = "encrypted (HTTPS)"
			}
			writeTable(out, [][]cell{
				{plain("Address"), plain(status.URL)},
				{plain("Agents"), plain(status.MCPURL + " (MCP)")},
				{plain("Process"), plain(strconv.Itoa(status.PID))},
				{plain("Since"), plain(status.Since.Local().Format("2006-01-02 15:04:05"))},
				{plain("Tokens"), plain(need)},
				{plain("Traffic"), plain(encrypted)},
			})
			return nil
		},
	}
	addFormatFlag(cmd, &format)
	return cmd
}

func newAPILogs() *cobra.Command {
	var (
		format  string
		charger string
		tail    int
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show who called the API, and what for",
		Long: "Show the calls the API received, oldest first: the time, the name of the\n" +
			"token, the address the call came from, how it was answered, and what it\n" +
			"did to which charger. Calls that were turned away are listed too. A token\n" +
			"itself is never recorded.\n\n" +
			"--charger shows only the calls about one charge point ID, which answers\n" +
			"who acted on it.",
		Example: "  plugyard api logs\n" +
			"  plugyard api logs --charger CP-1 --tail 20",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			entries, err := api.ReadLog(dir)
			if err != nil {
				return err
			}
			if charger != "" {
				entries = slices.DeleteFunc(entries, func(e api.Entry) bool { return !slices.Contains(e.Chargers, charger) })
			}
			if tail >= 0 && len(entries) > tail {
				entries = entries[len(entries)-tail:]
			}
			out := cmd.OutOrStdout()
			if format == formatJSON {
				return writeJSONLines(out, entries)
			}
			for _, e := range entries {
				fmt.Fprintln(out, e.Line())
			}
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&charger, "charger", "", "only calls about this charge point ID")
	flags.IntVar(&tail, "tail", -1, "show only the last N calls (default: all)")
	addFormatFlag(cmd, &format)
	return cmd
}

func newAPISpec() *cobra.Command {
	return &cobra.Command{
		Use:   "spec",
		Short: "Print the description of the API, as an OpenAPI document",
		Long: "Print every address of the API with what it takes and answers, as an\n" +
			"OpenAPI 3 document in JSON. A running API serves the same document at\n" +
			"/v1/openapi.json.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(api.Spec)
			return err
		},
	}
}

func newAPIToken() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Make, list and remove the tokens callers of the API need",
		Long: "A token is what a caller of the API sends to show it may call. Each has a\n" +
			"name, which the log of calls shows, so give every team or tool its own.\n" +
			"Tokens are managed here only, never through the API.\n\n" +
			"As soon as one token exists, every call needs one, also from this machine.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return &UsageError{Err: fmt.Errorf("unknown command %q for \"plugyard api token\"", args[0])}
			}
			return cmd.Help()
		},
	}

	var (
		site     string
		readOnly bool
	)
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Make a token and show it, once",
		Long: "Make a token and print it. It is shown this once and cannot be shown\n" +
			"again: plugyard keeps only what it needs to recognize it. A caller sends\n" +
			"it as \"Authorization: Bearer <token>\".\n\n" +
			"--site limits the token to the chargers of one site. --read-only makes a\n" +
			"token that can look at chargers and not act on them.",
		Example: "  plugyard api token create mobile-team --site staging\n" +
			"  plugyard api token create dashboard --read-only",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			secret, err := api.CreateToken(dir, args[0], site, readOnly)
			if err != nil {
				return &UsageError{Err: err}
			}
			// The token alone goes to standard output, so that a script
			// can take it; the words around it go beside.
			fmt.Fprintln(cmd.OutOrStdout(), secret)
			about := cmd.ErrOrStderr()
			limits := "every site"
			if site != "" {
				limits = "site " + site
			}
			can := "can act on chargers and look at them"
			if readOnly {
				can = "can look at chargers and not act on them"
			}
			fmt.Fprintf(about, "Token %s is for %s, and %s.\n", args[0], limits, can)
			fmt.Fprintln(about, "It is shown this once: keep it somewhere safe. From now on every call to the API needs a token.")
			return nil
		},
	}
	create.Flags().StringVarP(&site, "site", "s", "", "limit the token to the chargers of this site")
	create.Flags().BoolVar(&readOnly, "read-only", false, "make a token that can look and not act")

	var format string
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the tokens, without showing them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			tokens, err := api.LoadTokens(dir)
			if err != nil {
				return err
			}
			return writeTokens(cmd.OutOrStdout(), tokens, format)
		},
	}
	addFormatFlag(ls, &format)

	rm := &cobra.Command{
		Use:   "rm <name>...",
		Short: "Remove tokens; they stop working at once",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			missing := 0
			for _, name := range args {
				found, err := api.RemoveToken(dir, name)
				switch {
				case err != nil:
					return err
				case found:
					fmt.Fprintf(cmd.OutOrStdout(), "Token %s is removed\n", name)
				default:
					missing++
					fmt.Fprintf(cmd.ErrOrStderr(), "plugyard: there is no token named %q\n", name)
				}
			}
			if missing > 0 {
				return errFailed
			}
			return nil
		},
	}

	cmd.AddCommand(create, ls, rm)
	return cmd
}

// tokenRow is a token as "plugyard api token ls" shows it: without what
// would let anyone use it.
type tokenRow struct {
	Name     string    `json:"name"`
	Site     string    `json:"site,omitempty"`
	ReadOnly bool      `json:"readOnly"`
	Created  time.Time `json:"created"`
}

func writeTokens(out io.Writer, tokens []api.Token, format string) error {
	rows := make([]tokenRow, 0, len(tokens))
	for _, t := range tokens {
		rows = append(rows, tokenRow{Name: t.Name, Site: t.Site, ReadOnly: t.ReadOnly, Created: t.Created})
	}
	if format == formatJSON {
		return writeJSONLines(out, rows)
	}
	table := [][]cell{{plain("NAME"), plain("SITE"), plain("CAN"), plain("CREATED")}}
	for _, r := range rows {
		site, can := "every site", "act and look"
		if r.Site != "" {
			site = r.Site
		}
		if r.ReadOnly {
			can = "look"
		}
		table = append(table, []cell{plain(r.Name), plain(site), plain(can), plain(r.Created.Local().Format("2006-01-02 15:04"))})
	}
	writeTable(out, table)
	return nil
}
