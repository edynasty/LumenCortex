// Command lcx-experience-demo is an offline, host-invoked demonstration of the native Experience
// pipeline. It creates a disposable git workspace, a private configuration/state home, an approved
// check set and a deterministic in-process provider, then runs two independent sessions through the
// ordinary public runtime API: A originates a validated experience, B recalls and adopts it, executes
// its own task and records independent feedback. The Engine is then closed and reopened to prove the
// history is durable.
//
// Nothing here contacts a model endpoint, reads the user's configuration, state or workspace, or
// claims that reusing a strategy improved a model's output. The only reuse claim it can support is
// the one the framework checks: the conditions held, the consumer's later run produced the expected
// outcome, and feedback was recorded against an independent terminal run.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
	lcx "github.com/edynasty/LumenCortex/runtime"
)

const (
	modelRef       = config.ModelRef("demo/offline")
	problemPattern = "prepare deterministic release marker"
	projectScope   = "project" // the public scope string the runtime accepts for a project record.
)

func main() {
	report, err := run()
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err != nil {
		_ = encoder.Encode(map[string]any{
			"ok":    false,
			"mode":  "offline-scripted-experience",
			"error": err.Error(),
		})
		os.Exit(1)
	}
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, "lcx-experience-demo: encode report:", err)
		os.Exit(1)
	}
}

