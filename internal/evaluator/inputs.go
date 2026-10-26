package evaluator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Input is a workspace file whose content is part of a check's definition (CHECKS-002): the schema of a
// schema check, or a file a test or build command executes, such as the script in `sh scripts/check.sh`
// or `./check.sh`. Its digest is in the definition's digest, so an approval covers the content the
// check runs and not only the command that runs it. Path is workspace-relative with forward slashes;
// Digest is "sha256:<hex>" of the content.
type Input struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// AbsentInput is the digest of an input that is not a readable regular file inside the workspace.
const AbsentInput = "absent"

// commandSeparators split words; commandBoundaries also end a simple command, so the next word is a
// program again. Quotes split words too, so the command string of `sh -c '...'` is read as commands.
const (
	commandSeparators = " \t\r'\"<>"
	commandBoundaries = ";&|()\n`"
)

// interpreters run the file their first operand names.
var interpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "node": true, "ruby": true,
	"perl": true, "php": true, "lua": true, "Rscript": true, "deno": true, "bun": true, "tsx": true, "pwsh": true,
}

// transparent words leave the next word in program position.
var transparent = map[string]bool{
	"env": true, "exec": true, "command": true, "time": true, "nice": true, "nohup": true,
	"if": true, "then": true, "else": true, "elif": true, "while": true, "until": true, "do": true, "!": true, "{": true,
}

func isInterpreter(word string) bool {
	base := filepath.Base(word)
	return interpreters[base] || strings.HasPrefix(base, "python")
}

// commandPaths returns the words of command in the position of a file it executes: the program of
// each simple command (after variable assignments and words like env or time), and the first operand
// of an interpreter (sh scripts/a.sh, python3 tools/check.py). An interpreter's -c starts a command
// string, read as commands. The files a program reads as arguments are its data, not its definition:
// `grep -q ok status.txt` has no input. Words with shell expansion ($, globs, a leading ~) are skipped;
// whether a word names a workspace file is decided against a workspace (ResolveInputs).
func commandPaths(command string) []string {
	const (
		program = iota
		script
		args
	)
	var out []string
	state := program
	var word strings.Builder
	flush := func() {
		w := word.String()
		word.Reset()
		if w == "" {
			return
		}
		switch state {
		case program:
			if transparent[w] || (strings.Contains(w, "=") && !strings.HasPrefix(w, "=")) {
				return
			}
			out = appendPath(out, w)
			state = args
			if isInterpreter(w) {
				state = script
			}
		case script:
			switch {
			case w == "-c":
				state = program
			case strings.HasPrefix(w, "-"):
			default:
				out = appendPath(out, w)
				state = args
			}
		}
	}
	for _, r := range command {
		switch {
		case strings.ContainsRune(commandBoundaries, r):
			flush()
			state = program
		case strings.ContainsRune(commandSeparators, r):
			flush()
		default:
			word.WriteRune(r)
		}
	}
	flush()
	return out
}

func appendPath(out []string, w string) []string {
	if strings.HasPrefix(w, "-") || strings.HasPrefix(w, "~") || strings.ContainsAny(w, "$*?[{\\") {
		return out
	}
	return append(out, w)
}

// ResolveInputs sets the Inputs of each spec from workspace (the configuration scope the definitions
// are read and approved in): the schema file of a schema check; for a test or build check, each file
// its command executes (commandPaths) that is a regular file inside workspace, a path taken relative
// to the workspace root (an absolute one only when it lies inside).
// A word naming a directory, nothing, or a file outside the workspace is not an input. The content of
// a schema check's Files is what it validates, not part of its definition.
func ResolveInputs(workspace string, specs []Spec) []Spec {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		root = workspace
	}
	for i := range specs {
		var candidates []string
		switch specs[i].Kind {
		case KindSchema:
			candidates = []string{specs[i].Schema}
		case KindTest, KindBuild:
			candidates = commandPaths(specs[i].Command)
		}
		seen := map[string]bool{}
		var inputs []Input
		for _, c := range candidates {
			rel, ok := relativeTo(root, c)
			if !ok || seen[rel] {
				continue
			}
			digest := fileDigest(root, rel)
			if digest == AbsentInput && specs[i].Kind != KindSchema {
				continue
			}
			seen[rel] = true
			inputs = append(inputs, Input{Path: rel, Digest: digest})
		}
		slices.SortFunc(inputs, func(a, b Input) int { return strings.Compare(a.Path, b.Path) })
		specs[i].Inputs = inputs
	}
	return specs
}

// relativeTo cleans p to a forward-slash path relative to root, or reports that it leaves root.
func relativeTo(root, p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(root, p)
		if err != nil {
			return "", false
		}
		p = r
	}
	p = filepath.Clean(p)
	if p == "." || p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) || filepath.IsAbs(p) {
		return "", false
	}
	return filepath.ToSlash(p), true
}

// fileDigest is the content digest of rel under root, or AbsentInput when it is not a regular file
// that resolves inside root.
func fileDigest(root, rel string) string {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return AbsentInput
	}
	if r, err := filepath.Rel(root, resolved); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return AbsentInput
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return AbsentInput
	}
	f, err := os.Open(resolved)
	if err != nil {
		return AbsentInput
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return AbsentInput
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// changedInput names the first input of spec whose content in the evaluator's workspace (the execution
// scope: the session's worktree when it has one) is not the content the definition was read with, or
// "" when all match. A check whose input changed runs content nobody approved.
func (e *Evaluator) changedInput(spec Spec) string {
	if len(spec.Inputs) == 0 {
		return ""
	}
	root, err := filepath.EvalSymlinks(e.Workspace)
	if err != nil {
		root = e.Workspace
	}
	for _, in := range spec.Inputs {
		if fileDigest(root, in.Path) != in.Digest {
			return in.Path
		}
	}
	return ""
}

// inputTail explains a verdict refused because an input's content is not the approved one.
func inputTail(path string) string {
	return fmt.Sprintf("%s, which the check runs, is not the approved content: %s", path, ErrSpecUnapproved.Error())
}
