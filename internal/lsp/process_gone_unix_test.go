//go:build !windows

package lsp

import (
	"errors"
	"syscall"
)

func processGone(pid int) bool { return pid <= 0 || errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }
