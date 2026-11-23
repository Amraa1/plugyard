//go:build !windows

package cli

import "os"

// enableTerminalColors reports whether the terminal behind f shows ANSI
// colors. Terminals on Unix systems do.
func enableTerminalColors(*os.File) bool { return true }
