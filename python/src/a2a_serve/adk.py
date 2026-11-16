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
session gets its own ACP session and its own agent process: a local
command with `argv`, one that `start` starts, or one in a Docker sandbox of
its own with `sandbox`, whose workspace lives in memory. The agent's reply
streams into the ADK session as the agent's events.

`model` makes an ADK model the coding agent's model. The agent sends its
model calls, in the OpenAI chat completions format, to `model_url`;
AcpAgent turns each into an LlmRequest and runs it with the model in the
invocation in progress, with `before_model_callback` and
`after_model_callback` as an LlmAgent runs them, without plugins and
on_model_error callbacks. A model error reaches the agent as an HTTP error
it can retry, such as a rate limit, and a callback's refusal, an
LlmResponse with an error code of its own, as 403, which ends the agent's
turn. Calls outside an invocation, after a turn ended, get 409. A streamed
answer arrives in one piece, once the model is done.

`tools` gives the coding agent ADK tools over MCP at `mcp_url`. A tool runs
in the invocation in progress in its session, and its call and response
are events of the invocation, so its state changes are saved as those of
any other tool.

Nothing listens on the host for either: the sandbox's boundary hands the
requests to the hosts of `model_url` and `mcp_url` to `handle_model` and
`handle_mcp`, and `SandboxConfig.egress` decides the rest. With `start`,
an interceptor of the caller does the same.
"""

from __future__ import annotations

import asyncio
import inspect
import json
import logging
import threading
import time
import urllib.parse
from collections import OrderedDict
from collections.abc import AsyncGenerator, Callable
from typing import Any

from google.adk.agents.base_agent import BaseAgent
from google.adk.agents.callback_context import CallbackContext
from google.adk.agents.invocation_context import InvocationContext
from google.adk.agents.run_config import StreamingMode
from google.adk.events.event import Event
from google.adk.events.event_actions import EventActions
from google.adk.models.base_llm import BaseLlm
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.adk.models.registry import LLMRegistry
from google.adk.tools.base_tool import BaseTool
from google.adk.tools.tool_context import ToolContext
from google.genai import errors as genai_errors
from google.genai import types
from pydantic import BaseModel, ConfigDict, Field, PrivateAttr

from . import openai_chat
from .acp import Executor
from .sandbox import Interceptor, Request, Sandbox, deny, respond, short_name

logger = logging.getLogger(__name__)

MCP_SERVER_NAME = "adk"
"""The name under which the coding agent sees the tools."""

MAX_SESSIONS = 10000
"""Bounds the sessions whose delivered history and pending question the
agent remembers."""

MODEL_TIMEOUT = 280.0
"""Bounds a model call of the coding agent, in seconds, below the time the
sandbox library waits for an answer to a request (5 minutes)."""

TOOL_TIMEOUT = 240.0
"""Bounds a tool call of the coding agent, in seconds. It stays below the
time the sandbox library waits for an answer to a request (5 minutes), so
that the agent never sees a failure for a tool that went on to run."""


class SandboxConfig(BaseModel):
    """A Docker sandbox per ADK session, whose workspace lives in memory and
    whose only network path is the egress boundary.

    The boundary sends the requests to the hosts of `AcpAgent.model_url` and
    `AcpAgent.mcp_url` to the agent's model and tools, in process; `egress`
    decides every other connection, and denies them all when it is None.
    """

    model_config = ConfigDict(arbitrary_types_allowed=True)

    image: str
    """The image of the containers."""
    command: list[str]
    """The agent's command in the image, such as ["opencode", "acp"]."""
    workspace: str = "/workspace"
    """Where the file system in memory is mounted, and the agent works."""
    files: dict[str, bytes | str] = Field(default_factory=dict)
    """The files each session's workspace starts with, by relative path."""
    workspace_size: str = "256m"
    """Bounds each workspace."""
    env: dict[str, str] = Field(default_factory=dict)
    """The agent's environment, such as its configuration."""
    egress: Any = None
    """An Interceptor, with connect and request methods, that decides the
    connections to other hosts and the requests of those it inspects."""
    inspect_tls: list[str] = Field(default_factory=list)
    """The hosts whose TLS an inspected connection terminates."""
    labels: dict[str, str] = Field(default_factory=dict)
    """The labels of the containers."""
    idle_timeout: float = 1800
    """Removes the sandbox of a session without traffic or a turn for so
    many seconds, at least a minute past the agent's own idle stop."""


