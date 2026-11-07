#!/usr/bin/env python3
"""LumenCortex persistent Python worker.

Speaks JSON-RPC 2.0 over two dedicated file descriptors (3: host -> worker,
4: worker -> host), one JSON object per line. stdout/stderr of user code never
touch the control channel: they are captured into bounded buffers.

The interpreter is reused for every cell of one session. Variables, functions and
imported modules survive a successful cell. An exception does not roll back side
effects. A cancelled cell keeps the kernel only when the interrupt was caught;
the host escalates to SIGKILL otherwise and starts a new generation.
"""

import ast
import base64
import builtins
import inspect
import io
import json
import mimetypes
import os
import sys
import time
import traceback

IN_FD = 3
OUT_FD = 4
MAX_FRAME = 4 * 1024 * 1024
MAX_REPR = 8 * 1024
MAX_TRACEBACK = 32 * 1024
PNG_MAGIC = b"\x89PNG\r\n\x1a\n"


class ProtocolError(Exception):
    pass


class ToolDenied(RuntimeError):
    """The host refused a tool call: not authorized, schema-invalid or budget-exhausted."""


class ToolFailed(RuntimeError):
    """The host reported the tool itself failed."""


class _Bounded(io.TextIOBase):
    """Keeps a bounded head and tail; total memory stays O(head+tail)."""

    def __init__(self, head=64 * 1024, tail=64 * 1024):
        self.head_limit = head
        self.tail_limit = tail
        self.head = []
        self.head_len = 0
        self.tail = []
        self.tail_len = 0
        self.total = 0

    # io.TextIOBase methods the stdlib probes for.
    def writable(self):
        return True

    def isatty(self):
        return False

    def readable(self):
        return False

    def fileno(self):
        raise io.UnsupportedOperation("fileno")

    def write(self, s):
        if not isinstance(s, str):
            s = str(s)
        length = len(s)
        self.total += length
        room = self.head_limit - self.head_len
        if room > 0:
            take = s[:room]
            self.head.append(take)
            self.head_len += len(take)
            s = s[len(take):]
        if s:
            self.tail.append(s)
            self.tail_len += len(s)
            while self.tail_len > self.tail_limit:
                first = self.tail[0]
                excess = self.tail_len - self.tail_limit
                if len(first) <= excess:
                    self.tail.pop(0)
                    self.tail_len -= len(first)
                else:
                    self.tail[0] = first[excess:]
                    self.tail_len -= excess
        return length

    @property
    def buffer(self):
        return _Buffer(self)

    def flush(self):
        return None

    def getvalue(self):
        kept = self.head_len + self.tail_len
        body = "".join(self.head) + "".join(self.tail)
        if self.total <= kept:
            return body
        marker = "\n... [truncated %d characters] ...\n" % (self.total - kept)
        return "".join(self.head) + marker + "".join(self.tail)

    @property
    def truncated(self):
        return self.total > self.head_len + self.tail_len


class _Buffer(io.RawIOBase):
    def __init__(self, sink):
        self.sink = sink

    def writable(self):
        return True

    def write(self, b):
        if isinstance(b, (bytes, bytearray)):
            self.sink.write(bytes(b).decode("utf-8", "replace"))
        else:
            self.sink.write(str(b))
        return len(b)


