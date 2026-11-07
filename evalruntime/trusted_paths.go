package evalruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

// A manifest names one executable, never an installation prefix or bind mount.
// Resolving the final symlink is host work; only the resulting regular executable
// becomes executable/readable, not either path's parent directory.
func trustedExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		p, err := exec.LookPath(path)
		if err != nil {
			return "", err
		}
		path = p
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("%w: runtime executable is not an executable regular file", evalapi.ErrInvalid)
	}
	return resolved, nil
}

// Python.org's macOS bin/python is a launcher that spawns its framework app.
// Execute the installed interpreter directly so no subprocess grant is needed.
func pythonRuntimeExecutable(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	script := `import os,sys,sysconfig
path=sys.executable
if sys.platform == 'darwin' and sysconfig.get_config_var('PYTHONFRAMEWORK'):
    path=os.path.join(sys.prefix,'Resources','Python.app','Contents','MacOS','Python')
print(path)`
	raw, err := exec.CommandContext(ctx, exe, "-I", "-c", script).Output()
	if err != nil {
		return "", err
	}
	if len(raw) > 16384 {
		return "", fmt.Errorf("runtime executable metadata too large")
	}
	return trustedExecutable(strings.TrimSpace(string(raw)))
}

// Built-in Python is selected explicitly by the host or PATH. Its own isolated
// sysconfig describes stdlib/site-packages and native library dependencies. No
// third-party Manifest can call this to acquire new mount roots.
func pythonReadPaths(exe string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	script := `import json,os,sysconfig
paths=[sysconfig.get_path(k) for k in ('stdlib','platstdlib','purelib','platlib')]+[sysconfig.get_config_var('LIBDIR')]
prefix=sysconfig.get_config_var('PYTHONFRAMEWORKPREFIX')
library=sysconfig.get_config_var('LDLIBRARY')
if prefix and library:
    paths.append(os.path.join(prefix,library))
print(json.dumps(list(set(paths))))`
	cmd := exec.CommandContext(ctx, exe, "-I", "-c", script)
	raw, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	if len(raw) > 16384 {
		return nil, fmt.Errorf("runtime install metadata too large")
	}
	var paths []string
	if err := json.Unmarshal(raw, &paths); err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	home, _ := os.UserHomeDir()
	home = resolvePath(home)
	for _, p := range paths {
		if p == "" {
			continue
		}
		p = resolvePath(p)
		if !filepath.IsAbs(p) || p == "/" || p == home || p == "/Users" || p == "/home" || p == "/run" || p == "/var" || p == "/etc" || p == "/private" || strings.Contains(p, "/.ssh") || strings.Contains(p, "/eval-workspaces/") {
			return nil, fmt.Errorf("%w: unsafe runtime library root %q", evalapi.ErrDenied, p)
		}
		if info, err := os.Stat(p); err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	return out, nil
}

func (m *Manager) probeBuiltin(p *provider) error {
	root, err := os.MkdirTemp(m.root, "probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	kernel := filepath.Join(root, "data")
	if err := os.Mkdir(kernel, 0700); err != nil {
		return err
	}
	args := []string{"-e", "console.log('lc-runtime-ready')"}
	if p.manifest.Language == "python" {
		args = []string{"-I", "-c", "print('lc-runtime-ready')"}
	}
	spec := launchSpec{exe: p.manifest.Executable, args: args, dir: kernel, kernelDir: kernel, profilePath: filepath.Join(root, "sandbox.sb"), workersDir: m.workersDir, cpuSeconds: 5, fileBytes: 1 << 20, extraRead: append(append([]string(nil), m.opts.Sandbox.ExtraReadPaths...), p.readPaths...)}
	cmd, err := m.launcher.command(spec)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	probe.Dir = kernel
	probe.Env = []string{"HOME=" + kernel, "TMPDIR=" + kernel, "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	raw, err := probe.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sandbox runtime probe: %w", err)
	}
	if strings.TrimSpace(string(raw)) != "lc-runtime-ready" {
		return fmt.Errorf("sandbox runtime probe did not acknowledge startup")
	}
	return nil
}
