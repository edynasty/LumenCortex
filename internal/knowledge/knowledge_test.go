package knowledge

import (
	"errors"
	"strings"
	"testing"
)

func sessionRef(id string) SourceRef {
	return SourceRef{Kind: KindSession, ID: id, Digest: Digest(id), Scope: "workspace", SessionID: id,
		Sensitivity: SensitivitySession, Provenance: ProvenanceRuntime}
}

func TestSourceRefValidateRefusesWhatCoreCannotProduce(t *testing.T) {
	good := sessionRef("s1")
	if err := good.Validate(); err != nil {
		t.Fatalf("valid ref refused: %v", err)
	}
	cases := map[string]func(*SourceRef){
		"unauthorized kind (dap has no producer yet)": func(r *SourceRef) { r.Kind = "dap" },
		"forged digest":       func(r *SourceRef) { r.Digest = "sha256:abc" },
		"missing scope":       func(r *SourceRef) { r.Scope = "" },
		"missing session":     func(r *SourceRef) { r.SessionID = "" },
		"unknown sensitivity": func(r *SourceRef) { r.Sensitivity = "public" },
		"model provenance":    func(r *SourceRef) { r.Provenance = "llm:x" },
	}
	for name, mutate := range cases {
		r := good
		mutate(&r)
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDedupeKeyChangesWithVersionScopeAndNothingElse(t *testing.T) {
	a := sessionRef("s1")
	b := a
	b.RunID = "another run"
	if DedupeKey(a, "w") != DedupeKey(b, "w") {
		t.Fatal("the run id changed the dedupe key of the same source version")
	}
	c := a
	c.Digest = Digest("s1", "v2")
	if DedupeKey(a, "w") == DedupeKey(c, "w") || DedupeKey(a, "w") == DedupeKey(a, "other") {
		t.Fatal("a new source version or reader scope kept the dedupe key")
	}
}

func TestParseOutputIsStrictPerEntry(t *testing.T) {
	out := `{"candidates":[
	 {"claimType":"fact","subject":"s","claim":"c","rationale":"r","sourceRefs":["x"]},
	 {"claimType":"fact","subject":"s","claim":"c","rationale":"r","sourceRefs":["x"],"status":"qualified"},
	 {"claimType":"fact","subject":"s","claim":"c","rationale":"r","sourceRefs":["x"],"checkIds":["build"]}]}`
	parsed, err := ParseOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 3 || parsed[0].Err != nil || parsed[1].Err == nil || !strings.Contains(parsed[1].Err.Error(), "status") ||
		parsed[2].Err == nil || !strings.Contains(parsed[2].Err.Error(), "checkIds") {
		t.Fatalf("a self-assigned status or check was not rejected: %+v", parsed)
	}
	fenced := "```json\n{\"candidates\":[]}\n```"
	if parsed, err := ParseOutput(fenced); err != nil || len(parsed) != 0 {
		t.Fatalf("fenced empty answer: %v %v", parsed, err)
	}
	for name, bad := range map[string]string{
		"prose":            "I think the build needs fast mode.",
		"unknown top key":  `{"candidates":[],"promote":true}`,
		"missing list":     `{}`,
		"trailing object":  `{"candidates":[]} {"candidates":[]}`,
		"too many":         `{"candidates":[` + strings.Repeat(`{},`, MaxCandidates) + `{}]}`,
		"oversized output": `{"candidates":[]}` + strings.Repeat(" ", MaxOutputBytes),
	} {
		if _, err := ParseOutput(bad); !errors.Is(err, ErrOutput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateKeepsOnlyReferencesCoreIssued(t *testing.T) {
	src := sessionRef("s1")
	private := sessionRef("s2")
	private.Sensitivity = SensitivityPrivate
	known := Known{Sources: map[string]SourceRef{src.Ref(): src, private.Ref(): private}}
	ok := Proposal{ClaimType: ClaimProcedure, Subject: "build mode", Claim: "use fast", SourceRefs: []string{src.Ref()},
		CitedPaths: []string{"scripts/deploy.sh"}}
	if reason := Validate(ok, known); reason != "" {
		t.Fatalf("valid proposal rejected: %s", reason)
	}
	fact := Proposal{ClaimType: ClaimFact, Subject: "port", SourceRefs: []string{src.Ref()},
		Assertion: &Assertion{Kind: AssertConfigValue, Target: "config/app.env", Key: "PORT", Value: "8080"}}
	if reason := Validate(fact, known); reason != "" {
		t.Fatalf("valid assertion rejected: %s", reason)
	}
	cases := map[string]func(*Proposal){
		"no source":        func(p *Proposal) { p.SourceRefs = nil },
		"forged source":    func(p *Proposal) { p.SourceRefs = []string{sessionRef("elsewhere").Ref()} },
		"private source":   func(p *Proposal) { p.SourceRefs = []string{private.Ref()} },
		"escaping path":    func(p *Proposal) { p.CitedPaths = []string{"../secrets"} },
		"absolute path":    func(p *Proposal) { p.CitedPaths = []string{"/etc/passwd"} },
		"unknown type":     func(p *Proposal) { p.ClaimType = "verified_fact" },
		"empty claim":      func(p *Proposal) { p.Claim = "  " },
		"oversized claim":  func(p *Proposal) { p.Claim = strings.Repeat("x", MaxClaimBytes+1) },
		"too many paths":   func(p *Proposal) { p.CitedPaths = []string{"a", "b", "c", "d", "e"} },
		"duplicate source": func(p *Proposal) { p.SourceRefs = []string{src.Ref(), src.Ref()} },
		"assertion on a procedure": func(p *Proposal) {
			p.Assertion = &Assertion{Kind: AssertFileExists, Target: "a", Value: "present"}
		},
	}
	for name, mutate := range cases {
		p := ok
		mutate(&p)
		if Validate(p, known) == "" {
			t.Errorf("%s: kept", name)
		}
	}
	assertions := map[string]Assertion{
		"unknown kind":           {Kind: "approved_constraint", Target: "a", Value: "x"},
		"escaping target":        {Kind: AssertConfigValue, Target: "../app.env", Key: "PORT", Value: "8080"},
		"keyed kind without key": {Kind: AssertDependencyVersion, Target: "go.mod", Value: "v1"},
		"key on unkeyed kind":    {Kind: AssertFileExists, Target: "a", Key: "k", Value: "present"},
		"bad file state":         {Kind: AssertFileExists, Target: "a", Value: "maybe"},
		"bad port":               {Kind: AssertPortValue, Target: "api", Value: "70000"},
		"bad service name":       {Kind: AssertPortValue, Target: "API/../x", Value: "80"},
		"multi-line value":       {Kind: AssertConfigValue, Target: "a.env", Key: "K", Value: "1\n2"},
		"empty value":            {Kind: AssertConfigValue, Target: "a.env", Key: "K", Value: ""},
	}
	for name, a := range assertions {
		p := fact
		p.Assertion = &a
		if Validate(p, known) == "" {
			t.Errorf("assertion %s: kept", name)
		}
	}
}

func TestAssertionIdentity(t *testing.T) {
	a := Assertion{Kind: AssertConfigValue, Target: "config/app.env", Key: "PORT", Value: "8080"}
	b := a
	b.Value = "9090"
	if a.Ref() != b.Ref() || a.ValueDigest() == b.ValueDigest() {
		t.Fatal("two values of one key must share the ref and differ in value digest")
	}
	if parsed, ok := ParseAssertionRef(a.Ref()); !ok || parsed.Ref() != a.Ref() || parsed.Value != "" {
		t.Fatalf("ref does not round-trip: %+v %v", parsed, ok)
	}
	other := a
	other.Target = "config/other.env"
	if other.Ref() == a.Ref() || other.ValueDigest() == a.ValueDigest() {
		t.Fatal("another target kept the identity")
	}
	if _, ok := ParseAssertionRef("constraint:something"); ok {
		t.Fatal("a non-assertion ref parsed")
	}
	p := Proposal{ClaimType: ClaimFact, Subject: "anything", Assertion: &a}
	if CandidateSubjectKey("anything", &a) != CandidateSubjectKey("else", &b) || ClaimDigest(p) != a.ValueDigest() {
		t.Fatal("an assertion's conflict key or claim identity depends on the maintainer's wording")
	}
	s1 := SupportDigest(ClaimDigest(p), "src-v1", "project", "policy", "bind")
	for name, s := range map[string]string{
		"source version": SupportDigest(ClaimDigest(p), "src-v2", "project", "policy", "bind"),
		"scope":          SupportDigest(ClaimDigest(p), "src-v1", "other", "policy", "bind"),
		"policy":         SupportDigest(ClaimDigest(p), "src-v1", "project", "policy2", "bind"),
		"binding":        SupportDigest(ClaimDigest(p), "src-v1", "project", "policy", "bind2"),
	} {
		if s == s1 {
			t.Errorf("another %s kept the support identity", name)
		}
	}
}

func TestDecideOrder(t *testing.T) {
	q := Facts{Status: StatusQualified, AtCurrent: "passed"}
	cases := []struct {
		name string
		f    Facts
		want Applicability
	}{
		{"current", q, Current},
		{"no result here", Facts{Status: StatusQualified}, Unknown},
		{"failed here", Facts{Status: StatusQualified, AtCurrent: "failed"}, Stale},
		{"cited changed beats a pass", Facts{Status: StatusQualified, AtCurrent: "passed", CitedChanged: true}, Stale},
		{"conflict beats a pass", Facts{Status: StatusQualified, AtCurrent: "passed", Conflict: true}, Conflict},
		{"missing cited path", Facts{Status: StatusQualified, AtCurrent: "passed", CitedMissing: true}, SourceMissing},
		{"missing source", Facts{Status: StatusQualified, AtCurrent: "passed", SourceMissing: true}, SourceMissing},
		{"withdrawn beats everything", Facts{Status: StatusQualified, AtCurrent: "passed", SourceWithdrawn: true, SourceMissing: true, Conflict: true}, Withdrawn},
		{"source changed beats a pass", Facts{Status: StatusQualified, AtCurrent: "passed", SourceChanged: true}, Stale},
		{"unsupported never current", Facts{Status: StatusQualified, AtCurrent: "passed", Unsupported: true}, CandidateOnly},
		{"candidate only never current", Facts{Status: StatusCandidateOnly, AtCurrent: "passed"}, CandidateOnly},
	}
	for _, c := range cases {
		if got, _ := Decide(c.f); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCleanPath(t *testing.T) {
	for _, p := range []string{"a", "a/b.go", "scripts/deploy.sh"} {
		if !CleanPath(p) {
			t.Errorf("%q refused", p)
		}
	}
	for _, p := range []string{"", ".", "..", "../a", "/a", "a/../b", "a//b", "a\\b", "./a"} {
		if CleanPath(p) {
			t.Errorf("%q accepted", p)
		}
	}
}
