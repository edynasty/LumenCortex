package knowledge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var experienceTestNow = time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)

func experienceTestScope() Scope {
	return Scope{Kind: ScopeProject, OwnerID: "owner", ProjectID: "project"}
}

func experienceTestSource(session string) SourceRef {
	return SourceRef{
		Kind:        KindSession,
		ID:          session,
		Digest:      Digest("source", session),
		Scope:       "project",
		RunID:       "run-" + session,
		SessionID:   session,
		Sensitivity: SensitivitySession,
		Provenance:  ProvenanceRuntime,
	}
}

func experienceTestEvidence(a Assertion, source SourceRef, session, run, verification string) Evidence {
	e := Evidence{
		ID:            "evidence-" + session + "-" + verification,
		Source:        source,
		Type:          "check",
		Provenance:    ProvenanceRuntime,
		Trust:         "framework",
		Verification:  verification,
		Scope:         experienceTestScope(),
		Times:         Times{RecordedAt: experienceTestNow, VerifiedAt: experienceTestNow},
		Freshness:     FreshnessVersion,
		Fingerprint:   "fingerprint",
		BindingDigest: Digest("binding"),
		SpecDigest:    Digest("spec"),
		ApprovalEpoch: 1,
		SessionID:     session,
		RunID:         run,
		ResultSeq:     1,
	}
	if verification == "failed" {
		e.ContradictingClaims = []string{a.ValueDigest()}
	} else {
		e.SupportingClaims = []string{a.ValueDigest()}
	}
	return e
}

func experienceTestObservation(a Assertion, source SourceRef, session, run, verification string) ExperienceObservation {
	return ExperienceObservation{Assertion: a, Evidence: experienceTestEvidence(a, source, session, run, verification)}
}

func experienceTestObservationPtr(a Assertion, source SourceRef, session, run, verification string) *ExperienceObservation {
	o := experienceTestObservation(a, source, session, run, verification)
	return &o
}

func experienceTestDefinition() ExperienceProposal {
	return ExperienceProposal{
		ProblemPattern:  "service fails to bind",
		ProblemType:     "configuration",
		Context:         "local test workspace",
		ProposedCause:   "stale port",
		Preconditions:   []Assertion{{Kind: AssertConfigValue, Target: "config/app.yaml", Key: "port", Value: "8080"}},
		Strategy:        []string{"update the configured port"},
		ExpectedOutcome: Assertion{Kind: AssertFileExists, Target: "build/app", Value: "present"},
		ExpectedResult:  "passed",
		CitedPaths:      []string{"config/app.yaml"},
	}
}

func experienceTestExperience() Experience {
	source := experienceTestSource("origin-session")
	definition := experienceTestDefinition()
	x := Experience{
		Origin: ExperienceOrigin{
			Source:      source,
			TaskVersion: 7,
			TaskRef:     "task_state:origin-session@7",
			Outcome:     "completed",
			CompletedAt: experienceTestNow,
		},
		Definition:        definition,
		DefinitionVersion: 1,
		State:             ExperienceValidated,
		Uncertainty:       ExperienceUncertainty,
		CreatedAt:         experienceTestNow,
		LastVerifiedAt:    experienceTestNow,
		Validation: ExperienceValidation{
			Method:     ExperienceMethod,
			Conditions: []ExperienceObservation{experienceTestObservation(definition.Preconditions[0], source, "origin-session", "origin-run", "passed")},
			Outcome:    experienceTestObservationPtr(definition.ExpectedOutcome, source, "origin-session", "origin-run", "passed"),
		},
	}
	x.Confidence = x.ConditionalConfidence()
	return x
}

func experienceTestUse(def ExperienceProposal, x Experience) ExperienceUse {
	consumer := "consumer-session"
	source := experienceTestSource(consumer)
	return ExperienceUse{
		ID:                "use-1",
		ConsumerID:        consumer,
		BaselineDigest:    Digest("baseline"),
		PreviousRunID:     "consumer-previous",
		DefinitionVersion: x.DefinitionVersion,
		DefinitionDigest:  def.Digest(),
		ExpectedOutcome:   def.ExpectedOutcome,
		ExpectedResult:    def.ExpectedResult,
		Conditions:        []ExperienceObservation{experienceTestObservation(def.Preconditions[0], source, consumer, "", "passed")},
		AdoptedAt:         experienceTestNow.Add(time.Minute),
		Feedback: &ExperienceFeedback{
			Source:         source,
			Outcome:        "success",
			Used:           true,
			ConditionsHeld: true,
			Reason:         "expected outcome held",
			Observation:    experienceTestObservationPtr(def.ExpectedOutcome, source, consumer, "", "passed"),
			At:             experienceTestNow.Add(time.Hour),
		},
	}
}

