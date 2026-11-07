package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/protocol"
)

// scienceScript makes no network calls. Every dependent request is constructed
// from the previous real tool result, including kernel and artifact identities.
type scienceScript struct {
	step                     int
	pythonRuntime, jsRuntime string
	python, js               string
	fit, plot, report        string
	generation               uint64
}

func (p *scienceScript) Model() string { return "synthetic-science-script" }
func (p *scienceScript) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.step++
	previous := func(target any) error {
		id := fmt.Sprintf("science-%d", p.step-1)
		for i := len(req.Messages) - 1; i >= 0; i-- {
			m := req.Messages[i]
			if m.Role == "tool" && m.ToolCallID == id {
				return json.Unmarshal([]byte(m.Content), target)
			}
		}
		return fmt.Errorf("missing visible result %s", id)
	}
	call := func(name string, args any) (protocol.ProviderResponse, error) {
		raw, err := json.Marshal(args)
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: fmt.Sprintf("science-%d", p.step), Name: name, Arguments: raw}}}, FinishReason: "tool_calls", Usage: protocol.Usage{Requests: 1}}, err
	}
	cell := func(session, code string) (protocol.ProviderResponse, error) {
		return call("eval", map[string]any{"session_id": session, "cell_id": fmt.Sprintf("cell-%d", p.step), "code": code, "timeout_sec": 60})
	}
	transfer := func(action, session, artifact, path, target string) (protocol.ProviderResponse, error) {
		return call("eval_artifact", map[string]any{"action": action, "session_id": session, "artifact_id": artifact, "path": path, "target_session_id": target})
	}
	switch p.step {
	case 1:
		return call("eval_create", map[string]any{"runtime_id": p.pythonRuntime})
	case 2:
		var s evalapi.Session
		if err := previous(&s); err != nil {
			return protocol.ProviderResponse{}, err
		}
		p.python = s.ID
		return call("eval_create", map[string]any{"runtime_id": p.jsRuntime})
	case 3:
		var s evalapi.Session
		if err := previous(&s); err != nil {
			return protocol.ProviderResponse{}, err
		}
		p.js = s.ID
		return transfer("import", p.python, "", "spectrum.csv", "")
	case 4:
		return cell(p.python, `import csv, json, os
import numpy as np
from scipy.optimize import curve_fit
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
with open('spectrum.csv') as f:
    rows = list(csv.DictReader(f))
x = np.array([float(r['x']) for r in rows])
y = np.array([float(r['y']) for r in rows])
def peak(x, amplitude, center, sigma, background):
    return amplitude*np.exp(-0.5*((x-center)/sigma)**2)+background
parameters, covariance = curve_fit(peak, x, y, p0=[1,0,1,0])
residual = float(np.sum((peak(x,*parameters)-y)**2))
fit = dict(parameters=parameters.tolist(), residual=residual, rows=len(rows), pid=os.getpid())
print(json.dumps(fit))
lc_artifact(json.dumps(fit), 'fit.json', 'application/json')
fig, ax = plt.subplots()
ax.plot(x,y,'o'); ax.plot(x,peak(x,*parameters))
lc_artifact(fig, 'fit.png', 'image/png')`)
	case 5:
		var result evalapi.Result
		if err := previous(&result); err != nil {
			return protocol.ProviderResponse{}, err
		}
		if !result.Succeeded() {
			return protocol.ProviderResponse{}, fmt.Errorf("scientific cell failed: %s %s", result.Error, result.Stderr)
		}
		p.generation = result.Generation
		for _, a := range result.Artifacts {
			switch a.Name {
			case "fit.json":
				p.fit = a.ID
			case "fit.png":
				p.plot = a.ID
			}
		}
		if p.fit == "" || p.plot == "" {
			return protocol.ProviderResponse{}, fmt.Errorf("scientific artifacts missing")
		}
		return transfer("copy", p.python, p.fit, "fit.json", p.js)
	case 6:
		return cell(p.js, `var fs = await import('node:fs/promises'); var fit = JSON.parse(await fs.readFile('fit.json','utf8')); var area = fit.parameters[0]*Math.abs(fit.parameters[2])*Math.sqrt(2*Math.PI); lc_artifact(JSON.stringify({...fit,area}), 'report.json', 'application/json'); console.log(area);`)
	case 7:
		var result evalapi.Result
		if err := previous(&result); err != nil {
			return protocol.ProviderResponse{}, err
		}
		if !result.Succeeded() {
			return protocol.ProviderResponse{}, fmt.Errorf("JS report failed: %s", result.Error)
		}
		for _, a := range result.Artifacts {
			if a.Name == "report.json" {
				p.report = a.ID
			}
		}
		if p.report == "" {
			return protocol.ProviderResponse{}, fmt.Errorf("JS report missing")
		}
		return transfer("copy", p.js, p.report, "report.json", p.python)
	case 8:
		return cell(p.python, `report = json.load(open('report.json'))
assert report['pid'] == os.getpid()
assert report['rows'] == len(rows)
assert np.allclose(report['parameters'], parameters)
print(json.dumps(dict(pid=os.getpid(), rows=len(rows), residual=residual)))`)
	case 9:
		var result evalapi.Result
		if err := previous(&result); err != nil {
			return protocol.ProviderResponse{}, err
		}
		if !result.Succeeded() || result.Generation != p.generation {
			return protocol.ProviderResponse{}, fmt.Errorf("original Python state did not survive language round trip")
		}
		return transfer("export", p.js, p.report, "report.json", "")
	case 10:
		return transfer("export", p.python, p.plot, "fit.png", "")
	default:
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "Original CSV fitted; Python state retained through JavaScript artifact processing. Report and PNG exported for host verification."}, FinishReason: "stop", Usage: protocol.Usage{Requests: 1}}, nil
	}
}

