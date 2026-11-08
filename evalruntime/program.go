package evalruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edynasty/LumenCortex/evalapi"
)

// ProgramOptions is host-owned configuration for an external program adapter.
// It shares the Eval filesystem/network launcher, never the Eval kernel or RPC.
// The caller supplies lifetime/resource containment before starting the command.
// Nothing here accepts model-authored sandbox rules or environment inheritance.
type ProgramOptions struct {
	Sandbox    SandboxOptions
	Executable string
	Args       []string
	Workspace  string
	PrivateDir string
	ReadPaths  []string
	Env        []string
	CPUSeconds int
	FileBytes  int64
	// Loopback is an explicit adapter host grant. It permits localhost services,
	// not external network; ordinary Eval workers never receive it.
	Loopback bool
	Socket   string
	// PrepareTarget runs this workspace executable beside the adapter. The host
	// retains it as an unreaped child until the adapter exits, pinning its PID.
	PrepareTarget  string
	TargetArgs     []string
	TargetIdentity *os.File
}

func ProgramCommand(ctx context.Context, opts ProgramOptions) (*exec.Cmd, error) {
	workspace, err := filepath.EvalSymlinks(opts.Workspace)
	if err != nil {
		return nil, err
	}
	private, err := filepath.EvalSymlinks(opts.PrivateDir)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(private) {
		return nil, errors.New("program sandbox requires absolute roots")
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("program workspace is not a directory")
	}
	exe, err := filepath.EvalSymlinks(opts.Executable)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(exe) {
		return nil, errors.New("program adapter must be absolute")
	}
	l, err := newLauncher(opts.Sandbox)
	if err != nil {
		return nil, err
	}
	if l.backend == evalapi.SandboxBubblewrap && !opts.Sandbox.RequireCallerIsolation {
		return nil, errors.New("program sandbox requires verified Linux caller isolation")
	}
	spec := launchSpec{exe: exe, args: opts.Args, dir: workspace, kernelDir: workspace, workersDir: private, profilePath: filepath.Join(private, "adapter.sb"), allowSubprocess: true, network: opts.Sandbox.AllowNetwork, cpuSeconds: opts.CPUSeconds, fileBytes: opts.FileBytes, extraRead: opts.ReadPaths}
	spec.debugProgram = true
	spec.loopbackOnly = opts.Loopback
	if err := os.Mkdir(filepath.Join(private, "tmp"), 0700); err != nil {
		return nil, err
	}
	if opts.Socket != "" {
		if filepath.Dir(opts.Socket) != private {
			return nil, errors.New("debug socket must be in private adapter directory")
		}
		spec.debugSocket = opts.Socket
	}
	if opts.PrepareTarget != "" {
		if opts.TargetIdentity == nil {
			return nil, errors.New("attach target requires private identity pipe")
		}
		target, err := filepath.EvalSymlinks(opts.PrepareTarget)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(workspace, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("attach target outside workspace")
		}
		helper, err := os.Executable()
		if err != nil {
			return nil, err
		}
		spec.extraRead = append(append([]string(nil), spec.extraRead...), exe)
		spec.exe = resolvePath(helper)
		spec.args = append([]string{"--lc-debug-target", target, strconv.Itoa(len(opts.TargetArgs))}, opts.TargetArgs...)
		spec.args = append(spec.args, exe)
		spec.args = append(spec.args, opts.Args...)
	}
	cmd, err := l.command(spec)
	if err != nil {
		return nil, err
	}
	bounded := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	bounded.Dir = workspace
	bounded.Env = opts.Env
	if opts.PrepareTarget != "" {
		bounded.ExtraFiles = []*os.File{opts.TargetIdentity}
	}
	return bounded, nil
}
