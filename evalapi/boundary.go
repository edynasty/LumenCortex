package evalapi

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const BoundarySchemaVersion = 1
const (
	BoundaryPass    = "PASS"
	BoundaryFail    = "FAIL"
	BoundaryUnknown = "UNKNOWN"
)

// BoundaryContext is supplied by the trusted host, never by a cell or tool result.
// RequestID is bound at admission; the remaining IDs identify the authorized job.
type BoundaryContext struct {
	CohortID    string `json:"cohortId"`
	JobID       string `json:"jobId"`
	TrialID     string `json:"trialId"`
	ExecutionID string `json:"executionId"`
	RequestID   string `json:"requestId"`
}

func (c BoundaryContext) Validate() error {
	for _, field := range [...]struct{ name, value string }{{"cohortId", c.CohortID}, {"jobId", c.JobID}, {"trialId", c.TrialID}, {"executionId", c.ExecutionID}, {"requestId", c.RequestID}} {
		if strings.TrimSpace(field.value) == "" || len(field.value) > 180 || strings.ContainsAny(field.value, "\x00\r\n") {
			return fmt.Errorf("boundary: invalid %s", field.name)
		}
	}
	return nil
}

type BoundaryCheck struct {
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

type BoundaryProcess struct {
	HostPID    int               `json:"hostPid"`
	Starttime  uint64            `json:"starttime"`
	Generation uint64            `json:"generation"`
	Cgroup     string            `json:"cgroup"`
	UID        int               `json:"uid"`
	GID        int               `json:"gid"`
	Namespaces map[string]string `json:"namespaces"`
}

// BoundaryReport is observation, not a grant. Deserializing a PASS report never
// authorizes execution. Admission calls the trusted host collector afresh.
type BoundaryReport struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Context       BoundaryContext          `json:"context"`
	CheckedAt     time.Time                `json:"checkedAt"`
	Status        string                   `json:"status"`
	Process       BoundaryProcess          `json:"process"`
	Checks        map[string]BoundaryCheck `json:"checks"`
}

func BoundaryRequiredChecks() []string {
	return []string{"identity", "generation", "cgroup", "credentials", "user", "pid", "mnt", "net", "ipc", "uts", "mounts", "sockets", "networkPolicy", "bubblewrap", "apparmor", "callerIsolation"}
}

// Decide is fail closed: missing observations never become successful checks.
func (r *BoundaryReport) Decide() {
	r.Status = BoundaryPass
	if r.SchemaVersion != BoundarySchemaVersion || r.Context.Validate() != nil {
		r.Status = BoundaryUnknown
	}
	for _, name := range BoundaryRequiredChecks() {
		check, ok := r.Checks[name]
		if ok && check.Status == BoundaryFail {
			r.Status = BoundaryFail
			return
		}
		if !ok || check.Status != BoundaryPass || strings.TrimSpace(check.Evidence) == "" {
			r.Status = BoundaryUnknown
		}
	}
}
func (r BoundaryReport) RequirePass(want BoundaryContext) error {
	if r.Context != want {
		return errors.New("boundary: context mismatch")
	}
	if r.CheckedAt.IsZero() || time.Since(r.CheckedAt) > 5*time.Second || time.Until(r.CheckedAt) > time.Second {
		return errors.New("boundary: stale or future observation")
	}
	if r.Process.HostPID <= 0 || r.Process.Starttime == 0 || r.Process.Generation == 0 || r.Process.Cgroup == "" || r.Process.UID <= 0 || r.Process.GID < 0 {
		return errors.New("boundary: incomplete process identity")
	}
	r.Decide()
	if r.Status != BoundaryPass {
		return fmt.Errorf("boundary: admission refused: %s", r.Status)
	}
	return nil
}