// run owns every temporary resource it creates. The scratch root, git repository, configuration,
// provider, state database and worktree root all live below one directory outside the caller's
// repository and are removed before the successful report is printed.
func run() (report demoReport, err error) {
	root, err := os.MkdirTemp("", "lcx-experience-demo-")
	if err != nil {
		return report, fmt.Errorf("create scratch root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		_ = os.RemoveAll(root)
		return report, errors.New(scrub(fmt.Sprintf("resolve scratch root: %v", err), root))
	}
	root = resolved

	// Scrub before reporting: no owned absolute path may appear in stdout/stderr evidence.
	defer func() {
		if err != nil {
			err = errors.New(scrub(err.Error(), root))
		}
	}()
	defer func() {
		removeErr := os.RemoveAll(root)
		report.Cleanup = cleanupEvidence{Removed: removeErr == nil}
		if removeErr != nil && err == nil {
			err = fmt.Errorf("remove scratch root: %w", removeErr)
		}
	}()

	paths, err := prepareScratch(root)
	if err != nil {
		return report, err
	}
	restoreEnv := installEnv(map[string]string{
		"HOME":            paths.home,
		"XDG_CONFIG_HOME": paths.configHome,
		"XDG_STATE_HOME":  paths.stateHome,
		"TMPDIR":          paths.tmp,
		"TMP":             paths.tmp,
		"TEMP":            paths.tmp,
	})
	defer restoreEnv()

	ctx := context.Background()
	engine, err := openEngine(ctx, paths.workspace, paths.repoDir)
	if err != nil {
		return report, fmt.Errorf("open engine: %w", err)
	}
	var engineOpen = true
	defer func() {
		if engineOpen {
			if closeErr := engine.Close(); closeErr != nil && err == nil {
				err = fmt.Errorf("close engine: %w", closeErr)
			}
		}
	}()

	approved, err := approveChecksAndBindings(ctx, engine)
	if err != nil {
		return report, err
	}

	// Session A: one ordinary scripted run, then host extraction and framework validation.
	sessionA, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: "Create the deterministic release marker in the scratch workspace."})
	if err != nil {
		return report, fmt.Errorf("create session A: %w", err)
	}
	resultA, err := engine.RunAgent(ctx, sessionA.ID, lcx.AgentOptions{
		ModelRef: modelRef,
		Policy:   config.PolicyWorkspace,
		MaxSteps: 6,
	})
	if err != nil {
		return report, fmt.Errorf("run session A: %w", err)
	}
	if resultA.Status != "completed" {
		return report, fmt.Errorf("session A ended %q, want completed", resultA.Status)
	}
	_, infoA, err := engine.Session(ctx, sessionA.ID)
	if err != nil {
		return report, fmt.Errorf("read session A: %w", err)
	}

	proposal := lcx.ExperienceProposal{
		ProblemPattern: problemPattern,
		ProblemType:    "release_marker",
		Context:        "A scratch git workspace contains gate.flag and check scripts; the release marker must be written and the approved outcome check must pass.",
		ProposedCause:  "Writing the marker before claiming completion makes the outcome check decide the result.",
		Preconditions: []lcx.CognitiveAssertion{{
			Kind: "file_exists", Target: "gate.flag", Value: "present",
		}},
		Strategy: []string{
			"Confirm the gate marker gate.flag is present before changing files.",
			"Write output.txt with exactly the line status=ready.",
			"Let the approved outcome check decide completion instead of assuming success.",
		},
		ExpectedOutcome: lcx.CognitiveAssertion{
			Kind: "config_value", Target: "output.txt", Key: "status", Value: "ready",
		},
		ExpectedResult: "passed",
		CitedPaths:     []string{"README.md"},
	}
	extracted, err := engine.ExtractExperience(ctx, sessionA.ID, lcx.ExperienceExtraction{
		ScopeKind: projectScope,
		Proposal:  proposal,
		Valid:     lcx.CognitiveValidTime{},
	})
	if err != nil {
		return report, fmt.Errorf("extract experience: %w", err)
	}
	validated, err := engine.ValidateExperience(ctx, sessionA.ID, extracted.ID, extracted.Version)
	if err != nil {
		return report, fmt.Errorf("validate experience: %w", err)
	}
	if validated.Experience == nil || validated.Experience.State != "validated" {
		return report, fmt.Errorf("experience state is %q, want validated", experienceState(validated))
	}
	if validated.Experience.Validation.Outcome == nil {
		return report, errors.New("validated experience has no qualified outcome observation")
	}
	if got, want := validated.Experience.Validation.Outcome.Evidence.Verification, validated.Experience.Definition.ExpectedResult; got != want {
		return report, fmt.Errorf("validation observed %q, want %q", got, want)
	}

	// Session B is independent: a new session with no A transcript. It only receives knowledge
	// through the public recall/adoption API before its own ordinary RunAgent.
	sessionB, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: "Apply the recalled conditional strategy to a fresh release marker task."})
	if err != nil {
		return report, fmt.Errorf("create session B: %w", err)
	}
	matches, err := engine.RecallExperiences(ctx, sessionB.ID, problemPattern, 5)
	if err != nil {
		return report, fmt.Errorf("recall in session B: %w", err)
	}
	if !containsExperience(matches, validated.ID) {
		return report, fmt.Errorf("session B did not recall experience %s", validated.ID)
	}
	adoption, err := engine.AdoptExperience(ctx, sessionB.ID, validated.ID, validated.Version)
	if err != nil {
		return report, fmt.Errorf("adopt in session B: %w", err)
	}
	// Remove A's output so a successful check cannot credit B for an artifact it inherited.
	outputPath := filepath.Join(paths.workspace, "output.txt")
	if err := os.Remove(outputPath); err != nil {
		return report, fmt.Errorf("clear origin output before session B: %w", err)
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		return report, fmt.Errorf("origin output still exists before session B: %v", err)
	}

	application, err := json.Marshal(map[string]any{"id": validated.ID, "adoptionId": adoption.ID, "actions": []map[string]any{{"tool": "write_file", "arguments": map[string]any{"path": "output.txt", "content": "status=ready\n"}}}})
	if err != nil {
		return report, err
	}
	input := "Apply this adopted procedure through apply_experience: " + string(application)
	resultB, err := engine.RunAgent(ctx, sessionB.ID, lcx.AgentOptions{
		ModelRef: modelRef,
		Policy:   config.PolicyWorkspace,
		MaxSteps: 6,
		Input:    input,
	})
	if err != nil {
		return report, fmt.Errorf("run session B: %w", err)
	}
	if resultB.Status != "completed" {
		return report, fmt.Errorf("session B ended %q, want completed", resultB.Status)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil || string(output) != "status=ready\n" {
		return report, fmt.Errorf("session B did not produce the exact fresh output: %v", err)
	}
	feedbackNode, err := engine.RecordExperienceFeedback(ctx, sessionB.ID, validated.ID, adoption.ID)
	if err != nil {
		return report, fmt.Errorf("record feedback: %w", err)
	}
	var feedbackOutcome, feedbackReason, feedbackVerification, feedbackRunID string
	var feedbackUsed, feedbackConditionsHeld, feedbackRevisionNeeded bool
	feedbackFound := false
	for _, use := range feedbackNode.Experience.Uses {
		if use.ID != adoption.ID {
			continue
		}
		if use.Feedback == nil {
			return report, errors.New("adoption has no feedback after recording")
		}
		feedbackFound = true
		feedbackOutcome = use.Feedback.Outcome
		feedbackReason = use.Feedback.Reason
		feedbackUsed = use.Feedback.Used
		feedbackConditionsHeld = use.Feedback.ConditionsHeld
		feedbackRevisionNeeded = use.Feedback.RevisionNeeded
		feedbackRunID = use.Feedback.Source.RunID
		if use.Feedback.Observation != nil {
			feedbackVerification = use.Feedback.Observation.Evidence.Verification
		}
	}
	if !feedbackFound {
		return report, errors.New("recorded feedback was not found on the adoption")
	}
	if feedbackOutcome != "success" || feedbackVerification != "passed" {
		return report, fmt.Errorf("feedback outcome=%q verification=%q, want success/passed", feedbackOutcome, feedbackVerification)
	}

	// Wrong-condition refusal: remove an uncited precondition and show that adoption refuses rather
	// than returning the strategy. The experience record and its uses must not change.
	versionBeforeRefusal := feedbackNode.Version
	usesBeforeRefusal := len(feedbackNode.Experience.Uses)
	if err := os.Remove(filepath.Join(paths.workspace, "gate.flag")); err != nil {
		return report, fmt.Errorf("remove gate marker for refusal case: %w", err)
	}
	sessionC, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: "Try to reuse the strategy without the gate marker."})
	if err != nil {
		return report, fmt.Errorf("create refusal session: %w", err)
	}
	_, refusalErr := engine.AdoptExperience(ctx, sessionC.ID, validated.ID, versionBeforeRefusal)
	if !errors.Is(refusalErr, lcx.ErrExperienceNotApplicable) {
		return report, fmt.Errorf("adoption without its precondition returned %v, want ErrExperienceNotApplicable", refusalErr)
	}
	afterRefusal, err := engine.ReadExperience(ctx, sessionB.ID, validated.ID, 0)
	if err != nil {
		return report, fmt.Errorf("read after refusal: %w", err)
	}
	if afterRefusal.Version != versionBeforeRefusal || len(afterRefusal.Experience.Uses) != usesBeforeRefusal {
		return report, errors.New("a refused adoption changed the experience record")
	}
	_, infoC, err := engine.Session(ctx, sessionC.ID)
	if err != nil {
		return report, fmt.Errorf("read refusal session: %w", err)
	}

	// Durability: close the Engine, reopen the same bound state and read the history again.
	if err := engine.Close(); err != nil {
		return report, fmt.Errorf("close before reopen: %w", err)
	}
	engineOpen = false
	engine, err = openEngine(ctx, paths.workspace, paths.repoDir)
	if err != nil {
		return report, fmt.Errorf("reopen engine: %w", err)
	}
	engineOpen = true
	reopened, err := engine.ReadExperience(ctx, sessionB.ID, validated.ID, 0)
	if err != nil {
		return report, fmt.Errorf("read experience after reopen: %w", err)
	}
	if reopened.Version != feedbackNode.Version || reopened.Experience == nil || reopened.Experience.State != "validated" {
		return report, errors.New("experience history did not survive Engine reopen")
	}
	reopenedOutcome := ""
	for _, use := range reopened.Experience.Uses {
		if use.ID == adoption.ID && use.Feedback != nil {
			reopenedOutcome = use.Feedback.Outcome
		}
	}
	if reopenedOutcome != "success" {
		return report, fmt.Errorf("reopened feedback outcome=%q, want success", reopenedOutcome)
	}
	recalledAfterReopen, err := engine.RecallExperiences(ctx, sessionB.ID, problemPattern, 5)
	if err != nil {
		return report, fmt.Errorf("recall after reopen: %w", err)
	}
	if !containsExperience(recalledAfterReopen, validated.ID) {
		return report, errors.New("experience was not recallable after reopen")
	}

	report = demoReport{
		OK:   true,
		Mode: "offline-scripted-experience",
		Isolation: isolationEvidence{
			ScratchGitRepo: true,
			LocalConfig:    true,
			LocalState:     true,
			NetworkUsed:    false,
			Provider:       "deterministic in-process ProviderFactory; no HTTP client",
			ModelRef:       string(modelRef),
			ApprovedChecks: approved,
		},
		Sessions: sessionsEvidence{
			A: sessionResult{
				ID: sessionA.ID, Goal: "Create the deterministic release marker in the scratch workspace.",
				Status: infoA.Status, ResultStatus: resultA.Status,
				RunID: extracted.Experience.Origin.Source.RunID,
			},
			B: sessionResult{
				ID: sessionB.ID, Goal: "Apply the recalled conditional strategy to a fresh release marker task.",
				Status: resultB.Status, ResultStatus: resultB.Status, RunID: feedbackRunID,
			},
			Refusal: sessionResult{
				ID: sessionC.ID, Goal: "Try to reuse the strategy without the gate marker.",
				Status: infoC.Status,
			},
		},
		Experience: experienceEvidence{
			ID:                reopened.ID,
			OriginSessionID:   reopened.Experience.Origin.Source.SessionID,
			OriginRunID:       reopened.Experience.Origin.Source.RunID,
			State:             reopened.Experience.State,
			NodeVersion:       reopened.Version,
			DefinitionVersion: reopened.Experience.DefinitionVersion,
			Confidence:        reopened.Experience.Confidence,
			ProblemPattern:    reopened.Experience.Definition.ProblemPattern,
			ExpectedResult:    reopened.Experience.Definition.ExpectedResult,
			Status:            reopened.Status,
		},
		Extraction: extractionEvidence{
			ScopeKind:        projectScope,
			CandidateState:   extracted.Experience.State,
			CandidateVersion: extracted.Version,
			OriginOutcome:    extracted.Experience.Origin.Outcome,
			ToolStepCount:    len(extracted.Experience.Origin.Trace.Steps),
			CheckResultCount: len(extracted.Experience.Origin.Trace.Checks),
			ToolSteps:        toolSteps(extracted),
			CheckResults:     checkResults(extracted),
			TraceIncomplete:  extracted.Experience.Origin.Trace.Incomplete,
			TraceTruncated:   extracted.Experience.Origin.Trace.Truncated,
		},
		Validation: validationEvidence{
			State:            validated.Experience.State,
			Reason:           validated.Experience.Validation.Reason,
			NodeVersion:      validated.Version,
			ConditionCount:   len(validated.Experience.Validation.Conditions),
			Conditions:       observations(validated.Experience.Validation.Conditions),
			ExpectedOutcome:  validated.Experience.Definition.ExpectedOutcome.Render(),
			ExpectedResult:   validated.Experience.Definition.ExpectedResult,
			ObservedResult:   validated.Experience.Validation.Outcome.Evidence.Verification,
			OutcomeQualified: validated.Experience.Validation.Outcome.Evidence.Verification == validated.Experience.Definition.ExpectedResult,
		},
		Recall: recallEvidence{
			Count:   len(matches),
			Matches: matches,
		},
		Adoption: adoptionEvidence{
			ID:                adoption.ID,
			ExperienceID:      adoption.ExperienceID,
			NodeVersion:       adoption.Version,
			DefinitionVersion: adoption.DefinitionVersion,
			Strategy:          adoption.Strategy,
			ConditionCount:    len(adoption.Conditions),
			Conditions:        observations(adoption.Conditions),
			ExpectedOutcome:   adoption.ExpectedOutcome.Render(),
			ExpectedResult:    adoption.ExpectedResult,
			Uncertainty:       adoption.Uncertainty,
		},
		SecondRun: secondRunEvidence{
			SessionID:               sessionB.ID,
			Status:                  resultB.Status,
			RunID:                   feedbackRunID,
			ExpectedOutcomeVerified: feedbackVerification == "passed",
			OriginOutputCleared:     true,
			FreshOutput:             string(output),
		},
		Feedback: feedbackEvidence{
			Outcome:          feedbackOutcome,
			Reason:           feedbackReason,
			Used:             feedbackUsed,
			ConditionsHeld:   feedbackConditionsHeld,
			RevisionNeeded:   feedbackRevisionNeeded,
			StateAfter:       feedbackNode.Experience.State,
			NodeVersionAfter: feedbackNode.Version,
			Statistics:       feedbackNode.Experience.Statistics(),
			SourceRunID:      feedbackRunID,
			Verification:     feedbackVerification,
		},
		Reopen: reopenEvidence{
			Readable:          true,
			State:             reopened.Experience.State,
			NodeVersion:       reopened.Version,
			DefinitionVersion: reopened.Experience.DefinitionVersion,
			Uses:              len(reopened.Experience.Uses),
			FeedbackOutcome:   reopenedOutcome,
			Statistics:        reopened.Experience.Statistics(),
			Recallable:        true,
		},
		WrongCondition: wrongConditionEvidence{
			Refused:       true,
			SessionID:     sessionC.ID,
			Error:         refusalErr.Error(),
			VersionBefore: versionBeforeRefusal,
			VersionAfter:  afterRefusal.Version,
			UsesBefore:    usesBeforeRefusal,
			UsesAfter:     len(afterRefusal.Experience.Uses),
		},
		Mechanism: mechanismEvidence{
			Result:          "conditional_reuse_verified",
			BenefitMeasured: false,
			Statement:       "The demo proves the deterministic reuse mechanism: conditions held at adoption, the consumer's later ordinary run matched the approved expected outcome, and feedback was recorded. It does not measure a model-quality or task-transfer benefit.",
		},
	}
	return report, nil
}

