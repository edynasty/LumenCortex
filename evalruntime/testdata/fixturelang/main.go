// Command fixturelang is a minimal third-party execution runtime used to prove
// that a new language plugs into LumenCortex through the public Eval SPI without
// touching the agent loop.
//
// It speaks the same JSON-RPC 2.0 control protocol as the built-in workers over
// fd 3 (host -> worker) and fd 4 (worker -> host), one JSON object per line. The
// language is a toy arithmetic shell with persistent variables:
//
//	let x = 1 + 2 * 3
//	print x
//	x * 10              (bare expression is displayed)
//	sleep 250           (milliseconds)
//	spawn sleep 300     (starts a child in the worker's process group)
//	call read_file {"path": "notes.txt"}
//	artifact notes.txt <text>
//	env NAME            (prints an environment variable or "<unset>")
//
// It declares interrupt unsupported: a cell runs to completion, and the host must
// terminate the process group to stop it. That is a truthful capability claim, not
// a hidden limitation.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	inFD  = 3
	outFD = 4
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type worker struct {
	in          *bufio.Reader
	out         *os.File
	vars        map[string]float64
	generation  uint64
	workspace   string
	artifactDir string
	tools       []toolSpec
	callSeq     int
	started     time.Time
	// initDelay makes startup slow on purpose, so a host test can prove that one
	// kernel's startup never serializes an unrelated Create.
	initDelay time.Duration
}

type toolSpec struct {
	Name string `json:"name"`
}

func main() {
	initDelay := time.Duration(0)
	for _, arg := range os.Args[1:] {
		if value, ok := strings.CutPrefix(arg, "--init-delay="); ok {
			if parsed, err := time.ParseDuration(value); err == nil {
				initDelay = parsed
			}
		}
	}
	w := &worker{
		in:         bufio.NewReaderSize(os.NewFile(inFD, "control-in"), 64<<10),
		out:        os.NewFile(outFD, "control-out"),
		vars:       map[string]float64{},
		generation: 1,
		initDelay:  initDelay,
	}
	if err := w.loop(); err != nil {
		fmt.Fprintln(os.Stderr, "fixturelang:", err)
		os.Exit(2)
	}
}

func (w *worker) loop() error {
	for {
		line, err := w.in.ReadBytes('\n')
		if err != nil {
			return nil
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var msg request
		if err := json.Unmarshal(line, &msg); err != nil {
			return fmt.Errorf("invalid frame: %w", err)
		}
		if msg.Method == "" {
			continue
		}
		if len(msg.ID) == 0 {
			continue
		}
		w.serve(msg)
	}
}

func (w *worker) send(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = w.out.Write(append(data, '\n'))
}

func (w *worker) reply(id json.RawMessage, result any, failure *rpcError) {
	msg := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id)}
	if failure != nil {
		msg["error"] = failure
	} else {
		msg["result"] = result
	}
	w.send(msg)
}

