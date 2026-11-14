package knowledge

import "errors"

// ValidateScope prevents a derived Experience from widening any embedded observation's audience.
// Consumer feedback contains host-assigned observation identities, never the consumer's transcript.
func (x Experience) ValidateScope(scope Scope) error {
	check := func(o ExperienceObservation, source SourceRef) error {
		if o.Evidence.Source != source || o.Evidence.SessionID != source.SessionID || !o.Evidence.Scope.Contains(scope) {
			return errors.New("experience evidence source or scope does not cover record")
		}
		return nil
	}
	for _, o := range x.Validation.Conditions {
		if err := check(o, x.Origin.Source); err != nil {
			return err
		}
	}
	if x.Validation.Outcome != nil {
		if err := check(*x.Validation.Outcome, x.Origin.Source); err != nil {
			return err
		}
	}
	for _, u := range x.Uses {
		for _, o := range u.Conditions {
			if o.Evidence.Source.SessionID != u.ConsumerID || !o.Evidence.Scope.Contains(scope) {
				return errors.New("consumer observation scope is narrower than experience")
			}
		}
		// The outcome is observed when the application ends, at an earlier version of the run the
		// feedback settles.
		if f := u.Feedback; f != nil && f.Observation != nil {
			if o := f.Observation.Evidence; !sameRun(o.Source, f.Source) || o.SessionID != f.Source.SessionID || !o.Scope.Contains(scope) {
				return errors.New("experience evidence source or scope does not cover record")
			}
		}
	}
	return nil
}
