// Package dapfake is a deterministic Debug Adapter Protocol server used only by
// LumenCortex's tests. It is not linked into the product.
//
// A test binary serves it: the test that needs an adapter points the Manager at
// its own executable with -test.run=<helper> and a configuration path after
// "--". The helper test calls ServeIfSelected, which reads the standard
// Content-Length framing on stdin/stdout and returns.
//
// The server reproduces the adapter behaviours the Manager must not trust:
// variablesReference values that silently stop resolving after a step, module
// events at launch, unverified breakpoints, abnormal exits, and a disconnect
// request that is accepted but never answered.
package dapfake

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is the adapter behaviour of one fake session.
type Config struct {
	// StopDelayMS delays the stopped event after configure/step/continue.
	StopDelayMS int `json:"stopDelayMs"`
	// CrashAfter makes the adapter exit without answering its Nth request.
	CrashAfter int `json:"crashAfter"`
	// CrashExitCode is the exit status of that crash (default 3).
	CrashExitCode int `json:"crashExitCode"`
	// IgnoreDisconnect accepts disconnect but never answers and never exits.
	IgnoreDisconnect bool `json:"ignoreDisconnect"`
	// Modules is how many module events are emitted after launch.
	Modules int `json:"modules"`
	// Exception makes every stop report reason "exception".
	Exception bool `json:"exception"`
	// Unverified reports every breakpoint verified=false.
	Unverified bool `json:"unverified"`
	// ExitOnContinue terminates the target instead of stopping again.
	ExitOnContinue bool `json:"exitOnContinue"`
	// ThrowOnResume is a target that throws whenever it resumes: configurationDone
	// and continue stop with reason "exception" while the adapter holds an
	// exception breakpoint filter, at the breakpoint otherwise. Only filters this
	// adapter received through setExceptionBreakpoints count.
	ThrowOnResume bool `json:"throwOnResume"`
	// ReuseReferences numbers references the way Delve does: every stop's
	// frames start again at 1000 and the Locals scope's variablesReference is
	// 1000 as well, so one number is both a frame id and a variables reference,
	// and an old reference resolves against the new stop.
	ReuseReferences bool `json:"reuseReferences"`
	// StaleEmptyVariables is the adapter behaviour of resolving an unknown
	// variablesReference with an empty list instead of an error.
	StaleEmptyVariables bool `json:"staleEmptyVariables"`
	// LargeValueBytes adds a Blob local of this size.
	LargeValueBytes int `json:"largeValueBytes"`
	// Locals overrides the default Locals scope.
	Locals []Local `json:"locals"`
}

// Local is one variable in the Locals scope.
type Local struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

func (c Config) locals() []Local {
	if len(c.Locals) > 0 {
		return c.Locals
	}
	out := []Local{{Name: "count", Type: "int", Value: "3"}, {Name: "multiplier", Type: "int", Value: "0"}}
	if c.LargeValueBytes > 0 {
		out = append(out, Local{Name: "blob", Type: "string", Value: strings.Repeat("x", c.LargeValueBytes)})
	}
	return out
}

// WriteConfig stores cfg below root and returns its path.
func WriteConfig(root string, cfg Config) (string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	path := root + string(os.PathSeparator) + ".dapfake.json"
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// Args is the adapter command line that runs the helper test with cfg.
func Args(configPath string) []string {
	return []string{"-test.run=^TestFakeDAPAdapterHelper$", "--", configPath}
}

// ServeIfSelected runs the fake adapter when the process was started by Args.
// It reports whether it served. The configuration path follows "--" so the
// testing package's own flag parsing never consumes it.
func ServeIfSelected() (bool, error) {
	args := ArgsAfterSeparator(os.Args)
	if len(args) == 0 {
		return false, nil
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return true, err
	}
	var cfg Config
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return true, err
	}
	return true, Serve(os.Stdin, os.Stdout, cfg)
}

// ArgsAfterSeparator returns the arguments following the first bare "--".
func ArgsAfterSeparator(argv []string) []string {
	for i, arg := range argv {
		if arg == "--" {
			return argv[i+1:]
		}
	}
	return nil
}

const maxMessage = 4 << 20

