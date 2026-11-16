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

"""Tests of the bindings with the scripted fakeacp agent.

Build the libraries with clib/build.sh and fakeacp with

  CGO_ENABLED=0 go build -o /tmp/fakeacp ./internal/agentsrv/testdata/fakeacp

then run, from python/,

  FAKEACP=/tmp/fakeacp python -m unittest discover -s tests

FAKEACP_IMAGE names an image with fakeacp at /usr/local/bin/fakeacp, such as
the one the e2e tests build, for the tests of the Docker sandbox.
"""

import asyncio
import json
import os
import shutil
import tempfile
import threading
import time
import unittest

from a2a_serve import Executor, NativeError, Sandbox, respond
from a2a_serve.sandbox import short_name

FAKEACP = os.environ.get("FAKEACP", "")
FAKEACP_IMAGE = os.environ.get("FAKEACP_IMAGE", "")


def reply_of(events):
    text, state = "", ""
    for ev in events:
        if "artifactUpdate" in ev:
            text += "".join(p.get("text", "") for p in ev["artifactUpdate"]["artifact"]["parts"])
        if "statusUpdate" in ev:
            state = ev["statusUpdate"]["status"]["state"]
    return text, state


@unittest.skipUnless(FAKEACP, "set FAKEACP to the path of fakeacp")
class ExecutorTest(unittest.TestCase):
    def test_process(self):
        with tempfile.TemporaryDirectory() as d:
            ex = Executor(argv=[FAKEACP], cwd=d)
            try:
                self.assertEqual(reply_of(ex.stream("hello")), ("Hello from fakeacp.", "TASK_STATE_COMPLETED"))
                self.assertEqual(reply_of(ex.stream("remember blue", context_id="c1"))[1], "TASK_STATE_COMPLETED")
                self.assertIn("blue", reply_of(ex.stream("recall", context_id="c1"))[0])
            finally:
                ex.close()

    def test_bad_config(self):
        with self.assertRaises(ValueError):
            Executor()


@unittest.skipUnless(FAKEACP, "set FAKEACP to the path of fakeacp")
class AcpAgentTest(unittest.IsolatedAsyncioTestCase):
    async def test_runs_in_adk(self):
        from google.adk.runners import InMemoryRunner
        from google.genai import types

        from a2a_serve.adk import AcpAgent

        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d)
        agent = AcpAgent(name="coder", argv=[FAKEACP], session_cwd=d)
        self.addCleanup(agent.close)
        runner = InMemoryRunner(agent=agent, app_name="test")
        session = await runner.session_service.create_session(app_name="test", user_id="u")
        texts = []
        async for ev in runner.run_async(user_id="u", session_id=session.id, new_message=types.Content(role="user", parts=[types.Part(text="hello")])):
            if not ev.partial and ev.content and ev.content.parts:
                texts.append(ev.content.parts[0].text)
        self.assertEqual(texts, ["Hello from fakeacp."])

    async def test_abandoned_run_cancels_its_task(self):
        from google.adk.runners import InMemoryRunner
        from google.genai import types

        from a2a_serve.adk import AcpAgent

        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d)
        agent = AcpAgent(name="coder", argv=[FAKEACP], session_cwd=d)
        self.addCleanup(agent.close)
        runner = InMemoryRunner(agent=agent, app_name="test")
        session = await runner.session_service.create_session(app_name="test", user_id="u")

        def message(text):
            return types.Content(role="user", parts=[types.Part(text=text)])

        events = runner.run_async(user_id="u", session_id=session.id, new_message=message("slow 60s"))
        async for ev in events:
            if ev.partial:
                break
        await events.aclose()
        started = asyncio.get_running_loop().time()
        texts = []
        async for ev in runner.run_async(user_id="u", session_id=session.id, new_message=message("hello")):
            if not ev.partial and ev.content and ev.content.parts:
                texts.append(ev.content.parts[0].text)
        self.assertEqual(texts, ["Hello from fakeacp."])
        self.assertLess(asyncio.get_running_loop().time() - started, 30)


class FakeExecutor:
    """Streams one task event after a delay, then waits until stopped."""

    def __init__(self):
        self.cancelled = []

    def stream(self, text, *, context_id="", task_id="", stop=None):
        time.sleep(0.2)
        yield {"task": {"id": "t1", "status": {"state": "TASK_STATE_WORKING"}}}
        while not stop():
            time.sleep(0.01)

    def cancel(self, task_id):
        self.cancelled.append(task_id)


class RunTest(unittest.IsolatedAsyncioTestCase):
    async def test_abandoned_before_its_first_event_cancels_its_task(self):
        from a2a_serve.adk import _Run

        run = _Run(None, None)
        executor = FakeExecutor()
        pump = threading.Thread(target=run.pump, args=(executor, "slow", "s1", ""))
        pump.start()
        run.abandon(executor)
        await asyncio.to_thread(pump.join, 10)
        self.assertFalse(pump.is_alive())
        self.assertEqual(executor.cancelled, ["t1"])


class McpTest(unittest.TestCase):
    def setUp(self):
        from google.adk.tools import FunctionTool

        from a2a_serve.adk import AcpAgent

        def add(a: int, b: int) -> int:
            """Adds two numbers."""
            return a + b

        self.agent = AcpAgent(name="coder", argv=["unused"], tools=[FunctionTool(add)])

    def call(self, msg):
        status, headers, body = self.agent.handle_mcp("s1", "POST", json.dumps(msg).encode())
        return status, json.loads(body) if body else None

    def test_lists_tools(self):
        status, resp = self.call({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
        self.assertEqual(status, 200)
        tool = resp["result"]["tools"][0]
        self.assertEqual((tool["name"], tool["inputSchema"]["required"]), ("add", ["a", "b"]))

    def test_call_without_invocation(self):
        _, resp = self.call({"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "add", "arguments": {"a": 1, "b": 2}}})
        self.assertTrue(resp["result"]["isError"])

    def test_notification_and_get(self):
        self.assertEqual(self.call({"jsonrpc": "2.0", "method": "notifications/initialized"}), (202, None))
        self.assertEqual(self.agent.handle_mcp("s1", "GET", b"")[0], 405)


class Intercept:
    def __init__(self):
        self.seen = []

    def connect(self, host, port):
        self.seen.append(f"{host}:{port}")
        return "deny"

    def request(self, req):
        return respond(200, "unused")


@unittest.skipUnless(FAKEACP_IMAGE and shutil.which("docker"), "set FAKEACP_IMAGE and install Docker")
class SandboxTest(unittest.TestCase):
    def test_memory_workspace_with_acp(self):
        boxes = []

        def start(key):
            sb = Sandbox(short_name("a2a-pytest"), FAKEACP_IMAGE, ["/usr/local/bin/fakeacp"], cwd="/workspace",
                         memory_dir="/workspace", files={"seed.txt": "seeded"}, interceptor=Intercept())
            boxes.append(sb)
            return sb.start_fd()

        ex = Executor(start=start, per_context=True, session_cwd="/workspace")
        try:
            self.assertEqual(reply_of(ex.stream("read seed.txt", context_id="c"))[0], "seeded")
            self.assertEqual(reply_of(ex.stream("write out.txt", context_id="c"))[0], "write ok")
            self.assertIn("out.txt", boxes[0].list_files())
            self.assertEqual(boxes[0].read_file("out.txt"), b"written by fakeacp\n")
            with self.assertRaises(NativeError):
                boxes[0].read_file("/etc/passwd")
        finally:
            ex.close()
            for b in boxes:
                b.close()


if __name__ == "__main__":
    unittest.main()
