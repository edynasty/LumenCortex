package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestCognitionRevokedBindingCannotCommitQualified(t *testing.T) {
	f := newCognitionFixture(t, true)
	a := f.run(t, "inspect service", cogAnswer("8080"))
	binding := f.bind(t, portFact("8080"), "port")
	f.answer = func(refs []string) string {
		if _, err := f.engine.RevokeApproval(context.Background(), RevokeApprovalRequest{Kind: RevokeBinding, BindingDigest: binding.BindingDigest}); err != nil {
			t.Fatal(err)
		}
		return candidates(factJSON("service port", []string{refOf(t, refs, a)}, nil, portFact("8080")))
	}
	r := f.maintain(t)
	if r.Qualified != 0 || r.CandidateOnly != 1 || len(r.Inserted) != 1 {
		t.Fatalf("revoked support was adopted as qualified: %+v", r)
	}
	if obs := f.observations(t, r.Inserted[0]); len(obs) != 0 {
		t.Fatalf("revoked support appended a qualified observation: %+v", obs)
	}
}

func TestCognitionPolicyChangeCannotReusePriorContract(t *testing.T) {
	f := newCognitionFixture(t, true)
	a := f.run(t, "inspect service", cogAnswer("8080"))
	r := f.adoptFacts(t, a, portFact("8080"))
	if r.Qualified != 1 {
		t.Fatalf("adoption: %+v", r)
	}
	oldPolicy := knowledge.PolicyVersion
	knowledge.PolicyVersion = oldPolicy + "-r2-test"
	t.Cleanup(func() { knowledge.PolicyVersion = oldPolicy })
	p := scripted(readStep("read", r.Inserted[0]))
	f.run(t, "read under updated policy", p)
	if v := viewIn(t, p.request(t, 1), "read"); v.Applicability != knowledge.CandidateOnly {
		t.Fatalf("old policy remained an active support contract: %+v", v)
	}
	b := f.run(t, "independent support under new policy", cogAnswer("8080"))
	withoutApproval := f.adoptFacts(t, b, portFact("8080"))
	if withoutApproval.Qualified != 0 || withoutApproval.CandidateOnly != 1 {
		t.Fatalf("old binding approved a new policy: %+v", withoutApproval)
	}
	f.bind(t, portFact("8080"), "port")
	c := f.run(t, "new policy approved support", cogAnswer("8080"))
	withApproval := f.adoptFacts(t, c, portFact("8080"))
	if withApproval.Qualified != 1 || withApproval.Duplicates != 0 {
		t.Fatalf("new policy approval lost support to old dedupe: %+v", withApproval)
	}
}

func TestCognitionCurrentReadReflectsBindingRevocation(t *testing.T) {
	f := newCognitionFixture(t, true)
	a := f.run(t, "inspect service", cogAnswer("8080"))
	binding := f.bind(t, portFact("8080"), "port")
	r := f.adoptFacts(t, a, portFact("8080"))
	if r.Qualified != 1 {
		t.Fatalf("adoption: %+v", r)
	}
	p := scripted(readStep("before", r.Inserted[0]), func(protocol.ProviderRequest) protocol.ProviderResponse {
		if _, err := f.engine.RevokeApproval(context.Background(), RevokeApprovalRequest{Kind: RevokeBinding, BindingDigest: binding.BindingDigest}); err != nil {
			t.Fatal(err)
		}
		return cogStep("after", "read_knowledge", `{"ref":"`+r.Inserted[0]+`"}`)
	})
	f.run(t, "read around revocation", p)
	if v := viewIn(t, p.request(t, 1), "before"); v.Applicability != knowledge.Current {
		t.Fatalf("before: %+v", v)
	}
	if v := viewIn(t, p.request(t, 2), "after"); v.Applicability != knowledge.CandidateOnly {
		t.Fatalf("after: %+v", v)
	}
}

