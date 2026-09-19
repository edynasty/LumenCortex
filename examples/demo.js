import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  CognitiveRepository,
  ModelWeaveRuntime,
  ingestWorkspace
} from '../src/index.js';

const workspace = fs.mkdtempSync(path.join(os.tmpdir(), 'modelweave-demo-'));
fs.mkdirSync(path.join(workspace, 'src'));
fs.writeFileSync(path.join(workspace, 'src', 'inventory.js'), `
export function submit(stock) {
  if (stock <= 0) throw new Error('out of stock')
  return { accepted: false }
}

export function accept(stock) {
  if (stock <= 0) throw new Error('out of stock')
  return { accepted: true, stock: stock - 1 }
}
`);
fs.writeFileSync(path.join(workspace, 'src', 'application.js'), `
import { submit, accept } from './inventory.js'
export const createApplication = submit
export const acceptApplication = accept
`);

const repo = new CognitiveRepository(workspace);
repo.init();
const ingested = ingestWorkspace(repo.graph().snapshot(), workspace);
repo.writeGraph(ingested.graph);
repo.commit('ingest demo repository');

const runtime = new ModelWeaveRuntime(repo);
const context = runtime.context('where is inventory deducted when an application is accepted?', { budgetTokens: 3000 });

console.log('Workspace:', workspace);
console.log('Selected context:');
for (const node of context.selectedNodes) {
  console.log(`- ${node.kind} ${node.title} (${node.activation.toFixed(3)})`);
}
