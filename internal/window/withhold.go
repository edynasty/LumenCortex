package window

import "github.com/edynasty/LumenCortex/protocol"

// WithheldContent replaces the content of a row a run must not send: a tool result or task result
// that carried content of a revoked source (DATA_AND_FAILURES §5 step 3).
const WithheldContent = "[runtime] Withheld: this row carried content of a source that was revoked. It is not available to this run; do not try to reconstruct it."

// Withhold returns m projected for a new run: the same role, origin, name, tool call id and tool
// calls - so the row stays in its group, in its place, and every call keeps its answer (I2) - with
// its content replaced by WithheldContent. Nothing is replayed: a withheld tool result is not run
// again. Only content is replaced; a row that carries text elsewhere is not a row a projection
// withholds (the Start refuses to project a thread whose model output may carry the source).
func Withhold(m protocol.Message) protocol.Message {
	m.Content = WithheldContent
	return m
}
