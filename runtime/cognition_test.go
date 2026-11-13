package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// Persistent cognition (R1b/T5a, remediation R1), end to end through the real loop, store, checks and
// git: scripted providers stand in for the models, nothing else is faked.

// The project: config.env sets PORT from BASE, which defaults.env sets; a candidate about the port cites
// config.env, while defaults.env is an input of the checks the candidate does not cite. "port" holds
// while the configuration yields port 8080, "port9090" while it yields 9090. The host binds the
// assertion port_value(service) = 8080 to "port" and = 9090 to "port9090" (bindPorts): those are the
// only propositions a check can support.
const cognitionChecks = `{
	"port": {"kind": "test", "command": ". ./config.env && test \"$PORT\" = 8080", "required": false},
	"port9090": {"kind": "test", "command": ". ./config.env && test \"$PORT\" = 9090", "required": false}
}`

const cognitionConfig = ". ./defaults.env\nPORT=$BASE\n"

// cognitionProvider answers request n with respond and keeps every request.
type cognitionProvider struct {
	mu       sync.Mutex
	requests []protocol.ProviderRequest
	respond  func(n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error)
}

func (p *cognitionProvider) Model() string { return "cognition" }

func (p *cognitionProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	n := len(p.requests)
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return p.respond(n, req)
}

func (p *cognitionProvider) request(t *testing.T, n int) protocol.ProviderRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= len(p.requests) {
		t.Fatalf("request %d of %d", n, len(p.requests))
	}
	return p.requests[n]
}

func cogFinal(text string) protocol.ProviderResponse {
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: text}, FinishReason: "stop"}
}

func cogCall(name, args string) protocol.ProviderResponse {
	return protocol.ProviderResponse{
		Message:      protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "call_1", Name: name, Arguments: json.RawMessage(args)}}},
		FinishReason: "tool_calls",
	}
}

// cogToolResult is the result of the call cogCall made, as the next request carries it.
func cogToolResult(t *testing.T, req protocol.ProviderRequest) string {
	t.Helper()
	out := lastToolResult(req, "call_1")
	if out == "" {
		t.Fatalf("no tool result in %+v", req.Messages)
	}
	return out
}

func cogAnswer(text string) *cognitionProvider {
	return &cognitionProvider{respond: func(int, protocol.ProviderRequest) (protocol.ProviderResponse, error) { return cogFinal(text), nil }}
}

type cognitionFixture struct {
	engine     *Engine
	root       string
	maintainer *cognitionProvider
	// answer is the maintainer's final answer for the source refs of its batch.
	answer func(refs []string) string
}