// deterministicProvider is a scripted local model. It emits one write_file tool call, then a final
// message once its call appears in the request transcript. It never opens a socket.
type deterministicProvider struct{}

func (deterministicProvider) Model() string { return "offline-scripted" }

func (deterministicProvider) Complete(_ context.Context, request protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	wrote := false
	for _, message := range request.Messages {
		for _, call := range message.ToolCalls {
			if call.Name == "write_file" || call.Name == "apply_experience" {
				wrote = true
			}
		}
	}
	if !wrote {
		for _, message := range request.Messages {
			const prefix = "Apply this adopted procedure through apply_experience: "
			if message.Role == "user" && strings.HasPrefix(message.Content, prefix) {
				return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "call_apply_marker", Name: "apply_experience", Arguments: json.RawMessage(strings.TrimPrefix(message.Content, prefix))}}}, FinishReason: "tool_calls"}, nil
			}
		}
		return protocol.ProviderResponse{
			Message: protocol.Message{
				Role: "assistant",
				ToolCalls: []protocol.ToolCall{{
					ID:        "call_write_marker",
					Name:      "write_file",
					Arguments: json.RawMessage(`{"path":"output.txt","content":"status=ready\n"}`),
				}},
			},
			FinishReason: "tool_calls",
		}, nil
	}
	return protocol.ProviderResponse{
		Message:      protocol.Message{Role: "assistant", Content: "The marker was written and the approved check decides completion."},
		FinishReason: "stop",
	}, nil
}

