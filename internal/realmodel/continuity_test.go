//go:build realmodel

// The continuity evaluation (ADR-0007 §3, ADR-0010 D6) on the one authorised model:
//
//	LCX_REAL_MODEL=local/deepseek-v4.1-flash LCX_EVAL_REPEATS=3 LCX_EVAL_OUT=/tmp/b3.json \
//	  go test -tags realmodel -count=1 -run RealModelContinuity -v -timeout 6h ./internal/realmodel
//
// Each repeat builds one workspace history by running earlier threads (A) on the real model: they
// hear facts and decisions from the user that do not land in the repository. The history is then
// copied once per (scenario, arm), and a new thread (B) runs a task that needs those facts:
//
//   - arm 1, "window": window + Task State + notes; recall and read_thread denied.
//   - arm 2, "recall": the same plus recall and read_thread.
//
// Every B result is graded by deterministic checks on files and build output, never by a model.
package realmodel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
	lcx "github.com/edynasty/LumenCortex/runtime"
)

var continuityFixture = map[string]string{
	"go.mod":            "module example.com/svc\n\ngo 1.22\n",
	"README.md":         "# svc\n\nA small service: an in-memory cache, metrics, a duration parser and deploy scripts.\n",
	"scripts/README.md": "Deploy and release scripts live here.\n",
	"scripts/deploy_prod.sh": `#!/bin/sh
# Uploads the build to production.
set -eu
curl -fsS -H "Authorization: Bearer $PROD_TOKEN" -T build/svc.tar.gz https://prod.example.com/upload
`,
	"cache/cache.go": `package cache

// Cache is a string cache. Refresh reloads entries through Get.
type Cache struct {
	items map[string]string
	load  func(string) string
}

func New(load func(string) string) *Cache { return &Cache{items: map[string]string{}, load: load} }

func (c *Cache) Get(key string) (string, bool) {
	v, ok := c.items[key]
	return v, ok
}

func (c *Cache) Set(key, value string) { c.items[key] = value }

// Refresh reloads every key through Get and load.
func (c *Cache) Refresh() {
	for key := range c.items {
		if _, ok := c.Get(key); ok {
			c.Set(key, c.load(key))
		}
	}
}
`,
	"metrics/registry.go": `package metrics

import "sync"

var (
	mu       sync.Mutex
	counters = map[string]*Counter{}
)

// Counter is a monotonically increasing count.
type Counter struct {
	mu sync.Mutex
	n  int64
}

func (c *Counter) Inc() { c.mu.Lock(); c.n++; c.mu.Unlock() }

// Register returns the counter with the given name, creating it once.
func Register(name string) *Counter {
	mu.Lock()
	defer mu.Unlock()
	if c, ok := counters[name]; ok {
		return c
	}
	c := &Counter{}
	counters[name] = c
	return c
}
`,
	"metrics/http.go": `package metrics

// RequestsTotal counts HTTP requests.
var RequestsTotal = Register("http_requests")
`,
	"mathx/mathx.go": "package mathx\n\n// Abs returns |v|.\nfunc Abs(v int) int {\n\tif v < 0 {\n\t\treturn -v\n\t}\n\treturn v\n}\n",
}

// historyThread is one earlier thread: its turns, as the user typed them.
type historyThread struct {
	name  string
	turns []string
}