class AcpAgent(BaseAgent):
    """Runs an ACP coding agent as an ADK agent."""

    model_config = ConfigDict(arbitrary_types_allowed=True)

    argv: list[str] = Field(default_factory=list)
    """The agent's command, when it runs on this host."""
    cwd: str = ""
    """The working directory of the agent's command."""
    env: dict[str, str] = Field(default_factory=dict)
    """Added to the environment of the agent's command."""
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
    sandbox: SandboxConfig | None = None
    """Runs the agent of each ADK session in a sandbox of its own. Set
    exactly one of argv, start and sandbox."""
    model: BaseLlm | str | None = None
    """The ADK model that answers the agent's model calls: the requests in
    the OpenAI chat completions format that it sends to model_url. The
    agent's configuration has to name model_url as its provider."""
    model_url: str = "http://llm.internal/v1"
    """The base URL of the model, which an interceptor answers with
    handle_model."""
    before_model_callback: Callable | list[Callable] | None = None
    """Called with (callback_context, llm_request) before each model call,
    as for an LlmAgent; a returned LlmResponse skips the model."""
    after_model_callback: Callable | list[Callable] | None = None
    """Called with (callback_context, llm_response) after each model call;
    a returned LlmResponse replaces the model's."""
    idle_timeout: float = 600
    """Stops the agent of an idle session after so many seconds."""
    inactivity_timeout: float = 900
    """Fails a turn whose agent is silent so long."""

    _executor: Executor | None = PrivateAttr(default=None)
    _llm: BaseLlm | None = PrivateAttr(default=None)
    # The sandboxes and their routers, by session, with sandbox.
    _sandboxes: dict[str, Sandbox] = PrivateAttr(default_factory=dict)
    _routers: dict[str, "_SessionRouter"] = PrivateAttr(default_factory=dict)
    _reaper: threading.Thread | None = PrivateAttr(default=None)
    _closed: threading.Event = PrivateAttr(default_factory=threading.Event)
    _lock: threading.Lock = PrivateAttr(default_factory=threading.Lock)
    _runs: dict[str, "_Run"] = PrivateAttr(default_factory=dict)
    # The task waiting for an answer, by session.
    _pending: OrderedDict[str, str] = PrivateAttr(default_factory=OrderedDict)
    # The last session event sent to the agent, by session.
    _delivered: OrderedDict[str, str] = PrivateAttr(default_factory=OrderedDict)

    def model_post_init(self, context: Any) -> None:
        super().model_post_init(context)
        if [bool(self.argv), self.start is not None, self.sandbox is not None].count(True) != 1:
            raise ValueError("AcpAgent needs exactly one of argv, start and sandbox")
        if self.argv and (self.tools or self.model is not None):
            raise ValueError("AcpAgent serves tools and a model only to an agent in a sandbox, or one that start runs behind an interceptor")
        if isinstance(self.model, BaseLlm):
            self._llm = self.model
        elif isinstance(self.model, str):
            self._llm = LLMRegistry.new_llm(self.model)
        for t in self.tools:
            if t._get_declaration() is None:  # pylint: disable=protected-access
                raise ValueError(f"AcpAgent tool {t.name!r} has no function declaration to serve over MCP")
        names = [t.name for t in self.tools]
        if len(names) != len(set(names)):
            raise ValueError(f"AcpAgent tools have duplicate names: {sorted(names)}")

    def _unseen(self, ctx: InvocationContext) -> tuple[str, str]:
        """Returns the messages of the session that the agent wasn't sent,
        and the ID of the last event they reach to.

        The agent keeps its own history, so it gets what others said since
        its last turn, such as other agents of a workflow, marked with their
        author, and the user's message. Events of other branches don't
        count, as in the history ADK gives a model.
        """
        events = [e for e in ctx.session.events if not e.partial and _on_branch(e, ctx.branch)]
        with self._lock:
            delivered = self._delivered.get(ctx.session.id, "")
        last = max((i for i, e in enumerate(events) if e.author == self.name or e.id == delivered), default=-1)
        unseen = events[last + 1 :]
        lines = []
        for e in unseen:
            text = _text(e.content)
            if text:
                lines.append(text if e.author == "user" else f"[{e.author}] {text}")
        user = _text(ctx.user_content)
        if user and not any(e.author == "user" and _text(e.content) == user for e in unseen):
            lines.append(user)
        return "\n\n".join(lines), events[-1].id if events else ""

    def _delivered_to(self, session_id: str, event_id: str) -> None:
        """Records that the agent was sent the session up to event_id."""
        if not event_id:
            return
        with self._lock:
            self._delivered[session_id] = event_id
            self._delivered.move_to_end(session_id)
            while len(self._delivered) > MAX_SESSIONS:
                self._delivered.popitem(last=False)

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
                    cwd=self.cwd,
                    env=self.env,
                    start=self._start_sandboxed if self.sandbox is not None else self.start,
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
        """Stops the agents and removes their sandboxes; runs in progress
        fail."""
        self._closed.set()
        with self._lock:
            executor, self._executor = self._executor, None
            boxes = list(self._sandboxes.values())
            self._sandboxes.clear()
            self._routers.clear()
        if executor is not None:
            executor.close()
        for sb in boxes:
            sb.close()

    def sandbox_of(self, session_id: str) -> Sandbox | None:
        """Returns the sandbox of an ADK session, with sandbox, to read and
        write its workspace."""
        with self._lock:
            return self._sandboxes.get(session_id)

    def _start_sandboxed(self, session_id: str) -> int:
        """Starts the agent of a session in its sandbox, made on first use."""
        assert self.sandbox is not None
        with self._lock:
            sb = self._sandboxes.get(session_id)
            router = self._routers.get(session_id)
            if router is not None:
                router.touch()
            if self._reaper is None:
                self._reaper = threading.Thread(target=self._reap, name="acp-sandbox-reaper", daemon=True)
                self._reaper.start()
        if sb is None:
            cfg = self.sandbox
            router = _SessionRouter(self, session_id)
            sb = Sandbox(
                short_name("a2a-acp-" + self.name.replace("_", "-")),
                cfg.image,
                cfg.command,
                cwd=cfg.workspace,
                env=cfg.env,
                memory_dir=cfg.workspace,
                memory_size=cfg.workspace_size,
                files=cfg.files,
                network="allowlist",
                interceptor=router,
                inspect_tls=cfg.inspect_tls,
                labels=cfg.labels,
            )
            with self._lock:
                closed = self._closed.is_set()
                if not closed:
                    self._sandboxes[session_id] = sb
                    self._routers[session_id] = router
            if closed:
                sb.close()
                raise RuntimeError("the agent is closed")
        return sb.start_fd()

    def _reap(self) -> None:
        """Removes the sandboxes of idle sessions."""
        assert self.sandbox is not None
        # The agent stops its idle process first, so that a turn which
        # starts as the reaper works asks for a sandbox under the lock.
        limit = max(self.sandbox.idle_timeout, self.idle_timeout + 60)
        was_busy: set[str] = set()
        while not self._closed.wait(min(60.0, limit)):
            busy = self.busy_sessions()
            now = time.monotonic()
            with self._lock:
                # The agent's idle time starts when a turn ends.
                for k in was_busy - busy:
                    if k in self._routers:
                        self._routers[k].touch()
                idle = [k for k, r in self._routers.items() if now - r.last_used > limit and k not in busy]
                boxes = [self._sandboxes.pop(k) for k in idle]
                for k in idle:
                    del self._routers[k]
            was_busy = busy
            for sb in boxes:
                try:
                    sb.close()
                except Exception as e:  # pylint: disable=broad-exception-caught
                    logger.warning("removing the sandbox of an idle session: %s", e)

    def handle_model(self, session_id: str, method: str, path: str, body: bytes) -> tuple[int, dict[str, str], bytes]:
        """Answers a request of the agent of the ADK session to its model,
        in the OpenAI chat completions API, with the ADK model.

        The call runs in the invocation in progress in the session, with
        the model callbacks. Call it from a worker thread, such as the one
        an interceptor runs on.
        """
        json_headers = {"Content-Type": "application/json"}
        if self._llm is None:
            return 404, json_headers, openai_chat.error_body("the agent has no model", "not_found")
        if method == "GET" and path.endswith("/models"):
            listing = {"object": "list", "data": [{"id": self._llm.model, "object": "model", "owned_by": "adk"}]}
            return 200, json_headers, json.dumps(listing).encode()
        if method != "POST" or not path.endswith("/chat/completions"):
            return 404, json_headers, openai_chat.error_body(f"no route {method} {path}", "not_found")
        try:
            req = json.loads(body)
            llm_request = openai_chat.to_llm_request(req, self._llm.model)
        except (json.JSONDecodeError, openai_chat.BadRequest) as e:
            return 400, json_headers, openai_chat.error_body(str(e), "invalid_request_error")
        with self._lock:
            run = self._runs.get(session_id)
        if run is None:
            return 409, json_headers, openai_chat.error_body("no ADK invocation of this session is in progress", "conflict")
        future = asyncio.run_coroutine_threadsafe(run.call_model(self._llm, llm_request), run.loop)
        try:
            response = future.result(timeout=MODEL_TIMEOUT)
        except Exception as e:  # pylint: disable=broad-exception-caught
            future.cancel()
            return 502, json_headers, openai_chat.error_body(f"{type(e).__name__}: {e}")
        if response.error_code and openai_chat.finish_of_error(response.error_code) is None:
            status = _error_status(response.error_code)
            return status, json_headers, openai_chat.error_body(response.error_message or response.error_code, response.error_code)
        if req.get("stream"):
            return 200, {"Content-Type": "text/event-stream"}, openai_chat.to_stream(response, self._llm.model)
        return 200, json_headers, json.dumps(openai_chat.to_completion(response, self._llm.model)).encode()

    async def _run_async_impl(
        self, ctx: InvocationContext
    ) -> AsyncGenerator[Event, None]:
        key = ctx.session.id
        with self._lock:
            task_id = self._pending.pop(key, "")
        # An answer to a question goes to the agent as typed, which is how
        # the agent matches it to an option; other messages carry what the
        # agent wasn't sent.
        if task_id:
            text, through = _text(ctx.user_content), ""
        else:
            text, through = self._unseen(ctx)
        executor = await asyncio.to_thread(self.executor)
        run = _Run(self, ctx, on_delivered=lambda: self._delivered_to(key, through))
        with self._lock:
            self._runs[key] = run
        pump = threading.Thread(target=run.pump, args=(executor, text, key, task_id), name="acp-stream", daemon=True)
        pump.start()
        reply: list[str] = []
        try:
            while True:
                kind, item, done = await run.queue.get()
                if kind == "end":
                    if item is not None:
                        for ev in self._failed(ctx, reply, str(item)):
                            yield ev
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
            if ctx.run_config is not None and ctx.run_config.streaming_mode == StreamingMode.NONE:
                return []
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
            with self._lock:
                self._pending[key] = task_id
                self._pending.move_to_end(key)
                while len(self._pending) > MAX_SESSIONS:
                    self._pending.popitem(last=False)
            text = "".join(reply)
            text = f"{text}\n\n{message}" if text else message
            return [
                self._event(ctx, text=text, metadata={"acp_input_required": task_id})
            ]
        if state in ("TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED"):
            return self._failed(ctx, reply, message or state)
        # A question carries its tool call too, so tool updates come after it.
        for p in parts:
            data = p.get("data")
            if isinstance(data, dict) and data.get("toolCall"):
                return [self._event(ctx, metadata={"acp_tool_call": data["toolCall"]})]
        return []

    def _failed(self, ctx: InvocationContext, reply: list[str], error: str) -> list[Event]:
        """Returns the events of a failed turn: the text streamed before it,
        then the error, which is also the content as in acpagent."""
        events = [self._event(ctx, text="".join(reply))] if reply else []
        return events + [self._event(ctx, text=error, error=error)]

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
            ev.error_message = error
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
            result, failed = future.result(timeout=TOOL_TIMEOUT)
        except Exception as e:  # pylint: disable=broad-exception-caught
            future.cancel()
            return _tool_error(f"{type(e).__name__}: {e}")
        text = json.dumps(result, default=str)
        out: dict[str, Any] = {"content": [{"type": "text", "text": text}]}
        if failed:
            out["isError"] = True
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

    def __init__(self, agent: AcpAgent, ctx: InvocationContext, on_delivered: Callable[[], None] = lambda: None):
        self.agent = agent
        self.ctx = ctx
        # on_delivered is called once the agent got the message.
        self._on_delivered = on_delivered
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
            delivered = False
            for ev in executor.stream(text, context_id=key, task_id=task_id, stop=self._stop):
                if not delivered and _answered(ev):
                    delivered = True
                    self._on_delivered()
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

    async def call_model(self, llm: BaseLlm, llm_request: LlmRequest) -> LlmResponse:
        """Runs a model call in the invocation, with the agent's model
        callbacks; the state they change is saved with an event."""
        actions = EventActions()
        callback_context = CallbackContext(self.ctx, event_actions=actions)
        response = await _first_response(self.agent.before_model_callback, callback_context, llm_request)
        if response is None:
            failed = False
            try:
                async for r in llm.generate_content_async(llm_request, stream=False):
                    response = r
            except genai_errors.APIError as e:
                response, failed = LlmResponse(error_code=str(e.code), error_message=e.message or str(e)), True
            if response is None:
                response, failed = LlmResponse(error_code="502", error_message="the model returned nothing"), True
            # As for an LlmAgent, after_model_callback sees answers of the
            # model only: not those of before_model_callback, nor errors.
            if not failed:
                changed = await _first_response(self.agent.after_model_callback, callback_context, response)
                if changed is not None:
                    response = changed
        if actions.state_delta or actions.artifact_delta:
            ev = self.agent._event(self.ctx)
            ev.actions = actions
            await self.emit(ev)
        return response

    async def call_tool(self, tool: BaseTool, args: dict) -> tuple[Any, bool]:
        """Runs tool in the invocation, and reports whether it failed."""
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
        failed = False
        try:
            result = await tool.run_async(args=args, tool_context=tool_ctx)
        except Exception as e:  # pylint: disable=broad-exception-caught
            result, failed = {"error": str(e) or type(e).__name__}, True
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
        return result, failed


