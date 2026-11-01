package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The detection walk that decides whether a workspace holds a language is bounded: a workspace that
// is not a source tree must not make every run start pay for a full scan.
const (
	maxDetectEntries = 4000
	maxDetectDepth   = 6
)

// DefaultServers is the built-in catalog: the language servers LumenCortex knows how to start.
// Nothing is bundled and nothing is installed. A server runs only when its command is on PATH and
// the workspace holds a file or a marker of its language, so a workspace without a server gets no
// lsp_* tools and no process.
func DefaultServers() []LanguageServer {
	return []LanguageServer{
		{Language: "go", Name: "gopls", Command: "gopls",
			Extensions: []string{".go"}, Markers: []string{"go.mod", "go.work"}},
		{Language: "typescript", Name: "typescript-language-server", Command: "typescript-language-server",
			Args:       []string{"--stdio"},
			Extensions: []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"},
			Markers:    []string{"tsconfig.json", "jsconfig.json", "package.json"}},
		{Language: "python", Name: "pyright", Command: "pyright-langserver",
			Args:       []string{"--stdio"},
			Extensions: []string{".py", ".pyi"},
			Markers:    []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"}},
		{Language: "rust", Name: "rust-analyzer", Command: "rust-analyzer",
			Extensions: []string{".rs"}, Markers: []string{"Cargo.toml"}},
	}
}

// normalizeServer fills the language, name and the default extensions of a catalog entry: a
// configuration entry that names a built-in language and no extensions or markers keeps that
// language's.
func normalizeServer(def LanguageServer) LanguageServer {
	def.Language = strings.ToLower(strings.TrimSpace(def.Language))
	def.Command = strings.TrimSpace(def.Command)
	def.Name = strings.TrimSpace(def.Name)
	if def.Name == "" {
		def.Name = filepath.Base(def.Command)
	}
	extensions := make([]string, 0, len(def.Extensions))
	for _, ext := range def.Extensions {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		extensions = append(extensions, ext)
	}
	def.Extensions = extensions
	// A configured entry that names a built-in language and leaves the files out keeps that
	// language's extensions and markers: changing the command must not change what the server serves.
	for _, builtin := range DefaultServers() {
		if builtin.Language != def.Language {
			continue
		}
		if len(def.Extensions) == 0 {
			def.Extensions = builtin.Extensions
		}
		if len(def.Markers) == 0 {
			def.Markers = builtin.Markers
		}
		break
	}
	return def
}

// resolveCommand returns the executable a server command names, "" when it is not there. An absolute
// path is checked directly; a bare name is looked up on PATH.
func resolveCommand(command string) string {
	if filepath.IsAbs(command) {
		info, err := os.Stat(command)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return ""
		}
		return command
	}
	found, err := exec.LookPath(command)
	if err != nil {
		return ""
	}
	return found
}

// detectLanguages reports, for every server, whether the workspace holds a marker of its language or
// a file with one of its extensions. One bounded walk of the tree answers all of them, and nothing is
// started.
func detectLanguages(root string, servers []LanguageServer) map[string]bool {
	out := make(map[string]bool, len(servers))
	marker := make(map[string]bool, len(servers))
	needsWalk := false
	for _, def := range servers {
		for _, name := range def.Markers {
			if info, err := os.Stat(filepath.Join(root, name)); err == nil && !info.IsDir() {
				out[def.Language] = true
				marker[def.Language] = true
				break
			}
		}
		if len(def.Extensions) > 0 && !out[def.Language] {
			needsWalk = true
		}
	}
	if !needsWalk {
		return out
	}
	seen := map[string]bool{}
	entries := 0
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if entries >= maxDetectEntries {
			return
		}
		items, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, item := range items {
			if entries >= maxDetectEntries {
				return
			}
			entries++
			name := item.Name()
			if item.IsDir() {
				if depth >= maxDetectDepth || skipDetectDir(name) {
					continue
				}
				walk(filepath.Join(dir, name), depth+1)
				continue
			}
			seen[strings.ToLower(filepath.Ext(name))] = true
		}
	}
	walk(root, 1)
	for _, def := range servers {
		if marker[def.Language] {
			continue
		}
		for _, ext := range def.Extensions {
			if seen[ext] {
				out[def.Language] = true
				break
			}
		}
	}
	return out
}

// skipDetectDir names the directories detection does not descend into: hidden ones (.git, .venv) and
// build output, which holds no file the model would ask about.
func skipDetectDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "target", "dist", "build", "out", "bin", "obj":
		return true
	}
	return false
}
