package knowledge

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// FFR-017. The fixtures below encode records the way Core issues them: a session is "session_" and
// 24 hex digits, a run 32 hex digits, a use and an application "kc_" and 20, a scope identity a
// digest, a fingerprint the head, two content hashes, the workspace and a 16 digit sum.

func roomTestID(prefix, seed string, digits int) string {
	return prefix + strings.TrimPrefix(Digest("room", seed), "sha256:")[:digits]
}

var roomTestScope = Scope{Kind: ScopeProject, OwnerID: Digest("cognition-owner"), ProjectID: Digest("cognition-project")}

func roomTestSource(session, run string) SourceRef {
	return SourceRef{Kind: KindSession, ID: session, Digest: Digest("version", session, run), Scope: "project", RunID: run, SessionID: session, Sensitivity: SensitivitySession, Provenance: ProvenanceRuntime}
}

// roomTestShape is everything about an observation that changes its width.
type roomTestShape struct {
	source       SourceRef
	seq, epoch   int64
	at           time.Time
	dirty        bool   // the workspace holds changed and ignored content: two more hashes
	workspace    string // "local", or the path of a worktree
	freshness    Freshness
	verification string
}

func roomTestObservation(a Assertion, s roomTestShape) ExperienceObservation {
	hex := func(seed string) string { return roomTestID("", seed, 64) }
	fingerprint := hex("head")[:40] + "++"
	if s.dirty {
		fingerprint = hex("head")[:40] + "+" + hex("dirty") + "+" + hex("ignored")
	}
	e := Evidence{
		ID: fmt.Sprintf("%s:%d", s.source.SessionID, s.seq), Source: s.source, Type: "check", Provenance: ProvenanceRuntime, Trust: "framework",
		Verification: s.verification, Scope: roomTestScope, Times: Times{ObservedAt: s.at, RecordedAt: s.at, VerifiedAt: s.at}, Freshness: s.freshness,
		Fingerprint: fingerprint + "@" + s.workspace + "#" + hex("sum")[:16], BindingDigest: Digest("binding", a.Ref()), SpecDigest: Digest("spec", a.Ref()),
		ApprovalEpoch: s.epoch, SessionID: s.source.SessionID, ResultSeq: s.seq,
	}
	if s.verification == "failed" {
		e.ContradictingClaims = []string{a.ValueDigest()}
	} else {
		e.SupportingClaims = []string{a.ValueDigest()}
	}
	return ExperienceObservation{Assertion: a, Evidence: e}
}

var (
	// The narrowest observation: a host's check in a clean checkout before the session ever ran.
	roomTestEarly = time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	// The widest: a run's check in a workspace it changed, after many results and approvals.
	roomTestLate = time.Date(2026, 11, 14, 9, 0, 0, 123456789, time.UTC)
)

func roomTestNarrow(session string) roomTestShape {
	return roomTestShape{source: roomTestSource(session, ""), seq: 1, epoch: 1, at: roomTestEarly, workspace: "local", freshness: FreshnessRun, verification: "passed"}
}

func roomTestWide(session, run, workspace, verification string) roomTestShape {
	return roomTestShape{source: roomTestSource(session, run), seq: math.MaxInt64, epoch: math.MaxInt64, at: roomTestLate, dirty: true, workspace: workspace, freshness: FreshnessVersion, verification: verification}
}

// roomTestRecord is a validated record of the largest definition: three conditions.
func roomTestRecord(t *testing.T) Node {
	t.Helper()
	origin := roomTestID("session_", "origin", 24)
	source := roomTestSource(origin, roomTestID("", "origin-run", 32))
	def := experienceTestDefinition()
	def.Preconditions = []Assertion{
		{Kind: AssertConfigValue, Target: "config/app.yaml", Key: "port", Value: "8080"},
		{Kind: AssertFileExists, Target: "config.env", Value: "present"},
		{Kind: AssertFileExists, Target: "defaults.env", Value: "present"},
	}
	observed := roomTestNarrow(origin)
	observed.source = source
	x := Experience{
		Origin:     ExperienceOrigin{Source: source, TaskVersion: 7, TaskRef: "task_state:" + origin + "@7", Outcome: "completed", CompletedAt: roomTestEarly},
		Definition: def, DefinitionVersion: 1, State: ExperienceValidated, Uncertainty: ExperienceUncertainty, CreatedAt: roomTestEarly, LastVerifiedAt: roomTestEarly,
		Validation: ExperienceValidation{Method: ExperienceMethod, Reason: "conditions_and_outcome_verified"},
	}
	for _, a := range def.Preconditions {
		x.Validation.Conditions = append(x.Validation.Conditions, roomTestObservation(a, observed))
	}
	outcome := roomTestObservation(def.ExpectedOutcome, observed)
	x.Validation.Outcome = &outcome
	x.Confidence = x.ConditionalConfidence()
	n := Node{
		ID: roomTestID("kc_", "record", 20), Version: 9, SessionID: roomTestID("session_", "owner", 24), Kind: "experience", Title: def.ProblemPattern, Status: "active", Scope: roomTestScope,
		Times: Times{RecordedAt: roomTestEarly, VerifiedAt: roomTestEarly}, Sources: []SourceRef{source}, Experience: &x, RevisionReason: "framework_experience_transition",
	}
	if err := n.Validate(); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return n
}

