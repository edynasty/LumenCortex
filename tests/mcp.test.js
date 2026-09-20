import test from 'node:test';
import assert from 'node:assert/strict';
import { HttpMcpClient, McpManager } from '../src/mcp.js';
import { ToolRegistry } from '../src/tools.js';

function response(value,status=200,type='application/json'){
  return new Response(JSON.stringify(value),{status,headers:{'content-type':type}});
}

test('MCP HTTP client negotiates 2026 modern era and calls tools', async () => {
  const seen=[];
  const fetchImpl=async (_url,init)=>{
    const body=JSON.parse(init.body);
    seen.push({body,headers:init.headers});
    if(body.method==='server/discover') return response({jsonrpc:'2.0',id:body.id,result:{protocolVersion:'2026-07-28',capabilities:{tools:{}}}});
    if(body.method==='tools/list') return response({jsonrpc:'2.0',id:body.id,result:{tools:[{name:'echo',description:'echo',inputSchema:{type:'object',properties:{text:{type:'string'}}}}]}});
    if(body.method==='tools/call') return response({jsonrpc:'2.0',id:body.id,result:{content:[{type:'text',text:body.params.arguments.text}]}});
    throw new Error('unexpected '+body.method);
  };
  const client=new HttpMcpClient('modern',{url:'https://mcp.test/mcp',fetchImpl});
  await client.connect();
  assert.equal(client.era,'modern');
  assert.equal((await client.listTools())[0].name,'echo');
  const result=await client.callTool('echo',{text:'hello'});
  assert.equal(result.content[0].text,'hello');
  assert.equal(seen[0].body.params._meta['io.modelcontextprotocol/clientInfo'].name,'ModelWeave');
  assert.equal(seen[1].headers['MCP-Protocol-Version'],'2026-07-28');
});

test('MCP client falls back to legacy initialize and manager exposes remote tools', async () => {
  const fetchImpl=async (_url,init)=>{
    const body=JSON.parse(init.body);
    if(body.method==='server/discover') return response({jsonrpc:'2.0',id:body.id,error:{code:-32601,message:'Method not found'}});
    if(body.method==='initialize') return response({jsonrpc:'2.0',id:body.id,result:{protocolVersion:'2025-11-25',capabilities:{tools:{}}}});
    if(body.method==='notifications/initialized') return response({jsonrpc:'2.0',result:{}});
    if(body.method==='tools/list') return response({jsonrpc:'2.0',id:body.id,result:{tools:[{name:'upper',description:'upper',annotations:{readOnlyHint:true},inputSchema:{type:'object',properties:{text:{type:'string'}},required:['text']}}]}});
    if(body.method==='tools/call') return response({jsonrpc:'2.0',id:body.id,result:{content:[{type:'text',text:body.params.arguments.text.toUpperCase()}]}});
    throw new Error('unexpected '+body.method);
  };
  const manager=new McpManager(process.cwd(),{config:{servers:{demo:{url:'https://legacy.test/mcp',fetchImpl}}}});
  const registry=new ToolRegistry();
  const added=await manager.registerTools(registry);
  assert.equal(added[0].localName,'mcp_demo_upper');
  const output=await registry.execute('mcp_demo_upper',{text:'modelweave'});
  assert.equal(output.ok,true);
  assert.match(output.content,/MODELWEAVE/);
  assert.equal(manager.clients.get('demo').era,'legacy');
  await manager.close();
});
