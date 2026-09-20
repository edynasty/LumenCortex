import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { CognitiveRepository } from '../src/repository.js';
import { AgentSessionStore } from '../src/session.js';
import { LumenCortexRuntime } from '../src/runtime.js';

function tempWorkspace(prefix='lcx-sqlite-') {
  return fs.mkdtempSync(path.join(os.tmpdir(), prefix));
}

test('repository uses a single SQLite database in WAL mode', () => {
  const root=tempWorkspace();
  const repo=new CognitiveRepository(root);
  repo.init();

  const dbFile=path.join(root,'.lumencortex','lumencortex.db');
  assert.equal(fs.existsSync(dbFile),true);
  assert.equal(fs.existsSync(path.join(root,'.lumencortex','graph.json')),false);
  assert.equal(fs.existsSync(path.join(root,'.lumencortex','search-index.json')),false);

  const db=new DatabaseSync(dbFile);
  try {
    const mode=db.prepare('PRAGMA journal_mode').get().journal_mode;
    assert.equal(String(mode).toLowerCase(),'wal');
    const tables=db.prepare(
      "SELECT name FROM sqlite_master WHERE type IN ('table','view') ORDER BY name"
    ).all().map(row=>row.name);
    for(const name of ['graph_nodes','graph_edges','cognitive_commits','cognitive_refs','sessions','session_messages','agent_steps','journal','symbols','node_fts']){
      assert.ok(tables.includes(name),`missing table ${name}`);
    }
  } finally {
    db.close();
  }
});

test('graph and cognitive git history survive repository reopen without JSON snapshots', () => {
  const root=tempWorkspace();
  let repo=new CognitiveRepository(root);
  repo.init();

  let graph=repo.graph();
  graph.addNode({id:'n1',kind:'belief',title:'Persistent belief',body:'v1'});
  repo.writeGraph(graph.snapshot());
  const first=repo.commit('add belief');

  graph=repo.graph();
  graph.updateNode('n1',{body:'v2'});
  repo.writeGraph(graph.snapshot());
  const second=repo.commit('update belief');

  repo=new CognitiveRepository(root);
  assert.equal(repo.graph().getNode('n1').body,'v2');
  assert.equal(repo.headCommitId(),second.id);
  assert.equal(repo.getCommit(second.id).snapshot.nodes.n1.body,'v2');
  assert.equal(repo.getCommit(first.id).snapshot.nodes.n1.body,'v1');
  assert.deepEqual(repo.log(2).map(x=>x.id),[second.id,first.id]);

  const db=new DatabaseSync(path.join(root,'.lumencortex','lumencortex.db'));
  try {
    const rows=db.prepare('SELECT id, diff_json FROM cognitive_commits ORDER BY created_at').all();
    assert.ok(rows.length>=3);
    assert.ok(rows.every(row=>!String(row.diff_json).includes('"snapshot"')));
  } finally {
    db.close();
  }
});

test('agent sessions persist messages and steps in normalized SQLite tables', () => {
  const root=tempWorkspace();
  const repo=new CognitiveRepository(root);
  repo.init();
  const store=new AgentSessionStore(repo.dir);
  const session=store.create({goal:'sqlite session',provider:'mock',model:'mock-model'});
  session.messages.push({role:'user',content:'hello'});
  session.messages.push({role:'assistant',content:'done'});
  session.steps.push({step:1,finishReason:'stop',toolCalls:[]});
  session.status='completed';
  session.final='done';
  session.usage={requests:1,totalTokens:12};
  store.save(session);

  const reopened=new AgentSessionStore(repo.dir).load(session.id);
  assert.equal(reopened.status,'completed');
  assert.equal(reopened.final,'done');
  assert.equal(reopened.messages.length,2);
  assert.equal(reopened.steps[0].step,1);
  assert.equal(reopened.usage.totalTokens,12);

  const listed=new AgentSessionStore(repo.dir).list(10).find(x=>x.id===session.id);
  assert.equal(listed.messageCount,2);
  assert.equal(listed.stepCount,1);
});