// offlineProviderFactory is the sanctioned host injection point: the catalog supplies model
// identity, while this process supplies the local client and performs no network request.
func offlineProviderFactory(model config.ResolvedModel) (protocol.Provider, error) {
	if model.ProviderID != "demo" || model.ModelID != "offline" {
		return nil, fmt.Errorf("offline demo refuses model %s", model.Ref)
	}
	return deterministicProvider{}, nil
}

func openEngine(ctx context.Context, workspace, repoDir string) (*lcx.Engine, error) {
	engine, err := lcx.OpenContext(ctx, lcx.Options{Workspace: workspace, RepoDir: repoDir, ProviderFactory: offlineProviderFactory})
	if err != nil {
		return nil, err
	}
	if err := engine.EnableCognition(ctx); err != nil {
		return nil, errors.Join(err, engine.Close())
	}
	return engine, nil
}

// approveChecksAndBindings approves the two local check definitions and binds the exact assertions
// the experience will use. The demo is the host here; no agent can approve or bind.
func approveChecksAndBindings(ctx context.Context, engine *lcx.Engine) ([]string, error) {
	state, err := engine.CheckApprovalState(ctx)
	if err != nil {
		return nil, fmt.Errorf("check approval state: %w", err)
	}
	approvedState, err := engine.ApproveChecks(ctx, lcx.ApproveChecksRequest{
		SetDigest:  state.SetDigest,
		ApprovedBy: lcx.ApprovedByUser,
		Actor:      "offline-demo-host",
	})
	if err != nil {
		return nil, fmt.Errorf("approve checks: %w", err)
	}
	digests := make(map[string]string, len(approvedState.Checks))
	ids := make([]string, 0, len(approvedState.Checks))
	for _, item := range approvedState.Checks {
		digests[item.Spec.ID] = item.SpecDigest
		ids = append(ids, item.Spec.ID)
	}
	if err := approveBinding(ctx, engine, digests, lcx.CognitiveAssertion{
		Kind: "file_exists", Target: "gate.flag", Value: "present",
	}, "gate"); err != nil {
		return nil, err
	}
	if err := approveBinding(ctx, engine, digests, lcx.CognitiveAssertion{
		Kind: "config_value", Target: "output.txt", Key: "status", Value: "ready",
	}, "outcome"); err != nil {
		return nil, err
	}
	return ids, nil
}

