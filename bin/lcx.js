#!/usr/bin/env node
process.env.LUMENCORTEX_INVOKED_AS = 'lcx';
if (process.argv.length === 2) process.argv.push('tui');
await import('../src/cli.js');
