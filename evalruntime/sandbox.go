package evalruntime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/edynasty/LumenCortex/evalapi"
)

// SandboxOptions selects the containment backend. It is host configuration: no
// manifest, model request or cell can change it.
type SandboxOptions struct {
	// Backend is "" (auto: seatbelt on macOS, bubblewrap on Linux),
	// evalapi.SandboxSeatbelt or evalapi.SandboxBubblewrap. Any other value, or a
	// platform without the named backend, fails closed at New.
	Backend string
	// AllowNetwork keeps the worker's network namespace. The default (false)
	// denies network at the kernel level.
	AllowNetwork bool

	// EnvAllow names host environment variables copied verbatim into the worker.
	// It is an exact-name allowlist: no prefix rule is treated as safe, and no
	// credential, socket or proxy variable is included implicitly.
	EnvAllow []string
	// ExtraReadPaths are additional host paths the worker may read. They are
	// host-owned and never derived from a cell.
	ExtraReadPaths []string
}

// launcher builds the sandboxed command line for one worker process.
type launcher struct {
	goos    string
	backend string
	exe     string
}

func newLauncher(opts SandboxOptions) (*launcher, error) {
	goos := runtime.GOOS
	backend := strings.TrimSpace(opts.Backend)
	switch goos {
	case "darwin":
		if backend == "" {
			backend = evalapi.SandboxSeatbelt
		}
		if backend != evalapi.SandboxSeatbelt {
			return nil, fmt.Errorf("%w: backend %q is not available on darwin", evalapi.ErrSandboxUnsupported, backend)
		}
		path, err := exec.LookPath("sandbox-exec")
		if err != nil {
			path = "/usr/bin/sandbox-exec"
			if _, statErr := os.Stat(path); statErr != nil {
				return nil, fmt.Errorf("%w: sandbox-exec not found; refusing to run cells without containment", evalapi.ErrSandboxUnsupported)
			}
		}
		return &launcher{goos: goos, backend: backend, exe: path}, nil
	case "linux":
		if backend == "" {
			backend = evalapi.SandboxBubblewrap
		}
		if backend != evalapi.SandboxBubblewrap {
			return nil, fmt.Errorf("%w: backend %q is not available on linux", evalapi.ErrSandboxUnsupported, backend)
		}
		path, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, fmt.Errorf("%w: bubblewrap (bwrap) is required on linux; refusing to run cells without real containment", evalapi.ErrSandboxUnsupported)
		}
		return &launcher{goos: goos, backend: backend, exe: path}, nil
	default:
		return nil, fmt.Errorf("%w: platform %s has no supported sandbox backend", evalapi.ErrSandboxUnsupported, goos)
	}
}

type launchSpec struct {
	exe             string
	args            []string
	dir             string
	env             []string
	kernelDir       string
	profilePath     string
	workersDir      string
	cpuSeconds      int
	fileBytes       int64
	network         bool
	allowSubprocess bool
	extraRead       []string
}

func (l *launcher) command(spec launchSpec) (*exec.Cmd, error) {
	switch l.backend {
	case evalapi.SandboxSeatbelt:
		return l.seatbeltCommand(spec)
	case evalapi.SandboxBubblewrap:
		return l.bubblewrapCommand(spec)
	}
	return nil, fmt.Errorf("%w: backend %q", evalapi.ErrSandboxUnsupported, l.backend)
}

// ulimitScript applies kernel-enforced CPU and file-size limits, then execs the
// interpreter. macOS cannot limit address space (RLIMIT_AS/DATA are rejected), so
// memory is bounded by the host monitor, not here.
func ulimitScript(cpuSeconds int, fileBytes int64) string {
	var b strings.Builder
	if cpuSeconds > 0 {
		fmt.Fprintf(&b, "ulimit -t %d 2>/dev/null; ", cpuSeconds)
	}
	if fileBytes > 0 {
		blocks := fileBytes / 512
		if blocks < 1 {
			blocks = 1
		}
		fmt.Fprintf(&b, "ulimit -f %d 2>/dev/null; ", blocks)
	}
	b.WriteString(`exec "$@"`)
	return b.String()
}

func (l *launcher) seatbeltCommand(spec launchSpec) (*exec.Cmd, error) {
	if err := writeSeatbeltProfile(spec); err != nil {
		return nil, err
	}
	exeDir := filepath.Dir(spec.exe)
	// Seatbelt matches resolved vnodes, and macOS makes /var a symlink to
	// /private/var (and a test workspace may be a symlink too), so every path in
	// the profile is resolved first. The interpreter may be a symlink
	// (python3 -> python3.10); both spellings are allowed.
	exeReal := resolvePath(spec.exe)
	args := []string{
		"-f", spec.profilePath,
		"-D", "KERNEL=" + resolvePath(spec.kernelDir),
		"-D", "WORKERS=" + resolvePath(spec.workersDir),
		"-D", "EXE=" + spec.exe,
		"-D", "EXEREAL=" + exeReal,
		"-D", "EXEDIR=" + resolvePath(exeDir),
	}
	args = append(args, "-D", "PREFIX="+resolvePath(interpreterPrefix(spec.exe)))
	script := ulimitScript(spec.cpuSeconds, spec.fileBytes)
	args = append(args, "/bin/sh", "-c", script, "lc-eval", spec.exe)
	args = append(args, spec.args...)
	return exec.Command(l.exe, args...), nil
}