func approveBinding(ctx context.Context, engine *lcx.Engine, digests map[string]string, assertion lcx.CognitiveAssertion, checkID string) error {
	specDigest, ok := digests[checkID]
	if !ok {
		return fmt.Errorf("approved check set has no %s", checkID)
	}
	if _, err := engine.ApproveConstraintBinding(ctx, lcx.ApproveBindingRequest{
		ConstraintRef:    assertion.Ref(),
		ConstraintDigest: assertion.ValueDigest(),
		CheckID:          checkID,
		SpecDigest:       specDigest,
		Scope:            projectScope,
		ApprovedBy:       lcx.ApprovedByUser,
		Actor:            "offline-demo-host",
	}); err != nil {
		return fmt.Errorf("bind %s to %s: %w", assertion.Ref(), checkID, err)
	}
	return nil
}

// scratchPaths keeps every file this process owns under root. None is derived from the caller's
// home, state or workspace.
type scratchPaths struct {
	root       string
	workspace  string
	configHome string
	stateHome  string
	home       string
	tmp        string
	repoDir    string
}

func prepareScratch(root string) (scratchPaths, error) {
	paths := scratchPaths{
		root:       root,
		workspace:  filepath.Join(root, "workspace"),
		configHome: filepath.Join(root, "config"),
		stateHome:  filepath.Join(root, "state"),
		home:       filepath.Join(root, "home"),
		tmp:        filepath.Join(root, "tmp"),
		repoDir:    filepath.Join(root, "state", "repo"),
	}
	for _, dir := range []string{paths.workspace, paths.configHome, paths.stateHome, paths.home, paths.tmp, paths.repoDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return scratchPaths{}, fmt.Errorf("create scratch directory: %w", err)
		}
	}

	globalConfig := map[string]any{
		"model":             string(modelRef),
		"trustedWorkspaces": []string{paths.workspace},
		"providers": map[string]any{
			"demo": map[string]any{
				// The injected factory never dials this loopback address; the catalog still needs
				// a syntactically valid provider entry.
				"settings": map[string]any{"baseURL": "http://127.0.0.1:9/v1"},
				"models": map[string]any{
					"offline": map[string]any{"modelID": "offline", "toolCalling": true},
				},
			},
		},
	}
	globalRaw, err := json.Marshal(globalConfig)
	if err != nil {
		return scratchPaths{}, fmt.Errorf("encode scratch config: %w", err)
	}
	globalDir := filepath.Join(paths.configHome, "lumencortex")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		return scratchPaths{}, fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, config.FileName), globalRaw, 0o600); err != nil {
		return scratchPaths{}, fmt.Errorf("write scratch config: %w", err)
	}

	workspaceConfig := map[string]any{
		"checks": map[string]any{
			"gate": map[string]any{
				"kind": "test", "command": "sh gate.sh", "required": false,
			},
			"outcome": map[string]any{
				"kind": "test", "command": "sh check.sh", "required": true,
			},
		},
	}
	workspaceRaw, err := json.Marshal(workspaceConfig)
	if err != nil {
		return scratchPaths{}, fmt.Errorf("encode workspace config: %w", err)
	}
	files := map[string]string{
		config.FileName: string(workspaceRaw),
		"README.md":     "Disposable offline Experience demo fixture.\n",
		"gate.flag":     "",
		"gate.sh":       "#!/bin/sh\nset -eu\ntest -f gate.flag\n",
		"check.sh":      "#!/bin/sh\nset -eu\ntest \"$(cat output.txt 2>/dev/null)\" = \"status=ready\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(paths.workspace, name), []byte(body), 0o600); err != nil {
			return scratchPaths{}, fmt.Errorf("write workspace fixture: %w", err)
		}
	}
	if err := initGit(paths.workspace); err != nil {
		return scratchPaths{}, err
	}
	return paths, nil
}

