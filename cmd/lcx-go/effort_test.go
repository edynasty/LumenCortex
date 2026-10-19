package main

import (
	"testing"

	"github.com/edynasty/LumenCortex/config"
)

func TestAgentOptionsEffortFromEnv(t *testing.T) {
	t.Setenv("LCX_EFFORT", "auto")
	opts, err := agentOptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Effort != config.EffortAuto {
		t.Fatalf("effort=%q, want auto", opts.Effort)
	}
	t.Setenv("LCX_EFFORT", "")
	opts, err = agentOptionsFromEnv()
	if err != nil || opts.Effort != "" {
		t.Fatalf("effort=%q err=%v, want the default", opts.Effort, err)
	}
}
