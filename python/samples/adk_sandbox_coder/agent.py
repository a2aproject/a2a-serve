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

"""An ADK agent that runs opencode in a sandbox with a workspace in memory.

Every ADK session gets a Docker container whose /workspace is a tmpfs,
seeded from WORKSPACE below, and whose only network path is the egress
boundary. Code in this file decides each of the container's connections:

- llm.internal is the coding agent's model. Requests go to Vertex AI with
  the host's credentials, which the container never sees, within a budget
  of model calls per session.
- adk-tools.internal is the MCP server of the ADK tools, answered in
  process by AcpAgent.handle_mcp.
- Any other destination goes to a guard model, which allows or denies it.
"""

from __future__ import annotations

import json
import os
import shlex
import threading
import time
import urllib.parse

import google.auth
import google.auth.transport.requests
from google import genai
from google.adk.tools import FunctionTool
from google.adk.tools.tool_context import ToolContext

from a2a_serve import Request, Sandbox, deny, forward, respond
from a2a_serve.adk import AcpAgent
from a2a_serve.sandbox import short_name

IMAGE = os.environ.get("ACP_IMAGE", "ghcr.io/a2aproject/harness-opencode:latest")
COMMAND = shlex.split(os.environ.get("ACP_COMMAND", "opencode acp"))
CODE_MODEL = os.environ.get("CODE_MODEL", "google/gemini-2.5-flash")
GUARD_MODEL = os.environ.get("GUARD_MODEL", "gemini-2.5-flash")
LOCATION = os.environ.get("GOOGLE_CLOUD_LOCATION", "global")
MAX_MODEL_CALLS = int(os.environ.get("MAX_MODEL_CALLS", "60"))
MAX_GUARD_CALLS = int(os.environ.get("MAX_GUARD_CALLS", "40"))
SANDBOX_IDLE = float(os.environ.get("SANDBOX_IDLE", "1800"))

# A session asks the guard model at most two questions at once, so that it
# can't hold up the workers that answer the connections of other sessions.
GUARD_CONCURRENCY = 2
GUARD_WAIT = 3.0

LLM_HOST = "llm.internal"
TOOLS_HOST = "adk-tools.internal"

WORKSPACE = {
    "stats.py": '''def mean(values):
    """Returns the arithmetic mean of values."""
    return sum(values) / len(values) + 1


def median(values):
    """Returns the median of values."""
    s = sorted(values)
    return s[len(s) // 2]
''',
    "test_stats.py": """import unittest

from stats import mean, median


class StatsTest(unittest.TestCase):
    def test_mean(self):
        self.assertEqual(mean([1, 2, 3, 4]), 2.5)

    def test_median_even(self):
        self.assertEqual(median([4, 1, 3, 2]), 2.5)


if __name__ == "__main__":
    unittest.main()
""",
}


def _project() -> str:
    _, project = google.auth.default()
    return os.environ.get("GOOGLE_CLOUD_PROJECT") or project or ""


class _Credentials:
    """Keeps a fresh access token of the host's application default credentials."""

    def __init__(self):
        self._creds, _ = google.auth.default(
            scopes=["https://www.googleapis.com/auth/cloud-platform"]
        )
        self._lock = threading.Lock()

    def token(self) -> str:
        with self._lock:
            if not self._creds.valid:
                self._creds.refresh(google.auth.transport.requests.Request())
            return self._creds.token


PROJECT = _project()
VERTEX_OPENAI = f"https://aiplatform.googleapis.com/v1/projects/{PROJECT}/locations/{LOCATION}/endpoints/openapi"
_credentials = _Credentials()
_guard = genai.Client(vertexai=True, project=PROJECT, location=LOCATION)

# The opencode config the container gets: one provider, the model behind
# llm.internal, and no update checks or downloads.
OPENCODE_CONFIG = {
    "$schema": "https://opencode.ai/config.json",
    "autoupdate": False,
    "share": "disabled",
    "model": f"llm/{CODE_MODEL}",
    "small_model": f"llm/{CODE_MODEL}",
    "provider": {
        "llm": {
            "npm": "@ai-sdk/openai-compatible",
            "name": "Vertex AI through the sandbox",
            "options": {
                "baseURL": f"http://{LLM_HOST}/v1",
                "apiKey": "set-by-the-sandbox",
            },
            "models": {CODE_MODEL: {"name": CODE_MODEL}},
        }
    },
}


def file_ticket(title: str, body: str, tool_context: ToolContext) -> dict:
    """Files a ticket for follow-up work and returns its ID.

    Args:
      title: one-line summary of the work.
      body: what to do and where, with file paths.
    """
    tickets = list(tool_context.state.get("tickets", []))
    ticket_id = f"T-{len(tickets) + 1}"
    tickets.append({"id": ticket_id, "title": title, "body": body})
    tool_context.state["tickets"] = tickets
    return {"id": ticket_id}