func experienceTestProposalJSON() string {
	raw, _ := json.Marshal(experienceTestDefinition())
	return string(raw)
}

func experienceTestProposalJSONWith(field, value string) string {
	var object map[string]json.RawMessage
	_ = json.Unmarshal([]byte(experienceTestProposalJSON()), &object)
	object[field] = json.RawMessage(value)
	raw, _ := json.Marshal(object)
	return string(raw)
}

func TestExperienceProposalStrictAndSafe(t *testing.T) {
	if _, err := ParseExperienceProposal(experienceTestProposalJSON()); err != nil {
		t.Fatalf("safe structured proposal rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"authority field", experienceTestProposalJSONWith("state", `"validated"`)},
		{"hidden reasoning field", experienceTestProposalJSONWith("hiddenReasoning", `"private chain of thought"`)},
		{"authority inside assertion", experienceTestProposalJSONWith("expectedOutcome", `{"kind":"file_exists","target":"build/app","value":"present","verified":true}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseExperienceProposal(tc.raw); err == nil {
				t.Fatalf("proposal with %s was accepted", tc.name)
			}
		})
	}
	if !SafeExperienceText("update config/app.yaml", 1024) {
		t.Fatal("safe description rejected")
	}
	for _, text := range []string{
		"read /Users/alice/.ssh/id_rsa",
		"token ghp_0123456789abcdefghij",
		"C:\\Users\\alice\\secret.txt",
	} {
		if SafeExperienceText(text, 1024) {
			t.Fatalf("unsafe text accepted: %q", text)
		}
	}
}

func TestExperienceProposalBoundsAndSecretFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ExperienceProposal)
	}{
		{"too many conditions", func(p *ExperienceProposal) {
			p.Preconditions = []Assertion{
				{Kind: AssertPortValue, Target: "service-a", Value: "8080"},
				{Kind: AssertPortValue, Target: "service-b", Value: "8081"},
				{Kind: AssertPortValue, Target: "service-c", Value: "8082"},
				{Kind: AssertPortValue, Target: "service-d", Value: "8083"},
			}
		}},
		{"duplicate condition proposition", func(p *ExperienceProposal) {
			p.Preconditions = append(p.Preconditions, p.Preconditions[0])
		}},
		{"too many strategy steps", func(p *ExperienceProposal) {
			p.Strategy = []string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}
		}},
		{"too many cited paths", func(p *ExperienceProposal) {
			p.CitedPaths = []string{"a", "b", "c", "d", "e"}
		}},
		{"absolute citation", func(p *ExperienceProposal) {
			p.CitedPaths = []string{"/etc/passwd"}
		}},
		{"private path in context", func(p *ExperienceProposal) {
			p.Context = "inspected /Users/alice/.ssh/id_rsa"
		}},
		{"credential assertion key", func(p *ExperienceProposal) {
			p.Preconditions = []Assertion{{Kind: AssertConfigValue, Target: "config/app.yaml", Key: "api_token", Value: "value"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := experienceTestDefinition()
			tc.change(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("proposal with %s was accepted", tc.name)
			}
		})
	}
}

func TestExperiencePromotionRequiresExactPassingObservations(t *testing.T) {
	if err := experienceTestExperience().Validate(); err != nil {
		t.Fatalf("validated experience fixture rejected: %v", err)
	}
	other := Assertion{Kind: AssertFileExists, Target: "build/other", Value: "present"}
	for _, tc := range []struct {
		name   string
		change func(*Experience)
	}{
		{"outcome evidence failed", func(x *Experience) {
			x.Validation.Outcome = experienceTestObservationPtr(x.Definition.ExpectedOutcome, x.Origin.Source, "origin-session", "origin-run", "failed")
		}},
		{"condition evidence failed", func(x *Experience) {
			x.Validation.Conditions[0] = experienceTestObservation(x.Definition.Preconditions[0], x.Origin.Source, "origin-session", "origin-run", "failed")
		}},
		{"condition differs from definition", func(x *Experience) {
			x.Validation.Conditions[0] = experienceTestObservation(other, x.Origin.Source, "origin-session", "origin-run", "passed")
		}},
		{"outcome differs from definition", func(x *Experience) {
			x.Validation.Outcome = experienceTestObservationPtr(other, x.Origin.Source, "origin-session", "origin-run", "passed")
		}},
		{"missing condition observation", func(x *Experience) {
			x.Validation.Conditions = nil
		}},
		{"failed origin cannot promote expected success", func(x *Experience) {
			x.Origin.Outcome = "failed"
		}},
		{"truncated origin trace", func(x *Experience) {
			x.Origin.Trace.Truncated = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := experienceTestExperience()
			tc.change(&x)
			if err := x.Validate(); err == nil {
				t.Fatalf("promotion with %s was accepted", tc.name)
			}
		})
	}
}

func TestExperienceObservationBindsExactClaim(t *testing.T) {
	a := Assertion{Kind: AssertFileExists, Target: "build/app", Value: "present"}
	source := experienceTestSource("origin-session")
	for _, tc := range []struct {
		name         string
		verification string
		claims       []string
		wantErr      bool
	}{
		{"passed supports exact proposition", "passed", []string{a.ValueDigest()}, false},
		{"passed refutes exact proposition", "passed", nil, true},
		{"failed refutes exact proposition", "failed", []string{a.ValueDigest()}, false},
		{"failed supports exact proposition", "failed", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := experienceTestObservation(a, source, "origin-session", "origin-run", tc.verification)
			if tc.verification == "failed" {
				o.Evidence.ContradictingClaims = tc.claims
			} else {
				o.Evidence.SupportingClaims = tc.claims
			}
			err := o.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestExperienceAdoptionBindsExactDefinition(t *testing.T) {
	base := experienceTestExperience()
	valid := experienceTestUse(base.Definition, base)
	base.Uses = []ExperienceUse{valid}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid adoption fixture rejected: %v", err)
	}
	other := Assertion{Kind: AssertFileExists, Target: "build/other", Value: "present"}
	for _, tc := range []struct {
		name   string
		change func(*Experience)
	}{
		{"definition digest is unrelated", func(x *Experience) {
			x.Uses[0].DefinitionDigest = Digest("another-definition")
		}},
		{"expected outcome differs from definition", func(x *Experience) {
			x.Uses[0].ExpectedOutcome = other
			x.Uses[0].Feedback.Observation = experienceTestObservationPtr(other, x.Uses[0].Feedback.Source, x.Uses[0].ConsumerID, "consumer-run", "passed")
		}},
		{"condition is unrelated to definition", func(x *Experience) {
			x.Uses[0].Conditions[0] = experienceTestObservation(other, experienceTestSource(x.Uses[0].ConsumerID), x.Uses[0].ConsumerID, "consumer-run", "passed")
		}},
		{"adoption predates definition", func(x *Experience) {
			x.Uses[0].AdoptedAt = x.CreatedAt.Add(-time.Minute)
			x.Uses[0].Feedback.At = x.CreatedAt.Add(time.Minute)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := experienceTestExperience()
			x.Uses = []ExperienceUse{experienceTestUse(x.Definition, x)}
			tc.change(&x)
			if err := x.Validate(); err == nil {
				t.Fatalf("adoption with %s was accepted", tc.name)
			}
		})
	}
}

func TestExperienceFeedbackRequiresIndependentLaterEvidence(t *testing.T) {
	base := experienceTestExperience()
	valid := experienceTestUse(base.Definition, base)
	base.Uses = []ExperienceUse{valid}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid feedback fixture rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Experience)
	}{
		{"feedback before adoption", func(x *Experience) {
			x.Uses[0].Feedback.At = x.Uses[0].AdoptedAt.Add(-time.Second)
		}},
		{"feedback replays previous run", func(x *Experience) {
			x.Uses[0].Feedback.Source.RunID = x.Uses[0].PreviousRunID
		}},
		{"feedback source is baseline", func(x *Experience) {
			x.Uses[0].Feedback.Source.Digest = x.Uses[0].BaselineDigest
		}},
		{"feedback is not a use", func(x *Experience) {
			x.Uses[0].Feedback.Used = false
		}},
		{"feedback source is another session", func(x *Experience) {
			x.Uses[0].Feedback.Source.SessionID = "other-session"
		}},
		{"feedback observation is another run", func(x *Experience) {
			x.Uses[0].Feedback.Observation.Evidence.RunID = "unrelated-run"
		}},
		{"failure cites a passing expected outcome", func(x *Experience) {
			x.Uses[0].Feedback.Outcome = "failure"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := experienceTestExperience()
			x.Uses = []ExperienceUse{experienceTestUse(x.Definition, x)}
			tc.change(&x)
			if err := x.Validate(); err == nil {
				t.Fatalf("feedback with %s was accepted", tc.name)
			}
		})
	}
}

func TestExperienceRevisionAndRecordBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Experience)
	}{
		{"revision without supersession", func(x *Experience) {
			x.DefinitionVersion, x.SupersedesVersion = 2, 0
		}},
		{"revision supersedes itself", func(x *Experience) {
			x.DefinitionVersion, x.SupersedesVersion = 2, 2
		}},
		{"first version supersedes itself", func(x *Experience) {
			x.SupersedesVersion = 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := experienceTestExperience()
			tc.change(&x)
			if err := x.Validate(); err == nil {
				t.Fatalf("revision with %s was accepted", tc.name)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*Experience)
	}{
		{"too many adoptions", func(x *Experience) {
			x.Uses = make([]ExperienceUse, MaxExperienceUses+1)
		}},
		{"too many trace steps", func(x *Experience) {
			x.Origin.Trace.Steps = make([]ExperienceStep, MaxExperienceSteps+1)
		}},
		{"too many trace checks", func(x *Experience) {
			x.Origin.Trace.Checks = make([]ExperienceCheck, MaxExperienceChecks+1)
		}},
		{"more observations than conditions", func(x *Experience) {
			x.Validation.Conditions = append(x.Validation.Conditions, x.Validation.Conditions[0])
		}},
		{"unbounded validation reason", func(x *Experience) {
			x.Validation.Reason = strings.Repeat("r", 4096)
		}},
		{"private path in validation reason", func(x *Experience) {
			x.Validation.Reason = "inspected /Users/alice/.ssh/id_rsa"
		}},
		{"unbounded feedback reason", func(x *Experience) {
			use := experienceTestUse(x.Definition, *x)
			use.Feedback.Reason = strings.Repeat("r", 4096)
			x.Uses = []ExperienceUse{use}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := experienceTestExperience()
			tc.change(&x)
			if err := x.Validate(); err == nil {
				t.Fatalf("experience with %s was accepted", tc.name)
			}
		})
	}
}

func experienceTestNode() Node {
	x := experienceTestExperience()
	return Node{
		ID:         "experience-1",
		Version:    1,
		SessionID:  "maintenance-session",
		Kind:       "experience",
		Title:      "experience",
		Status:     "active",
		Scope:      experienceTestScope(),
		Times:      Times{RecordedAt: experienceTestNow},
		Sources:    []SourceRef{x.Origin.Source},
		Experience: &x,
	}
}

func TestExperienceNodeScopeAndPayloadInvariants(t *testing.T) {
	if err := experienceTestNode().Validate(); err != nil {
		t.Fatalf("valid experience node rejected: %v", err)
	}
	narrowed := experienceTestNode()
	narrowed.Scope = Scope{Kind: ScopeSession, OwnerID: "owner", ProjectID: "project", WorkspaceID: "workspace", SessionID: "origin-session"}
	if err := narrowed.Validate(); err != nil {
		t.Fatalf("experience node narrowed to origin session rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Node)
	}{
		{"scope session is not the origin", func(n *Node) {
			n.Scope = Scope{Kind: ScopeSession, OwnerID: "owner", ProjectID: "project", WorkspaceID: "workspace", SessionID: "other-session"}
		}},
		{"origin source is not retained", func(n *Node) {
			n.Sources = nil
		}},
		{"experience node has no payload", func(n *Node) {
			n.Experience = nil
		}},
		{"experience node carries an assertion", func(n *Node) {
			n.Assertion = &Assertion{Kind: AssertPortValue, Target: "service", Value: "8080"}
		}},
		{"non-experience node carries the payload", func(n *Node) {
			n.Kind = "task"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := experienceTestNode()
			tc.change(&n)
			if err := n.Validate(); err == nil {
				t.Fatalf("experience node with %s was accepted", tc.name)
			}
		})
	}
}
