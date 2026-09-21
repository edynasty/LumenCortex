import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { LspManager, applyWorkspaceEdit } from '../src/lsp.js';

const fakeServer=`
let buffer=Buffer.alloc(0);
process.stdin.on('data',chunk=>{buffer=Buffer.concat([buffer,chunk]); pump();});
function pump(){
  while(true){
    const h=buffer.indexOf('\\r\\n\\r\\n');
    if(h<0)return;
    const header=buffer.subarray(0,h).toString();
    const m=header.match(/Content-Length:\\s*(\\d+)/i);
    if(!m){buffer=buffer.subarray(h+4);continue;}
    const len=Number(m[1]), start=h+4;
    if(buffer.length<start+len)return;
    const msg=JSON.parse(buffer.subarray(start,start+len).toString());
    buffer=buffer.subarray(start+len);
    handle(msg);
  }
}
function send(obj){
  const body=Buffer.from(JSON.stringify(obj));
  process.stdout.write('Content-Length: '+body.length+'\\r\\n\\r\\n');
  process.stdout.write(body);
}
function handle(msg){
  if(msg.method==='exit'){ process.exit(0); return; }
  if(msg.id===undefined)return;
  let result=null;
  if(msg.method==='initialize') result={capabilities:{definitionProvider:true,referencesProvider:true,documentSymbolProvider:true,hoverProvider:true,renameProvider:true,codeActionProvider:true}};
  else if(msg.method==='textDocument/definition') result={uri:msg.params.textDocument.uri,range:{start:{line:0,character:0},end:{line:0,character:5}}};
  else if(msg.method==='textDocument/references') result=[{uri:msg.params.textDocument.uri,range:{start:{line:0,character:0},end:{line:0,character:5}}}];
  else if(msg.method==='textDocument/documentSymbol') result=[{name:'hello',kind:12,range:{start:{line:0,character:0},end:{line:0,character:10}},selectionRange:{start:{line:0,character:9},end:{line:0,character:14}}}];
  else if(msg.method==='textDocument/hover') result={contents:{kind:'plaintext',value:'hello(): string'}};
  else if(msg.method==='textDocument/rename') result={changes:{[msg.params.textDocument.uri]:[{range:{start:{line:0,character:9},end:{line:0,character:14}},newText:msg.params.newName}]}};
  else if(msg.method==='textDocument/codeAction') result=[{
    title:'Use fixed literal',
    kind:'quickfix',
    edit:{changes:{[msg.params.textDocument.uri]:[{range:{start:{line:0,character:25},end:{line:0,character:29}},newText:'"fixed"'}]}}
  }];
  else if(msg.method==='textDocument/diagnostic') result={items:[]};
  else if(msg.method==='shutdown') result=null;
  send({jsonrpc:'2.0',id:msg.id,result});
}
`;

test('LSP manager speaks Content-Length JSON-RPC and resolves workspace files', { timeout: 8000 }, async () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'mw-lsp-'));
  const source=path.join(root,'main.js');
  fs.writeFileSync(source,'function hello(){ return "ok"; }\n');
  const server=path.join(root,'fake-lsp.mjs');
  fs.writeFileSync(server,fakeServer);

  const lsp=new LspManager(root,{config:{servers:{fake:{name:'fake',command:process.execPath,args:[server],extensions:['.js']}}}});
  try{
    const definition=await lsp.definition('main.js',1,10);
    assert.equal(definition.uri,pathToFileURL(source).href);
    const symbols=await lsp.symbols('main.js');
    assert.equal(symbols[0].name,'hello');
    const hover=await lsp.hover('main.js',1,10);
    assert.match(hover.contents.value,/hello/);
    assert.equal((await lsp.diagnostics('main.js')).length,0);

    const actions=await lsp.codeActions('main.js',1,1,1,32,{only:['quickfix']});
    assert.equal(actions[0].title,'Use fixed literal');
    const appliedAction=await lsp.applyCodeAction('main.js',1,1,1,32,{title:'Use fixed literal'});
    assert.equal(appliedAction.action.title,'Use fixed literal');
    assert.match(fs.readFileSync(source,'utf8'),/return "fixed"/);

    const renamed=await lsp.rename('main.js',1,10,'renamed',{apply:true});
    assert.equal(renamed.editCount,1);
    assert.match(fs.readFileSync(source,'utf8'),/function renamed\(\)/);
  } finally {
    await lsp.close();
  }
});


test('workspace edits reject paths outside the workspace and apply multiple files atomically', () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'lcx-lsp-edit-'));
  const one=path.join(root,'one.js');
  const two=path.join(root,'two.js');
  fs.writeFileSync(one,'const one = 1;\n');
  fs.writeFileSync(two,'const two = 2;\n');

  const result=applyWorkspaceEdit(root,{
    changes:{
      [pathToFileURL(one).href]:[{range:{start:{line:0,character:6},end:{line:0,character:9}},newText:'first'}],
      [pathToFileURL(two).href]:[{range:{start:{line:0,character:6},end:{line:0,character:9}},newText:'second'}]
    }
  });
  assert.equal(result.editCount,2);
  assert.match(fs.readFileSync(one,'utf8'),/const first = 1/);
  assert.match(fs.readFileSync(two,'utf8'),/const second = 2/);

  const outside=path.join(path.dirname(root),'outside.js');
  fs.writeFileSync(outside,'const outside = true;\n');
  assert.throws(() => applyWorkspaceEdit(root,{
    changes:{
      [pathToFileURL(outside).href]:[{range:{start:{line:0,character:0},end:{line:0,character:5}},newText:'let'}]
    }
  }),/escapes workspace/);
});
