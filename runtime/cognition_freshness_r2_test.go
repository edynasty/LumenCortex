package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestCognitionRunEnvironmentChangeRevokesPass(t *testing.T) {
	f := newCognitionFixture(t, true)
	t.Setenv("LC_R1_PORT", "8080")
	f.setChecks(t, freshChecks, map[string]knowledge.Assertion{"port": portFact("8080")})
	source := f.run(t, "inspect external service", cogAnswer("port 8080"))
	adopted := f.adoptFacts(t, source, portFact("8080"))
	if adopted.Qualified != 1 {
		t.Fatalf("adoption: %+v", adopted)
	}
	id := adopted.Inserted[0]
	p := scripted(readStep("before", id), func(protocol.ProviderRequest) protocol.ProviderResponse {
		// A host changes the environment, not a workspace file or an agent tool.
		t.Setenv("LC_R1_PORT", "9090")
		return cogStep("after", "read_knowledge", `{"ref":"`+id+`"}`)
	})
	f.run(t, "use service across host environment change", p)
	if v := viewIn(t, p.request(t, 1), "before"); v.Applicability != knowledge.Current {
		t.Fatalf("fresh evidence unavailable before change: %+v", v)
	}
	if v := viewIn(t, p.request(t, 2), "after"); v.Applicability != knowledge.Unknown {
		t.Fatalf("same-run environment change reused an old PASS: %+v", v)
	}
	q := scripted(readStep("fresh", id))
	f.run(t, "verify changed environment in new run", q)
	if v := viewIn(t, q.request(t, 1), "fresh"); v.Applicability != knowledge.Stale {
		t.Fatalf("new check failure did not make knowledge stale: %+v", v)
	}
}

func TestCognitionSchemaCannotPromoteExternalUnversionedDependency(t *testing.T) {
	f := newCognitionFixture(t, true)
	external := filepath.Join(t.TempDir(), "external-schema.json")
	writeFile(t, external, `{"const":{"port":"8080"}}`)
	ref, err := json.Marshal(map[string]string{"$ref": "file://" + filepath.ToSlash(external)})
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, "schema.json", string(ref))
	f.write(t, "port.json", `{"port":"8080"}`)
	assertion := knowledge.Assertion{Kind: knowledge.AssertConfigValue, Target: "port.json", Key: "port", Value: "8080"}
	f.setChecks(t, `{"schema":{"kind":"schema","schema":"schema.json","files":["port.json"],"required":false}}`, map[string]knowledge.Assertion{"schema": assertion})
	source := f.run(t, "inspect static configuration", cogAnswer("port 8080"))
	adopted := f.adoptFacts(t, source, assertion)
	if adopted.Qualified != 0 || adopted.CandidateOnly != 1 {
		t.Fatalf("unversioned external schema acquired reusable support: %+v", adopted)
	}
	if v := f.hostView(t, adopted.Inserted[0]); v.Applicability != knowledge.CandidateOnly {
		t.Fatalf("external schema is not static version-bound evidence: %+v", v)
	}
}

func TestCognitionStaticSchemaReusesOnlyUnchangedFiles(t *testing.T) {
	f := newCognitionFixture(t, true)
	f.write(t, "schema.json", `{"type":"object","properties":{"port":{"const":"8080"}},"required":["port"]}`)
	f.write(t, "port.json", `{"port":"8080"}`)
	assertion := knowledge.Assertion{Kind: knowledge.AssertConfigValue, Target: "port.json", Key: "port", Value: "8080"}
	f.setChecks(t, `{"schema":{"kind":"schema","schema":"schema.json","files":["port.json"],"required":false}}`, map[string]knowledge.Assertion{"schema": assertion})
	source := f.run(t, "inspect static configuration", cogAnswer("port 8080"))
	adopted := f.adoptFacts(t, source, assertion)
	if adopted.Qualified != 1 {
		t.Fatalf("adoption: %+v", adopted)
	}
	id := adopted.Inserted[0]
	before := len(f.observations(t, id))
	for range 2 {
		p := scripted(readStep("read", id))
		f.run(t, "reuse immutable file evidence", p)
		if v := viewIn(t, p.request(t, 1), "read"); v.Applicability != knowledge.Current {
			t.Fatalf("unchanged static evidence not reusable: %+v", v)
		}
	}
	if n := len(f.observations(t, id)); n != before {
		t.Fatalf("unchanged static evidence reran unnecessarily: %d -> %d", before, n)
	}
	f.write(t, "port.json", `{"port":"9090"}`)
	if v := f.hostView(t, id); v.Applicability != knowledge.Unknown {
		t.Fatalf("changed static evidence still current: %+v", v)
	}
	p := scripted(readStep("read", id))
	f.run(t, "revalidate changed static files", p)
	if v := viewIn(t, p.request(t, 1), "read"); v.Applicability != knowledge.Stale {
		t.Fatalf("failed static revalidation not stale: %+v", v)
	}
}

