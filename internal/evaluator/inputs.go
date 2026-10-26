package evaluator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Input is a workspace file whose content is part of a check's definition (CHECKS-002): the schema of a
// schema check and the schema files it references, or a file a test or build command executes, such as
// the script in `sh scripts/check.sh` or `./check.sh`. Its digest is in the definition's digest, so an
// approval covers the content the check runs and not only the command that runs it. Path is
// workspace-relative with forward slashes; Digest is "sha256:<hex>" of the content, or AbsentInput.
type Input struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// AbsentInput is the digest of an input that is not a readable regular file inside the workspace. An
// input absent at approval is bound as absent: creating it later is a change like any other.
const AbsentInput = "absent"

// token is one lexed piece of a command: a word, the end of a simple command, or a redirection.
type token struct {
	kind   int
	text   string
	expand bool // the word has an unquoted or double-quoted $, an unquoted glob or a leading ~
}

const (
	tokWord = iota
	tokEnd
	tokRedirect
)

// lex splits a command as a POSIX shell would into words, command ends (; & && | || ( ) newline `
// and $( ), and redirections (<, >, >>, <<, &>, >&, with an fd number before them). Quotes and
// backslashes are removed from words. It does not expand anything.
func lex(command string) []token {
	var (
		out    []token
		word   strings.Builder
		inWord bool
		expand bool
	)
	flush := func() {
		if inWord {
			out = append(out, token{kind: tokWord, text: word.String(), expand: expand})
		}
		word.Reset()
		inWord, expand = false, false
	}
	rs := []rune(command)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs):
			i++
			word.WriteRune(rs[i])
			inWord = true
		case r == '\'':
			inWord = true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				word.WriteRune(rs[i])
			}
		case r == '"':
			inWord = true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				} else if rs[i] == '$' || rs[i] == '`' {
					expand = true
				}
				word.WriteRune(rs[i])
			}
		case r == ' ' || r == '\t' || r == '\r':
			flush()
		case r == '<' || r == '>' || (r == '&' && i+1 < len(rs) && rs[i+1] == '>'):
			// An fd number written right before the operator (2>) belongs to it.
			if inWord && !expand && strings.Trim(word.String(), "0123456789") == "" {
				word.Reset()
				inWord = false
			}
			flush()
			op := string(r)
			for i+1 < len(rs) && (rs[i+1] == '<' || rs[i+1] == '>' || rs[i+1] == '&') {
				i++
				op += string(rs[i])
			}
			out = append(out, token{kind: tokRedirect, text: op})
		case r == ';' || r == '&' || r == '|' || r == '(' || r == ')' || r == '\n' || r == '`':
			flush()
			out = append(out, token{kind: tokEnd})
		case r == '$' && i+1 < len(rs) && rs[i+1] == '(':
			flush()
			i++
			out = append(out, token{kind: tokEnd})
		default:
			if r == '$' || r == '*' || r == '?' || r == '[' || (r == '~' && !inWord) {
				expand = true
			}
			word.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return out
}

// Interpreters run the file their first operand names; shells and python take a command string after
// -c instead, which is read as commands (shells) or ends the search (python: the string is not shell).
var (
	shells       = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true, "mksh": true}
	interpreters = map[string]bool{
		"node": true, "ruby": true, "perl": true, "php": true, "lua": true, "Rscript": true, "deno": true, "bun": true,
		"tsx": true, "ts-node": true, "pwsh": true, "fish": true,
	}
	// inline marks the options of the other interpreters whose argument is code, not a file.
	inline = map[string]bool{"-e": true, "--eval": true, "-p": true, "--print": true, "-c": true, "--command": true, "-E": true}
	// keywords leave the next word in program position.
	keywords = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "while": true, "until": true, "do": true, "!": true, "{": true, "}": true}
	// wrappers run the program that follows their options; the value names their options that take an
	// argument, and operand is true when one operand (timeout's duration) comes before the program.
	wrappers = map[string]struct {
		argOptions map[string]bool
		operand    bool
	}{
		"env":     {argOptions: map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true}},
		"exec":    {argOptions: map[string]bool{"-a": true}},
		"command": {},
		"time":    {},
		"nohup":   {},
		"nice":    {argOptions: map[string]bool{"-n": true, "--adjustment": true}},
		"timeout": {argOptions: map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true}, operand: true},
		"stdbuf":  {},
		"xargs":   {argOptions: map[string]bool{"-I": true, "-n": true, "-P": true, "-L": true, "-d": true, "-E": true, "-s": true}},
	}
)