func TestCognitionStartIndexRechecksRevokedBinding(t *testing.T) {
	f := newCognitionFixture(t, true)
	ext := t.TempDir()
	started, release := filepath.Join(ext, "started"), filepath.Join(ext, "release")
	t.Setenv("LC_R2_CHECK_WAIT", ext)
	t.Setenv("LC_R1_PORT", "8080")
	if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	checks := `{"port":{"kind":"test","command":"touch \"$LC_R2_CHECK_WAIT/started\"; while ! test -e \"$LC_R2_CHECK_WAIT/release\"; do sleep 0.01; done; test \"$LC_R1_PORT\" = 8080","required":false}}`
	f.setChecks(t, checks, map[string]knowledge.Assertion{"port": portFact("8080")})
	binding := f.bind(t, portFact("8080"), "port")
	a := f.run(t, "inspect service", cogAnswer("8080"))
	r := f.adoptFacts(t, a, portFact("8080"))
	if r.Qualified != 1 {
		t.Fatalf("adoption: %+v", r)
	}
	for _, p := range []string{release, started} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("go"), 0600) })
	p := scripted(readStep("read", r.Inserted[0]))
	h, err := f.engine.NewSession(ctx, SessionOptions{Goal: "consume after revocation"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.engine.runAgentWith(ctx, h.ID, p, AgentOptions{Policy: "workspace", MaxSteps: 3})
		done <- err
	}()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("reader ended before check: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err := f.engine.RevokeApproval(ctx, RevokeApprovalRequest{Kind: RevokeBinding, BindingDigest: binding.BindingDigest}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if app := startApplicability(p.request(t, 0), r.Inserted[0]); app == knowledge.Current {
		t.Fatalf("start inherited revoked support as current: %s", app)
	}
	if view := viewIn(t, p.request(t, 1), "read"); view.Applicability != knowledge.CandidateOnly {
		t.Fatalf("reader ignored binding revocation: %+v", view)
	}
}

func TestCognitionSupportedViewDoesNotPromoteModelProse(t *testing.T) {
	f := newCognitionFixture(t, true)
	a := f.run(t, "inspect service", cogAnswer("8080"))
	const poisoned = "VERIFIED-PORT-9090"
	f.answer = func(refs []string) string {
		return candidates(fmt.Sprintf(`{"claimType":"fact","subject":%q,"claim":"9090 is verified","rationale":%q,"sourceRefs":[%q],"assertion":{"kind":"port_value","target":"service","value":"8080"}}`, poisoned, poisoned, refOf(t, refs, a)))
	}
	r := f.maintain(t)
	if r.Qualified != 1 {
		t.Fatalf("exact assertion should remain supportable: %+v", r)
	}
	v := f.readOnce(t, r.Inserted[0])
	if v.Applicability != knowledge.Current || v.Claim != portFact("8080").Render() {
		t.Fatalf("supported assertion: %+v", v)
	}
	if strings.Contains(v.Subject+v.Claim+v.Rationale, poisoned) {
		t.Fatalf("unsupported model prose rode on a current assertion: %+v", v)
	}
}

