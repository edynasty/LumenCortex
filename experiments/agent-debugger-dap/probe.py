#!/usr/bin/env python3
"""Stdlib-only DAP client for the Agent Debugger P0 experiment."""
import json, os, queue, subprocess, sys, threading, time

class DAP:
    def __init__(self, adapter):
        self.proc = subprocess.Popen([adapter], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.PIPE, bufsize=0)
        self.messages, self.events, self.seq = queue.Queue(), [], 0
        threading.Thread(target=self._read_loop, daemon=True).start()

    def _read_loop(self):
        try:
            while True:
                headers = {}
                while True:
                    line = self.proc.stdout.readline()
                    if not line:
                        raise EOFError("DAP adapter stdout closed")
                    if line in (b"\r\n", b"\n"):
                        break
                    key, value = line.decode("ascii").split(":", 1)
                    headers[key.lower()] = value.strip()
                length = int(headers["content-length"])
                if length < 0 or length > 4 * 1024 * 1024:
                    raise ValueError("invalid/oversized DAP frame")
                body = self.proc.stdout.read(length)
                if len(body) != length:
                    raise EOFError("truncated DAP body")
                self.messages.put(json.loads(body))
        except Exception as exc:
            self.messages.put({"_reader_error": str(exc)})

    def send(self, command, arguments=None, timeout=15):
        self.seq += 1
        seq = self.seq
        request = {"seq": seq, "type": "request", "command": command, "arguments": arguments or {}}
        body = json.dumps(request, separators=(",", ":")).encode()
        self.proc.stdin.write(f"Content-Length: {len(body)}\r\n\r\n".encode() + body)
        self.proc.stdin.flush()
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                msg = self.messages.get(timeout=max(.01, deadline-time.monotonic()))
            except queue.Empty:
                break
            if "_reader_error" in msg:
                raise RuntimeError(msg["_reader_error"])
            if msg.get("type") == "event":
                self.events.append(msg)
            elif msg.get("type") == "response" and msg.get("request_seq") == seq:
                if not msg.get("success"):
                    raise RuntimeError(f"{command} failed: {msg.get('message')} {msg.get('body')}")
                return msg.get("body", {})
            else:
                self.messages.put(msg)
                time.sleep(.01)
        raise TimeoutError(f"timeout waiting for DAP response: {command}")

    def wait_event(self, name, timeout=15):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            for i, event in enumerate(self.events):
                if event.get("event") == name:
                    return self.events.pop(i).get("body", {})
            try:
                msg = self.messages.get(timeout=max(.01, deadline-time.monotonic()))
            except queue.Empty:
                break
            if "_reader_error" in msg:
                raise RuntimeError(msg["_reader_error"])
            if msg.get("type") == "event":
                if msg.get("event") == name:
                    return msg.get("body", {})
                self.events.append(msg)
            else:
                self.messages.put(msg)
                time.sleep(.01)
        raise TimeoutError(f"timeout waiting for DAP event: {name}")

    def close(self):
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()

def read_locals(dap, frame_id):
    scopes = dap.send("scopes", {"frameId": frame_id}).get("scopes", [])
    local = next((s for s in scopes if s.get("name", "").lower() in ("locals", "local")),
                 scopes[0] if scopes else None)
    if not local:
        raise RuntimeError(f"no scopes returned: {scopes}")
    variables = dap.send("variables", {"variablesReference": local["variablesReference"]}).get("variables", [])
    return [{"name": v.get("name"), "value": v.get("value"), "type": v.get("type")} for v in variables[:20]]

def main():
    if len(sys.argv) != 3:
        raise SystemExit("usage: probe.py <lldb-dap> <fixture-binary>")
    adapter, program = sys.argv[1:]
    source = os.path.abspath(os.path.join(os.path.dirname(__file__), "fixture.c"))
    dap, evidence = DAP(adapter), {}
    try:
        init = dap.send("initialize", {"clientID": "lumencortex-agent-debugger-p0",
            "adapterID": "lldb", "pathFormat": "path", "linesStartAt1": True,
            "columnsStartAt1": True, "supportsVariableType": True, "supportsVariablePaging": True})
        evidence["initialize"] = {k: init.get(k) for k in (
            "supportsConfigurationDoneRequest", "supportsStepBack", "supportsEvaluateForHovers")}
        dap.send("launch", {"program": os.path.abspath(program),
            "cwd": os.path.dirname(os.path.abspath(program)), "args": [], "stopOnEntry": False,
            "console": "internalConsole", "disableASLR": True})
        dap.wait_event("initialized")
        bp = dap.send("setBreakpoints", {"source": {"name": os.path.basename(source), "path": source},
            "breakpoints": [{"line": 4}], "sourceModified": False})
        evidence["breakpoint"] = bp.get("breakpoints", [])
        dap.send("configurationDone")
        stopped = dap.wait_event("stopped")
        thread_id = stopped.get("threadId")
        evidence["stopped"] = {"reason": stopped.get("reason"), "threadId": thread_id}
        stack = dap.send("stackTrace", {"threadId": thread_id, "startFrame": 0, "levels": 8})
        frames = stack.get("stackFrames", [])
        evidence["stack"] = [{"name": f.get("name"), "line": f.get("line"),
            "source": (f.get("source") or {}).get("name"), "id": f.get("id")} for f in frames[:8]]
        if not frames:
            raise RuntimeError("adapter returned no stack frames")
        evidence["locals_at_breakpoint"] = read_locals(dap, frames[0]["id"])
        for step_number in (1, 2):
            dap.send("next", {"threadId": thread_id, "granularity": "line"})
            stepped = dap.wait_event("stopped")
            evidence[f"step_over_{step_number}"] = {"reason": stepped.get("reason"), "threadId": stepped.get("threadId")}
            stack_now = dap.send("stackTrace", {"threadId": thread_id, "startFrame": 0, "levels": 4})
            frames_now = stack_now.get("stackFrames", [])
            evidence[f"location_after_step_{step_number}"] = [{"name": f.get("name"), "line": f.get("line"),
                "source": (f.get("source") or {}).get("name")} for f in frames_now[:4]]
            evidence[f"locals_after_step_{step_number}"] = read_locals(dap, frames_now[0]["id"])
        dap.send("continue", {"threadId": thread_id, "singleThread": False})
        try:
            evidence["exit"] = {"exitCode": dap.wait_event("exited", 10).get("exitCode")}
        except TimeoutError:
            evidence["exit"] = {"note": "exited event not observed"}
        try:
            dap.wait_event("terminated", 3)
            evidence["terminated"] = True
        except TimeoutError:
            evidence["terminated"] = False
        print(json.dumps(evidence, indent=2))
    finally:
        dap.close()

if __name__ == "__main__":
    main()
