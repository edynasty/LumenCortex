import path from 'node:path';

export const BRAND = Object.freeze({
  name: 'LumenCortex',
  cli: 'lcx',
  fullCli: 'lumencortex',
  stateDir: '.lumencortex',
  version: '0.6.0'
});

export function resolveStateDir(workspace = process.cwd()) {
  return path.join(path.resolve(workspace), BRAND.stateDir);
}

export function resolveConfigFile(workspace, name) {
  return path.join(resolveStateDir(workspace), name);
}