func initGit(workspace string) error {
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=LumenCortex Offline Demo", "-c", "user.email=offline-demo@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "offline experience demo fixture"},
	} {
		command := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		command.Env = append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_CONFIG_SYSTEM="+os.DevNull,
		)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func installEnv(values map[string]string) func() {
	type saved struct {
		key   string
		value string
		set   bool
	}
	old := make([]saved, 0, len(values))
	for key, value := range values {
		previous, set := os.LookupEnv(key)
		old = append(old, saved{key: key, value: previous, set: set})
		_ = os.Setenv(key, value)
	}
	return func() {
		for _, item := range old {
			if item.set {
				_ = os.Setenv(item.key, item.value)
			} else {
				_ = os.Unsetenv(item.key)
			}
		}
	}
}

func containsExperience(matches []lcx.ExperienceMatch, id string) bool {
	for _, match := range matches {
		if match.ID == id {
			return true
		}
	}
	return false
}

func experienceState(node lcx.CognitiveNode) string {
	if node.Experience == nil {
		return ""
	}
	return node.Experience.State
}

func observations(items []lcx.ExperienceObservation) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Assertion.Render()+" -> "+item.Evidence.Verification)
	}
	return out
}

func toolSteps(node lcx.CognitiveNode) []string {
	if node.Experience == nil {
		return nil
	}
	out := make([]string, 0, len(node.Experience.Origin.Trace.Steps))
	for _, step := range node.Experience.Origin.Trace.Steps {
		out = append(out, step.Tool+":"+step.Outcome)
	}
	return out
}

