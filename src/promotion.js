import { CognitiveGraph } from './graph.js';
import { createNode } from './model.js';
import { id } from './util.js';

export function promoteNodes(graphState, nodeIds, options = {}) {
  const graph = new CognitiveGraph(graphState);
  const children = nodeIds.map((nodeId) => graph.requireNode(nodeId));
  if (!children.length) throw new Error('Promotion requires at least one child node');

  const abstraction = createNode({
    id: options.id ?? id('abs'),
    kind: 'abstraction',
    title: options.title ?? inferTitle(children),
    body: options.body ?? deterministicAbstract(children),
    tags: [...new Set([...(options.tags ?? []), 'abstraction'])],
    trustZone: options.trustZone ?? 'model_inferred',
    grade: options.grade ?? lowestGrade(children),
    childIds: children.map((n) => n.id),
    unresolved: options.unresolved ?? collectUnresolved(children),
    metadata: {
      generatedBy: options.generatedBy ?? 'promotion',
      childCount: children.length,
      ...(options.metadata ?? {})
    }
  });
  graph.addNode(abstraction);
  for (const child of children) {
    graph.addEdge({
      from: abstraction.id,
      to: child.id,
      type: 'abstracts',
      weight: 1,
      metadata: { promoted: true }
    });
  }
  return { graph: graph.snapshot(), abstraction };
}

function inferTitle(children) {
  const prefixes = children.map((n) => n.title.split(/[/:>-]/)[0].trim()).filter(Boolean);
  const shared = prefixes.find((prefix) => prefixes.filter((x) => x === prefix).length > 1);
  return shared ? `${shared} overview` : `Abstraction of ${children.length} context nodes`;
}

function deterministicAbstract(children) {
  const lines = children.map((node) => {
    const body = String(node.body ?? '').replace(/\s+/g, ' ').trim();
    const excerpt = body.length > 180 ? `${body.slice(0, 177)}...` : body;
    return `- ${node.title}${excerpt ? `: ${excerpt}` : ''}`;
  });
  return `Children:\n${lines.join('\n')}`;
}

function collectUnresolved(children) {
  return [...new Set(children.flatMap((node) => node.unresolved ?? []))];
}

function lowestGrade(children) {
  const order = ['hypothesis', 'static', 'tested', 'runtime', 'reproduced'];
  let index = order.length - 1;
  for (const node of children) index = Math.min(index, Math.max(0, order.indexOf(node.grade)));
  return order[index];
}
