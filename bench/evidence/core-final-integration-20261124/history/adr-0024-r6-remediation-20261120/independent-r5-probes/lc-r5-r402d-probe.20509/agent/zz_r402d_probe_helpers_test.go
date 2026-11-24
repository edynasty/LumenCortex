package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// HEAD-only probe of the new helper functions.
func TestZZR402DProbeHelpers(t *testing.T) {
	// 1. appendNestedPartialPublication re-encodes parent JSON through map[string]any.
	parent := `{"z":1,"big":9007199254740993,"stdout":"a<b&c","a":true}`
	got := appendNestedPartialPublication(parent, []string{"write_file commitState=published_with_post_error"})
	t.Logf("PROBE nested_in=%s", parent)
	t.Logf("PROBE nested_out=%s", got)
	// 2. Oversized parent JSON -> text fallback.
	bigParent := `{"stdout":"` + strings.Repeat("x", 9000) + `"}`
	got2 := appendNestedPartialPublication(bigParent, []string{"n1"})
	t.Logf("PROBE nested_big_valid_json=%v tail=%q", json.Valid([]byte(got2)), got2[len(got2)-60:])
	// 3. preserveErrorToolContent: content JSON with error field wins over err text.
	got3 := preserveErrorToolContent(`{"error":"tool-said","commitState":"committed","dirtyFailure":false}`, errors.New("real-err"))
	t.Logf("PROBE preserve_error_key=%s", got3)
	// 4. preserveErrorToolContent: JSON object without error key.
	got4 := preserveErrorToolContent(`{"ok":true}`, errors.New("real-err"))
	t.Logf("PROBE preserve_no_error_key=%s", got4)
	// 5. preserve with JSON array / null / string content.
	for _, c := range []string{`[1,2]`, `null`, `"s"`, "plain\x00text\xff"} {
		out := preserveErrorToolContent(c, errors.New("e\xffrr"))
		t.Logf("PROBE preserve(%q)=%s valid=%v", c, out, json.Valid([]byte(out)))
	}
	// 6. Truncation of multibyte error at 1024 and validity.
	msg := strings.Repeat("界", 2000)
	out := preserveErrorToolContent("", errors.New(msg))
	t.Logf("PROBE preserve_empty_len=%d (no truncation; baseline-equivalent)", len(out))
	long := `{"error":"` + strings.Repeat("界", 4000) + `","commitState":"partially_published","files":[{"path":"a"}]}`
	out2 := preserveErrorToolContent(long, errors.New("x"))
	var o2 map[string]any
	t.Logf("PROBE preserve_long_len=%d valid=%v err=%v keys_files_present=%v", len(out2), json.Unmarshal([]byte(out2), &o2) == nil, nil, o2["files"] != nil)
	// 7. formatDirtyReceiptSummary for each state.
	for _, st := range []editing.CommitState{"", editing.CommitStateRolledBack, editing.CommitStatePartiallyPublished, editing.CommitStatePrePublishFailure} {
		r := editing.CommitReceipt{State: st, DirtyFailure: true, Files: []editing.FileCommitReceipt{{Path: "d/a.txt", PublishState: editing.PublishRolledBack}}}
		t.Logf("PROBE summary(%q)=%s", st, formatDirtyReceiptSummary("apply_patch", r))
	}
	files := make([]editing.FileCommitReceipt, 20)
	for i := range files {
		files[i] = editing.FileCommitReceipt{Path: strings.Repeat("p", 80), PublishState: editing.PublishRollbackFailed}
	}
	s := formatDirtyReceiptSummary("apply_patch", editing.CommitReceipt{State: editing.CommitStatePartiallyPublished, DirtyFailure: true, Files: files})
	t.Logf("PROBE summary_long_len=%d tail=%q", len(s), s[len(s)-40:])
}
