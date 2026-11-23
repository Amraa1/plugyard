package cli

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// setDetached arranges for cmd to outlive the terminal: it starts with no
// console attached and in its own process group, so closing the console
// window or pressing Ctrl+C in it does not reach it.
func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// checkFileLimit has nothing to check on Windows, which does not cap the
// number of sockets a process may open in the way Unix systems do.
func checkFileLimit(int) error { return nil }
