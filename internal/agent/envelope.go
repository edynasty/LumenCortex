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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// Every provider request of a run has a durable envelope before it is sent (ADR-0018, ADR-0024):
// the identity of the request and the visible parts it is made of, never their text. The envelope is
// written by Store.AdmitRequest immediately before Client.Complete; the answer and the envelope's
// answered state commit together in Store.AppendAnswered; every other end of the attempt settles the
// envelope as unknown, because a provider error does not prove that nothing was delivered.

// projectionVersion identifies the rule that turns a candidate projection into refs.
// Version 2 records terminal-sealed lossless ModelRef chunks first, proj:mode, transmitted system
// layers, ts:v<version>, and rendered-only Task State ItemRefs (ADR-0024 §10).
const projectionVersion = 2

// MaxModelRefChunks is the proven upper bound on chunks produced by EncodeEnvelopeModelRef for any
// valid UTF-8 provider/model within 64B + 1B + 192B = 257B raw / <= 769B escaped (RC33-F03).
const MaxModelRefChunks = 10

// ErrIncompleteModelRefChunks reports a multi-chunk ModelRef sequence missing its terminal ".~" seal.
var ErrIncompleteModelRefChunks = errors.New("incomplete model ref chunks: missing terminal seal")

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

func modelRefPassthrough(b byte) bool {
	if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') {
		return true
	}
	switch b {
	case '.', '_', ':', '/', '@', '#', '+', '=', '-':
		return true
	}
	return false
}

const upperHex = "0123456789ABCDEF"

// EncodeEnvelopeModelRef encodes ref losslessly into 1..10 envelope refs matching session.refRE.
// Escaped payloads <= 90B emit ["model:<escaped>"]. Longer payloads split on complete original UTF-8
// rune boundaries into "model~0~<c0>" .. "model~<K-1>.~<c_{K-1}>", where the unique ".~" seal marks
// the final chunk and prevents partial decoding on truncated slices (REV-F03, REV-F07, RC33-F03).
func EncodeEnvelopeModelRef(ref config.ModelRef) ([]string, error) {
	s := string(ref)
	if !utf8.ValidString(s) {
		return nil, errors.New("model ref is not valid UTF-8")
	}
	prov, model, ok := ref.Split()
	if !ok {
		return nil, fmt.Errorf("invalid model ref %q", ref)
	}
	if len(prov) > config.MaxProviderIDBytes {
		return nil, fmt.Errorf("provider id exceeds %d UTF-8 bytes", config.MaxProviderIDBytes)
	}
	if len(model) > config.MaxModelKeyBytes {
		return nil, fmt.Errorf("model key exceeds %d UTF-8 bytes", config.MaxModelKeyBytes)
	}

	var esc strings.Builder
	esc.Grow(len(s) * 2)
	runeBounds := make([]int, 0, len(s)+1)
	runeBounds = append(runeBounds, 0)
	for i := 0; i < len(s); {
		_, width := utf8.DecodeRuneInString(s[i:])
		for k := 0; k < width; k++ {
			b := s[i+k]
			if modelRefPassthrough(b) {
				esc.WriteByte(b)
			} else {
				esc.WriteByte('%')
				esc.WriteByte(upperHex[b>>4])
				esc.WriteByte(upperHex[b&0x0f])
			}
		}
		i += width
		runeBounds = append(runeBounds, esc.Len())
	}

	escaped := esc.String()
	const (
		singleCap = session.MaxEnvelopeRefBytes - len("model:")    // 90
		capCont   = session.MaxEnvelopeRefBytes - len("model~0~")  // 88
		capTerm   = session.MaxEnvelopeRefBytes - len("model~0.~") // 87
	)
	if len(escaped) <= singleCap {
		return []string{"model:" + escaped}, nil
	}

	var chunks []string
	pos := 0
	for idx := 0; pos < len(escaped); idx++ {
		if idx >= MaxModelRefChunks {
			return nil, fmt.Errorf("encoded model ref exceeds %d chunks", MaxModelRefChunks)
		}
		rem := len(escaped) - pos
		if rem <= capTerm {
			chunks = append(chunks, "model~"+strconv.Itoa(idx)+".~"+escaped[pos:])
			pos = len(escaped)
			break
		}
		maxEnd := pos + min(capCont, rem-1)
		end := -1
		for _, b := range runeBounds {
			if b > pos && b <= maxEnd {
				end = b
			} else if b > maxEnd {
				break
			}
		}
		if end <= pos {
			return nil, errors.New("cannot split model ref on UTF-8 boundary")
		}
		chunks = append(chunks, "model~"+strconv.Itoa(idx)+"~"+escaped[pos:end])
		pos = end
	}
	if len(chunks) < 2 || len(chunks) > MaxModelRefChunks {
		return nil, fmt.Errorf("invalid model ref chunk count %d", len(chunks))
	}
	return chunks, nil
}

