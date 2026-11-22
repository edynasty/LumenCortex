"""Black-box CLI fixture. The local transport supplies tool calls, never file results."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

root = Path(__file__).resolve().parent
ws = root / 'sample'
ws.mkdir(exist_ok=True)
(ws / 'a.txt').unlink(missing_ok=True)
(ws / 'b.txt').write_text('original B\n')
calls = [('Write', {'file_path': str(ws / 'a.txt'), 'content': 'committed A\n'}),
         ('Read', {'file_path': str(ws / 'b.txt')}),
         ('Edit', {'file_path': str(ws / 'b.txt'), 'old_string': 'missing', 'new_string': 'unused'})]
seen = []
class Gateway(BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers['content-length'])))
        if '/messages' not in self.path or 'messages' not in req:
            self.send_error(404); return
        messages = req['messages']
        results = [b for m in messages if isinstance(m.get('content'), list) for b in m['content'] if b.get('type') == 'tool_result']
        n = len(results)
        if n > len(calls): self.send_error(400); return
        if results:
            last = results[-1]
            print('model-input', last['tool_use_id'], 'error=' + str(last.get('is_error', False)), str(last.get('content', ''))[:200].replace(str(ws), '<fixture>'), flush=True)
            seen.append((last['tool_use_id'], bool(last.get('is_error'))))
        usage = {'input_tokens': 50, 'output_tokens': 5}
        msg = {'id': 'msg_fixture_' + str(n), 'type': 'message', 'role': 'assistant', 'model': req['model'], 'usage': usage, 'content': [], 'stop_reason': None, 'stop_sequence': None}
        events = [('message_start', {'message': msg})]
        if n < len(calls):
            name, args = calls[n]
            block = {'type':'tool_use', 'id': 'toolu_fixture_' + str(n), 'name':name, 'input':{}}
            delta = {'type':'input_json_delta', 'partial_json':json.dumps(args)}
            reason = 'tool_use'
        else:
            block = {'type':'text', 'text':''}; delta = {'type':'text_delta', 'text':'Fixture finished.'}; reason = 'end_turn'
        events += [('content_block_start', {'index':0,'content_block':block}), ('content_block_delta', {'index':0,'delta':delta}), ('content_block_stop', {'index':0}), ('message_delta', {'delta':{'stop_reason':reason,'stop_sequence':None},'usage':usage}), ('message_stop', {})]
        data = ''.join('event: '+t+'\ndata: '+json.dumps(dict(type=t, **v))+'\n\n' for t,v in events).encode()
        self.send_response(200); self.send_header('Content-Type','text/event-stream'); self.send_header('Content-Length',str(len(data))); self.end_headers(); self.wfile.write(data)

server = ThreadingHTTPServer(('127.0.0.1', 0), Gateway)
threading.Thread(target=server.serve_forever, daemon=True).start()
env = {'PATH':os.environ['PATH'], 'HOME':str(root / 'cfg'), 'CLAUDE_CONFIG_DIR':str(root / 'cfg'), 'ANTHROPIC_BASE_URL':'http://127.0.0.1:'+str(server.server_port), 'ANTHROPIC_AUTH_TOKEN':'local-fixture', 'CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC':'1', 'DISABLE_AUTOUPDATER':'1', 'NO_PROXY':'127.0.0.1'}
cmd = [shutil.which('node'), str(root/'package/cli.js'), '-p', 'Run the file-edit fixture.', '--tools','Read,Edit,Write','--allowedTools','Read,Edit,Write','--setting-sources','','--strict-mcp-config','--permission-mode','acceptEdits','--max-turns','5','--no-session-persistence','--output-format','stream-json','--verbose']
try:
    completed = subprocess.run(cmd, env=env, cwd=ws, text=True, capture_output=True, timeout=50)
    for line in completed.stdout.splitlines():
        try: item = json.loads(line)
        except ValueError: continue
        if item.get('type') == 'result': print('CLI', item.get('subtype'), str(item.get('result',''))[:200])
    print('exit',completed.returncode, 'a=',(ws/'a.txt').read_bytes() if (ws/'a.txt').exists() else None,'b=',(ws/'b.txt').read_bytes())
    assert seen == [('toolu_fixture_0',False),('toolu_fixture_1',False),('toolu_fixture_2',True)] and (ws/'a.txt').read_bytes()==b'committed A\n' and (ws/'b.txt').read_bytes()==b'original B\n'
finally:
    server.shutdown()