func TestCognitionFiveAssertionSupportBoundaries(t *testing.T) {
	for _, tc := range []struct {
		assertion knowledge.Assertion
		wrong     string
		command   string
	}{
		{knowledge.Assertion{Kind: knowledge.AssertConfigValue, Target: "config.env", Key: "PORT", Value: "8080"}, "9090", `. ./config.env; test "$PORT" = 8080`},
		{knowledge.Assertion{Kind: knowledge.AssertDependencyVersion, Target: "dependencies.txt", Key: "pkg", Value: "1.0.0"}, "2.0.0", `test "$(cat dependencies.txt)" = 'pkg=1.0.0'`},
		{knowledge.Assertion{Kind: knowledge.AssertSymbolSignature, Target: "signature.txt", Key: "Serve", Value: "func Serve() error"}, "func Serve() string", `test "$(cat signature.txt)" = 'func Serve() error'`},
		{knowledge.Assertion{Kind: knowledge.AssertFileExists, Target: "marker.txt", Value: "present"}, "absent", `test -f marker.txt`},
		{knowledge.Assertion{Kind: knowledge.AssertPortValue, Target: "api", Value: "8080"}, "9090", `test "$LC_R2_ASSERTION_PORT" = 8080`},
	} {
		t.Run(tc.assertion.Kind, func(t *testing.T) {
			f := newCognitionFixture(t, true)
			t.Setenv("LC_R2_ASSERTION_PORT", "8080")
			writeFile(t, filepath.Join(f.root, "dependencies.txt"), "pkg=1.0.0\n")
			writeFile(t, filepath.Join(f.root, "signature.txt"), "func Serve() error\n")
			writeFile(t, filepath.Join(f.root, "marker.txt"), "present\n")
			runGitIn(t, f.root, "add", "dependencies.txt", "signature.txt", "marker.txt")
			runGitIn(t, f.root, "commit", "-m", "support fixtures")
			check, err := json.Marshal(map[string]any{"bound": map[string]any{"kind": "test", "command": tc.command, "required": false}})
			if err != nil {
				t.Fatal(err)
			}
			f.setChecks(t, string(check), map[string]knowledge.Assertion{"bound": tc.assertion})
			positiveSource := f.run(t, "inspect supported fact", cogAnswer(tc.assertion.Render()))
			positive := f.adoptFacts(t, positiveSource, tc.assertion)
			if positive.Qualified != 1 || len(positive.Inserted) != 1 {
				t.Fatalf("positive adoption: %+v", positive)
			}
			if view := f.readOnce(t, positive.Inserted[0]); view.Applicability != knowledge.Current || view.Claim != tc.assertion.Render() {
				t.Fatalf("positive current read: %+v", view)
			}
			wrong := tc.assertion
			wrong.Value = tc.wrong
			otherScope := tc.assertion
			otherScope.Target += "-other"
			f.bindAt(t, otherScope, "bound", "another-project")
			alias := tc.assertion
			alias.Kind += "_alias"
			source := f.run(t, "inspect typed facts", cogAnswer(tc.assertion.Render()))
			f.propose(func(refs []string) []string {
				ref := refOf(t, refs, source)
				entry := func(label string, a knowledge.Assertion) string { return factJSON(label, []string{ref}, nil, a) }
				attached := strings.TrimSuffix(entry("attached-check", wrong), "}") + `,"checkIds":["bound"]}`
				predicate := strings.Replace(entry("predicate", tc.assertion), `"assertion":{`, `"assertion":{"predicate":"not_equals",`, 1)
				duplicate := strings.Replace(entry("duplicate-value", tc.assertion), `"value":`+strconv.Quote(tc.assertion.Value), `"value":`+strconv.Quote(tc.assertion.Value)+`,"value":`+strconv.Quote(tc.wrong), 1)
				return []string{entry("correct", tc.assertion), entry("unbound-value", wrong), attached, entry("scope", otherScope), predicate, entry("alias", alias), duplicate, candidateJSON("fact", "free-text", wrong.Render(), ref, nil)}
			})
			r := f.maintain(t)
			// The duplicate value decodes to the same unsupported proposition as
			// unbound-value; support dedupe must keep one, never the earlier PASS.
			if r.Qualified != 1 || r.CandidateOnly != 3 || r.Rejected != 3 || r.Duplicates != 1 || len(r.Inserted) != 7 {
				t.Fatalf("typed support boundary: %+v", r)
			}
			contrary := 0
			for _, id := range r.Inserted {
				rec := f.candidate(t, id)
				if rec.Status == knowledge.StatusQualified {
					if rec.Subject != "correct" || rec.Assertion == nil || *rec.Assertion != tc.assertion {
						t.Fatalf("unsupported adoption: %+v", rec)
					}
					// The unsupported contrary value is still a live candidate, and it is model output
					// without host support: it cannot change what the supported assertion is to a reader
					// (knowledge.EvidenceLevel).
					if view := f.readOnce(t, id); view.Applicability != knowledge.Current || view.Claim != tc.assertion.Render() {
						t.Fatalf("supported read next to an unsupported contrary value: %+v", view)
					}
					continue
				}
				if observations := f.observations(t, id); len(observations) != 0 {
					t.Fatalf("unsupported candidate gained observations: %+v", observations)
				}
				// The conflict is not hidden: it is what the unsupported contrary value reads as.
				if rec.Status == knowledge.StatusCandidateOnly && rec.Assertion != nil && *rec.Assertion == wrong {
					contrary++
					if view := f.readOnce(t, id); view.Applicability != knowledge.Conflict || view.Claim != wrong.Render() {
						t.Fatalf("unsupported contrary read: %+v", view)
					}
				}
			}
			if contrary != 1 {
				t.Fatalf("%d unsupported contrary candidates, want 1", contrary)
			}
			t.Log("one exact host-supported assertion; unrelated PASS, selected check, scope, predicate, alias, duplicate value and free text cannot broaden support")
		})
	}
}