test('existing .lumencortex JSON repository migrates once into SQLite and is archived', () => {
  const root=tempWorkspace('lcx-json-migrate-');
  const dir=path.join(root,'.lumencortex');
  fs.mkdirSync(path.join(dir,'commits'),{recursive:true});
  fs.mkdirSync(path.join(dir,'refs','heads'),{recursive:true});
  fs.mkdirSync(path.join(dir,'sessions'),{recursive:true});

  const empty={version:1,nodes:{},edges:{},metadata:{}};
  const graph={
    version:1,
    nodes:{
      legacy:{id:'legacy',kind:'belief',title:'Legacy JSON cognition',body:'preserve me',tags:[],status:'active',trustZone:'model_inferred',grade:'hypothesis',createdAt:'2026-01-01T00:00:00.000Z',updatedAt:'2026-01-01T00:00:00.000Z',version:1,metadata:{}}
    },
    edges:{},
    metadata:{}
  };
  const genesis={
    id:'genesis1234567890',
    formatVersion:1,
    message:'Initialize cognitive repository',
    parents:[],
    createdAt:'2026-01-01T00:00:00.000Z',
    graphHash:'old-genesis',
    diff:{operations:[]},
    metadata:{genesis:true},
    snapshot:empty
  };
  const add={
    id:'commit1234567890',
    formatVersion:1,
    message:'legacy commit',
    parents:[genesis.id],
    createdAt:'2026-01-01T00:00:01.000Z',
    graphHash:'old-graph',
    diff:{operations:[{type:'put_node',id:'legacy',before:null,after:graph.nodes.legacy}]},
    metadata:{},
    snapshot:graph
  };
  const session={
    id:'session_legacy',
    createdAt:'2026-01-01T00:00:02.000Z',
    updatedAt:'2026-01-01T00:00:03.000Z',
    status:'completed',
    provider:'mock',
    model:'mock',
    goal:'legacy session',
    messages:[{role:'user',content:'old'}],
    steps:[{step:1,finishReason:'stop',toolCalls:[]}],
    metadata:{},
    final:'old done'
  };

  fs.writeFileSync(path.join(dir,'graph.json'),JSON.stringify(graph));
  fs.writeFileSync(path.join(dir,'graph.revision'),'7\n');
  fs.writeFileSync(path.join(dir,'HEAD'),'ref: refs/heads/main\n');
  fs.writeFileSync(path.join(dir,'refs','heads','main'),add.id+'\n');
  fs.writeFileSync(path.join(dir,'commits',genesis.id+'.json'),JSON.stringify(genesis));
  fs.writeFileSync(path.join(dir,'commits',add.id+'.json'),JSON.stringify(add));
  fs.writeFileSync(path.join(dir,'sessions',session.id+'.json'),JSON.stringify(session));
  fs.writeFileSync(path.join(dir,'journal.jsonl'),JSON.stringify({at:'2026-01-01T00:00:04.000Z',event:'legacy-event',value:1})+'\n');
  fs.writeFileSync(path.join(dir,'search-index.json'),'{}');

  const repo=new CognitiveRepository(root);
  assert.equal(repo.exists(),true);
  assert.equal(repo.graph().getNode('legacy').body,'preserve me');
  assert.equal(repo.headCommitId(),add.id);
  assert.equal(repo.getCommit(add.id).snapshot.nodes.legacy.body,'preserve me');
  assert.equal(repo.graphRevision(),7);

  const store=new AgentSessionStore(repo.dir);
  assert.equal(store.load(session.id).final,'old done');
  assert.equal(repo.journal(5)[0].event,'legacy-event');

  assert.equal(fs.existsSync(path.join(dir,'lumencortex.db')),true);
  assert.equal(fs.existsSync(path.join(dir,'graph.json')),false);
  const backups=fs.readdirSync(dir).filter(name=>name.startsWith('json-backup-'));
  assert.equal(backups.length,1);
  assert.equal(fs.existsSync(path.join(dir,backups[0],'graph.json')),true);

  const reopened=new CognitiveRepository(root);
  assert.equal(reopened.graph().getNode('legacy').body,'preserve me');
  assert.equal(fs.readdirSync(dir).filter(name=>name.startsWith('json-backup-')).length,1);
});


test('single-node graph mutation only dirties and refreshes that search row', () => {
  const root=tempWorkspace('lcx-incremental-');
  const repo=new CognitiveRepository(root);
  repo.init();
  let graph=repo.graph();
  graph.addNode({
    id:'a',kind:'evidence',title:'A',body:'function alphaSymbol() {}',
    grade:'static',trustZone:'repo_trusted',metadata:{sourceKind:'file-chunk',path:'a.js'}
  });
  graph.addNode({
    id:'b',kind:'evidence',title:'B',body:'function betaSymbol() {}',
    grade:'static',trustZone:'repo_trusted',metadata:{sourceKind:'file-chunk',path:'b.js'}
  });
  repo.writeGraph(graph.snapshot());

  const runtime=new LumenCortexRuntime(repo);
  runtime.refreshSearchIndex();

  const db=new DatabaseSync(path.join(root,'.lumencortex','lumencortex.db'));
  try {
    assert.equal(Number(db.prepare('SELECT count(*) AS n FROM search_dirty_nodes').get().n),0);

    graph=repo.graph();
    graph.updateNode('a',{body:'function alphaRenamedSymbol() {}'});
    repo.writeGraph(graph.snapshot());

    const dirty=db.prepare('SELECT node_id, removed FROM search_dirty_nodes ORDER BY node_id').all();
    assert.deepEqual(dirty.map(row=>[row.node_id,Number(row.removed)]),[['a',0]]);

    const beforeB=db.prepare('SELECT title, path FROM search_documents WHERE node_id = ?').get('b');
    const hits=runtime.search('alphaRenamedSymbol');
    assert.equal(hits[0].nodeId,'a');
    assert.equal(Number(db.prepare('SELECT count(*) AS n FROM search_dirty_nodes').get().n),0);
    const afterB=db.prepare('SELECT title, path FROM search_documents WHERE node_id = ?').get('b');
    assert.deepEqual(afterB,beforeB);
  } finally {
    db.close();
  }
});