// The history every repeat builds, oldest first. The facts sit in the last turn of the two-turn
// threads, so after the thread they survive only in its notes or final answer, not its goal.
var history = []historyThread{
	{"deploy", []string{
		"Let's look at how deployment works in this repo. What do the scripts under scripts/ do?",
		"Thanks. For the record, since nobody wrote it down: staging is the host stg-7.internal (HTTPS, path /upload, same as prod), and its token is in the environment variable LCX_STAGING_TOKEN, not STAGING_TOKEN as the old wiki says. There is no staging script yet; we'll add one later. Nothing to change now.",
	}},
	{"cache", []string{
		"I need context on cache/cache.go before we make it concurrency-safe. Background: last sprint we tried guarding Get and Set with one sync.Mutex and it deadlocked, because Refresh calls Get and Set while iterating, and Refresh itself needed the lock. We decided the fix is sync.Map instead of a mutex. Read the file and tell me whether that decision still fits the current code. Don't edit anything yet.",
	}},
	{"metrics", []string{
		"Quick question about metrics/: how are counters registered today?",
		"Good. The SRE team and I settled the naming convention this morning: every counter is named lumen_<area>_<what>_total, for example lumen_http_requests_total, and is registered with metrics.Register. The existing http_requests name will be renamed in a separate change; don't touch it now.",
	}},
	{"release-old", []string{
		"Release branches in this project are named release/vYYYY.MM, e.g. release/v2026.09. Is there anything in the repo that creates release branches yet? Just check, no changes.",
	}},
	{"dashboard", []string{
		"Unrelated: the team dashboard colours are blue for prod and amber for staging. Does the repo contain any dashboard config? Just look.",
	}},
	{"release-new", []string{
		"Update on releases: we changed the convention today. Release branches are now named rel-<semver>, for example rel-1.4.0. The old release/vYYYY.MM naming is retired. Is there anything in the repo that depends on the old naming? Just check.",
	}},
	{"parser-idea", []string{
		"Idea for later, don't implement: a ParseDuration helper in a new package durx that accepts a 'd' suffix for days. Does anything like it exist? Just look.",
	}},
}

// continuityTask is one B thread: its prompt and the grader of its outcome.
type continuityTask struct {
	name, prompt string
	// relevant names the history threads whose content the task needs (labels for precision).
	relevant []string
	grade    func(t *testing.T, dir, final string) (bool, string)
}

func readAll(dir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".lumencortex") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			raw, _ := os.ReadFile(path)
			rel, _ := filepath.Rel(dir, path)
			fmt.Fprintf(&b, "\n=== %s\n%s", rel, raw)
		}
		return nil
	})
	return b.String()
}

func goBuild(dir string) (bool, string) {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return err == nil, strings.TrimSpace(string(out))
}

func fileText(dir, name string) string {
	raw, _ := os.ReadFile(filepath.Join(dir, name))
	return string(raw)
}

