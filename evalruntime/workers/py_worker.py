#!/usr/bin/env python3
"""LumenCortex persistent Python worker.

Speaks JSON-RPC 2.0 over two dedicated file descriptors (3: host -> worker,
4: worker -> host), one JSON object per line. stdout/stderr of user code never
touch the control channel: they are captured into bounded buffers.

The interpreter is reused for every cell of one session. Variables, functions and
imported modules survive a successful cell. An exception does not roll back side
effects. A cancelled cell keeps the kernel only when the interrupt was caught;
the host escalates to SIGKILL otherwise and starts a new generation.

SIGINT is the interrupt. It raises KeyboardInterrupt only while the cell runs:
its own code and every piece of user code that renders its result (the last
value's __repr__ or _repr_html_, callbacks of the figures it drew). A signal
that arrives while the worker reads a request, collects artifacts or writes a
reply belongs to an execution that is already over and is dropped, so it can
never cost the kernel its reply. A cell can swallow the
exception (a finalizer that runs when the signal lands ignores it, a cell may
catch it), so the host repeats the signal while the cell keeps running; a
repeat closer than INTERRUPT_SPACING to the last delivered one is dropped, which
keeps a second exception out of the unwinding of the first. While the cell is
inside a control-channel exchange (a tool callback, a display event) the signal
is held and raised when the exchange is complete, so a frame is never half read
or half written. A cell - or the __repr__ of its result - may block SIGINT or
replace its handler; when the last user code of the cell has returned, a SIGINT
still pending for it is discarded and the worker's own handler and signal mask
come back, and every cell starts from that state again, so no signal and no
signal state crosses into the next cell.
"""

import ast
import base64
import builtins
import inspect
import io
import json
import mimetypes
import os
import signal
import sys
import time
import traceback

IN_FD = 3
OUT_FD = 4
MAX_FRAME = 4 * 1024 * 1024
MAX_REPR = 8 * 1024
MAX_TRACEBACK = 32 * 1024
# INTERRUPT_SPACING is the least time between two delivered interrupts of one
# cell; the host repeats an unacknowledged interrupt at a longer interval.
INTERRUPT_SPACING = 0.2
# MAX_INLINE_DISPLAY bounds one inline display payload. Anything larger is
# spilled to the artifact directory, so a big figure never breaks the frame.
MAX_INLINE_DISPLAY = 256 * 1024
# MAX_CELL_ARTIFACTS mirrors the host's per-cell adoption bound: collecting more
# would only make the host refuse the cell.
MAX_CELL_ARTIFACTS = 256
PNG_MAGIC = b"\x89PNG\r\n\x1a\n"


def _private_mime_types():
    """A MIME table built only from the stdlib's builtin map.

    The module-level lazy init reads the host's knownfiles (/etc/mime.types and
    friends), which the tightened sandbox denies. A private MimeTypes instance
    with `inited` preset never touches the filesystem, and installing it as the
    module database keeps mimetypes.guess_type from triggering that init.
    """
    mimetypes.inited = True
    db = mimetypes.MimeTypes(filenames=())
    mimetypes._db = db
    return db


MIME_TYPES = _private_mime_types()


def _mime_for_name(name, fallback="application/octet-stream"):
    guessed, _ = MIME_TYPES.guess_type(str(name or ""))
    return guessed or fallback


