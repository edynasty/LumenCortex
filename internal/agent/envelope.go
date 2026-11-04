package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// Every provider request of a run has a durable envelope before it is sent (ADR-0018, plan T1-S2):
// the identity of the request and the visible parts it is made of, never their text. The envelope is
// written by Store.AdmitRequest immediately before Client.Complete; the answer and the envelope's
// answered state commit together in Store.AppendAnswered; every other end of the attempt settles the
// envelope as unknown, because a provider error does not prove that nothing was delivered.

// projectionVersion identifies the rule that turns a window into refs. It changes when the ref set a
// request is described by changes, so an exposure report can tell which rule produced an envelope.
const projectionVersion = 1

// Reasons the loop settles an envelope with. They are machine codes (the store refuses prose, because
// a provider's error text can echo the request).
const (
	reasonProviderError  = "provider_error"
	reasonCancelled      = "cancelled"
	reasonCapacity       = "capacity_rejected"
	reasonPanic          = "panic"
	reasonAnswerNotSaved = "answer_not_stored"
)

// settleTimeout bounds the write that settles an envelope after the run's context ended or failed.
const settleTimeout = 5 * time.Second

var refSafeRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,48}$`)

// refID makes an item id usable inside a ref: ids the model chose may hold any character, so one that
// does not fit the ref alphabet is replaced by a digest prefix, which is still stable.
func refID(id string) string {
	if refSafeRE.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "h" + hex.EncodeToString(sum[:8])
}

// toolSpecsDigest is the canonical digest of the tools a request offers: name, description and
// parameter schema, the parts the model sees, in the order they are sent. Go marshals map keys sorted,
// so the encoding is canonical.
func toolSpecsDigest(specs []protocol.ToolSpec) string {
	type visible struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}
	list := make([]visible, len(specs))
	for i, s := range specs {
		list[i] = visible{s.Name, s.Description, s.Parameters}
	}
	raw, err := json.Marshal(list)
	if err != nil {
		// Unreachable for a JSON schema; a digest of nothing would claim an empty tool list.
		raw = []byte(fmt.Sprintf("unmarshalable:%d", len(specs)))
	}
	return digestOf(raw)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// requestRefs lists what a request shows besides the transcript window: the Task State version and
// every rendered item, and each system layer by kind and a short digest of its text (its version).
// The list is cut by the store at MaxEnvelopeRefs and the envelope then marked incomplete.
func requestRefs(opts Options, r *run) []string {
	refs := []string{fmt.Sprintf("ts:v%d", r.ts.Version)}
	if r.ts.Brief != "" {
		refs = append(refs, "ts:brief")
	}
	for _, ref := range r.ts.References {
		if ref.State == taskstate.RefIncluded {
			refs = append(refs, "ts:ref:"+refID(ref.ID))
		}
	}
	if r.wf != nil {
		refs = append(refs, "ts:workflow")
	}
	if r.ts.WorkUnits != nil {
		for _, u := range r.ts.Units().List() {
			refs = append(refs, "ts:unit:"+refID(u.ID))
		}
	}
	for _, n := range r.ts.Notes {
		refs = append(refs, "ts:note:"+refID(n.ID))
	}
	for _, t := range r.ts.Tasks {
		refs = append(refs, "ts:task:"+refID(t.ID))
	}
	refs = append(refs, "sys:prompt:"+shortDigest(opts.SystemPrompt))
	for _, layer := range opts.SystemLayers {
		if layer.Text == "" {
			continue
		}
		refs = append(refs, "sys:"+refID(layer.Kind)+":"+shortDigest(layer.Text))
	}
	if opts.Instructions != "" {
		refs = append(refs, "sys:instructions:"+shortDigest(opts.Instructions))
	}
	return refs
}

func shortDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// newRequestID is a 128-bit random request identity. It is never derived from the run, step or
// attempt: ids are never reused, and a restarted run must not be able to collide with an envelope it
// did not write.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "rq-" + hex.EncodeToString(b[:]), nil
}

// admit writes the envelope of one attempt. It returns the request id to send under and whether the
// stored envelope is lineage-complete, or the exit the run takes instead of sending: a fenced session
// ends the run (the store has already committed the failed envelope, and nothing may be sent or
// retried), any other error is a storage failure and fails closed.
func (l *Loop) admit(ctx context.Context, sessionID string, step int64, attempt int, opts Options, r *run, built window.Result, specs []protocol.ToolSpec) (string, bool, *ExitCause) {
	id, err := newRequestID()
	if err != nil {
		return "", false, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
	}
	if built.FirstSeq < 0 {
		// No transcript row reaches the model: there is no window to record, and a request that
		// cannot be described is not sent.
		return "", false, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: errors.New("request has no transcript window")}
	}
	first, last := built.FirstSeq, built.LastSeq
	adm := session.RequestAdmission{
		RequestID: id, SessionID: sessionID, RunID: opts.RunID, Step: int(step), Attempt: attempt,
		Purpose: session.PurposeMain, FirstSeq: &first, LastSeq: &last,
		Refs: requestRefs(opts, r), ToolSpecsDigest: toolSpecsDigest(specs),
		ProjectionVersion: projectionVersion, LineageComplete: true,
	}
	env, err := l.Store.AdmitRequest(ctx, adm)
	if err != nil {
		if errors.Is(err, session.ErrFenced) {
			return "", false, &ExitCause{Status: StatusFailed, Kind: KindFenced, Err: err}
		}
		return "", false, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
	}
	return id, env.LineageComplete, nil
}

// settle ends an admitted envelope that did not end in an answer. It runs on a context that survives
// the run's cancellation, because cancellation is the commonest reason to settle. A settlement that
// cannot be written is left for Reconcile, which turns every admitted envelope into unknown at the
// next start; the loop reports the original error, not this one.
func (l *Loop) settle(sessionID, requestID, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), settleTimeout)
	defer cancel()
	if err := l.Store.SettleRequest(ctx, requestID, session.Settlement{To: session.RequestUnknown, Reason: reason}); err != nil {
		l.emit("request.settle_failed", sessionID, map[string]any{"requestId": requestID, "reason": reason})
	}
}

// settleReason is the machine code for the error that ended a Complete call.
func settleReason(class protocol.ErrorClass) string {
	switch class {
	case protocol.ClassCancelled:
		return reasonCancelled
	case protocol.ClassCapacity:
		return reasonCapacity
	default:
		return reasonProviderError
	}
}

// complete calls the model. A panic inside the client settles the envelope before it propagates to the
// run's panic exit: the request may have been sent.
func (l *Loop) complete(ctx context.Context, sessionID, requestID string, m Model, req protocol.ProviderRequest) (response protocol.ProviderResponse, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			l.settle(sessionID, requestID, reasonPanic)
			panic(recovered)
		}
	}()
	if err := ctx.Err(); err != nil {
		return protocol.ProviderResponse{}, err
	}
	return m.Client.Complete(ctx, req)
}
