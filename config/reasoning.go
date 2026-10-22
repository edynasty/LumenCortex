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

// adaptiveEfforts are the levels adaptive thinking's output_config.effort accepts: it has no "off"
// (none) and no minimal.
var adaptiveEfforts = []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}

// ReasoningEfforts returns the effort levels a model of the provider package may list under the
// given reasoning.format, lowest first: adaptive thinking accepts low … max, every other format
// accepts every level (its adapter maps or sends them). nil for a package or format this build
// does not accept. The caller owns the copy.
func ReasoningEfforts(pkg, format string) []Effort {
	normalized, err := NormalizePackage(pkg)
	if err != nil {
		return nil
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if !slices.Contains(reasoningFormats[normalized], format) {
		return nil
	}
	if format == ReasoningAdaptive {
		return slices.Clone(adaptiveEfforts)
	}
	return EffortLevels()
}

// ValidateProviderReasoning checks every model's reasoning.format against its provider's package
// and its reasoning.efforts against what that format accepts (ReasoningEfforts). Load runs it on
// the merged catalog; a host that edits providers runs it before saving, so it never writes a
// file Load would refuse.
//
// On load it runs after a trusted workspace's providers are applied: the package a model's
// format is checked against may come from the global file while the model entry, the format or
// the package itself comes from the workspace file.
func ValidateProviderReasoning(providers map[string]Provider) error {
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
			accepted := ReasoningEfforts(pkg, format)
			for i, e := range r.Efforts {
				if slices.Contains(accepted, e) {
					continue
				}
				if format == ReasoningAdaptive && e == EffortNone {
					return fmt.Errorf("%w: providers.%s.models.%s.reasoning.efforts[%d]: %q is not accepted with reasoning.format %q, "+
						"because adaptive thinking cannot be turned off; remove the level (a model with no efforts list sends no thinking "+
						"parameter when the run asks for none)",
						ErrInvalidConfig, pid, key, i, EffortNone, ReasoningAdaptive)
				}
				return fmt.Errorf("%w: providers.%s.models.%s.reasoning.efforts[%d]: %q is not accepted with reasoning.format %q (accepted: %s)",
					ErrInvalidConfig, pid, key, i, e, r.Format, effortList(accepted))
			}
		}
	}
	return nil
}

func effortList(levels []Effort) string {
	parts := make([]string, len(levels))
	for i, e := range levels {
		parts[i] = string(e)
	}
	return strings.Join(parts, ", ")
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
