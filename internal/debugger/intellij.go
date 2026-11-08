package debugger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/mcp"
)

// IntelliJ reuses the existing MCP registry and LC control lease. Raw IDE IDs
// remain private, and a dedicated endpoint admits only one owned debug session.
func (m *Manager) ideCall(ctx context.Context, s *session, name string, args map[string]any) (map[string]any, error) {
	configured := false
	for _, cfg := range m.opts.MCP.Configs() {
		if cfg.ID == s.adapter.MCPServer {
			project, err := filepath.EvalSymlinks(cfg.Headers["IJ_MCP_SERVER_PROJECT_PATH"])
			configured = err == nil && project == s.opts.Workspace && cfg.Scope == mcp.ScopeGlobal && cfg.HostOnly && !cfg.Disabled && cfg.Session == nil
			if configured {
				args["projectPath"] = cfg.Headers["IJ_MCP_SERVER_PROJECT_PATH"]
			}
			break
		}
	}
	if !configured {
		return nil, errors.New("IntelliJ needs a host-only MCP configuration pinned to this workspace")
	}
	result, err := m.opts.MCP.CallTool(ctx, s.opts.Workspace, s.adapter.MCPServer, name, args)
	if err != nil {
		return nil, err
	}
	text := ideText(result.Content)
	if result.IsError {
		return nil, fmt.Errorf("IntelliJ %s rejected: %s", name, text)
	}
	if result.StructuredContent != nil {
		return result.StructuredContent, nil
	}
	var value map[string]any
	if json.Unmarshal([]byte(text), &value) == nil {
		return value, nil
	}
	return map[string]any{"text": text}, nil
}
func ideText(items []mcp.ToolContent) string {
	var out strings.Builder
	for _, item := range items {
		if item.Type == "text" {
			out.WriteString(item.Text)
		}
	}
	return out.String()
}
func (m *Manager) startIntelliJ(ctx context.Context, s *session) error {
	status, err := m.ideCall(ctx, s, "xdebug_get_debugger_status", map[string]any{})
	if err != nil {
		return err
	}
	if sessions, ok := status["sessions"].([]any); !ok || len(sessions) != 0 {
		return errors.New("IntelliJ endpoint already has sessions or invalid status")
	}
	rel, err := filepath.Rel(s.opts.Workspace, s.opts.Program)
	if err != nil {
		return err
	}
	points, err := m.ideCall(ctx, s, "get_run_configurations", map[string]any{"filePath": rel})
	if err != nil {
		return err
	}
	entries, _ := points["runPoints"].([]any)
	if len(entries) == 0 {
		return errors.New("IntelliJ found no runnable entry point")
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		return errors.New("invalid IntelliJ entry point")
	}
	s.ideEntry = int(ideNumber(entry["line"]))
	if s.ideEntry < 1 {
		return errors.New("invalid IntelliJ entry line")
	}
	s.breakpoints = make(map[string][]Breakpoint)
	s.ideBreakpoints = make(map[string][]string)
	s.ideThreads = make(map[int]string)
	m.mu.Lock()
	s.snapshot.State = "configuring"
	m.mu.Unlock()
	return nil
}
func ideNumber(v any) float64 { n, _ := v.(float64); return n }
func (m *Manager) ideState(s *session, result map[string]any) {
	state, _ := result["status"].(string)
	if state == "" {
		state, _ = result["state"].(string)
	}
	switch state {
	case "paused":
		state = "stopped"
	case "stopped":
		state = "terminated"
	case "running":
	default:
		return
	}
	m.mu.Lock()
	s.snapshot.State = state
	if state == "stopped" {
		s.snapshot.ThreadID = 1
	}
	m.mu.Unlock()
	m.emit(s, "debug."+state, map[string]any{"status": state})
}
func (m *Manager) requestIntelliJ(ctx context.Context, s *session, command string, input any) (json.RawMessage, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var args map[string]any
	if err = json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	bound := map[string]any{"sessionId": s.ideSession}
	marshal := func(v any) (json.RawMessage, error) { return json.Marshal(v) }
	switch command {
	case "configurationDone":
		if s.ideSession != "" {
			return nil, ErrState
		}
		rel, _ := filepath.Rel(s.opts.Workspace, s.opts.Program)
		result, err := m.ideCall(ctx, s, "xdebug_start_debugger_session", map[string]any{"filePath": rel, "line": s.ideEntry, "timeout": 10, "graceWaitMs": 200})
		if err != nil {
			recovery, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			status, statusErr := m.ideCall(recovery, s, "xdebug_get_debugger_status", map[string]any{})
			cancel()
			items, valid := status["sessions"].([]any)
			if statusErr == nil && valid && len(items) == 1 {
				entry, _ := items[0].(map[string]any)
				s.ideSession, _ = entry["id"].(string)
			} else if statusErr != nil || !valid || len(items) > 1 {
				s.ideUncertain = true
			}
			s.cancel()
			return nil, err
		}
		s.ideSession, _ = result["sessionId"].(string)
		if s.ideSession == "" {
			return nil, errors.New("IntelliJ start returned no session identity")
		}
		m.ideState(s, result)
		return marshal(result)
	case "setBreakpoints":
		source, _ := args["source"].(map[string]any)
		path, _ := source["path"].(string)
		for _, id := range s.ideBreakpoints[path] {
			if _, err = m.ideCall(ctx, s, "xdebug_remove_breakpoint", map[string]any{"breakpointId": id, "owner": "agent"}); err != nil {
				return nil, err
			}
		}
		s.ideBreakpoints[path] = nil
		results := []any{}
		points, _ := args["breakpoints"].([]any)
		for _, value := range points {
			p, _ := value.(map[string]any)
			if p["hitCondition"] != "" && p["hitCondition"] != nil {
				return nil, errors.New("IntelliJ hit conditions are not supported by this toolset")
			}
			request := map[string]any{"filePath": path, "line": p["line"]}
			if condition, ok := p["condition"].(string); ok && condition != "" {
				request["condition"] = condition
			}
			result, err := m.ideCall(ctx, s, "xdebug_set_breakpoint", request)
			if err != nil {
				return nil, err
			}
			id, _ := result["breakpointId"].(string)
			if id == "" {
				return nil, errors.New("IntelliJ breakpoint returned no identity")
			}
			s.ideBreakpoints[path] = append(s.ideBreakpoints[path], id)
			results = append(results, map[string]any{"verified": true, "line": p["line"]})
		}
		return marshal(map[string]any{"breakpoints": results})
	case "continue", "pause", "next", "stepIn", "stepOut":
		if s.ideSession == "" {
			return nil, ErrState
		}
		action := map[string]string{"continue": "RESUME", "pause": "PAUSE", "next": "STEP_OVER", "stepIn": "STEP_INTO", "stepOut": "STEP_OUT"}[command]
		bound["action"] = action
		bound["timeout"] = 5
		result, err := m.ideCall(ctx, s, "xdebug_control_session", bound)
		if err != nil {
			return nil, err
		}
		m.ideState(s, result)
		return marshal(result)
	case "threads":
		result, err := m.ideCall(ctx, s, "xdebug_get_threads", bound)
		if err != nil {
			return nil, err
		}
		items, _ := result["threads"].([]any)
		threads := []any{}
		clear(s.ideThreads)
		for i, item := range items {
			if i >= 100 {
				break
			}
			entry, _ := item.(map[string]any)
			id, _ := entry["id"].(string)
			if id == "" {
				id, _ = entry["threadId"].(string)
			}
			s.ideThreads[i+1] = id
			threads = append(threads, map[string]any{"id": i + 1, "name": fmt.Sprint(entry["name"])})
		}
		return marshal(map[string]any{"threads": threads})
	case "stackTrace":
		if id := s.ideThreads[int(ideNumber(args["threadId"]))]; id != "" {
			bound["threadId"] = id
		}
		bound["limit"] = args["levels"]
		bound["offset"] = args["startFrame"]
		result, err := m.ideCall(ctx, s, "xdebug_get_stack", bound)
		if err != nil {
			return nil, err
		}
		items, _ := result["frames"].([]any)
		frames := []any{}
		for _, item := range items {
			entry, _ := item.(map[string]any)
			path, _ := entry["file"].(string)
			if u, err := url.Parse(path); err == nil && u.Scheme == "file" {
				path = u.Path
			}
			frames = append(frames, map[string]any{"id": entry["index"], "name": entry["presentation"], "line": entry["line"], "source": map[string]any{"path": path}})
		}
		return marshal(map[string]any{"stackFrames": frames})
	case "scopes":
		return marshal(map[string]any{"scopes": []any{map[string]any{"name": "Locals", "variablesReference": int(ideNumber(args["frameId"])) + 1}}})
	case "variables":
		bound["frameIndex"] = int(ideNumber(args["variablesReference"])) - 1
		bound["depth"] = 0
		result, err := m.ideCall(ctx, s, "xdebug_get_frame_values", bound)
		if err != nil {
			return nil, err
		}
		text, _ := result["text"].(string)
		variables := []any{}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimLeft(line, " │├└─\t")
			name, value, ok := strings.Cut(line, " = ")
			if !ok {
				continue
			}
			variables = append(variables, map[string]any{"name": name, "value": value, "variablesReference": 0})
		}
		return marshal(map[string]any{"variables": variables})
	case "evaluate":
		bound["frameIndex"] = args["frameId"]
		bound["expression"] = args["expression"]
		bound["depth"] = 0
		result, err := m.ideCall(ctx, s, "xdebug_evaluate_expression", bound)
		if err != nil {
			return nil, err
		}
		return marshal(map[string]any{"result": result["text"], "variablesReference": 0})
	default:
		return nil, fmt.Errorf("unsupported IntelliJ operation %s", strconv.Quote(command))
	}
}
func (m *Manager) closeIntelliJ(s *session) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errs []error
	if s.ideUncertain {
		errs = append(errs, errors.New("IntelliJ launch outcome unknown; endpoint requires host cleanup"))
	}
	if s.ideSession != "" {
		_, err := m.ideCall(ctx, s, "xdebug_control_session", map[string]any{"sessionId": s.ideSession, "action": "STOP", "timeout": 5})
		errs = append(errs, err)
	}
	for _, ids := range s.ideBreakpoints {
		for _, id := range ids {
			_, err := m.ideCall(ctx, s, "xdebug_remove_breakpoint", map[string]any{"breakpointId": id, "owner": "agent"})
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) watchIntelliJ(s *session) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.life.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
		}
		select {
		case s.op <- struct{}{}:
		case <-s.life.Done():
			return
		case <-s.done:
			return
		}
		if s.ideSession == "" {
			<-s.op
			continue
		}
		ctx, cancel := context.WithTimeout(s.life, 5*time.Second)
		status, err := m.ideCall(ctx, s, "xdebug_get_debugger_status", map[string]any{})
		cancel()
		terminal := false
		if err == nil {
			items, valid := status["sessions"].([]any)
			if !valid {
				err = errors.New("invalid IntelliJ session status")
			} else {
				found := false
				for _, item := range items {
					entry, ok := item.(map[string]any)
					if !ok || entry["id"] != s.ideSession {
						continue
					}
					found = true
					state, _ := entry["state"].(string)
					if state == "paused" {
						state = "stopped"
					}
					m.mu.Lock()
					previous := s.snapshot.State
					m.mu.Unlock()
					if state != previous {
						m.ideState(s, entry)
					}
				}
				if !found {
					if len(items) != 0 {
						s.ideUncertain = true
					}
					s.ideSession = ""
					terminal = true
				}
			}
		}
		<-s.op
		if terminal || err != nil {
			m.stopSession(s)
			return
		}
	}
}
