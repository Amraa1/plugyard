package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableTerminalColors switches the Windows console behind f to interpret
// ANSI color codes, and reports whether that worked. Older consoles print
// the codes as text unless this mode is on.
func enableTerminalColors(f *os.File) bool {
	handle := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