def _extension_for(mime):
    return {
        "image/png": ".png",
        "image/jpeg": ".jpg",
        "image/svg+xml": ".svg",
        "text/html": ".html",
        "application/json": ".json",
    }.get(mime or "", ".bin")


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
        self._request_id = None
        self._artifacts = []
        self._out = None
        self._err = None
        self._stdout_saved = None
        self._stderr_saved = None
        # One event loop for the kernel's lifetime: an asyncio task, client or
        # connection created in one cell is usable in the next.
        self._loop = None
        # Interrupt state of the running cell, see the module docstring.
        self._in_cell = False
        self._shield = 0
        self._interrupt_pending = False
        self._last_interrupt = None
        signal.signal(signal.SIGINT, self._on_sigint)
        # The signal mask every cell starts with: whatever the worker
        # inherited, without SIGINT, which is the interrupt.
        self._base_mask = signal.pthread_sigmask(signal.SIG_UNBLOCK, {signal.SIGINT}) - {signal.SIGINT}

    # --------------------------------------------------------------- interrupt
    def _may_deliver(self):
        last = self._last_interrupt
        return last is None or time.monotonic() - last >= INTERRUPT_SPACING

    def _deliver(self):
        self._interrupt_pending = False
        self._last_interrupt = time.monotonic()
        raise KeyboardInterrupt()

    def _on_sigint(self, signum, frame):
        if not self._in_cell or not self._may_deliver():
            return  # stale (the cell is over) or too close to the last one
        if self._shield:
            self._interrupt_pending = True
            return
        self._deliver()

    def _raise_pending(self):
        if (self._interrupt_pending and self._in_cell and not self._shield
                and self._may_deliver()):
            self._deliver()

    def _deliver_interrupt(self):
        """Raise the cell's interrupt from a control-channel notification."""
        if self._may_deliver():
            self._deliver()

    def _restore_signal_state(self):
        """End a cell's ownership of SIGINT.

        A cell can block SIGINT or replace its handler. A SIGINT sent for that
        cell then stays pending, and would interrupt whichever later cell
        unblocks it. Setting the disposition to SIG_IGN discards a pending
        SIGINT, blocked or not (POSIX), so the signal of a finished cell is
        gone; then the worker's handler and signal mask come back, and every
        cell starts with the same signal state.
        """
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        signal.signal(signal.SIGINT, self._on_sigint)
        signal.pthread_sigmask(signal.SIG_SETMASK, self._base_mask)

    def _eval_compiled(self, compiled):
        """Run one compiled cell fragment and return its value, if any.

        eval() is used for both modes: an exec-mode code object returns None,
        an eval-mode one returns the expression's value, and a coroutine code
        object (module-level await) returns a coroutine that must be driven.
        exec() would silently drop the coroutine and the expression value.
        """
        value = eval(compiled, self.g)
        return self._run_awaitable(value)

    def _run_awaitable(self, value):
        """Drive an awaitable on the kernel's one event loop.

        The loop lives as long as the kernel, so asyncio tasks, clients and
        connections created in one cell are usable in the next. asyncio.run()
        would create and close a fresh loop for every cell and break that.
        """
        if not inspect.isawaitable(value):
            return value
        import asyncio
        loop = self._loop
        if loop is None or loop.is_closed():
            loop = asyncio.new_event_loop()
            asyncio.set_event_loop(loop)
            self._loop = loop
        return loop.run_until_complete(value)

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
        # A signal can cut a pipe write short: finish the frame.
        view = memoryview(data)
        while view:
            view = view[os.write(OUT_FD, view):]

    def _send_shielded(self, obj):
        self._shield += 1
        try:
            self._send(obj)
        finally:
            self._shield -= 1
            self._raise_pending()

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
        # The exchange is shielded: a SIGINT during it is held until the frame
        # is complete. The host's interrupt notification ends the wait.
        self._shield += 1
        try:
            self._send({
                "jsonrpc": "2.0",
                "id": call_id,
                "method": "tool/call",
                "params": {"callId": call_id, "name": name, "arguments": arguments,
                           "requestId": self._request_id},
            })
            while True:
                msg = self._read_message()
                if "method" in msg and "id" not in msg:
                    if msg.get("method") == "interrupt":
                        target = (msg.get("params") or {}).get("requestId")
                        if not target or target == self._request_id:
                            self._deliver_interrupt()
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
        finally:
            self._shield -= 1
            self._raise_pending()

    # --------------------------------------------------------------- artifacts
    def _safe_name(self, name, default="artifact"):
        base = os.path.basename(str(name or "").replace("\\", "/")).strip()
        base = "".join(ch for ch in base if ch.isalnum() or ch in "._- ")
        base = base.replace("..", "_").strip(". ")
        return base or default

    def _mime_for(self, name, fallback="application/octet-stream"):
        return _mime_for_name(name, fallback)

    def lc_artifact(self, data, name=None, mime=None):
        if self.artifact_dir is None:
            raise RuntimeError("artifact directory is not configured")
        if len(self._artifacts) >= MAX_CELL_ARTIFACTS:
            raise ValueError("cell already declared %d artifacts; the limit is %d"
                             % (len(self._artifacts), MAX_CELL_ARTIFACTS))
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
        items = []
        self._add_display(items, mime or kind, text, size)
        for item in items:
            self._send_shielded({"jsonrpc": "2.0", "method": "event", "params": {
                "type": "display", "data": item}})
        return None

    def _add_display(self, displays, mime, text, size):
        """Append one display, keeping the control frame bounded.

        A payload that would not fit inline is written to the artifact
        directory and declared as an artifact instead: the kernel stays alive
        and the bytes stay reachable through Manager.Artifact.
        """
        encoding = "base64" if mime == "image/png" else "utf-8"
        if text and len(text) > MAX_INLINE_DISPLAY:
            data = base64.b64decode(text, validate=True) if encoding == "base64" else text.encode("utf-8")
            if data is not None and self.artifact_dir and len(self._artifacts) < MAX_CELL_ARTIFACTS:
                name = self._safe_name("display-%d%s" % (len(self._artifacts) + 1, _extension_for(mime)))
                try:
                    os.makedirs(self.artifact_dir, exist_ok=True)
                    with open(os.path.join(self.artifact_dir, name), "wb") as fh:
                        fh.write(data)
                    self._artifacts.append({"name": name, "mime": mime, "size": len(data)})
                    return
                except OSError:
                    pass
            raise ValueError("display exceeds inline limit and could not be stored as an artifact")
        displays.append({"mime": mime, "encoding": encoding, "text": text, "size": size})

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
        # One pass with a set of known names: the previous implementation
        # scanned self._artifacts for every directory entry, which is quadratic
        # and lets a flood of files exhaust the worker's CPU.
        if self.artifact_dir and os.path.isdir(self.artifact_dir):
            known = {a["name"] for a in self._artifacts}
            try:
                entries = sorted(os.listdir(self.artifact_dir))
            except OSError:
                entries = []
            for entry in entries:
                if len(self._artifacts) >= MAX_CELL_ARTIFACTS:
                    break
                if entry in known:
                    continue
                path = os.path.join(self.artifact_dir, entry)
                try:
                    if not os.path.isfile(path):
                        continue
                    size = os.path.getsize(path)
                except OSError:
                    continue
                known.add(entry)
                self._artifacts.append({"name": entry, "mime": self._mime_for(entry), "size": size})
        return list(self._artifacts)

    def _render(self, displays, last_value):
        """Render the cell's rich output. This runs user code - the last
        value's __repr__ or _repr_html_, matplotlib callbacks of the cell's
        figures - so it belongs to the cell: it runs while the cell owns
        SIGINT, before the cell's signal state is discarded."""
        if last_value is not None:
            payload = _display_payload(last_value)
            if payload is not None:
                self._add_display(displays, payload[0], payload[1], payload[2])
        for item in self._flush_figures():
            self._add_display(displays, item["mime"], item["text"], item["size"])

    def _execute(self, params):
        code = params.get("code") or ""
        self._request_id = params.get("requestId") or None
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
        self._interrupt_pending = False
        self._last_interrupt = None
        # Every cell starts from the worker's own signal state: whatever ran
        # between cells (a finalizer of an earlier cell's object) cannot hand a
        # blocked mask, a replaced handler or a pending SIGINT to this cell.
        self._restore_signal_state()
        try:
            try:
                # From here a SIGINT belongs to this cell. The host signals
                # only after it has read the "started" event.
                self._in_cell = True
                self._send_shielded({"jsonrpc": "2.0", "method": "event", "params": {
                    "type": "started", "data": {"requestId": self._request_id}}})
                last_value = None
                try:
                    await_flags = ast.PyCF_ALLOW_TOP_LEVEL_AWAIT
                    try:
                        tree = ast.parse(code, filename="<cell>", mode="exec")
                    except SyntaxError:
                        # An older parser rejects module-level await outright.
                        # Compile the whole cell with the flag; the last
                        # expression's display is unavailable in this path.
                        whole = compile(code, "<cell>", "exec", await_flags, dont_inherit=True)
                        value = self._eval_compiled(whole)
                        self.g["_"] = value
                        last_value = value
                    else:
                        body = list(tree.body)
                        last_expr = None
                        if body and isinstance(body[-1], ast.Expr):
                            last_expr = body.pop()
                        if body:
                            module = ast.Module(body=body, type_ignores=[])
                            # A wait in the body (x = await f()) makes the module
                            # a coroutine; compiling it with the flag returns
                            # CO_COROUTINE, so it must be driven, not merely
                            # exec'd.
                            compiled = compile(module, "<cell>", "exec", await_flags,
                                               dont_inherit=True)
                            value = self._eval_compiled(compiled)
                            if value is not None:
                                self.g["_"] = value
                        if last_expr is not None:
                            compiled = compile(ast.Expression(last_expr.value), "<cell>",
                                               "eval", await_flags, dont_inherit=True)
                            value = self._eval_compiled(compiled)
                            self.g["_"] = value
                            last_value = value
                finally:
                    # Rendering is still the cell, whether its code finished
                    # or not: figures the cell drew are flushed either way.
                    # An interrupt landing here cancels the cell.
                    self._render(displays, last_value)
            except KeyboardInterrupt:
                raise
            except SystemExit as exc:
                code_value = exc.code
                if code_value is None:
                    code_value = 0
                if not isinstance(code_value, int):
                    error = str(code_value)
                    code_value = 1
                status = "failed"
                exit_code = code_value
                if not error:
                    error = "SystemExit(%r)" % (exc.code,)
            except BaseException as exc:  # formatting may execute user code
                status = "failed"
                error = "%s: %s" % (type(exc).__name__, exc)
                tb = traceback.format_exc()[:MAX_TRACEBACK]
        except KeyboardInterrupt:
            status, error = "cancelled", "KeyboardInterrupt"
        finally:
            # Rendering and exception formatting still belong to this cell.
            # Discard its signal state only after all of them have finished.
            self._in_cell = False
            sys.stdout, sys.stderr = self._stdout_saved, self._stderr_saved
            self._restore_signal_state()
        # Collecting artifacts only lists the artifact directory: no user code.
        try:
            artifacts = self._collect_artifacts()
        except BaseException as exc:  # noqa: BLE001 - never die without a reply
            status = "failed"
            error = "%s: %s" % (type(exc).__name__, exc)
            if not tb:
                tb = traceback.format_exc()[:MAX_TRACEBACK]
            artifacts = list(self._artifacts)
        finally:
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
                             "interrupt", "subprocess", "notebook", "inspect"],
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

    _INSPECT_HIDDEN = frozenset({
        "__name__", "__doc__", "__builtins__", "__package__", "__file__",
        "lc_tool", "lc_tools", "lc_artifact", "display", "lc_display",
        "ToolDenied", "ToolFailed",
    })

    def _inspect(self, params):
        """Names and types of the kernel's live user state.

        Values are never rendered: repr() would run user code (a side effect, a
        block, an exception) during what is meant to be an inspection.
        """
        limit = params.get("limit") or 100
        try:
            limit = int(limit)
        except (TypeError, ValueError):
            limit = 100
        if limit <= 0 or limit > 1000:
            limit = 100
        wanted = params.get("names") or []
        keep = {name for name in wanted if isinstance(name, str)} if isinstance(wanted, list) else set()
        names = [name for name in self.g
                 if isinstance(name, str) and name not in self._INSPECT_HIDDEN
                 and not name.startswith("__")]
        if keep:
            names = [name for name in names if name in keep]
        names.sort()
        entries = []
        for name in names[:limit]:
            try:
                kind = type(self.g[name]).__name__
            except Exception:  # noqa: BLE001 - inspection never fails a kernel
                kind = "unknown"
            entries.append({"name": name, "type": kind})
        return {"generation": self.generation, "pid": os.getpid(),
                "entries": entries, "total": len(names),
                "truncated": len(names) > limit}

    def _health(self, params):
        return {"status": "ok", "generation": self.generation, "pid": os.getpid(),
                "workspace": self.workspace}

    def dispatch(self, msg):
        method = msg.get("method")
        req_id = msg.get("id")
        params = msg.get("params") or {}
        if req_id is None:
            # Notification: an interrupt for a cell that is no longer running
            # (a running cell reads it inside lc_tool). Nothing to do.
            return
        try:
            if method == "initialize":
                result = self._initialize(params)
            elif method == "execute":
                result = self._execute(params)
            elif method == "reset":
                result = self._reset(params)
            elif method == "inspect":
                result = self._inspect(params)
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
