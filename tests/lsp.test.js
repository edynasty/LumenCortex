import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { LspManager, applyLspWorkspaceEdit } from '../src/lsp.js';
import { createCodingTools } from '../src/tools.js';

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
  if(msg.method==='initialize') result={capabilities:{definitionProvider:true,referencesProvider:true,documentSymbolProvider:true,hoverProvider:true,renameProvider:true,codeActionProvider:{resolveProvider:true}}};
  else if(msg.method==='textDocument/definition') result={uri:msg.params.textDocument.uri,range:{start:{line:0,character:0},end:{line:0,character:5}}};
  else if(msg.method==='textDocument/references') result=[{uri:msg.params.textDocument.uri,range:{start:{line:0,character:0},end:{line:0,character:5}}}];
  else if(msg.method==='textDocument/documentSymbol') result=[{name:'hello',kind:12,range:{start:{line:0,character:0},end:{line:0,character:10}},selectionRange:{start:{line:0,character:9},end:{line:0,character:14}}}];
  else if(msg.method==='textDocument/hover') result={contents:{kind:'plaintext',value:'hello(): string'}};
  else if(msg.method==='textDocument/diagnostic') result={items:[]};
  else if(msg.method==='textDocument/rename') result={changes:{[msg.params.textDocument.uri]:[{
    range:{start:{line:0,character:9},end:{line:0,character:14}},
    newText:msg.params.newName
  }]}};
  else if(msg.method==='textDocument/codeAction') result=[
    {
      title:'Replace ok literal',
      kind:'quickfix',
      edit:{changes:{[msg.params.textDocument.uri]:[{
        range:{start:{line:0,character:25},end:{line:0,character:29}},
        newText:'"fixed"'
      }]}}
    },
    {
      title:'Command only',
      kind:'source',
      command:{title:'Run command',command:'fake.command'}
    }
  ];
  else if(msg.method==='codeAction/resolve') result=msg.params;
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

    const actions=await lsp.codeActions('main.js',1,1,1,32);
    assert.equal(actions[0].title,'Replace ok literal');
    const appliedAction=await lsp.applyCodeAction('main.js',actions[0]);
    assert.equal(appliedAction.editCount,1);
    assert.match(fs.readFileSync(source,'utf8'),/"fixed"/);

    const renameEdit=await lsp.rename('main.js',1,10,'world');
    const renamed=await lsp.applyWorkspaceEdit(renameEdit);
    assert.equal(renamed.editCount,1);
    assert.match(fs.readFileSync(source,'utf8'),/function world\(\)/);

    await assert.rejects(
      lsp.applyCodeAction('main.js',actions[1]),
      /command-only/
    );
  } finally {
    await lsp.close();
  }
});


test('LSP WorkspaceEdit validates all files before atomically mutating them', () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'mw-lsp-edit-'));
  const a=path.join(root,'a.js');
  const b=path.join(root,'b.js');
  fs.writeFileSync(a,'const alpha = 1;\n');
  fs.writeFileSync(b,'const beta = 2;\n');

  const applied=applyLspWorkspaceEdit(root,{
    changes:{
      [pathToFileURL(a).href]:[{
        range:{start:{line:0,character:6},end:{line:0,character:11}},
        newText:'first'
      }],
      [pathToFileURL(b).href]:[{
        range:{start:{line:0,character:6},end:{line:0,character:10}},
        newText:'second'
      }]
    }
  });
  assert.equal(applied.editCount,2);
  assert.match(fs.readFileSync(a,'utf8'),/const first/);
  assert.match(fs.readFileSync(b,'utf8'),/const second/);

  const before=fs.readFileSync(a,'utf8');
  assert.throws(
    () => applyLspWorkspaceEdit(root,{
      changes:{
        [pathToFileURL(a).href]:[{
          range:{start:{line:0,character:6},end:{line:0,character:11}},
          newText:'changed'
        }],
        [pathToFileURL(path.join(root,'missing.js')).href]:[{
          range:{start:{line:0,character:0},end:{line:0,character:0}},
          newText:'x'
        }]
      }
    }),
    /target not found/
  );
  assert.equal(fs.readFileSync(a,'utf8'),before);

  assert.throws(
    () => applyLspWorkspaceEdit(root,{
      documentChanges:[{kind:'rename',oldUri:pathToFileURL(a).href,newUri:pathToFileURL(b).href}]
    }),
    /resource operation/
  );
});

test('coding tools expose LSP rename and bounded code-action apply', { timeout: 8000 }, async () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'mw-lsp-tools-'));
  const source=path.join(root,'main.js');
  fs.writeFileSync(source,'function hello(){ return "ok"; }\n');
  const server=path.join(root,'fake-lsp.mjs');
  fs.writeFileSync(server,fakeServer);
  const lsp=new LspManager(root,{config:{servers:{fake:{name:'fake',command:process.execPath,args:[server],extensions:['.js']}}}});
  const tools=createCodingTools({workspace:root,lsp});
  try {
    const listed=await tools.execute('lsp_code_actions',{
      path:'main.js',
      start_line:1,
      start_character:1,
      end_line:1,
      end_character:32,
      include_diagnostics:false
    });
    assert.equal(listed.ok,true);
    assert.match(listed.content,/Replace ok literal/);

    const action=await tools.execute('lsp_code_action_apply',{
      path:'main.js',
      start_line:1,
      start_character:1,
      end_line:1,
      end_character:32,
      action_index:0,
      include_diagnostics:false
    });
    assert.equal(action.ok,true);
    assert.equal(action.mutatesWorkspace,true);
    assert.match(fs.readFileSync(source,'utf8'),/"fixed"/);

    const rename=await tools.execute('lsp_rename',{
      path:'main.js',
      line:1,
      character:10,
      new_name:'world'
    });
    assert.equal(rename.ok,true);
    assert.equal(rename.mutatesWorkspace,true);
    assert.match(fs.readFileSync(source,'utf8'),/function world\(\)/);
  } finally {
    await lsp.close();
  }
});
