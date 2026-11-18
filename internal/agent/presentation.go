package agent

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

// PresentationOptions is how a model's execution profile presents the same authority: a compact
// tool catalog and shorter failure feedback, or the detailed forms. It never changes a tool's
// permission, parameters, mutating flag or the run's policy: two models with different profiles can
// do exactly the same things.
type PresentationOptions struct {
	// ToolDescriptions is "", config.ProfileCompact or config.ProfileDetailed.
	ToolDescriptions string
	// Feedback is "", config.ProfileCompact or config.ProfileDetailed.
	Feedback string
}

// ProjectionMode is the binary stateless candidate prefix mode (ADR-0024 §4).
type ProjectionMode string

const (
	ModeFull    ProjectionMode = "full"
	ModeCompact ProjectionMode = "compact"
)

const (
	PrefixReasonFitsFull          = "fits_full"
	PrefixReasonCandidateProfile  = "candidate_profile"
	PrefixReasonFixedPrefixBudget = "fixed_prefix_budget"
	PrefixReasonNewestGroupFit    = "newest_group_fit"
)

const (
	// CompactDescriptionBytes bounds generic compact tool descriptions.
	CompactDescriptionBytes = 200
	// CompactFeedbackBytes bounds compact failure feedback UTF-8 bytes (including trailing ellipsis).
	CompactFeedbackBytes = 1200
	// MaxDetailedFeedbackBytes bounds detailed failure feedback UTF-8 bytes (including trailing ellipsis).
	MaxDetailedFeedbackBytes = 6144
	// GateMessageEmptyJSONBytes is len(json.Marshal(protocol.Message{Role:"user", Origin:OriginCompletionBlocked})).
	GateMessageEmptyJSONBytes = 58
	// MaxCompactGateJSONBytes is the hard marshaled JSON byte cap for compact gate feedback (58 + 2*1200).
	MaxCompactGateJSONBytes = 2458
	// MaxDetailedGateJSONBytes is the hard marshaled JSON byte cap for detailed gate feedback (58 + 2*6144).
	MaxDetailedGateJSONBytes = 12346
	// ProposalReserveTokens reserves window capacity for the rejected assistant completion proposal.
	ProposalReserveTokens = 512
)

func effectiveVariant(candidateVariant, fallbackVariant string) string {
	if candidateVariant != "" {
		return candidateVariant
	}
	return fallbackVariant
}

func effectiveFeedbackVariant(m Model, fallback PresentationOptions, mode ProjectionMode) string {
	pref := effectiveVariant(m.Presentation.Feedback, fallback.Feedback)
	if pref == config.ProfileCompact || pref == config.ProfileDetailed {
		return pref
	}
	if mode == ModeCompact {
		return config.ProfileCompact
	}
	return config.ProfileDetailed
}

// presentSpecs applies an explicit profile's description variant when StepTool source metadata is
// unavailable. The schema, permission and mutating flag are copied unchanged.
func presentSpecs(specs []protocol.ToolSpec, variant string) []protocol.ToolSpec {
	if variant != config.ProfileCompact || len(specs) == 0 {
		out := make([]protocol.ToolSpec, len(specs))
		copy(out, specs)
		return out
	}
	out := make([]protocol.ToolSpec, len(specs))
	for i, spec := range specs {
		out[i] = spec
		if strings.HasPrefix(spec.Name, "mcp__") || strings.HasPrefix(spec.Name, "lsp_") {
			out[i].Description = compactExternalToolDescription(spec, spec.Permission == "read")
		} else {
			out[i].Description = compactBuiltinToolDescription(spec)
		}
	}
	return out
}

// presentStepTools produces a fresh candidate-scoped ToolSpec slice without mutating tools.
// Under explicit ProfileCompact, built-in safety invariants (REV-F06) and external fence rules apply.
// Under budget-triggered ModeCompact, builtin/task/delegation and non-read-only external tools retain
// 100% of their original descriptions verbatim, while read-only external MCP/LSP tools fold only
// positively verified pure example fences while preserving (MCP server: <id>).
func presentStepTools(tools []StepTool, profileVariant string, mode ProjectionMode) []protocol.ToolSpec {
	out := make([]protocol.ToolSpec, len(tools))
	for i, entry := range tools {
		spec := entry.Spec
		switch {
		case profileVariant == config.ProfileCompact:
			if isExternalStepTool(entry.Source, spec.Name) {
				spec.Description = compactExternalToolDescription(spec, spec.Permission == "read")
				if spec.Permission != "read" {
					body, suffix := splitMCPServerSuffix(spec.Description)
					spec.Description = compactDescription(body) + suffix
				}
			} else {
				spec.Description = compactBuiltinToolDescription(spec)
			}
		case mode == ModeCompact:
			if isExternalStepTool(entry.Source, spec.Name) && spec.Permission == "read" {
				spec.Description = compactExternalToolDescription(spec, true)
			}
		}
		out[i] = spec
	}
	return out
}