// Serve speaks DAP on in/out until the client disconnects or the configured
// crash point is reached (os.Exit then applies).
func Serve(in io.Reader, out io.Writer, cfg Config) error {
	s := &server{cfg: cfg, w: bufio.NewWriter(out), stopSeq: 0}
	r := bufio.NewReaderSize(in, 8<<10)
	for {
		msg, err := read(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if msg.Type != "request" {
			continue
		}
		s.requests++
		if cfg.CrashAfter > 0 && s.requests == cfg.CrashAfter {
			code := cfg.CrashExitCode
			if code == 0 {
				code = 3
			}
			_ = s.w.Flush()
			os.Exit(code)
		}
		if err = s.handle(msg); err != nil {
			return err
		}
	}
}

type server struct {
	cfg        Config
	w          *bufio.Writer
	mu         sync.Mutex
	seq        int64
	requests   int
	stopSeq    int
	stopped    bool
	launchPath string
	breakpoint int
	// exceptionFilters is the last setExceptionBreakpoints request's list; only
	// the request loop reads or writes it.
	exceptionFilters []string
}

func (s *server) write(m map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	m["seq"] = s.seq
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(s.w, "Content-Length: %d\r\n\r\n", len(raw)); err != nil {
		return err
	}
	if _, err = s.w.Write(raw); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *server) reply(req message, body any) error {
	m := map[string]any{"type": "response", "request_seq": req.Seq, "success": true, "command": req.Command}
	if body != nil {
		m["body"] = body
	}
	return s.write(m)
}

func (s *server) event(name string, body any) error {
	m := map[string]any{"type": "event", "event": name}
	if body != nil {
		m["body"] = body
	}
	return s.write(m)
}

// stop publishes a stopped event after the configured delay. The delay runs on
// its own goroutine so the request response follows DAP ordering, and it also
// guarantees the Manager must actually wait rather than read a stale state.
func (s *server) stop(reason string) {
	delay := time.Duration(s.cfg.StopDelayMS) * time.Millisecond
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		s.mu.Lock()
		s.stopSeq++
		seq := s.stopSeq
		s.stopped = true
		s.mu.Unlock()
		_ = s.event("stopped", map[string]any{"reason": reason, "threadId": 1, "allThreadsStopped": true, "description": stopDescription(reason), "text": "fake stop " + strconv.Itoa(seq)})
	}()
}

// resumeReason is the stop a resumed target reaches.
func (s *server) resumeReason() string {
	if s.cfg.Exception || (s.cfg.ThrowOnResume && len(s.exceptionFilters) > 0) {
		return "exception"
	}
	return "breakpoint"
}

// references returns the first frame id and the Locals and Globals references
// of stop seq.
func (s *server) references(seq int) (frame, locals, globals int) {
	if s.cfg.ReuseReferences {
		return 1000, 1000, 1001
	}
	return 1000 * seq, 5000 + seq, 6000 + seq
}

func stopDescription(reason string) string {
	if reason == "exception" {
		return "signal SIGABRT"
	}
	return "fake " + reason
}

