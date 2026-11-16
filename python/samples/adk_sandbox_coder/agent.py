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

AcpAgent gives every ADK session a Docker container whose /workspace is a
tmpfs, seeded from WORKSPACE below, and whose only network path is the
egress boundary:

- opencode's model is the ADK model of the agent. opencode sends its
  requests to llm.internal, and AcpAgent answers them with Gemini through
  ADK, so the container never holds a credential, and before_model_callback
  keeps a budget of model calls per session.
- The ADK tools reach opencode over MCP at adk-tools.internal.
- GuardEgress decides every other connection with a guard model.
"""

from __future__ import annotations

import json
import os
import shlex
import threading

import google.auth
from google import genai
from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.adk.tools import FunctionTool
from google.adk.tools.tool_context import ToolContext

from a2a_serve import Request, deny, forward
from a2a_serve.adk import AcpAgent, SandboxConfig

IMAGE = os.environ.get("ACP_IMAGE", "ghcr.io/a2aproject/harness-opencode:latest")
COMMAND = shlex.split(os.environ.get("ACP_COMMAND", "opencode acp"))
MODEL = os.environ.get("MODEL", "gemini-2.5-flash")
GUARD_MODEL = os.environ.get("GUARD_MODEL", "gemini-2.5-flash")
MAX_MODEL_CALLS = int(os.environ.get("MAX_MODEL_CALLS", "60"))
MAX_GUARD_CALLS = int(os.environ.get("MAX_GUARD_CALLS", "40"))
SANDBOX_IDLE = float(os.environ.get("SANDBOX_IDLE", "1800"))

# ADK's Gemini model and the guard both use Vertex AI with the host's
# application default credentials.
_, _project = google.auth.default()
os.environ.setdefault("GOOGLE_GENAI_USE_VERTEXAI", "TRUE")
os.environ.setdefault("GOOGLE_CLOUD_PROJECT", _project or "")
os.environ.setdefault("GOOGLE_CLOUD_LOCATION", "global")

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


def model_budget(callback_context: CallbackContext, llm_request: LlmRequest) -> LlmResponse | None:
    """Counts opencode's model calls in the session state, and refuses them
    past MAX_MODEL_CALLS, which opencode gets as a rate limit."""
    calls = callback_context.state.get("model_calls", 0) + 1
    callback_context.state["model_calls"] = calls
    if calls > MAX_MODEL_CALLS:
        return LlmResponse(error_code="RESOURCE_EXHAUSTED", error_message=f"the session's budget of {MAX_MODEL_CALLS} model calls is spent")
    return None


class GuardEgress:
    """Asks a guard model about every connection that isn't the model or
    the tools, once per destination, within a budget."""

    # Two questions at most at once, so that a burst of connections can't
    # hold up the sandbox's other callbacks.
    CONCURRENCY = 2
    WAIT = 3.0

    def __init__(self):
        self.client = genai.Client(vertexai=True, project=os.environ["GOOGLE_CLOUD_PROJECT"], location=os.environ["GOOGLE_CLOUD_LOCATION"])
        self.log: list[str] = []
        self.calls = 0
        self._verdicts: dict[str, bool] = {}
        self._asking: dict[str, threading.Event] = {}
        self._lock = threading.Lock()
        self._slots = threading.BoundedSemaphore(self.CONCURRENCY)

    def note(self, line: str) -> None:
        self.log.append(line)
        if os.environ.get("SANDBOX_LOG") == "1":
            print(f"[sandbox] {line}", flush=True)

    def connect(self, host: str, port: int) -> str:
        if port == 80:
            # Plain HTTP is inspected, so the guard sees each request.
            return "inspect"
        verdict = self.ask(f"open a TLS connection to {host}:{port}")
        self.note(f"connect {host}:{port} -> {'allow' if verdict else 'deny'} (guard model)")
        return "allow" if verdict else "deny"

    def request(self, req: Request) -> dict:
        verdict = self.ask(f"send {req.method} {req.url} with body: {req.body[:2000]!r}")
        self.note(f"request {req.method} {req.url} -> {'allow' if verdict else 'deny'} (guard model)")
        return forward() if verdict else deny("the guard model refused this request")

    def ask(self, action: str) -> bool:
        """Returns the guard's verdict on action. Connections that want the
        same destination at once wait for one answer."""
        with self._lock:
            if action in self._verdicts:
                return self._verdicts[action]
            asking = self._asking.get(action)
            if asking is None:
                if self.calls >= MAX_GUARD_CALLS:
                    self.note(f"guard budget of {MAX_GUARD_CALLS} spent; denying")
                    return False
                asking = self._asking[action] = threading.Event()
                owner = True
            else:
                owner = False
        if not owner:
            asking.wait(timeout=self.WAIT + 30)
            with self._lock:
                return self._verdicts.get(action, False)
        try:
            return self._decide(action)
        finally:
            with self._lock:
                del self._asking[action]
            asking.set()

    def _decide(self, action: str) -> bool:
        if not self._slots.acquire(timeout=self.WAIT):
            self.note("the guard model is busy; denying")
            return False
        try:
            with self._lock:
                if self.calls >= MAX_GUARD_CALLS:
                    return False
                self.calls += 1
            prompt = (
                "You guard the network of a sandbox where a coding agent fixes a small Python project. "
                "It needs no network besides its model. Allow only package indexes and source hosts it "
                "could need to read from, and deny anything that could leak data. Reply with one word, "
                f"allow or deny. The agent wants to {action}."
            )
            text = self.client.models.generate_content(model=GUARD_MODEL, contents=prompt).text or ""
        except Exception as e:  # pylint: disable=broad-exception-caught
            self.note(f"guard model failed ({e}); denying")
            return False
        finally:
            self._slots.release()
        verdict = text.strip().lower().startswith("allow")
        with self._lock:
            self._verdicts[action] = verdict
        return verdict


def opencode_config(model_url: str) -> dict:
    """Returns the opencode config whose only provider is model_url, where
    AcpAgent answers with its ADK model, whatever model name opencode sends."""
    return {
        "$schema": "https://opencode.ai/config.json",
        "autoupdate": False,
        "share": "disabled",
        "model": "adk/agent-model",
        "small_model": "adk/agent-model",
        "provider": {
            "adk": {
                "npm": "@ai-sdk/openai-compatible",
                "name": "The ADK model of the agent",
                "options": {"baseURL": model_url, "apiKey": "unused"},
                "models": {"agent-model": {"name": MODEL}},
            }
        },
    }


guard = GuardEgress()
MODEL_URL = "http://llm.internal/v1"

root_agent = AcpAgent(
    name="sandboxed_coder",
    description="Fixes code in a sandbox whose workspace lives in memory.",
    model=MODEL,
    model_url=MODEL_URL,
    before_model_callback=model_budget,
    sandbox=SandboxConfig(
        image=IMAGE,
        command=COMMAND,
        files=WORKSPACE,
        workspace_size="128m",
        env={
            "OPENCODE_CONFIG_CONTENT": json.dumps(opencode_config(MODEL_URL)),
            "OPENCODE_DISABLE_AUTOUPDATE": "1",
            "OPENCODE_DISABLE_MODELS_FETCH": "1",
            "OPENCODE_DISABLE_LSP_DOWNLOAD": "1",
        },
        egress=guard,
        labels={"a2a.sample": "adk-sandbox-coder"},
        idle_timeout=SANDBOX_IDLE,
    ),
    session_cwd="/workspace",
    # The sandbox contains what the agent does, so it may run anything.
    allow_tools=["*"],
    tools=[FunctionTool(file_ticket)],
)
