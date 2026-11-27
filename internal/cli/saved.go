package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/datadir"
	"github.com/Amraa1/plugyard/internal/logfile"
	"github.com/Amraa1/plugyard/internal/names"
	"github.com/Amraa1/plugyard/internal/site"
	"github.com/Amraa1/plugyard/internal/sitefile"
	"github.com/Amraa1/plugyard/internal/store"
)

// detachTimeout is how long a command waits for a background copy of
// plugyard to report that its site is running.
const detachTimeout = 30 * time.Second

// target is a charger a command can name: one of a running site, powered on
// or not, or one that exists only as saved state.
type target struct {
	// client is nil when the charger's site has no process.
	client *control.Client
	state  control.ChargerState
}

// world is every charger plugyard knows of on this machine.
type world struct {
	dir     string
	clients []*control.Client
	targets []target
	// saved holds the saved state of every site, running or not.
	saved []store.Site
}

// loadWorld finds the running sites and the saved ones, narrowed to one
// site when siteName is given. The caller closes it.
func loadWorld(ctx context.Context, siteName string) (*world, error) {
	dir, err := datadir.Path()
	if err != nil {
		return nil, err
	}
	w := &world{dir: dir}
	if w.clients, err = control.Discover(ctx, dir); err != nil {
		return nil, err
	}
	if w.saved, err = store.All(dir); err != nil {
		w.close()
		return nil, err
	}

	found := false
	running := map[string]bool{}
	for _, client := range w.clients {
		running[client.Info.Site] = true
		if siteName != "" && client.Info.Site != siteName {
			continue
		}
		found = true
		chargers, err := client.Chargers(ctx)
		if err != nil {
			w.close()
			return nil, fmt.Errorf("site %q did not answer: %w", client.Info.Site, err)
		}
		for _, c := range chargers {
			w.targets = append(w.targets, target{client: client, state: c})
		}
	}
	for _, s := range w.saved {
		if running[s.Meta.Name] || (siteName != "" && s.Meta.Name != siteName) {
			continue
		}
		found = true
		// The chargers of a site that was taken down are not listed.
		if s.Meta.Down {
			continue
		}
		for _, c := range s.Chargers {
			w.targets = append(w.targets, target{state: control.ChargerState{
				Site:     s.Meta.Name,
				Name:     c.Name,
				Snapshot: c.State.Snapshot(c.ID, c.Spec.Model, c.Spec.State),
			}})
		}
	}
	if siteName != "" && !found {
		w.close()
		return nil, &matchError{kind: matchNone, text: fmt.Sprintf("no site is named %q", siteName)}
	}

	sort.SliceStable(w.targets, func(i, j int) bool {
		a, b := w.targets[i].state, w.targets[j].state
		if a.ChargePointID != b.ChargePointID {
			return a.ChargePointID < b.ChargePointID
		}
		return a.Site < b.Site
	})
	return w, nil
}

func (w *world) close() {
	for _, c := range w.clients {
		c.Close()
	}
}

// matchError says why a charger argument names no charger to act on.
type matchError struct {
	kind int
	text string
}

const (
	matchNone      = iota // no charger matches
	matchStopped          // the charger is stopped
	matchAmbiguous        // the same charge point ID is in two sites
)

func (e *matchError) Error() string { return e.text }