func isExternalStepTool(source, name string) bool {
	return source == "lsp" || source == "mcp" || strings.HasPrefix(source, "mcp:") ||
		strings.HasPrefix(name, "mcp__") || strings.HasPrefix(name, "lsp_")
}

// compactBuiltinToolDescription preserves critical built-in operational/safety preconditions under
// explicit ProfileCompact (REV-F06 P3 mandatory contract).
func compactBuiltinToolDescription(spec protocol.ToolSpec) string {
	switch spec.Name {
	case "read_file":
		return "Read a UTF-8 workspace file; pass anchors=true for rev header and N:hhh|text rows, and start_line/end_line to page."
	case "write_file":
		return "Create or replace a UTF-8 file; refused when changed since read (pass base_rev to name the revision)."
	case "replace_in_file":
		return "Replace one exact text occurrence in a UTF-8 file; refused when changed since read (pass base_rev to name the revision)."
	case "edit_lines":
		return "Edit anchored lines N:hhh at rev from read_file; refused as stale when target or neighbour lines changed."
	case protocol.ToolBash:
		first := compactDescription(spec.Description)
		if hasBackgroundModeParam(spec.Parameters) || strings.Contains(spec.Description, `mode "background"`) {
			return strings.TrimSuffix(first, "…") + ` mode "background" starts a background job; active background jobs block completion.`
		}
		return first
	case "eval":
		return "Execute one cell in an authorized persistent runtime: variables survive cells, exceptions do not roll back side effects, cancel/timeout destroys worker state without automatic replay, and final code must be verified in a clean process."
	default:
		return compactDescription(spec.Description)
	}
}

func hasBackgroundModeParam(params map[string]any) bool {
	if params == nil {
		return false
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, hasMode := props["mode"]
	return hasMode
}

// compactExternalToolDescription strips and restores any trailing " (MCP server: <id>)" tag and,
// for read-only external tools, retains all code fences by default, replacing a fenced block with
// "[example omitted]" only when all four positive pure-example conditions hold (RC3-NEW-05).
func compactExternalToolDescription(spec protocol.ToolSpec, readOnly bool) string {
	body, suffix := splitMCPServerSuffix(spec.Description)
	if !readOnly || body == "" {
		return body + suffix
	}
	return foldVerifiedExampleFences(body) + suffix
}

func splitMCPServerSuffix(desc string) (body, suffix string) {
	const marker = " (MCP server: "
	idx := strings.LastIndex(desc, marker)
	if idx < 0 || !strings.HasSuffix(desc, ")") {
		return desc, ""
	}
	serverID := desc[idx+len(marker) : len(desc)-1]
	if serverID == "" || strings.ContainsAny(serverID, "()\r\n") {
		return desc, ""
	}
	return desc[:idx], desc[idx:]
}

var normativeFenceSignals = []string{
	"must", "required", "should", "never", "only", "do not", "don't", "cannot", "can't",
	"forbidden", "prohibited", "schema", "format", "constraint", "env", "environment",
	"token", "secret", "header", "auth", "before", "after", "order", "sequence", "error",
	"必须", "禁止", "不得", "只能", "严禁", "格式", "约束", "顺序", "鉴权", "环境变量",
}

func foldVerifiedExampleFences(text string) string {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		line := lines[i]
		ch, fLen, ok := parseFenceOpen(line)
		if !ok {
			out = append(out, line)
			i++
			continue
		}
		// Find matching closing fence.
		closeIdx := -1
		for j := i + 1; j < len(lines); j++ {
			if isFenceClose(lines[j], ch, fLen) {
				closeIdx = j
				break
			}
		}
		if closeIdx < 0 {
			// Unclosed fence: retain remainder verbatim.
			out = append(out, lines[i:]...)
			break
		}
		// Check condition 1: immediately preceding non-blank line in out is a standalone example heading.
		prevIdx := len(out) - 1
		for prevIdx >= 0 && strings.TrimSpace(out[prevIdx]) == "" {
			prevIdx--
		}
		if prevIdx >= 0 && isStandaloneExampleHeading(out[prevIdx]) {
			fenceText := strings.Join(lines[i:closeIdx+1], "\n")
			if !containsNormativeSignal(out[prevIdx]) && !containsNormativeSignal(fenceText) {
				out = append(out, "[example omitted]")
				i = closeIdx + 1
				continue
			}
		}
		out = append(out, lines[i:closeIdx+1]...)
		i = closeIdx + 1
	}
	return strings.Join(out, "\n")
}

