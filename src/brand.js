import fs from 'node:fs';
import path from 'node:path';

export const BRAND = Object.freeze({
  name: 'LumenCortex',
  cli: 'lcx',
  fullCli: 'lumencortex',
  legacyCli: 'modelweave',
  stateDir: '.lumencortex',
  legacyStateDir: '.modelweave',
  version: '0.5.0'
});

export function resolveStateDir(workspace = process.cwd(), { migrateLegacy = true } = {}) {
  const root = path.resolve(workspace);
  const preferred = path.join(root, BRAND.stateDir);
  const legacy = path.join(root, BRAND.legacyStateDir);

  if (fs.existsSync(preferred)) return preferred;
  if (!migrateLegacy || !fs.existsSync(legacy)) return preferred;

  try {
    fs.renameSync(legacy, preferred);
  } catch (error) {
    if (error?.code !== 'EXDEV') throw error;
    fs.cpSync(legacy, preferred, { recursive: true, errorOnExist: true });
    fs.rmSync(legacy, { recursive: true, force: true });
  }
  return preferred;
}

export function resolveConfigFile(workspace, name) {
  return path.join(resolveStateDir(workspace), name);
}

export function envValue(primary, legacy) {
  return process.env[primary] ?? process.env[legacy];
}
