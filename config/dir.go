package config

import (
	"os"
	"path/filepath"
)

// File and directory names in GlobalDir.
const (
	FileName      = "lumencortex.json"
	MCPFileName   = "mcp.json"
	SkillsDirName = "skills"
	appDirName    = "lumencortex"
)

// GlobalDir is the one global configuration root for lumencortex.json, mcp.json and skills/ on
// every OS (F9): $XDG_CONFIG_HOME/lumencortex when XDG_CONFIG_HOME is an absolute path, else
// ~/.config/lumencortex. It does not create the directory.
func GlobalDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, appDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", appDirName), nil
}

// legacyDirs are the directories earlier builds used for the same files: core's
// os.UserConfigDir()/lumencortex (Application Support on macOS, %AppData% on Windows) and
// Desktop's ~/.config/lumencortex. The current GlobalDir is excluded by the caller.
func legacyDirs() []string {
	var dirs []string
	if d, err := os.UserConfigDir(); err == nil && d != "" {
		dirs = append(dirs, filepath.Join(d, appDirName))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, ".config", appDirName))
	}
	return dirs
}

// LegacyWarnings reports each lumencortex.json, mcp.json or skills/ found in a directory earlier
// builds used that is not globalDir, with the path to move it to. It reads nothing from those
// files and moves nothing (§8.3). Load includes these in Effective.Warnings.
func LegacyWarnings(globalDir string) []Warning {
	var out []Warning
	seen := map[string]bool{}
	for _, dir := range legacyDirs() {
		if seen[dir] || sameDir(dir, globalDir) {
			continue
		}
		seen[dir] = true
		for _, name := range []string{FileName, MCPFileName, SkillsDirName} {
			old := filepath.Join(dir, name)
			info, err := os.Lstat(old)
			if err != nil {
				continue
			}
			if name == SkillsDirName {
				if st, err := os.Stat(old); err != nil || !st.IsDir() {
					continue
				}
			} else if info.IsDir() {
				continue
			}
			target := filepath.Join(globalDir, name)
			out = append(out, Warning{
				Code:   WarnLegacyPath,
				Path:   old,
				MoveTo: target,
				Message: "found " + old + " in a directory earlier builds used; it is not read. " +
					"Move it to " + target + " to use it",
			})
		}
	}
	return out
}

// sameDir reports whether a and b are the same directory: equal after symlink resolution, or the
// same file system object (case-insensitive volumes, bind mounts).
func sameDir(a, b string) bool {
	ca, errA := canonical(a)
	cb, errB := canonical(b)
	if errA == nil && errB == nil && ca == cb {
		return true
	}
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ia, ib)
}

// canonical returns the absolute path with every symlink resolved.
func canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
