import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { spawn } from 'node:child_process';
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
  fs.mkdirSync(path.join(dir,'refs','heads','feature'),{recursive:true});
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
  fs.writeFileSync(path.join(dir,'refs','heads','feature','nested'),add.id+'\n');
  fs.writeFileSync(path.join(dir,'commits',genesis.id+'.json'),JSON.stringify(genesis));
  fs.writeFileSync(path.join(dir,'commits',add.id+'.json'),JSON.stringify(add));
  fs.writeFileSync(path.join(dir,'sessions',session.id+'.json'),JSON.stringify(session));
  fs.writeFileSync(path.join(dir,'journal.jsonl'),JSON.stringify({at:'2026-01-01T00:00:04.000Z',event:'legacy-event',value:1})+'\n');
  fs.writeFileSync(path.join(dir,'search-index.json'),'{}');

  const repo=new CognitiveRepository(root);
  assert.equal(repo.exists(),true);
  assert.equal(repo.graph().getNode('legacy').body,'preserve me');
  assert.equal(repo.headCommitId(),add.id);
  assert.ok(repo.branches().some(branch=>branch.name==='feature/nested' && branch.commitId===add.id));
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


test('session growth upserts new rows without rewriting existing message and step rows', () => {
  const root=tempWorkspace('lcx-session-incremental-');
  const repo=new CognitiveRepository(root);
  repo.init();
  const store=new AgentSessionStore(repo.dir);

  const session=store.create({goal:'incremental session',provider:'mock',model:'mock'});
  session.messages.push({role:'user',content:'first'});
  session.steps.push({step:1,finishReason:'tool_calls',toolCalls:[]});
  store.save(session);

  const db=new DatabaseSync(path.join(root,'.lumencortex','lumencortex.db'));
  try {
    const messageRowid=Number(db.prepare(
      'SELECT rowid FROM session_messages WHERE session_id = ? AND seq = 0'
    ).get(session.id).rowid);
    const stepRowid=Number(db.prepare(
      'SELECT rowid FROM agent_steps WHERE session_id = ? AND step = 1'
    ).get(session.id).rowid);

    session.messages.push({role:'assistant',content:'second'});
    session.steps.push({step:2,finishReason:'stop',toolCalls:[]});
    session.status='completed';
    store.save(session);

    assert.equal(
      Number(db.prepare('SELECT rowid FROM session_messages WHERE session_id = ? AND seq = 0').get(session.id).rowid),
      messageRowid
    );
    assert.equal(
      Number(db.prepare('SELECT rowid FROM agent_steps WHERE session_id = ? AND step = 1').get(session.id).rowid),
      stepRowid
    );
    assert.equal(
      Number(db.prepare('SELECT count(*) AS n FROM session_messages WHERE session_id = ?').get(session.id).n),
      2
    );
    assert.equal(
      Number(db.prepare('SELECT count(*) AS n FROM agent_steps WHERE session_id = ?').get(session.id).n),
      2
    );
  } finally {
    db.close();
  }
});


test('tool-call step payload round-trips through SQLite exactly', () => {
  const root=tempWorkspace('lcx-toolcall-roundtrip-');
  const repo=new CognitiveRepository(root);
  repo.init();
  const store=new AgentSessionStore(repo.dir);
  const session=store.create({goal:'persist tool call',provider:'mock',model:'mock-model'});
  session.steps.push({
    step:1,
    at:'2026-09-20T00:00:00.000Z',
    focus:{goal:'persist tool call'},
    contextNodeIds:['evidence-a'],
    contextTokens:321,
    promotionId:null,
    finishReason:'tool_calls',
    toolCalls:[{
      id:'call_1',
      name:'read_file',
      args:{path:'memory-long/item-01.txt'},
      ok:true,
      denied:false,
      observationId:'obs_123'
    }],
    content:''
  });
  session.messages.push({
    role:'assistant',
    content:'',
    tool_calls:[{
      id:'call_1',
      type:'function',
      function:{name:'read_file',arguments:'{"path":"memory-long/item-01.txt"}'}
    }]
  });
  session.messages.push({
    role:'tool',
    tool_call_id:'call_1',
    name:'read_file',
    content:'VALUE_01=9123'
  });
  store.save(session);

  const reopened=new AgentSessionStore(repo.dir).load(session.id);
  assert.equal(reopened.steps.length,1);
  assert.deepEqual(reopened.steps[0].toolCalls,session.steps[0].toolCalls);
  assert.equal(reopened.steps[0].toolCalls[0].observationId,'obs_123');
  assert.equal(reopened.steps[0].toolCalls[0].args.path,'memory-long/item-01.txt');
  assert.equal(reopened.messages[0].tool_calls[0].function.name,'read_file');
});


