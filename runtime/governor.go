package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/edynasty/LumenCortex/internal/cognition"
	_ "github.com/mattn/go-sqlite3"
)

type GovernorPlanResult struct {
	Analysis cognition.GovernorAnalysis `json:"analysis"`
	Plan cognition.GovernorPlan `json:"plan"`
	Validation cognition.GovernorValidation `json:"validation"`
	Curator bool `json:"curator"`
	CuratorModel string `json:"curatorModel,omitempty"`
}

func (e *Engine) CognitiveGraphSnapshot(ctx context.Context) (cognition.GraphState, map[string]cognition.StorageTelemetry, error) {
	if e == nil || e.store == nil {
		return cognition.GraphState{}, nil, errors.New("runtime store is not initialized")
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000&_foreign_keys=on", filepath.ToSlash(e.store.Path()))
	db, err := sql.Open("sqlite3", dsn)
	if err != nil { return cognition.GraphState{}, nil, err }
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil { return cognition.GraphState{}, nil, err }
	defer tx.Rollback()
	if !sqliteTableExists(ctx, tx, "graph_nodes") || !sqliteTableExists(ctx, tx, "graph_edges") {
		return cognition.GraphState{}, nil, errors.New("cognitive graph tables are not initialized in shared SQLite store")
	}

	graph := cognition.GraphState{
		Version: 1,
		Nodes: map[string]cognition.GraphNode{},
		Edges: map[string]cognition.GraphEdge{},
		Metadata: map[string]any{},
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, json FROM graph_nodes ORDER BY id")
	if err != nil { return cognition.GraphState{}, nil, err }
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil { rows.Close(); return cognition.GraphState{}, nil, err }
		var node cognition.GraphNode
		if err := json.Unmarshal([]byte(raw), &node); err != nil { rows.Close(); return cognition.GraphState{}, nil, fmt.Errorf("decode graph node %s: %w", id, err) }
		if node.ID == "" { node.ID = id }
		graph.Nodes[id] = node
	}
	if err := rows.Close(); err != nil { return cognition.GraphState{}, nil, err }

	edgeRows, err := tx.QueryContext(ctx, "SELECT id, json FROM graph_edges ORDER BY id")
	if err != nil { return cognition.GraphState{}, nil, err }
	for edgeRows.Next() {
		var id, raw string
		if err := edgeRows.Scan(&id, &raw); err != nil { edgeRows.Close(); return cognition.GraphState{}, nil, err }
		var edge cognition.GraphEdge
		if err := json.Unmarshal([]byte(raw), &edge); err != nil { edgeRows.Close(); return cognition.GraphState{}, nil, fmt.Errorf("decode graph edge %s: %w", id, err) }
		if edge.ID == "" { edge.ID = id }
		graph.Edges[id] = edge
	}
	if err := edgeRows.Close(); err != nil { return cognition.GraphState{}, nil, err }

	var graphVersion string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key = ?", "graph_version").Scan(&graphVersion); err == nil {
		var version int
		if _, scanErr := fmt.Sscanf(graphVersion, "%d", &version); scanErr == nil && version > 0 { graph.Version = version }
	} else if !errors.Is(err, sql.ErrNoRows) {
		return cognition.GraphState{}, nil, err
	}
	var graphMetadata string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key = ?", "graph_metadata").Scan(&graphMetadata); err == nil {
		if graphMetadata != "" {
			if err := json.Unmarshal([]byte(graphMetadata), &graph.Metadata); err != nil { return cognition.GraphState{}, nil, fmt.Errorf("decode graph metadata: %w", err) }
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return cognition.GraphState{}, nil, err
	}

	storage := map[string]cognition.StorageTelemetry{}
	if sqliteTableExists(ctx, tx, "graph_node_storage") {
		storageRows, err := tx.QueryContext(ctx, "SELECT node_id, tier, last_access_at, access_count FROM graph_node_storage ORDER BY node_id")
		if err != nil { return cognition.GraphState{}, nil, err }
		for storageRows.Next() {
			var id, tier string
			var lastAccess sql.NullString
			var accessCount int
			if err := storageRows.Scan(&id, &tier, &lastAccess, &accessCount); err != nil { storageRows.Close(); return cognition.GraphState{}, nil, err }
			item := cognition.StorageTelemetry{Tier: tier, AccessCount: accessCount}
			if lastAccess.Valid && lastAccess.String != "" {
				if parsed, parseErr := time.Parse(time.RFC3339Nano, lastAccess.String); parseErr == nil { item.LastAccessAt = parsed }
			}
			storage[id] = item
		}
		if err := storageRows.Close(); err != nil { return cognition.GraphState{}, nil, err }
	}
	if err := tx.Commit(); err != nil { return cognition.GraphState{}, nil, err }
	return graph, storage, nil
}

func (e *Engine) GovernorAnalyze(ctx context.Context, options cognition.GovernorAnalyzeOptions) (cognition.GovernorAnalysis, error) {
	graph, storage, err := e.CognitiveGraphSnapshot(ctx)
	if err != nil { return cognition.GovernorAnalysis{}, err }
	if options.StorageByNode == nil { options.StorageByNode = storage }
	return (cognition.GovernorAnalyzer{}).Analyze(graph, options), nil
}

func (e *Engine) GovernorPlan(ctx context.Context, curator *cognition.LLMGovernorCurator, options cognition.GovernorAnalyzeOptions) (GovernorPlanResult, error) {
	graph, storage, err := e.CognitiveGraphSnapshot(ctx)
	if err != nil { return GovernorPlanResult{}, err }
	if options.StorageByNode == nil { options.StorageByNode = storage }
	analysis := (cognition.GovernorAnalyzer{}).Analyze(graph, options)
	plan := cognition.DeterministicGovernorPlan(analysis)
	usedCurator := false
	curatorModel := ""
	if curator != nil {
		plan, err = curator.Propose(ctx, graph, analysis)
		if err != nil { return GovernorPlanResult{}, err }
		usedCurator = true
		if curator.Provider != nil { curatorModel = curator.Provider.Model() }
	}
	validation := cognition.ValidateGovernorPlan(plan, graph)
	return GovernorPlanResult{Analysis: analysis, Plan: validation.Normalized, Validation: validation, Curator: usedCurator, CuratorModel: curatorModel}, nil
}

func (e *Engine) GovernorValidate(ctx context.Context, plan cognition.GovernorPlan) (cognition.GovernorValidation, error) {
	graph, _, err := e.CognitiveGraphSnapshot(ctx)
	if err != nil { return cognition.GovernorValidation{}, err }
	return cognition.ValidateGovernorPlan(plan, graph), nil
}

func sqliteTableExists(ctx context.Context, tx *sql.Tx, name string) bool {
	var value string
	err := tx.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type = ? AND name = ?", "table", name).Scan(&value)
	return err == nil && value == name
}