func isPython(base string) bool { return strings.HasPrefix(base, "python") }

// commandPaths returns the words of command that name a file it executes, in order: the program of a
// simple command when it is a path (it contains a slash: ./check.sh, bin/x; a bare name is looked up
// in PATH), and the script operand of `.`, `source`, a shell, python or another interpreter, after
// their options and the arguments of those options (bash -euo pipefail x.sh, python3 -W error x.py).
// Wrappers (env, nice -n 5, timeout 30, exec, …), variable assignments and keywords are skipped
// before the program. A shell's -c string is read as commands. The other words of a simple command
// are its arguments: data the program reads, not its definition (`grep -q ok status.txt` has none).
// Redirection targets are skipped. A word with shell expansion is not a path.
func commandPaths(command string) []string {
	var out []string
	toks := lex(command)
	for i := 0; i < len(toks); {
		i = simpleCommand(toks, i, &out)
	}
	return out
}

// simpleCommand reads one simple command from toks[i:], appends its paths, and returns the index
// after its end token.
func simpleCommand(toks []token, i int, out *[]string) int {
	next := func() (token, bool) {
		for i < len(toks) {
			t := toks[i]
			i++
			switch t.kind {
			case tokEnd:
				i--
				return token{}, false
			case tokRedirect:
				if i < len(toks) && toks[i].kind == tokWord {
					i++ // the redirection target
				}
				continue
			}
			return t, true
		}
		return token{}, false
	}
	add := func(t token) {
		if !t.expand && t.text != "" && !strings.HasPrefix(t.text, "-") {
			*out = append(*out, t.text)
		}
	}
	finish := func() int {
		for i < len(toks) && toks[i].kind != tokEnd {
			i++
		}
		return i + 1
	}
	// stdin is the file a `<` redirection of this simple command reads: a shell or python with no
	// script operand runs it.
	var stdin *token
	for j := i; j < len(toks) && toks[j].kind != tokEnd; j++ {
		if toks[j].kind == tokRedirect && toks[j].text == "<" && j+1 < len(toks) && toks[j+1].kind == tokWord {
			stdin = &toks[j+1]
		}
	}
	// Program position.
	var prog token
	for {
		t, ok := next()
		if !ok {
			return finish()
		}
		if keywords[t.text] || (!t.expand && strings.Contains(t.text, "=") && !strings.HasPrefix(t.text, "=") && !strings.HasPrefix(t.text, "-")) {
			continue
		}
		if w, isWrapper := wrappers[path.Base(t.text)]; isWrapper {
			operand := w.operand
			for {
				o, ok := next()
				if !ok {
					return finish()
				}
				if strings.HasPrefix(o.text, "-") {
					if w.argOptions[o.text] {
						next()
					}
					continue
				}
				if strings.Contains(o.text, "=") && path.Base(t.text) == "env" {
					continue
				}
				if operand {
					operand = false
					continue
				}
				i-- // o is the program
				break
			}
			continue
		}
		prog = t
		break
	}
	base := path.Base(prog.text)
	switch {
	case prog.text == "." || prog.text == "source":
		if t, ok := next(); ok {
			add(t)
		}
	case shells[base]:
		script := false
		for {
			t, ok := next()
			if !ok {
				break
			}
			if t.text == "--" {
				if t, ok := next(); ok {
					add(t)
					script = true
				}
				break
			}
			if t.text == "--rcfile" || t.text == "--init-file" {
				next()
				continue
			}
			if strings.HasPrefix(t.text, "--") {
				continue
			}
			if len(t.text) > 1 && (t.text[0] == '-' || t.text[0] == '+') {
				letters := t.text[1:]
				if strings.ContainsRune(letters, 'c') {
					if s, ok := next(); ok {
						*out = append(*out, commandPaths(s.text)...)
					}
					script = true
					break
				}
				if strings.ContainsRune(letters, 's') {
					break // the script is read from stdin
				}
				if last := letters[len(letters)-1]; last == 'o' || last == 'O' {
					next()
				}
				continue
			}
			add(t)
			script = true
			break
		}
		if !script && stdin != nil {
			add(*stdin)
		}
	case isPython(base):
		script := false
		for {
			t, ok := next()
			if !ok || t.text == "-c" || t.text == "-m" {
				script = ok
				break
			}
			if t.text == "-" {
				break
			}
			if t.text == "-W" || t.text == "-X" {
				next()
				continue
			}
			if strings.HasPrefix(t.text, "-") {
				continue
			}
			add(t)
			script = true
			break
		}
		if !script && stdin != nil {
			add(*stdin)
		}
	case interpreters[base]:
		for {
			t, ok := next()
			if !ok || inline[t.text] {
				break
			}
			if strings.HasPrefix(t.text, "-") || ((base == "deno" || base == "bun") && (t.text == "run" || t.text == "test")) {
				continue
			}
			add(t)
			break
		}
	default:
		if strings.Contains(prog.text, "/") {
			add(prog)
		}
	}
	return finish()
}

