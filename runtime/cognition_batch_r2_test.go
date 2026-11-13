package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

func TestCognitionBatchUTF8HardBoundary(t *testing.T) {
	const capBytes = 32768
	catalog := []knowledge.Assertion{{Kind: knowledge.AssertConfigValue, Target: "配置/服务.json", Key: "端口<&>"}}
	serialize := func(n int) string {
		t.Helper()
		projection, err := json.Marshal(map[string]any{"ref": "session:unicode@sha256:0", "scope": "工程/工作区", "metadata": strings.Repeat("中<&>\\\"\n", 80), "thread": strings.Repeat("x", n)})
		if err != nil {
			t.Fatal(err)
		}
		input, err := maintenanceInput([]batchSource{{projection: projection}}, catalog)
		if err != nil {
			t.Fatal(err)
		}
		return input
	}
	base := serialize(0)
	for _, size := range []int{capBytes, capBytes + 1} {
		input := serialize(size - len(base))
		if len(input) != size || !utf8.ValidString(input) {
			t.Fatalf("final bytes/UTF8: got %d want %d", len(input), size)
		}
		_, document, ok := strings.Cut(input, "\n")
		if !ok || !json.Valid([]byte(document)) {
			t.Fatal("serialization cut JSON")
		}
		if fits := batchFits(input); fits != (size == capBytes) {
			t.Fatalf("%d serialized bytes accepted=%v", size, fits)
		}
		t.Logf("actual serialized maintenance input %d bytes; accepted=%v; Unicode, metadata, scope and JSON escapes included", len(input), batchFits(input))
	}
}

func TestCognitionBatchUnicodeCursorRetry(t *testing.T) {
	f := newCognitionFixture(t, true)
	var sources []string
	for i := 0; i < 10; i++ {
		sources = append(sources, f.run(t, fmt.Sprintf("来源%d", i), cogAnswer(strings.Repeat("中文<&>\\\"\n", 400))))
	}
	initial := f.state(t)
	first := f.maintain(t)
	cut := f.state(t)
	if first.Sources < 1 || cut.Cursor >= cut.Dirty || cut.Dirty != initial.Dirty {
		t.Fatalf("fixture did not exercise a bounded cut: %+v %+v", first, cut)
	}
	f.answer = func([]string) string { return "" }
	if _, err := f.engine.MaintainCognition(context.Background()); err == nil {
		t.Fatal("failed maintainer completed")
	}
	if state := f.state(t); state.Cursor != cut.Cursor || state.Dirty != cut.Dirty {
		t.Fatalf("failed retry lost dirty rows: %+v -> %+v", cut, state)
	}
	f.answer = func([]string) string { return `{"candidates":[]}` }
	for i := 0; ; i++ {
		state := f.state(t)
		if state.Cursor == state.Dirty {
			break
		}
		if i >= len(sources) {
			t.Fatalf("cursor did not drain: %+v", state)
		}
		f.maintain(t)
	}
	for _, input := range f.batchInputs() {
		if len(input) > 32768 || !utf8.ValidString(input) {
			t.Fatalf("provider received %d bytes or invalid UTF8", len(input))
		}
		withinCap(t, input)
	}
	f.committedOnce(t, sources)
}