test('separate Node processes can persist independent sessions concurrently under WAL', { timeout: 12000 }, async () => {
  const root=tempWorkspace('lcx-wal-concurrency-');
  const repo=new CognitiveRepository(root);
  repo.init();
  repo.close?.();

  const worker=path.join(root,'session-writer.mjs');
  fs.writeFileSync(worker, `
    import { AgentSessionStore } from ${JSON.stringify(new URL('../src/session.js', import.meta.url).href)};
    const [repoDir,id,delay] = process.argv.slice(2);
    const store=new AgentSessionStore(repoDir);
    const session=store.create({id,goal:'parallel '+id,provider:'mock',model:'mock'});
    for(let i=0;i<12;i+=1){
      session.messages.push({role:'assistant',content:id+':'+i});
      session.steps.push({step:i+1,finishReason:'tool_calls',toolCalls:[]});
      store.save(session);
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,Number(delay));
    }
    session.status='completed';
    session.final='done '+id;
    store.save(session);
    store.close?.();
  `);

  const repoDir=path.join(root,'.lumencortex');
  const run=(id,delay)=>new Promise((resolve,reject)=>{
    const child=spawn(process.execPath,[worker,repoDir,id,String(delay)],{stdio:['ignore','pipe','pipe']});
    let stderr='';
    child.stderr.on('data',chunk=>stderr+=chunk);
    child.on('error',reject);
    child.on('exit',code=>code===0?resolve():reject(new Error(`${id} exit=${code} ${stderr}`)));
  });

  const reader=new AgentSessionStore(repoDir);
  const tasks=[
    run('session_parallel_a',3),
    run('session_parallel_b',4)
  ];

  let observed=false;
  while(true){
    const states=reader.list(10).filter(x=>x.id.startsWith('session_parallel_'));
    if(states.length>=1) observed=true;
    const settled=await Promise.race([
      Promise.all(tasks).then(()=>true),
      new Promise(resolve=>setTimeout(()=>resolve(false),15))
    ]);
    if(settled) break;
  }
  await Promise.all(tasks);

  const a=reader.load('session_parallel_a');
  const b=reader.load('session_parallel_b');
  assert.equal(observed,true);
  assert.equal(a.status,'completed');
  assert.equal(b.status,'completed');
  assert.equal(a.messages.length,12);
  assert.equal(b.messages.length,12);
  assert.equal(a.steps.length,12);
  assert.equal(b.steps.length,12);
  assert.equal(a.final,'done session_parallel_a');
  assert.equal(b.final,'done session_parallel_b');
  reader.close?.();
});


test('database maintenance status integrity checkpoint and journal are operational', () => {
  const root=tempWorkspace('lcx-db-maint-');
  const repo=new CognitiveRepository(root);
  repo.init();
  repo.appendJournal('maintenance-test',{value:42});

  const status=repo.database.status();
  assert.equal(status.journalMode.toLowerCase(),'wal');
  assert.equal(status.schemaVersion,1);
  assert.ok(status.fileSizeBytes>0);
  assert.ok(status.counts.cognitiveCommits>=1);
  assert.equal(status.counts.journal,1);

  const integrity=repo.database.integrityCheck();
  assert.equal(integrity.ok,true);
  assert.deepEqual(integrity.messages,['ok']);

  const checkpoint=repo.database.checkpoint('PASSIVE');
  assert.equal(checkpoint.mode,'PASSIVE');
  assert.ok(Array.isArray(checkpoint.rows));

  const journal=repo.journal(5);
  assert.equal(journal[0].event,'maintenance-test');
  assert.equal(journal[0].payload.value,42);
  repo.close();
});
