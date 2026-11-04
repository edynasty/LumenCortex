package protocol

// ToolBash is the model-visible name of the command tool (ADR-0019 §0). Before ADR-0019 it was "shell".
const ToolBash = "bash"

// renamedTools maps a tool name the runtime no longer offers to the tool that replaced it under the
// same arguments. A host list written before the rename keeps its meaning through it: a denylist that
// names "shell" denies "bash", an allowlist that names it admits "bash".
var renamedTools = map[string]string{"shell": ToolBash}

// ReplacedToolName is the tool that replaced name; ok is false when name was not renamed.
func ReplacedToolName(name string) (replacement string, ok bool) {
	replacement, ok = renamedTools[name]
	return replacement, ok
}

// CanonicalToolName is name, or the tool that replaced it.
func CanonicalToolName(name string) string {
	if replacement, ok := renamedTools[name]; ok {
		return replacement
	}
	return name
}

// CanonicalToolNames maps every name of a host tool list (allowlist, denylist, a workflow's allowed
// tools) to its current name, keeping the first occurrence of each. nil stays nil: an absent list and
// an empty one mean different things. A list with no renamed name is returned as is.
func CanonicalToolNames(names []string) []string {
	renamed := false
	for _, name := range names {
		if _, ok := renamedTools[name]; ok {
			renamed = true
			break
		}
	}
	if !renamed {
		return names
	}
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name = CanonicalToolName(name)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}
