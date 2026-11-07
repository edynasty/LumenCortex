package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// ErrUnknownCheck reports a check id the effective configuration does not define; an untrusted
// workspace's checks are not defined (design §4.7, A8).
var ErrUnknownCheck = evaluator.ErrUnknownCheck

// ErrNoFingerprint reports an acceptance outside git, where there is no fingerprint to accept.
var ErrNoFingerprint = evaluator.ErrNoFingerprint

// ErrArtifactNotFound reports an artifact hash the session does not hold.
var ErrArtifactNotFound = session.ErrArtifactNotFound

// CheckSpec, CheckResult, CheckEnv and CheckArtifactData name the evaluator's types for hosts, which
// cannot import internal packages. They are aliases: the values are the evaluator's own.
type (
	CheckSpec         = evaluator.Spec
	CheckResult       = evaluator.Result
	CheckEnv          = evaluator.Env
	CheckArtifactData = session.Artifact
)

// sessionEvaluator returns the evaluator for the session's workspace: its worktree when it has one.
func (e *Engine) sessionEvaluator(ctx context.Context, sessionID string) (*evaluator.Evaluator, error) {
	workspace, err := e.agentWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	worktreeID := "local"
	if workspace != e.workspace {
		worktreeID = workspace
	}
	return &evaluator.Evaluator{Store: e.store, SessionID: sessionID, Workspace: workspace, WorktreeID: worktreeID}, nil
}

// effectiveChecks loads the effective configuration's checks and the warnings for checks it dropped.
func (e *Engine) effectiveChecks() ([]evaluator.Spec, []config.Warning, error) {
	cfg, err := config.Load(e.workspace)
	if err != nil {
		return nil, nil, err
	}
	eff := cfg.Effective()
	var warnings []config.Warning
	for _, w := range eff.Warnings {
		if strings.HasPrefix(w.Key, "checks.") {
			warnings = append(warnings, w)
		}
	}
	// Inputs are read here, in the Engine workspace (configScope), so the approval covers the content
	// of the scripts and schemas the definitions run (CHECKS-002).
	return evaluator.ResolveInputs(e.workspace, evaluator.Specs(eff.Checks)), warnings, nil
}

func required(specs []evaluator.Spec) []evaluator.Spec {
	var out []evaluator.Spec
	for _, s := range specs {
		if s.Required {
			out = append(out, s)
		}
	}
	return out
}

func findSpec(specs []evaluator.Spec, id string) (evaluator.Spec, bool) {
	for _, s := range specs {
		if s.ID == id {
			return s, true
		}
	}
	return evaluator.Spec{}, false
}

// checkEvidence reports, for each required check, whether a stored result satisfies it at the
// workspace's current fingerprint, with the satisfying result as a ref.
func checkEvidence(ctx context.Context, ev *evaluator.Evaluator, specs []evaluator.Spec) ([]taskstate.CheckEvidence, error) {
	req := required(specs)
	if len(req) == 0 {
		return nil, nil
	}
	ok, results, _, err := ev.SatisfiedAll(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]taskstate.CheckEvidence, 0, len(req))
	for i, s := range req {
		c := taskstate.CheckEvidence{ID: s.ID, Satisfied: ok[i]}
		if ok[i] && results[i] != nil {
			c.Ref = fmt.Sprintf("check:%s@eval:%d", s.ID, results[i].Seq)
		}
		out = append(out, c)
	}
	return out, nil
}

// runChecks is agent.Checks over one session's evaluator. specs are the run's check specs, frozen at
// Start: they only pick which ids the gate asks for. Decide reads the definitions and their approval
// again from the Engine workspace (configScope) before and after deciding (ADR-0017), so a definition
// changed during the run never decides with the approval of the one it replaced. store and sessionID
// let Decide refuse to run anything once the session is fenced (T3-S1).
type runChecks struct {
	engine    *Engine
	ev        *evaluator.Evaluator
	specs     []evaluator.Spec
	store     *session.Store
	sessionID string
}

func (c runChecks) Configured() (all, req []string) {
	for _, s := range c.specs {
		all = append(all, s.ID)
		if s.Required {
			req = append(req, s.ID)
		}
	}
	return all, req
}

func (c runChecks) Mutated() { c.ev.Mutated() }

