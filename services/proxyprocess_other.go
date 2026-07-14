//go:build !linux

package services

import "os/exec"

func configureMihomoProcess(_ *exec.Cmd) {}