func (w *worker) serve(msg request) {
	switch msg.Method {
	case "initialize":
		var params struct {
			Workspace   string     `json:"workspace"`
			ArtifactDir string     `json:"artifactDir"`
			Tools       []toolSpec `json:"tools"`
			SPIVersion  string     `json:"spiVersion"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		if w.initDelay > 0 {
			time.Sleep(w.initDelay)
		}
		w.workspace = params.Workspace
		w.artifactDir = params.ArtifactDir
		w.tools = params.Tools
		w.reply(msg.ID, map[string]any{
			"spiVersion":     "1.0",
			"runtimeVersion": "fixture-1.0",
			"language":       "fixture",
			"pid":            os.Getpid(),
			"capabilities":   []string{"persistent_execution", "artifact", "display", "tool_callback", "subprocess"},
		}, nil)
	case "execute":
		w.reply(msg.ID, w.execute(msg.Params), nil)
	case "reset":
		w.vars = map[string]float64{}
		w.generation++
		w.reply(msg.ID, map[string]any{"generation": w.generation, "pid": os.Getpid()}, nil)
	case "health":
		w.reply(msg.ID, map[string]any{"status": "ok", "generation": w.generation, "pid": os.Getpid()}, nil)
	case "close":
		w.reply(msg.ID, map[string]any{"closed": true}, nil)
		os.Exit(0)
	default:
		w.reply(msg.ID, nil, &rpcError{Code: -32601, Message: "unknown method " + msg.Method})
	}
}

func (w *worker) execute(raw json.RawMessage) map[string]any {
	var params struct {
		Code  string     `json:"code"`
		Tools []toolSpec `json:"tools"`
	}
	_ = json.Unmarshal(raw, &params)
	// The host advertises the per-execution tool surface with each cell.
	if params.Tools != nil {
		w.tools = params.Tools
	}
	started := time.Now()
	var stdout, stderr strings.Builder
	status := "completed"
	errText := ""
	var displays []map[string]any
	var artifacts []map[string]any

	for _, line := range strings.Split(params.Code, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "let":
			name, value, err := w.assignment(line)
			if err != nil {
				status, errText = "failed", err.Error()
				break
			}
			w.vars[name] = value
		case "print":
			value, err := w.eval(strings.TrimSpace(strings.TrimPrefix(line, "print")))
			if err != nil {
				status, errText = "failed", err.Error()
				break
			}
			fmt.Fprintf(&stdout, "%s\n", formatNumber(value))
		case "sleep":
			ms, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "sleep")))
			if err != nil {
				status, errText = "failed", "sleep needs milliseconds"
				break
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
		case "spawn":
			parts := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "spawn")))
			if len(parts) == 0 {
				status, errText = "failed", "spawn needs a command"
				break
			}
			cmd := exec.Command(parts[0], parts[1:]...)
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				status, errText = "failed", err.Error()
				break
			}
			fmt.Fprintf(&stdout, "spawned %d\n", cmd.Process.Pid)
		case "call":
			content, err := w.callTool(strings.TrimSpace(strings.TrimPrefix(line, "call")))
			if err != nil {
				status, errText = "failed", err.Error()
				break
			}
			fmt.Fprintf(&stdout, "%s\n", content)
		case "artifact":
			rest := strings.TrimSpace(strings.TrimPrefix(line, "artifact"))
			parts := strings.SplitN(rest, " ", 2)
			if len(parts) != 2 {
				status, errText = "failed", "artifact needs a name and text"
				break
			}
			name := filepath.Base(parts[0])
			if w.artifactDir == "" {
				status, errText = "failed", "artifact directory is not configured"
				break
			}
			if err := os.MkdirAll(w.artifactDir, 0o700); err != nil {
				status, errText = "failed", err.Error()
				break
			}
			if err := os.WriteFile(filepath.Join(w.artifactDir, name), []byte(parts[1]), 0o600); err != nil {
				status, errText = "failed", err.Error()
				break
			}
			artifacts = append(artifacts, map[string]any{"name": name, "mime": "text/plain"})
		case "env":
			name := strings.TrimSpace(strings.TrimPrefix(line, "env"))
			if value, ok := os.LookupEnv(name); ok {
				fmt.Fprintf(&stdout, "%s\n", value)
			} else {
				fmt.Fprintln(&stdout, "<unset>")
			}
		default:
			value, err := w.eval(line)
			if err != nil {
				status, errText = "failed", err.Error()
				break
			}
			displays = append(displays, map[string]any{
				"mime": "text/plain", "encoding": "utf-8",
				"text": formatNumber(value), "size": len(formatNumber(value)),
			})
		}
		if status == "failed" {
			break
		}
	}
	return map[string]any{
		"status":      status,
		"stdout":      stdout.String(),
		"stderr":      stderr.String(),
		"error":       errText,
		"traceback":   "",
		"stateStatus": "ok",
		"exitCode":    0,
		"displays":    displays,
		"artifacts":   artifacts,
		"durationMs":  time.Since(started).Milliseconds(),
	}
}

func (w *worker) assignment(line string) (string, float64, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "let"))
	eq := strings.Index(rest, "=")
	if eq <= 0 {
		return "", 0, fmt.Errorf("let needs name = expression")
	}
	name := strings.TrimSpace(rest[:eq])
	value, err := w.eval(strings.TrimSpace(rest[eq+1:]))
	if err != nil {
		return "", 0, err
	}
	return name, value, nil
}

func (w *worker) callTool(rest string) (string, error) {
	parts := strings.SplitN(rest, " ", 2)
	name := parts[0]
	arguments := map[string]any{}
	if len(parts) == 2 {
		if err := json.Unmarshal([]byte(parts[1]), &arguments); err != nil {
			return "", fmt.Errorf("tool arguments must be a JSON object: %w", err)
		}
	}
	found := false
	for _, tool := range w.tools {
		if tool.Name == name {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("tool %q is not authorized for this execution", name)
	}
	w.callSeq++
	callID := fmt.Sprintf("f%d", w.callSeq)
	w.send(map[string]any{
		"jsonrpc": "2.0", "id": callID, "method": "tool/call",
		"params": map[string]any{"callId": callID, "name": name, "arguments": arguments},
	})
	for {
		line, err := w.in.ReadBytes('\n')
		if err != nil {
			return "", fmt.Errorf("control channel closed during tool call")
		}
		var msg request
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Method != "" || string(msg.ID) != `"`+callID+`"` {
			continue
		}
		if msg.Error != nil {
			return "", fmt.Errorf("%s", msg.Error.Message)
		}
		var result struct {
			OK      bool   `json:"ok"`
			Denied  bool   `json:"denied"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return "", err
		}
		if result.Denied {
			return "", fmt.Errorf("tool denied: %s", result.Content)
		}
		if !result.OK {
			return "", fmt.Errorf("tool failed: %s", result.Content)
		}
		return result.Content, nil
	}
}

