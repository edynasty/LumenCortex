import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { AgentLoop } from '../src/agent.js';
import { CognitiveRepository } from '../src/repository.js';
import { LumenCortexRuntime } from '../src/runtime.js';
import { ToolRegistry } from '../src/tools.js';

test('old tool evidence returns through the cognitive graph after chat rounds are paged out', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-graph-memory-agent-'));
  const repository = new CognitiveRepository(root);
  repository.init();
  const runtime = new LumenCortexRuntime(repository);

  const values = ['memory-one', 'memory-two', 'memory-three', 'memory-four'];
  let call = 0;
  let finalRequest;
  const provider = {
    model: 'mock-graph-memory',
    async complete({ messages }) {
      call += 1;
      if (call <= values.length) {
        return {
          message: {
            role: 'assistant',
            content: '',
            tool_calls: [{
              id: `memory-call-${call}`,
              type: 'function',
              function: {
                name: 'remember',
                arguments: JSON.stringify({ value: values[call - 1] })
              }
            }]
          },
          finishReason: 'tool_calls'
        };
      }
      finalRequest = messages;
      return { message: { role: 'assistant', content: 'memory-complete' }, finishReason: 'stop' };
    }
  };

  const tools = new ToolRegistry().register({
    name: 'remember',
    permission: 'read',
    parameters: {
      type: 'object',
      properties: { value: { type: 'string' } },
      required: ['value']
    },
    execute: ({ value }) => `observed:${value}`
  });

  const agent = new AgentLoop({ provider, repository, runtime, workspace: root, tools });
  const result = await agent.run('collect four observations and retain them', {
    recentRounds: 1,
    workingChars: 4500,
    budgetTokens: 4000,
    autoIngest: false,
    autoPromote: false,
    recordTask: false
  });

  assert.equal(result.final, 'memory-complete');

  const activeContext = finalRequest.find(
    (message, index) => index > 0 && message.role === 'system' && message.content.includes('active cognitive context')
  );
  assert.ok(activeContext);
  for (const value of values) {
    assert.match(activeContext.content, new RegExp(value));
  }

  const conversationalTail = finalRequest
    .filter((message) => message.role !== 'system')
    .map((message) => String(message.content ?? ''))
    .join('\n');
  assert.doesNotMatch(conversationalTail, /memory-one/);
  assert.match(conversationalTail, /memory-four/);

  assert.equal(result.session.metadata.recentObservationNodeIds.length, 4);
  assert.equal(result.session.metadata.contextHistory.length, 5);
});