test('stale graph writer is rejected instead of overwriting a newer WAL commit', () => {
  const root=tempWorkspace('lcx-revision-conflict-');
  const first=new CognitiveRepository(root);
  first.init();
  const second=new CognitiveRepository(root);

  const graphA=first.graph();
  const graphB=second.graph();

  graphA.addNode({id:'from-a',kind:'entity',title:'writer A'});
  first.writeGraph(graphA.snapshot());

  graphB.addNode({id:'from-b',kind:'entity',title:'writer B'});
  assert.throws(
    () => second.writeGraph(graphB.snapshot()),
    (error) => error?.code === 'GRAPH_REVISION_CONFLICT'
  );

  const reloaded=second.graph();
  assert.ok(reloaded.getNode('from-a'));
  reloaded.addNode({id:'from-b',kind:'entity',title:'writer B'});
  second.writeGraph(reloaded.snapshot());

  const finalGraph=first.graph();
  assert.ok(finalGraph.getNode('from-a'));
  assert.ok(finalGraph.getNode('from-b'));
});


test('SQLite integrity remains valid after graph, session, journal and FTS mutations', () => {
  const root=tempWorkspace('lcx-integrity-');
  const repo=new CognitiveRepository(root);
  repo.init();

  let graph=repo.graph();
  graph.addNode({
    id:'integrity-node',
    kind:'evidence',
    title:'Integrity source',
    body:'function integritySymbol() { return true; }',
    grade:'static',
    trustZone:'repo_trusted',
    metadata:{sourceKind:'file-chunk',path:'integrity.js'}
  });
  repo.writeGraph(graph.snapshot());
  repo.commit('integrity graph');

  const runtime=new LumenCortexRuntime(repo);
  runtime.refreshSearchIndex();
  assert.equal(runtime.search('integritySymbol')[0].nodeId,'integrity-node');

  const store=new AgentSessionStore(repo.dir);
  const session=store.create({goal:'integrity session'});
  session.messages.push({role:'user',content:'verify'});
  session.steps.push({step:1,finishReason:'stop',toolCalls:[]});
  session.status='completed';
  store.save(session);
  repo.appendJournal('integrity-check',{sessionId:session.id});

  const db=new DatabaseSync(path.join(root,'.lumencortex','lumencortex.db'));
  try {
    const result=db.prepare('PRAGMA integrity_check').get();
    assert.equal(result.integrity_check,'ok');
  } finally {
    db.close();
  }
});


test('active promotion incrementally synchronizes search index without full dirty backlog', () => {
  const root=tempWorkspace('lcx-promotion-index-');
  const repo=new CognitiveRepository(root);
  repo.init();

  let graph=repo.graph();
  for (const [id,title] of [['a','Inventory validation'],['b','Inventory locking'],['c','Inventory allocation']]) {
    graph.addNode({
      id,
      kind:'entity',
      title,
      body:title,
      metadata:{path:`src/${id}.txt`}
    });
  }
  repo.writeGraph(graph.snapshot());

  const runtime=new LumenCortexRuntime(repo);
  runtime.refreshSearchIndex();
  const abstraction=runtime.promote(['a','b','c'],{id:'inventory-abstract',title:'Inventory consistency'});
  assert.equal(abstraction.id,'inventory-abstract');

  const db=new DatabaseSync(path.join(root,'.lumencortex','lumencortex.db'));
  try {
    assert.equal(Number(db.prepare('SELECT count(*) AS n FROM search_dirty_nodes').get().n),0);
    const searchRevision=Number(db.prepare("SELECT value FROM metadata WHERE key='search_index_revision'").get().value);
    assert.equal(searchRevision,repo.graphRevision());
    const indexed=db.prepare('SELECT node_id FROM search_documents WHERE node_id = ?').get('inventory-abstract');
    assert.equal(indexed.node_id,'inventory-abstract');
  } finally {
    db.close();
  }
});
