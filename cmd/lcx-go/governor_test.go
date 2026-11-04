package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// `lcx-go governor plan --curator` fails closed (ADR-0018, plan T1-S3): the curator is a model
// request with no request envelope, so the command refuses before any request leaves the process.
func TestGovernorCuratorFailsClosedWithoutAModelCall(t *testing.T) {
	srv := newFakeProvider(t)
	globalFile, ws := cliEnv(t)
	writeConfig(t, globalFile, globalFor(srv.URL, ""))
	t.Setenv("LOCAL_TEST_KEY", "curator-key")

	cmd := exec.Command(os.Args[0], "governor", "plan", "--curator")
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "LCX_TEST_RUN_MAIN=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		t.Fatalf("governor plan --curator succeeded: stdout=%s stderr=%s", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "curator is disabled") {
		t.Fatalf("stderr=%s, want the fail-closed reason", errb.String())
	}
	if requests := srv.requests(); len(requests) != 0 {
		t.Fatalf("a refused curator made %d model requests: %+v", len(requests), requests)
	}
}
