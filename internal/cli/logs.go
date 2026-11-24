package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/logfile"
	"github.com/Amraa1/plugyard/internal/store"
)

// logTimeLayout is how a log line shows its time.
const logTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// logPollInterval is how often a followed log is read again.
const logPollInterval = 200 * time.Millisecond

// logOptions are the flags the log commands share.
type logOptions struct {
	follow bool
	events bool
	since  string
	tail   int
	format string
}

func addLogFlags(cmd *cobra.Command, o *logOptions) {
	flags := cmd.Flags()
	flags.BoolVarP(&o.follow, "follow", "f", false, "keep showing new lines until the site stops or Ctrl+C")
	flags.BoolVar(&o.events, "events", false, "show only events, not OCPP messages")
	flags.StringVar(&o.since, "since", "", "show lines from this long ago (10m, 2h) or from a time (2026-01-02T15:04:05Z)")
	flags.IntVar(&o.tail, "tail", -1, "show only the last N lines (default: all)")
	addFormatFlag(cmd, &o.format)
}

const logHelp = "Each line is a time, the charge point ID, a marker and the text. The marker\n" +
	"is | for an event, > for a message sent to the CSMS and < for one received\n" +
	"from it. With --format json each line is an object, and an OCPP message is\n" +
	"also given as JSON in its \"message\" field.\n\n" +
	"The log is kept in files beside the site's saved state, so it can be read\n" +
	"while the chargers are stopped. It holds the newest 30 MB unless the site\n" +
	"file sets another cap under \"log\"."

func newLogs() *cobra.Command {
	var (
		opts     logOptions
		siteName string
	)
	cmd := &cobra.Command{
		Use:   "logs <charger>",
		Short: "Show the events and OCPP messages of chargers",
		Long: "Show the log of one charger, named by its charge point ID or its name, or\n" +
			"of every charger matching a pattern such as 'FAST-*'.\n\n" + logHelp,
		Example: "  plugyard logs CP-1\n" +
			"  plugyard logs CP-1 -f --events\n" +
			"  plugyard logs 'FAST-*' --since 10m\n" +
			"  plugyard logs CP-1 --format json --tail 20",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			since, err := opts.check()
			if err != nil {
				return err
			}
			if err := checkPattern(args[0]); err != nil {
				return err
			}
			w, err := loadWorld(cmd.Context(), siteName)
			if err != nil {
				return err
			}
			targets, err := w.match(args[0], nil)
			w.close()
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				return fmt.Errorf("no charger matches %q", args[0])
			}
			sites := map[string]map[string]bool{}
			for _, t := range targets {
				if sites[t.state.Site] == nil {
					sites[t.state.Site] = map[string]bool{}
				}
				sites[t.state.Site][t.state.ChargePointID] = true
			}
			return showLogs(cmd.Context(), cmd.OutOrStdout(), w.dir, sites, opts, since)
		},
	}
	addLogFlags(cmd, &opts)
	cmd.Flags().StringVarP(&siteName, "site", "s", "", "only look at chargers of this site")
	return cmd
}

func newSiteLogs() *cobra.Command {
	var (
		opts     logOptions
		file     string
		siteName string
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show the events and OCPP messages of a whole site",
		Long: "Show the log of every charger of a site: the one named with -s, or the one\n" +
			"the site file declares. Here -f follows the log, as it does for\n" +
			"\"docker compose logs\"; name another site file with --file.\n\n" + logHelp,
		Example: "  plugyard site logs -f\n" +
			"  plugyard site logs -s depot-north --events --since 1h",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			since, err := opts.check()
			if err != nil {
				return err
			}
			name, err := siteNameFromFlags(file, siteName)
			if err != nil {
				return err
			}
			w, err := loadWorld(cmd.Context(), name)
			if err != nil {
				return err
			}
			w.close()
			return showLogs(cmd.Context(), cmd.OutOrStdout(), w.dir, map[string]map[string]bool{name: nil}, opts, since)
		},
	}
	addLogFlags(cmd, &opts)
	cmd.Flags().StringVar(&file, "file", "site.yaml", "site file")
	cmd.Flags().StringVarP(&siteName, "site", "s", "", "name of the site")
	return cmd
}

// check validates the flags and works out the moment --since names. The
// zero time stands for no limit.
func (o logOptions) check() (time.Time, error) {
	if err := checkFormat(o.format); err != nil {
		return time.Time{}, err
	}
	if o.tail < -1 {
		return time.Time{}, &UsageError{Err: errors.New("--tail must be 0 or more")}
	}
	if o.since == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(o.since); err == nil {
		if d < 0 {
			return time.Time{}, &UsageError{Err: errors.New("--since cannot be negative")}
		}
		return time.Now().Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, o.since, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &UsageError{Err: fmt.Errorf("invalid --since %q: want how long ago, such as 10m or 2h, or a time such as 2026-01-02T15:04:05Z", o.since)}
}

// parseLogLine reads a line as the log printer writes it.
func parseLogLine(line string) (charger.Entry, bool) {
	stamp, rest, ok := strings.Cut(line, " ")
	if !ok {
		return charger.Entry{}, false
	}
	when, err := time.Parse(logTimeLayout, stamp)
	if err != nil {
		return charger.Entry{}, false
	}
	// The marker is the first of " | ", " > " and " < " in the line.
	at, kind := -1, charger.Event
	for marker, k := range map[string]charger.EntryKind{" | ": charger.Event, " > ": charger.MessageSent, " < ": charger.MessageReceived} {
		if i := strings.Index(rest, marker); i >= 0 && (at < 0 || i < at) {
			at, kind = i, k
		}
	}
	if at < 0 {
		return charger.Entry{}, false
	}
	return charger.Entry{Time: when, ChargePointID: rest[:at], Kind: kind, Text: rest[at+3:]}, true
}

// logSource reads the log of one site.
type logSource struct {
	site string
	path string
	// ids are the chargers wanted; nil means all of the site.
	ids    map[string]bool
	events bool
	since  time.Time
	// offset is how far the current file has been read.
	offset int64
}

func (s *logSource) wanted(e charger.Entry) bool {
	switch {
	case s.ids != nil && !s.ids[e.ChargePointID]:
		return false
	case s.events && e.Kind != charger.Event:
		return false
	case !s.since.IsZero() && e.Time.Before(s.since):
		return false
	}
	return true
}

// read returns the wanted lines of a file from offset on, and the offset
// after the last whole line. The file is not kept open, so that the site can
// rotate it meanwhile.
func (s *logSource) read(path string, offset int64) ([]charger.Entry, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, offset
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, offset
	}

	var out []charger.Entry
	for _, line := range strings.Split(string(data[:end]), "\n") {
		if e, ok := parseLogLine(strings.TrimSuffix(line, "\r")); ok && s.wanted(e) {
			out = append(out, e)
		}
	}
	return out, offset + int64(end) + 1
}