func checkResults(node lcx.CognitiveNode) []string {
	if node.Experience == nil {
		return nil
	}
	out := make([]string, 0, len(node.Experience.Origin.Trace.Checks))
	for _, check := range node.Experience.Origin.Trace.Checks {
		out = append(out, fmt.Sprintf("%s:%s:qualified=%t", check.CheckID, check.Outcome, check.Qualified))
	}
	return out
}

func scrub(message, root string) string {
	if root != "" {
		message = strings.ReplaceAll(message, root, "<scratch>")
	}
	return message
}

type demoReport struct {
	OK             bool                   `json:"ok"`
	Mode           string                 `json:"mode"`
	Cleanup        cleanupEvidence        `json:"cleanup"`
	Isolation      isolationEvidence      `json:"isolation"`
	Sessions       sessionsEvidence       `json:"sessions"`
	Experience     experienceEvidence     `json:"experience"`
	Extraction     extractionEvidence     `json:"extraction"`
	Validation     validationEvidence     `json:"validation"`
	Recall         recallEvidence         `json:"recall"`
	Adoption       adoptionEvidence       `json:"adoption"`
	SecondRun      secondRunEvidence      `json:"secondRun"`
	Feedback       feedbackEvidence       `json:"feedback"`
	Reopen         reopenEvidence         `json:"reopen"`
	WrongCondition wrongConditionEvidence `json:"wrongConditionRefusal"`
	Mechanism      mechanismEvidence      `json:"mechanism"`
}

type cleanupEvidence struct {
	Removed bool `json:"removed"`
}

type isolationEvidence struct {
	ScratchGitRepo bool     `json:"scratchGitRepo"`
	LocalConfig    bool     `json:"localConfig"`
	LocalState     bool     `json:"localState"`
	NetworkUsed    bool     `json:"networkUsed"`
	Provider       string   `json:"provider"`
	ModelRef       string   `json:"modelRef"`
	ApprovedChecks []string `json:"approvedChecks"`
}

type sessionsEvidence struct {
	A       sessionResult `json:"a"`
	B       sessionResult `json:"b"`
	Refusal sessionResult `json:"refusal"`
}

type sessionResult struct {
	ID           string `json:"id"`
	Goal         string `json:"goal"`
	Status       string `json:"status"`
	ResultStatus string `json:"resultStatus,omitempty"`
	RunID        string `json:"runId,omitempty"`
}

