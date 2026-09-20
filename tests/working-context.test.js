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
