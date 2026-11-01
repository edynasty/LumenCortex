package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// T1-S4 at the loop: which calls are admitted, in what order admission, execution and commit happen,
// and which outcome, coverage and edges a call commits. The real store, snapshots and processes are
// exercised in runtime/execution_test.go.

func TestExecutionTierIsTheFrozenTable(t *testing.T) {
	specs := []protocol.ToolSpec{
		{Name: "mcp__s__exec", Permission: "exec"},
		{Name: "mcp__s__read", Permission: "read"},
		{Name: "mcp__s__write", Permission: "write"},
		{Name: "mcp__s__blank"},
	}
	cases := map[string]executionTier{
		"write_file": tierFiles, "replace_in_file": tierFiles, "apply_patch": tierFiles,
		"shell":        tierWorkspace,
		"lsp_rename":   tierWorkspace,
		"mcp__s__exec": tierWorkspace, "mcp__s__blank": tierWorkspace, "mcp__s__gone": tierWorkspace,
		"mcp__s__read": tierAdmit, "mcp__s__write": tierAdmit,
		"start_task": tierAdmit, "spawn_subagent": tierAdmit,
		"read_file": tierNone, "search_text": tierNone, "find_files": tierNone, "list_dir": tierNone,
		"git_status": tierNone, "git_diff": tierNone, "read_skill": tierNone,
		"read_thread": tierNone, "recall": tierNone, "wait_subagent": tierNone,
		"note_write": tierNone, "note_remove": tierNone, "work_unit_create": tierNone, "reference_suggest": tierNone,
	}
	for name, want := range cases {
		if got := executionTierOf(name, specs); got != want {
			t.Errorf("%s: tier %d, want %d", name, got, want)
		}
	}
}

// tierTools offers a read tool, the file tools, shell and two MCP tools; every call runs fn.
type tierTools struct {
	mu       sync.Mutex
	fn       func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error)
	executed []string
}

func (t *tierTools) Specs([]string) []protocol.ToolSpec {
	return []protocol.ToolSpec{
		{Name: "read_file", Permission: "read"},
		{Name: "write_file", Permission: "write"},
		{Name: "apply_patch", Permission: "write"},
		{Name: "shell", Permission: "exec"},
		{Name: "mcp__s__exec", Permission: "exec"},
		{Name: "mcp__s__read", Permission: "read"},
	}
}

func (t *tierTools) Execute(ctx context.Context, name string, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	t.mu.Lock()
	t.executed = append(t.executed, name)
	t.mu.Unlock()
	if t.fn == nil {
		return protocol.ToolResult{OK: true, Content: "ok"}, nil
	}
	return t.fn(ctx, name, args)
}

// fakeFiles is a workspace of path -> blob for the file tier; a missing path is absent.
type fakeFiles struct {
	mu    sync.Mutex
	blobs map[string]string
	calls int
}

func (f *fakeFiles) SnapshotFiles(_ context.Context, paths []string) (repository.FilesSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	snap := repository.FilesSnapshot{Complete: true}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	for _, p := range sorted {
		blob, ok := f.blobs[p]
		if !ok {
			blob = repository.BlobAbsent
		}
		snap.Files = append(snap.Files, repository.FileState{Path: p, Blob: blob})
	}
	return snap, nil
}

func (f *fakeFiles) SnapshotWorkspace(context.Context) repository.WorkspaceSnapshot {
	return repository.WorkspaceSnapshot{}
}

func (f *fakeFiles) set(path, blob string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs[path] = blob
}