func TestCognitionCheckFailureAndRealRevalidationDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		timeout      int
		want         knowledge.Applicability
	}{
		{"nonzero", "exit 7", 30, knowledge.Stale},
		{"process-killed", "kill -KILL $$", 30, knowledge.Stale},
		{"check-timeout", "sleep 30", 1, knowledge.Unknown},
		{"host-15-second-budget", "sleep 30", 60, knowledge.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCognitionFixture(t, true)
			t.Setenv("LC_R2_CHECK_MODE", "pass")
			command := `if [ "$LC_R2_CHECK_MODE" = pass ]; then exit 0; fi; ` + tc.action
			encoded, err := json.Marshal(command)
			if err != nil {
				t.Fatal(err)
			}
			f.setChecks(t, fmt.Sprintf(`{"port":{"kind":"test","command":%s,"timeoutSeconds":%d,"required":false}}`, encoded, tc.timeout), map[string]knowledge.Assertion{"port": portFact("8080")})
			src := f.run(t, "inspect service", cogAnswer("8080"))
			r := f.adoptFacts(t, src, portFact("8080"))
			if r.Qualified != 1 {
				t.Fatalf("adoption: %+v", r)
			}
			id := r.Inserted[0]
			before := f.observations(t, id)
			t.Setenv("LC_R2_CHECK_MODE", "fail")
			p := scripted(readStep("read", id))
			began := time.Now()
			f.run(t, "revalidate service", p)
			duration := time.Since(began)
			if tc.name == "host-15-second-budget" && (duration < 14*time.Second || duration > 22*time.Second) {
				t.Fatalf("real 15-second deadline not enforced (bounded shell cleanup included): %v", duration)
			}
			if v := viewIn(t, p.request(t, 1), "read"); v.Applicability != tc.want {
				t.Fatalf("%s kept old PASS: %+v", tc.name, v)
			}
			after := f.observations(t, id)
			if tc.want == knowledge.Unknown && len(after) != len(before) {
				t.Fatalf("incomplete check fabricated support: %+v", after)
			}
			if tc.want == knowledge.Stale && (len(after) != len(before)+1 || after[0].Outcome != "failed") {
				t.Fatalf("failure not appended: %+v", after)
			}
			t.Logf("%s: %s after %.3fs; historical PASS preserved", tc.name, tc.want, duration.Seconds())
		})
	}
}

func TestCognitionConcurrentRunsOwnTheirObservations(t *testing.T) {
	f := newCognitionFixture(t, true)
	src := f.run(t, "inspect service", cogAnswer("8080"))
	r := f.adoptFacts(t, src, portFact("8080"))
	if r.Qualified != 1 {
		t.Fatalf("adoption: %+v", r)
	}
	id := r.Inserted[0]
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	ps := make([]*cognitionProvider, 2)
	ids := make([]string, 2)
	done := make(chan error, 2)
	for i := range 2 {
		p := scripted(func(protocol.ProviderRequest) protocol.ProviderResponse {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return cogStep("read", "read_knowledge", `{"ref":"`+id+`"}`)
		})
		ps[i] = p
		h, err := f.engine.NewSession(ctx, SessionOptions{Goal: "concurrent reader"})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = h.ID
		model := bind(f.engine, p)
		go func(sid string) {
			result, err := f.engine.RunAgent(ctx, sid, AgentOptions{ModelRef: model, Policy: "workspace", MaxSteps: 3})
			if err == nil && result.Status != "completed" {
				err = fmt.Errorf("run ended %s", result.Status)
			}
			done <- err
		}(h.ID)
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("readers did not overlap")
		}
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range ps {
		if v := viewIn(t, p.request(t, 1), "read"); v.Applicability != knowledge.Current {
			t.Fatalf("reader: %+v", v)
		}
	}
	observations := f.observations(t, id)
	owners := map[string]string{}
	for _, o := range observations {
		if o.Purpose == "revalidation" {
			owners[o.SessionID] = o.RunID
		}
	}
	if owners[ids[0]] == "" || owners[ids[1]] == "" || owners[ids[0]] == owners[ids[1]] {
		t.Fatalf("concurrent readers shared evidence: %+v", observations)
	}
	if v := f.hostView(t, id); v.Applicability != knowledge.Unknown {
		t.Fatalf("completed run left host evidence current: %+v", v)
	}
}
