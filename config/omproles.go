package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// The oh-my-pi model roles are one-time input for the presets, never a second model-routing
// system: OMPRoleModels converts them into plain LC model lists and nothing reads OMP at run time.

// OMPAgentDir returns the oh-my-pi agent directory: PI_CODING_AGENT_DIR when it is set to an
// absolute path, else ~/.omp/agent. A relative override is ignored, because it would mean a
// different directory for every process working directory. The session importer uses it to find
// <dir>/sessions.
func OMPAgentDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); dir != "" && filepath.IsAbs(dir) {
		return filepath.Clean(dir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("oh-my-pi agent directory: %w", err)
	}
	return filepath.Join(home, ".omp", "agent"), nil
}

// OMPConfigPath returns <OMPAgentDir>/config.yml, or "" when the home directory is unknown.
func OMPConfigPath() string {
	dir, err := OMPAgentDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "config.yml")
}

// ompEfforts maps an oh-my-pi thinking level onto LC's effort. A level absent from the table is
// unknown: the candidate keeps no effort and the caller reports a warning.
var ompEfforts = map[string]Effort{
	"auto":    EffortAuto,
	"off":     EffortNone,
	"none":    EffortNone,
	"minimal": EffortLow,
	"low":     EffortLow,
	"medium":  EffortMedium,
	"high":    EffortHigh,
	"xhigh":   EffortMax,
	"max":     EffortMax,
}

// OMPRoleModels reads the modelRoles of an oh-my-pi config.yml and converts each role into LC model
// candidates. A role is a string or a list of strings, each "provider/model[:effort]".
//
// Candidates keep only models of eff's merged catalog (one warning per dropped reference), are
// deduplicated, capped at MaxAgentModels, and lose leading candidates that cannot call tools: an
// agent's first model must. A missing file is an empty map and no error, so a machine without
// oh-my-pi still gets the presets. A malformed file is an error; its problems are not guesses.
func OMPRoleModels(path string, eff *Effective) (map[string][]AgentModel, []string, error) {
	out := map[string][]AgentModel{}
	raw, err := readBounded(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var doc struct {
		ModelRoles map[string]any `yaml:"modelRoles"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("not valid YAML: %w", err)
	}
	catalog := map[ModelRef]ModelInfo{}
	if eff != nil {
		for _, m := range eff.Catalog {
			catalog[m.Ref] = m
		}
	}
	var warnings []string
	for _, role := range sortedKeys(doc.ModelRoles) {
		entries, ok := ompRoleEntries(doc.ModelRoles[role])
		if !ok {
			warnings = append(warnings, fmt.Sprintf("modelRoles.%s is not a string or a list of strings; ignored", role))
			continue
		}
		models := convertOMPRole(role, entries, catalog, eff != nil, &warnings)
		if len(models) > 0 {
			out[role] = models
		}
	}
	return out, warnings, nil
}

// ompRoleEntries reads one modelRoles value: a string, a list of strings, or nothing.
func ompRoleEntries(value any) ([]string, bool) {
	switch v := value.(type) {
	case nil:
		return nil, true
	case string:
		return []string{v}, true
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}

// convertOMPRole turns one role's entries into candidates in the order written.
func convertOMPRole(role string, entries []string, catalog map[ModelRef]ModelInfo, haveCatalog bool, warnings *[]string) []AgentModel {
	out := make([]AgentModel, 0, len(entries))
	seen := map[ModelRef]bool{}
	for _, entry := range entries {
		ref, effort, hadEffort, knownEffort := parseOMPModel(entry)
		if hadEffort && !knownEffort {
			*warnings = append(*warnings, fmt.Sprintf("modelRoles.%s: unknown effort in %q; %s keeps no effort", role, entry, ref))
		}
		if _, _, ok := ref.Split(); !ok {
			*warnings = append(*warnings, fmt.Sprintf("modelRoles.%s: %q is not provider/model; dropped", role, entry))
			continue
		}
		if haveCatalog {
			if _, ok := catalog[ref]; !ok {
				*warnings = append(*warnings, fmt.Sprintf("modelRoles.%s: model %s is not in the provider catalog; dropped", role, ref))
				continue
			}
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, AgentModel{Model: ref, Effort: effort})
	}
	if len(out) > MaxAgentModels {
		*warnings = append(*warnings, fmt.Sprintf("modelRoles.%s lists %d models; keeping the first %d", role, len(out), MaxAgentModels))
		out = out[:MaxAgentModels]
	}
	if !haveCatalog {
		return out
	}
	// An agent's first model must call tools; the runtime skips a later one that cannot.
	for len(out) > 0 && !catalog[out[0].Model].ToolCalling {
		*warnings = append(*warnings, fmt.Sprintf("modelRoles.%s: %s does not call tools and cannot lead the list; dropped", role, out[0].Model))
		out = out[1:]
	}
	return out
}

// parseOMPModel splits "provider/model[:effort]". A trailing colon only counts as an effort when
// what precedes it is a provider/model reference, so a model key containing ':' survives.
func parseOMPModel(raw string) (ref ModelRef, effort Effort, hadEffort, knownEffort bool) {
	raw = strings.TrimSpace(raw)
	i := strings.LastIndex(raw, ":")
	if i < 0 {
		return ModelRef(raw), "", false, false
	}
	base, suffix := raw[:i], strings.ToLower(strings.TrimSpace(raw[i+1:]))
	if _, _, ok := ModelRef(base).Split(); !ok {
		return ModelRef(raw), "", false, false
	}
	e, known := ompEfforts[suffix]
	return ModelRef(base), e, true, known
}