// DecodeEnvelopeModelRef decodes a candidate ModelRef offline from an envelope's Refs without
// consulting any configuration table. Missing terminal seals return ErrIncompleteModelRefChunks.
func DecodeEnvelopeModelRef(refs []string) (config.ModelRef, error) {
	var singles []string
	var chunked []string
	for _, r := range refs {
		switch {
		case strings.HasPrefix(r, "model:"):
			singles = append(singles, r)
		case strings.HasPrefix(r, "model~"):
			chunked = append(chunked, r)
		}
	}
	if len(singles) == 0 && len(chunked) == 0 {
		return "", errors.New("envelope refs contain no model identity")
	}
	if len(singles) > 0 && len(chunked) > 0 {
		return "", errors.New("envelope refs mix single and chunked model encodings")
	}
	if len(singles) > 0 {
		if len(singles) != 1 {
			return "", fmt.Errorf("envelope refs contain %d single model entries", len(singles))
		}
		payload := singles[0][len("model:"):]
		if payload == "" || len(singles[0]) > session.MaxEnvelopeRefBytes || strings.ContainsRune(payload, '~') {
			return "", errors.New("invalid single model ref entry")
		}
		decoded, err := unescapeModelRefChunk(payload)
		if err != nil {
			return "", err
		}
		return validateDecodedModelRef(decoded)
	}

	if len(chunked) < 2 || len(chunked) > MaxModelRefChunks {
		if len(chunked) == 1 && !strings.Contains(chunked[0], ".~") {
			return "", ErrIncompleteModelRefChunks
		}
		return "", fmt.Errorf("invalid chunked model ref count %d", len(chunked))
	}
	var full strings.Builder
	for i, entry := range chunked {
		if len(entry) > session.MaxEnvelopeRefBytes || len(entry) < len("model~0~")+1 {
			return "", fmt.Errorf("invalid model ref chunk %d", i)
		}
		d := entry[6]
		if d < '0' || d > '9' || int(d-'0') != i {
			return "", fmt.Errorf("out-of-order or invalid model ref chunk index at %d: %q", i, entry)
		}
		var (
			terminal bool
			payload  string
		)
		switch {
		case strings.HasPrefix(entry[7:], ".~"):
			terminal = true
			payload = entry[9:]
		case entry[7] == '~':
			terminal = false
			payload = entry[8:]
		default:
			return "", fmt.Errorf("malformed model ref chunk header %q", entry)
		}
		if payload == "" || strings.ContainsRune(payload, '~') {
			return "", fmt.Errorf("invalid model ref chunk payload %q", entry)
		}
		if i < len(chunked)-1 && terminal {
			return "", errors.New("premature terminal seal before final model ref chunk")
		}
		if i == len(chunked)-1 && !terminal {
			return "", ErrIncompleteModelRefChunks
		}
		part, err := unescapeModelRefChunk(payload)
		if err != nil {
			return "", err
		}
		full.WriteString(part)
	}
	return validateDecodedModelRef(full.String())
}

func unescapeModelRefChunk(escaped string) (string, error) {
	var raw []byte
	raw = make([]byte, 0, len(escaped))
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if c == '%' {
			if i+2 >= len(escaped) {
				return "", errors.New("truncated percent escape in model ref")
			}
			hi, ok1 := fromHexDigit(escaped[i+1])
			lo, ok2 := fromHexDigit(escaped[i+2])
			if !ok1 || !ok2 {
				return "", errors.New("invalid percent escape in model ref")
			}
			raw = append(raw, (hi<<4)|lo)
			i += 2
			continue
		}
		if !modelRefPassthrough(c) {
			return "", fmt.Errorf("disallowed byte %q in model ref", c)
		}
		raw = append(raw, c)
	}
	if !utf8.Valid(raw) {
		return "", errors.New("unescaped model ref chunk is not valid UTF-8")
	}
	return string(raw), nil
}

