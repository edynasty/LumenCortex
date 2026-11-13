package knowledge

import (
	"errors"
	"reflect"
	"slices"
)

// ExperienceTransition permits a single auditable event per revision. It does not qualify an
// observation: the Store rechecks new evidence, source versions, approvals and fences in the write.
func ExperienceTransition(old, next Experience) error {
	if !reflect.DeepEqual(old.Origin, next.Origin) || !old.CreatedAt.Equal(next.CreatedAt) {
		return errors.New("experience revision changed original evidence")
	}
	if next.DefinitionVersion == old.DefinitionVersion+1 {
		if old.Definition.Digest() == next.Definition.Digest() || next.SupersedesVersion != old.DefinitionVersion || next.State != ExperienceCandidate ||
			!next.LastVerifiedAt.IsZero() || !reflect.DeepEqual(next.Validation, ExperienceValidation{}) || !reflect.DeepEqual(old.Uses, next.Uses) {
			return errors.New("experience definition revision must reset validation and preserve feedback")
		}
		if !slices.Equal(old.Definition.CitedPaths, next.Definition.CitedPaths) {
			return errors.New("new source citations require a new extraction")
		}
		return nil
	}
	if next.DefinitionVersion != old.DefinitionVersion || next.SupersedesVersion != old.SupersedesVersion || !reflect.DeepEqual(next.Definition, old.Definition) {
		return errors.New("experience definition changed without a revision")
	}
	if len(next.Uses) == len(old.Uses)+1 {
		if old.State != ExperienceValidated || next.State != old.State || !reflect.DeepEqual(next.Validation, old.Validation) || !next.LastVerifiedAt.Equal(old.LastVerifiedAt) ||
			!slices.EqualFunc(next.Uses[:len(old.Uses)], old.Uses, func(a, b ExperienceUse) bool { return reflect.DeepEqual(a, b) }) || next.Uses[len(old.Uses)].Feedback != nil {
			return errors.New("adoption requires validated unchanged experience")
		}
		u := next.Uses[len(old.Uses)]
		if u.DefinitionVersion != old.DefinitionVersion || u.DefinitionDigest != old.Definition.Digest() || u.ExpectedOutcome != old.Definition.ExpectedOutcome || u.ExpectedResult != old.Definition.ExpectedResult || len(u.Conditions) != len(old.Definition.Preconditions) {
			return errors.New("adoption differs from validated definition")
		}
		for i, o := range u.Conditions {
			if o.Assertion != old.Definition.Preconditions[i] {
				return errors.New("adoption checked the wrong conditions")
			}
		}
		return nil
	}
	if len(next.Uses) != len(old.Uses) {
		return errors.New("experience revision removed adoption history")
	}
	feedback := false
	for i, before := range old.Uses {
		after := next.Uses[i]
		if reflect.DeepEqual(before, after) {
			continue
		}
		if feedback || before.Feedback != nil || after.Feedback == nil {
			return errors.New("experience revision rewrote historical feedback")
		}
		f := after.Feedback
		after.Feedback = nil
		if !reflect.DeepEqual(before, after) {
			return errors.New("feedback rewrote its adoption")
		}
		feedback = true
		want := old.State
		if f.Outcome == "failure" && after.DefinitionVersion == old.DefinitionVersion {
			want = ExperienceNeedsRevision
		}
		if next.State != want {
			return errors.New("feedback changed unrelated experience state")
		}
	}
	if feedback {
		if !reflect.DeepEqual(next.Validation, old.Validation) || !next.LastVerifiedAt.Equal(old.LastVerifiedAt) {
			return errors.New("feedback rewrote validation")
		}
		return nil
	}
	if old.State == ExperienceNeedsRevision {
		return errors.New("counterexample requires definition revision before revalidation")
	}
	if next.State != ExperienceValidated && next.State != ExperienceNeedsEvidence && next.State != ExperienceRejected {
		return errors.New("invalid experience validation transition")
	}
	return nil
}
