package knowledge

import (
	"testing"
	"time"
)

func TestScopeVisibilityAndInheritance(t *testing.T) {
	project := Scope{Kind: ScopeProject, OwnerID: "owner", ProjectID: "project"}
	workspace := project
	workspace.Kind, workspace.WorkspaceID = ScopeWorkspace, "workspace"
	session := workspace
	session.Kind, session.SessionID = ScopeSession, "session"
	reader := session
	reader.Kind, reader.ReaderID = ScopeReader, "reader"
	source := session
	source.Kind, source.SourceID = ScopeSource, "source"
	r := Reader{OwnerID: "owner", ProjectID: "project", WorkspaceID: "workspace", SessionID: "session", ID: "reader", SourceIDs: []string{"source"}}
	for _, s := range []Scope{project, workspace, session, reader, source} {
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
		if !s.VisibleTo(r) {
			t.Errorf("own reader cannot see %s", s.Kind)
		}
		foreign := r
		foreign.ProjectID = "elsewhere"
		if s.VisibleTo(foreign) {
			t.Errorf("cross-project leak: %s", s.Kind)
		}
		foreign = r
		foreign.OwnerID = "stranger"
		if s.VisibleTo(foreign) {
			t.Errorf("cross-owner leak: %s", s.Kind)
		}
	}
	other := r
	other.ID = "other-reader"
	if reader.VisibleTo(other) {
		t.Fatal("reader identity was ignored")
	}
	other = r
	other.SourceIDs = nil
	if source.VisibleTo(other) {
		t.Fatal("source grant was ignored")
	}
	other = r
	other.SessionID = "another-session"
	if !project.VisibleTo(other) || !workspace.VisibleTo(other) || session.VisibleTo(other) {
		t.Fatal("cross-session sharing did not respect scope")
	}
	other = r
	other.WorkspaceID = "moved"
	if workspace.VisibleTo(other) {
		t.Fatal("workspace move silently inherited scope")
	}
	for _, child := range []Scope{workspace, session, reader, source} {
		if !project.Contains(child) || child.Contains(project) {
			t.Fatalf("scope widening: %+v", child)
		}
	}
	if !workspace.Contains(reader) || !session.Contains(source) || source.Contains(reader) || reader.Contains(source) {
		t.Fatal("private boundary inheritance")
	}
	global := Scope{Kind: ScopeGlobal, OwnerID: "owner"}
	if global.VisibleTo(r) || global.Contains(project) {
		t.Fatal("implicit global grant")
	}
	r.GlobalAccess = true
	if !global.VisibleTo(r) {
		t.Fatal("explicit owner global grant rejected")
	}
	for _, s := range []Scope{{Kind: ScopeProject}, {Kind: ScopeWorkspace, OwnerID: "owner", ProjectID: "project"}, {Kind: ScopeGlobal, OwnerID: "owner", ProjectID: "project"}, {Kind: "new", OwnerID: "owner", ProjectID: "project"}} {
		if s.Validate() == nil || s.VisibleTo(r) {
			t.Fatalf("malformed scope accepted: %+v", s)
		}
	}
}

func TestTimeValidityAndFreshnessBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	base := Times{RecordedAt: now.Add(-time.Hour), ObservedAt: now.Add(-time.Minute), VerifiedAt: now, Valid: ValidTime{From: now, Until: now.Add(time.Hour)}}
	if got := base.TimeState(now); got != "" {
		t.Fatal(got)
	}
	cases := []struct {
		name   string
		change func(*Times)
		want   string
	}{
		{"future observation", func(v *Times) { v.ObservedAt = now.Add(time.Second) }, "future_observation"},
		{"future verification", func(v *Times) { v.VerifiedAt = now.Add(time.Second) }, "future_observation"},
		{"future valid", func(v *Times) { v.Valid.From = now.Add(time.Minute) }, "not_yet_valid"},
		{"expired", func(v *Times) { v.Valid.Until = now; v.Valid.From = now.Add(-time.Hour) }, "expired"},
		{"invalidated", func(v *Times) { v.InvalidatedAt = now }, "invalidated"},
		{"invalid interval", func(v *Times) { v.Valid.Until = v.Valid.From }, "invalid_time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := base
			tc.change(&value)
			if got := value.TimeState(now); got != tc.want {
				t.Fatalf("%s != %s", got, tc.want)
			}
		})
	}
	if !FreshAt(now.Add(-time.Second), now, time.Minute) || FreshAt(now.Add(time.Second), now, time.Minute) || FreshAt(now.Add(-time.Minute), now, time.Minute) || FreshAt(time.Time{}, now, time.Minute) {
		t.Fatal("invalid freshness window")
	}
	a, b := ValidTime{From: now, Until: now.Add(time.Hour)}, ValidTime{From: now.Add(time.Hour)}
	if a.Overlaps(b) || b.Overlaps(a) {
		t.Fatal("adjacent time intervals conflict")
	}
	if !a.Overlaps(ValidTime{}) || !a.Overlaps(ValidTime{From: now.Add(time.Minute)}) {
		t.Fatal("overlap missed")
	}
	zone := now.In(time.FixedZone("other", -5*60*60))
	if base.TimeState(zone) != "" {
		t.Fatal("equivalent instant changed validity")
	}
	if base.RecordedAt != now.Add(-time.Hour) {
		t.Fatal("read rewrote recorded history")
	}
}

