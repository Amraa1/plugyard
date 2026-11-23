//go:build unix

package cli

import (
	"fmt"
	"os/exec"
	"syscall"
)

// setDetached arranges for cmd to outlive the terminal: it starts in a new
// session with no controlling terminal, so the hang-up sent when the
// terminal closes never reaches it.
func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// fileLimitMargin is how many open files plugyard needs besides one per
// charger: its sockets, log files and the Go runtime's own.
const fileLimitMargin = 64

// checkFileLimit makes sure the process may open enough files for the given
// number of chargers, each of which holds a connection. It raises the limit
// itself where the system allows, and explains what to do where it does not.
func checkFileLimit(chargers int) error {
	need := uint64(chargers) + fileLimitMargin

	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return nil // cannot tell; let the connections speak for themselves
	}
	if uint64(limit.Cur) >= need {
		return nil
	}

	raised := limit
	raised.Cur = limit.Max
	if uint64(raised.Cur) >= need && syscall.Setrlimit(syscall.RLIMIT_NOFILE, &raised) == nil {
		return nil
	}
	return fmt.Errorf("%d chargers need about %d open files, but this system allows the process %d; raise the limit with \"ulimit -n %d\" and try again",
		chargers, need, uint64(limit.Cur), need)
}