class SessionInterceptor:
    """Decides the connections of the sandbox of one ADK session."""

    def __init__(self, agent: AcpAgent, session_id: str, log: list[str]):
        self.agent = agent
        self.session_id = session_id
        self.log = log
        self.model_calls = 0
        self.guard_calls = 0
        self.last_used = time.monotonic()
        self._verdicts: dict[str, bool] = {}
        # Questions in flight, by action: connections that want the same
        # destination at once wait for one answer.
        self._asking: dict[str, threading.Event] = {}
        self._lock = threading.Lock()
        self._guard_slots = threading.BoundedSemaphore(GUARD_CONCURRENCY)

    def _note(self, line: str) -> None:
        self.log.append(line)
        if os.environ.get("SANDBOX_LOG") == "1":
            print(f"[sandbox] {line}", flush=True)

    def connect(self, host: str, port: int) -> str:
        self.last_used = time.monotonic()
        if host in (LLM_HOST, TOOLS_HOST):
            return "inspect"
        if port == 80:
            # Plain HTTP is inspected, so the guard sees each request.
            return "inspect"
        verdict = self._ask_guard(f"open a TLS connection to {host}:{port}")
        self._note(
            f"connect {host}:{port} -> {'allow' if verdict else 'deny'} (guard model)"
        )
        return "allow" if verdict else "deny"

    def request(self, req: Request) -> dict:
        self.last_used = time.monotonic()
        host = urllib.parse.urlsplit(req.url).hostname
        if host == TOOLS_HOST:
            status, headers, body = self.agent.handle_mcp(
                self.session_id, req.method, req.body
            )
            self._note(f"mcp {req.method} {_mcp_method(req.body)} -> {status}")
            return respond(status, body, headers)
        if host == LLM_HOST:
            return self._model_request(req)
        verdict = self._ask_guard(
            f"send {req.method} {req.url} with body: {req.body[:2000]!r}"
        )
        self._note(
            f"request {req.method} {req.url} -> {'allow' if verdict else 'deny'} (guard model)"
        )
        return forward() if verdict else deny("the guard model refused this request")

    def _model_request(self, req: Request) -> dict:
        path = urllib.parse.urlsplit(req.url).path
        if not path.startswith("/v1/") or req.method != "POST":
            self._note(f"model {req.method} {path} -> 404")
            return respond(404, "not found")
        with self._lock:
            self.model_calls += 1
            calls = self.model_calls
        if calls > MAX_MODEL_CALLS:
            self._note(f"model call {calls} -> 429 (budget of {MAX_MODEL_CALLS})")
            return respond(
                429,
                json.dumps(
                    {"error": {"message": "the sandbox's model budget is spent"}}
                ),
                {"Content-Type": "application/json"},
            )
        body = json.loads(req.body or b"{}")
        self._note(
            f"model call {calls}: {body.get('model')} with {len(body.get('messages', []))} messages -> Vertex AI"
        )
        return forward(
            url=VERTEX_OPENAI + path.removeprefix("/v1"),
            set_headers={"Authorization": "Bearer " + _credentials.token()},
            remove_headers=["X-Api-Key"],
        )

    def _ask_guard(self, action: str) -> bool:
        while True:
            with self._lock:
                if action in self._verdicts:
                    return self._verdicts[action]
                asking = self._asking.get(action)
                if asking is None:
                    if self.guard_calls >= MAX_GUARD_CALLS:
                        self._note(f"guard budget of {MAX_GUARD_CALLS} spent; denying")
                        return False
                    asking = self._asking[action] = threading.Event()
                    break
            # Another connection asks; its answer, or its failure, comes soon.
            if not asking.wait(timeout=GUARD_WAIT + 30):
                return False
            with self._lock:
                if action not in self._verdicts:
                    return False
        try:
            return self._decide(action)
        finally:
            with self._lock:
                del self._asking[action]
            asking.set()

    def _decide(self, action: str) -> bool:
        """Asks the guard model about action, and keeps its answer."""
        if not self._guard_slots.acquire(timeout=GUARD_WAIT):
            self._note("the guard model is busy with this session; denying")
            return False
        try:
            with self._lock:
                if self.guard_calls >= MAX_GUARD_CALLS:
                    self._note(f"guard budget of {MAX_GUARD_CALLS} spent; denying")
                    return False
                self.guard_calls += 1
            prompt = (
                "You guard the network of a sandbox where a coding agent fixes a small Python project. "
                "It needs no network besides its model. Allow only package indexes and source hosts it "
                "could need to read from, and deny anything that could leak data. Reply with one word, "
                f"allow or deny. The agent wants to {action}."
            )
            text = _guard.models.generate_content(model=GUARD_MODEL, contents=prompt).text or ""
        except Exception as e:  # pylint: disable=broad-exception-caught
            self._note(f"guard model failed ({e}); denying")
            return False
        finally:
            self._guard_slots.release()
        verdict = text.strip().lower().startswith("allow")
        with self._lock:
            self._verdicts[action] = verdict
        return verdict