async def _first_response(callbacks: Callable | list[Callable] | None, *args: Any) -> Any:
    """Calls callbacks in turn, as ADK calls an LlmAgent's model callbacks,
    and returns the first answer that isn't None."""
    if callbacks is None:
        return None
    for cb in callbacks if isinstance(callbacks, list) else [callbacks]:
        result = cb(*args)
        if inspect.isawaitable(result):
            result = await result
        if result is not None:
            return result
    return None


def _error_status(error_code: str) -> int:
    """Returns the HTTP status of a model call that ended with error_code:
    429 for a rate limit, which the agent retries, the model's own status
    for another error of the model, and 403 for a refusal of a callback,
    which the agent doesn't retry."""
    if error_code in ("429", "RESOURCE_EXHAUSTED"):
        return 429
    if error_code == "MALFORMED_FUNCTION_CALL":
        return 502
    if error_code.isdigit() and 400 <= int(error_code) < 600:
        return int(error_code)
    return 403


class _SessionRouter:
    """Decides the connections of the sandbox of one session: the model and
    tool hosts are answered in process, the rest by the egress policy."""

    def __init__(self, agent: AcpAgent, session_id: str):
        self.agent = agent
        self.session_id = session_id
        self.last_used = time.monotonic()
        self.model_host = urllib.parse.urlsplit(agent.model_url).hostname
        self.tools_host = urllib.parse.urlsplit(agent.mcp_url).hostname

    def touch(self) -> None:
        self.last_used = time.monotonic()

    def _egress(self) -> Interceptor | None:
        return self.agent.sandbox.egress if self.agent.sandbox is not None else None

    def connect(self, host: str, port: int) -> str:
        self.touch()
        if host in (self.model_host, self.tools_host):
            return "inspect"
        egress = self._egress()
        return egress.connect(host, port) if egress is not None else "deny"

    def request(self, req: Request) -> dict:
        self.touch()
        url = urllib.parse.urlsplit(req.url)
        if url.hostname == self.model_host:
            status, headers, body = self.agent.handle_model(self.session_id, req.method, url.path, req.body)
            return respond(status, body, headers)
        if url.hostname == self.tools_host:
            status, headers, body = self.agent.handle_mcp(self.session_id, req.method, req.body)
            return respond(status, body, headers)
        egress = self._egress()
        return egress.request(req) if egress is not None else deny("the sandbox reaches no other host")


def _answered(ev: dict) -> bool:
    """Reports whether ev shows that the agent got the message: a reply, a
    tool update, a question or the end of its turn."""
    if "artifactUpdate" in ev:
        return True
    status = (ev.get("statusUpdate") or {}).get("status") or {}
    if status.get("state") in ("TASK_STATE_COMPLETED", "TASK_STATE_INPUT_REQUIRED"):
        return True
    parts = (status.get("message") or {}).get("parts", [])
    return any(isinstance(p.get("data"), dict) and p["data"].get("toolCall") for p in parts)


def _on_branch(ev: Event, branch: str | None) -> bool:
    """Reports whether ev belongs to the history of branch, as ADK decides
    for the history it gives a model."""
    return not ev.branch or not branch or branch == ev.branch or branch.startswith(ev.branch + ".")


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
