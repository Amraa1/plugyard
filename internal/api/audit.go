package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Amraa1/plugyard/internal/logfile"
)

// Entry is the record of one call to the API: who did what to which
// charger, and how it ended. A token itself is never part of it.
type Entry struct {
	Time time.Time `json:"time"`
	// Token is the name of the token the call came with, "local" for a
	// call that needed none, and "-" for one that was turned away before
	// its token was known.
	Token string `json:"token"`
	// Remote is the address the call came from. Behind a reverse proxy
	// that is the proxy.
	Remote string `json:"remote"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Status is the HTTP status the call was answered with.
	Status int `json:"status"`
	// Action is the action asked for, or "list" and "inspect" for reading.
	Action string `json:"action,omitempty"`
	// Chargers are the charge point IDs the call was about.
	Chargers []string `json:"chargers,omitempty"`
	// Message says how the call ended when that needs words.
	Message string `json:"message,omitempty"`
}

// Line returns the entry as one line of text.
func (e Entry) Line() string {
	what := e.Action
	if what == "" {
		what = e.Method + " " + e.Path
	}
	if len(e.Chargers) > 0 {
		what += " " + strings.Join(e.Chargers, ",")
	}
	line := fmt.Sprintf("%s %s %s %d %s", e.Time.Format("2006-01-02T15:04:05.000Z07:00"), e.Token, e.Remote, e.Status, what)
	if e.Message != "" {
		line += ": " + e.Message
	}
	return line
}

func auditPath(dataDir string) string { return filepath.Join(dataDir, "api.log") }

// audit writes entries to the log of the API and, for an API in the
// foreground, to the terminal.
type audit struct {
	mu   sync.Mutex
	file *logfile.Writer
	out  io.Writer
}

func openAudit(dataDir string, out io.Writer) (*audit, error) {
	file, err := logfile.Open(auditPath(dataDir), 0, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open the log of the API: %w", err)
	}
	return &audit{file: file, out: out}, nil
}

func (a *audit) record(e Entry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if data, err := json.Marshal(e); err == nil {
		_, _ = a.file.Write(append(data, '\n'))
	}
	if a.out != nil {
		fmt.Fprintln(a.out, e.Line())
	}
}

func (a *audit) close() { _ = a.file.Close() }

// ReadLog returns the calls recorded in the data directory, oldest first.
func ReadLog(dataDir string) ([]Entry, error) {
	var entries []Entry
	for _, path := range logfile.Paths(auditPath(dataDir)) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		lines := bufio.NewScanner(f)
		lines.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for lines.Scan() {
			var e Entry
			if json.Unmarshal(lines.Bytes(), &e) == nil {
				entries = append(entries, e)
			}
		}
		f.Close()
	}
	return entries, nil
}
