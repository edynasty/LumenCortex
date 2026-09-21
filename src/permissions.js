export const POLICY_NAMES = Object.freeze(['read-only', 'workspace', 'full']);

export function normalizePolicy(value = 'full') {
  const policy = String(value ?? '').trim() || 'full';
  if (!POLICY_NAMES.includes(policy)) {
    throw new Error(`Unknown policy: ${policy}. Expected one of: ${POLICY_NAMES.join(', ')}`);
  }
  return policy;
}

export function policyAllowsTool(tool, policyValue = 'full') {
  const policy = normalizePolicy(policyValue);
  const permission = tool?.permission ?? 'read';
  const scope = tool?.scope ?? 'workspace';

  if (policy === 'full') return true;
  if (policy === 'read-only') return permission === 'read';

  if (permission === 'read') return true;
  if (permission === 'write') return scope === 'workspace';
  return false;
}
