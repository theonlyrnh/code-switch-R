//go:build linux

package services

import (
	"os/exec"
	"syscall"
)

// configureMihomoProcess makes the runtime-owned Mihomo process die with the
// application if shutdown hooks cannot run. SIGKILL avoids leaving a stale
// local listener alive after an unclean application exit.
func configureMihomoProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