// isPattern reports whether a charger argument is a glob pattern and not a
// single charge point ID or name.
func isPattern(arg string) bool { return strings.ContainsAny(arg, `*?[\`) }

// checkPattern rejects a charger argument that is not a valid pattern.
func checkPattern(arg string) error {
	if _, err := path.Match(arg, ""); err != nil {
		return &UsageError{Err: fmt.Errorf("invalid charger pattern %q", arg)}
	}
	return nil
}

// match returns the chargers a charger argument names: those whose charge
// point ID or name matches it and that keep reports true for.
func (w *world) match(arg string, keep func(target) bool) ([]target, error) {
	var out []target
	sites := map[string]bool{}
	for _, t := range w.targets {
		byID, _ := path.Match(arg, t.state.ChargePointID)
		byName, _ := path.Match(arg, t.state.Name)
		if (byID || byName) && (keep == nil || keep(t)) {
			out = append(out, t)
			sites[t.state.Site] = true
		}
	}
	if !isPattern(arg) && len(out) > 1 {
		list := make([]string, 0, len(sites))
		for s := range sites {
			list = append(list, s)
		}
		sort.Strings(list)
		return nil, &matchError{kind: matchAmbiguous, text: fmt.Sprintf("charger %q exists in sites %s; choose one with -s, or use its name", arg, strings.Join(list, " and "))}
	}
	return out, nil
}

// siteNameFromFlags works out which site a site command is about: the one
// named with -s, or the one the site file declares.
func siteNameFromFlags(file, siteName string) (string, error) {
	if siteName != "" {
		return siteName, nil
	}
	name, err := sitefile.Name(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && file == sitefile.DefaultName {
			return "", &UsageError{Err: fmt.Errorf("there is no %s in this directory; name a site with -s or a site file with -f", sitefile.DefaultName)}
		}
		return "", &UsageError{Err: err}
	}
	return name, nil
}

// desired is a charger as the files declare it.
type desired struct {
	ID string
	// Name is the name the user gave, or empty.
	Name string
	Spec store.Spec
}

// plan is what bringing saved state in line with the files takes.
type plan struct {
	change store.Change
	// confirm lists, per charge point ID, why saved state would be lost.
	confirm [][2]string
}

// reconcile compares the chargers the files declare with the ones saved for
// the site. others is the saved state of every site, used to keep names
// unique on the machine.
func reconcile(siteName string, want []desired, have []store.Charger, others []store.Site) (plan, error) {
	var p plan
	saved := map[string]store.Charger{}
	for _, c := range have {
		saved[c.ID] = c
	}
	wanted := map[string]bool{}
	for _, d := range want {
		wanted[d.ID] = true
	}

	// taken maps each name in use to where it is used.
	taken := map[string]string{}
	for _, s := range others {
		if s.Meta.Name == siteName {
			continue
		}
		for _, c := range s.Chargers {
			taken[c.Name] = fmt.Sprintf("charger %s of site %s", c.ID, s.Meta.Name)
		}
	}
	for _, c := range have {
		if !wanted[c.ID] {
			if c.State != nil {
				p.confirm = append(p.confirm, [2]string{c.ID, "is no longer declared; its saved state will be deleted"})
			}
			p.change.Remove = append(p.change.Remove, c.ID)
			continue
		}
		taken[c.Name] = "charger " + c.ID
	}

	// Names the user gave come first, so that a made-up name never takes
	// one of them.
	for _, d := range want {
		if d.Name == "" {
			continue
		}
		old, exists := saved[d.ID]
		if exists && old.Name == d.Name {
			continue
		}
		if where, used := taken[d.Name]; used {
			return p, fmt.Errorf("the name %q is already used by %s", d.Name, where)
		}
		if exists {
			delete(taken, old.Name)
		}
		taken[d.Name] = "charger " + d.ID
	}

	for _, d := range want {
		old, exists := saved[d.ID]
		name := d.Name
		switch {
		case name != "":
		case exists:
			name = old.Name
		default:
			name = names.Generate(func(n string) bool { _, used := taken[n]; return used })
			taken[name] = "charger " + d.ID
		}

		if exists && old.State != nil {
			before, after := len(old.Spec.Model.Connectors), len(d.Spec.Model.Connectors)
			if before != after {
				p.confirm = append(p.confirm, [2]string{d.ID, fmt.Sprintf("went from %s to %s; its saved state will be reset", countConnectors(before), countConnectors(after))})
				p.change.Reset = append(p.change.Reset, d.ID)
			}
		}
		p.change.Put = append(p.change.Put, store.Charger{ID: d.ID, Name: name, Power: !d.Spec.State.Stopped(), Spec: d.Spec})
	}
	return p, nil
}

func countConnectors(n int) string {
	if n == 1 {
		return "1 connector"
	}
	return fmt.Sprintf("%d connectors", n)
}

// stdin and stdinIsTerminal are variables so that tests can answer prompts.
var (
	stdin           io.Reader = os.Stdin
	stdinIsTerminal           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
)

// confirmReset asks before saved state is lost. With yes it does not ask;
// with nobody to ask it refuses.
func confirmReset(out io.Writer, yes bool, confirm [][2]string) error {
	if len(confirm) == 0 {
		return nil
	}
	fmt.Fprintln(out, "The files no longer match the saved state of these chargers:")
	rows := make([][]cell, 0, len(confirm))
	for _, c := range confirm {
		rows = append(rows, []cell{plain("  " + c[0]), plain(c[1])})
	}
	writeTable(out, rows)
	if yes {
		return nil
	}

	if !stdinIsTerminal() {
		return fmt.Errorf("%s would lose saved state; run again with --yes to allow it", countChargers(len(confirm)))
	}
	what := "these " + countChargers(len(confirm))
	if len(confirm) == 1 {
		what = "this charger"
	}
	fmt.Fprintf(out, "Reset %s? [y/N] ", what)
	answer, _ := bufio.NewReader(stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return errors.New("nothing was changed and nothing was started")
}

// prepare brings the saved state of a site in line with what the files
// declare, asking before any of it is lost, and returns the open store with
// every declared charger marked to be powered on.
func prepare(ctx context.Context, cmd *cobra.Command, dir string, meta store.Meta, want []desired, yes, memory bool) (*store.Store, error) {
	if client, err := control.Probe(ctx, dir, meta.Name); err == nil {
		client.Close()
		return nil, fmt.Errorf("site %q: %w", meta.Name, control.ErrAlreadyRunning)
	}
	others, err := store.All(dir)
	if err != nil {
		return nil, err
	}

	var st *store.Store
	if memory {
		if store.Exists(dir, meta.Name) {
			return nil, fmt.Errorf("charger %s has saved state, which --rm would not use; remove it with \"plugyard rm %s\" or leave --rm out", meta.Name, meta.Name)
		}
		st, err = store.OpenMemory(meta.Name)
	} else {
		st, err = store.Open(dir, meta.Name)
	}
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*store.Store, error) {
		st.Close()
		return nil, err
	}

	old, err := st.Meta()
	if err != nil {
		return fail(err)
	}
	have, err := st.Chargers()
	if err != nil {
		return fail(err)
	}
	if len(have) > 0 && old.Implicit != meta.Implicit {
		if old.Implicit {
			return fail(fmt.Errorf("the name %q is taken by a charger started with \"plugyard run\"; remove it with \"plugyard rm %s\" or give the site another name", meta.Name, meta.Name))
		}
		return fail(fmt.Errorf("a site named %q already exists; use another charge point ID, or wipe the site with \"plugyard site down --reset -s %s\"", meta.Name, meta.Name))
	}

	p, err := reconcile(meta.Name, want, have, others)
	if err != nil {
		return fail(&UsageError{Err: err})
	}
	if err := confirmReset(cmd.OutOrStdout(), yes, p.confirm); err != nil {
		return fail(err)
	}
	if err := st.Apply(p.change); err != nil {
		return fail(err)
	}
	if err := st.SetMeta(meta); err != nil {
		return fail(err)
	}
	return st, nil
}

// powerOnAll marks the chargers of a site that are to be powered on when its
// process starts: every one but those declared stopped. It returns how many
// that is, and how many chargers the site has.
func powerOnAll(st *store.Store) (powered, total int, err error) {
	chargers, err := st.Chargers()
	if err != nil {
		return 0, 0, err
	}
	// Not nil, which would stand for every charger.
	ids := []string{}
	for _, c := range chargers {
		if !c.Spec.State.Stopped() {
			ids = append(ids, c.ID)
		}
	}
	return len(ids), len(chargers), st.SetPower(ids)
}

// nonePoweredOn says that a site came up with nothing running.
func nonePoweredOn(siteName string, chargers int) string {
	return fmt.Sprintf("Site %s has %s, all declared stopped: none was powered on\n", siteName, countChargers(chargers))
}

// serveSaved runs a site from its saved state in this process until ctx is
// cancelled, a command asks it to shut down, or its last powered charger is
// stopped. It answers other commands on the site's socket meanwhile, and
// closes the store when done. The log of every charger is printed to out,
// unless that is nil, and with keepLog written to the site's log files too.
// password gives the CSMS password of a charger.
func serveSaved(ctx context.Context, dir string, st *store.Store, out io.Writer, keepLog bool, password func(chargePointID string) string) error {
	defer st.Close()

	saved, err := st.Load()
	if err != nil {
		return err
	}
	var members []site.Member
	powered := map[string]bool{}
	for _, c := range saved.Chargers {
		m := site.Member{
			ID:    c.ID,
			Name:  c.Name,
			Model: c.Spec.Model,
			CSMS:  charger.CSMS{URL: c.Spec.CSMSURL, InsecureSkipVerify: c.Spec.InsecureSkipVerify},
			EV:    c.Spec.EV,
			State: c.Spec.State,
		}
		if c.Spec.HasPassword && password != nil {
			m.CSMS.Password = password(c.ID)
		}
		members = append(members, m)
		if c.Power {
			powered[c.ID] = true
		}
	}
	if len(powered) == 0 {
		return errors.New("the site has no charger to start")
	}
	if err := checkFileLimit(len(powered)); err != nil {
		return err
	}

	var sinks []func(charger.Entry)
	if out != nil {
		sinks = append(sinks, newLogPrinter(out))
	}
	// The log is also what a charger uploads when the CSMS asks for
	// diagnostics.
	var diagnostics func(id string, from, to time.Time) ([]byte, error)
	var logWriter *logfile.Writer
	if keepLog {
		logPath := store.LogPath(dir, saved.Meta.Name)
		diagnostics = func(id string, from, to time.Time) ([]byte, error) {
			return diagnosticsLog(logPath, id, from, to)
		}
		logWriter, err = logfile.Open(store.LogPath(dir, saved.Meta.Name), saved.Meta.LogMaxSize, saved.Meta.LogFiles)
		if err != nil {
			return fmt.Errorf("cannot open the site's log: %w", err)
		}
		defer logWriter.Close()
		sinks = append(sinks, newLogPrinter(logWriter))
	}

	s, err := site.New(site.Config{
		Name:        saved.Meta.Name,
		Speed:       saved.Meta.Speed,
		ConnectRate: saved.Meta.ConnectRate,
		Presets:     saved.Meta.EVs,
		Log: func(e charger.Entry) {
			for _, sink := range sinks {
				sink(e)
			}
		},
		Diagnostics:  diagnostics,
		AllowUploads: saved.Meta.AllowUploads,
		Store:        st,

		UploadsInsecureSkipVerify: saved.Meta.UploadsInsecureSkipVerify,
	}, members)
	if err != nil {
		return err
	}

	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()
	server, err := control.Serve(dir, s, shutdown)
	if err != nil {
		return err
	}
	runErr := s.Run(ctx, func(id string) bool { return powered[id] })
	server.Close()

	// A site whose last charger was removed leaves nothing behind.
	if left, err := st.Chargers(); err == nil && len(left) == 0 {
		st.Close()
		if logWriter != nil {
			logWriter.Close()
		}
		_ = store.Delete(dir, saved.Meta.Name)
	}
	return runErr
}

// savedPasswords works out the CSMS passwords of a saved site's chargers.
// Passwords are never saved, so they are read again from where they came
// from: the environment for a charger started with "plugyard run", and the
// site file otherwise.
func savedPasswords(saved store.Site) (func(string) string, error) {
	needed := ""
	for _, c := range saved.Chargers {
		if c.Spec.HasPassword && c.Power {
			needed = c.ID
			break
		}
	}
	if needed == "" {
		return nil, nil
	}

	if saved.Meta.Implicit {
		password := os.Getenv(passwordEnv)
		if password == "" {
			return nil, fmt.Errorf("charger %s connects with a password, which is not saved; set %s and try again", needed, passwordEnv)
		}
		return func(string) string { return password }, nil
	}

	sf, err := sitefile.Load(saved.Meta.File)
	if err != nil {
		return nil, fmt.Errorf("charger %s connects with a password, which is not saved and could not be read from the site file: %w", needed, err)
	}
	passwords := filePasswords(sf)
	return func(id string) string { return passwords[id] }, nil
}

func filePasswords(sf *sitefile.Site) map[string]string {
	passwords := map[string]string{}
	for _, c := range sf.Chargers() {
		passwords[c.ID] = c.CSMS.Password
	}
	return passwords
}

// startBackground starts a background copy of plugyard that runs a site
// from its saved state, and waits until it answers.
func startBackground(ctx context.Context, dir, siteName string) (control.Info, error) {
	var none control.Info
	if client, err := control.Probe(ctx, dir, siteName); err == nil {
		client.Close()
		return none, fmt.Errorf("site %q: %w", siteName, control.ErrAlreadyRunning)
	}

	// The background copy has no terminal. The log of its chargers goes to
	// the site's log files; what the process itself prints, which is only
	// what goes wrong with it, goes to a file of its own that starts empty
	// each time.
	logPath := store.OutputPath(dir, siteName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return none, err
	}
	defer logFile.Close()
	before := int64(0)

	exe, err := os.Executable()
	if err != nil {
		return none, err
	}
	child := exec.Command(exe, "site", "serve", "--site", siteName)
	child.Stdout = logFile
	child.Stderr = logFile
	child.Dir = filepath.Dir(logPath)
	setDetached(child)
	if err := child.Start(); err != nil {
		return none, fmt.Errorf("cannot start the background process: %w", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	deadline := time.After(detachTimeout)
	for {
		if client, err := control.Probe(ctx, dir, siteName); err == nil {
			info := client.Info
			client.Close()
			return info, nil
		}
		select {
		case <-exited:
			return none, fmt.Errorf("the site did not start:\n%s", strings.TrimSpace(tailFrom(logPath, before, 20)))
		case <-deadline:
			return none, fmt.Errorf("the site did not report as running within %s; see %s", detachTimeout, logPath)
		case <-ctx.Done():
			return none, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// reportBackground says where a site started in the background runs.
func reportBackground(out io.Writer, dir string, info control.Info) {
	fmt.Fprintf(out, "Site %s is running in the background with %s (process %d)\n", info.Site, countChargers(info.Powered), info.PID)
	fmt.Fprintf(out, "Follow its log with: plugyard site logs -s %s -f\n", info.Site)
}

// tailFrom returns the last lines of what a text file holds after offset.
func tailFrom(path string, offset int64, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) < offset {
		return ""
	}
	all := strings.Split(strings.TrimRight(string(data[offset:]), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

func countChargers(n int) string {
	if n == 1 {
		return "1 charger"
	}
	return fmt.Sprintf("%d chargers", n)
}
