package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edynasty/LumenCortex/evalruntime"
	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/streambuf"
)

// CommandHookOptions is trusted host configuration, never tool or workspace input.
// A command receives one JSON LifecycleRequest on stdin and returns exactly one
// LifecycleResult on stdout. Its sandbox has only a disposable workspace, explicit
// read paths and no network, Engine handle, inherited environment or debug grant.
type CommandHookOptions struct {
	ID            string
	Version       int
	Events        []LifecycleEvent
	Priority      int
	Timeout       time.Duration
	FailurePolicy HookFailurePolicy
	MutableArgs   bool
	Safety        bool
	Command       string
	Args          []string
	ReadPaths     []string
	Sandbox       evalruntime.SandboxOptions
}

type commandLifecycleHook struct{ opts CommandHookOptions }

// RegisterCommandHook uses the existing lifecycle registry with an isolated process
// implementation. There is no plugin discovery, automatic installation or fallback
// to an unsandboxed command. Linux requires the same caller-isolation admission as
// other sandboxed external programs.
func (e *Engine) RegisterCommandHook(opts CommandHookOptions) error {
	if !filepath.IsAbs(opts.Command) {
		return errors.New("hook command must be absolute")
	}
	command, err := filepath.EvalSymlinks(opts.Command)
	if err != nil {
		return err
	}
	info, err := os.Stat(command)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("hook command must be executable")
	}
	if opts.Sandbox.AllowNetwork {
		return errors.New("command hooks cannot enable network")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.Timeout > 30*time.Second {
		return errors.New("hook timeout exceeds 30 seconds")
	}
	opts.Command = command
	opts.Events = slices.Clone(opts.Events)
	opts.Args = slices.Clone(opts.Args)
	opts.ReadPaths = slices.Clone(opts.ReadPaths)
	return e.RegisterLifecycleHook(&commandLifecycleHook{opts: opts})
}

func (h *commandLifecycleHook) ID() string                       { return h.opts.ID }
func (h *commandLifecycleHook) Version() int                     { return h.opts.Version }
func (h *commandLifecycleHook) Events() []LifecycleEvent         { return slices.Clone(h.opts.Events) }
func (h *commandLifecycleHook) Priority() int                    { return h.opts.Priority }
func (h *commandLifecycleHook) Timeout() time.Duration           { return h.opts.Timeout }
func (h *commandLifecycleHook) FailurePolicy() HookFailurePolicy { return h.opts.FailurePolicy }
func (h *commandLifecycleHook) MutableArgs() bool                { return h.opts.MutableArgs }
func (h *commandLifecycleHook) Safety() bool                     { return h.opts.Safety }

func (h *commandLifecycleHook) Handle(ctx context.Context, req LifecycleRequest) (result LifecycleResult, err error) {
	input, err := json.Marshal(req)
	if err != nil {
		return result, err
	}
	if len(input) > 64<<10 {
		return result, errors.New("hook input exceeds 64 KiB")
	}
	root, err := os.MkdirTemp("", "lc-hook-")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	// Leave a portion of the hook deadline for process-tree reap and pipe drain.
	life, cancel := context.WithTimeout(ctx, h.opts.Timeout-min(h.opts.Timeout/4, 2*time.Second))
	defer cancel()
	cmd, err := evalruntime.ProgramCommand(life, evalruntime.ProgramOptions{
		Sandbox: h.opts.Sandbox, Executable: h.opts.Command, Args: h.opts.Args,
		Workspace: root, PrivateDir: root, ReadPaths: h.opts.ReadPaths,
		Env:        []string{"PATH=/usr/bin:/bin", "HOME=" + root, "TMPDIR=" + filepath.Join(root, "tmp")},
		CPUSeconds: max(1, int(h.opts.Timeout.Seconds())), FileBytes: 1 << 20,
	})
	if err != nil {
		return result, err
	}
	stdout, stderr := streambuf.New(64<<10, 0), streambuf.New(0, 0)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 100 * time.Millisecond
	owned, err := shell.StartManaged(cmd)
	if err != nil {
		return result, err
	}
	if err = owned.Wait(); err != nil {
		return result, fmt.Errorf("hook process failed: %w", err)
	}
	if err = life.Err(); err != nil {
		return result, err
	}
	output := stdout.Snapshot()
	if output.Truncated {
		return result, errors.New("hook response exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Head))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("invalid hook response: %w", err)
	}
	switch result.Decision {
	case HookContinue, HookDeny, HookModifyAllowedFields:
	default:
		return LifecycleResult{}, errors.New("invalid hook decision")
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return LifecycleResult{}, errors.New("hook must return exactly one JSON result")
	}
	return result, nil
}
