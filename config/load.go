package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Warning codes.
const (
	// WarnUntrustedOverride: a workspace entry was dropped because the workspace is not trusted.
	WarnUntrustedOverride = "untrusted_override"
	// WarnLegacyPath: a global file sits in a directory earlier builds used.
	WarnLegacyPath = "legacy_global_path"
	// WarnDefaultLimit: a model has no limit.context or limit.output.
	WarnDefaultLimit = "default_limit"
	// WarnCognitionProfile: <workspace>/.lumencortex/cognition.json exists. Earlier builds read
	// provider, category and health settings from it; they are ignored now.
	WarnCognitionProfile = "cognition_profile"
)

// Warning is a structured, non-fatal finding. Hosts decide how to show it: Desktop turns an
// untrusted-override warning into its trust prompt, the CLI prints Message.
type Warning struct {
	Code string `json:"code"`
	// Path is the file the warning is about: the workspace file for a dropped entry, the old
	// location for a legacy file.
	Path string `json:"path,omitempty"`
	// Key is the dropped entry, e.g. "providers.deepseek", "checks.go-test" or "model".
	Key string `json:"key,omitempty"`
	// MoveTo is where a legacy file belongs.
	MoveTo string `json:"moveTo,omitempty"`
	// TrustEntry is the canonical workspace path to add to trustedWorkspaces in TrustFile so the
	// dropped entries apply.
	TrustEntry string `json:"trustEntry,omitempty"`
	TrustFile  string `json:"trustFile,omitempty"`
	Message    string `json:"message"`
	// Err is the sentinel for the finding (ErrUntrustedOverride); errors.Is(w.Err, ...) matches it.
	Err error `json:"-"`
}

// Document is one configuration file as read from disk.
type Document struct {
	Path string
	// Digest identifies the bytes read; pass it to Save as expectedDigest. It is "" when the file
	// does not exist, so a Save with "" fails if someone created the file meanwhile.
	Digest string
	File   File
}

// Digest is the identity Save compares: "sha256:" and the hex SHA-256 of the file bytes.
func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ReadDocument reads and validates one configuration file. A missing file is an empty document
// with an empty digest. Only regular files are read, at most 1 MiB, through symlinks.
func ReadDocument(path string) (Document, error) {
	doc := Document{Path: path}
	raw, err := readBounded(path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return Document{}, fmt.Errorf("config %s: %w", path, err)
	}
	f, err := Parse(raw)
	if err != nil {
		return Document{}, fmt.Errorf("config %s: %w", path, err)
	}
	doc.Digest = Digest(raw)
	doc.File = f
	return doc, nil
}

func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFileBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxFileBytes)
	}
	return raw, nil
}

// Effective is the merged view hosts and the runtime use.
type Effective struct {
	// GlobalDir holds lumencortex.json, mcp.json and skills/.
	GlobalDir string
	// Workspace is the workspace directory after symlink resolution, "" when none was given.
	Workspace string
	// Trusted reports whether Workspace is listed in the global trustedWorkspaces.
	Trusted       bool
	Model         ModelRef
	SubagentModel ModelRef
	Failover      []ModelRef
	EffortDefault Effort
	Escalation    *Escalation
	Checks        map[string]Check
	// Warnings lists ignored workspace entries and legacy files, in a stable order.
	Warnings []Warning
}

// Config is a loaded configuration: the global and workspace documents and their merge.
type Config struct {
	global, workspace Document
	eff               Effective
	providers         map[string]Provider
}

// Load reads the global file from GlobalDir and, when workspace is not empty, the workspace file
// <workspace>/lumencortex.json, and merges them (§4.8). A missing file is an empty one. It fails
// with ErrInvalidConfig for a malformed or schema-violating file, a model reference outside the
// merged catalog, and ErrCredentialRedirect.
func Load(workspace string) (*Config, error) {
	globalDir, err := GlobalDir()
	if err != nil {
		return nil, fmt.Errorf("config: no global directory: %w", err)
	}
	return load(globalDir, workspace)
}

func load(globalDir, workspace string) (*Config, error) {
	c := &Config{}
	c.eff.GlobalDir = globalDir
	var err error
	if c.global, err = ReadDocument(filepath.Join(globalDir, FileName)); err != nil {
		return nil, err
	}
	if workspace != "" {
		if c.eff.Workspace, err = canonical(workspace); err != nil {
			return nil, fmt.Errorf("config: workspace %s: %w", workspace, err)
		}
		wsPath := filepath.Join(c.eff.Workspace, FileName)
		// The global file read as a workspace file (workspace == GlobalDir) is the same document.
		if !sameFile(wsPath, c.global.Path) {
			if c.workspace, err = ReadDocument(wsPath); err != nil {
				return nil, err
			}
		} else {
			c.workspace = Document{Path: wsPath}
		}
		c.eff.Trusted = isTrusted(c.eff.Workspace, c.global.File.TrustedWorkspaces)
	}
	if err := c.merge(); err != nil {
		return nil, err
	}
	c.eff.Warnings = append(c.eff.Warnings, LegacyWarnings(globalDir)...)
	if c.eff.Workspace != "" {
		old := filepath.Join(c.eff.Workspace, ".lumencortex", "cognition.json")
		if info, err := os.Stat(old); err == nil && info.Mode().IsRegular() {
			c.eff.Warnings = append(c.eff.Warnings, Warning{
				Code: WarnCognitionProfile, Path: old,
				Message: "found " + old + "; its provider, category and health settings are ignored. " +
					"Configure models in lumencortex.json",
			})
		}
	}
	return c, nil
}

func sameFile(a, b string) bool {
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

// isTrusted reports whether workspace (already canonical) is one of the listed directories.
// Entries are compared after symlink resolution, as the same directory, never as a prefix: a
// subdirectory of a trusted workspace is not trusted.
func isTrusted(workspace string, entries []string) bool {
	for _, e := range entries {
		if !filepath.IsAbs(e) {
			continue // another OS's absolute form (C:\...) never names a directory here
		}
		if sameDir(e, workspace) {
			return true
		}
	}
	return false
}

// Global and Workspace return the documents as read, for editing and Save. Workspace is the zero
// Document when Load was given no workspace.
func (c *Config) Global() Document    { return cloneDocument(c.global) }
func (c *Config) Workspace() Document { return cloneDocument(c.workspace) }

// Effective returns the merged view. The caller owns the copy.
func (c *Config) Effective() Effective {
	e := c.eff
	e.Failover = append([]ModelRef(nil), e.Failover...)
	if e.Escalation != nil {
		esc := *e.Escalation
		e.Escalation = &esc
	}
	e.Checks = cloneChecks(e.Checks)
	e.Warnings = append([]Warning(nil), e.Warnings...)
	return e
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
