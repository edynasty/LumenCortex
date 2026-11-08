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
	"github.com/edynasty/LumenCortex/internal/safefs"
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

	// BubblewrapPath is the bwrap the Linux backend runs. Empty means "bwrap" on
	// PATH. A deployment that grants user namespaces to one dedicated, LC-only
	// copy through an AppArmor profile attached to that path (rather than to the
	// public /usr/bin/bwrap, which any caller could use) names it here. It must be
	// an absolute path to a regular file owned by root and writable by no one
	// else; anything else fails closed at New.
	BubblewrapPath string
	// RequireCallerIsolation is the production gate of the Linux backend: New
	// fails closed unless the user-namespace grant behind BubblewrapPath cannot
	// be borrowed by other callers (see checkCallerIsolation). It requires
	// BubblewrapPath. Development hosts leave it off.
	RequireCallerIsolation bool
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
		path := strings.TrimSpace(opts.BubblewrapPath)
		if opts.RequireCallerIsolation && path == "" {
			return nil, fmt.Errorf("%w: caller isolation requires a dedicated bubblewrap path", evalapi.ErrSandboxUnsupported)
		}
		if path != "" {
			if err := checkTrustedExecutable(path); err != nil {
				return nil, fmt.Errorf("%w: bubblewrap path: %v", evalapi.ErrSandboxUnsupported, err)
			}
			if opts.RequireCallerIsolation {
				if err := checkCallerIsolation(path); err != nil {
					return nil, fmt.Errorf("%w: bubblewrap caller isolation: %v", evalapi.ErrSandboxUnsupported, err)
				}
			}
			return &launcher{goos: goos, backend: backend, exe: path}, nil
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
	}
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
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup-try",
		"--new-session",
	}
	if !spec.network {
		args = append(args, "--unshare-net")
	}
	for _, dir := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
		args = append(args, "--ro-bind-try", dir, dir)
	}
	// Two read-only system configuration entries, never the rest of /etc:
	// Debian/Ubuntu resolve shared libraries such as libblas.so.3 through the
	// /etc/alternatives symlink farm (NumPy cannot load without it), and their
	// Matplotlib reads its packaged defaults from /etc/matplotlibrc.
	for _, path := range []string{"/etc/alternatives", "/etc/matplotlibrc"} {
		args = append(args, "--ro-bind-try", path, path)
	}
	// Establish private scratch mounts before explicit runtime/workspace binds.
	// Otherwise a legitimate root below /tmp is hidden after it was mounted.
	args = append(args, "--tmpfs", "/tmp", "--tmpfs", "/run", "--dir", "/var", "--symlink", "/run", "/var/run")
	args = append(args, "--ro-bind-try", spec.workersDir, spec.workersDir)
	// Executable identity is host-validated; mounting its parent would silently
	// grant the plugin unrelated host files and sockets.
	args = append(args, "--ro-bind", spec.exe, spec.exe)
	for _, extra := range spec.extraRead {
		args = append(args, "--ro-bind-try", extra, extra)
	}
	args = append(args, "--bind", spec.kernelDir, spec.kernelDir)
	args = append(args, "--chdir", spec.dir, "--proc", "/proc", "--dev", "/dev")
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
	b.WriteString("(allow sysctl-read)\n(allow ipc-posix-shm*)\n")
	// Signals are how a cell is interrupted; without this the host's SIGINT is
	// dropped, the worker never sees the interrupt, and every timeout would be a
	// hard kill that loses the kernel.
	b.WriteString("(allow signal)\n")
	// getcwd needs ancestor directories, not a global directory-content grant.
	b.WriteString("(allow file-read-metadata)\n")
	for dir := resolvePath(spec.kernelDir); ; dir = filepath.Dir(dir) {
		fmt.Fprintf(&b, "(allow file-read-data (require-all (literal %q) (vnode-type DIRECTORY)))\n", dir)
		if dir == "/" {
			break
		}
	}
	b.WriteString(`(allow file-read* (subpath "/System") (subpath "/usr/lib") (subpath "/usr/share") (subpath "/bin") (subpath "/sbin") (subpath "/dev") (subpath (param "WORKERS")) (literal (param "EXE")) (literal (param "EXEREAL")))` + "\n")
	for _, file := range []string{"/private/etc/localtime", "/private/etc/hosts", "/private/etc/resolv.conf"} {
		fmt.Fprintf(&b, "(allow file-read* (literal %q))\n", file)
	}
	b.WriteString(`(allow file-read* file-write* (subpath (param "KERNEL")))` + "\n")
	b.WriteString(`(allow file-map-executable (literal (param "EXE")) (literal (param "EXEREAL")))` + "\n")
	for _, extra := range spec.extraRead {
		fmt.Fprintf(&b, "(allow file-read* file-map-executable (subpath %q))\n", resolvePath(extra))
	}
	b.WriteString(`(allow file-write* (literal "/dev/null") (literal "/dev/stdout") (literal "/dev/stderr") (literal "/dev/urandom") (literal "/dev/dtracehelper"))` + "\n")
	if !spec.network {
		b.WriteString("(deny network*)\n")
	}
	return b.String()
}

func writeSeatbeltProfile(spec launchSpec) error {
	if spec.profilePath == "" {
		return errors.New("eval: sandbox profile path is required")
	}
	root, err := safefs.Open(resolvePath(filepath.Dir(spec.profilePath)))
	if err != nil {
		return err
	}
	defer root.Close()
	return root.WriteFile(filepath.Base(spec.profilePath), []byte(seatbeltProfile(spec)), 0600)
}
