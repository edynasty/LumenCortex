import test from 'node:test';
import assert from 'node:assert/strict';
import { renderTuiFrame } from '../src/tui.js';

test('TUI frame renders provider, sessions, events and answer in bounded terminal layout', () => {
  const frame=renderTuiFrame({
    provider:'deepseek/deepseek-flash',
    currentSessionId:'session_b',
    sessions:[
      {id:'session_a',status:'completed',goal:'old task'},
      {id:'session_b',status:'running',goal:'current task'}
    ],
    events:['step 2 → model','tool → code_search'],
    answer:'changed two files\nall tests passed',
    busy:true,
    width:90
  });
  assert.match(frame,/ModelWeave TUI/);
  assert.match(frame,/\[RUNNING\]/);
  assert.match(frame,/> session_b/);
  assert.match(frame,/tool → code_search/);
  assert.match(frame,/all tests passed/);
  assert.match(frame,/:parallel/);
});
