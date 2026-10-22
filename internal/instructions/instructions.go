// Package instructions loads the instruction files a run's system prompt carries: the user's global
// AGENTS.md and the project's AGENTS.md, or its CLAUDE.md when it has none (ADR-0013).
//
// Both files are project inputs in one respect only: they are read from the workspace the Engine was
// opened on, never from a session worktree, so an agent cannot edit the instructions that govern it
// and have them apply to the same run. The global file belongs to the user and is always read. The
// project file is repository content: it is read only when the user trusts the workspace.
package instructions

import (
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/repository"
)

const (
	// FileName is the instruction file name in both scopes.
	FileName = "AGENTS.md"
	// FallbackFileName is the project instruction file read when the workspace has no AGENTS.md. One
	// file is read, never both: two sets of project instructions would be a second source of truth.
	FallbackFileName = "CLAUDE.md"
	// MaxFileBytes bounds one instruction file. A longer file is truncated rather than refused, so a
	// large document still contributes its beginning.
	MaxFileBytes = 64 << 10

	// KindGlobal and KindProject name the prompt layers these files become.
	KindGlobal  = "global-instructions"
	KindProject = "project-instructions"
	// ScopeGlobal and ScopeProject say who owns the file: the user, or the workspace.
	ScopeGlobal  = "global"
	ScopeProject = "project"
)

// Reasons a candidate file is not included. They are stable strings: the effective prompt reports
// them to the host.
const (
	// ReasonMissing: no candidate file exists.
	ReasonMissing = "missing"
	// ReasonUntrusted: the workspace is not trusted, so its instruction file is not read.
	ReasonUntrusted = "untrusted workspace"
	// ReasonOutsideWorkspace: the file is a symlink whose target resolves outside the workspace.
	ReasonOutsideWorkspace = "resolves outside the workspace"
	// ReasonNotRegular: the candidate exists but is not a regular file.
	ReasonNotRegular = "not a regular file"
	// ReasonUnreadable: the file could not be read.
	ReasonUnreadable = "unreadable"
)

// File is one instruction file as the prompt assembly sees it.
type File struct {
	// Kind is the prompt layer (KindGlobal, KindProject) and Scope who owns the file.
	Kind  string
	Scope string
	// Source is the absolute path that was found, "" when no candidate exists.
	Source string
	// Included reports whether Text enters the prompt.
	Included bool
	// Reason says why a found file is not included, or that none exists; "" for an included file.
	Reason string
	// Text is the file's content without any prompt label, possibly truncated.
	Text string
	// Bytes is len(Text): what this layer adds to the prompt.
	Bytes int
	// Truncated reports that the file is longer than MaxFileBytes.
	Truncated bool
	// Warning is the visible consequence of Truncated or of a refused file, "" when there is none.
	Warning string
}

// Load reads the global instruction file and the project instruction file, in prompt order.
//
// The global file is <globalDir>/AGENTS.md and is always read: it is the user's own configuration.
// The project file is <workspace>/AGENTS.md, or <workspace>/CLAUDE.md when the workspace has no
// AGENTS.md, and is read only when trusted. A project file that resolves outside the workspace
// through a symlink is refused, and so is one that is not a regular file. A missing or refused file
// is reported as a layer with Included false, never as an error: instructions are advisory, and a
// run must not fail because a workspace has an odd AGENTS.md.
func Load(globalDir, workspace string, trusted bool) []File {
	out := make([]File, 0, 2)
	out = append(out, loadGlobal(globalDir))
	if trusted {
		out = append(out, loadProject(workspace))
	} else {
		out = append(out, withheldProject(workspace))
	}
	return out
}

func loadGlobal(globalDir string) File {
	file := File{Kind: KindGlobal, Scope: ScopeGlobal}
	if globalDir == "" {
		file.Reason = ReasonMissing
		return file
	}
	abs := filepath.Join(globalDir, FileName)
	// The global directory is the user's own: a symlink out of it is the user's arrangement, not a
	// repository's attempt to reach outside. Only the file kind and size are checked here, so Stat
	// follows the link.
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		file.Reason = ReasonMissing
		if err == nil {
			file.Source, file.Reason = abs, ReasonNotRegular
		}
		return file
	}
	file.Source = abs
	read(&file, abs)
	return file
}

func loadProject(workspace string) File {
	file := File{Kind: KindProject, Scope: ScopeProject}
	if workspace == "" {
		file.Reason = ReasonMissing
		return file
	}
	for _, name := range []string{FileName, FallbackFileName} {
		abs := filepath.Join(workspace, name)
		if _, _, ok := repository.ConfinedFile(workspace, abs); !ok {
			if exists(abs) {
				// Found but refused: report the first candidate, not the fallback.
				file.Source, file.Reason = abs, refuseReason(abs)
				file.Warning = abs + " is not loaded: it is not a regular file inside the workspace."
				return file
			}
			continue
		}
		file.Source = abs
		read(&file, abs)
		return file
	}
	file.Reason = ReasonMissing
	return file
}

// withheldProject is the project layer of an untrusted workspace: it names the file that would be
// read, without reading it, so the effective prompt can say what is being withheld and why.
func withheldProject(workspace string) File {
	file := File{Kind: KindProject, Scope: ScopeProject, Reason: ReasonUntrusted}
	if workspace == "" {
		return file
	}
	for _, name := range []string{FileName, FallbackFileName} {
		if abs := filepath.Join(workspace, name); exists(abs) {
			file.Source = abs
			file.Warning = abs + " is not loaded: the workspace is not trusted. Trust it to include its instructions."
			return file
		}
	}
	return file
}

// refuseReason names why a candidate ConfinedFile refused is not used. The link itself is not a
// regular file, so Stat, which follows it, answers the file-kind question.
func refuseReason(abs string) string {
	info, err := os.Stat(abs)
	if err != nil {
		return ReasonUnreadable
	}
	if !info.Mode().IsRegular() {
		return ReasonNotRegular
	}
	return ReasonOutsideWorkspace
}

// read fills file from abs, truncating at MaxFileBytes. It reports a read failure as a refused file:
// the run continues without the instructions.
func read(file *File, abs string) {
	handle, err := os.Open(abs)
	if err != nil {
		file.Reason = ReasonUnreadable
		return
	}
	defer handle.Close()
	raw, err := io.ReadAll(io.LimitReader(handle, MaxFileBytes+1))
	if err != nil {
		file.Reason = ReasonUnreadable
		return
	}
	if len(raw) > MaxFileBytes {
		raw, file.Truncated = raw[:MaxFileBytes], true
	}
	// A cut in the middle of a rune would put invalid UTF-8 in the prompt.
	for i := 0; i < utf8.UTFMax && len(raw) > 0; i++ {
		if r, size := utf8.DecodeLastRune(raw); r != utf8.RuneError || size > 1 {
			break
		}
		raw = raw[:len(raw)-1]
	}
	file.Text = string(raw)
	file.Bytes = len(file.Text)
	file.Included = true
	if file.Truncated {
		file.Warning = abs + " is larger than 64 KiB and was truncated."
	}
}

func exists(abs string) bool {
	_, err := os.Lstat(abs)
	return err == nil
}