class Worker:
    def __init__(self):
        self.ctl = os.fdopen(IN_FD, "rb")
        self.g = self._fresh_globals()
        self.generation = 1
        self.config = {}
        self.tools = []
        self.tool_names = set()
        self.artifact_dir = None
        self.workspace = None
        self.limits = {}
        self._call_seq = 0
        self._seen_calls = set()
        self._artifacts = []
        self._out = None
        self._err = None
        self._stdout_saved = None
        self._stderr_saved = None

    # ---------------------------------------------------------------- plumbing
    def _fresh_globals(self):
        g = {
            "__name__": "__main__",
            "__doc__": None,
            "__builtins__": builtins,
            "__package__": None,
        }
        g["lc_tool"] = self.lc_tool
        g["lc_tools"] = self.lc_tools
        g["lc_artifact"] = self.lc_artifact
        g["display"] = self.display
        g["lc_display"] = self.display
        g["ToolDenied"] = ToolDenied
        g["ToolFailed"] = ToolFailed
        return g

    def _send(self, obj):
        data = json.dumps(obj, ensure_ascii=False, separators=(",", ":")).encode("utf-8") + b"\n"
        if len(data) > MAX_FRAME:
            raise ProtocolError("outgoing frame exceeds limit")
        os.write(OUT_FD, data)

    def _reply(self, req_id, result=None, error=None):
        msg = {"jsonrpc": "2.0", "id": req_id}
        if error is not None:
            msg["error"] = error
        else:
            msg["result"] = result
        self._send(msg)

    def _read_line(self):
        line = self.ctl.readline(MAX_FRAME + 1)
        if not line:
            raise EOFError()
        if len(line) > MAX_FRAME:
            raise ProtocolError("incoming frame exceeds limit")
        return line

    def _read_message(self):
        while True:
            line = self._read_line()
            text = line.strip()
            if not text:
                continue
            try:
                return json.loads(text)
            except ValueError as exc:
                raise ProtocolError("invalid JSON frame: %s" % exc)

    # ------------------------------------------------------------- tool bridge
    def lc_tools(self):
        return list(self.tools)

    def lc_tool(self, name, **arguments):
        if name not in self.tool_names:
            raise ToolDenied("tool %r is not authorized for this execution" % name)
        self._call_seq += 1
        call_id = "c%d" % self._call_seq
        if call_id in self._seen_calls:  # defensive: ids are never reused
            raise ProtocolError("duplicate callback id")
        self._seen_calls.add(call_id)
        self._send({
            "jsonrpc": "2.0",
            "id": call_id,
            "method": "tool/call",
            "params": {"callId": call_id, "name": name, "arguments": arguments},
        })
        while True:
            msg = self._read_message()
            if "method" in msg and "id" not in msg:
                if msg.get("method") == "interrupt":
                    raise KeyboardInterrupt()
                continue
            if msg.get("id") != call_id:
                # A stale or unexpected response: never treat it as ours.
                continue
            if "error" in msg:
                raise ToolFailed(str(msg["error"].get("message", "tool call failed")))
            result = msg.get("result") or {}
            content = result.get("content", "")
            if result.get("denied"):
                raise ToolDenied(content or "tool call denied")
            if not result.get("ok", False):
                raise ToolFailed(content or "tool call failed")
            return content

    # --------------------------------------------------------------- artifacts
    def _safe_name(self, name, default="artifact"):
        base = os.path.basename(str(name or "").replace("\\", "/")).strip()
        base = "".join(ch for ch in base if ch.isalnum() or ch in "._- ")
        base = base.replace("..", "_").strip(". ")
        return base or default

    def _mime_for(self, name, fallback="application/octet-stream"):
        guessed, _ = mimetypes.guess_type(name)
        return guessed or fallback

    def lc_artifact(self, data, name=None, mime=None):
        if self.artifact_dir is None:
            raise RuntimeError("artifact directory is not configured")
        if hasattr(data, "savefig"):  # a matplotlib figure
            buf = io.BytesIO()
            data.savefig(buf, format="png")
            payload, name, mime = buf.getvalue(), name or "figure.png", mime or "image/png"
        elif isinstance(data, (bytes, bytearray)):
            payload = bytes(data)
            name = name or "artifact.bin"
        elif isinstance(data, str) and os.path.isfile(data):
            with open(data, "rb") as fh:
                payload = fh.read(self._artifact_limit() + 1)
            name = name or os.path.basename(data)
        elif isinstance(data, str):
            payload = data.encode("utf-8")
            name = name or "artifact.txt"
        else:
            payload = repr(data).encode("utf-8")
            name = name or "artifact.txt"
        limit = self._artifact_limit()
        if len(payload) > limit:
            raise ValueError("artifact exceeds the %d byte limit" % limit)
        safe = self._safe_name(name)
        os.makedirs(self.artifact_dir, exist_ok=True)
        target = os.path.join(self.artifact_dir, safe)
        with open(target, "wb") as fh:
            fh.write(payload)
        record = {"name": safe, "mime": mime or self._mime_for(safe), "size": len(payload)}
        self._artifacts = [a for a in self._artifacts if a["name"] != safe]
        self._artifacts.append(record)
        return dict(record)

    def _artifact_limit(self):
        value = self.limits.get("artifactBytes") or 0
        return int(value) if value > 0 else 8 << 20

    def display(self, obj, mime=None):
        payload = _display_payload(obj)
        if payload is None:
            return None
        kind, text, size = payload
        self._send({"jsonrpc": "2.0", "method": "event", "params": {
            "type": "display", "data": {"mime": mime or kind, "encoding": "utf-8", "text": text, "size": size}}})
        return payload

    # --------------------------------------------------------------- execution
    def _flush_figures(self):
        displays = []
        plt = sys.modules.get("matplotlib.pyplot")
        if plt is None:
            return displays
        try:
            numbers = list(plt.get_fignums())
        except Exception:
            return displays
        for number in numbers:
            try:
                fig = plt.figure(number)
                buf = io.BytesIO()
                fig.savefig(buf, format="png")
                data = buf.getvalue()
                plt.close(fig)
                if data:
                    displays.append({"mime": "image/png", "encoding": "base64",
                                     "text": base64.b64encode(data).decode("ascii"),
                                     "size": len(data)})
            except Exception:
                try:
                    plt.close(number)
                except Exception:
                    pass
        return displays

    def _collect_artifacts(self):
        if self.artifact_dir and os.path.isdir(self.artifact_dir):
            for entry in sorted(os.listdir(self.artifact_dir)):
                path = os.path.join(self.artifact_dir, entry)
                if not os.path.isfile(path):
                    continue
                if any(a["name"] == entry for a in self._artifacts):
                    continue
                try:
                    size = os.path.getsize(path)
                except OSError:
                    continue
                self._artifacts.append({"name": entry, "mime": self._mime_for(entry), "size": size})
        return list(self._artifacts)

    def _execute(self, params):
        code = params.get("code") or ""
        # The host advertises the per-execution tool surface with each cell.
        tools = params.get("tools")
        if tools is not None:
            self.tools = tools
            self.tool_names = {t.get("name") for t in tools if isinstance(t, dict)}
        started = time.time()
        out, err = _Bounded(), _Bounded()
        self._out, self._err = out, err
        self._stdout_saved, self._stderr_saved = sys.stdout, sys.stderr
        sys.stdout, sys.stderr = out, err
        status, error, tb, exit_code = "completed", "", "", 0
        displays = []
        last_value = None
        try:
            tree = ast.parse(code, filename="<cell>", mode="exec")
            body = list(tree.body)
            last_expr = None
            if body and isinstance(body[-1], ast.Expr):
                last_expr = body.pop()
            if body:
                module = ast.Module(body=body, type_ignores=[])
                exec(compile(module, "<cell>", "exec"), self.g)
            if last_expr is not None:
                value = eval(compile(ast.Expression(last_expr.value), "<cell>", "eval"), self.g)
                if inspect.isawaitable(value):
                    import asyncio
                    value = asyncio.run(value)
                self.g["_"] = value
                last_value = value
        except KeyboardInterrupt:
            status, error = "cancelled", "KeyboardInterrupt"
        except SystemExit as exc:
            status, error, exit_code = "failed", "SystemExit(%r)" % (exc.code,), 0
        except BaseException as exc:  # noqa: BLE001 - the cell's own failure is a result
            status = "failed"
            error = "%s: %s" % (type(exc).__name__, exc)
            tb = traceback.format_exc()[:MAX_TRACEBACK]
        finally:
            sys.stdout, sys.stderr = self._stdout_saved, self._stderr_saved
        if last_value is not None:
            payload = _display_payload(last_value)
            if payload is not None:
                displays.append({"mime": payload[0], "encoding": "utf-8",
                                 "text": payload[1], "size": payload[2]})
        displays.extend(self._flush_figures())
        artifacts = self._collect_artifacts()
        self._artifacts = []
        return {
            "status": status,
            "stdout": out.getvalue(),
            "stderr": err.getvalue(),
            "error": error,
            "traceback": tb,
            "stateStatus": "ok",
            "exitCode": exit_code,
            "displays": displays,
            "artifacts": artifacts,
            "durationMs": int((time.time() - started) * 1000),
            "outputTruncated": out.truncated or err.truncated,
        }

    # ----------------------------------------------------------------- control
    def _initialize(self, params):
        self.config = params or {}
        self.workspace = self.config.get("workspace") or os.getcwd()
        self.artifact_dir = self.config.get("artifactDir")
        self.limits = self.config.get("limits") or {}
        self.tools = self.config.get("tools") or []
        self.tool_names = {t.get("name") for t in self.tools if isinstance(t, dict)}
        self.g["__file__"] = os.path.join(self.workspace, "<cell>")
        return {
            "spiVersion": self.config.get("spiVersion", "1.0"),
            "runtimeVersion": sys.version.split()[0],
            "language": "python",
            "pid": os.getpid(),
            "capabilities": ["persistent_execution", "modules", "file_io", "scientific",
                             "plotting", "tool_callback", "artifact", "display",
                             "interrupt", "subprocess", "notebook"],
        }

    def _reset(self, params):
        try:
            plt = sys.modules.get("matplotlib.pyplot")
            if plt is not None:
                plt.close("all")
        except Exception:
            pass
        self.g = self._fresh_globals()
        self._artifacts = []
        self._seen_calls = set()
        self.generation += 1
        return {"generation": self.generation, "pid": os.getpid()}

    def _health(self, params):
        return {"status": "ok", "generation": self.generation, "pid": os.getpid(),
                "workspace": self.workspace}

    def dispatch(self, msg):
        method = msg.get("method")
        req_id = msg.get("id")
        params = msg.get("params") or {}
        if req_id is None:
            # Notification: the host asks for a cooperative interrupt. The
            # authoritative mechanism is SIGINT, which Python raises here.
            return
        try:
            if method == "initialize":
                result = self._initialize(params)
            elif method == "execute":
                result = self._execute(params)
            elif method == "reset":
                result = self._reset(params)
            elif method == "health":
                result = self._health(params)
            elif method == "close":
                self._reply(req_id, {"closed": True})
                os._exit(0)
            else:
                self._reply(req_id, error={"code": -32601, "message": "unknown method %r" % method})
                return
            self._reply(req_id, result)
        except ProtocolError as exc:
            self._reply(req_id, error={"code": -32700, "message": str(exc)})
            raise
        except Exception as exc:  # noqa: BLE001 - report, never die silently
            self._reply(req_id, error={"code": -32603, "message": "%s: %s" % (type(exc).__name__, exc)})


def _display_payload(value):
    """Return (mime, text, size) for a rich value, or None when it is empty."""
    try:
        if value is None:
            return None
        if isinstance(value, (bytes, bytearray)) and bytes(value).startswith(PNG_MAGIC):
            data = bytes(value)
            return ("image/png", base64.b64encode(data).decode("ascii"), len(data))
        html = getattr(value, "_repr_html_", None)
        if callable(html):
            text = str(html())[:MAX_REPR]
            return ("text/html", text, len(text))
        text = repr(value)
        if len(text) > MAX_REPR:
            text = text[:MAX_REPR] + "... [repr truncated]"
        return ("text/plain", text, len(text))
    except Exception:
        return None


def main():
    worker = Worker()
    while True:
        try:
            msg = worker._read_message()
        except EOFError:
            return 0
        except KeyboardInterrupt:
            continue
        except ProtocolError as exc:
            try:
                worker._reply(None, error={"code": -32700, "message": str(exc)})
            except Exception:
                pass
            return 2
        try:
            worker.dispatch(msg)
        except KeyboardInterrupt:
            continue
        except EOFError:
            return 0


if __name__ == "__main__":
    sys.exit(main())
