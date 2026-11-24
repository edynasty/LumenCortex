package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// HEAD-only: apply_experience parent content > 8192 bytes after nested dirty note -> text fallback ->
// resultSessions (which feeds sessionResultEdges/message edges for Fence/privacy) loses sourceSessions.
func TestZZR402DProbeApplyExperienceSources(t *testing.T) {
	big := strings.Repeat("r", 8500)
	inner := []protocol.ToolResult{{OK: true, Content: big}, {OK: false, Content: `{"error":"close failed","commitState":"published_with_post_error"}`}}
	raw, _ := json.Marshal(map[string]any{"applicationId": "app-1", "state": "unknown", "results": inner, "sourceSessions": []string{"s-origin"}})
	base := protocol.ToolResult{OK: false, Content: string(raw)}
	srcBefore, okBefore := resultSessions("apply_experience", "s-own", base)
	after := base
	after.Content = appendNestedPartialPublication(base.Content, []string{"write_file commitState=published_with_post_error dirtyFailure=true partialPublication=true files=[x.txt:published_post_error]"})
	srcAfter, okAfter := resultSessions("apply_experience", "s-own", after)
	t.Logf("PROBE before_len=%d sources=%v ok=%v", len(base.Content), srcBefore, okBefore)
	t.Logf("PROBE after_len=%d valid_json=%v sources=%v ok=%v", len(after.Content), json.Valid([]byte(after.Content)), srcAfter, okAfter)
}
