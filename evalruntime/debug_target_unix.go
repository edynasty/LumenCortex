//go:build !windows

package evalruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// This fixed reexec mode is entered only after ProgramCommand's sandbox and
// StartManaged's admission gate. It owns both target and adapter; no shell text,
// mount, credential or environment is accepted over the debugger protocol.
func init() {
	if len(os.Args) < 2 || os.Args[1] != "--lc-debug-target" {
		return
	}
	os.Exit(runDebugTarget(os.Args[2:]))
}

func runDebugTarget(args []string) int {
	if len(args) < 3 {
		return 126
	}
	count, err := strconv.Atoi(args[1])
	if err != nil || count < 0 || count > 128 || len(args) < count+3 {
		return 126
	}
	identity := os.NewFile(3, "debug-target-identity")
	if identity == nil {
		return 126
	}
	syscall.CloseOnExec(3)
	target := exec.Command(args[0], args[2:2+count]...)
	target.Env = os.Environ()
	target.Stderr = os.Stderr
	target.Stdout = os.Stderr
	if err = target.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 126
	}
	// Do not Wait here. An exited but unreaped child keeps the PID reserved, so
	// an attach can fail but can never hit an unrelated reused numeric PID.
	defer func() { _ = target.Process.Kill(); _ = target.Wait() }()
	marker, err := json.Marshal(map[string]int{"pid": target.Process.Pid, "parentPID": os.Getpid(), "uid": os.Getuid(), "gid": os.Getgid()})
	if err != nil {
		return 126
	}
	if _, err = identity.Write(append(marker, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 126
	}
	_ = identity.Close()
	adapter := exec.Command(args[2+count], args[3+count:]...)
	adapter.Env = os.Environ()
	adapter.Stdin = os.Stdin
	adapter.Stdout = os.Stdout
	adapter.Stderr = os.Stderr
	if err = adapter.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok {
				return status.ExitStatus()
			}
		}
		return 126
	}
	return 0
}
