package taskstate

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// DeliveryContract is an explicit host contract, never inferred from an assistant reply.
// BaseHead and BaseBlob are captured once before execution, not refreshed on resume.
// Configured required checks remain the evaluator's separate completion condition.
type DeliveryContract struct {
	Files    []DeliveryFile `json:"files,omitempty"`
	Branch   string         `json:"branch,omitempty"`
	Commit   bool           `json:"commit,omitempty"`
	BaseHead string         `json:"baseHead,omitempty"`
}

type DeliveryFile struct {
	Path     string `json:"path"`
	Changed  bool   `json:"changed,omitempty"`
	BaseBlob string `json:"baseBlob,omitempty"`
}

func (d DeliveryContract) Validate() error {
	if len(d.Files) > 32 {
		return errors.New("delivery supports at most 32 files")
	}
	if len(d.Files) == 0 && d.Branch == "" && !d.Commit {
		return errors.New("delivery requires files, branch or commit")
	}
	if len(d.Branch) > 256 || strings.HasPrefix(d.Branch, "-") || strings.ContainsAny(d.Branch, "\x00\r\n") {
		return errors.New("invalid delivery branch")
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
				if f.Path != s.Delivery.Files[i].Path || f.Changed != s.Delivery.Files[i].Changed {
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
	s.Delivery = &clone
	return true, nil
}