func parseFenceOpen(line string) (byte, int, bool) {
	indent := 0
	for indent < len(line) && indent < 4 && line[indent] == ' ' {
		indent++
	}
	if indent == 4 {
		return 0, 0, false
	}
	rest := line[indent:]
	if len(rest) < 3 || (rest[0] != '`' && rest[0] != '~') {
		return 0, 0, false
	}
	ch := rest[0]
	run := 0
	for run < len(rest) && rest[run] == ch {
		run++
	}
	if ch == '`' && strings.ContainsRune(rest[run:], '`') {
		return 0, 0, false
	}
	return ch, run, true
}

func isFenceClose(line string, ch byte, minLen int) bool {
	indent := 0
	for indent < len(line) && indent < 4 && line[indent] == ' ' {
		indent++
	}
	if indent == 4 {
		return false
	}
	rest := line[indent:]
	if len(rest) < minLen || rest[0] != ch {
		return false
	}
	run := 0
	for run < len(rest) && rest[run] == ch {
		run++
	}
	return run >= minLen && strings.Trim(rest[run:], " \t") == ""
}

func isStandaloneExampleHeading(line string) bool {
	s := strings.TrimSpace(line)
	s = strings.TrimLeft(s, "#*- \t")
	s = strings.Trim(s, "*_ \t")
	s = strings.TrimSuffix(s, ":")
	s = strings.TrimSuffix(s, "：")
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "example", "examples", "usage example", "usage examples", "sample", "samples", "示例", "用法示例", "调用示例":
		return true
	}
	return false
}

func containsNormativeSignal(text string) bool {
	lower := strings.ToLower(text)
	for _, sig := range normativeFenceSignals {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// compactDescription is the first sentence of a description, bounded.
func compactDescription(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if end := strings.IndexAny(text, ".\n"); end > 0 && end+1 <= len(text) {
		text = text[:end+1]
	}
	if len(text) > CompactDescriptionBytes {
		text = capUTF8WithEllipsis(text, CompactDescriptionBytes)
	}
	return text
}

// presentFeedback applies the profile's feedback variant and enforces both UTF-8 byte caps
// (1200B / 6144B) and measured OriginCompletionBlocked JSON byte caps (2458B / 12346B, RC3-NEW-02).
func presentFeedback(text, variant string) string {
	clean := sanitizeFeedbackText(text)
	if clean == "" {
		return ""
	}
	maxRaw := MaxDetailedFeedbackBytes
	maxJSON := MaxDetailedGateJSONBytes
	outText := clean
	if variant == config.ProfileCompact {
		maxRaw = CompactFeedbackBytes
		maxJSON = MaxCompactGateJSONBytes
		lines := strings.Split(clean, "\n")
		kept := make([]string, 0, len(lines))
		inFence := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") {
				inFence = !inFence
				continue
			}
			if inFence || trimmed == "" {
				continue
			}
			// A tail line is indented under its item; compact keeps the item line only.
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(trimmed, "- ") {
				continue
			}
			kept = append(kept, line)
		}
		outText = strings.Join(kept, "\n")
	}
	if len(outText) > maxRaw {
		outText = capUTF8WithEllipsis(outText, maxRaw)
	}
	return capGateFeedbackJSONBytes(outText, maxJSON)
}

func sanitizeFeedbackText(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "\ufffd")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch < 0x20 && ch != '\n' && ch != '\t') || ch == 0x7f {
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func capUTF8WithEllipsis(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	if maxBytes <= len(ellipsis) {
		return ""
	}
	limit := maxBytes - len(ellipsis)
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + ellipsis
}

func gateFeedbackJSONLen(content string) int {
	raw, err := json.Marshal(protocol.Message{
		Role:    "user",
		Origin:  protocol.OriginCompletionBlocked,
		Content: content,
	})
	if err != nil {
		return MaxDetailedGateJSONBytes + 1
	}
	return len(raw)
}

func capGateFeedbackJSONBytes(s string, maxJSON int) string {
	if gateFeedbackJSONLen(s) <= maxJSON {
		return s
	}
	runes := []rune(s)
	lo, hi := 0, len(runes)
	best := ""
	for lo <= hi {
		mid := (lo + hi) / 2
		candidate := string(runes[:mid])
		if mid < len(runes) && mid > 0 {
			candidate += "…"
		}
		if gateFeedbackJSONLen(candidate) <= maxJSON {
			best = candidate
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best
}

// utf8Valid reports whether s is valid UTF-8.
func utf8Valid(s string) bool {
	return utf8.ValidString(s)
}
