import { BRAND } from './brand.js';
export const PROVIDER_PRESETS = {
  openrouter: {
    baseURL: 'https://openrouter.ai/api/v1',
    apiKeyEnv: 'OPENROUTER_API_KEY',
    defaultModel: 'openrouter/free',
    headers: { 'HTTP-Referer': 'https://github.com/edynasty/LumenCortex', 'X-Title': BRAND.name },
    reasoning: { format: 'reasoning-object', supported: ['none', 'low', 'medium', 'high', 'max'] }
  },
  'openrouter-deepseek-free': {
    baseURL: 'https://openrouter.ai/api/v1',
    apiKeyEnv: 'OPENROUTER_API_KEY',
    defaultModel: 'deepseek/deepseek-v4-flash-0731:free',
    headers: { 'HTTP-Referer': 'https://github.com/edynasty/LumenCortex', 'X-Title': 'LumenCortex DeepSeek Free' },
    reasoning: { format: 'reasoning-object', supported: ['none', 'low', 'medium', 'high', 'max'] }
  },
  groq: {
    baseURL: 'https://api.groq.com/openai/v1',
    apiKeyEnv: 'GROQ_API_KEY',
    defaultModel: 'openai/gpt-oss-120b',
    reasoning: {
      format: 'reasoning-effort',
      supported: ['low', 'medium', 'high'],
      map: { max: 'high' }
    }
  },
  deepseek: {
    baseURL: 'https://api.deepseek.com',
    apiKeyEnv: 'DEEPSEEK_API_KEY',
    defaultModel: 'deepseek-flash',
    reasoning: {
      format: 'deepseek',
      supported: ['none', 'low', 'high', 'max'],
      map: { medium: 'high' }
    }
  },
  generic: {
    baseURL: process.env.LUMENCORTEX_BASE_URL,
    apiKeyEnv: 'LUMENCORTEX_API_KEY',
    defaultModel: process.env.LUMENCORTEX_MODEL
  }
};

export class OpenAICompatibleProvider {
  constructor({
    baseURL,
    apiKey,
    model,
    providerName = 'generic',
    headers = {},
    fetchImpl = globalThis.fetch,
    timeoutMs = 120000,
    reasoningProfile = null
  }) {
    if (!baseURL) throw new Error('Provider baseURL is required');
    if (!model) throw new Error('Provider model is required');
    if (typeof fetchImpl !== 'function') throw new Error('fetch implementation is required');
    this.baseURL = baseURL.replace(/\/$/, '');
    this.apiKey = apiKey;
    this.model = model;
    this.providerName = providerName;
    this.headers = headers;
    this.reasoningProfile = reasoningProfile;
    this.fetch = fetchImpl;
    this.timeoutMs = timeoutMs;
  }

  async complete({ messages, tools = [], toolChoice = 'auto', temperature, maxTokens, reasoningEffort, signal } = {}) {
    const controller = signal ? null : new AbortController();
    const timeout = controller ? setTimeout(() => controller.abort(), this.timeoutMs) : null;
    try {
      const body = {
        model: this.model,
        messages,
        ...(tools.length ? { tools, tool_choice: toolChoice } : {}),
        ...(temperature !== undefined ? { temperature } : {}),
        ...(maxTokens !== undefined ? { max_tokens: maxTokens } : {}),
        ...reasoningRequestFields(this.reasoningProfile, reasoningEffort)
      };
      const response = await this.fetch(`${this.baseURL}/chat/completions`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(this.apiKey ? { Authorization: `Bearer ${this.apiKey}` } : {}),
          ...this.headers
        },
        body: JSON.stringify(body),
        signal: signal ?? controller.signal
      });
      const text = await response.text();
      let data;
      try { data = text ? JSON.parse(text) : {}; } catch { data = { raw: text }; }
      if (!response.ok) {
        const message = data?.error?.message ?? data?.message ?? text ?? `HTTP ${response.status}`;
        const error = new Error(`LLM request failed (${response.status}): ${message}`);
        error.status = response.status;
        error.response = data;
        throw error;
      }
      const choice = data?.choices?.[0];
      if (!choice?.message) throw new Error('LLM response missing choices[0].message');
      return {
        message: normalizeAssistantMessage(choice.message),
        finishReason: choice.finish_reason ?? null,
        usage: data.usage ?? null,
        model: data.model ?? this.model,
        raw: data
      };
    } finally {
      if (timeout) clearTimeout(timeout);
    }
  }
}

export function createProvider(name = process.env.LUMENCORTEX_PROVIDER ?? 'openrouter', options = {}) {
  const preset = PROVIDER_PRESETS[name];
  if (!preset) throw new Error(`Unknown provider: ${name}`);
  const apiKey = options.apiKey ?? process.env[preset.apiKeyEnv];
  const model = options.model ?? preset.defaultModel;
  const baseURL = options.baseURL ?? preset.baseURL;
  if (!apiKey && name !== 'generic') {
    throw new Error(`Missing ${preset.apiKeyEnv}. Set it before running LumenCortex agent.`);
  }
  if (!apiKey && name === 'generic' && process.env.LUMENCORTEX_REQUIRE_API_KEY !== 'false') {
    throw new Error(`Missing ${preset.apiKeyEnv}. Set LUMENCORTEX_REQUIRE_API_KEY=false for unauthenticated local endpoints.`);
  }
  return new OpenAICompatibleProvider({
    baseURL,
    apiKey,
    model,
    providerName: name,
    headers: { ...(preset.headers ?? {}), ...(options.headers ?? {}) },
    fetchImpl: options.fetchImpl,
    timeoutMs: options.timeoutMs,
    reasoningProfile: options.reasoningProfile ?? preset.reasoning ?? null
  });
}

export function providerInfo() {
  return Object.entries(PROVIDER_PRESETS).map(([name, preset]) => ({
    name,
    baseURL: preset.baseURL ?? '(LUMENCORTEX_BASE_URL)',
    apiKeyEnv: preset.apiKeyEnv,
    defaultModel: preset.defaultModel ?? '(LUMENCORTEX_MODEL)',
    reasoning: preset.reasoning?.format ?? 'provider-default'
  }));
}

function normalizeAssistantMessage(message) {
  return {
    role: 'assistant',
    content: message.content ?? '',
    ...(message.tool_calls ? { tool_calls: message.tool_calls.map((call) => ({
      id: call.id,
      type: call.type ?? 'function',
      function: {
        name: call.function?.name,
        arguments: typeof call.function?.arguments === 'string'
          ? call.function.arguments
          : JSON.stringify(call.function?.arguments ?? {})
      }
    })) } : {}),
    ...((message.reasoning ?? message.reasoning_content)
      ? { reasoning: message.reasoning ?? message.reasoning_content }
      : {})
  };
}


export function reasoningRequestFields(profile, effort) {
  if (!profile || effort === undefined || effort === null) return {};
  const requested = String(effort).toLowerCase();
  const mapped = profile.map?.[requested] ?? requested;
  if (!profile.supported?.includes(mapped)) return {};

  if (profile.format === 'reasoning-object') {
    return { reasoning: { effort: mapped } };
  }

  if (profile.format === 'reasoning-effort') {
    return { reasoning_effort: mapped };
  }

  if (profile.format === 'deepseek') {
    return {
      reasoning_effort: mapped,
      thinking: { type: mapped === 'none' ? 'disabled' : 'enabled' }
    };
  }

  return {};
}
