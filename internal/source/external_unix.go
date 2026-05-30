//go:build unix

package source

import (
	"os/exec"
	"syscall"
)

// SetProcAttrs places the child in its own process group and replaces the
// default Cancel (which only kills the direct child) with one that signals
// the whole group. -pid in syscall.Kill is the standard "send signal to
// process group" idiom on POSIX.
//
// Exported so cooper's external source discoverer can reuse the same
// discipline without duplicating the platform-specific code.
func SetProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
