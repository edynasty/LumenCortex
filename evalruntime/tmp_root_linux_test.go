//go:build linux

package evalruntime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxRuntimeRootUnderPrivateTmp(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "lc-private-root-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	h, _ := newPythonHarness(t, func(opts *Options) { opts.Root = filepath.Join(root, "state") })
	s := h.create("python", "owner", "owner")
	r := h.expectCompleted(s, "owner", "kept = 43\nkept")
	if len(r.Displays) != 1 || r.Displays[0].Text != "43" {
		t.Fatalf("root below /tmp: %+v", r)
	}
}