// all returns the wanted lines of every file of the log, oldest first.
func (s *logSource) all() []charger.Entry {
	var out []charger.Entry
	for _, path := range logfile.Paths(s.path) {
		lines, end := s.read(path, 0)
		out = append(out, lines...)
		if path == s.path {
			s.offset = end
		}
	}
	return out
}

// more returns the wanted lines written since the last read.
func (s *logSource) more() []charger.Entry {
	var out []charger.Entry
	info, err := os.Stat(s.path)
	if err != nil {
		return nil
	}
	if info.Size() < s.offset {
		// The file was rotated: the rest of what was being read is in the
		// file it was renamed to.
		lines, _ := s.read(logfile.Rotated(s.path), s.offset)
		out = append(out, lines...)
		s.offset = 0
	}
	lines, end := s.read(s.path, s.offset)
	s.offset = end
	return append(out, lines...)
}

// logJSON is a log line as --format json prints it.
type logJSON struct {
	Time          string `json:"time"`
	ChargePointID string `json:"chargePointId"`
	// Kind is "event", "sent" or "received".
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Message is the OCPP message of a line that is one.
	Message json.RawMessage `json:"message,omitempty"`
}

func newLogWriter(out io.Writer, format string) func(charger.Entry) {
	if format != formatJSON {
		return newLogPrinter(out)
	}
	enc := json.NewEncoder(out)
	return func(e charger.Entry) {
		line := logJSON{Time: e.Time.Format(logTimeLayout), ChargePointID: e.ChargePointID, Kind: "event", Text: e.Text}
		if e.Kind != charger.Event {
			line.Kind = "sent"
			if e.Kind == charger.MessageReceived {
				line.Kind = "received"
			}
			if json.Valid([]byte(e.Text)) {
				line.Message = json.RawMessage(e.Text)
			}
		}
		_ = enc.Encode(line)
	}
}

// showLogs prints the log of the given chargers of the given sites, and
// with --follow keeps printing until no site of them runs any more.
func showLogs(ctx context.Context, out io.Writer, dir string, sites map[string]map[string]bool, opts logOptions, since time.Time) error {
	var sources []*logSource
	exists := false
	for name, ids := range sites {
		s := &logSource{site: name, path: store.LogPath(dir, name), ids: ids, events: opts.events, since: since}
		sources = append(sources, s)
		exists = exists || len(logfile.Paths(s.path)) > 0
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].site < sources[j].site })
	if !exists {
		return errors.New("there is no log: the chargers have not run yet, or were started with --rm, which keeps none")
	}

	var lines []charger.Entry
	for _, s := range sources {
		lines = append(lines, s.all()...)
	}
	if len(sources) > 1 {
		sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time.Before(lines[j].Time) })
	}
	if opts.tail >= 0 && len(lines) > opts.tail {
		lines = lines[len(lines)-opts.tail:]
	}
	write := newLogWriter(out, opts.format)
	for _, e := range lines {
		write(e)
	}
	if !opts.follow {
		return nil
	}

	running := func() bool {
		for _, s := range sources {
			if client, err := control.Probe(ctx, dir, s.site); err == nil {
				client.Close()
				return true
			}
		}
		return false
	}
	ticker := time.NewTicker(logPollInterval)
	defer ticker.Stop()
	for n := 0; ; n++ {
		// Like "docker logs -f", following ends when nothing is left that
		// could write more.
		done := n%5 == 0 && !running()
		for _, s := range sources {
			for _, e := range s.more() {
				write(e)
			}
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// diagnosticsLog returns the lines of a site's log that belong to one
// charger and fall between two moments, as they stand in the log. A zero
// time leaves that end open. It is the file a charger uploads for
// GetDiagnostics.
func diagnosticsLog(path, chargePointID string, from, to time.Time) ([]byte, error) {
	var out bytes.Buffer
	for _, file := range logfile.Paths(path) {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSuffix(line, "\r")
			e, ok := parseLogLine(line)
			switch {
			case !ok, e.ChargePointID != chargePointID:
			case !from.IsZero() && e.Time.Before(from):
			case !to.IsZero() && e.Time.After(to):
			default:
				out.WriteString(line + "\n")
			}
		}
	}
	return out.Bytes(), nil
}