func TestEvidenceAuthorityAndUseTime(t *testing.T) {
	now := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	scope := Scope{Kind: ScopeProject, OwnerID: "owner", ProjectID: "project"}
	e := Evidence{ID: "ev", Source: SourceRef{Kind: KindSession, ID: "origin", Digest: Digest("source"), Scope: "project", SessionID: "origin", Sensitivity: SensitivitySession, Provenance: ProvenanceRuntime}, Type: "check", Provenance: ProvenanceRuntime, Trust: "framework", Verification: "passed", Scope: scope, Times: Times{RecordedAt: now, VerifiedAt: now}, Freshness: FreshnessRun, Fingerprint: "fp", BindingDigest: Digest("binding"), SpecDigest: Digest("spec"), ApprovalEpoch: 2, SessionID: "reader", RunID: "run", ResultSeq: 1, SupportingClaims: []string{Digest("claim")}}
	c := EvidenceContext{Reader: Reader{OwnerID: "owner", ProjectID: "project", SessionID: "reader"}, At: now, SourceVersion: e.Source.Digest, BindingActive: true, Fingerprint: "fp", RunID: "run", InRun: true}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Applicability(c); got != Current {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		name   string
		change func(*EvidenceContext)
		want   Applicability
	}{
		{"foreign reader run", func(c *EvidenceContext) { c.RunID = "other" }, Unknown},
		{"wrong owner", func(c *EvidenceContext) { c.Reader.OwnerID = "other" }, Withdrawn},
		{"source changed", func(c *EvidenceContext) { c.SourceVersion = Digest("changed") }, Stale},
		{"source missing", func(c *EvidenceContext) { c.SourceMissing = true }, SourceMissing},
		{"withdrawn wins", func(c *EvidenceContext) { c.SourceWithdrawn = true; c.SourceMissing = true }, Withdrawn},
		{"revoked binding", func(c *EvidenceContext) { c.BindingActive = false }, CandidateOnly},
		{"side effect", func(c *EvidenceContext) { c.InRun = false }, Unknown},
		{"window expired", func(c *EvidenceContext) { c.At = now.Add(RunReuseWindow) }, Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := c
			tc.change(&current)
			if got, why := e.Applicability(current); got != tc.want {
				t.Fatalf("%s (%s), want %s", got, why, tc.want)
			}
		})
	}
	fake := e
	fake.Type, fake.Trust = "model", "model"
	if fake.Validate() == nil {
		t.Fatal("model declared verified evidence")
	}
	fake = e
	fake.Source.Sensitivity = SensitivityPrivate
	if fake.Validate() == nil {
		t.Fatal("private source widened to project")
	}
	fake = e
	fake.Times.InvalidatedAt = now
	if got, _ := fake.Applicability(c); got != Stale {
		t.Fatal("invalidated observation revived")
	}
}

func TestClaimsConflictRespectsScopeAndValidTime(t *testing.T) {
	a := Assertion{Kind: AssertPortValue, Target: "service", Value: "8080"}
	b := a
	b.Value = "9090"
	scope := Scope{Kind: ScopeProject, OwnerID: "owner", ProjectID: "project"}
	at := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	before, after := ValidTime{Until: at}, ValidTime{From: at}
	if ClaimsConflict(a, b, scope, scope, before, after) {
		t.Fatal("nonoverlapping facts conflict")
	}
	if !ClaimsConflict(a, b, scope, scope, ValidTime{}, after) {
		t.Fatal("same-domain contradiction missed")
	}
	other := scope
	other.ProjectID = "other"
	if ClaimsConflict(a, b, scope, other, ValidTime{}, ValidTime{}) || ClaimsConflict(a, a, scope, scope, ValidTime{}, ValidTime{}) {
		t.Fatal("independent or equal claims conflict")
	}
}
