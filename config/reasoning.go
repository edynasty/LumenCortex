package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// reasoningFormats lists the reasoning.format values each normalized provider package accepts, the
// package default first (""). A format names the wire encoding of effort, so it is a property of
// the package: a value an adapter does not implement would otherwise be silently ignored, which is
// how a model that requires adaptive thinking got a budget request and answered HTTP 400.
var reasoningFormats = map[string][]string{
	PackageOpenAICompatible: {
		"", ReasoningNone, ReasoningObject, ReasoningEffortField, ReasoningDeepSeek,
	},
	PackageAnthropicMessages: {"", ReasoningAdaptive},
}

// ReasoningFormats returns the reasoning.format values the provider package accepts, the package
// default first ("" meaning: do not set format). pkg is a package id as configured: "" and the
// legacy ids mean openai-compatible. The caller owns the copy; a package this build does not know
// has no accepted values.
func ReasoningFormats(pkg string) []string {
	normalized, err := NormalizePackage(pkg)
	if err != nil {
		return nil
	}
	return slices.Clone(reasoningFormats[normalized])
}

// defaultReasoningFormat is the resolved reasoning.format of a model that declares none: the
// package default. anthropic-messages resolves to "" because its default encoding is the budget
// one, while "none" there would mean the openai-compatible encoding that sends nothing.
func defaultReasoningFormat(pkg string) string {
	if pkg == PackageAnthropicMessages {
		return ""
	}
	return ReasoningNone
}

// validateReasoning checks every model's reasoning.format against its provider's package and, for
// an adaptive model, that efforts cannot ask for no thinking.
//
// It runs on the merged catalog, after a trusted workspace's providers are applied: the package a
// model's format is checked against may come from the global file while the model entry, the
// format or the package itself comes from the workspace file.
func validateReasoning(providers map[string]Provider) error {
	for _, pid := range sortedKeys(providers) {
		p := providers[pid]
		pkg, err := NormalizePackage(p.Package)
		if err != nil {
			// An unknown package is reported when a model of the provider is resolved; its models'
			// formats cannot be checked against a package this build does not know.
			continue
		}
		for _, key := range sortedKeys(p.Models) {
			r := p.Models[key].Reasoning
			if r == nil {
				continue
			}
			format := strings.ToLower(strings.TrimSpace(r.Format))
			if !slices.Contains(ReasoningFormats(pkg), format) {
				return fmt.Errorf("%w: providers.%s.models.%s.reasoning.format: %q is not accepted by package %s (accepted: %s)",
					ErrInvalidConfig, pid, key, r.Format, pkg, reasoningFormatList(pkg))
			}
			if format == ReasoningAdaptive {
				if i := slices.Index(r.Efforts, EffortNone); i >= 0 {
					return fmt.Errorf("%w: providers.%s.models.%s.reasoning.efforts[%d]: %q is not accepted with reasoning.format %q, "+
						"because adaptive thinking cannot be turned off; remove the level (a model with no efforts list sends no thinking "+
						"parameter when the run asks for none)",
						ErrInvalidConfig, pid, key, i, EffortNone, ReasoningAdaptive)
				}
			}
		}
	}
	return nil
}

// reasoningFormatList renders a package's accepted formats for an error or a doc: the default is
// named, the rest are quoted.
func reasoningFormatList(pkg string) string {
	formats := ReasoningFormats(pkg)
	parts := make([]string, len(formats))
	for i, f := range formats {
		if f == "" {
			parts[i] = `"" (the package default)`
			continue
		}
		parts[i] = strconv.Quote(f)
	}
	return strings.Join(parts, ", ")
}