func newCognitionFixture(t *testing.T, enable bool) *cognitionFixture {
	t.Helper()
	engine, root := trustedProject(t, cognitionChecks, false)
	writeFile(t, filepath.Join(root, "config.env"), cognitionConfig)
	writeFile(t, filepath.Join(root, "defaults.env"), "BASE=8080\n")
	runGitIn(t, root, "add", "config.env", "defaults.env")
	runGitIn(t, root, "commit", "-m", "config")
	approveChecks(t, engine)
	if enable {
		if err := engine.EnableCognition(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	f := &cognitionFixture{engine: engine, root: root, answer: func([]string) string { return `{"candidates":[]}` }}
	f.maintainer = &cognitionProvider{respond: func(_ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return cogFinal(f.answer(batchRefs(req))), nil
	}}
	engine.cognition = &CognitionOptions{Maintainer: bind(engine, f.maintainer)}
	f.bindPorts(t)
	return f
}

// portFact is the assertion that the service listens on port.
func portFact(port string) knowledge.Assertion {
	return knowledge.Assertion{Kind: knowledge.AssertPortValue, Target: "service", Value: port}
}

// bindAt approves, as the user, the binding of assertion a to check id in scope, with the check's
// current definition.
func (f *cognitionFixture) bindAt(t *testing.T, a knowledge.Assertion, id, scope string) ApprovalRecord {
	t.Helper()
	specs, _, err := f.engine.effectiveChecks()
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := findSpec(specs, id)
	if !ok {
		t.Fatalf("no check %s", id)
	}
	rec, err := f.engine.ApproveConstraintBinding(context.Background(), ApproveBindingRequest{ConstraintRef: a.Ref(), ConstraintDigest: a.ValueDigest(),
		CheckID: id, SpecDigest: evaluator.SpecDigest(spec), Scope: scope, ApprovedBy: session.ApprovedByUser, Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (f *cognitionFixture) bind(t *testing.T, a knowledge.Assertion, id string) ApprovalRecord {
	t.Helper()
	return f.bindAt(t, a, id, session.SourceScopeProject)
}

func (f *cognitionFixture) bindPorts(t *testing.T) {
	t.Helper()
	f.bind(t, portFact("8080"), "port")
	f.bind(t, portFact("9090"), "port9090")
}

// batchRefs are the source refs of the maintenance batch a maintainer request carries.
func batchRefs(req protocol.ProviderRequest) []string {
	for _, m := range req.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "Maintenance batch.") {
			_, raw, ok := strings.Cut(m.Content, "\n")
			if !ok {
				return nil
			}
			var batch struct {
				Sources []struct {
					Ref string `json:"ref"`
				} `json:"sources"`
			}
			if err := json.Unmarshal([]byte(raw), &batch); err != nil {
				return nil
			}
			refs := make([]string, len(batch.Sources))
			for i, source := range batch.Sources {
				refs[i] = source.Ref
			}
			return refs
		}
	}
	return nil
}

// run starts a new top-level session with goal and runs it to its end with p.
func (f *cognitionFixture) run(t *testing.T, goal string, p protocol.Provider) string {
	t.Helper()
	ctx := context.Background()
	handle, err := f.engine.NewSession(ctx, SessionOptions{Goal: goal})
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.engine.runAgentWith(ctx, handle.ID, p, AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusCompleted {
		t.Fatalf("run %s ended %s: %+v", handle.ID, result.Status, result)
	}
	return handle.ID
}

func (f *cognitionFixture) maintain(t *testing.T) MaintenanceReport {
	t.Helper()
	report, err := f.engine.MaintainCognition(context.Background())
	if err != nil {
		t.Fatalf("maintenance: %v (%+v)", err, report)
	}
	return report
}

func (f *cognitionFixture) state(t *testing.T) session.MaintenanceState {
	t.Helper()
	st, err := f.engine.CognitionState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (f *cognitionFixture) write(t *testing.T, name, body string) {
	t.Helper()
	writeFile(t, filepath.Join(f.root, name), body)
}

func refOf(t *testing.T, refs []string, sessionID string) string {
	t.Helper()
	for _, r := range refs {
		if strings.Contains(r, ":"+sessionID+"@") {
			return r
		}
	}
	t.Errorf("no ref of %s in %v", sessionID, refs)
	return ""
}

// candidateJSON is a free-text candidate: no check can support it.
func candidateJSON(claimType, subject, claim, ref string, paths []string) string {
	raw, _ := json.Marshal(map[string]any{"claimType": claimType, "subject": subject, "claim": claim, "rationale": "the run said so",
		"sourceRefs": []string{ref}, "citedPaths": paths})
	return string(raw)
}

// factJSON is a fact candidate carrying the structured assertion a; Core renders its claim.
func factJSON(subject string, refs []string, paths []string, a knowledge.Assertion) string {
	raw, _ := json.Marshal(map[string]any{"claimType": "fact", "subject": subject, "rationale": "the run said so",
		"sourceRefs": refs, "citedPaths": paths, "assertion": a})
	return string(raw)
}

func candidates(entries ...string) string {
	return `{"candidates":[` + strings.Join(entries, ",") + `]}`
}

// indexRow returns the cognition index row of a request, "" when it has none.
func indexRow(req protocol.ProviderRequest) string {
	for _, m := range req.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[runtime] Project cognition index") {
			return m.Content
		}
	}
	return ""
}

// reader is a run that reads candidate ref with read_knowledge, then answers.
func reader(ref string) *cognitionProvider {
	return &cognitionProvider{respond: func(n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 0 {
			return cogCall("read_knowledge", `{"ref":"`+ref+`"}`), nil
		}
		return cogFinal("done"), nil
	}}
}

func readView(t *testing.T, f *cognitionFixture, ref string) KnowledgeView {
	t.Helper()
	p := reader(ref)
	f.run(t, "use the project's cognition", p)
	var view KnowledgeView
	decodeJSON(t, []byte(cogToolResult(t, p.request(t, 1))), &view)
	return view
}

func (f *cognitionFixture) observations(t *testing.T, id string) []session.ObservationRecord {
	t.Helper()
	obs, err := f.engine.store.Observations(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

var (
	firstLocator = regexp.MustCompile(`Entries: (kc_[0-9a-f]+) \(`)
	claimPort    = regexp.MustCompile(`^port_value\(service\) = (\d+)$`)
	basePort     = regexp.MustCompile(`BASE=(\d+)`)
)

func cogStep(id, name, args string) protocol.ProviderResponse {
	return protocol.ProviderResponse{
		Message:      protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}}},
		FinishReason: "tool_calls",
	}
}

// lastTool is the newest tool row of a request; ok is false before the first tool call.
func lastTool(req protocol.ProviderRequest) (protocol.Message, bool) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "tool" {
			return req.Messages[i], true
		}
	}
	return protocol.Message{}, false
}

// called reports whether the run whose newest request is req called tool name.
func called(req protocol.ProviderRequest, name string) bool {
	for _, m := range req.Messages {
		for _, c := range m.ToolCalls {
			if c.Name == name {
				return true
			}
		}
	}
	return false
}

// portWorker configures the client (client.env) for the service port. It opens the first locator its
// Start names and uses the claim only when Core says it is current; otherwise it works the port out
// from the files. Every choice depends only on what Core returned, so the file it writes shows what
// inheritance changed.
func portWorker() *cognitionProvider {
	write := func(port string) protocol.ProviderResponse {
		return cogStep("w3", "write_file", `{"path":"client.env","content":"TARGET_PORT=`+port+`\n"}`)
	}
	derive := cogStep("w2", "read_file", `{"path":"defaults.env"}`)
	return &cognitionProvider{respond: func(_ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		last, ok := lastTool(req)
		if !ok {
			if m := firstLocator.FindStringSubmatch(indexRow(req)); m != nil {
				return cogStep("w1", "read_knowledge", `{"ref":"`+m[1]+`"}`), nil
			}
			return derive, nil
		}
		switch last.ToolCallID {
		case "w1":
			var view KnowledgeView
			if json.Unmarshal([]byte(last.Content), &view) == nil && view.Applicability == knowledge.Current {
				if m := claimPort.FindStringSubmatch(view.Claim); m != nil {
					return write(m[1]), nil
				}
			}
			return derive, nil
		case "w2":
			if m := basePort.FindStringSubmatch(last.Content); m != nil {
				return write(m[1]), nil
			}
			return cogFinal("the port is not in defaults.env"), nil
		}
		return cogFinal("client configured"), nil
	}}
}