func TestAgentScientificWorkflowPassesIndependentCheckAfterLanguageRoundTrip(t *testing.T) {
	if os.Getenv("LCX_TEST_PYTHON") == "" {
		t.Skip("set LCX_TEST_PYTHON to a Python with NumPy/SciPy/Matplotlib; dependencies are never installed by this test")
	}
	engine, owner := executionTestEngine(t)
	initGitRepo(t, engine.workspace)
	var data strings.Builder
	data.WriteString("x,y\n")
	for i := 0; i < 41; i++ {
		x := -4 + float64(i)/5
		y := 1.25*math.Exp(-0.5*math.Pow((x-0.75)/1.1, 2)) + 0.2
		fmt.Fprintf(&data, "%.17g,%.17g\n", x, y)
	}
	if err := os.WriteFile(filepath.Join(engine.workspace, "spectrum.csv"), []byte(data.String()), 0600); err != nil {
		t.Fatal(err)
	}
	// These truth values belong to the independent host fixture, never to the
	// scripted provider's fitting code or tool prompt.
	check := fmt.Sprintf("%q - <<'PY'\nimport json,math\nr=json.load(open('report.json'))\nassert len(r['parameters'])==4\nassert all(abs(a-b)<1e-5 for a,b in zip(r['parameters'],[1.25,0.75,1.1,0.2]))\nassert r['rows']==41 and r['residual']<1e-12\nassert abs(r['area']-1.25*1.1*math.sqrt(2*math.pi))<1e-5\nassert open('fit.png','rb').read(8)==bytes([137,80,78,71,13,10,26,10])\nprint('independent original-input check passed')\nPY", os.Getenv("LCX_TEST_PYTHON"))
	// The truth values are embedded in the host's command, which the definition digest covers: the
	// self-contained mode is declared explicitly.
	checks, err := json.Marshal(map[string]any{"science": map[string]any{"kind": "semantic", "command": check, "files": []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	useChecks(t, string(checks))
	approveChecks(t, engine)
	provider := &scienceScript{pythonRuntime: executionRuntimeID(t, engine, "python"), jsRuntime: executionRuntimeID(t, engine, "javascript")}
	result, err := engine.runAgentWith(context.Background(), owner, provider, AgentOptions{Policy: "full", EvalEnabled: true, MaxSteps: 15,
		Verification: &VerificationOptions{RequireSemantic: true},
		Delivery:     &DeliveryContract{Files: []DeliveryFile{{Path: "report.json", Changed: true}, {Path: "fit.png", Changed: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("workflow rejected: %#v", result)
	}
	if result.Delivery == nil || result.Delivery.Status != DeliveryVerified {
		t.Fatalf("delivery not verified: %+v", result.Delivery)
	}
	records, err := engine.CheckResults(context.Background(), owner, -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	passed := false
	for _, record := range records {
		if record.Check == "science" && record.Status == evaluator.StatusPassed {
			passed = true
		}
	}
	if !passed {
		t.Fatalf("no independent semantic pass: %#v", records)
	}
}
