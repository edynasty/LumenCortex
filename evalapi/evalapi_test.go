package evalapi

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestBudgetMergeAndDefaults(t *testing.T) {
	base := Budget{WallSeconds: 30, OutputBytes: 1024, MaxCallbacks: 4}
	override := Budget{WallSeconds: 5, ArtifactBytes: 2048}
	merged := MergeBudget(base, override)
	if merged.WallSeconds != 5 || merged.OutputBytes != 1024 || merged.ArtifactBytes != 2048 || merged.MaxCallbacks != 4 {
		t.Fatalf("merged = %+v", merged)
	}
	// Zero inherits; a negative field is copied as "unlimited".
	unlimited := MergeBudget(base, Budget{MaxCallbacks: -1})
	if unlimited.MaxCallbacks != -1 {
		t.Fatalf("negative override = %d", unlimited.MaxCallbacks)
	}
	filled := Budget{}.WithDefaults(DefaultTestBudget())
	if filled.WallSeconds <= 0 || filled.OutputBytes <= 0 || filled.ArtifactBytes <= 0 || filled.DiskBytes <= 0 {
		t.Fatalf("defaults = %+v", filled)
	}
	if filled.MaxWorkers != 2 {
		t.Fatalf("default MaxWorkers = %d, want 2 for mixed-language hosts", filled.MaxWorkers)
	}
}

func DefaultTestBudget() Budget {
	return Budget{WallSeconds: 300, MemoryBytes: 1 << 30, OutputBytes: 256 << 10, ArtifactBytes: 8 << 20, DiskBytes: 64 << 20, MaxWorkers: 2, MaxCallbacks: 64, MaxDepth: 2}
}

func TestRoles(t *testing.T) {
	for _, role := range []string{RoleOwner, RoleExecutor, RoleObserver} {
		if !ValidRole(role) {
			t.Fatalf("%s is not a valid role", role)
		}
	}
	if ValidRole("ADMIN") || ValidRole("") {
		t.Fatalf("invalid role accepted")
	}
	if RoleRank(RoleOwner) <= RoleRank(RoleExecutor) || RoleRank(RoleExecutor) <= RoleRank(RoleObserver) {
		t.Fatalf("role rank is not ordered")
	}
	if RoleRank("ADMIN") != 0 {
		t.Fatalf("unknown role rank = %d", RoleRank("ADMIN"))
	}
}

func TestManifestDeclarations(t *testing.T) {
	manifest := Manifest{
		ID:         "python",
		SPIVersion: SPIVersion,
		Language:   "python",
		Platforms:  []string{"darwin", "linux"},
		Capabilities: []Capability{
			{Name: CapPersistent, Supported: true},
			{Name: CapInterrupt, Supported: false},
		},
		Limits: Limits{WallSeconds: 60, OutputBytes: 1024, MemoryEnforcement: EnforcementMonitored},
	}
	if !manifest.Supports(CapPersistent) {
		t.Fatalf("persistent not supported")
	}
	if manifest.Supports(CapInterrupt) {
		t.Fatalf("unsupported capability reported as supported")
	}
	if !manifest.SupportsPlatform("darwin") || manifest.SupportsPlatform("windows") {
		t.Fatalf("platform support is wrong")
	}
	names := manifest.CapabilityNames()
	if len(names) != 1 || names[0] != CapPersistent {
		t.Fatalf("capability names = %v", names)
	}
	budget := manifest.Budget()
	if budget.WallSeconds != 60 || budget.OutputBytes != 1024 {
		t.Fatalf("manifest budget = %+v", budget)
	}
	if (Manifest{}).SupportsPlatform("plan9") != true {
		t.Fatalf("an empty platform list must not restrict")
	}
}

func TestRequestCapabilities(t *testing.T) {
	request := Request{AllowedCapabilities: []string{"read_file", "list_dir"}}
	if !request.AllowsCapability("read_file") || request.AllowsCapability("bash") {
		t.Fatalf("capability filter is wrong")
	}
	if request.AllowsAllCapabilities() {
		t.Fatalf("a named list is not a wildcard")
	}
	all := Request{AllowedCapabilities: []string{"*"}}
	if !all.AllowsAllCapabilities() || !all.AllowsCapability("anything") {
		t.Fatalf("wildcard is wrong")
	}
	none := Request{}
	if none.AllowsCapability("read_file") || none.AllowsAllCapabilities() {
		t.Fatalf("an empty list must allow nothing")
	}
}

func TestResultClassification(t *testing.T) {
	if !(Result{Status: ResultStatusCompleted}).Succeeded() {
		t.Fatalf("completed must succeed")
	}
	if (Result{Status: ResultStatusFailed}).Succeeded() {
		t.Fatalf("failed must not succeed")
	}
	for _, status := range []string{ResultStatusTimeout, ResultStatusCancelled, ResultStatusCrashed} {
		if !(Result{Status: status}).Interrupted() {
			t.Fatalf("%s must be interrupted", status)
		}
	}
	if !(Result{Status: ResultStatusFailed, StateStatus: StateStatusLost}).Interrupted() {
		t.Fatalf("a lost kernel must be interrupted")
	}
	if (Result{Status: ResultStatusCompleted, StateStatus: StateStatusOK}).Interrupted() {
		t.Fatalf("a completed cell is not interrupted")
	}
	if StateStatusOK != "ok" {
		t.Fatalf("StateStatusOK = %q", StateStatusOK)
	}
}

func TestWireJSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	result := Result{
		RequestID: "r", SessionID: "s", CellID: "c", Status: ResultStatusCompleted,
		Stdout: "out", StateStatus: StateStatusOK, Generation: 3,
		Displays:  []Display{{MIME: "text/plain", Encoding: "utf-8", Text: "hi", Size: 2}},
		Artifacts: []Artifact{{ID: "a", SessionID: "s", CellID: "c", RuntimeID: "python", Name: "n", MIME: "text/plain", SHA256: "deadbeef", Size: 2, CreatedAt: now}},
		StartedAt: now, CompletedAt: now,
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Result
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.RequestID != result.RequestID || decoded.Generation != 3 || len(decoded.Displays) != 1 || len(decoded.Artifacts) != 1 {
		t.Fatalf("round trip = %+v", decoded)
	}
	if !decoded.StartedAt.Equal(now) {
		t.Fatalf("time did not survive JSON: %s != %s", decoded.StartedAt, now)
	}
	if decoded.Artifacts[0].SHA256 != "deadbeef" {
		t.Fatalf("artifact digest lost")
	}
	// Budget zero values serialize as zeros so a worker can inherit them.
	budgetData, _ := json.Marshal(Budget{})
	var budget Budget
	if err := json.Unmarshal(budgetData, &budget); err != nil {
		t.Fatalf("budget: %v", err)
	}
	if budget != (Budget{}) {
		t.Fatalf("zero budget = %+v", budget)
	}
}

func TestErrorsAreDistinct(t *testing.T) {
	sentinels := []error{ErrNotFound, ErrDenied, ErrClosed, ErrBusy, ErrExpired, ErrSandboxUnsupported, ErrUnsupportedCapability, ErrLimit, ErrInvalid, ErrWorker}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Fatalf("%v matches %v", a, b)
			}
		}
	}
}