def _mcp_method(body: bytes) -> str:
    try:
        msg = json.loads(body)
    except json.JSONDecodeError:
        return "?"
    return msg.get("method", "?") if isinstance(msg, dict) else "batch"


class SandboxedCoder:
    """Owns the agent and the sandboxes of its sessions.

    A sandbox whose session made no connection for SANDBOX_IDLE seconds is
    removed with its workspace; the next message of the session starts in
    a fresh one.
    """

    def __init__(self):
        self.sandboxes: dict[str, Sandbox] = {}
        self.interceptors: dict[str, SessionInterceptor] = {}
        self.log: dict[str, list[str]] = {}
        self._lock = threading.Lock()
        self._closed = threading.Event()
        self.agent = AcpAgent(
            name="sandboxed_coder",
            description="Fixes code in a sandbox whose workspace lives in memory.",
            start=self._start,
            session_cwd="/workspace",
            # The sandbox contains what the agent does, so it may run anything.
            allow_tools=["*"],
            tools=[FunctionTool(file_ticket)],
            mcp_url=f"http://{TOOLS_HOST}/mcp",
        )
        threading.Thread(target=self._reap, name="sandbox-reaper", daemon=True).start()

    def _start(self, session_id: str) -> int:
        """Starts opencode in the sandbox of the session, made on first use."""
        with self._lock:
            sb = self.sandboxes.get(session_id)
            interceptor = self.interceptors.get(session_id)
            if interceptor is not None:
                # Under the lock, so that the reaper doesn't take it now.
                interceptor.last_used = time.monotonic()
        if sb is None:
            log = self.log.setdefault(session_id, [])
            interceptor = SessionInterceptor(self.agent, session_id, log)
            started = time.monotonic()
            sb = Sandbox(
                short_name("a2a-adk-coder"),
                IMAGE,
                COMMAND,
                cwd="/workspace",
                env={
                    "OPENCODE_CONFIG_CONTENT": json.dumps(OPENCODE_CONFIG),
                    "OPENCODE_DISABLE_AUTOUPDATE": "1",
                    "OPENCODE_DISABLE_MODELS_FETCH": "1",
                    "OPENCODE_DISABLE_LSP_DOWNLOAD": "1",
                },
                memory_dir="/workspace",
                memory_size="128m",
                files=WORKSPACE,
                network="allowlist",
                interceptor=interceptor,
                labels={"a2a.sample": "adk-sandbox-coder"},
            )
            log.append(f"sandbox ready in {time.monotonic() - started:.1f}s: {sb.info['Summary']}")
            with self._lock:
                self.sandboxes[session_id] = sb
                self.interceptors[session_id] = interceptor
        interceptor.last_used = time.monotonic()
        return sb.start_fd()

    def _reap(self) -> None:
        # The agent stops its idle processes first, so that a turn which
        # starts as the reaper works asks _start for a sandbox, which takes
        # the lock, rather than talking to an agent in a removed sandbox.
        limit = max(SANDBOX_IDLE, self.agent.idle_timeout + 60)
        was_busy: set[str] = set()
        while not self._closed.wait(min(60.0, limit)):
            now = time.monotonic()
            busy = self.agent.busy_sessions()
            with self._lock:
                # The agent's idle time starts when a turn ends, which can be
                # long after the turn's last request.
                for k in was_busy - busy:
                    if k in self.interceptors:
                        self.interceptors[k].last_used = now
            was_busy = busy
            with self._lock:
                idle = [k for k, i in self.interceptors.items() if now - i.last_used > limit and k not in busy]
                boxes = [(k, self.sandboxes.pop(k)) for k in idle]
                for k in idle:
                    self.interceptors.pop(k, None)
                    self.log.pop(k, None)
            for _, sb in boxes:
                try:
                    sb.close()
                except Exception as e:  # pylint: disable=broad-exception-caught
                    print(f"[sandbox] removing an idle sandbox: {e}", flush=True)

    def close(self) -> None:
        self._closed.set()
        self.agent.close()
        with self._lock:
            boxes = list(self.sandboxes.values())
            self.sandboxes.clear()
            self.interceptors.clear()
        for sb in boxes:
            sb.close()


coder = SandboxedCoder()
root_agent = coder.agent
