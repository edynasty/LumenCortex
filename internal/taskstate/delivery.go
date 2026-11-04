package taskstate

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// DeliveryContract is an explicit host contract, never inferred from an assistant reply.
// Baselines are captured once before execution and never refreshed on resume.
type DeliveryContract struct {
	Files          []DeliveryFile  `json:"files,omitempty"`
	Branch         string          `json:"branch,omitempty"`
	Commit         bool            `json:"commit,omitempty"`
	BaseHead       string          `json:"baseHead,omitempty"`
	BaselineIssues []DeliveryIssue `json:"baselineIssues,omitempty"`
}

type DeliveryFile struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed,omitempty"`
	// Absent requires deletion/absence instead of a regular file. Changed also requires a baseline delta.
	Absent   bool              `json:"absent,omitempty"`
	Baseline *DeliverySnapshot `json:"baseline,omitempty"`
	// BaseBlob is read-only historical evidence; it is not a native content digest.
	BaseBlob string `json:"baseBlob,omitempty"`
}

type DeliverySnapshot struct {
	Exists bool   `json:"exists"`
	Kind   string `json:"kind,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
}

const (
	DeliveryPending     = "pending"
	DeliveryVerified    = "verified"
	DeliveryFailed      = "failed"
	DeliveryUnavailable = "verification_unavailable"
)

type DeliveryIssue struct {
	Phase  string `json:"phase"`
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason"`
}

type DeliveryFileEvidence struct {
	Path     string            `json:"path"`
	Change   string            `json:"change"`
	Baseline *DeliverySnapshot `json:"baseline,omitempty"`
	Current  *DeliverySnapshot `json:"current,omitempty"`
}

// DeliveryResult is independent of execution status. Issues take precedence over missing requirements.
type DeliveryResult struct {
	Status  string                 `json:"status"`
	RunID   string                 `json:"runId,omitempty"`
	Files   []DeliveryFileEvidence `json:"files,omitempty"`
	Missing []string               `json:"missing,omitempty"`
	Issues  []DeliveryIssue        `json:"issues,omitempty"`
}

func (d DeliveryContract) Validate() error {
	if len(d.Files) > 32 {
		return errors.New("delivery supports at most 32 files")
	}
	if len(d.Files) == 0 && d.Branch == "" && !d.Commit {
		return errors.New("delivery requires files, branch or commit")
	}
	if d.Branch != "" && !validBranchName(d.Branch) {
		return fmt.Errorf("invalid delivery branch %q", d.Branch)
	}
	seen := map[string]bool{}
	for _, f := range d.Files {
		if f.Path == "" || len(f.Path) > 128 || strings.HasPrefix(f.Path, "-") || strings.Contains(f.Path, ":") || path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || f.Path == "." || f.Path == ".." || strings.HasPrefix(f.Path, "../") || strings.ContainsAny(f.Path, "\\\x00\r\n") || f.Path == ".git" || strings.HasPrefix(f.Path, ".git/") {
			return fmt.Errorf("invalid delivery file %q", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("duplicate delivery file %q", f.Path)
		}
		seen[f.Path] = true
	}
	return nil
}

// validBranchName applies `git check-ref-format --branch` rules in Go, so an impossible branch
// requirement is a host error at Start whether or not Git is installed, not a later verification outage.
func validBranchName(b string) bool {
	if len(b) > 256 || b == "HEAD" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") || strings.Contains(b, "..") ||
		strings.Contains(b, "//") || strings.Contains(b, "@{") || strings.ContainsAny(b, " ~^:?*[\\") {
		return false
	}
	for _, r := range b {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(b, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// SeedDelivery accepts the same host requirements on resume without replacing their baseline.
func (s *State) SeedDelivery(d *DeliveryContract) (bool, error) {
	if d == nil {
		return false, nil
	}
	if err := d.Validate(); err != nil {
		return false, err
	}
	if s.Delivery != nil {
		same := s.Delivery.Branch == d.Branch && s.Delivery.Commit == d.Commit && len(s.Delivery.Files) == len(d.Files)
		if same {
			for i, f := range d.Files {
				if f.Path != s.Delivery.Files[i].Path || f.Changed != s.Delivery.Files[i].Changed || f.Absent != s.Delivery.Files[i].Absent {
					same = false
					break
				}
			}
		}
		if !same {
			return false, errors.New("delivery requirements differ from the contract persisted in this session")
		}
		return false, nil
	}
	clone := *d
	clone.Files = slices.Clone(d.Files)
	// Ignore host-supplied evidence. Only the runtime can capture a new baseline.
	clone.BaseHead, clone.BaselineIssues = "", nil
	for i := range clone.Files {
		clone.Files[i].Baseline, clone.Files[i].BaseBlob = nil, ""
	}
	s.Delivery = &clone
	return true, nil
}
