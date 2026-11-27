// Package store keeps the saved state of sites: one SQLite database per
// site in the data directory (see docs/adr/0002 and 0003). The process that
// runs a site writes to its database; other commands read it, or change it
// while the site is not running.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	_ "modernc.org/sqlite" // the pure-Go SQLite driver

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/datadir"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/logfile"
	"github.com/Amraa1/plugyard/internal/model"
)

// schemaVersion is stored in each database. A database written by a newer
// plugyard is refused instead of misread.
const schemaVersion = 1

const dbSuffix = ".db"

// ErrNoSite is returned when a site has no saved state.
var ErrNoSite = errors.New("the site has no saved state")

// Meta is what is saved about a site as a whole.
type Meta struct {
	Name string `json:"name"`
	// File is the site file the site was last brought up from. It is empty
	// for the implicit site of "plugyard run".
	File string `json:"file,omitempty"`
	// Implicit marks the site of a charger started with "plugyard run".
	Implicit bool `json:"implicit,omitempty"`
	// Down says the site was taken down: its chargers are no longer listed,
	// though their state is kept.
	Down        bool                    `json:"down,omitempty"`
	Speed       float64                 `json:"speed,omitempty"`
	ConnectRate float64                 `json:"connectRate,omitempty"`
	EVs         map[string]ev.Overrides `json:"evs,omitempty"`
	// LogMaxSize and LogFiles cap the site's log: at most LogFiles files of
	// LogMaxSize bytes. Zero stands for the defaults.
	LogMaxSize int64 `json:"logMaxSize,omitempty"`
	LogFiles   int   `json:"logFiles,omitempty"`
	// AllowUploads lists the hosts diagnostics may be uploaded to besides
	// the machine the CSMS is on.
	AllowUploads []string `json:"allowUploads,omitempty"`
	// UploadsInsecureSkipVerify turns off the check of the certificate of
	// an https or ftps server diagnostics are uploaded to.
	UploadsInsecureSkipVerify bool `json:"uploadsInsecureSkipVerify,omitempty"`
}

// Spec is the snapshot of the files a charger was created from.
type Spec struct {
	Model              *model.Model `json:"model"`
	CSMSURL            string       `json:"csmsUrl"`
	InsecureSkipVerify bool         `json:"insecureSkipVerify,omitempty"`
	// HasPassword says the charger authenticates with a password. The
	// password itself is never saved.
	HasPassword bool `json:"hasPassword,omitempty"`
	// EV names the EV preset used when a plug does not name one.
	EV string `json:"ev,omitempty"`
	// Declared is the declared state of the charger, or nil when its site
	// file declares none.
	Declared *charger.Declared `json:"declared,omitempty"`
}

// Charger is a saved charger.
type Charger struct {
	// ID is the charge point ID.
	ID   string
	Name string
	// Power says whether the charger is to be powered on when the site's
	// process starts.
	Power bool
	Spec  Spec
	// State is nil for a charger that has never run.
	State *charger.State
}

// Site is the saved state of a whole site.
type Site struct {
	Meta     Meta
	Chargers []Charger
}

// Store is the open database of one site.
type Store struct {
	db   *sql.DB
	path string

	// Saves are grouped: every save waiting when the writer is free goes
	// into one database transaction.
	writerOnce sync.Once
	saves      chan save
	mu         sync.Mutex
	closed     bool
	writerDone chan struct{}
}

type save struct {
	id    string
	state []byte
	done  chan error
}

// Path returns the database file of the site with the given name.
func Path(dataDir, siteName string) string {
	return datadir.SiteBase(dataDir, siteName) + dbSuffix
}

// LogPath returns the file a site logs to. When it is full it is rotated to
// files of the same name with a number added.
func LogPath(dataDir, siteName string) string {
	return datadir.SiteBase(dataDir, siteName) + ".log"
}

// OutputPath returns the file that takes what the background process of a
// site prints, which is only what goes wrong with the process itself.
func OutputPath(dataDir, siteName string) string {
	return datadir.SiteBase(dataDir, siteName) + ".out"
}

