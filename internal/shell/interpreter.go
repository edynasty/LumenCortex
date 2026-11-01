package shell

import (
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// Interpreter is the program a command string runs in: Path, then Args, then the command.
type Interpreter struct {
	Path string
	Args []string
	// Name is what the interpreter is called where the model or a user reads it: "bash", "sh" or
	// "cmd.exe".
	Name string
}

// IsZero reports whether i names no program; a Runner then uses System.
func (i Interpreter) IsZero() bool { return i.Path == "" }

// String is the invocation as it would be typed, without the command: "/bin/bash -lc".
func (i Interpreter) String() string {
	return strings.TrimSpace(i.Path + " " + strings.Join(i.Args, " "))
}

// System is the interpreter of configured checks and of the host's RunShell: /bin/sh -lc, cmd.exe /C
// on Windows. It is what every command ran in before ADR-0019; an approved check keeps the meaning it
// was approved with.
func System() Interpreter {
	if runtime.GOOS == "windows" {
		return Interpreter{Path: "cmd.exe", Args: []string{"/C"}, Name: "cmd.exe"}
	}
	return Interpreter{Path: "/bin/sh", Args: []string{"-lc"}, Name: "sh"}
}

var (
	bashOnce sync.Once
	bashFor  Interpreter
)

// Bash is the interpreter of the agent's bash tool (ADR-0019 §0), resolved once per process. On Unix
// it is /bin/bash, else bash on PATH, run as a login shell (-lc) like System so the same profile PATH
// applies; without bash it is System, named "sh", and the tool says so. On Windows it is Git Bash
// (Program Files), else a bash.exe on PATH other than the WSL launcher, else cmd.exe /C, named as such.
func Bash() Interpreter {
	bashOnce.Do(func() {
		bashFor = resolveBash(runtime.GOOS, os.Getenv, fileExists, exec.LookPath)
	})
	return bashFor
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// wslLauncher matches the WSL bash.exe in the Windows directory: it runs the command inside a Linux
// distribution with its own filesystem, not in the workspace as the host sees it.
var wslLauncher = regexp.MustCompile(`(?i)^[a-z]:\\windows\\(system32|sysnative)\\bash\.exe$`)

func resolveBash(goos string, getenv func(string) string, exists func(string) bool, lookPath func(string) (string, error)) Interpreter {
	if goos == "windows" {
		for _, root := range []string{getenv("ProgramFiles"), getenv("ProgramFiles(x86)")} {
			if root == "" {
				continue
			}
			if path := strings.TrimRight(root, `\`) + `\Git\bin\bash.exe`; exists(path) {
				return Interpreter{Path: path, Args: []string{"-lc"}, Name: "bash"}
			}
		}
		if path, err := lookPath("bash.exe"); err == nil && !isWSLLauncher(path, getenv("SystemRoot")) {
			return Interpreter{Path: path, Args: []string{"-lc"}, Name: "bash"}
		}
		return Interpreter{Path: "cmd.exe", Args: []string{"/C"}, Name: "cmd.exe"}
	}
	if exists("/bin/bash") {
		return Interpreter{Path: "/bin/bash", Args: []string{"-lc"}, Name: "bash"}
	}
	if path, err := lookPath("bash"); err == nil {
		return Interpreter{Path: path, Args: []string{"-lc"}, Name: "bash"}
	}
	return Interpreter{Path: "/bin/sh", Args: []string{"-lc"}, Name: "sh"}
}

func isWSLLauncher(path, systemRoot string) bool {
	lower := strings.ToLower(path)
	if root := strings.ToLower(strings.TrimRight(systemRoot, `\`)); root != "" {
		return lower == root+`\system32\bash.exe` || lower == root+`\sysnative\bash.exe`
	}
	return wslLauncher.MatchString(path)
}
