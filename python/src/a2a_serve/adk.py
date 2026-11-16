# Copyright 2026 The A2A Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""An ADK agent backed by an ACP coding agent, such as `opencode acp`.

`AcpAgent` runs the coding agent through the a2aacp library. Each ADK
session gets its own ACP session and, with `start`, its own agent process,
such as one in a `Sandbox`. The agent's reply streams into the ADK session
as the agent's events.

`tools` gives the coding agent ADK tools over MCP. The library has no HTTP
server: `mcp_url` names a host that the sandbox's interceptor answers by
calling `handle_mcp`, with the session of the sandbox. A tool runs in the
ADK invocation in progress in that session, and its call and response are
events of the invocation, so its state changes are saved as those of any
other tool.
"""

from __future__ import annotations

import asyncio
import json
import threading
from collections import OrderedDict
from collections.abc import AsyncGenerator, Callable
from typing import Any

from google.adk.agents.base_agent import BaseAgent
from google.adk.agents.invocation_context import InvocationContext
from google.adk.events.event import Event
from google.adk.events.event_actions import EventActions
from google.adk.tools.base_tool import BaseTool
from google.adk.tools.tool_context import ToolContext
from google.genai import types
from pydantic import ConfigDict, Field, PrivateAttr

from .acp import Executor

MCP_SERVER_NAME = "adk"
"""The name under which the coding agent sees the tools."""

MAX_SESSIONS = 10000
"""Bounds the sessions whose delivered history the agent remembers."""

TOOL_TIMEOUT = 240.0
"""Bounds a tool call of the coding agent, in seconds. It stays below the
time the sandbox library waits for an answer to a request (5 minutes), so
that the agent never sees a failure for a tool that went on to run."""


class AcpAgent(BaseAgent):
    """Runs an ACP coding agent as an ADK agent."""

    model_config = ConfigDict(arbitrary_types_allowed=True)

    argv: list[str] = Field(default_factory=list)
    """The agent's command, when it runs on this host."""
    start: Callable[[str], int] | None = None
    """Starts the agent of an ADK session, by its ID, and returns the file
    descriptor of its stdio. Set it or argv."""
    session_cwd: str = ""
    """The working directory of the ACP sessions, as the agent sees it."""
    allow_tools: list[str] = Field(default_factory=list)
    """The ACP tool kinds the agent may run without asking."""
    deny_tools: list[str] = Field(default_factory=list)
    """The ACP tool kinds the agent may never run."""
    tools: list[BaseTool] = Field(default_factory=list)
    """ADK tools the agent may call over MCP; see the module documentation."""
    mcp_url: str = "http://adk-tools.internal/mcp"
    """The URL of the tools, which an interceptor answers with handle_mcp."""
    idle_timeout: float = 600
    """Stops the agent of an idle session after so many seconds."""
    inactivity_timeout: float = 900
    """Fails a turn whose agent is silent so long."""

    _executor: Executor | None = PrivateAttr(default=None)
    _lock: threading.Lock = PrivateAttr(default_factory=threading.Lock)
    _runs: dict[str, "_Run"] = PrivateAttr(default_factory=dict)
    _pending: dict[str, str] = PrivateAttr(default_factory=dict)
    # The last session event sent to the agent, by session.
    _delivered: OrderedDict[str, str] = PrivateAttr(default_factory=OrderedDict)

    def model_post_init(self, context: Any) -> None:
        super().model_post_init(context)
        if bool(self.argv) == (self.start is not None):
            raise ValueError("AcpAgent needs either argv or start")
        names = [t.name for t in self.tools]
        if len(names) != len(set(names)):
            raise ValueError(f"AcpAgent tools have duplicate names: {sorted(names)}")

    def _unseen(self, ctx: InvocationContext) -> str:
        """Returns the messages of the session since the agent's last turn.

        The agent keeps its own history, so it gets only what it wasn't
        sent before: what others said meanwhile, such as other agents of a
        workflow, and the user's message. Messages of others are marked
        with their author.
        """
        events = [e for e in ctx.session.events if not e.partial]
        with self._lock:
            delivered = self._delivered.get(ctx.session.id, "")
        last = max((i for i, e in enumerate(events) if e.author == self.name or e.id == delivered), default=-1)
        if events:
            with self._lock:
                self._delivered[ctx.session.id] = events[-1].id
                self._delivered.move_to_end(ctx.session.id)
                while len(self._delivered) > MAX_SESSIONS:
                    self._delivered.popitem(last=False)
        lines = []
        for e in events[last + 1 :]:
            text = _text(e.content)
            if not text:
                continue
            lines.append(text if e.author == "user" else f"[{e.author}] {text}")
        user = _text(ctx.user_content)
        if user and (not lines or lines[-1] != user):
            lines.append(user)
        return "\n\n".join(lines)

    def executor(self) -> Executor:
        """Returns the executor, starting it on the first call."""
        with self._lock:
            if self._executor is None:
                mcp = []
                if self.tools:
                    mcp = [
                        {
                            "type": "http",
                            "name": MCP_SERVER_NAME,
                            "url": self.mcp_url,
                            "headers": [],
                        }
                    ]
                self._executor = Executor(
                    argv=self.argv or None,
                    start=self.start,
                    per_context=True,
                    idle_timeout=self.idle_timeout,
                    session_cwd=self.session_cwd,
                    mcp_servers=mcp,
                    allow=self.allow_tools,
                    deny=self.deny_tools,
                    inactivity_timeout=self.inactivity_timeout,
                )
            return self._executor

    def busy_sessions(self) -> set[str]:
        """Returns the IDs of the ADK sessions with an invocation in progress."""
        with self._lock:
            return set(self._runs)

    def close(self) -> None:
        """Stops the agents; runs in progress fail."""
        with self._lock:
            if self._executor is not None:
                self._executor.close()
                self._executor = None

    async def _run_async_impl(
        self, ctx: InvocationContext
    ) -> AsyncGenerator[Event, None]:
        key = ctx.session.id
        task_id = self._pending.pop(key, "")
        # An answer to a question goes to the agent as typed; other messages
        # carry what the agent hasn't seen since its last turn.
        text = _text(ctx.user_content) if task_id else self._unseen(ctx)
        run = _Run(self, ctx)
        with self._lock:
            self._runs[key] = run
        executor = await asyncio.to_thread(self.executor)
        pump = threading.Thread(target=run.pump, args=(executor, text, key, task_id), name="acp-stream", daemon=True)
        pump.start()
        reply: list[str] = []
        try:
            while True:
                kind, item, done = await run.queue.get()
                if kind == "end":
                    if item is not None:
                        yield self._event(ctx, error=str(item))
                    return
                if kind == "tool":
                    yield item
                    done.set_result(None)
                    continue
                for ev in self._convert(ctx, key, item, reply):
                    yield ev
        finally:
            with self._lock:
                if self._runs.get(key) is run:
                    del self._runs[key]
            run.fail_waiting()
            run.abandon(executor)

    def _convert(
        self, ctx: InvocationContext, key: str, ev: dict, reply: list[str]
    ) -> list[Event]:
        """Turns an A2A stream event into ADK events."""
        if "artifactUpdate" in ev:
            chunk = "".join(
                p.get("text", "")
                for p in ev["artifactUpdate"]["artifact"].get("parts", [])
            )
            if not chunk:
                return []
            reply.append(chunk)
            return [self._event(ctx, text=chunk, partial=True)]
        status = None
        task_id = ""
        if "statusUpdate" in ev:
            status, task_id = ev["statusUpdate"]["status"], ev["statusUpdate"]["taskId"]
        elif "task" in ev:
            status, task_id = ev["task"]["status"], ev["task"]["id"]
        if status is None:
            return []
        state = status.get("state", "")
        parts = (status.get("message") or {}).get("parts", [])
        message = "".join(p.get("text", "") for p in parts)
        if state == "TASK_STATE_COMPLETED":
            return [self._event(ctx, text="".join(reply))]
        if state == "TASK_STATE_INPUT_REQUIRED":
            self._pending[key] = task_id
            text = "".join(reply)
            text = f"{text}\n\n{message}" if text else message
            return [
                self._event(ctx, text=text, metadata={"acp_input_required": task_id})
            ]
        if state in ("TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED"):
            return [self._event(ctx, error=message or state)]
        # A question carries its tool call too, so tool updates come after it.
        for p in parts:
            data = p.get("data")
            if isinstance(data, dict) and data.get("toolCall"):
                return [self._event(ctx, metadata={"acp_tool_call": data["toolCall"]})]
        return []

    def _event(
        self,
        ctx: InvocationContext,
        *,
        text: str = "",
        partial: bool = False,
        error: str = "",
        metadata: dict | None = None,
    ) -> Event:
        ev = Event(invocation_id=ctx.invocation_id, author=self.name, branch=ctx.branch)
        if text:
            ev.content = types.Content(role="model", parts=[types.Part(text=text)])
        if partial:
            ev.partial = True
        if error:
            ev.error_code, ev.error_message = "ACP_TASK_FAILED", error
        if metadata:
            ev.custom_metadata = metadata
        return ev

    def handle_mcp(
        self, session_id: str, method: str, body: bytes
    ) -> tuple[int, dict[str, str], bytes]:
        """Answers an MCP request of the agent of the ADK session.

        It serves the Streamable HTTP transport without streams: every
        request gets a JSON response. Call it from a worker thread, such as
        the one an interceptor runs on.
        """
        if method != "POST":
            return 405, {"Allow": "POST"}, b""
        try:
            req = json.loads(body)
        except json.JSONDecodeError as e:
            return 400, {}, f"the body isn't JSON: {e}".encode()
        batch = isinstance(req, list)
        responses = [
            r
            for r in (self._mcp_call(session_id, m) for m in (req if batch else [req]))
            if r is not None
        ]
        if not responses:
            return 202, {}, b""
        out = responses if batch else responses[0]
        return 200, {"Content-Type": "application/json"}, json.dumps(out).encode()

    def _mcp_call(self, session_id: str, msg: dict) -> dict | None:
        if "id" not in msg:
            return None
        method, params = msg.get("method"), msg.get("params") or {}
        try:
            if method == "initialize":
                result = {
                    "protocolVersion": params.get("protocolVersion", "2025-06-18"),
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": self.name, "version": "1"},
                }
            elif method == "ping":
                result = {}
            elif method == "tools/list":
                result = {"tools": [_mcp_tool(t) for t in self.tools]}
            elif method == "tools/call":
                result = self._call_tool(
                    session_id, params.get("name", ""), params.get("arguments") or {}
                )
            else:
                return {
                    "jsonrpc": "2.0",
                    "id": msg["id"],
                    "error": {"code": -32601, "message": f"unknown method {method}"},
                }
        except Exception as e:  # pylint: disable=broad-exception-caught
            return {
                "jsonrpc": "2.0",
                "id": msg["id"],
                "error": {"code": -32603, "message": str(e)},
            }
        return {"jsonrpc": "2.0", "id": msg["id"], "result": result}

    def _call_tool(self, session_id: str, name: str, args: dict) -> dict:
        tool = next((t for t in self.tools if t.name == name), None)
        if tool is None:
            return _tool_error(f"no tool {name}")
        with self._lock:
            run = self._runs.get(session_id)
        if run is None:
            return _tool_error("no ADK invocation of this session is in progress")
        future = asyncio.run_coroutine_threadsafe(run.call_tool(tool, args), run.loop)
        try:
            result = future.result(timeout=TOOL_TIMEOUT)
        except Exception as e:  # pylint: disable=broad-exception-caught
            future.cancel()
            return _tool_error(f"{type(e).__name__}: {e}")
        text = json.dumps(result, default=str)
        out: dict[str, Any] = {"content": [{"type": "text", "text": text}]}
        if isinstance(result, dict):
            out["structuredContent"] = result
        return out