func fromHexDigit(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	default:
		return 0, false
	}
}

func validateDecodedModelRef(raw string) (config.ModelRef, error) {
	if !utf8.ValidString(raw) {
		return "", errors.New("decoded model ref is not valid UTF-8")
	}
	ref := config.ModelRef(raw)
	prov, model, ok := ref.Split()
	if !ok {
		return "", fmt.Errorf("decoded model ref %q is malformed", raw)
	}
	if len(prov) > config.MaxProviderIDBytes || len(model) > config.MaxModelKeyBytes {
		return "", errors.New("decoded model ref exceeds UTF-8 byte limits")
	}
	return ref, nil
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

// requestRefs constructs PurposeMain envelope references in strict P0..P4 priority order:
//
//	P0: modelRefs (1..10 chunks, never truncated)
//	P1: proj:mode:<full|compact> (1)
//	P2: sys:prompt:<h> + SentLayers sys:<kind>:<h> (<=3) + sys:instructions:<h> (<=1)
//	P3: ts:v<version> (1)
//	P4: renderedStateRefs from taskstate.RenderBlock.ItemRefs (startByteOffset < cutoff)
//
// P0..P3 occupy at most 10 + 1 + 5 + 1 = 17 slots, leaving >= 111 slots for P4 before MaxEnvelopeRefs (128).
func requestRefs(modelRefs []string, opts Options, sentLayers []SystemLayer, mode ProjectionMode, version int64, renderedStateRefs []string) []string {
	if mode == "" {
		mode = ModeFull
	}
	refs := make([]string, 0, len(modelRefs)+8+len(renderedStateRefs))
	refs = append(refs, modelRefs...)
	refs = append(refs, "proj:mode:"+string(mode))
	refs = append(refs, "sys:prompt:"+shortDigest(opts.SystemPrompt))
	for _, layer := range sentLayers {
		if layer.Text == "" {
			continue
		}
		refs = append(refs, "sys:"+refID(layer.Kind)+":"+shortDigest(layer.Text))
	}
	if opts.Instructions != "" {
		refs = append(refs, "sys:instructions:"+shortDigest(opts.Instructions))
	}
	refs = append(refs, fmt.Sprintf("ts:v%d", version))
	refs = append(refs, renderedStateRefs...)
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

// admit writes the envelope of one candidate attempt. ModelRef chunks are validated up front (1..10)
// so a candidate's model identity is never truncated to make room for lower-priority refs.
func (l *Loop) admit(
	ctx context.Context,
	sessionID string,
	step int64,
	attempt int,
	opts Options,
	modelRef config.ModelRef,
	sentLayers []SystemLayer,
	mode ProjectionMode,
	taskStateVersion int64,
	renderedStateRefs []string,
	built window.Result,
	specs []protocol.ToolSpec,
) (string, bool, *ExitCause) {
	id, err := newRequestID()
	if err != nil {
		return "", false, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
	}
	if built.FirstSeq < 0 {
		// No transcript row reaches the model: there is no window to record, and a request that
		// cannot be described is not sent.
		return "", false, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: errors.New("request has no transcript window")}
	}
	modelRefs, err := EncodeEnvelopeModelRef(modelRef)
	if err != nil || len(modelRefs) == 0 || len(modelRefs) > MaxModelRefChunks {
		if err == nil {
			err = fmt.Errorf("model ref %q produced %d chunks", modelRef, len(modelRefs))
		}
		return "", false, &ExitCause{Status: StatusFailed, Kind: KindConfig, Err: err}
	}
	first, last := built.FirstSeq, built.LastSeq
	adm := session.RequestAdmission{
		RequestID: id, SessionID: sessionID, RunID: opts.RunID, Step: int(step), Attempt: attempt,
		Purpose: session.PurposeMain, FirstSeq: &first, LastSeq: &last,
		Refs:              requestRefs(modelRefs, opts, sentLayers, mode, taskStateVersion, renderedStateRefs),
		ToolSpecsDigest:   toolSpecsDigest(specs),
		ProjectionVersion: projectionVersion,
		LineageComplete:   true,
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
