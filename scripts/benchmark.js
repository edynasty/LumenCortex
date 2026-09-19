import { AttentionEngine, CognitiveGraph, estimateTokens } from '../src/index.js';

const graph = new CognitiveGraph();

// Large unrelated background.
for (let i = 0; i < 1200; i += 1) {
  graph.addNode({
    id: `noise-${i}`,
    kind: 'evidence',
    title: `Unrelated module ${i}`,
    body: `This node contains implementation details for unrelated report module ${i}. `.repeat(8),
    grade: 'static',
    trustZone: 'repo_trusted'
  });
  if (i > 0) graph.addEdge({ from: `noise-${i - 1}`, to: `noise-${i}`, type: 'relates_to', weight: 0.2 });
}

// A small causally connected area relevant to the question.
const relevant = [
  ['application', 'Application acceptance flow', 'B side accepts an application'],
  ['inventory', 'Inventory acceptance', 'acceptance rechecks available inventory'],
  ['transaction', 'Inventory transaction', 'inventory acceptance runs in a transaction'],
  ['lock', 'Optimistic inventory lock', 'version field prevents concurrent stale update'],
  ['db', 'Inventory database row', 'stock and version are persisted here']
];
for (const [id, title, body] of relevant) {
  graph.addNode({ id, kind: 'evidence', title, body: body.repeat(30), grade: 'static', trustZone: 'repo_trusted' });
}
graph.addEdge({ from: 'application', to: 'inventory', type: 'calls' });
graph.addEdge({ from: 'inventory', to: 'transaction', type: 'depends_on' });
graph.addEdge({ from: 'transaction', to: 'lock', type: 'depends_on' });
graph.addEdge({ from: 'lock', to: 'db', type: 'affects' });

const state = graph.snapshot();
const fullTokens = estimateTokens(state);
const active = new AttentionEngine(state).illuminate(
  'why can concurrent application acceptance make inventory inconsistent and where is the lock?',
  { budgetTokens: 12000, maxHops: 5 }
);

const ratio = active.usedTokens / fullTokens;
console.log(JSON.stringify({
  graphNodes: Object.keys(state.nodes).length,
  graphEdges: Object.keys(state.edges).length,
  fullEstimatedTokens: fullTokens,
  activeEstimatedTokens: active.usedTokens,
  selectedNodes: active.selectedNodes.length,
  contextRatio: Number(ratio.toFixed(4)),
  reductionPercent: Number(((1 - ratio) * 100).toFixed(2)),
  selectedTitles: active.selectedNodes.map((n) => n.title)
}, null, 2));