// Exists reports whether the site has saved state.
func Exists(dataDir, siteName string) bool {
	_, err := os.Stat(Path(dataDir, siteName))
	return err == nil
}

// Open opens the saved state of a site, creating it when the site has none.
func Open(dataDir, siteName string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := Path(dataDir, siteName)
	_, statErr := os.Stat(path)
	st, err := open(path, path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	if statErr != nil {
		if err := st.SetMeta(Meta{Name: siteName}); err != nil {
			st.Close()
			return nil, err
		}
	}
	return st, nil
}

// OpenExisting opens the saved state of a site, or returns ErrNoSite.
func OpenExisting(dataDir, siteName string) (*Store, error) {
	if !Exists(dataDir, siteName) {
		return nil, fmt.Errorf("site %q: %w", siteName, ErrNoSite)
	}
	return Open(dataDir, siteName)
}

// OpenMemory returns a store that keeps nothing on disk, for a charger that
// is to leave no trace.
func OpenMemory(siteName string) (*Store, error) {
	st, err := open("", ":memory:")
	if err != nil {
		return nil, err
	}
	if err := st.SetMeta(Meta{Name: siteName}); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

func open(path, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: an in-memory database lives in its connection, and a
	// site's writes are serialized anyway.
	db.SetMaxOpenConns(1)

	st := &Store{db: db, path: path}
	if err := st.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("cannot open saved state %s: %w", path, err)
	}
	return st, nil
}

func (st *Store) migrate() error {
	var version int
	if err := st.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	switch {
	case version > schemaVersion:
		return errors.New("it was written by a newer version of plugyard")
	case version == schemaVersion:
		return nil
	}
	_, err := st.db.Exec(`
		CREATE TABLE IF NOT EXISTS meta (
			id   INTEGER PRIMARY KEY CHECK (id = 1),
			data TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS chargers (
			id    TEXT PRIMARY KEY,
			name  TEXT NOT NULL UNIQUE,
			power INTEGER NOT NULL,
			spec  TEXT NOT NULL,
			state TEXT
		);
		PRAGMA user_version = 1;`)
	return err
}

// Close flushes pending saves and closes the database.
func (st *Store) Close() error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true
	if st.saves != nil {
		close(st.saves)
	}
	done := st.writerDone
	st.mu.Unlock()
	if done != nil {
		<-done
	}
	return st.db.Close()
}

// Meta returns what is saved about the site as a whole.
func (st *Store) Meta() (Meta, error) {
	var data string
	var meta Meta
	if err := st.db.QueryRow("SELECT data FROM meta WHERE id = 1").Scan(&data); err != nil {
		return meta, err
	}
	err := json.Unmarshal([]byte(data), &meta)
	return meta, err
}

// SetMeta replaces what is saved about the site as a whole.
func (st *Store) SetMeta(meta Meta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = st.db.Exec("INSERT INTO meta (id, data) VALUES (1, ?) ON CONFLICT (id) DO UPDATE SET data = excluded.data", string(data))
	return err
}

