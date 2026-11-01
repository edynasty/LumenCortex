package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every preset is ordinary configuration once saved: the catalog must accept it as written.
func TestAgentPresetsAreValidCatalogEntries(t *testing.T) {
	presets := AgentPresets()
	if len(presets) != 4 {
		t.Fatalf("presets = %d, want 4", len(presets))
	}
	file := File{Agents: map[string]Agent{}}
	for _, preset := range presets {
		if preset.ID == "" || preset.Agent.Name == "" || preset.Agent.Description == "" {
			t.Fatalf("preset %+v has no id, name or description", preset)
		}
		if IsBuiltinAgent(preset.ID) {
			t.Fatalf("preset %q reuses a built-in id", preset.ID)
		}
		if len(preset.Agent.Description) > MaxAgentDescriptionSize {
			t.Fatalf("preset %q description is %d bytes", preset.ID, len(preset.Agent.Description))
		}
		if len(preset.Agent.Instructions) > MaxAgentInstructions {
			t.Fatalf("preset %q instructions are %d bytes", preset.ID, len(preset.Agent.Instructions))
		}
		if strings.TrimSpace(preset.Agent.Instructions) == "" {
			t.Fatalf("preset %q has empty instructions", preset.ID)
		}
		if preset.Agent.Models != nil {
			t.Fatalf("preset %q ships models: %v", preset.ID, preset.Agent.Models)
		}
		file.Agents[preset.ID] = preset.Agent
	}
	if err := Validate(file); err != nil {
		t.Fatalf("presets are not a valid catalog: %v", err)
	}
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatalf("presets do not round-trip through Parse: %v", err)
	}
}

// Callers own the copies: filling one preset's models must not change the next call.
func TestAgentPresetsReturnsFreshCopies(t *testing.T) {
	first := AgentPresets()
	first[0].Agent.Models = []AgentModel{{Model: "test/a"}}
	first[0].Agent.Instructions = "changed"
	second := AgentPresets()
	if second[0].Agent.Models != nil {
		t.Fatalf("models leaked between calls: %v", second[0].Agent.Models)
	}
	if second[0].Agent.Instructions == "changed" {
		t.Fatal("instructions leaked between calls")
	}
}

// No preset tells the agent to use a tool the runtime no longer offers (RT-capability-1): git_status and
// git_diff were removed and shell renamed bash by ADR-0019 §0, so a preset that names one sends the
// model after a tool it does not have.
func TestAgentPresetsNameNoRemovedTool(t *testing.T) {
	for _, preset := range AgentPresets() {
		for _, gone := range []string{"git_status", "git_diff", "`shell`"} {
			if strings.Contains(preset.Agent.Instructions, gone) {
				t.Errorf("preset %q names %s, removed by ADR-0019 §0", preset.ID, gone)
			}
		}
	}
}

// ompTestEffective is a catalog of four usable models and one that cannot call tools.
func ompTestEffective() *Effective {
	info := func(ref ModelRef, tools bool) ModelInfo {
		return ModelInfo{Ref: ref, ToolCalling: tools}
	}
	return &Effective{Catalog: []ModelInfo{
		info("test/a", true), info("test/b", true), info("test/plain", true), info("test/c", false),
	}}
}

func writeOMPConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOMPRoleModelsConvertsRoles(t *testing.T) {
	path := writeOMPConfig(t, `modelRoles:
  slow:
    - test/a:high
    - test/b:xhigh
    - gone/model:max
    - test/a:low
  smol: test/b
  fast: test/plain:weird
  lead:
    - test/c:high
    - test/a:low
  broken: 7
  empty:
`)
	roles, warnings, err := OMPRoleModels(path, ompTestEffective())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]AgentModel{
		"slow": {{Model: "test/a", Effort: EffortHigh}, {Model: "test/b", Effort: EffortXHigh}},
		"smol": {{Model: "test/b"}},
		"fast": {{Model: "test/plain"}},
		"lead": {{Model: "test/a", Effort: EffortLow}},
	}
	for role, models := range want {
		if !slices.Equal(roles[role], models) {
			t.Fatalf("role %s = %v, want %v", role, roles[role], models)
		}
	}
	if _, ok := roles["broken"]; ok {
		t.Fatalf("a non-string role produced models: %v", roles["broken"])
	}
	if _, ok := roles["empty"]; ok {
		t.Fatal("an empty role produced models")
	}
	joined := strings.Join(warnings, "\n")
	for _, expected := range []string{"gone/model", "unknown effort", "test/c does not call tools", "modelRoles.broken"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("warnings %q do not mention %q", joined, expected)
		}
	}
}

