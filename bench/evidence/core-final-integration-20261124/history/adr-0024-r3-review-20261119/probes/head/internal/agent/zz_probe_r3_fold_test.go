package agent

// Review probe (throwaway; /tmp only). R2-04 randomized differential check on a REAL SQLite store:
// the live incremental baseSM fold, the cold load (fold during load with load-time summaries) and the
// stateless SQLite stream (StreamRepairContextFromStore, i.e. the pre-R2-04 full replay) must return
// the identical RepairContext at every checkpoint, across many 1024-group folds, multiple gate cycles,
// Proven/Unknown/interrupted/mixed groups, steers and external user resets.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestZZR3FoldDifferential(t *testing.T) {
	for _, tc := range []struct {
		seed    int64
		groups  int
		resetAt int
	}{{1, 5200, -1}, {2, 5200, 1500}, {3, 4300, 2049}} {
		tc := tc
		t.Run(fmt.Sprintf("seed%d", tc.seed), func(t *testing.T) {
			ctx := context.Background()
			raw, err := session.Open(filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			const sid, maxCtx = "s-zz-fold", 16384
			if err := raw.Create(ctx, session.Session{ID: sid, Goal: "fold", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-fold", Input: &protocol.Message{Role: "user", Content: "start"}}); err != nil {
				t.Fatal(err)
			}
			st := realSQLiteAgentStore{s: raw}
			rLive := &run{}
			if err := rLive.load(ctx, st, sid, nil, maxCtx); err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(uint64(tc.seed), 0x5eed))
			body := strings.Repeat("z", 700)
			sha := func(c byte) string { return strings.Repeat(string(c), 64) }
			kinds := map[string]int{}
			maxLen, folds := 0, 0
			sinceReset := 0
			check := func(g int, cold bool) {
				live, err := rLive.deriveRepairContext(ctx, st, sid)
				if err != nil {
					t.Fatal(err)
				}
				stream, err := StreamRepairContextFromStore(ctx, st, sid, rLive.latestUserSeq)
				if err != nil {
					t.Fatal(err)
				}
				if live != stream {
					t.Fatalf("group %d: live baseSM fold != SQLite stream replay\nlive=%+v\nstream=%+v", g, live, stream)
				}
				if cold {
					rCold := &run{}
					if err := rCold.load(ctx, st, sid, nil, maxCtx); err != nil {
						t.Fatal(err)
					}
					coldRep, err := rCold.deriveRepairContext(ctx, st, sid)
					if err != nil {
						t.Fatal(err)
					}
					if coldRep != stream {
						t.Fatalf("group %d: cold load fold != SQLite stream replay\ncold=%+v\nstream=%+v", g, coldRep, stream)
					}
					if len(rCold.historyGroups) > MaxInMemoryHistoryGroups {
						t.Fatalf("group %d: cold historyGroups=%d", g, len(rCold.historyGroups))
					}
				}
			}
			checks, coldChecks := 0, 0
			for g := 1; g <= tc.groups; g++ {
				hadBase := rLive.hasBaseSM
				r := rng.IntN(1000)
				kind := ""
				switch {
				case g == tc.resetAt:
					kind = "user"
					u := protocol.Message{Role: "user", Content: fmt.Sprintf("external user %d", g)}
					seq, _, err := st.AppendMessage(ctx, sid, u, nil)
					if err != nil {
						t.Fatal(err)
					}
					rLive.appendRow(seq, u)
					sinceReset = 0
				case r < 12:
					kind = "steer"
					u := protocol.Message{Role: "user", Origin: protocol.OriginSteer, Content: fmt.Sprintf("steer %d", g)}
					seq, _, err := st.AppendMessage(ctx, sid, u, nil)
					if err != nil {
						t.Fatal(err)
					}
					rLive.appendRow(seq, u)
				case r < 50:
					kind = "gate"
					p := protocol.Message{Role: "assistant", Content: fmt.Sprintf("done %d", g)}
					pSeq, _, err := st.AppendMessage(ctx, sid, p, nil)
					if err != nil {
						t.Fatal(err)
					}
					rLive.appendAssistant(pSeq, p, "p/m", protocol.Echo{})
					b := protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: fmt.Sprintf("Completion refused at group %d: failing check `go test ./...`.", g)}
					bSeq, _, err := st.AppendMessage(ctx, sid, b, nil)
					if err != nil {
						t.Fatal(err)
					}
					rLive.appendRow(bSeq, b)
				case r < 80:
					kind = "text"
					a := protocol.Message{Role: "assistant", Content: fmt.Sprintf("thinking %d", g)}
					seq, _, err := st.AppendMessage(ctx, sid, a, nil)
					if err != nil {
						t.Fatal(err)
					}
					rLive.appendAssistant(seq, a, "p/m", protocol.Echo{})
				case r < 92:
					kind = "interrupted"
					id := fmt.Sprintf("it-%d", g)
					a := protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: id, Name: "bash", Arguments: json.RawMessage(`{"command":"make"}`)}}}
					aSeq, _, err := st.AppendMessage(ctx, sid, a, nil)
					if err != nil {
						t.Fatal(err)
					}
					tr := protocol.Message{Role: "tool", ToolCallID: id, Name: "bash", Origin: protocol.OriginToolInterrupted, Content: "interrupted"}
					tSeq, _, err := st.AppendMessage(ctx, sid, tr, nil)
					if err != nil {
						t.Fatal(err)
					}
					rLive.appendAssistant(aSeq, a, "p/m", protocol.Echo{})
					rLive.appendRow(tSeq, tr)
				case r < 300:
					// Admitted executions: bash (Unknown), write_file (Proven), mixed [read_file, write_file].
					sub := rng.IntN(10)
					reqID := fmt.Sprintf("req-%d", g)
					f0, l0 := int64(0), int64(0)
					if len(rLive.transcript) > 0 {
						l0 = rLive.transcript[len(rLive.transcript)-1].Seq
					}
					if _, err := st.AdmitRequest(ctx, session.RequestAdmission{RequestID: reqID, SessionID: sid, RunID: "run-fold", Purpose: session.PurposeMain,
						Step: g, FirstSeq: &f0, LastSeq: &l0, ProjectionVersion: 2, Refs: []string{"model:p/m"}}); err != nil {
						t.Fatal(err)
					}
					type tc struct{ id, name, args, pre, post string }
					var calls []tc
					switch {
					case sub < 5:
						kind = "bash_unknown"
						calls = []tc{{fmt.Sprintf("b-%d", g), "bash", `{"command":"make test"}`, "ws:sha256:" + sha('1'), "ws:sha256:" + sha('1')}}
					case sub < 8:
						kind = "write_proven"
						calls = []tc{{fmt.Sprintf("w-%d", g), "write_file", `{"path":"f.go","content":"package p"}`, "files:1:sha256:" + sha('1'), "files:1:sha256:" + sha('2') + ";cm=proven"}}
					default:
						kind = "mixed"
						calls = []tc{{fmt.Sprintf("mr-%d", g), "read_file", `{"path":"a.go"}`, "", ""},
							{fmt.Sprintf("mw-%d", g), "write_file", `{"path":"f.go","content":"package q"}`, "files:1:sha256:" + sha('2'), "files:1:sha256:" + sha('3') + ";cm=proven"}}
					}
					var tcs []protocol.ToolCall
					for _, c := range calls {
						tcs = append(tcs, protocol.ToolCall{ID: c.id, Name: c.name, Arguments: json.RawMessage(c.args)})
					}
					a := protocol.Message{Role: "assistant", ToolCalls: tcs}
					aSeq, err := st.AppendAnswered(ctx, sid, reqID, a)
					if err != nil {
						t.Fatal(err)
					}
					rLive.appendAssistant(aSeq, a, "p/m", protocol.Echo{})
					for i, c := range calls {
						tr := protocol.Message{Role: "tool", ToolCallID: c.id, Name: c.name, Content: body}
						if c.pre == "" {
							tSeq, _, err := st.AppendMessage(ctx, sid, tr, nil)
							if err != nil {
								t.Fatal(err)
							}
							rLive.appendRow(tSeq, tr)
							continue
						}
						exID := fmt.Sprintf("ex-%d-%d", g, i)
						if err := st.AdmitExecution(ctx, session.ExecutionAdmission{ExecutionID: exID, SessionID: sid, RunID: "run-fold", RequestID: reqID,
							ToolName: c.name, ArgsDigest: "sha256:" + sha('a'), PreRef: c.pre}); err != nil {
							t.Fatal(err)
						}
						tSeq, _, err := st.CommitToolResult(ctx, ToolResultCommit{SessionID: sid, RunID: "run-fold", Message: tr,
							Execution: &session.ExecutionResult{ExecutionID: exID, Outcome: session.OutcomeComplete, Coverage: session.CoverageComplete, PostRef: c.post}})
						if err != nil {
							t.Fatal(err)
						}
						rLive.appendRow(tSeq, tr)
					}
					if err := rLive.refreshGroupMutation(ctx, st, sid, aSeq); err != nil {
						t.Fatal(err)
					}
				default:
					kind = "read"
					id := fmt.Sprintf("r-%d", g)
					a := protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: id, Name: "read_file", Arguments: json.RawMessage(`{"path":"x.go"}`)}}}
					aSeq, _, err := st.AppendMessage(ctx, sid, a, nil)
					if err != nil {
						t.Fatal(err)
					}
					tr := protocol.Message{Role: "tool", ToolCallID: id, Name: "read_file", Content: body}
					tSeq, _, err := st.AppendMessage(ctx, sid, tr, nil)
					if err != nil {
						t.Fatal(err)
					}
					rLive.appendAssistant(aSeq, a, "p/m", protocol.Echo{})
					rLive.appendRow(tSeq, tr)
				}
				kinds[kind]++
				sinceReset++
				if g%64 == 0 {
					if _, err := rLive.groups(maxCtx); err != nil {
						t.Fatal(err)
					}
				}
				if len(rLive.historyGroups) > maxLen {
					maxLen = len(rLive.historyGroups)
				}
				if len(rLive.historyGroups) > MaxInMemoryHistoryGroups {
					t.Fatalf("group %d: historyGroups=%d > %d", g, len(rLive.historyGroups), MaxInMemoryHistoryGroups)
				}
				justFolded := rLive.hasBaseSM && (!hadBase || len(rLive.historyGroups) <= historyFoldBatchGroups+2)
				if justFolded {
					folds++
				}
				cold := justFolded || g%700 == 0 || g == tc.groups
				if justFolded || g%37 == 0 || g == tc.groups || kind == "gate" && rng.IntN(4) == 0 {
					check(g, cold)
					checks++
					if cold {
						coldChecks++
					}
				}
			}
			final, _ := rLive.deriveRepairContext(ctx, st, sid)
			t.Logf("seed %d: groups=%d kinds=%v maxInMemory=%d foldEvents~%d checks=%d (cold %d) final active=%v blockSeq=%d proven=%d unknown=%d refs=%d",
				tc.seed, tc.groups, kinds, maxLen, folds, checks, coldChecks, final.Active, final.BlockSeq, final.PostBlockProvenCount, final.PostBlockUnknownCount, final.CausalProgressLen)
		})
	}
}
