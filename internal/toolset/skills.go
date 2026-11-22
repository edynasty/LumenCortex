package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/edynasty/LumenCortex/protocol"
)

const (
	// MaxSkillCatalogPage bounds the number of skill entries returned in a single ListSkills page.
	MaxSkillCatalogPage = 50
	// MaxSkillCatalogResponseBytes bounds the marshaled JSON byte length of SkillCatalogPage (16 KiB).
	MaxSkillCatalogResponseBytes = 16 << 10
)

// SkillCatalogEntry is one skill summary returned by read_skill in catalog mode.
type SkillCatalogEntry struct {
	ID                   string `json:"id"`
	Name                 string `json:"name,omitempty"`
	Scope                string `json:"scope"`
	Description          string `json:"description"`
	DescriptionTruncated bool   `json:"descriptionTruncated,omitempty"`
}

// SkillCatalogPage is one bounded page of the run's immutable eligible skill snapshot.
type SkillCatalogPage struct {
	TotalEligible int                 `json:"totalEligible"`
	Matched       int                 `json:"matched"`
	Offset        int                 `json:"offset"`
	NextOffset    int                 `json:"nextOffset,omitempty"`
	HasMore       bool                `json:"hasMore"`
	Skills        []SkillCatalogEntry `json:"skills"`
}

// SkillReader loads the skills a run's catalog offers. The runtime builds it from the skills the
// run's system prompt snapshot captured, so read_skill reads only eligible skills and nothing a
// disabled, withheld or shadowed skill provides.
type SkillReader interface {
	ReadSkill(ctx context.Context, id, path string) (any, error)
}

// SkillCataloger extends SkillReader with paginated search over the run's eligible skill snapshot.
type SkillCataloger interface {
	SkillReader
	ListSkills(ctx context.Context, query string, offset int) (SkillCatalogPage, error)
}

func (s *Set) registerSkillTools() {
	if s.skills == nil {
		return
	}
	if _, ok := s.skills.(SkillCataloger); ok {
		s.add(protocol.ToolSpec{
			Name: "read_skill",
			Description: "Load or browse skills from the run's Skills snapshot: pass id (and optional relative path) to read a skill's SKILL.md or auxiliary file/directory; " +
				"or omit id and pass optional query and/or 0-based offset to search or page through all eligible skills. " +
				"Call it before acting on a task a skill's description matches.",
			Permission: "read",
			Parameters: obj(map[string]any{
				"id":     map[string]any{"type": "string", "description": "skill id to load; omit when browsing or searching the catalog"},
				"path":   map[string]any{"type": "string", "description": "optional file or directory inside the skill (only valid with id), e.g. references/api.md"},
				"query":  map[string]any{"type": "string", "description": "optional case-insensitive filter over skill id, name and description (catalog mode only; omit id)"},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "0-based pagination offset for catalog mode (omit id)"},
			}),
		}, s.readSkill)
		return
	}
	s.add(protocol.ToolSpec{
		Name: "read_skill",
		Description: "Load a skill from the Skills catalog in your instructions: with only an id it returns the skill's instructions (SKILL.md); " +
			"with a path relative to the skill directory it returns that file, or the entries of that directory. " +
			"Call it before acting on a task a skill's description matches.",
		Permission: "read",
		Parameters: obj(map[string]any{
			"id":   map[string]any{"type": "string", "description": "the skill id from the catalog"},
			"path": map[string]any{"type": "string", "description": "optional file or directory inside the skill, e.g. references/api.md"},
		}, "id"),
	}, s.readSkill)
}

func (s *Set) readSkill(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	rawID, hasID := args["id"]
	rawPath, hasPath := args["path"]
	rawQuery, hasQuery := args["query"]
	rawOffset, hasOffset := args["offset"]

	if hasID && rawID != nil {
		id, ok := rawID.(string)
		if !ok {
			return protocol.ToolResult{}, errors.New("read_skill id must be a string")
		}
		if strings.TrimSpace(id) == "" {
			return protocol.ToolResult{}, errors.New("read_skill id must not be empty; omit id to list skills")
		}
		if (hasQuery && rawQuery != nil) || (hasOffset && rawOffset != nil) {
			return protocol.ToolResult{}, errors.New("read_skill cannot combine id with query or offset")
		}
		var path string
		if hasPath && rawPath != nil {
			p, ok := rawPath.(string)
			if !ok {
				return protocol.ToolResult{}, errors.New("read_skill path must be a string")
			}
			path = p
		}
		result, err := s.skills.ReadSkill(ctx, id, path)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		return jsonResult(result)
	}

	cataloger, ok := s.skills.(SkillCataloger)
	if !ok {
		return protocol.ToolResult{}, errors.New("read_skill requires id")
	}
	if hasPath && rawPath != nil {
		return protocol.ToolResult{}, errors.New("read_skill path requires id")
	}
	var query string
	if hasQuery && rawQuery != nil {
		q, ok := rawQuery.(string)
		if !ok {
			return protocol.ToolResult{}, errors.New("read_skill query must be a string")
		}
		query = q
	}
	offset := 0
	if hasOffset && rawOffset != nil {
		parsed, err := parseNonNegativeIntArg(rawOffset)
		if err != nil {
			return protocol.ToolResult{}, fmt.Errorf("read_skill offset: %w", err)
		}
		offset = parsed
	}
	page, err := cataloger.ListSkills(ctx, query, offset)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return jsonResult(page)
}

func parseNonNegativeIntArg(v any) (int, error) {
	switch n := v.(type) {
	case int:
		if n < 0 {
			return 0, errors.New("must be >= 0")
		}
		return n, nil
	case int64:
		if n < 0 || n > int64(math.MaxInt) {
			return 0, errors.New("must be >= 0")
		}
		return int(n), nil
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || math.Trunc(n) != n || n > float64(math.MaxInt32) {
			return 0, errors.New("must be a non-negative integer")
		}
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 || i > int64(math.MaxInt32) {
			return 0, errors.New("must be a non-negative integer")
		}
		return int(i), nil
	default:
		return 0, errors.New("must be an integer")
	}
}