// SetApproved: the Engine workspace's check set is approved as it is now, or the project never
// approved one (ADR-0017). A set approved once and changed since, or revoked, is not.
func (c runChecks) SetApproved(ctx context.Context) (bool, error) {
	_, a, _, err := c.engine.approvedChecks(ctx)
	if err != nil {
		return false, err
	}
	return a.set == nil || a.epoch > 0, nil
}

// Decide decides the ids as one check episode (ADR-0017), in order, with the definitions and approval
// the Engine workspace has now. An id the current set does not define yields a spec_unapproved verdict
// when the set is not approved (it was changed) and an error verdict when it is; an unapproved
// definition is not run (evaluator.CheckVerdict). Nothing runs or is stored for either. The episode
// closes spec_unapproved when the set or its approval changed while the checks ran, and then no
// verdict passes. In a fenced session nothing runs: a check is a command with effects, and a
// revocation stops the session's next one (session.ErrFenced).
func (c runChecks) Decide(ctx context.Context, ids []string, workUnitID string) ([]evaluator.Verdict, error) {
	if c.store != nil {
		fenced, err := c.store.SessionFenced(ctx, c.sessionID)
		if err != nil {
			return nil, err
		}
		if fenced {
			return nil, session.ErrFenced
		}
	}
	current, before, _, err := c.engine.approvedChecks(ctx)
	if err != nil {
		return nil, err
	}
	var specs []evaluator.Spec
	for _, id := range ids {
		if s, ok := findSpec(current, id); ok {
			specs = append(specs, s)
		}
	}
	decided, err := c.ev.Decide(ctx, specs, workUnitID, func(ctx context.Context) (bool, error) {
		_, after, _, err := c.engine.approvedChecks(ctx)
		if err != nil {
			return false, err
		}
		return after.setDigest == before.setDigest && after.epoch == before.epoch, nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]evaluator.Verdict, 0, len(ids))
	next := 0
	for _, id := range ids {
		if _, ok := findSpec(current, id); !ok {
			status, tail := evaluator.StatusError, ErrUnknownCheck.Error()
			if before.epoch == 0 {
				status, tail = evaluator.StatusSpecUnapproved, ErrSpecUnapproved.Error()
			}
			out = append(out, evaluator.Verdict{
				Result: evaluator.Result{WorkUnitID: workUnitID, Check: id, Status: status, Provenance: evaluator.ProvenanceRuntime},
				Tail:   tail,
			})
			continue
		}
		out = append(out, decided[next])
		next++
	}
	return out, nil
}

// runEvidence is the loop's Evidence: the session's workspace and its required checks, as the Engine
// workspace defines and approves them now. The loop fills in the workflow's verdicts.
func (e *Engine) runEvidence(sessionID string, checks runChecks) func(context.Context) (taskstate.Evidence, error) {
	return func(ctx context.Context) (taskstate.Evidence, error) {
		var ev taskstate.Evidence
		pending, refs, err := e.workspaceEvidence(ctx, sessionID)
		if err != nil {
			return ev, err
		}
		ev.PendingApplication, ev.Refs = pending, refs
		// The fingerprint is what a claim confirmation is valid for: a workspace that moved makes a
		// confirmation stale, and the gate asks for revalidation.
		if env, envErr := checks.ev.Environment(ctx); envErr == nil {
			ev.Fingerprint = env.String()
		}
		if ev.Tasks, err = e.executionEvidence(ctx, sessionID); err != nil {
			return ev, err
		}
		specs, _, _, err := e.approvedChecks(ctx)
		if err != nil {
			return ev, err
		}
		if ev.Checks, err = checkEvidence(ctx, checks.ev, specs); err != nil {
			return ev, err
		}
		return ev, nil
	}
}

var _ agent.Checks = runChecks{}

// CheckInfo is one configured check as a host shows it: its spec (with the approval epoch it runs
// under, absent when unapproved), whether a stored result satisfies it at the current fingerprint under
// that approval (and that result), and the newest result at any fingerprint.
type CheckInfo struct {
	Spec      CheckSpec    `json:"spec"`
	Satisfied bool         `json:"satisfied"`
	Deciding  *CheckResult `json:"deciding,omitempty"`
	Newest    *CheckResult `json:"newest,omitempty"`
}

