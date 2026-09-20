import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { LspManager } from '../src/lsp.js';

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
  if(msg.id===undefined)return;
  let result=null;
  if(msg.method==='initialize') result={capabilities:{definitionProvider:true,referencesProvider:true,documentSymbolProvider:true,hoverProvider:true}};
  else if(msg.method==='textDocument/definition') result={uri:msg.params.textDocument.uri,range:{start:{line:0,character:0},end:{line:0,character:5}}};
  else if(msg.method==='textDocument/references') result=[{uri:msg.params.textDocument.uri,range:{start:{line:0,character:0},end:{line:0,character:5}}}];
  else if(msg.method==='textDocument/documentSymbol') result=[{name:'hello',kind:12,range:{start:{line:0,character:0},end:{line:0,character:10}},selectionRange:{start:{line:0,character:9},end:{line:0,character:14}}}];
  else if(msg.method==='textDocument/hover') result={contents:{kind:'plaintext',value:'hello(): string'}};
  else if(msg.method==='textDocument/diagnostic') result={items:[]};
  else if(msg.method==='shutdown') result=null;
  send({jsonrpc:'2.0',id:msg.id,result});
}
`;

test('LSP manager speaks Content-Length JSON-RPC and resolves workspace files', async () => {
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
  } finally {
    await lsp.close();
  }
});
