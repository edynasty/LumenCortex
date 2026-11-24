package agent

// Pre-fix overlay only (never committed): the e2e R3-04 test needs the step locator; 2ba79ae lacks it.

import (
	"encoding/json"

	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

var _ = json.Marshal

func latestAtomicStepSeq(transcript []window.Entry) int64 {
	for i := len(transcript) - 1; i >= 0; i-- {
		m := transcript[i].Msg
		if m.Role == "assistant" || (m.Role == "user" && m.Origin == protocol.OriginCompletionBlocked) {
			return transcript[i].Seq
		}
	}
	return -1
}
