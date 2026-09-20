#!/usr/bin/env node
process.env.LUMENCORTEX_INVOKED_AS = 'modelweave';
if (process.env.LUMENCORTEX_SILENCE_LEGACY_WARNING !== '1') {
  console.error('[deprecated] "modelweave" was renamed to "lcx". Use "lumencortex" to open the TUI.');
}
await import('../src/cli.js');