// ChecksInfo is a session's checks against its workspace's current state. Env.Head is "" outside git:
// no result is ever reused there, and nothing can be accepted. SetDigest and Approved are the current
// check set and whether the host approved it as it is (ADR-0017; CheckApprovalState has the detail).
// Warnings name checks the configuration dropped, such as an untrusted workspace's.
type ChecksInfo struct {
	SessionID string           `json:"sessionId"`
	Env       CheckEnv         `json:"env"`
	SetDigest string           `json:"setDigest"`
	Approved  bool             `json:"approved"`
	Checks    []CheckInfo      `json:"checks"`
	Warnings  []config.Warning `json:"warnings,omitempty"`
}

// SessionChecks reports the configured checks for the session's workspace. It reads only; it runs no
// check and takes no lock.
func (e *Engine) SessionChecks(ctx context.Context, sessionID string) (ChecksInfo, error) {
	specs, approval, warnings, err := e.approvedChecks(ctx)
	if err != nil {
		return ChecksInfo{}, err
	}
	ev, err := e.sessionEvaluator(ctx, sessionID)
	if err != nil {
		return ChecksInfo{}, err
	}
	ok, deciding, env, err := ev.SatisfiedAll(ctx, specs)
	if err != nil {
		return ChecksInfo{}, err
	}
	info := ChecksInfo{SessionID: sessionID, Env: env, SetDigest: approval.setDigest, Approved: approval.epoch > 0,
		Checks: make([]CheckInfo, 0, len(specs)), Warnings: warnings}
	for i, s := range specs {
		c := CheckInfo{Spec: s, Satisfied: ok[i], Deciding: deciding[i]}
		row, err := e.store.NewestEvalResult(ctx, sessionID, s.ID)
		if err != nil {
			return ChecksInfo{}, err
		}
		if row != nil {
			r := evaluator.FromRow(*row)
			c.Newest = &r
		}
		info.Checks = append(info.Checks, c)
	}
	return info, nil
}

// CheckResults pages the session's check results, newest first: those with seq below beforeSeq
// (-1 for the newest page, as for MessagePage), at most limit (default 50, at most 200).
func (e *Engine) CheckResults(ctx context.Context, sessionID string, beforeSeq int64, limit int) ([]CheckResult, error) {
	if limit > 200 {
		limit = 200
	}
	if _, err := e.store.Get(ctx, sessionID); err != nil {
		return nil, err
	}
	rows, err := e.store.EvalResults(ctx, sessionID, beforeSeq, limit)
	if err != nil {
		return nil, err
	}
	out := make([]CheckResult, 0, len(rows))
	for _, row := range rows {
		out = append(out, evaluator.FromRow(row))
	}
	return out, nil
}

// CheckArtifact returns one artifact of the session: a check's log or the diff a non-passing result
// covers.
func (e *Engine) CheckArtifact(ctx context.Context, sessionID, hash string) (CheckArtifactData, error) {
	return e.store.Artifact(ctx, sessionID, strings.TrimSpace(hash))
}

// AcceptCheck records a person's acceptance of a configured check at the workspace's current
// fingerprint, as a human:<actor> result (design §4.7). It holds until the fingerprint or the check's
// approval changes. Outside git it fails with ErrNoFingerprint; an unknown or untrusted check fails
// with ErrUnknownCheck, and a check whose current definition is not approved with ErrSpecUnapproved
// (ADR-0017: accepting a result does not approve the definition that produced it).
//
// Acceptance is evidence, not a resolution: the thread is latched done at the next run end or Apply
// that finds no obligation left (ADR-0006 D2).
func (e *Engine) AcceptCheck(ctx context.Context, sessionID, checkID, actor string) (CheckResult, error) {
	specs, _, _, err := e.approvedChecks(ctx)
	if err != nil {
		return evaluator.Result{}, err
	}
	spec, ok := findSpec(specs, strings.TrimSpace(checkID))
	if !ok {
		return evaluator.Result{}, fmt.Errorf("%w: %q", ErrUnknownCheck, checkID)
	}
	ev, err := e.sessionEvaluator(ctx, sessionID)
	if err != nil {
		return evaluator.Result{}, err
	}
	r, err := ev.Accept(ctx, spec, actor)
	if err != nil {
		return evaluator.Result{}, err
	}
	e.events.publish("check.accepted", sessionID, map[string]any{"check": r.Check, "seq": r.Seq, "provenance": r.Provenance})
	_ = e.store.Journal(context.Background(), "check.accepted", map[string]any{
		"sessionId": sessionID, "check": r.Check, "seq": r.Seq, "provenance": r.Provenance, "at": r.At.Format(time.RFC3339Nano),
	})
	return r, nil
}