// A side-effect call is admitted, with the request that issued it, before the tool runs; a read-only
// call is not admitted and its row commits without an execution. Each admitted call commits exactly
// one post record.
func TestSideEffectCallIsAdmittedBeforeItRuns(t *testing.T) {
	store := envStore("adm")
	files := &fakeFiles{blobs: map[string]string{}}
	var seenAdmitted []int
	tools := &tierTools{}
	tools.fn = func(_ context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
		store.mu.Lock()
		admitted := 0
		for _, x := range store.executions {
			if x.post == nil {
				admitted++
			}
		}
		seenAdmitted = append(seenAdmitted, admitted)
		store.mu.Unlock()
		if name == "write_file" {
			files.set("a.txt", "blob-a")
		}
		return protocol.ToolResult{OK: true, Content: "ok"}, nil
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		toolStep(call("1", "read_file", `{"path":"a.txt"}`), call("2", "write_file", `{"path":"a.txt","content":"x"}`)),
		toolStep(call("3", "shell", `{"command":"true"}`), call("4", "mcp__s__read", `{}`), call("5", "mcp__s__exec", `{}`)),
		answerOf("done"),
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools, Snapshots: files}
	if _, err := loop.Run(context.Background(), "adm", Options{Session: testRef, MaxSteps: 4, RunID: "run-x"}); err != nil {
		t.Fatal(err)
	}
	// read_file runs with nothing admitted; every other call runs with exactly its own admission open.
	if fmt.Sprint(seenAdmitted) != "[0 1 1 1 1]" {
		t.Fatalf("open admissions seen by each tool = %v, want [0 1 1 1 1]", seenAdmitted)
	}
	envs := store.envelopes()
	wantTools := []string{"write_file", "shell", "mcp__s__read", "mcp__s__exec"}
	wantRequest := []string{envs[0].RequestID, envs[1].RequestID, envs[1].RequestID, envs[1].RequestID}
	wantArgs := []string{`{"path":"a.txt","content":"x"}`, `{"command":"true"}`, `{}`, `{}`}
	if len(store.executions) != len(wantTools) {
		t.Fatalf("executions = %d, want %d", len(store.executions), len(wantTools))
	}
	for i, x := range store.executions {
		a := x.admission
		if a.ToolName != wantTools[i] || a.RequestID != wantRequest[i] || a.RunID != "run-x" || a.SessionID != "adm" {
			t.Errorf("execution %d = %+v", i, a)
		}
		if a.ArgsDigest != digestOf([]byte(wantArgs[i])) {
			t.Errorf("execution %d args digest = %s, want the digest of the arguments the model sent", i, a.ArgsDigest)
		}
		if x.post == nil || x.post.Outcome != session.OutcomeComplete || x.post.ExecutionID != a.ExecutionID {
			t.Errorf("execution %d post = %+v", i, x.post)
		}
	}
	// write_file: the pre-state is the target's blob; shell had no work tree; admission-only calls
	// carry no pre-state at all.
	if a := store.executions[0].admission; !strings.HasPrefix(a.PreRef, "files:1:sha256:") || a.PreUnavailable {
		t.Errorf("write_file pre = %q unavailable=%v", a.PreRef, a.PreUnavailable)
	}
	if a := store.executions[1].admission; a.PreRef != "" || !a.PreUnavailable {
		t.Errorf("shell without a work tree: pre = %q unavailable=%v, want unavailable", a.PreRef, a.PreUnavailable)
	}
	if a := store.executions[2].admission; a.PreRef != "" || a.PreUnavailable {
		t.Errorf("read MCP: pre = %q unavailable=%v, want neither", a.PreRef, a.PreUnavailable)
	}
	if p := store.executions[1].post; p.Coverage != session.CoverageUnavailable {
		t.Errorf("shell coverage = %s, want unavailable", p.Coverage)
	}
	if p := store.executions[2].post; p.Coverage != session.CoverageComplete || p.PostRef != "" {
		t.Errorf("read MCP post = %+v", p)
	}
	if len(store.edges) != 1 || store.edges[0].TargetRef != "a.txt@blob-a" || store.edges[0].SourceRef != envs[0].RequestID ||
		store.edges[0].SourceKind != session.SourceRequest || store.edges[0].OriginClass != session.OriginAgentGen || !store.edges[0].LineageComplete {
		t.Fatalf("edges = %+v, want one complete file edge a.txt@blob-a from the first request", store.edges)
	}
	if tools.executed[0] != "read_file" {
		t.Fatalf("executed = %v", tools.executed)
	}
}