// eval is a tiny recursive-descent evaluator: + - * /, parentheses, numbers and
// variables. Division by zero is a cell failure.
func (w *worker) eval(expr string) (float64, error) {
	tokens := tokenize(expr)
	pos := 0
	var parse func() (float64, error)
	parseExpr := func() (float64, error) {
		left, err := parse()
		if err != nil {
			return 0, err
		}
		for pos < len(tokens) && (tokens[pos] == "+" || tokens[pos] == "-") {
			op := tokens[pos]
			pos++
			right, err := parse()
			if err != nil {
				return 0, err
			}
			if op == "+" {
				left += right
			} else {
				left -= right
			}
		}
		return left, nil
	}
	parse = func() (float64, error) {
		left, err := w.atom(tokens, &pos)
		if err != nil {
			return 0, err
		}
		for pos < len(tokens) && (tokens[pos] == "*" || tokens[pos] == "/") {
			op := tokens[pos]
			pos++
			right, err := w.atom(tokens, &pos)
			if err != nil {
				return 0, err
			}
			if op == "*" {
				left *= right
			} else {
				if right == 0 {
					return 0, fmt.Errorf("division by zero")
				}
				left /= right
			}
		}
		return left, nil
	}
	value, err := parseExpr()
	if err != nil {
		return 0, err
	}
	if pos != len(tokens) {
		return 0, fmt.Errorf("unexpected token %q", tokens[pos])
	}
	return value, nil
}

func (w *worker) atom(tokens []string, pos *int) (float64, error) {
	if *pos >= len(tokens) {
		return 0, fmt.Errorf("unexpected end of expression")
	}
	token := tokens[*pos]
	*pos++
	if token == "(" {
		value, err := w.evalTokens(tokens, pos)
		if err != nil {
			return 0, err
		}
		if *pos >= len(tokens) || tokens[*pos] != ")" {
			return 0, fmt.Errorf("missing closing parenthesis")
		}
		*pos++
		return value, nil
	}
	if token == "-" {
		value, err := w.atom(tokens, pos)
		return -value, err
	}
	if number, err := strconv.ParseFloat(token, 64); err == nil {
		return number, nil
	}
	if value, ok := w.vars[token]; ok {
		return value, nil
	}
	return 0, fmt.Errorf("undefined variable %q", token)
}

func (w *worker) evalTokens(tokens []string, pos *int) (float64, error) {
	// Reuse eval by rebuilding the sub-expression is not worth it: evaluate a
	// parenthesised group with the same precedence rules inline.
	var parse func() (float64, error)
	parse = func() (float64, error) {
		left, err := w.atom(tokens, pos)
		if err != nil {
			return 0, err
		}
		for *pos < len(tokens) && (tokens[*pos] == "*" || tokens[*pos] == "/") {
			op := tokens[*pos]
			*pos++
			right, err := w.atom(tokens, pos)
			if err != nil {
				return 0, err
			}
			if op == "*" {
				left *= right
			} else {
				if right == 0 {
					return 0, fmt.Errorf("division by zero")
				}
				left /= right
			}
		}
		for *pos < len(tokens) && (tokens[*pos] == "+" || tokens[*pos] == "-") {
			op := tokens[*pos]
			*pos++
			right, err := parse()
			if err != nil {
				return 0, err
			}
			if op == "+" {
				left += right
			} else {
				left -= right
			}
		}
		return left, nil
	}
	return parse()
}

func tokenize(expr string) []string {
	var tokens []string
	i := 0
	for i < len(expr) {
		c := expr[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(' || c == ')' || c == '+' || c == '-' || c == '*' || c == '/':
			tokens = append(tokens, string(c))
			i++
		default:
			start := i
			for i < len(expr) && !strings.ContainsRune(" \t()+-*/", rune(expr[i])) {
				i++
			}
			tokens = append(tokens, expr[start:i])
		}
	}
	return tokens
}

func formatNumber(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}