func (s *server) handle(req message) error {
	switch req.Command {
	case "initialize":
		return s.reply(req, map[string]any{
			"supportsConfigurationDoneRequest": true,
			"supportsExceptionInfoRequest":     true,
			"exceptionBreakpointFilters": []map[string]any{
				{"filter": "all", "label": "All Exceptions", "default": false},
				{"filter": "uncaught", "label": "Uncaught Exceptions", "default": true},
			},
		})
	case "launch", "attach":
		var args struct {
			Program string `json:"program"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		s.launchPath = args.Program
		if err := s.reply(req, nil); err != nil {
			return err
		}
		if err := s.event("initialized", nil); err != nil {
			return err
		}
		for i := range s.cfg.Modules {
			if err := s.event("module", map[string]any{"reason": "new", "module": map[string]any{"id": i + 1, "name": "module" + strconv.Itoa(i), "path": "/usr/lib/module" + strconv.Itoa(i)}}); err != nil {
				return err
			}
		}
		return nil
	case "configurationDone":
		if err := s.reply(req, nil); err != nil {
			return err
		}
		s.stop(s.resumeReason())
		return nil
	case "setBreakpoints":
		var args struct {
			Breakpoints []struct {
				Line int `json:"line"`
			} `json:"breakpoints"`
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		out := make([]map[string]any, 0, len(args.Breakpoints))
		for _, bp := range args.Breakpoints {
			s.breakpoint++
			out = append(out, map[string]any{"id": s.breakpoint, "verified": !s.cfg.Unverified, "line": bp.Line, "column": 18, "source": map[string]any{"name": "fixture.c", "path": args.Source.Path}})
		}
		return s.reply(req, map[string]any{"breakpoints": out})
	case "setExceptionBreakpoints":
		var args struct {
			Filters []string `json:"filters"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		s.exceptionFilters = args.Filters
		return s.reply(req, map[string]any{"filters": args.Filters})
	case "threads":
		return s.reply(req, map[string]any{"threads": []map[string]any{{"id": 1, "name": "Thread 1"}}})
	case "stackTrace":
		s.mu.Lock()
		stopped, seq := s.stopped, s.stopSeq
		s.mu.Unlock()
		if !stopped {
			return s.reply(req, map[string]any{"stackFrames": []any{}})
		}
		base, _, _ := s.references(seq)
		line := 4 + seq - 1
		return s.reply(req, map[string]any{"stackFrames": []map[string]any{
			{"id": base, "name": "scale", "line": line, "column": 18, "source": map[string]any{"name": "fixture.c", "path": s.launchPath}},
			{"id": base + 1, "name": "main", "line": 8, "column": 15, "source": map[string]any{"name": "fixture.c", "path": s.launchPath}},
		}})
	case "scopes":
		var args struct {
			FrameID int `json:"frameId"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		s.mu.Lock()
		stopped, seq := s.stopped, s.stopSeq
		s.mu.Unlock()
		base, locals, globals := s.references(seq)
		if !stopped || args.FrameID < base || args.FrameID >= base+100 {
			return s.reply(req, map[string]any{"scopes": []any{}})
		}
		return s.reply(req, map[string]any{"scopes": []map[string]any{
			{"name": "Locals", "variablesReference": locals, "presentationHint": "locals", "expensive": false},
			{"name": "Globals", "variablesReference": globals, "presentationHint": "globals", "expensive": false},
		}})
	case "variables":
		var args struct {
			Reference int `json:"variablesReference"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		s.mu.Lock()
		seq := s.stopSeq
		s.mu.Unlock()
		_, locals, globals := s.references(seq)
		switch args.Reference {
		case locals:
			out := make([]map[string]any, 0, len(s.cfg.locals()))
			for _, v := range s.cfg.locals() {
				out = append(out, map[string]any{"name": v.Name, "value": v.Value, "type": v.Type, "variablesReference": 0})
			}
			return s.reply(req, map[string]any{"variables": out})
		case globals:
			return s.reply(req, map[string]any{"variables": []any{}})
		default:
			// An adapter resolving an unknown reference with an empty list is
			// exactly the behaviour that hid E1-D2 from the Agent.
			return s.reply(req, map[string]any{"variables": []any{}})
		}
	case "evaluate":
		var args struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		for _, v := range s.cfg.locals() {
			if v.Name == args.Expression {
				return s.reply(req, map[string]any{"result": v.Value, "type": v.Type, "variablesReference": 0})
			}
		}
		return s.reply(req, map[string]any{"result": "43", "type": "int", "variablesReference": 0})
	case "exceptionInfo":
		return s.reply(req, map[string]any{"exceptionId": "SIGABRT", "description": "aborted()", "breakMode": "always", "details": map[string]any{"message": "fake exception"}})
	case "next", "stepIn", "stepOut":
		if err := s.reply(req, nil); err != nil {
			return err
		}
		s.mu.Lock()
		s.stopped = false
		s.mu.Unlock()
		reason := "step"
		if s.cfg.Exception {
			reason = "exception"
		}
		s.stop(reason)
		return nil
	case "continue":
		if err := s.reply(req, nil); err != nil {
			return err
		}
		s.mu.Lock()
		s.stopped = false
		exit := s.cfg.ExitOnContinue
		s.mu.Unlock()
		if exit {
			go func() {
				delay := time.Duration(s.cfg.StopDelayMS) * time.Millisecond
				if delay > 0 {
					time.Sleep(delay)
				}
				_ = s.event("exited", map[string]any{"exitCode": 0})
				_ = s.event("terminated", nil)
			}()
			return nil
		}
		if err := s.event("continued", map[string]any{"threadId": 1, "allThreadsContinued": true}); err != nil {
			return err
		}
		s.stop(s.resumeReason())
		return nil
	case "pause":
		if err := s.reply(req, nil); err != nil {
			return err
		}
		s.stop("pause")
		return nil
	case "disconnect":
		if s.cfg.IgnoreDisconnect {
			// Accept nothing and never exit: the Manager must time out and kill.
			time.Sleep(time.Hour)
		}
		if err := s.reply(req, nil); err != nil {
			return err
		}
		os.Exit(0)
		return nil
	case "terminate":
		if err := s.reply(req, nil); err != nil {
			return err
		}
		os.Exit(0)
		return nil
	default:
		return s.reply(req, map[string]any{})
	}
}

type message struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Command   string          `json:"command"`
	Arguments json.RawMessage `json:"arguments"`
}

func read(r *bufio.Reader) (message, error) {
	var m message
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return m, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return m, errors.New("dapfake: malformed header")
		}
		if strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return m, err
			}
			length = n
		}
	}
	if length < 0 || length > maxMessage {
		return m, errors.New("dapfake: bad length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return m, err
	}
	return m, json.Unmarshal(body, &m)
}