// R-1 at tool admission: a fenced session runs nothing and the run ends revoked; a storage failure
// runs nothing and fails closed. No tool row is written either way.
func TestExecutionAdmissionRefusalRunsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		kind string
	}{
		"fenced":  {session.ErrFenced, KindFenced},
		"storage": {errors.New("database is locked"), KindStorage},
	} {
		t.Run(name, func(t *testing.T) {
			store := envStore("ref")
			store.execErr = tc.err
			tools := &tierTools{}
			provider := &scriptedProvider{responses: []protocol.ProviderResponse{
				toolStep(call("1", "shell", `{"command":"rm -rf x"}`)), answerOf("never"),
			}}
			rows := 0
			loop := Loop{Models: testModels(provider), Store: store, Tools: tools}
			result, err := loop.Run(context.Background(), "ref", Options{Session: testRef, MaxSteps: 3})
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			assertExit(t, store, result, StatusFailed, tc.kind)
			for _, m := range store.messages {
				if m.Role == "tool" {
					rows++
				}
			}
			if len(tools.executed) != 0 || rows != 0 || len(provider.seen) != 1 {
				t.Fatalf("tools=%v tool rows=%d requests=%d: nothing may run, be stored or be sent again", tools.executed, rows, len(provider.seen))
			}
		})
	}
}

// R-1 at the commit: a fence that appears while the tool runs keeps the row out, the execution is
// settled by the store, and the run ends without another request.
func TestFencedToolCommitEndsTheRun(t *testing.T) {
	store := envStore("fc")
	store.commitErr = session.ErrFenced
	tools := &tierTools{}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		toolStep(call("1", "shell", `{"command":"true"}`)), answerOf("never"),
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools}
	result, err := loop.Run(context.Background(), "fc", Options{Session: testRef, MaxSteps: 3})
	if !errors.Is(err, session.ErrFenced) {
		t.Fatalf("err = %v, want ErrFenced", err)
	}
	assertExit(t, store, result, StatusFailed, KindFenced)
	if len(store.executions) != 1 || store.executions[0].post == nil || len(provider.seen) != 1 {
		t.Fatalf("executions=%+v requests=%d", store.executions, len(provider.seen))
	}
}

// A cancellation while the tool runs ends the run with the call unanswered, as before, and leaves the
// execution admitted: nothing is committed for it, and Reconcile settles it as outcome_unknown at the
// next start.
func TestCancelledToolLeavesTheExecutionAdmitted(t *testing.T) {
	store := envStore("cx")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tools := &tierTools{fn: func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
		cancel()
		return protocol.ToolResult{OK: true, Content: "partial"}, nil
	}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{toolStep(call("1", "shell", `{"command":"sleep 9"}`))}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools}
	result, _ := loop.Run(ctx, "cx", Options{Session: testRef, MaxSteps: 3})
	assertExit(t, store, result, StatusInterrupted, KindCancelled)
	if len(store.executions) != 1 || store.executions[0].post != nil {
		t.Fatalf("executions = %+v, want one still admitted", store.executions)
	}
	if last := store.messages[len(store.messages)-1]; last.Role == "tool" {
		t.Fatalf("a cancelled call's result was stored: %+v", last)
	}
}

