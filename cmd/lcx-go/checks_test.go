package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	lcx "github.com/edynasty/LumenCortex/runtime"
)

// `lcx-go checks` reads and approves through the Engine's approval API (ADR-0017): status prints the
// state the Engine reports, approve refuses a digest the configuration no longer has, and an approval
// it makes is the one the Engine then runs the checks under.
func TestChecksCommandUsesTheEngineApproval(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	globalFile, root := cliEnv(t)
	initCLIGitRepo(t, root)
	writeConfig(t, globalFile, `{"trustedWorkspaces": [`+quote(root)+`]}`)
	project := filepath.Join(root, config.FileName)
	writeConfig(t, project, `{"checks": {"gate": {"kind": "test", "command": "exit 1"}}}`)
	engine, err := lcx.Open(lcx.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()

	status := func() lcx.CheckApprovalState {
		t.Helper()
		var out bytes.Buffer
		if err := runChecksCommand(ctx, engine, &out, []string{"status"}); err != nil {
			t.Fatal(err)
		}
		var st lcx.CheckApprovalState
		if err := json.Unmarshal(out.Bytes(), &st); err != nil {
			t.Fatalf("status output %s: %v", out.Bytes(), err)
		}
		return st
	}
	printed := status()
	engineState, err := engine.CheckApprovalState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if printed.SetDigest != engineState.SetDigest || printed.Approved || len(printed.Checks) != 1 || printed.Checks[0].Change != lcx.CheckAdded {
		t.Fatalf("status %+v, engine %+v", printed, engineState)
	}

	for _, bad := range [][]string{{"approve"}, {"approve", printed.SetDigest, "--actor"}, {"approve", printed.SetDigest, "--approved-by", "agent"}, {"bogus"}} {
		if err := runChecksCommand(ctx, engine, &bytes.Buffer{}, bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}

	writeConfig(t, project, `{"checks": {"gate": {"kind": "test", "command": "exit 0"}}}`)
	if err := runChecksCommand(ctx, engine, &bytes.Buffer{}, []string{"approve", printed.SetDigest}); !errors.Is(err, lcx.ErrApprovalStale) {
		t.Fatalf("approving the digest of the old definitions: %v", err)
	}
	current := status()
	var out bytes.Buffer
	if err := runChecksCommand(ctx, engine, &out, []string{"approve", current.SetDigest, "--actor", "owner", "--decision-ref", "review:1"}); err != nil {
		t.Fatal(err)
	}
	var approved lcx.CheckApprovalState
	if err := json.Unmarshal(out.Bytes(), &approved); err != nil {
		t.Fatal(err)
	}
	after, err := engine.CheckApprovalState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !approved.Approved || !after.Approved || after.Epoch != approved.Epoch || after.Previous == nil ||
		after.Previous.Actor != "owner" || after.Previous.ApprovedBy != lcx.ApprovedByUser || after.Previous.DecisionRef != "review:1" {
		t.Fatalf("approve printed %+v, engine %+v", approved, after)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
