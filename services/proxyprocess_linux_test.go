//go:build linux

package services

import (
	"syscall"
	"testing"
)

func TestNewMihomoCommandUsesParentDeathSignal(t *testing.T) {
	command := newMihomoCommand("mihomo", "/tmp/runtime", "/tmp/runtime/config.yaml")
	if command.SysProcAttr == nil {
		t.Fatal("Mihomo command is missing process attributes")
	}
	if command.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("Mihomo parent-death signal = %v, want %v", command.SysProcAttr.Pdeathsig, syscall.SIGKILL)
	}
}
