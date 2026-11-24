package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	userEnvironmentKey = "Environment"
	userPathValue      = "Path"
)

// userPath reads the PATH the user has set for their account, as it is
// stored: with its %VARIABLES% not filled in.
func userPath() (value string, valueType uint32, err error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, userEnvironmentKey, registry.QUERY_VALUE)
	if err != nil {
		return "", 0, err
	}
	defer key.Close()
	return key.GetStringValue(userPathValue)
}

// onUserPath reports whether a folder is on the user's PATH, where the
// install script puts the folder of the program.
func onUserPath(dir string) bool {
	value, _, err := userPath()
	if err != nil {
		return false
	}
	_, found := withoutPathEntry(value, dir, ";")
	return found
}

// removeFromUserPath takes a folder off the user's PATH and tells running
// programs that the environment changed.
func removeFromUserPath(dir string) error {
	value, valueType, err := userPath()
	if err != nil {
		return err
	}
	rest, found := withoutPathEntry(value, dir, ";")
	if !found {
		return nil
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, userEnvironmentKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if valueType == registry.EXPAND_SZ {
		err = key.SetExpandStringValue(userPathValue, rest)
	} else {
		err = key.SetStringValue(userPathValue, rest)
	}
	if err != nil {
		return err
	}

	// Terminals opened from now on see the new PATH.
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001a
		abortIfHung     = 0x0002
	)
	what, _ := windows.UTF16PtrFromString(userEnvironmentKey)
	var result uintptr
	_, _, _ = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW").Call(
		hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(what)), abortIfHung, 1000, uintptr(unsafe.Pointer(&result)))
	return nil
}

// removeProgram takes the program away. Windows does not let a running
// program be deleted, only renamed, so the program is renamed now, and the
// function returned starts a hidden command that deletes it, and its folder
// when asked to, once plugyard has ended.
func removeProgram(exe string, withFolder bool) (finish func() string, err error) {
	old := exe + ".old"
	_ = os.Remove(old) // what an earlier update left
	if err := os.Rename(exe, old); err != nil {
		var link *os.LinkError
		if errors.As(err, &link) {
			err = link.Err
		}
		return nil, err
	}

	return func() string {
		// ping is the wait that works without a console.
		script := fmt.Sprintf(`ping -n 3 127.0.0.1 >nul & del /f /q "%s"`, old)
		if withFolder {
			// rmdir without /s removes a folder only when it is empty.
			script += fmt.Sprintf(` & rmdir "%s"`, filepath.Dir(exe))
		}
		shell := os.Getenv("ComSpec")
		if shell == "" {
			shell = "cmd.exe"
		}
		helper := exec.Command(shell)
		helper.Dir = os.TempDir() // not the folder that is about to go
		helper.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
			// cmd has quoting rules of its own, so the line is given as it is.
			CmdLine: fmt.Sprintf(`"%s" /d /c "%s"`, shell, script),
		}
		if err := helper.Start(); err != nil {
			return fmt.Sprintf("The program is now %s; delete that file yourself.", old)
		}
		_ = helper.Process.Release()
		return fmt.Sprintf("The program file is deleted a few seconds from now. If %s is still there after that, delete it yourself.", old)
	}, nil
}
