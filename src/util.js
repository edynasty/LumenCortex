import { createHash, randomUUID } from 'node:crypto';

export function nowIso() {
  return new Date().toISOString();
}

export function id(prefix = 'mw') {
  return `${prefix}_${randomUUID().replaceAll('-', '').slice(0, 16)}`;
}

export function stableStringify(value) {
  return JSON.stringify(sortDeep(value));
}

function sortDeep(value) {
  if (Array.isArray(value)) return value.map(sortDeep);
  if (value && typeof value === 'object') {
    return Object.keys(value)
      .sort()
      .reduce((acc, key) => {
        acc[key] = sortDeep(value[key]);
        return acc;
      }, {});
  }
  return value;
}

export function hash(value) {
  return createHash('sha256').update(typeof value === 'string' ? value : stableStringify(value)).digest('hex');
}

export function clone(value) {
  return structuredClone(value);
}

export function isEqual(a, b) {
  return stableStringify(a) === stableStringify(b);
}

export function tokenize(text = '') {
  return String(text)
    .toLowerCase()
    .replace(/[\p{P}\p{S}]+/gu, ' ')
    .split(/\s+/)
    .flatMap((token) => token ? [token, ...bigrams(token)] : [])
    .filter((x) => x.length > 1);
}

function bigrams(token) {
  if (token.length < 4 || /^[a-z0-9_-]+$/i.test(token)) return [];
  const out = [];
  for (let i = 0; i < token.length - 1; i += 1) out.push(token.slice(i, i + 2));
  return out;
}

export function lexicalScore(a, b) {
  const left = new Set(tokenize(a));
  const right = new Set(tokenize(b));
  if (!left.size || !right.size) return 0;
  let intersection = 0;
  for (const token of left) if (right.has(token)) intersection += 1;
  return intersection / Math.sqrt(left.size * right.size);
}

export function estimateTokens(value) {
  const text = typeof value === 'string' ? value : stableStringify(value);
  // Good enough for budgeting without binding the runtime to one tokenizer.
  return Math.max(1, Math.ceil([...text].length / 3.2));
}

export function clamp(n, min = 0, max = 1) {
  return Math.min(max, Math.max(min, n));
}

export function uniq(values) {
  return [...new Set(values)];
}
