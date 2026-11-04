package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// Symlink targets are filesystem paths, not lexical expressions: x/.. must visit x
// before .., even when x is missing or is itself a symlink.
func TestDeliveryAbsoluteLinkPreservesTraversal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string, string) string
		want  string
	}{
		{"outside link before parent", func(t *testing.T, root, outside string) string {
			if err := os.Mkdir(filepath.Join(outside, "deep"), 0700); err != nil {
				t.Fatal(err)
			}
			link(t, filepath.Join(outside, "deep"), filepath.Join(root, "jump"))
			return root + "/jump/../real"
		}, taskstate.DeliveryUnavailable},
		{"missing component before parent", func(t *testing.T, root, _ string) string {
			return root + "/missing/../real"
		}, taskstate.DeliveryFailed},
		{"regular component before parent", func(t *testing.T, root, _ string) string {
			write(t, filepath.Join(root, "file"), "not a directory")
			return root + "/file/../real"
		}, taskstate.DeliveryFailed},
		{"inside link before parent", func(t *testing.T, root, _ string) string {
			if err := os.Mkdir(filepath.Join(root, "real", "deep"), 0700); err != nil {
				t.Fatal(err)
			}
			link(t, "real/deep", filepath.Join(root, "jump"))
			return root + "/jump/.."
		}, taskstate.DeliveryVerified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "real"), 0700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(root, "real", "result"), "inside")
			link(t, tc.setup(t, root, outside), filepath.Join(root, "alias"))
			repo, err := New(root)
			if err != nil {
				t.Fatal(err)
			}
			r := repo.VerifyDelivery(context.Background(), &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "alias/result"}}})
			if r.Status != tc.want {
				t.Fatalf("got %+v, want %s", r, tc.want)
			}
		})
	}
}

// Absolute links commonly retain the user's spelling of a workspace (macOS /var
// versus /private/var, or a user-selected workspace symlink).
func TestDeliveryAbsoluteLinkThroughWorkspaceAlias(t *testing.T) {
	container := t.TempDir()
	root := filepath.Join(container, "actual")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(container, "workspace")
	link(t, root, alias)
	if err := os.Mkdir(filepath.Join(root, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "real", "result"), "inside")
	link(t, filepath.Join(alias, "real"), filepath.Join(root, "link"))
	repo, err := New(alias)
	if err != nil {
		t.Fatal(err)
	}
	r := repo.VerifyDelivery(context.Background(), &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "link/result"}}})
	if r.Status != taskstate.DeliveryVerified {
		t.Fatalf("confined absolute alias: %+v", r)
	}
}

// A relative link may leave the workspace through its parent and enter it again under the same or
// another name (link -> ../actual/real, ../workspace/real). Physically that stays inside, so it is
// judged like the absolute spelling; leaving for real, or a missing entry outside, is still unavailable.
func TestDeliveryRelativeLinkThroughWorkspaceParent(t *testing.T) {
	container := t.TempDir()
	root := filepath.Join(container, "actual")
	if err := os.MkdirAll(filepath.Join(root, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	link(t, root, filepath.Join(container, "workspace"))
	write(t, filepath.Join(root, "real", "result"), "inside")
	if err := os.Mkdir(filepath.Join(container, "outside"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(container, "outside", "result"), "outside")
	for _, tc := range []struct {
		target string
		path   string
		absent bool
		want   string
	}{
		{"../actual/real", "result", false, taskstate.DeliveryVerified},
		{"../workspace/real", "result", false, taskstate.DeliveryVerified},
		{"../actual/real", "missing", true, taskstate.DeliveryVerified},
		{"../actual/real/../..", "actual/real/result", false, taskstate.DeliveryVerified},
		{"../outside", "result", false, taskstate.DeliveryUnavailable},
		{"../outside", "missing", true, taskstate.DeliveryUnavailable},
		{"../nowhere", "missing", true, taskstate.DeliveryUnavailable},
	} {
		name := filepath.Join(root, "link")
		_ = os.Remove(name)
		link(t, tc.target, name)
		repo, err := New(root)
		if err != nil {
			t.Fatal(err)
		}
		r := repo.VerifyDelivery(context.Background(), &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "link/" + tc.path, Absent: tc.absent}}})
		if r.Status != tc.want {
			t.Fatalf("link -> %s, %s absent=%t: %+v, want %s", tc.target, tc.path, tc.absent, r, tc.want)
		}
	}
}

// Err is also the snapshot's read checkpoint. Change the directory link at that
// checkpoint, after opening the original file, without timing-dependent sleeps.
type deliveryRetargetContext struct {
	context.Context
	calls    int
	retarget func()
}

func (c *deliveryRetargetContext) Err() error {
	c.calls++
	if c.calls == 2 {
		c.retarget()
	}
	return nil
}

func TestDeliverySnapshotRejectsRetargetedAncestor(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"old", "new"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(root, dir, "result"), dir)
	}
	link(t, "old", filepath.Join(root, "current"))
	repo, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &deliveryRetargetContext{Context: context.Background(), retarget: func() {
		link(t, "new", filepath.Join(root, "next"))
		if err := os.Rename(filepath.Join(root, "next"), filepath.Join(root, "current")); err != nil {
			t.Fatal(err)
		}
	}}
	got, err := repo.deliverySnapshot(ctx, "current/result")
	if ctx.calls < 2 || err == nil || !strings.Contains(err.Error(), "changed during hashing") {
		t.Fatalf("retarget checkpoint=%d result=%+v error=%v", ctx.calls, got, err)
	}
}