// Outcomes: a returned result is completed or failed by its OK; an error is unknown, because the tool
// may have acted, except a file call whose targets the post-state shows unchanged.
func TestExecutionOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		call    protocol.ToolCall
		act     func(*fakeFiles)
		result  protocol.ToolResult
		err     error
		outcome session.ExecutionOutcome
		edges   []string
	}{
		{"file ok", call("1", "write_file", `{"path":"a"}`), func(f *fakeFiles) { f.set("a", "b1") }, protocol.ToolResult{OK: true}, nil, session.OutcomeComplete, []string{"a@b1"}},
		{"file refused unchanged", call("1", "write_file", `{"path":"a"}`), nil, protocol.ToolResult{}, errors.New("path escapes workspace"), session.OutcomeFailed, nil},
		{"file error but changed", call("1", "write_file", `{"path":"a"}`), func(f *fakeFiles) { f.set("a", "b2") }, protocol.ToolResult{}, errors.New("short write"), session.OutcomeUnknown, []string{"a@b2"}},
		{"patch removes", call("1", "apply_patch", `{"patches":[{"path":"gone","operation":"delete"},{"path":"kept","operation":"update"}]}`), func(f *fakeFiles) {
			f.mu.Lock()
			delete(f.blobs, "gone")
			f.mu.Unlock()
		}, protocol.ToolResult{OK: true}, nil, session.OutcomeComplete, []string{"gone@-"}},
		{"shell nonzero", call("1", "shell", `{"command":"false"}`), nil, protocol.ToolResult{OK: false}, nil, session.OutcomeFailed, nil},
		{"shell error", call("1", "shell", `{"command":"x"}`), nil, protocol.ToolResult{}, errors.New("spawn failed"), session.OutcomeUnknown, nil},
		{"mcp error", call("1", "mcp__s__read", `{}`), nil, protocol.ToolResult{}, errors.New("broken pipe"), session.OutcomeUnknown, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := envStore("oc")
			files := &fakeFiles{blobs: map[string]string{"gone": "g0", "kept": "k0"}}
			tools := &tierTools{fn: func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
				if tc.act != nil {
					tc.act(files)
				}
				return tc.result, tc.err
			}}
			provider := &scriptedProvider{responses: []protocol.ProviderResponse{toolStep(tc.call), answerOf("done")}}
			loop := Loop{Models: testModels(provider), Store: store, Tools: tools, Snapshots: files}
			if _, err := loop.Run(context.Background(), "oc", Options{Session: testRef, MaxSteps: 3}); err != nil {
				t.Fatal(err)
			}
			if len(store.executions) != 1 || store.executions[0].post == nil {
				t.Fatalf("executions = %+v", store.executions)
			}
			post := store.executions[0].post
			if post.Outcome != tc.outcome {
				t.Fatalf("outcome = %s, want %s", post.Outcome, tc.outcome)
			}
			var got []string
			for _, e := range store.edges {
				got = append(got, e.TargetRef)
				if e.LineageComplete != (tc.outcome != session.OutcomeUnknown && post.Coverage == session.CoverageComplete) {
					t.Errorf("edge %s lineage = %v with outcome %s coverage %s", e.TargetRef, e.LineageComplete, post.Outcome, post.Coverage)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.edges) {
				t.Fatalf("edges = %v, want %v", got, tc.edges)
			}
		})
	}
}

