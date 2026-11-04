package shell

import (
	"errors"
	"testing"
)

// resolveBash picks the bash tool's interpreter from what the host has; on Windows it never picks the
// WSL launcher, which runs the command in a Linux distribution instead of the workspace.
func TestResolveBash(t *testing.T) {
	env := map[string]string{"ProgramFiles": `C:\Program Files`, "SystemRoot": `C:\Windows`}
	getenv := func(key string) string { return env[key] }
	for _, c := range []struct {
		name   string
		goos   string
		files  []string
		onPath string
		want   Interpreter
	}{
		{"unix bin bash", "linux", []string{"/bin/bash"}, "/usr/local/bin/bash", Interpreter{Path: "/bin/bash", Args: []string{"-lc"}, Name: "bash"}},
		{"unix bash on PATH", "darwin", nil, "/opt/homebrew/bin/bash", Interpreter{Path: "/opt/homebrew/bin/bash", Args: []string{"-lc"}, Name: "bash"}},
		{"unix without bash", "linux", nil, "", Interpreter{Path: "/bin/sh", Args: []string{"-lc"}, Name: "sh"}},
		{"windows git bash", "windows", []string{`C:\Program Files\Git\bin\bash.exe`}, `C:\Windows\System32\bash.exe`, Interpreter{Path: `C:\Program Files\Git\bin\bash.exe`, Args: []string{"-lc"}, Name: "bash"}},
		{"windows bash on PATH", "windows", nil, `C:\msys64\usr\bin\bash.exe`, Interpreter{Path: `C:\msys64\usr\bin\bash.exe`, Args: []string{"-lc"}, Name: "bash"}},
		{"windows WSL launcher only", "windows", nil, `C:\WINDOWS\system32\bash.exe`, Interpreter{Path: "cmd.exe", Args: []string{"/C"}, Name: "cmd.exe"}},
		{"windows nothing", "windows", nil, "", Interpreter{Path: "cmd.exe", Args: []string{"/C"}, Name: "cmd.exe"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			exists := func(path string) bool {
				for _, f := range c.files {
					if f == path {
						return true
					}
				}
				return false
			}
			lookPath := func(string) (string, error) {
				if c.onPath == "" {
					return "", errors.New("not found")
				}
				return c.onPath, nil
			}
			got := resolveBash(c.goos, getenv, exists, lookPath)
			if got.Path != c.want.Path || got.Name != c.want.Name || got.String() != c.want.String() {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
	if !isWSLLauncher(`D:\Win\Sysnative\bash.exe`, `D:\Win`) || isWSLLauncher(`D:\Win\bash.exe`, `D:\Win`) {
		t.Fatal("WSL launcher detection ignores SystemRoot")
	}
}