// roomTestUse is the open use of a consumer whose conditions were observed at shape s.
func roomTestUse(n Node, consumer string, s roomTestShape, run string) ExperienceUse {
	x := n.Experience
	u := ExperienceUse{
		RunID: run, ID: roomTestID("kc_", "use-"+consumer, 20), ConsumerID: consumer, BaselineDigest: s.source.Digest, PreviousRunID: s.source.RunID,
		DefinitionVersion: x.DefinitionVersion, DefinitionDigest: x.Definition.Digest(), ExpectedOutcome: x.Definition.ExpectedOutcome, ExpectedResult: x.Definition.ExpectedResult, AdoptedAt: s.at,
	}
	for _, a := range x.Definition.Preconditions {
		u.Conditions = append(u.Conditions, roomTestObservation(a, s))
	}
	return u
}

// roomTestSettle gives the open use i the largest feedback Core writes for it: the consumer's first
// run, in a workspace the run changed, after as many results and approvals as a number can count.
// failure is a counterexample; otherwise the run failed although the expected outcome held, which has
// the longest reason.
func roomTestSettle(n *Node, i int, workspace string, failure bool) {
	x := n.Experience
	u := &x.Uses[i]
	run := roomTestID("", "run-"+u.ConsumerID, 32)
	verification := u.ExpectedResult
	f := &ExperienceFeedback{Used: true, ConditionsHeld: true, Outcome: "unknown", Reason: "consumer_run_failed_with_matching_check", ApplicationID: roomTestID("kc_", "application-"+u.ConsumerID, 20), At: roomTestLate}
	if failure {
		verification = "failed"
		f.Outcome, f.Reason, f.RevisionNeeded = "failure", "outcome_counterexample", true
		x.State = ExperienceNeedsRevision
		x.Confidence = x.ConditionalConfidence()
	}
	wide := roomTestWide(u.ConsumerID, run, workspace, verification)
	observation := roomTestObservation(u.ExpectedOutcome, wide)
	f.Source, f.Observation = wide.source, &observation
	f.Source.Digest = Digest("version", u.ConsumerID, "terminal")
	u.Feedback = f
}

// roomTestFill pads the body of n until its room is exactly want.
func roomTestFill(t *testing.T, n *Node, want int) {
	t.Helper()
	n.Body = "b"
	pad := n.Room() - want
	if pad < 0 || pad > 8191 {
		t.Fatalf("fixture: the body cannot move a room of %d to %d", n.Room(), want)
	}
	n.Body += strings.Repeat("b", pad)
	if n.Room() != want {
		t.Fatalf("fixture: room %d, want %d", n.Room(), want)
	}
	if err := n.Validate(); err != nil {
		t.Fatalf("fixture: %v", err)
	}
}

// roomTestWorktree is the path of a worktree as much longer than "local" as the slack of an
// observation allows for.
var roomTestWorktree = "/" + strings.Repeat("w", 255)

// A content revision may leave a record with no room at all (Room() == 0). The feedback of every use
// such a record holds open must then stay within the room kept for it: the settlement of an applied
// use is not refused for bytes, and it leaves the room of a lifecycle revision untouched.
func TestRecordKeepsTheRoomOfTheFeedbackOfItsOpenUses(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", failure), func(t *testing.T) {
			n := roomTestRecord(t)
			x := n.Experience
			// A host adopted for each consumer in the clean local checkout.
			for len(x.Uses) < MaxExperienceUses {
				consumer := roomTestID("session_", fmt.Sprintf("consumer-%d", len(x.Uses)), 24)
				held := x.Uses
				x.Uses = append(x.Uses[:len(x.Uses):len(x.Uses)], roomTestUse(n, consumer, roomTestNarrow(consumer), ""))
				if n.Room() < 0 {
					x.Uses = held
					break
				}
			}
			if len(x.Uses) < 2 {
				t.Fatalf("fixture: %d open uses", len(x.Uses))
			}
			roomTestFill(t, &n, 0)
			full := encodedLen(n)

			// Each consumer was then moved into a worktree and ran there.
			for i := range x.Uses {
				roomTestSettle(&n, i, roomTestWorktree, failure)
				n.Version++
				n.Times.RecordedAt = roomTestLate
				if err := n.Validate(); err != nil {
					t.Fatalf("the feedback of open use %d of %d does not fit its record (%d bytes before the settlements, now %d of %d): %v",
						i+1, len(x.Uses), full, encodedLen(n), MaxGraphRecordBytes, err)
				}
				if room := n.Room(); room < 0 {
					t.Fatalf("the feedback of open use %d of %d overran the room its record kept for it by %d bytes", i+1, len(x.Uses), -room)
				}
			}
			t.Logf("%d open uses: %d bytes with no room left, %d settled, room %d", len(x.Uses), full, encodedLen(n), n.Room())
		})
	}
}