// Without snapshots (or with a pre-state that cannot be read) the call still runs, its admission says
// the pre-state is unavailable, and nothing it commits is lineage-complete.
func TestUnavailablePreStateStillRunsAndIsIncomplete(t *testing.T) {
	store := envStore("pu")
	tools := &tierTools{}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{toolStep(call("1", "write_file", `{"path":"a"}`)), answerOf("done")}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools}
	if _, err := loop.Run(context.Background(), "pu", Options{Session: testRef, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	if len(tools.executed) != 1 || len(store.executions) != 1 {
		t.Fatalf("executed=%v executions=%d", tools.executed, len(store.executions))
	}
	x := store.executions[0]
	if !x.admission.PreUnavailable || x.admission.PreRef != "" || x.post.Coverage != session.CoverageUnavailable || len(store.edges) != 0 {
		t.Fatalf("admission=%+v post=%+v edges=%v", x.admission, x.post, store.edges)
	}
}

// stuckSnapshots never finishes a snapshot before its context ends, as a hung git would.
type stuckSnapshots struct{}

func (stuckSnapshots) SnapshotFiles(ctx context.Context, _ []string) (repository.FilesSnapshot, error) {
	<-ctx.Done()
	return repository.FilesSnapshot{}, ctx.Err()
}

func (stuckSnapshots) SnapshotWorkspace(ctx context.Context) repository.WorkspaceSnapshot {
	<-ctx.Done()
	return repository.WorkspaceSnapshot{}
}

// A snapshot that does not finish is cut at snapshotTimeout: the tool still runs, the admission says the
// pre-state is unavailable, the post coverage is unavailable, and the run goes on.
func TestStuckSnapshotIsBoundedAndUnavailable(t *testing.T) {
	old := snapshotTimeout
	snapshotTimeout = 20 * time.Millisecond
	t.Cleanup(func() { snapshotTimeout = old })
	for _, c := range []protocol.ToolCall{call("1", "write_file", `{"path":"a"}`), call("1", "shell", `{"command":"true"}`)} {
		t.Run(c.Name, func(t *testing.T) {
			store := envStore("st")
			tools := &tierTools{}
			provider := &scriptedProvider{responses: []protocol.ProviderResponse{toolStep(c), answerOf("done")}}
			loop := Loop{Models: testModels(provider), Store: store, Tools: tools, Snapshots: stuckSnapshots{}}
			done := make(chan error, 1)
			go func() {
				_, err := loop.Run(context.Background(), "st", Options{Session: testRef, MaxSteps: 3})
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("a stuck snapshot held the run")
			}
			if len(tools.executed) != 1 || len(store.executions) != 1 {
				t.Fatalf("executed=%v executions=%d", tools.executed, len(store.executions))
			}
			x := store.executions[0]
			if !x.admission.PreUnavailable || x.post.Coverage != session.CoverageUnavailable || x.post.Outcome != session.OutcomeComplete || len(store.edges) != 0 {
				t.Fatalf("admission=%+v post=%+v edges=%v", x.admission, x.post, store.edges)
			}
		})
	}
}

// An edge inherits the lineage of the request that issued the call: a request whose envelope the store
// kept incomplete makes every edge of its calls incomplete.
func TestEdgeLineageFollowsTheIssuingRequest(t *testing.T) {
	store := envStore("ln")
	store.incompleteEnvelopes = true
	files := &fakeFiles{blobs: map[string]string{}}
	tools := &tierTools{fn: func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
		files.set("a", "b")
		return protocol.ToolResult{OK: true}, nil
	}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{toolStep(call("1", "write_file", `{"path":"a"}`)), answerOf("done")}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools, Snapshots: files}
	if _, err := loop.Run(context.Background(), "ln", Options{Session: testRef, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	if len(store.edges) != 1 || store.edges[0].LineageComplete {
		t.Fatalf("edges = %+v, want one incomplete edge", store.edges)
	}
}

// Store writes per call: a read-only call is one write (its row), a side-effect call two (admission and
// commit). The snapshot work happens outside both.
func TestStoreWritesPerCall(t *testing.T) {
	for _, tc := range []struct {
		call   protocol.ToolCall
		writes int
	}{
		{call("1", "read_file", `{"path":"a"}`), 1},
		{call("1", "mcp__s__read", `{}`), 2},
		{call("1", "write_file", `{"path":"a"}`), 2},
		{call("1", "shell", `{"command":"true"}`), 2},
	} {
		t.Run(tc.call.Name, func(t *testing.T) {
			store := &countingStore{memoryStore: envStore("w")}
			provider := &scriptedProvider{responses: []protocol.ProviderResponse{toolStep(tc.call), answerOf("done")}}
			loop := Loop{Models: testModels(provider), Store: store, Tools: &tierTools{}, Snapshots: &fakeFiles{blobs: map[string]string{}}}
			if _, err := loop.Run(context.Background(), "w", Options{Session: testRef, MaxSteps: 3}); err != nil {
				t.Fatal(err)
			}
			if store.toolWrites != tc.writes {
				t.Fatalf("tool-path writes = %d, want %d", store.toolWrites, tc.writes)
			}
		})
	}
}

// countingStore counts the writes the tool path makes: AdmitExecution, CommitToolResult and any
// AppendMessage of a tool row.
type countingStore struct {
	*memoryStore
	toolWrites int
}

func (s *countingStore) AdmitExecution(ctx context.Context, a session.ExecutionAdmission) error {
	s.toolWrites++
	return s.memoryStore.AdmitExecution(ctx, a)
}

func (s *countingStore) CommitToolResult(ctx context.Context, c ToolResultCommit) (int64, int64, error) {
	s.toolWrites++
	return s.memoryStore.CommitToolResult(ctx, c)
}

func (s *countingStore) AppendMessage(ctx context.Context, id string, m protocol.Message, change *TaskStateChange) (int64, int64, error) {
	if m.Role == "tool" {
		s.toolWrites++
	}
	return s.memoryStore.AppendMessage(ctx, id, m, change)
}