var continuityTasks = []continuityTask{
	{
		name:     "deploy",
		prompt:   "Write scripts/deploy_staging.sh: it uploads build/svc.tar.gz to the staging environment, like the prod script does for prod.",
		relevant: []string{"deploy"},
		grade: func(t *testing.T, dir, _ string) (bool, string) {
			s := fileText(dir, "scripts/deploy_staging.sh")
			ok := strings.Contains(s, "stg-7.internal") && strings.Contains(s, "LCX_STAGING_TOKEN")
			return ok, fmt.Sprintf("host=%v token=%v", strings.Contains(s, "stg-7.internal"), strings.Contains(s, "LCX_STAGING_TOKEN"))
		},
	},
	{
		name:     "cache",
		prompt:   "Make cache.Cache in cache/cache.go safe for concurrent use.",
		relevant: []string{"cache"},
		grade: func(t *testing.T, dir, _ string) (bool, string) {
			s := fileText(dir, "cache/cache.go")
			built, out := goBuild(dir)
			syncMap := strings.Contains(s, "sync.Map")
			mutex := regexp.MustCompile(`sync\.(RW)?Mutex`).MatchString(s)
			return built && syncMap && !mutex, fmt.Sprintf("build=%v syncMap=%v mutex=%v %s", built, syncMap, mutex, firstLine(out))
		},
	},
	{
		name:     "metrics",
		prompt:   "Add a counter for cache misses, following our metrics naming convention, and increment it in Cache.Get when a key is missing.",
		relevant: []string{"metrics"},
		grade: func(t *testing.T, dir, _ string) (bool, string) {
			all := readAll(dir)
			built, out := goBuild(dir)
			named := regexp.MustCompile(`lumen_cache_miss(es)?_total`).MatchString(all)
			return built && named, fmt.Sprintf("build=%v named=%v %s", built, named, firstLine(out))
		},
	},
	{
		name:     "release",
		prompt:   "Write scripts/cut_release.sh that creates and pushes the release branch for version 1.5.0 from main.",
		relevant: []string{"release-new", "release-old"},
		grade: func(t *testing.T, dir, _ string) (bool, string) {
			s := fileText(dir, "scripts/cut_release.sh")
			current := strings.Contains(s, "rel-1.5.0") || strings.Contains(s, "rel-$") || strings.Contains(s, "rel-${")
			retired := strings.Contains(s, "release/v")
			return current && !retired, fmt.Sprintf("current=%v retired=%v", current, retired)
		},
	},
	{
		name:   "control",
		prompt: "Add func Clamp(v, lo, hi int) int to mathx/mathx.go, with a table test in mathx/mathx_test.go.",
		grade: func(t *testing.T, dir, _ string) (bool, string) {
			cmd := exec.Command("go", "test", "./mathx")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			has := strings.Contains(fileText(dir, "mathx/mathx.go"), "func Clamp(")
			return err == nil && has, fmt.Sprintf("test=%v clamp=%v %s", err == nil, has, firstLine(string(out)))
		},
	},
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// evalRecord is one B run.
type evalRecord struct {
	Repeat     int      `json:"repeat"`
	Task       string   `json:"task"`
	Arm        string   `json:"arm"`
	Success    bool     `json:"success"`
	Detail     string   `json:"detail"`
	Status     string   `json:"status"`
	Requests   int64    `json:"requests"`
	Prompt     int64    `json:"promptTokens"`
	Completion int64    `json:"completionTokens"`
	Recalls    int      `json:"recalls"`
	Reads      int      `json:"readThreads"`
	Tools      []string `json:"tools"`
	// RelevantRank is the best 1-based rank of a relevant history thread across the run's recall
	// results (0: none returned); RecalledIrrelevant counts irrelevant hits above it.
	RelevantRank       int     `json:"relevantRank"`
	PrecisionAt3       float64 `json:"precisionAt3"`
	ReadRelevant       bool    `json:"readRelevant"`
	Seconds            float64 `json:"seconds"`
	HistoryNotes       int     `json:"historyNotes"`
	RelevantNotes      int     `json:"relevantNotes"`
	RelevantNoteHasKey bool    `json:"relevantNoteHasKey"`
}

func TestRealModelContinuity(t *testing.T) {
	repeats := 2
	if v := os.Getenv("LCX_EVAL_REPEATS"); v != "" {
		fmt.Sscan(v, &repeats)
	}
	only := os.Getenv("LCX_EVAL_TASKS")
	var records []evalRecord
	for rep := 0; rep < repeats; rep++ {
		base := fixtureRepo(t, continuityFixture)
		parent := filepath.Dir(base)
		var arms []string
		for _, task := range continuityTasks {
			for _, arm := range []string{"window", "recall"} {
				arms = append(arms, filepath.Join(parent, task.name+"-"+arm))
			}
		}
		factory := realModelEnv(t, nil, append([]string{base}, arms...)...)
		ids, notes := buildHistory(t, base, factory)
		for _, task := range continuityTasks {
			if only != "" && !strings.Contains(","+only+",", ","+task.name+",") {
				continue
			}
			for _, arm := range []string{"window", "recall"} {
				dir := filepath.Join(parent, task.name+"-"+arm)
				if out, err := exec.Command("cp", "-a", base, dir).CombinedOutput(); err != nil {
					t.Fatalf("copy history: %v %s", err, out)
				}
				rec := runContinuityTask(t, dir, factory, task, arm, ids)
				rec.Repeat = rep
				rec.HistoryNotes = notes.total
				for _, name := range task.relevant {
					rec.RelevantNotes += notes.byThread[name]
					rec.RelevantNoteHasKey = rec.RelevantNoteHasKey || notes.hasKey[name]
				}
				records = append(records, rec)
				t.Logf("rep=%d task=%s arm=%s success=%v (%s) status=%s req=%d prompt=%d recalls=%d reads=%d rank=%d tools=%v",
					rep, task.name, arm, rec.Success, rec.Detail, rec.Status, rec.Requests, rec.Prompt, rec.Recalls, rec.Reads, rec.RelevantRank, rec.Tools)
			}
		}
	}
	summarize(t, records)
	if out := os.Getenv("LCX_EVAL_OUT"); out != "" {
		raw, _ := json.MarshalIndent(records, "", "  ")
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type historyNotes struct {
	total    int
	byThread map[string]int
	hasKey   map[string]bool
}

// historyKeys is the fact each history thread's notes would need to carry to be useful.
var historyKeys = map[string]*regexp.Regexp{
	"deploy":      regexp.MustCompile(`(?i)stg-7\.internal|LCX_STAGING_TOKEN`),
	"cache":       regexp.MustCompile(`(?i)sync\.Map`),
	"metrics":     regexp.MustCompile(`(?i)lumen_`),
	"release-new": regexp.MustCompile(`(?i)rel-`),
	"release-old": regexp.MustCompile(`(?i)release/v`),
}

// buildHistory runs every history thread to completion, oldest first, and returns their session ids
// by name and what notes they wrote.
func buildHistory(t *testing.T, repo string, factory lcx.ProviderFactory) (map[string]string, historyNotes) {
	t.Helper()
	ctx := context.Background()
	engine, err := lcx.OpenContext(ctx, lcx.Options{Workspace: repo, ProviderFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	supervisor, err := lcx.NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	ids := map[string]string{}
	notes := historyNotes{byThread: map[string]int{}, hasKey: map[string]bool{}}
	for _, h := range history {
		handle, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: h.turns[0]})
		if err != nil {
			t.Fatal(err)
		}
		for i, turn := range h.turns {
			opts := lcx.AgentOptions{Interactive: true, Effort: "low", MaxSteps: 10}
			if i > 0 {
				opts.Input = turn
			}
			if _, err := supervisor.Start(ctx, handle.ID, opts); err != nil {
				t.Fatal(err)
			}
			waitIdle(t, supervisor, handle.ID, 6*time.Minute)
		}
		ids[h.name] = handle.ID
		info, err := engine.TaskState(ctx, handle.ID)
		if err != nil {
			t.Fatal(err)
		}
		notes.total += len(info.State.Notes)
		notes.byThread[h.name] = len(info.State.Notes)
		for _, n := range info.State.Notes {
			if key := historyKeys[h.name]; key != nil && key.MatchString(n.Text) {
				notes.hasKey[h.name] = true
			}
		}
		t.Logf("history %s: %s notes=%d key=%v", h.name, handle.ID, len(info.State.Notes), notes.hasKey[h.name])
		for _, n := range info.State.Notes {
			t.Logf("  note [%s] %s", n.By, n.Text)
		}
	}
	return ids, notes
}

func waitIdle(t *testing.T, supervisor *lcx.RunSupervisor, id string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for supervisor.Active(id) {
		if time.Now().After(deadline) {
			supervisor.Cancel(id)
			t.Fatalf("run %s still active after %v", id, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func runContinuityTask(t *testing.T, dir string, factory lcx.ProviderFactory, task continuityTask, arm string, ids map[string]string) evalRecord {
	t.Helper()
	ctx := context.Background()
	engine, err := lcx.OpenContext(ctx, lcx.Options{Workspace: dir, ProviderFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	supervisor, err := lcx.NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	handle, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: task.prompt})
	if err != nil {
		t.Fatal(err)
	}
	// Thread B makes the change and may build and test it; the fixture is a private temp repo.
	opts := lcx.AgentOptions{Interactive: true, Effort: "low", MaxSteps: 16, Policy: "full"}
	if arm == "window" {
		opts.ToolDenylist = []string{"recall", "read_thread"}
	}
	started := time.Now()
	if _, err := supervisor.Start(ctx, handle.ID, opts); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, supervisor, handle.ID, 10*time.Minute)
	_, info, err := engine.Session(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec := evalRecord{Task: task.name, Arm: arm, Status: info.Status, Seconds: time.Since(started).Seconds()}
	var usage protocol.Usage
	_ = json.Unmarshal(info.Usage, &usage)
	rec.Requests, rec.Prompt, rec.Completion = usage.Requests, usage.PromptTokens, usage.CompletionTokens
	relevant := map[string]bool{}
	for _, name := range task.relevant {
		relevant[ids[name]] = true
	}
	page, err := engine.MessagePage(ctx, handle.ID, -1, 200)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]string{}
	for _, m := range page.Messages {
		var msg protocol.Message
		if json.Unmarshal(m.JSON, &msg) != nil {
			continue
		}
		for _, c := range msg.ToolCalls {
			calls[c.ID] = c.Name
			rec.Tools = append(rec.Tools, c.Name)
			if c.Name == "read_thread" {
				var args struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(c.Arguments, &args)
				rec.ReadRelevant = rec.ReadRelevant || relevant[args.ID]
			}
		}
		if msg.Role != "tool" || calls[msg.ToolCallID] != "recall" {
			continue
		}
		rec.Recalls++
		var out struct {
			Hits []struct {
				ID string `json:"id"`
			} `json:"hits"`
		}
		if json.Unmarshal([]byte(msg.Content), &out) != nil {
			continue
		}
		top := 0
		for i, h := range out.Hits {
			if relevant[h.ID] {
				if rec.RelevantRank == 0 || i+1 < rec.RelevantRank {
					rec.RelevantRank = i + 1
				}
				if i < 3 {
					top++
				}
			}
		}
		if n := min(3, len(out.Hits)); n > 0 {
			rec.PrecisionAt3 = max(rec.PrecisionAt3, float64(top)/float64(n))
		}
	}
	for _, c := range rec.Tools {
		if c == "read_thread" {
			rec.Reads++
		}
	}
	final := ""
	if info.Final != nil {
		final = *info.Final
	}
	rec.Success, rec.Detail = task.grade(t, dir, final)
	return rec
}

// summarize logs the per-arm and per-task success, cost and retrieval figures the ADR-0010 D6 gate
// reads.
func summarize(t *testing.T, records []evalRecord) {
	type agg struct {
		n, ok, recalls, used int
		prompt, requests     int64
		rank1, inTop3        int
	}
	byArm := map[string]*agg{}
	byTaskArm := map[string]*agg{}
	for _, r := range records {
		for key, m := range map[string]map[string]*agg{r.Arm: byArm, r.Task + "/" + r.Arm: byTaskArm} {
			a := m[key]
			if a == nil {
				a = &agg{}
				m[key] = a
			}
			a.n++
			if r.Success {
				a.ok++
			}
			a.prompt += r.Prompt
			a.requests += r.Requests
			a.recalls += r.Recalls
			if r.Recalls > 0 {
				a.used++
			}
			if r.RelevantRank == 1 {
				a.rank1++
			}
			if r.RelevantRank >= 1 && r.RelevantRank <= 3 {
				a.inTop3++
			}
		}
	}
	keys := make([]string, 0, len(byTaskArm))
	for k := range byTaskArm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := byTaskArm[k]
		t.Logf("%-16s success %d/%d  avg prompt %6d  avg requests %.1f  recall used %d/%d  relevant@1 %d  @3 %d",
			k, a.ok, a.n, a.prompt/int64(a.n), float64(a.requests)/float64(a.n), a.used, a.n, a.rank1, a.inTop3)
	}
	for _, arm := range []string{"window", "recall"} {
		if a := byArm[arm]; a != nil {
			t.Logf("ARM %-7s success %d/%d  avg prompt tokens %d  avg requests %.1f", arm, a.ok, a.n, a.prompt/int64(a.n), float64(a.requests)/float64(a.n))
		}
	}
}