// Chargers returns the site's chargers, sorted by charge point ID.
func (st *Store) Chargers() ([]Charger, error) {
	rows, err := st.db.Query("SELECT id, name, power, spec, state FROM chargers ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Charger
	for rows.Next() {
		var c Charger
		var spec string
		var state sql.NullString
		if err := rows.Scan(&c.ID, &c.Name, &c.Power, &spec, &state); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(spec), &c.Spec); err != nil {
			return nil, fmt.Errorf("charger %s: %w", c.ID, err)
		}
		if state.Valid {
			c.State = &charger.State{}
			if err := json.Unmarshal([]byte(state.String), c.State); err != nil {
				return nil, fmt.Errorf("charger %s: %w", c.ID, err)
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Change is a set of changes to a site's chargers that is applied as a
// whole or not at all.
type Change struct {
	// Put creates chargers or replaces their name, power and spec. The
	// state of an existing charger is kept.
	Put []Charger
	// Reset wipes the state of chargers, which keep their name.
	Reset []string
	// Remove deletes chargers.
	Remove []string
}

// Apply makes a set of changes.
func (st *Store) Apply(change Change) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, id := range change.Remove {
		if _, err := tx.Exec("DELETE FROM chargers WHERE id = ?", id); err != nil {
			return err
		}
	}
	for _, id := range change.Reset {
		if _, err := tx.Exec("UPDATE chargers SET state = NULL WHERE id = ?", id); err != nil {
			return err
		}
	}
	for _, c := range change.Put {
		spec, err := json.Marshal(c.Spec)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO chargers (id, name, power, spec) VALUES (?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, power = excluded.power, spec = excluded.spec`,
			c.ID, c.Name, c.Power, string(spec))
		if err != nil {
			return fmt.Errorf("charger %s: %w", c.ID, err)
		}
	}
	return tx.Commit()
}

// SetPower marks which chargers are to be powered on when the site's process
// starts: the given ones, or all of them when ids is nil.
func (st *Store) SetPower(ids []string) error {
	if ids == nil {
		_, err := st.db.Exec("UPDATE chargers SET power = 1")
		return err
	}
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE chargers SET power = 0"); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec("UPDATE chargers SET power = 1 WHERE id = ?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Remove deletes a charger and its state.
func (st *Store) Remove(id string) error {
	return st.Apply(Change{Remove: []string{id}})
}

// LoadState returns the saved state of a charger, or nil if it has never
// run.
func (st *Store) LoadState(id string) (*charger.State, error) {
	var state sql.NullString
	if err := st.db.QueryRow("SELECT state FROM chargers WHERE id = ?", id).Scan(&state); err != nil {
		return nil, err
	}
	if !state.Valid {
		return nil, nil
	}
	out := &charger.State{}
	if err := json.Unmarshal([]byte(state.String), out); err != nil {
		return nil, err
	}
	return out, nil
}

// SaveState saves the state of a charger and returns once it is on disk.
func (st *Store) SaveState(id string, state charger.State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return errors.New("the saved state is closed")
	}
	if st.saves == nil {
		st.saves = make(chan save, 256)
		st.writerDone = make(chan struct{})
		go st.writer()
	}
	s := save{id: id, state: data, done: make(chan error, 1)}
	saves := st.saves
	// Sending under the lock keeps Close from closing the channel midway.
	saves <- s
	st.mu.Unlock()
	return <-s.done
}

// writer writes saves in groups, so that thousands of chargers saving their
// meter readings in the same second cost one database transaction.
func (st *Store) writer() {
	defer close(st.writerDone)
	for first := range st.saves {
		group := []save{first}
	collect:
		for len(group) < 1000 {
			select {
			case s, ok := <-st.saves:
				if !ok {
					break collect
				}
				group = append(group, s)
			default:
				break collect
			}
		}

		err := st.writeGroup(group)
		for _, s := range group {
			s.done <- err
		}
	}
}

func (st *Store) writeGroup(group []save) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range group {
		if _, err := tx.Exec("UPDATE chargers SET state = ? WHERE id = ?", string(s.state), s.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Load reads the whole saved state of the site.
func (st *Store) Load() (Site, error) {
	meta, err := st.Meta()
	if err != nil {
		return Site{}, err
	}
	chargers, err := st.Chargers()
	return Site{Meta: meta, Chargers: chargers}, err
}

// All reads the saved state of every site in the data directory, sorted by
// site name.
func All(dataDir string) ([]Site, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, "s-*"+dbSuffix))
	if err != nil {
		return nil, err
	}
	var sites []Site
	for _, path := range paths {
		st, err := open(path, path+"?_pragma=busy_timeout(10000)")
		if err != nil {
			return nil, err
		}
		site, err := st.Load()
		st.Close()
		if err != nil {
			return nil, fmt.Errorf("cannot read saved state %s: %w", path, err)
		}
		sites = append(sites, site)
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Meta.Name < sites[j].Meta.Name })
	return sites, nil
}

// Delete wipes the saved state of a site, and its log.
func Delete(dataDir, siteName string) error {
	path := Path(dataDir, siteName)
	for _, file := range []string{path + "-wal", path + "-shm", OutputPath(dataDir, siteName), path} {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return logfile.Remove(LogPath(dataDir, siteName))
}
