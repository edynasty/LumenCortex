package window

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

func TestEstimateFormulaAndDeterminism(t *testing.T) {
	if got := EstimateBytes(nil); got != 0 {
		t.Fatalf("EstimateBytes(nil)=%d want 0", got)
	}
	if got := EstimateBytes([]byte{}); got != 0 {
		t.Fatalf("EstimateBytes(empty)=%d want 0", got)
	}
	// {"role":"user","content":"abc"}: 18 ASCII letters, 13 ASCII symbols -> floor((4*18+7*13+11)/12)+4 = 18
	m := protocol.Message{Role: "user", Content: "abc"}
	raw, _ := json.Marshal(m)
	want := EstimateBytes(raw)
	if want != 18 || Estimate(m) != want || Estimate(m) != Estimate(m) {
		t.Fatalf("Estimate=%d rawWant=%d (expected 18)", Estimate(m), want)
	}
	if Estimate([]protocol.Message{m, m}) <= Estimate(m) {
		t.Fatal("a longer request must not estimate lower")
	}
}

func TestReserveTokensForBytesGoldenAndMonotonicity(t *testing.T) {
	golden := []struct {
		n    int
		want int
	}{
		{-5, 0},
		{0, 0},
		{1, 5},
		{12, 11},
		{13, 12},
		{58, 38},
		{1200, 704},
		{1456, 854},
		{2458, 1438},
		{6144, 3588},
		{6400, 3738},
		{12346, 7206},
		{36922, 21542},
	}
	for _, tc := range golden {
		if got := ReserveTokensForBytes(tc.n); got != tc.want {
			t.Fatalf("ReserveTokensForBytes(%d)=%d want %d", tc.n, got, tc.want)
		}
	}

	prev := 0
	for n := 0; n <= 13000; n++ {
		cur := ReserveTokensForBytes(n)
		if cur < prev {
			t.Fatalf("non-monotonic ReserveTokensForBytes at n=%d: %d < %d", n, cur, prev)
		}
		prev = cur
	}

	corpora := map[string]string{
		"english":      strings.Repeat("The deterministic projection pipeline preserves repair floors. ", 30),
		"chinese":      strings.Repeat("候选模型确定性上下文投影与因果封签预算守卫。", 30),
		"japanese":     strings.Repeat("候補モデルのコンテキスト予算と決定論的投影検証。", 25),
		"go_code":      strings.Repeat("func (l *Loop) send(ctx context.Context) error { return nil }\n", 30),
		"json_dense":   strings.Repeat(`{"a":[1,2,3],"b":"x\ny\tz","c":true},`, 40),
		"emoji":        strings.Repeat("🚀🔍✅🛠️📦⚡", 60),
		"dense_array":  "[" + strings.Repeat("0,", 900) + "0]",
		"escaped_html": strings.Repeat(`\u003cscript\u003e\n\"\\`, 60),
	}
	for name, sample := range corpora {
		raw := []byte(sample)
		est := EstimateBytes(raw)
		res := ReserveTokensForBytes(len(raw))
		if est <= 4 || est > res {
			t.Fatalf("%s: EstimateBytes(%d bytes)=%d exceeds ReserveTokensForBytes=%d", name, len(raw), est, res)
		}
	}
}

// A stored assistant row whose arguments are not valid JSON must still estimate, not become 0.
func TestEstimateSurvivesInvalidToolArguments(t *testing.T) {
	m := protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "1", Name: "x", Arguments: json.RawMessage(`{broken`)}}}
	if n := Estimate(m); n <= 4 {
		t.Fatalf("Estimate=%d for a message with a broken argument", n)
	}
	if Estimate([]protocol.Message{m}) <= 4 {
		t.Fatal("slice estimate lost the message")
	}
}
