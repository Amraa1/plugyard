// Package logfile writes a site's log to plain files that rotate, so that
// the log never takes more than a set amount of disk.
package logfile

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The cap of a site's log when its site file does not say: 3 files of 10 MB.
const (
	DefaultMaxSize = 10_000_000
	DefaultFiles   = 3
)

// Bounds of what a site file may ask for.
const (
	MinSize  = 1000
	MaxFiles = 100
)

// Writer appends to a log file and rotates it when it is full: the file is
// renamed to <path>.1, what was <path>.1 to <path>.2, and so on, and the
// oldest file is deleted. Every Write should be one or more whole lines.
type Writer struct {
	path    string
	maxSize int64
	files   int

	mu   sync.Mutex
	f    *os.File
	size int64
	// retry is when a rotation that failed is tried again.
	retry time.Time
}

// Open opens the log at path for appending. At most files files are kept,
// the current one included, each of at most maxSize bytes. Zero stands for
// the defaults.
func Open(path string, maxSize int64, files int) (*Writer, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	if files <= 0 {
		files = DefaultFiles
	}
	w := &Writer{path: path, maxSize: maxSize, files: files}
	if err := w.open(false); err != nil {
		return nil, err
	}
	// Files beyond the cap are left over from a larger setting.
	for n := files; n <= MaxFiles; n++ {
		_ = os.Remove(numbered(path, n))
	}
	return w, nil
}

func numbered(path string, n int) string {
	if n == 0 {
		return path
	}
	return path + "." + strconv.Itoa(n)
}

func (w *Writer) open(truncate bool) error {
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if truncate {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(w.path, flags, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, info.Size()
	return nil
}

// Write appends p, rotating first when p would take the file past its cap.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, errors.New("the log is closed")
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize && !time.Now().Before(w.retry) {
		if err := w.rotate(); err != nil {
			// Another program has a file open, as can happen on Windows.
			// The log grows past its cap for a moment instead of losing
			// lines.
			w.retry = time.Now().Add(time.Second)
		}
		if w.f == nil {
			return 0, errors.New("the log could not be opened again after rotating")
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *Writer) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil

	var failed error
	if w.files > 1 {
		// Each rename replaces the file one older, so the oldest goes
		// without a moment in which fewer files exist than are kept.
		for n := w.files - 1; n >= 1 && failed == nil; n-- {
			err := os.Rename(numbered(w.path, n-1), numbered(w.path, n))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				failed = err
			}
		}
	}
	// With a single file there is nowhere to move it: it starts over.
	if err := w.open(w.files == 1); err != nil {
		return err
	}
	return failed
}

// Close closes the log.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Paths returns the files of the log at path that exist, oldest first.
func Paths(path string) []string {
	var out []string
	for n := MaxFiles; n >= 0; n-- {
		if _, err := os.Stat(numbered(path, n)); err == nil {
			out = append(out, numbered(path, n))
		}
	}
	return out
}

// Rotated returns the name of the file the log at path was last rotated to.
func Rotated(path string) string { return numbered(path, 1) }

// Remove deletes every file of the log at path.
func Remove(path string) error {
	for n := 0; n <= MaxFiles; n++ {
		if err := os.Remove(numbered(path, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ParseSize reads a size such as "10MB", "500kB" or "1GB" into bytes.
func ParseSize(s string) (int64, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	units := []struct {
		suffix string
		factor float64
	}{{"gb", 1e9}, {"mb", 1e6}, {"kb", 1e3}, {"b", 1}}
	for _, u := range units {
		number, ok := strings.CutSuffix(lower, u.suffix)
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
		if err != nil || value <= 0 {
			break
		}
		return int64(value * u.factor), nil
	}
	return 0, fmt.Errorf("invalid size %q: want a positive number with a unit, such as 10MB", s)
}