// A record that content has filled to its last byte of room takes the widest lifecycle revision a
// host can make of it: a full record can still be archived, revoked, tiered or consolidated.
func TestRecordFilledByContentTakesTheWidestLifecycleRevision(t *testing.T) {
	n := roomTestRecord(t)
	x := n.Experience
	// Settled uses and the body fill the record.
	for len(x.Uses) < MaxExperienceUses {
		consumer := roomTestID("session_", fmt.Sprintf("settled-%d", len(x.Uses)), 24)
		held := x.Uses
		x.Uses = append(x.Uses[:len(x.Uses):len(x.Uses)], roomTestUse(n, consumer, roomTestNarrow(consumer), ""))
		roomTestSettle(&n, len(x.Uses)-1, "local", false)
		if n.Room() < 0 {
			x.Uses = held
			break
		}
	}
	// The narrowest lifecycle fields: no reason, no policy, the shortest status, version and times.
	n.Version, n.Status, n.RevisionReason, n.Memory = 1, "stale", "", nil
	n.Times.RecordedAt, n.Times.InvalidatedAt = roomTestEarly, time.Time{}
	roomTestFill(t, &n, 0)
	full := encodedLen(n)

	// The widest: every byte of the reason and of the canonical identity takes six in JSON.
	n.Version, n.Status, n.RevisionReason = math.MaxInt64, "archived", strings.Repeat("<", 512)
	n.Times.RecordedAt, n.Times.InvalidatedAt = roomTestLate, roomTestLate
	n.Memory = &MemoryPolicy{Tier: "warm", Retention: "counterexample", Importance: 1.2345678901234567e-300, Canonical: &NodeRef{ID: strings.Repeat("<", 128), Version: math.MaxInt64}}
	if err := n.Validate(); err != nil {
		t.Fatalf("the widest lifecycle revision (%d bytes) does not fit a record whose content left no room (%d bytes, now %d of %d): %v",
			encodedLen(n)-full, full, encodedLen(n), MaxGraphRecordBytes, err)
	}
	t.Logf("%d bytes with no room left, %d after the widest lifecycle revision (%d bytes), bound %d", full, encodedLen(n), encodedLen(n)-full, MaxGraphRecordBytes)
}

// A record that admits a use (AdmitsExperienceUse) must be able to hold the widest use a consumer
// can make of it, with the room of that use's feedback: the adoption that recall announced is not
// refused by the store.
func TestRecordThatAdmitsAUseHoldsTheWidestOne(t *testing.T) {
	n := roomTestRecord(t)
	x := n.Experience
	admits := func(body int) bool {
		padded := n
		padded.Body = strings.Repeat("b", body)
		return padded.AdmitsExperienceUse()
	}
	// Settled uses fill the record until its body decides whether it admits one more.
	for admits(8192) {
		if len(x.Uses) == MaxExperienceUses-1 {
			t.Fatal("fixture: the record admits a use whatever its body holds")
		}
		consumer := roomTestID("session_", fmt.Sprintf("settled-%d", len(x.Uses)), 24)
		x.Uses = append(x.Uses, roomTestUse(n, consumer, roomTestNarrow(consumer), ""))
		roomTestSettle(&n, len(x.Uses)-1, "local", false)
	}
	// The fullest record that still admits a use.
	fits, refused := 1, 8192
	if !admits(fits) {
		t.Fatal("fixture: the record admits no use")
	}
	for refused-fits > 1 {
		if body := (fits + refused) / 2; admits(body) {
			fits = body
		} else {
			refused = body
		}
	}
	n.Body = strings.Repeat("b", fits)
	if err := n.Validate(); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	before := n.Room()

	// The widest use: adopted by a tool in a run, in a worktree the run changed, of a record whose
	// origin was validated in the clean local checkout.
	consumer := roomTestID("session_", "widest", 24)
	run := roomTestID("", "run-"+consumer, 32)
	x.Uses = append(x.Uses, roomTestUse(n, consumer, roomTestWide(consumer, run, roomTestWorktree, "passed"), run))
	n.Version++
	n.Times.RecordedAt = roomTestLate
	if err := n.Validate(); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if room := n.Room(); room < 0 {
		t.Fatalf("a record that admitted a use cannot hold it with the room of its feedback: room %d after %d", room, before)
	}
	t.Logf("%d settled uses and a body of %d bytes: room %d before the widest use, %d with it", len(x.Uses)-1, fits, before, n.Room())
}
