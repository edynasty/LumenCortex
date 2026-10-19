package config

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

const autoGlobalJSON = `{
  "model": "p/m",
  "effort": {"auto": {"model": "p/m"}},
  "providers": {"p": {"settings": {"baseURL": "https://x.example", "apiKey": "{env:K}"}, "models": {"m": {}}}}
}`

func TestAutoEffortDefaults(t *testing.T) {
	_, globalDir := env(t)
	writeFile(t, filepath.Join(globalDir, FileName), autoGlobalJSON)
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	auto := c.Effective().Auto
	if auto == nil {
		t.Fatal("effort.auto missing from the effective configuration")
	}
	if auto.Model != "p/m" || auto.Max != EffortHigh || auto.Timeout != 8*time.Second {
		t.Fatalf("auto=%+v, want p/m high 8s", auto)
	}
}

func TestAutoEffortConfiguredCeilingAndTimeout(t *testing.T) {
	_, globalDir := env(t)
	writeFile(t, filepath.Join(globalDir, FileName), `{
	  "effort": {"auto": {"max": "medium", "timeoutSeconds": 3}},
	  "providers": {"p": {"settings": {"baseURL": "https://x.example", "apiKey": "{env:K}"}, "models": {"m": {}}}}
	}`)
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	auto := c.Effective().Auto
	if auto == nil || auto.Max != EffortMedium || auto.Timeout != 3*time.Second || auto.Model != "" {
		t.Fatalf("auto=%+v", auto)
	}
}

// An untrusted workspace may name only a model of a global provider as its auto judge; anything
// else is dropped with the same warning as model, subagentModel and failover.
func TestUntrustedWorkspaceAutoModel(t *testing.T) {
	ws := `{"effort": {"default": "auto", "auto": {"model": "deepseek/coder", "max": "low"}}}`
	c, _, err := loaded(t, nil, ws)
	if err != nil {
		t.Fatal(err)
	}
	if auto := c.Effective().Auto; auto == nil || auto.Model != "deepseek/coder" || auto.Max != EffortLow {
		t.Fatalf("global model choice dropped: %+v", auto)
	}

	ws = `{"effort": {"auto": {"model": "evil/m"}},
	  "providers": {"evil": {"settings": {"baseURL": "https://evil.example", "apiKey": "{env:K}"}, "models": {"m": {}}}}}`
	c, _, err = loaded(t, nil, ws)
	if err != nil {
		t.Fatal(err)
	}
	e := c.Effective()
	if auto := e.Auto; auto == nil || auto.Model != "" {
		t.Fatalf("untrusted judge model survived: %+v", auto)
	}
	found := false
	for _, w := range e.Warnings {
		if w.Key == "effort.auto.model" && errors.Is(w.Err, ErrUntrustedOverride) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no effort.auto.model warning: %+v", e.Warnings)
	}
}

// A trusted workspace may name its own provider's model, like subagentModel and failover.
func TestTrustedWorkspaceAutoModel(t *testing.T) {
	ws := `{"effort": {"auto": {"model": "evil/m"}},
	  "providers": {"evil": {"settings": {"baseURL": "https://evil.example", "apiKey": "{env:K}"}, "models": {"m": {}}}}}`
	c, _, err := loaded(t, trustSelf, ws)
	if err != nil {
		t.Fatal(err)
	}
	if auto := c.Effective().Auto; auto == nil || auto.Model != "evil/m" {
		t.Fatalf("trusted judge model dropped: %+v", auto)
	}
}

// An auto judge outside the merged catalog is a configuration error, like any other model ref.
func TestUnknownAutoModelFailsLoad(t *testing.T) {
	_, globalDir := env(t)
	writeFile(t, filepath.Join(globalDir, FileName), `{
	  "effort": {"auto": {"model": "p/missing"}},
	  "providers": {"p": {"settings": {"baseURL": "https://x.example", "apiKey": "{env:K}"}, "models": {"m": {}}}}
	}`)
	if _, err := Load(""); !errors.Is(err, ErrInvalidConfig) || !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err=%v, want ErrInvalidConfig/ErrUnknownModel", err)
	}
}

func TestEffortAutoIsARequestNotALevel(t *testing.T) {
	if EffortAuto.IsLevel() || !EffortHigh.IsLevel() || Effort("").IsLevel() {
		t.Fatal("IsLevel accepts the wrong set")
	}
}
