//go:build unix

package cli

import (
	"os"
	"path/filepath"
)

// onUserPath reports whether an install script put a folder on the user's
// PATH. Only the Windows one does.
func onUserPath(string) bool { return false }

func removeFromUserPath(string) error { return nil }

// removeProgram deletes the program, and its folder when asked to. The
// function it returns ends the work and may give a line to print.
func removeProgram(exe string, withFolder bool) (finish func() string, err error) {
	if err := os.Remove(exe); err != nil {
		return nil, err
	}
	return func() string {
		if withFolder {
			_ = os.Remove(filepath.Dir(exe))
		}
		return ""
	}, nil
}