func (f *cognitionFixture) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (f *cognitionFixture) hostView(t *testing.T, id string) KnowledgeView {
	t.Helper()
	v, err := f.engine.ReadKnowledge(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v.(KnowledgeView)
}

// TestCognitionVerticalSlice is the T5a chain (task §10.1): Agent A works out a fact from the files;
// maintenance proposes it and the host's approved check qualifies it; Agent B inherits a locator, reads
// the claim as current and acts on it without re-deriving it; a change to a file the candidate does
// not cite but the check reads makes it unknown; the next Start reruns the check, which fails, so the
// candidate is stale and Agent C works the port out itself; maintenance of C's run yields a corrected
// candidate that Agent D uses, while the old one stays stale with its history intact.
func TestCognitionVerticalSlice(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()

	a := f.run(t, "find the service port", &cognitionProvider{respond: func(n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		switch n {
		case 0:
			return cogStep("a1", "read_file", `{"path":"config.env"}`), nil
		case 1:
			return cogStep("a2", "read_file", `{"path":"defaults.env"}`), nil
		}
		return cogFinal("The service listens on port 8080: config.env sets PORT from BASE in defaults.env."), nil
	}})
	if st := f.state(t); st.Dirty != 1 || st.Cursor != 0 {
		t.Fatalf("after A: %+v", st)
	}
	f.answer = func(refs []string) string {
		return candidates(factJSON("service port", []string{refOf(t, refs, a)}, []string{"config.env"}, portFact("8080")))
	}
	report := f.maintain(t)
	if !report.Ran || report.Sources != 1 || report.Qualified != 1 || len(report.Inserted) != 1 || !slices.Equal(report.Checks, []string{"port"}) {
		t.Fatalf("report %+v", report)
	}
	old := report.Inserted[0]
	// The maintainer's run offered read_thread only, and saw A's answer.
	req := f.maintainer.request(t, 0)
	if names := toolNames(req); !slices.Equal(names, []string{"read_thread"}) {
		t.Fatalf("maintainer tools %v", names)
	}
	if !strings.Contains(fmt.Sprint(req.Messages), "listens on port 8080") {
		t.Fatal("the batch does not hold A's answer")
	}
	if st := f.state(t); st.Cursor != 1 || st.Dirty != 1 || st.Pending != "idle" {
		t.Fatalf("after maintenance: %+v", st)
	}
	// The maintenance session's own end is no source: nothing is pending.
	if again := f.maintain(t); again.Ran {
		t.Fatalf("second pass ran: %+v", again)
	}
	if obs := f.observations(t, old); len(obs) != 1 || obs[0].Outcome != "passed" || obs[0].Purpose != session.ObservationAdoption {
		t.Fatalf("adoption observations %+v", obs)
	}

	// Agent B inherits a locator, never the claim, reads the claim as current and uses it.
	b := portWorker()
	f.run(t, "configure the client for the service port", b)
	row := indexRow(b.request(t, 0))
	if !strings.Contains(row, old+" (fact, current; paths: config.env; checks: port)") || strings.Contains(row, "8080") {
		t.Fatalf("index row %q", row)
	}
	if !slices.Contains(toolNames(b.request(t, 0)), "read_knowledge") {
		t.Fatalf("tools %v", toolNames(b.request(t, 0)))
	}
	var view KnowledgeView
	decodeJSON(t, []byte(lastToolResult(b.request(t, 1), "w1")), &view)
	if view.Applicability != knowledge.Current || !strings.Contains(view.Claim, "8080") || view.Context != KnowledgeFraming ||
		len(view.CitedPaths) != 1 || view.CitedPaths[0].State != "unchanged" {
		t.Fatalf("B's view %+v", view)
	}
	if got := f.read(t, "client.env"); got != "TARGET_PORT=8080\n" || called(b.request(t, 2), "read_file") {
		t.Fatalf("B wrote %q, re-derived: %v", got, called(b.request(t, 2), "read_file"))
	}

	// B's write moved the workspace fingerprint, and "port" runs a process: its observations are fresh
	// only for the run that made them. A host read is no run, so outside a Start nothing is current.
	if v := f.hostView(t, old); v.Applicability != knowledge.Unknown {
		t.Fatalf("after B's write %+v", v)
	}
	// A Start reruns the check in its own session and appends an observation; earlier ones stay.
	x := cogAnswer("ok")
	f.run(t, "another service port task", x)
	if row := indexRow(x.request(t, 0)); !strings.Contains(row, old+" (fact, current") {
		t.Fatalf("after revalidation, index row %q", row)
	}
	obs := f.observations(t, old)
	if len(obs) != 3 || obs[0].Purpose != session.ObservationRevalidation || obs[0].Outcome != "passed" ||
		obs[1].Purpose != session.ObservationRevalidation || obs[2].Purpose != session.ObservationAdoption {
		t.Fatalf("observations after revalidation %+v", obs)
	}

	// The indirect dependency changes: the candidate does not cite defaults.env, but the check reads it.
	f.write(t, "defaults.env", "BASE=9090\n")
	if v := f.hostView(t, old); v.Applicability != knowledge.Unknown || v.CitedPaths[0].State != "unchanged" {
		t.Fatalf("after the indirect change %+v", v)
	}
	// Agent C's Start reruns the check, which fails: the candidate is stale, C does not use it and works
	// the port out from the files.
	c := portWorker()
	cID := f.run(t, "configure the client for the service port", c)
	if row := indexRow(c.request(t, 0)); !strings.Contains(row, old+" (fact, stale") {
		t.Fatalf("C's index row %q", row)
	}
	if got := f.read(t, "client.env"); got != "TARGET_PORT=9090\n" || !called(c.request(t, 3), "read_file") {
		t.Fatalf("C wrote %q", got)
	}
	// The failed rerun is appended; the passes before it are never rewritten.
	obs = f.observations(t, old)
	if len(obs) != 4 || obs[0].Outcome != "failed" || obs[0].Purpose != session.ObservationRevalidation ||
		obs[1].Outcome != "passed" || obs[2].Outcome != "passed" || obs[3].Outcome != "passed" || obs[3].Purpose != session.ObservationAdoption {
		t.Fatalf("observations after the failed rerun %+v", obs)
	}

	// Correction: maintenance of C's run proposes the new port, and the check for it qualifies it.
	f.answer = func(refs []string) string {
		return candidates(factJSON("Service  port", []string{refOf(t, refs, cID)}, []string{"config.env", "defaults.env"}, portFact("9090")))
	}
	report = f.maintain(t)
	if report.Qualified != 1 || len(report.Inserted) != 1 {
		t.Fatalf("correction report %+v", report)
	}
	corrected := report.Inserted[0]
	// Agent D uses the corrected candidate; the stale one is named after it and does not contest it.
	d := portWorker()
	f.run(t, "configure the client for the service port", d)
	row = indexRow(d.request(t, 0))
	if !strings.Contains(row, corrected+" (fact, current") || !strings.Contains(row, old+" (fact, stale") ||
		strings.Index(row, corrected) > strings.Index(row, old) {
		t.Fatalf("index after correction %q", row)
	}
	if got := f.read(t, "client.env"); got != "TARGET_PORT=9090\n" || called(d.request(t, 2), "read_file") {
		t.Fatalf("D wrote %q, re-derived: %v", got, called(d.request(t, 2), "read_file"))
	}
	host, err := f.engine.ReadKnowledge(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if list := host.(KnowledgeList); len(list.Candidates) != 2 {
		t.Fatalf("host list %+v", list)
	}
}

// TestCognitionSourceMissingAndCitedChange: a cited file that changes makes the candidate stale even
// while its check passes; a cited file that is gone, or a source session whose row is gone, makes it
// source_missing; restoring the file restores the applicability. Deleting a source through the Engine
// fences it first, so its candidate is withdrawn and its text no longer served. A Start names neither.
func TestCognitionSourceMissingAndCitedChange(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	a := f.run(t, "find the port", cogAnswer("8080"))
	f.answer = func(refs []string) string {
		return candidates(factJSON("service port", []string{refOf(t, refs, a)}, []string{"config.env"}, portFact("8080")))
	}
	fromA := f.maintain(t).Inserted[0]

	f.write(t, "config.env", cognitionConfig+"# reviewed\n")
	if v := f.hostView(t, fromA); v.Applicability != knowledge.Stale || v.CitedPaths[0].State != "changed" {
		t.Fatalf("after the cited change %+v", v)
	}
	if err := os.Remove(filepath.Join(f.root, "config.env")); err != nil {
		t.Fatal(err)
	}
	if v := f.hostView(t, fromA); v.Applicability != knowledge.SourceMissing || v.CitedPaths[0].State != "missing" {
		t.Fatalf("after the cited file is gone %+v", v)
	}
	runGitIn(t, f.root, "checkout", "--", "config.env")
	if v := f.hostView(t, fromA); v.Applicability != knowledge.Unknown {
		t.Fatalf("after the file is restored %+v", v)
	}

	b := f.run(t, "find the build command", cogAnswer("make"))
	f.answer = func(refs []string) string {
		return candidates(candidateJSON("procedure", "build", "Build with make.", refOf(t, refs, b), nil))
	}
	fromB := f.maintain(t).Inserted[0]
	// A session row removed without a fence (an older binary, a manual repair): the source is missing.
	if err := f.engine.store.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	if v := f.hostView(t, fromB); v.Applicability != knowledge.SourceMissing || !strings.Contains(v.Reason, "source session") {
		t.Fatalf("after the source row is gone %+v", v)
	}
	if _, err := f.engine.DeleteSession(ctx, a, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
	if v := f.hostView(t, fromA); v.Applicability != knowledge.Withdrawn || v.Claim != "" {
		t.Fatalf("after the source session is deleted %+v", v)
	}
	p := cogAnswer("ok")
	f.run(t, "a task", p)
	if row := indexRow(p.request(t, 0)); row != "" {
		t.Fatalf("a Start named a candidate whose source is gone: %q", row)
	}
}

// TestCognitionRejectsUntrustedProposals: every reference in a maintainer's answer must be one Core
// issued; a status or a check the model gives itself rejects the entry; an assertion its bound check
// contradicts, or a free-text claim, is never qualified and is never pushed to a Start.
func TestCognitionRejectsUntrustedProposals(t *testing.T) {
	f := newCognitionFixture(t, true)
	a := f.run(t, "find the service port", cogAnswer("The port is 8080."))
	zero := "session:session_00@sha256:" + strings.Repeat("0", 64)
	f.answer = func(refs []string) string {
		ref := refOf(t, refs, a)
		good := factJSON("port", []string{ref}, nil, portFact("8080"))
		selfQualified := strings.TrimSuffix(good, "}") + `,"status":"qualified"}`
		selfChecked := strings.TrimSuffix(good, "}") + `,"checkIds":["port"]}`
		return candidates(
			factJSON("port", []string{zero}, nil, portFact("8080")), // source outside the batch
			selfChecked,   // self-assigned check
			selfQualified, // self-assigned status
			factJSON("port", []string{ref}, []string{"missing.txt"}, portFact("8080")),        // not a file here
			factJSON("port", []string{ref}, []string{"../config.env"}, portFact("8080")),      // escapes the workspace
			factJSON("service port", []string{ref}, []string{"config.env"}, portFact("9090")), // contradicted by its bound check
			candidateJSON("procedure", "deploys", "Deploy with make ship.", ref, nil),         // free text: unsupported
		)
	}
	report := f.maintain(t)
	if report.Rejected != 5 || report.CandidateOnly != 2 || report.Qualified != 0 || !slices.Equal(report.Checks, []string{"port9090"}) {
		t.Fatalf("report %+v", report)
	}
	recs, err := f.engine.store.Candidates(context.Background(), session.CandidateFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var contradicted string
	for _, r := range recs {
		if r.Status == knowledge.StatusCandidateOnly && r.Assertion != nil && r.Assertion.Value == "9090" {
			contradicted = r.ID
			if r.Claim != "port_value(service) = 9090" || !strings.Contains(r.StatusReason, "bound check failed") {
				t.Fatalf("contradicted candidate %+v", r)
			}
		}
	}
	if obs := f.observations(t, contradicted); len(obs) != 1 || obs[0].Outcome != "failed" || obs[0].CheckID != "port9090" {
		t.Fatalf("the contradicted candidate's observations %+v", obs)
	}
	// Nothing qualified: a Start gets no index row; a listing shows the two candidate_only entries only.
	p := &cognitionProvider{respond: func(n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 0 {
			return cogCall("read_knowledge", `{}`), nil
		}
		return cogFinal("done"), nil
	}}
	f.run(t, "another task", p)
	if row := indexRow(p.request(t, 0)); row != "" {
		t.Fatalf("index row without a qualified candidate: %q", row)
	}
	var list KnowledgeList
	decodeJSON(t, []byte(cogToolResult(t, p.request(t, 1))), &list)
	if len(list.Candidates) != 2 {
		t.Fatalf("list %+v", list)
	}
	for _, c := range list.Candidates {
		if c.Applicability != knowledge.CandidateOnly {
			t.Fatalf("listed %+v", c)
		}
	}
	// An answer that is not the candidates object is kept for audit as one rejected record; the
	// source is consumed, not retried forever.
	f.run(t, "a third task", cogAnswer("ok"))
	f.answer = func([]string) string { return "I think the port is 8080." }
	if report := f.maintain(t); report.Rejected != 1 || len(report.Inserted) != 1 {
		t.Fatalf("malformed answer report %+v", report)
	}
	if st := f.state(t); st.Cursor != st.Dirty {
		t.Fatalf("state after a malformed answer %+v", st)
	}

	// No source and a forged digest of a real source are rejected, each with its reason. A check the
	// host bound but configured after it approved the set is never run: its candidate stays
	// candidate_only without an observation.
	b := f.run(t, "find the port once more", cogAnswer("8080"))
	setProjectChecks(t, f.root, strings.Replace(cognitionChecks, `"port9090"`, `"fresh": {"kind": "test", "command": "touch fresh-ran", "required": false}, "port9090"`, 1))
	ran := knowledge.Assertion{Kind: knowledge.AssertFileExists, Target: "fresh-ran", Value: "present"}
	f.bind(t, ran, "fresh")
	f.answer = func(refs []string) string {
		ref := refOf(t, refs, b)
		forged := ref[:strings.Index(ref, "@sha256:")] + "@sha256:" + strings.Repeat("f", 64)
		return candidates(factJSON("port", []string{}, nil, portFact("8080")), factJSON("port", []string{forged}, nil, portFact("8080")),
			factJSON("fresh", []string{ref}, nil, ran))
	}
	report = f.maintain(t)
	if report.Rejected != 2 || report.Qualified != 0 || report.CandidateOnly != 1 {
		t.Fatalf("second untrusted batch %+v", report)
	}
	if _, err := os.Stat(filepath.Join(f.root, "fresh-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unapproved check ran: %v", err)
	}
	all, err := f.engine.store.Candidates(context.Background(), session.CandidateFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if !slices.Contains(report.Inserted, r.ID) {
			continue
		}
		switch r.Status {
		case knowledge.StatusRejected:
			if r.StatusReason == "" {
				t.Fatalf("rejected without a reason: %+v", r)
			}
		case knowledge.StatusCandidateOnly:
			if obs := f.observations(t, r.ID); len(obs) != 0 || !strings.Contains(r.StatusReason, "no qualified result") {
				t.Fatalf("the unapproved check's candidate %+v, observations %+v", r, obs)
			}
		default:
			t.Fatalf("unexpected status %+v", r)
		}
	}
}

// TestCognitionConflictAndWithdrawal: two qualified candidates that assert different values of one key
// are both a conflict; the same assertion restated from another source, in other words, is new support
// and no conflict. Revoking one source reaches the maintenance session that copied it and every run
// that read its candidate; the candidate is withdrawn (its text is no longer served) and the others are
// current again. A batch that saw a revoked source withdraws everything it proposed.
func TestCognitionConflictAndWithdrawal(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	// Checks that do not decide the value: the host binds one value of a key to each (a binding
	// supersedes the binding of another value of that key to the same check), so both candidates
	// qualify, and Core, which never picks, reports the conflict.
	setProjectChecks(t, f.root, strings.Replace(cognitionChecks, `"port9090"`, `"configured": {"kind": "test", "command": "test -f config.env", "required": false}, `+
		`"defaulted": {"kind": "test", "command": "test -f defaults.env", "required": false}, "port9090"`, 1))
	approveChecks(t, f.engine)
	value := func(v string) knowledge.Assertion {
		return knowledge.Assertion{Kind: knowledge.AssertConfigValue, Target: "config.env", Key: "PORT", Value: v}
	}
	f.bind(t, value("8080"), "configured")
	f.bind(t, value("9090"), "defaulted")
	propose := func(source, subject, v string) MaintenanceReport {
		f.answer = func(refs []string) string {
			return candidates(factJSON(subject, []string{refOf(t, refs, source)}, nil, value(v)))
		}
		return f.maintain(t)
	}
	a := f.run(t, "find the port", cogAnswer("8080"))
	first := propose(a, "service port", "8080")
	p := f.run(t, "find the port again", cogAnswer("8080 too"))
	restated := propose(p, "the PORT setting", "8080")
	b := f.run(t, "find the port a third time", cogAnswer("9090"))
	second := propose(b, "port", "9090")
	if first.Qualified != 1 || restated.Qualified != 1 || restated.Duplicates != 0 || second.Qualified != 1 {
		t.Fatalf("reports %+v %+v %+v", first, restated, second)
	}
	fromA, fromP, fromB := first.Inserted[0], restated.Inserted[0], second.Inserted[0]
	c := reader(fromB)
	readerID := f.run(t, "a service port task", c)
	row := indexRow(c.request(t, 0))
	for _, id := range []string{fromA, fromP, fromB} {
		if !strings.Contains(row, id+" (fact, conflict") {
			t.Fatalf("index with a conflict %q", row)
		}
	}
	rev, err := f.engine.RevokeSource(ctx, SourceSession, b, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rev.Sessions, second.MaintenanceSessionID) || !slices.Contains(rev.Sessions, readerID) ||
		slices.Contains(rev.Sessions, first.MaintenanceSessionID) || slices.Contains(rev.Sessions, restated.MaintenanceSessionID) ||
		slices.Contains(rev.Sessions, a) {
		t.Fatalf("revocation reached %v (maintenance %s, reader %s)", rev.Sessions, second.MaintenanceSessionID, readerID)
	}
	withdrawn, err := f.engine.ReadKnowledge(ctx, fromB)
	if err != nil {
		t.Fatal(err)
	}
	if v := withdrawn.(KnowledgeView); v.Applicability != knowledge.Withdrawn || v.Claim != "" || len(v.Sources) != 0 || v.Assertion != nil {
		t.Fatalf("withdrawn view %+v", v)
	}
	d := cogAnswer("ok")
	f.run(t, "a service port task after the revocation", d)
	row = indexRow(d.request(t, 0))
	if !strings.Contains(row, fromA+" (fact, current") || !strings.Contains(row, fromP+" (fact, current") || strings.Contains(row, fromB) {
		t.Fatalf("index after the revocation %q", row)
	}

	// One batch reads two sources; revoking either withdraws both of its candidates.
	g := f.run(t, "find the build command", cogAnswer("make build"))
	h := f.run(t, "find the test command", cogAnswer("make test"))
	f.answer = func(refs []string) string {
		return candidates(
			candidateJSON("procedure", "build", "Build with make build.", refOf(t, refs, g), nil),
			candidateJSON("procedure", "test", "Test with make test.", refOf(t, refs, h), nil),
		)
	}
	batch := f.maintain(t)
	// The batch also holds the task run after the revocation; the fenced reader is skipped.
	if batch.Skipped != 1 || len(batch.Inserted) != 2 {
		t.Fatalf("batch report %+v", batch)
	}
	if _, err := f.engine.RevokeSource(ctx, SourceSession, g, "test"); err != nil {
		t.Fatal(err)
	}
	for _, id := range batch.Inserted {
		got, err := f.engine.ReadKnowledge(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if v := got.(KnowledgeView); v.Applicability != knowledge.Withdrawn || v.Claim != "" {
			t.Fatalf("a candidate of the batch that saw the revoked source: %+v", v)
		}
	}
}

// TestCognitionMaintenanceSessionIsolation: a maintenance session runs only the maintainer and is not
// a thread of the project; a host cannot create one, an agent cannot read or recall it.
func TestCognitionMaintenanceSessionIsolation(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	a := f.run(t, "find the port", cogAnswer("8080"))
	f.answer = func(refs []string) string {
		return candidates(factJSON("service port", []string{refOf(t, refs, a)}, nil, portFact("8080")))
	}
	report := f.maintain(t)
	m := report.MaintenanceSessionID
	if _, err := f.engine.runAgentWith(ctx, m, cogAnswer("hijack"), AgentOptions{Policy: "workspace"}); err == nil ||
		!strings.Contains(err.Error(), "maintenance") {
		t.Fatalf("a host Start of the maintenance session: %v", err)
	}
	if _, err := f.engine.NewSession(ctx, SessionOptions{Goal: "x", Metadata: map[string]any{session.MetadataKeyPurpose: session.PurposeMaintenance}}); err == nil {
		t.Fatal("a host created a maintenance session")
	}
	if _, err := (threadReader{engine: f.engine, sessionID: a, workspace: f.root}).ReadThread(ctx, m, true, -1); err == nil {
		t.Fatal("an agent read the maintenance session")
	}
	hits, err := f.engine.Recall(ctx, "maintenance", 8, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == m {
			t.Fatal("recall found the maintenance session")
		}
	}
	if st := f.state(t); st.Dirty != 1 {
		t.Fatalf("the maintenance run became a source: %+v", st)
	}
}

// TestCognitionFailedPassKeepsSourcesDirty: a maintainer run that does not complete (an empty reply, an
// exhausted step budget) fails the pass; the cursor stays, the source stays dirty, and the next pass
// processes it.
func TestCognitionFailedPassKeepsSourcesDirty(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	a := f.run(t, "find the port", cogAnswer("8080"))
	f.answer = func([]string) string { return "" } // an empty reply interrupts the run
	report, err := f.engine.MaintainCognition(ctx)
	if err == nil || report.Error == "" {
		t.Fatalf("a failed maintainer run passed: %+v", report)
	}
	if st := f.state(t); st.Cursor != 0 || st.Dirty != 1 || st.Pending != "failed" || st.LastError == "" {
		t.Fatalf("state after a failed pass %+v", st)
	}

	answer := f.maintainer.respond
	f.engine.cognition.MaxSteps = 2
	f.maintainer.respond = func(int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return cogStep("r", "read_thread", `{"id":"`+a+`"}`), nil
	}
	if report, err := f.engine.MaintainCognition(ctx); err == nil || !strings.Contains(report.Error, "maintainer run") {
		t.Fatalf("a maintainer over its step budget passed: %+v %v", report, err)
	}
	if st := f.state(t); st.Cursor != 0 || st.Dirty != 1 || st.Pending != "failed" {
		t.Fatalf("state after an exhausted budget %+v", st)
	}

	f.engine.cognition.MaxSteps = 0
	f.maintainer.respond = answer
	f.answer = func(refs []string) string {
		return candidates(factJSON("service port", []string{refOf(t, refs, a)}, nil, portFact("8080")))
	}
	if report := f.maintain(t); report.Qualified != 1 {
		t.Fatalf("retry report %+v", report)
	}
}

// TestCognitionMaintainerToolBoundary: the maintainer is offered read_thread only. Every other tool it
// calls (shell, file write, DAP, MCP, Eval, subagents, cognition reads) is refused by the loop, nothing
// in the workspace changes, and the pass completes with Core deciding the candidate.
func TestCognitionMaintainerToolBoundary(t *testing.T) {
	f := newCognitionFixture(t, true)
	f.engine.cognition.MaxSteps = 12
	a := f.run(t, "find the port", cogAnswer("8080"))
	attempts := [][2]string{
		{"bash", `{"command":"touch pwned"}`},
		{"write_file", `{"path":"pwned","content":"x"}`},
		{"debug_start", `{"adapter_id":"lldb"}`},
		{"mcp__helper__inspect", `{}`},
		{"eval_create", `{"runtime_id":"python"}`},
		{"spawn_subagent", `{"goal":"write pwned"}`},
		{"read_knowledge", `{}`},
		{"read_thread", `{"id":"` + a + `"}`},
	}
	f.maintainer.respond = func(n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n < len(attempts) {
			return cogStep(fmt.Sprintf("m%d", n), attempts[n][0], attempts[n][1]), nil
		}
		return cogFinal(candidates(factJSON("service port", []string{refOf(t, batchRefs(req), a)}, nil, portFact("8080")))), nil
	}
	if report := f.maintain(t); report.Qualified != 1 {
		t.Fatalf("report %+v", report)
	}
	last := f.maintainer.request(t, len(attempts))
	for i, at := range attempts[:len(attempts)-1] {
		if got := lastToolResult(last, fmt.Sprintf("m%d", i)); !strings.Contains(got, "not allowed") {
			t.Errorf("the maintainer's %s call was not refused: %q", at[0], got)
		}
	}
	if got := lastToolResult(last, fmt.Sprintf("m%d", len(attempts)-1)); !strings.Contains(got, "8080") {
		t.Errorf("read_thread of the source: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.root, "pwned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the maintainer changed the workspace: %v", err)
	}
}

// TestCognitionLeaseTakeoverAcrossEngines: two Engines on one project share one maintenance authority,
// the database. While the first holds the lease the second is refused; once the lease has expired (its
// holder stuck or gone; the database cannot tell the two apart) the second takes over and commits, and
// the first, when it comes back, commits nothing and marks nothing failed: one candidate, one cursor
// move.
func TestCognitionLeaseTakeoverAcrossEngines(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	a := f.run(t, "find the port", cogAnswer("8080"))
	propose := func(req protocol.ProviderRequest) protocol.ProviderResponse {
		return cogFinal(candidates(factJSON("service port", []string{refOf(t, batchRefs(req), a)}, nil, portFact("8080"))))
	}
	second, err := Open(Options{Workspace: f.root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	second.cognition = &CognitionOptions{Maintainer: bind(second, &cognitionProvider{respond: func(_ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return propose(req), nil
	}})}

	release, entered := make(chan struct{}), make(chan struct{}, 1)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.maintainer.respond = func(_ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		entered <- struct{}{}
		<-release
		return propose(req), nil
	}
	type outcome struct {
		report MaintenanceReport
		err    error
	}
	first := make(chan outcome, 1)
	go func() {
		r, err := f.engine.MaintainCognition(ctx)
		first <- outcome{r, err}
	}()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the first pass did not reach its maintainer")
	}

	if r, err := second.MaintainCognition(ctx); err != nil || !r.Held || r.Ran {
		t.Fatalf("a pass while the lease is held: %+v %v", r, err)
	}
	second.clock = func() time.Time { return time.Now().Add(maintenanceLeaseTTL + time.Minute) }
	taken, err := second.MaintainCognition(ctx)
	if err != nil || !taken.Ran || taken.Qualified != 1 || taken.Generation != 2 {
		t.Fatalf("takeover %+v %v", taken, err)
	}
	once.Do(func() { close(release) })
	var old outcome
	select {
	case old = <-first:
	case <-time.After(20 * time.Second):
		t.Fatal("the first pass did not return")
	}
	if !errors.Is(old.err, session.ErrStaleLease) || len(old.report.Inserted) != 0 {
		t.Fatalf("the old holder: %+v %v", old.report, old.err)
	}
	if st := f.state(t); st.Cursor != 1 || st.Dirty != 1 || st.Generation != 2 || st.Pending != "idle" || st.LastError != "" {
		t.Fatalf("state %+v", st)
	}
	if recs, err := f.engine.store.Candidates(ctx, session.CandidateFilter{Limit: 10}); err != nil || len(recs) != 1 {
		t.Fatalf("candidates %+v %v", recs, err)
	}
}

func (f *cognitionFixture) waitState(t *testing.T, ok func(session.MaintenanceState) bool) session.MaintenanceState {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st := f.state(t)
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance state %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCognitionUnrelatedKnowledgeStaysOutOfTheWay: a Start of a task the knowledge has nothing to do
// with receives at most MaxLocators locators in one bounded row and never a subject or claim; a run
// that does not ask completes without any candidate text reaching its model.
func TestCognitionUnrelatedKnowledgeStaysOutOfTheWay(t *testing.T) {
	f := newCognitionFixture(t, true)
	marker := func(pass, i int) knowledge.Assertion {
		return knowledge.Assertion{Kind: knowledge.AssertPortValue, Target: fmt.Sprintf("marker-%d-%d", pass, i), Value: "8080"}
	}
	for pass := 0; pass < 2; pass++ {
		for i := 0; i < knowledge.MaxCandidates; i++ {
			f.bind(t, marker(pass, i), "port")
		}
	}
	for pass := 0; pass < 2; pass++ {
		src := f.run(t, fmt.Sprintf("survey %d", pass), cogAnswer("notes"))
		f.answer = func(refs []string) string {
			var entries []string
			for i := 0; i < knowledge.MaxCandidates; i++ {
				entries = append(entries, factJSON(fmt.Sprintf("topic-%d-%d", pass, i), []string{refOf(t, refs, src)}, nil, marker(pass, i)))
			}
			return candidates(entries...)
		}
		if report := f.maintain(t); report.Qualified != knowledge.MaxCandidates {
			t.Fatalf("pass %d %+v", pass, report)
		}
	}
	p := cogAnswer("renamed the variable")
	f.run(t, "rename a variable in an unrelated module", p)
	req := p.request(t, 0)
	row := indexRow(req)
	if text := fmt.Sprint(req.Messages); strings.Contains(text, "marker-") || strings.Contains(text, "topic-") {
		t.Fatalf("candidate text reached an unrelated run: %q", row)
	}
}

// TestCognitionOffByDefault: without EnableCognition no source is tracked, no tool or index is offered
// and a pass refuses.
func TestCognitionOffByDefault(t *testing.T) {
	f := newCognitionFixture(t, false)
	p := cogAnswer("8080")
	f.run(t, "find the port", p)
	if slices.Contains(toolNames(p.request(t, 0)), "read_knowledge") || indexRow(p.request(t, 0)) != "" {
		t.Fatal("cognition offered while it is off")
	}
	if _, err := f.engine.MaintainCognition(context.Background()); !errors.Is(err, ErrCognitionDisabled) {
		t.Fatalf("a pass while off: %v", err)
	}
}

// TestCognitionBackgroundMaintenance: with Auto, a top-level run's end starts a pass in the background.
// A main run started while the maintainer is still working completes without waiting for it; the pass
// then finishes, and a second round processes that run. A failing maintainer leaves its sources dirty
// and main runs unaffected. Close waits for the pass (the fixture's cleanup).
func TestCognitionBackgroundMaintenance(t *testing.T) {
	f := newCognitionFixture(t, true)
	f.engine.cognition.Auto = true
	release, entered := make(chan struct{}), make(chan struct{}, 8)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.maintainer.respond = func(n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		entered <- struct{}{}
		<-release
		if refs := batchRefs(req); n == 0 && len(refs) > 0 {
			return cogFinal(candidates(factJSON("service port", refs[:1], nil, portFact("8080")))), nil
		}
		return cogFinal(`{"candidates":[]}`), nil
	}
	f.run(t, "find the port", cogAnswer("8080"))
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("no background pass started")
	}
	start := time.Now()
	f.run(t, "an unrelated task", cogAnswer("done"))
	t.Logf("a main run while the maintainer worked took %s", time.Since(start))
	if st := f.state(t); st.Cursor != 0 || st.Dirty != 2 || st.Pending != "running" {
		t.Fatalf("while the maintainer works %+v", st)
	}
	once.Do(func() { close(release) })
	f.waitState(t, func(st session.MaintenanceState) bool { return st.Dirty == 2 && st.Cursor == 2 && st.Pending == "idle" })
	recs, err := f.engine.store.Candidates(context.Background(), session.CandidateFilter{Statuses: []string{knowledge.StatusQualified}, Limit: 5})
	if err != nil || len(recs) != 1 {
		t.Fatalf("background candidates %+v %v", recs, err)
	}

	f.maintainer.respond = func(int, protocol.ProviderRequest) (protocol.ProviderResponse, error) { return cogFinal(""), nil }
	f.run(t, "a third task", cogAnswer("done"))
	f.waitState(t, func(st session.MaintenanceState) bool { return st.Dirty == 3 && st.Pending == "failed" })
	f.run(t, "a fourth task", cogAnswer("done"))
	st := f.waitState(t, func(st session.MaintenanceState) bool { return st.Dirty == 4 && st.Pending == "failed" })
	if st.Cursor != 2 || st.LastError == "" {
		t.Fatalf("after failing passes %+v", st)
	}
}