// resolvePath follows symlinks so a profile names the vnode the kernel checks.
func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func (l *launcher) bubblewrapCommand(spec launchSpec) (*exec.Cmd, error) {
	args := []string{
		"--die-with-parent",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup-try",
		"--new-session",
	}
	if !spec.network {
		args = append(args, "--unshare-net")
	}
	for _, dir := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/opt", "/run"} {
		args = append(args, "--ro-bind-try", dir, dir)
	}
	args = append(args, "--ro-bind-try", spec.workersDir, spec.workersDir)
	exeDir := filepath.Dir(spec.exe)
	args = append(args, "--ro-bind-try", exeDir, exeDir)
	for _, extra := range spec.extraRead {
		args = append(args, "--ro-bind-try", extra, extra)
	}
	args = append(args, "--bind", spec.kernelDir, spec.kernelDir)
	args = append(args, "--chdir", spec.dir, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	script := ulimitScript(spec.cpuSeconds, spec.fileBytes)
	args = append(args, "--", "/bin/sh", "-c", script, "lc-eval", spec.exe)
	args = append(args, spec.args...)
	return exec.Command(l.exe, args...), nil
}

// seatbeltProfile is the macOS containment policy: deny by default, allow the
// process and read-only system surface a language runtime needs, deny the user's
// home, other sessions' kernels, temporary directories and the network, then
// re-allow exactly this kernel, the worker scripts and the interpreter.
func seatbeltProfile(spec launchSpec) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	if spec.allowSubprocess {
		b.WriteString("(allow process*)\n")
	} else {
		// No fork: the worker cannot create a second process, so the owned tree is
		// exactly this process and a stop is complete by construction. Only the
		// wrapper shell and the interpreter itself may be exec'd (/bin/sh is bash
		// on macOS and re-execs /bin/bash for the "sh" variant).
		b.WriteString(`(allow process-exec (literal "/bin/sh") (literal "/bin/bash") (literal (param "EXE")) (literal (param "EXEREAL")))` + "\n")
	}
	b.WriteString("(allow sysctl-read)\n(allow mach-lookup)\n(allow ipc-posix-shm*)\n")
	// Signals are how a cell is interrupted; without this the host's SIGINT is
	// dropped, the worker never sees the interrupt, and every timeout would be a
	// hard kill that loses the kernel.
	b.WriteString("(allow signal)\n")
	// Contents are denied by default. Metadata is allowed so path resolution works,
	// and directory data is allowed so getcwd()/process.cwd() work: denying an
	// ancestor's data breaks Node and Python outright. Directory *listing* is the
	// accepted cost; reading a file outside the allowlist stays denied, including
	// other kernels (they are simply not listed) and every host document.
	b.WriteString("(allow file-read-metadata)\n")
	b.WriteString("(allow file-read-data (vnode-type DIRECTORY))\n")
	// Read-only system surface the loader and the language runtimes need, plus the
	// interpreter's own installation prefix.
	b.WriteString(`(allow file-read* (subpath "/System") (subpath "/usr") (subpath "/bin") (subpath "/sbin") (subpath "/private/etc") (subpath "/dev") (subpath (param "PREFIX")) (subpath (param "WORKERS")) (literal (param "EXE")) (literal (param "EXEREAL")))` + "\n")
	// /private/etc is allowed for locale and hosts; its key material is not.
	b.WriteString(`(deny file-read* file-write* (subpath "/private/etc/ssh"))` + "\n")
	b.WriteString(`(allow file-read* file-write* (subpath (param "KERNEL")))` + "\n")
	b.WriteString(`(allow file-map-executable (literal (param "EXE")) (literal (param "EXEREAL")) (subpath (param "PREFIX")))` + "\n")
	for _, extra := range spec.extraRead {
		fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", resolvePath(extra))
	}
	b.WriteString(`(allow file-write* (literal "/dev/null") (literal "/dev/stdout") (literal "/dev/stderr") (literal "/dev/urandom") (literal "/dev/dtracehelper"))` + "\n")
	if !spec.network {
		b.WriteString("(deny network*)\n")
	}
	return b.String()
}

// interpreterPrefix is the installation root a runtime may read: the parent of
// its bin directory (e.g. /opt/miniconda3/envs/x, or .../node/24).
func interpreterPrefix(exe string) string {
	return filepath.Dir(filepath.Dir(exe))
}

func writeSeatbeltProfile(spec launchSpec) error {
	if spec.profilePath == "" {
		return errors.New("eval: sandbox profile path is required")
	}
	if err := os.MkdirAll(filepath.Dir(spec.profilePath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(spec.profilePath, []byte(seatbeltProfile(spec)), 0o600)
}
