package toolset

import (
	"context"
	"fmt"
	"strings"

	"github.com/edynasty/LumenCortex/protocol"
)

// Wakeups lets a run ask the runtime to start it again later (ADR-0019 §4): a wake-up is a one-shot
// agent schedule on the calling session, with the calling run's policy as its ceiling. The runtime
// binds it per run; nil offers neither tool.
type WakeupController interface {
	// ScheduleWakeup creates a one-shot agent task on the calling session and returns its id and the
	// time it fires (UTC).
	ScheduleWakeup(ctx context.Context, delaySec int, message string) (Wakeup, error)
	// CancelWakeup removes a pending wake-up of the calling session.
	CancelWakeup(ctx context.Context, id string) error
}

// Wakeup is a scheduled wake-up as the tools report it.
type Wakeup struct {
	ID     string
	FireAt string // RFC 3339, UTC
}

// Bounds of a wake-up, from ADR-0019 §4.
const (
	MinWakeupDelaySec = 1
	MaxWakeupDelaySec = 86400
	MaxWakeupMessage  = 2000
)

func (s *Set) registerWakeupTools() {
	if s.wakeups == nil {
		return
	}
	s.add(protocol.ToolSpec{
		Name: "wakeup_schedule",
		Description: "Schedule a one-shot wake-up of this session: after delay_sec (1..86400) the runtime " +
			"starts a new run here with your message as its instruction, so work can resume later " +
			"(a build finishing, a review window, a retry after a cool-down). At most 3 wake-ups may be " +
			"pending per session. The wake-up runs with this run's policy, never a wider one. " +
			"Returns its id and the time it fires.",
		// A wake-up schedules future work, so it is a write even though it touches no file.
		Permission: "write",
		Parameters: obj(map[string]any{
			"delay_sec": map[string]any{
				"type": "integer", "minimum": MinWakeupDelaySec, "maximum": MaxWakeupDelaySec,
				"description": "seconds from now until the wake-up fires",
			},
			"message": map[string]any{
				"type": "string", "minLength": 1, "maxLength": MaxWakeupMessage,
				"description": "what the resumed run should do, in your own words",
			},
		}, "delay_sec", "message"),
	}, s.scheduleWakeup)
	s.add(protocol.ToolSpec{
		Name: "wakeup_cancel",
		Description: "Cancel a pending wake-up of this session by its id (from wakeup_schedule). " +
			"A wake-up of another session is not reachable.",
		Permission: "write",
		Parameters: obj(map[string]any{
			"wakeup_id": map[string]any{"type": "string", "description": "the id wakeup_schedule returned"},
		}, "wakeup_id"),
	}, s.cancelWakeup)
}

func (s *Set) scheduleWakeup(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	delay, ok := args["delay_sec"].(float64)
	if !ok || delay != float64(int(delay)) || int(delay) < MinWakeupDelaySec || int(delay) > MaxWakeupDelaySec {
		return protocol.ToolResult{}, fmt.Errorf("delay_sec must be a whole number of seconds between %d and %d", MinWakeupDelaySec, MaxWakeupDelaySec)
	}
	message := strings.TrimSpace(stringArg(args, "message"))
	if message == "" || len(message) > MaxWakeupMessage {
		return protocol.ToolResult{}, fmt.Errorf("message must be 1-%d bytes", MaxWakeupMessage)
	}
	wakeup, err := s.wakeups.ScheduleWakeup(ctx, int(delay), message)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return jsonResult(map[string]any{"wakeupId": wakeup.ID, "fireAt": wakeup.FireAt})
}

func (s *Set) cancelWakeup(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	id := strings.TrimSpace(stringArg(args, "wakeup_id"))
	if id == "" {
		return protocol.ToolResult{}, fmt.Errorf("wakeup_id is required")
	}
	if err := s.wakeups.CancelWakeup(ctx, id); err != nil {
		return protocol.ToolResult{}, err
	}
	return jsonResult(map[string]any{"cancelled": id})
}
