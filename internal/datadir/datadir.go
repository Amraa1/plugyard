// Package datadir locates plugyard's per-user data directory, which holds
// the sockets of running sites and, later, their saved state and logs.
package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// Env names an environment variable that overrides the data directory.
const Env = "PLUGYARD_DATA_DIR"

// Path returns the data directory:
//
//	Windows  %LOCALAPPDATA%\plugyard
//	macOS    ~/Library/Application Support/plugyard
//	Linux    $XDG_DATA_HOME/plugyard, or ~/.local/share/plugyard
//
// It does not create the directory.
func Path() (string, error) {
	if dir := os.Getenv(Env); dir != "" {
		return dir, nil
	}

	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, "plugyard"), nil
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "plugyard"), nil
		}
	default:
		if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
			return filepath.Join(dir, "plugyard"), nil
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "plugyard"), nil
		}
	}
	return "", errors.New("cannot work out where to keep plugyard's data; set " + Env)
}