// ResolveInputs sets the Inputs of each spec from workspace (the configuration scope the definitions
// are read and approved in): for a schema check its schema and the schema files it references by a
// relative $ref, for a test or build check the files its command executes (commandPaths), each taken
// relative to the workspace root (an absolute path only when it lies inside). A candidate that is not
// a regular file inside the workspace is bound as AbsentInput. Files git ignores in workspace are not
// inputs: they are generated, like a binary the check builds and then runs. The content of a schema
// check's Files is what it validates, not part of its definition.
func ResolveInputs(workspace string, specs []Spec) []Spec {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		root = workspace
	}
	type pending struct {
		spec  int
		paths []string
	}
	var (
		all   []pending
		names []string
	)
	for i := range specs {
		var candidates []string
		switch specs[i].Kind {
		case KindSchema:
			candidates = schemaFiles(root, specs[i].Schema)
		case KindTest, KindBuild:
			candidates = commandPaths(specs[i].Command)
		}
		seen := map[string]bool{}
		p := pending{spec: i}
		for _, c := range candidates {
			if rel, ok := relativeTo(root, c); ok && !seen[rel] {
				seen[rel] = true
				p.paths = append(p.paths, rel)
				names = append(names, rel)
			}
		}
		all = append(all, p)
	}
	ignored := ignoredPaths(root, names)
	for _, p := range all {
		var inputs []Input
		for _, rel := range p.paths {
			if !ignored[rel] {
				inputs = append(inputs, Input{Path: rel, Digest: fileDigest(root, rel)})
			}
		}
		slices.SortFunc(inputs, func(a, b Input) int { return strings.Compare(a.Path, b.Path) })
		specs[p.spec].Inputs = inputs
	}
	return specs
}

// schemaFiles is the schema file and, transitively, the files it references by a relative $ref
// (a URI without a scheme; the fragment is dropped), at most 64 of them.
func schemaFiles(root, schema string) []string {
	rel, ok := relativeTo(root, schema)
	if !ok {
		return []string{schema}
	}
	out := []string{rel}
	for i := 0; i < len(out) && len(out) < 64; i++ {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(out[i])))
		if err != nil {
			continue
		}
		var doc any
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		for _, ref := range schemaRefs(doc, nil) {
			if j := strings.IndexByte(ref, '#'); j >= 0 {
				ref = ref[:j]
			}
			if ref == "" || strings.Contains(ref, ":") || strings.HasPrefix(ref, "/") {
				continue
			}
			if r, ok := relativeTo(root, path.Join(path.Dir(out[i]), ref)); ok && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	return out
}

func schemaRefs(v any, out []string) []string {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if s, ok := x.(string); ok && k == "$ref" {
				out = append(out, s)
			} else {
				out = schemaRefs(x, out)
			}
		}
	case []any:
		for _, x := range v {
			out = schemaRefs(x, out)
		}
	}
	return out
}

// ignoredPaths reports which of rels git ignores in root (one git process; tracked files are never
// ignored). Outside git, or when git fails, nothing is ignored.
func ignoredPaths(root string, rels []string) map[string]bool {
	out := map[string]bool{}
	if len(rels) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "--stdin", "-z")
	cmd.Stdin = strings.NewReader(strings.Join(rels, "\x00") + "\x00")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// Exit 1 is "nothing ignored"; any other failure leaves every candidate an input.
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			return out
		}
	}
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			out[p] = true
		}
	}
	return out
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
