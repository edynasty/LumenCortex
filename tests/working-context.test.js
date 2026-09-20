import test from 'node:test';
import assert from 'node:assert/strict';
import { buildWorkingMessages } from '../src/agent.js';

test('working-set pager keeps recent complete tool rounds instead of replaying full session', () => {
  const messages = [
    { role: 'system', content: 'system' },
    { role: 'user', content: 'long task' }
  ];

  for (let i = 1; i <= 10; i += 1) {
    messages.push({
      role: 'assistant',
      content: '',
      tool_calls: [{
        id: `call-${i}`,
        type: 'function',
        function: { name: 'read_file', arguments: `{"path":"file-${i}.txt"}` }
      }]
    });
    messages.push({
      role: 'tool',
      tool_call_id: `call-${i}`,
      name: 'read_file',
      content: `result-${i}`
    });
  }

  const working = buildWorkingMessages(messages, {
    recentRounds: 3,
    maxWorkingChars: 100000
  });

  const toolMessages = working.filter((message) => message.role === 'tool');
  assert.deepEqual(toolMessages.map((message) => message.tool_call_id), [
    'call-8',
    'call-9',
    'call-10'
  ]);
  assert.ok(working.some((message) => message.role === 'system' && message.content === 'system'));
  assert.ok(working.some((message) => message.role === 'user' && message.content === 'long task'));

  const assistantCallIds = new Set(
    working
      .filter((message) => message.role === 'assistant')
      .flatMap((message) => message.tool_calls ?? [])
      .map((call) => call.id)
  );
  for (const tool of toolMessages) assert.ok(assistantCallIds.has(tool.tool_call_id));
});


test('recentRounds=1 keeps the verifier shell result for the next reasoning turn', () => {
  const messages=[
    {role:'system',content:'system'},
    {role:'user',content:'read four values, write report, verify, then finish'}
  ];
  for(let i=1;i<=4;i+=1){
    messages.push({
      role:'assistant',
      content:'',
      tool_calls:[{
        id:`read-${i}`,
        type:'function',
        function:{name:'read_file',arguments:JSON.stringify({path:`item-${i}.txt`})}
      }]
    });
    messages.push({
      role:'tool',
      tool_call_id:`read-${i}`,
      name:'read_file',
      content:`VALUE_${i}=${9100+i}`
    });
  }
  messages.push({
    role:'assistant',
    content:'',
    tool_calls:[{
      id:'write-1',
      type:'function',
      function:{name:'write_file',arguments:'{"path":"report.json","content":"{}"}'}
    }]
  });
  messages.push({role:'tool',tool_call_id:'write-1',name:'write_file',content:'{"ok":true}'});
  messages.push({
    role:'assistant',
    content:'',
    tool_calls:[{
      id:'verify-1',
      type:'function',
      function:{name:'shell',arguments:'{"command":"node verify.mjs"}'}
    }]
  });
  messages.push({
    role:'tool',
    tool_call_id:'verify-1',
    name:'shell',
    content:JSON.stringify({exitCode:0,stdout:'GRAPH_MEMORY_LONG_TASK_OK\n',stderr:''})
  });

  const working=buildWorkingMessages(messages,{recentRounds:1,maxWorkingChars:100000});
  const toolMessages=working.filter(message=>message.role==='tool');
  assert.equal(toolMessages.length,1);
  assert.equal(toolMessages[0].tool_call_id,'verify-1');
  assert.match(toolMessages[0].content,/GRAPH_MEMORY_LONG_TASK_OK/);
  const assistant=working.find(message=>
    message.role==='assistant' &&
    message.tool_calls?.some(call=>call.id==='verify-1')
  );
  assert.ok(assistant,'matching verifier assistant tool-call must be retained');
  assert.equal(
    working.some(message=>message.role==='tool' && String(message.tool_call_id).startsWith('read-')),
    false
  );
});
