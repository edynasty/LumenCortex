package jobs

import (
	"io"
	"os"
	"os/exec"
)

// execCommand is one started shell command: the process the manager signals and the wait it observes.
type execCommand struct {
	cmd *exec.Cmd
}

// Pid is the process id of the command, which is also its process group's id.
func (c *execCommand) Pid() int { return c.cmd.Process.Pid }

// Wait waits for the command to exit and its pipes to close (bounded by the wait delay the start set).
func (c *execCommand) Wait() error { return c.cmd.Wait() }

// ProcessState is the exit state Wait recorded, nil before it returns.
func (c *execCommand) ProcessState() *os.ProcessState { return c.cmd.ProcessState }

// startCommand starts the command in its own process group, so a kill reaches everything it started.
// The command runs through /bin/sh -lc in dir, exactly as the foreground shell tool does.
func startCommand(dir, command string, stdout, stderr io.Writer) (*execCommand, error) {
	cmd := exec.Command(shellPath(), shellArgs(command)...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// A surviving background child must not keep the job open past the grace period.
	cmd.WaitDelay = killGrace
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execCommand{cmd: cmd}, nil
}
