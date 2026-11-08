package evalapi

import (
	"testing"
	"time"
)

func TestBoundaryRejectsIncompleteStaleAndMismatchedProof(t *testing.T) {
	binding := BoundaryContext{CohortID: "cohort", JobID: "job", TrialID: "trial", ExecutionID: "execution", RequestID: "request"}
	complete := func() BoundaryReport {
		r := BoundaryReport{SchemaVersion: BoundarySchemaVersion, Context: binding, CheckedAt: time.Now(), Status: BoundaryPass, Process: BoundaryProcess{HostPID: 42, Starttime: 100, Generation: 1, UID: 1001, GID: 1001, Cgroup: "/owned/leaf"}, Checks: make(map[string]BoundaryCheck)}
		for _, name := range BoundaryRequiredChecks() {
			r.Checks[name] = BoundaryCheck{Status: BoundaryPass, Evidence: "observed"}
		}
		return r
	}
	for _, test := range []struct {
		name   string
		mutate func(*BoundaryReport)
	}{
		{"missing", func(r *BoundaryReport) { delete(r.Checks, "mounts") }},
		{"unknown", func(r *BoundaryReport) {
			r.Checks["identity"] = BoundaryCheck{Status: BoundaryUnknown, Evidence: "unreadable"}
		}},
		{"fail", func(r *BoundaryReport) {
			r.Checks["callerIsolation"] = BoundaryCheck{Status: BoundaryFail, Evidence: "same UID"}
		}},
		{"no evidence", func(r *BoundaryReport) { r.Checks["sockets"] = BoundaryCheck{Status: BoundaryPass} }},
		{"stale", func(r *BoundaryReport) { r.CheckedAt = time.Now().Add(-time.Minute) }},
		{"future", func(r *BoundaryReport) { r.CheckedAt = time.Now().Add(time.Minute) }},
		{"wrong request", func(r *BoundaryReport) { r.Context.RequestID = "other" }},
		{"wrong trial", func(r *BoundaryReport) { r.Context.TrialID = "other" }},
		{"schema", func(r *BoundaryReport) { r.SchemaVersion++ }},
		{"missing process", func(r *BoundaryReport) { r.Process = BoundaryProcess{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := complete()
			test.mutate(&r)
			if err := r.RequirePass(binding); err == nil {
				t.Fatal("unsafe proof accepted")
			}
		})
	}
	r := complete()
	if err := r.RequirePass(binding); err != nil {
		t.Fatal(err)
	}
}
