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

"""Python bindings of the a2aacp library (liba2aacp.so).

An `Executor` runs an agent of the Agent Client Protocol, such as
`opencode acp`, and talks A2A to it in process: `send` returns the A2A
stream events of a message as dicts. The agent runs as a local command, or
`start` starts it itself, such as in a `Sandbox`, and returns the file
descriptor of its stdio.
"""

from __future__ import annotations

import asyncio
import ctypes
import json
import threading
import uuid
from collections.abc import AsyncIterator, Callable, Iterator
from typing import Any

from . import _native

_SIGNATURES = {
    "new": [ctypes.c_char_p],
    "close": [ctypes.c_int64],
    "send": [ctypes.c_int64, ctypes.c_char_p],
    "stream_next": [ctypes.c_int64, ctypes.c_int32],
    "stream_close": [ctypes.c_int64],
    "cancel": [ctypes.c_int64, ctypes.c_char_p],
    "next_event": [ctypes.c_int32],
    "reply": [ctypes.c_int64, ctypes.c_char_p],
}

StartFunc = Callable[[str], int]
"""Starts the agent of a key (the A2A context with per_context) and returns
the file descriptor of its stdio, which the library then owns."""

_lib: _native.Library | None = None
_pump: _native.EventPump | None = None
_starters: dict[str, StartFunc] = {}
_lock = threading.Lock()


def _library() -> _native.Library:
    global _lib, _pump
    with _lock:
        if _lib is None:
            _lib = _native.Library("liba2aacp.so", "a2a_acp_", _SIGNATURES)
            _pump = _native.EventPump(_lib, _dispatch)
        return _lib


def _dispatch(kind: str, data: dict) -> dict:
    if kind != "start":
        return {"error": f"unknown question {kind}"}
    starter = _starters.get(data["id"])
    if starter is None:
        return {"error": "the executor has no start function"}
    return {"fd": starter(data["key"])}


class Executor:
    """An ACP agent behind an in-process A2A request handler."""

    def __init__(
        self,
        *,
        argv: list[str] | None = None,
        cwd: str = "",
        env: dict[str, str] | None = None,
        start: StartFunc | None = None,
        per_context: bool = False,
        idle_timeout: float = 0,
        max_agents: int = 0,
        session_cwd: str = "",
        mcp_servers: list[dict[str, Any]] | None = None,
        allow: list[str] | None = None,
        deny: list[str] | None = None,
        settings: dict[str, Any] | None = None,
        inactivity_timeout: float = 0,
        question_timeout: float = 0,
    ):
        """Starts the executor, and the agent unless per_context.

        Args:
          argv: the agent's command, such as ["opencode", "acp"].
          cwd: the agent's working directory.
          env: added to the agent's environment.
          start: starts the agent instead of argv; see StartFunc.
          per_context: runs an agent per A2A context, started on its first
            message.
          idle_timeout: stops an idle agent of a context after so many seconds.
          max_agents: bounds the agents that run.
          session_cwd: the working directory of the ACP sessions, as the agent
            sees it.
          mcp_servers: the MCP servers of the sessions, in the ACP schema.
          allow: the ACP tool kinds the agent may run without asking.
          deny: the tool kinds it may never run.
          settings: session settings, such as {"model": "provider/model"}.
          inactivity_timeout: fails a task whose agent is silent so long.
          question_timeout: rejects an unanswered question after so long.
        """
        lib = _library()
        cfg: dict[str, Any] = {
            "per_context": per_context,
            "idle_timeout_ms": int(idle_timeout * 1000),
            "max_agents": max_agents,
            "session": {"cwd": session_cwd, "mcp_servers": mcp_servers or []},
            "allow": allow or [],
            "deny": deny or [],
            "settings": settings or {},
            "inactivity_timeout_ms": int(inactivity_timeout * 1000),
            "question_timeout_ms": int(question_timeout * 1000),
        }
        if start is not None:
            cfg["external"] = True
        elif argv:
            cfg["agent"] = {
                "argv": argv,
                "dir": cwd,
                "env": [f"{k}={v}" for k, v in (env or {}).items()],
            }
        else:
            raise ValueError("set argv or start")
        self._id = str(uuid.uuid4())
        cfg["id"] = self._id
        if start is not None:
            _starters[self._id] = start
        try:
            self.handle: int = lib.call("new", json.dumps(cfg))["handle"]
        except Exception:
            _starters.pop(self._id, None)
            raise

    def stream(
        self,
        text: str = "",
        *,
        context_id: str = "",
        task_id: str = "",
        parts: list[dict] | None = None,
        metadata: dict | None = None,
        stop: Callable[[], bool] | None = None,
    ) -> Iterator[dict]:
        """Sends a message and yields the A2A stream events of its task.

        stop, when it returns true between events, stops waiting for them.
        """
        sid = self._send(text, context_id, task_id, parts, metadata)
        try:
            while stop is None or not stop():
                r = _library().call("stream_next", sid, 500)
                if r.get("done"):
                    sid = 0
                    return
                if "event" in r:
                    yield r["event"]
        finally:
            self._close_stream(sid)

    async def astream(
        self,
        text: str = "",
        *,
        context_id: str = "",
        task_id: str = "",
        parts: list[dict] | None = None,
        metadata: dict | None = None,
    ) -> AsyncIterator[dict]:
        """The async form of stream; the library waits on worker threads.

        Closing or canceling it stops waiting for the task's events. The
        task goes on until cancel.
        """
        send = asyncio.ensure_future(asyncio.to_thread(self._send, text, context_id, task_id, parts, metadata))
        try:
            sid = await asyncio.shield(send)
        except asyncio.CancelledError:
            # The send goes on in its thread; close the stream it opens.
            send.add_done_callback(lambda f: f.cancelled() or f.exception() is not None or self._close_stream(f.result()))
            raise
        try:
            while True:
                r = await asyncio.to_thread(_library().call, "stream_next", sid, 500)
                if r.get("done"):
                    sid = 0
                    return
                if "event" in r:
                    yield r["event"]
        finally:
            self._close_stream(sid)

    def _send(self, text: str, context_id: str, task_id: str, parts: list[dict] | None, metadata: dict | None) -> int:
        msg: dict[str, Any] = {
            "messageId": str(uuid.uuid4()),
            "role": "ROLE_USER",
            "parts": parts or [{"text": text}],
        }
        if context_id:
            msg["contextId"] = context_id
        if task_id:
            msg["taskId"] = task_id
        if metadata:
            msg["metadata"] = metadata
        return _library().call("send", self.handle, json.dumps({"message": msg}))["stream"]

    @staticmethod
    def _close_stream(sid: int) -> None:
        if not sid:
            return
        try:
            _library().call("stream_close", sid)
        except _native.NativeError:
            # The stream ended meanwhile.
            pass

    def cancel(self, task_id: str) -> dict:
        return _library().call("cancel", self.handle, task_id)["task"]

    def close(self) -> None:
        _starters.pop(self._id, None)
        _library().call("close", self.handle)