_PAUSED_OR_DONE = {"TASK_STATE_COMPLETED", "TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED", "TASK_STATE_INPUT_REQUIRED"}


def _cancel_quietly(executor: Executor, task_id: str) -> None:
    try:
        executor.cancel(task_id)
    except Exception:  # pylint: disable=broad-exception-caught
        # The task ended meanwhile.
        pass


class _Run:
    """An invocation of the agent, which the tools of its session run in."""

    def __init__(self, agent: AcpAgent, ctx: InvocationContext):
        self.agent = agent
        self.ctx = ctx
        self.loop = asyncio.get_running_loop()
        self.queue: asyncio.Queue = asyncio.Queue()
        self._waiting: list[asyncio.Future] = []
        # The task and whether it stopped working are written by the pump
        # thread and read when the invocation ends.
        self._mu = threading.Lock()
        self._task_id = ""
        self._finished = False
        self._abandoned = threading.Event()

    def pump(self, executor: Executor, text: str, key: str, task_id: str) -> None:
        """Reads the events of the message on a thread of its own.

        Once the invocation is abandoned, it reads on until it learns the
        task, which it then cancels.
        """
        err = None
        try:
            for ev in executor.stream(text, context_id=key, task_id=task_id, stop=self._stop):
                if self._note(ev) and self._abandoned.is_set():
                    self._cancel(executor)
                    return
                if not self._abandoned.is_set():
                    self._put(("a2a", ev, None))
        except Exception as e:  # pylint: disable=broad-exception-caught
            err = e
        self._put(("end", err, None))

    def _note(self, ev: dict) -> bool:
        """Records the task of ev, and reports whether it is known."""
        status, task_id = None, ""
        if "statusUpdate" in ev:
            status, task_id = ev["statusUpdate"]["status"], ev["statusUpdate"]["taskId"]
        elif "task" in ev:
            status, task_id = ev["task"]["status"], ev["task"]["id"]
        with self._mu:
            if status is not None:
                self._task_id = task_id
                self._finished = status.get("state", "") in _PAUSED_OR_DONE
            return bool(self._task_id)

    def _stop(self) -> bool:
        """Tells the stream to stop waiting: abandoned, with the task known."""
        with self._mu:
            return self._abandoned.is_set() and bool(self._task_id)

    def _cancel(self, executor: Executor) -> None:
        with self._mu:
            task_id, finished = self._task_id, self._finished
        if task_id and not finished:
            _cancel_quietly(executor, task_id)

    def _put(self, item: tuple) -> None:
        if self._abandoned.is_set():
            return
        try:
            self.loop.call_soon_threadsafe(self.queue.put_nowait, item)
        except RuntimeError:
            # The loop of the invocation closed.
            pass

    def abandon(self, executor: Executor) -> None:
        """Ends the invocation, cancelling the task when it still works.

        A task that isn't known yet is cancelled by the pump once its first
        event comes.
        """
        self._abandoned.set()
        threading.Thread(target=self._cancel, args=(executor,), daemon=True).start()

    async def emit(self, ev: Event) -> None:
        """Yields ev from the invocation, and waits until it was yielded."""
        done = self.loop.create_future()
        self._waiting.append(done)
        await self.queue.put(("tool", ev, done))
        await done

    def fail_waiting(self) -> None:
        for f in self._waiting:
            if not f.done():
                f.set_exception(RuntimeError("the ADK invocation ended"))

    async def call_tool(self, tool: BaseTool, args: dict) -> Any:
        call_id = "acp-" + Event.new_id()
        call = self.agent._event(self.ctx)
        call.content = types.Content(
            role="model",
            parts=[
                types.Part(
                    function_call=types.FunctionCall(
                        id=call_id, name=tool.name, args=args
                    )
                )
            ],
        )
        await self.emit(call)
        actions = EventActions()
        tool_ctx = ToolContext(
            self.ctx, function_call_id=call_id, event_actions=actions
        )
        try:
            result = await tool.run_async(args=args, tool_context=tool_ctx)
        except Exception as e:  # pylint: disable=broad-exception-caught
            result = {"error": f"{type(e).__name__}: {e}"}
        response = result if isinstance(result, dict) else {"result": result}
        resp = self.agent._event(self.ctx)
        resp.content = types.Content(
            role="user",
            parts=[
                types.Part(
                    function_response=types.FunctionResponse(
                        id=call_id, name=tool.name, response=response
                    )
                )
            ],
        )
        resp.actions = actions
        await self.emit(resp)
        return result


def _text(content: types.Content | None) -> str:
    if content is None or not content.parts:
        return ""
    return "".join(p.text or "" for p in content.parts if not p.thought)


def _mcp_tool(tool: BaseTool) -> dict:
    decl = tool._get_declaration()  # pylint: disable=protected-access
    schema: Any = {"type": "object"}
    if decl is not None and decl.parameters_json_schema:
        schema = decl.parameters_json_schema
    elif decl is not None and decl.parameters is not None:
        schema = _lower_types(
            decl.parameters.model_dump(exclude_none=True, mode="json")
        )
    return {"name": tool.name, "description": tool.description, "inputSchema": schema}


def _lower_types(v: Any) -> Any:
    if isinstance(v, dict):
        return {
            k: (c.lower() if k == "type" and isinstance(c, str) else _lower_types(c))
            for k, c in v.items()
        }
    if isinstance(v, list):
        return [_lower_types(c) for c in v]
    return v


def _tool_error(message: str) -> dict:
    return {"content": [{"type": "text", "text": message}], "isError": True}
