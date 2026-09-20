import test from 'node:test';
import assert from 'node:assert/strict';
import { OpenAICompatibleProvider, PROVIDER_PRESETS } from '../src/provider.js';

test('provider sends tools and normalizes tool calls', async () => {
  let request;
  const fetchImpl = async (url, init) => {
    request = { url, init, body: JSON.parse(init.body) };
    return new Response(JSON.stringify({
      model: 'fake',
      choices: [{ finish_reason: 'tool_calls', message: { role: 'assistant', content: null, tool_calls: [{ id: 'c1', type: 'function', function: { name: 'read_file', arguments: { path: 'a.js' } } }] } }],
      usage: { prompt_tokens: 1, completion_tokens: 2, total_tokens: 3 }
    }), { status: 200, headers: { 'content-type': 'application/json' } });
  };
  const provider = new OpenAICompatibleProvider({ baseURL: 'https://example.test/v1', apiKey: 'x', model: 'm', fetchImpl });
  const result = await provider.complete({ messages: [{ role: 'user', content: 'x' }], tools: [{ type: 'function', function: { name: 'read_file', parameters: { type: 'object' } } }] });
  assert.equal(request.url, 'https://example.test/v1/chat/completions');
  assert.equal(request.body.tool_choice, 'auto');
  assert.equal(result.message.tool_calls[0].function.arguments, '{"path":"a.js"}');
});


test('DeepSeek preset points at the official OpenAI-compatible API', () => {
  assert.equal(PROVIDER_PRESETS.deepseek.baseURL, 'https://api.deepseek.com');
  assert.equal(PROVIDER_PRESETS.deepseek.apiKeyEnv, 'DEEPSEEK_API_KEY');
  assert.equal(PROVIDER_PRESETS.deepseek.defaultModel, 'deepseek-flash');
});