type experienceEvidence struct {
	ID                string `json:"id"`
	OriginSessionID   string `json:"originSessionId"`
	OriginRunID       string `json:"originRunId"`
	State             string `json:"state"`
	NodeVersion       int64  `json:"nodeVersion"`
	DefinitionVersion int64  `json:"definitionVersion"`
	Confidence        string `json:"confidence"`
	ProblemPattern    string `json:"problemPattern"`
	ExpectedResult    string `json:"expectedResult"`
	Status            string `json:"status"`
}

type extractionEvidence struct {
	ScopeKind        string   `json:"scopeKind"`
	CandidateState   string   `json:"candidateState"`
	CandidateVersion int64    `json:"candidateVersion"`
	OriginOutcome    string   `json:"originOutcome"`
	ToolStepCount    int      `json:"toolStepCount"`
	CheckResultCount int      `json:"checkResultCount"`
	ToolSteps        []string `json:"toolSteps"`
	CheckResults     []string `json:"checkResults"`
	TraceIncomplete  bool     `json:"traceIncomplete"`
	TraceTruncated   bool     `json:"traceTruncated"`
}

type validationEvidence struct {
	State            string   `json:"state"`
	Reason           string   `json:"reason"`
	NodeVersion      int64    `json:"nodeVersion"`
	ConditionCount   int      `json:"conditionCount"`
	Conditions       []string `json:"conditions"`
	ExpectedOutcome  string   `json:"expectedOutcome"`
	ExpectedResult   string   `json:"expectedResult"`
	ObservedResult   string   `json:"observedResult"`
	OutcomeQualified bool     `json:"outcomeQualified"`
}

type recallEvidence struct {
	Count   int                   `json:"count"`
	Matches []lcx.ExperienceMatch `json:"matches"`
}

type adoptionEvidence struct {
	ID                string   `json:"id"`
	ExperienceID      string   `json:"experienceId"`
	NodeVersion       int64    `json:"nodeVersion"`
	DefinitionVersion int64    `json:"definitionVersion"`
	Strategy          []string `json:"strategy"`
	ConditionCount    int      `json:"conditionCount"`
	Conditions        []string `json:"conditions"`
	ExpectedOutcome   string   `json:"expectedOutcome"`
	ExpectedResult    string   `json:"expectedResult"`
	Uncertainty       string   `json:"uncertainty"`
}

type secondRunEvidence struct {
	SessionID               string `json:"sessionId"`
	Status                  string `json:"status"`
	RunID                   string `json:"runId"`
	ExpectedOutcomeVerified bool   `json:"expectedOutcomeVerified"`
	OriginOutputCleared     bool   `json:"originOutputCleared"`
	FreshOutput             string `json:"freshOutput"`
}

type feedbackEvidence struct {
	Outcome          string                   `json:"outcome"`
	Reason           string                   `json:"reason"`
	Used             bool                     `json:"used"`
	ConditionsHeld   bool                     `json:"conditionsHeld"`
	RevisionNeeded   bool                     `json:"revisionNeeded"`
	StateAfter       string                   `json:"stateAfter"`
	NodeVersionAfter int64                    `json:"nodeVersionAfter"`
	Statistics       lcx.ExperienceStatistics `json:"statistics"`
	SourceRunID      string                   `json:"sourceRunId"`
	Verification     string                   `json:"verification"`
}

type reopenEvidence struct {
	Readable          bool                     `json:"readable"`
	State             string                   `json:"state"`
	NodeVersion       int64                    `json:"nodeVersion"`
	DefinitionVersion int64                    `json:"definitionVersion"`
	Uses              int                      `json:"uses"`
	FeedbackOutcome   string                   `json:"feedbackOutcome"`
	Statistics        lcx.ExperienceStatistics `json:"statistics"`
	Recallable        bool                     `json:"recallable"`
}

type wrongConditionEvidence struct {
	Refused       bool   `json:"refused"`
	SessionID     string `json:"sessionId"`
	Error         string `json:"error"`
	VersionBefore int64  `json:"versionBefore"`
	VersionAfter  int64  `json:"versionAfter"`
	UsesBefore    int    `json:"usesBefore"`
	UsesAfter     int    `json:"usesAfter"`
}

type mechanismEvidence struct {
	Result          string `json:"result"`
	BenefitMeasured bool   `json:"benefitMeasured"`
	Statement       string `json:"statement"`
}