func TestOMPRoleModelsCapsTheList(t *testing.T) {
	var b strings.Builder
	b.WriteString("modelRoles:\n  many:\n")
	catalog := make([]ModelInfo, 0, 10)
	for i := range 10 {
		ref := ModelRef("test/m" + string(rune('0'+i)))
		catalog = append(catalog, ModelInfo{Ref: ref, ToolCalling: true})
		b.WriteString("    - " + string(ref) + ":high\n")
	}
	roles, warnings, err := OMPRoleModels(writeOMPConfig(t, b.String()), &Effective{Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(roles["many"]); got != MaxAgentModels {
		t.Fatalf("role kept %d models, want %d", got, MaxAgentModels)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "keeping the first 8") {
		t.Fatalf("warnings do not mention the cap: %v", warnings)
	}
}

func TestOMPRoleModelsMissingFileIsEmpty(t *testing.T) {
	roles, warnings, err := OMPRoleModels(filepath.Join(t.TempDir(), "absent.yml"), ompTestEffective())
	if err != nil || len(warnings) != 0 || len(roles) != 0 {
		t.Fatalf("missing file: roles=%v warnings=%v err=%v", roles, warnings, err)
	}
}

func TestResolveAgentPresetsFillsModelsFromRoles(t *testing.T) {
	path := writeOMPConfig(t, `modelRoles:
  slow: test/a:high
  smol: test/b
`)
	presets, warnings := ResolveAgentPresets(ompTestEffective(), path)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	byID := map[string]AgentPreset{}
	for _, preset := range presets {
		byID[preset.ID] = preset
	}
	if got := byID["reviewer"].Agent.Models; !slices.Equal(got, []AgentModel{{Model: "test/a", Effort: EffortHigh}}) {
		t.Fatalf("reviewer models = %v", got)
	}
	if got := byID["sonic"].Agent.Models; !slices.Equal(got, []AgentModel{{Model: "test/b"}}) {
		t.Fatalf("sonic models = %v", got)
	}
	if got := byID["security-reviewer"].Agent.Models; got != nil {
		t.Fatalf("security-reviewer has no OMP role but got %v", got)
	}
	if got := byID["thread-historian"].Agent.Models; got != nil {
		t.Fatalf("thread-historian has no OMP role but got %v", got)
	}
}

func TestResolveAgentPresetsWithoutOMPConfig(t *testing.T) {
	presets, warnings := ResolveAgentPresets(ompTestEffective(), "")
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	for _, preset := range presets {
		if preset.Agent.Models != nil {
			t.Fatalf("preset %q has models without an OMP config: %v", preset.ID, preset.Agent.Models)
		}
	}
}

func TestOMPAgentDirHonorsTheEnvironment(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "/tmp/omp-agent")
	dir, err := OMPAgentDir()
	if err != nil || dir != "/tmp/omp-agent" {
		t.Fatalf("OMPAgentDir = %q, %v", dir, err)
	}
	if got := OMPConfigPath(); got != filepath.Join("/tmp/omp-agent", "config.yml") {
		t.Fatalf("OMPConfigPath = %q", got)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	// A relative override would mean a different directory per working directory: ignored.
	t.Setenv("PI_CODING_AGENT_DIR", "relative/agent")
	if dir, err := OMPAgentDir(); err != nil || dir != filepath.Join(home, ".omp", "agent") {
		t.Fatalf("relative override: OMPAgentDir = %q, %v", dir, err)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "")
	if got := OMPConfigPath(); got != filepath.Join(home, ".omp", "agent", "config.yml") {
		t.Fatalf("OMPConfigPath = %q", got)
	}
}
