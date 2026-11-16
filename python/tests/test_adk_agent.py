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

"""Tests of AcpAgent with the scripted fakeacp agent, after the tests of the
acpagent prototype of adk-go (google/adk-go#2021). See test_bindings.py for
how to build fakeacp and the libraries."""

import asyncio
import json
import os
import shutil
import socket
import subprocess
import tempfile
import threading
import types as pytypes
import unittest

FAKEACP = os.environ.get("FAKEACP", "")


def start_fake(cwd):
    """Starts fakeacp on one end of a socket pair and returns the other."""
    parent, child = socket.socketpair()
    proc = subprocess.Popen([FAKEACP], cwd=cwd, stdin=child, stdout=child)
    child.close()
    threading.Thread(target=proc.wait, daemon=True).start()
    return parent.detach()


def start_recording_fake(cwd, methods):
    """Starts fakeacp like start_fake and appends the method of every
    request the agent is sent to methods."""
    parent, child = socket.socketpair()
    proc = subprocess.Popen([FAKEACP], cwd=cwd, stdin=subprocess.PIPE, stdout=child)

    def relay():
        with child.makefile("rb") as r:
            for line in r:
                methods.append(json.loads(line).get("method", ""))
                proc.stdin.write(line)
                proc.stdin.flush()
        proc.stdin.close()

    threading.Thread(target=relay, daemon=True).start()
    threading.Thread(target=proc.wait, daemon=True).start()
    return parent.detach()


def message(text):
    from google.genai import types

    return types.Content(role="user", parts=[types.Part(text=text)])


@unittest.skipUnless(FAKEACP, "set FAKEACP to the path of fakeacp")
class AgentTest(unittest.IsolatedAsyncioTestCase):
    def agent(self, **kwargs):
        from google.adk.runners import InMemoryRunner

        from a2a_serve.adk import AcpAgent

        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d)
        if kwargs.get("tools") or kwargs.get("model") is not None:
            # A test stands in for the interceptor that would route the
            # agent's requests, so the agent starts through start.
            agent = AcpAgent(name="coder", start=lambda key: start_fake(d), session_cwd=d, **kwargs)
        else:
            agent = AcpAgent(name="coder", argv=[FAKEACP], session_cwd=d, **kwargs)
        self.addCleanup(agent.close)
        return agent, InMemoryRunner(agent=agent, app_name="test")

    async def session(self, runner):
        return await runner.session_service.create_session(app_name="test", user_id="u")

    async def turn(self, runner, session, text, on_partial=None, run_config=None):
        """Runs one invocation and returns its final events. on_partial, if
        set, runs once the invocation works, as the agent would call a tool."""
        if on_partial is not None:
            agent = runner.agent

            def when_busy():
                for _ in range(500):
                    if session.id in agent.busy_sessions():
                        on_partial()
                        return
                    threading.Event().wait(0.01)

            t = threading.Thread(target=when_busy)
            t.start()
            self.addCleanup(t.join)
        events = []
        async for ev in runner.run_async(user_id="u", session_id=session.id, new_message=message(text), run_config=run_config):
            if not ev.partial:
                events.append(ev)
        return events

    @staticmethod
    def texts(events):
        return [
            p.text for ev in events if ev.content for p in ev.content.parts if p.text
        ]

    async def test_conversation(self):
        _, runner = self.agent()
        first, second = await self.session(runner), await self.session(runner)

        await self.turn(runner, first, "remember blue")
        await self.turn(runner, second, "remember green")
        got = self.texts(await self.turn(runner, first, "recall"))

        self.assertEqual(len(got), 1)
        self.assertIn("blue", got[0])
        self.assertNotIn("green", got[0])

    async def test_abandoned_run_cancels_agent_turn(self):
        from google.adk.agents.run_config import RunConfig, StreamingMode
        from google.adk.runners import InMemoryRunner

        from a2a_serve.adk import AcpAgent

        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d)
        methods = []
        agent = AcpAgent(name="coder", start=lambda key: start_recording_fake(d, methods), session_cwd=d)
        self.addCleanup(agent.close)
        runner = InMemoryRunner(agent=agent, app_name="test")
        session = await self.session(runner)

        events = runner.run_async(user_id="u", session_id=session.id, new_message=message("slow 60s"), run_config=RunConfig(streaming_mode=StreamingMode.SSE))
        async for ev in events:
            if ev.partial:
                break
        await events.aclose()

        for _ in range(500):
            if "session/cancel" in methods:
                break
            await asyncio.sleep(0.01)
        self.assertIn("session/cancel", methods)

    async def test_last_event_completes_the_turn(self):
        tests = [("completed", "hello"), ("question", "ask"), ("failed", "crash")]
        for name, text in tests:
            with self.subTest(name):
                _, runner = self.agent()
                session = await self.session(runner)

                got = await self.turn(runner, session, text)

                self.assertEqual([bool(ev.turn_complete) for ev in got], [False] * (len(got) - 1) + [True])

    async def test_permissions(self):
        tests = [
            ("allowed kind", {"allow_tools": ["execute"]}, "Pushed (allow)."),
            (
                "denied kind",
                {"allow_tools": ["*"], "deny_tools": ["execute"]},
                "Push denied.",
            ),
        ]
        for name, kwargs, want in tests:
            with self.subTest(name):
                _, runner = self.agent(**kwargs)
                session = await self.session(runner)
                self.assertEqual(
                    self.texts(await self.turn(runner, session, "ask")), [want]
                )

    async def test_answer_in_later_invocation(self):
        _, runner = self.agent()
        session = await self.session(runner)

        asked = await self.turn(runner, session, "ask")
        answered = await self.turn(runner, session, "once")

        self.assertTrue(
            any((ev.custom_metadata or {}).get("acp_input_required") for ev in asked),
            asked,
        )
        self.assertIn("git push", "\n".join(self.texts(asked)))
        self.assertEqual(self.texts(answered), ["Pushed (allow)."])

    async def test_reject_answer(self):
        _, runner = self.agent()
        session = await self.session(runner)

        await self.turn(runner, session, "ask")
        rejected = await self.turn(runner, session, "reject")
        after = await self.turn(runner, session, "hello")

        self.assertEqual(self.texts(rejected), ["Push denied."])
        self.assertEqual(self.texts(after), ["Hello from fakeacp."])

    async def test_failure_reports_error_after_streamed_text(self):
        _, runner = self.agent()
        session = await self.session(runner)

        got = await self.turn(runner, session, "crash")
        after = await self.turn(runner, session, "hello")

        self.assertEqual(self.texts(got), ["about to crash", "acp agent error: acp agent exited"])
        self.assertEqual([ev.error_code for ev in got], [None, None])
        self.assertEqual(got[1].error_message, "acp agent error: acp agent exited")
        self.assertEqual(self.texts(after), ["Hello from fakeacp."])

    async def test_streaming_modes(self):
        from google.adk.agents.run_config import RunConfig, StreamingMode

        _, runner = self.agent()
        for mode, want_partial in [(StreamingMode.NONE, False), (StreamingMode.SSE, True)]:
            with self.subTest(mode):
                session = await self.session(runner)
                events = [ev async for ev in runner.run_async(user_id="u", session_id=session.id, new_message=message("hello"), run_config=RunConfig(streaming_mode=mode))]

                self.assertEqual(any(ev.partial for ev in events), want_partial)
                self.assertEqual(self.texts([ev for ev in events if not ev.partial]), ["Hello from fakeacp."])

    async def test_tool_error(self):
        from google.adk.tools import FunctionTool

        def broken(title: str) -> dict:
            """Always fails."""
            raise RuntimeError("tracker down")

        agent, runner = self.agent(tools=[FunctionTool(broken)])
        session = await self.session(runner)
        results = []

        def call():
            body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "broken", "arguments": {"title": "x"}}})
            t = threading.Thread(target=lambda: results.append(json.loads(agent.handle_mcp(session.id, "POST", body.encode())[2])["result"]))
            t.start()
            self.addCleanup(t.join)

        await self.turn(runner, session, "slow 2s", on_partial=call)

        self.assertTrue(results[0].get("isError"), results[0])
        self.assertIn("tracker down", results[0]["content"][0]["text"])

    async def test_tools_in_later_invocations(self):
        agent, runner = self.agent(tools=[self.ticket_tool()])
        session = await self.session(runner)
        results = []

        def call(title):
            def start():
                t = threading.Thread(target=lambda: results.append(self.call_tool(agent, session.id, title)))
                t.start()
                self.addCleanup(t.join)

            return start

        await self.turn(runner, session, "slow 2s", on_partial=call("first"))
        await self.turn(runner, session, "slow 2s", on_partial=call("second"))
        stored = await runner.session_service.get_session(app_name="test", user_id="u", session_id=session.id)

        self.assertEqual([r["structuredContent"] for r in results], [{"id": "T-1"}, {"id": "T-2"}])
        self.assertEqual(stored.state.get("tickets"), ["first", "second"])

    def ticket_tool(self):
        from google.adk.tools import FunctionTool
        from google.adk.tools.tool_context import ToolContext

        def file_ticket(title: str, tool_context: ToolContext) -> dict:
            """Files a ticket and returns its ID."""
            tickets = list(tool_context.state.get("tickets", []))
            tickets.append(title)
            tool_context.state["tickets"] = tickets
            return {"id": f"T-{len(tickets)}"}

        return FunctionTool(file_ticket)

    def call_tool(self, agent, session_id, title):
        body = json.dumps(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "tools/call",
                "params": {"name": "file_ticket", "arguments": {"title": title}},
            }
        )
        _, _, out = agent.handle_mcp(session_id, "POST", body.encode())
        return json.loads(out)["result"]

    async def test_tools(self):
        agent, runner = self.agent(tools=[self.ticket_tool()])
        session = await self.session(runner)
        results = []

        def call():
            # The agent would call the tool over MCP while it works.
            t = threading.Thread(
                target=lambda: results.append(
                    self.call_tool(agent, session.id, "fix the build")
                )
            )
            t.start()
            self.addCleanup(t.join)

        events = await self.turn(runner, session, "slow 2s", on_partial=call)
        stored = await runner.session_service.get_session(
            app_name="test", user_id="u", session_id=session.id
        )

        self.assertEqual(results[0]["structuredContent"], {"id": "T-1"})
        calls = [
            p.function_call
            for ev in events
            if ev.content
            for p in ev.content.parts
            if p.function_call
        ]
        responses = [
            p.function_response
            for ev in events
            if ev.content
            for p in ev.content.parts
            if p.function_response
        ]
        self.assertEqual(
            [(c.name, c.args) for c in calls],
            [("file_ticket", {"title": "fix the build"})],
        )
        self.assertEqual([r.response for r in responses], [{"id": "T-1"}])
        self.assertEqual(stored.state.get("tickets"), ["fix the build"])

    async def test_tools_abandoned_run_keeps_session(self):
        from google.adk.agents.run_config import RunConfig, StreamingMode

        agent, runner = self.agent(tools=[self.ticket_tool()])
        session = await self.session(runner)

        events = runner.run_async(
            user_id="u",
            session_id=session.id,
            new_message=message("slow 60s"),
            run_config=RunConfig(streaming_mode=StreamingMode.SSE),
        )
        async for ev in events:
            if ev.partial:
                break
        await events.aclose()
        late = await asyncio.to_thread(self.call_tool, agent, session.id, "late")
        again = self.texts(await self.turn(runner, session, "hello"))

        self.assertTrue(late["isError"], late)
        self.assertEqual(again, ["Hello from fakeacp."])

    async def test_tools_per_session(self):
        agent, runner = self.agent(tools=[self.ticket_tool()])
        working, idle = await self.session(runner), await self.session(runner)
        results = []

        def call():
            t = threading.Thread(
                target=lambda: results.append(
                    (
                        self.call_tool(agent, idle.id, "idle"),
                        self.call_tool(agent, working.id, "working"),
                    )
                )
            )
            t.start()
            self.addCleanup(t.join)

        await self.turn(runner, working, "slow 2s", on_partial=call)
        stored_working = await runner.session_service.get_session(
            app_name="test", user_id="u", session_id=working.id
        )
        stored_idle = await runner.session_service.get_session(
            app_name="test", user_id="u", session_id=idle.id
        )

        idle_result, working_result = results[0]
        self.assertTrue(idle_result["isError"], idle_result)
        self.assertEqual(working_result["structuredContent"], {"id": "T-1"})
        self.assertEqual(stored_working.state.get("tickets"), ["working"])
        self.assertIsNone(stored_idle.state.get("tickets"))


class ValidationTest(unittest.TestCase):
    def test_config(self):
        from google.adk.tools import FunctionTool

        from a2a_serve.adk import AcpAgent

        def tool() -> dict:
            """Does nothing."""
            return {}

        for name, kwargs in [
            ("neither argv nor start", {}),
            ("both argv and start", {"argv": ["agent"], "start": lambda key: 0}),
            ("tools without an interceptor", {"argv": ["agent"], "tools": [FunctionTool(tool)]}),
            (
                "duplicate tools",
                {"argv": ["agent"], "tools": [FunctionTool(tool), FunctionTool(tool)]},
            ),
        ]:
            with self.subTest(name), self.assertRaises(ValueError):
                AcpAgent(name="coder", **kwargs)


class UnseenTest(unittest.TestCase):
    """The agent gets what it wasn't sent: what others said since its last
    turn, and the user's message."""

    def event(self, author, text, partial=False):
        from google.adk.events.event import Event

        return Event(author=author, content=message(text), partial=partial)

    def test_unseen(self):
        from a2a_serve.adk import AcpAgent

        agent = AcpAgent(name="coder", argv=["agent"])
        history = [
            self.event("user", "plan the work"),
            self.event("coder", "done before"),
            self.event("planner", "split it in two"),
            self.event("planner", "draft", partial=True),
            self.event("user", "go"),
        ]
        ctx = pytypes.SimpleNamespace(
            session=pytypes.SimpleNamespace(id="s", events=history),
            user_content=message("go"),
            branch=None,
        )

        text, through = agent._unseen(ctx)
        self.assertEqual(text, "[planner] split it in two\n\ngo")
        agent._delivered_to("s", through)
        history.append(self.event("user", "and test it"))
        ctx.user_content = message("and test it")
        self.assertEqual(agent._unseen(ctx)[0], "and test it")

    def test_unseen_in_a_workflow(self):
        from a2a_serve.adk import AcpAgent

        agent = AcpAgent(name="coder", argv=["agent"])
        other_branch = self.event("critic", "not for you")
        other_branch.branch = "review.critic"
        history = [self.event("user", "fix the bug"), self.event("planner", "step 1"), other_branch]
        ctx = pytypes.SimpleNamespace(
            session=pytypes.SimpleNamespace(id="s", events=history),
            user_content=message("fix the bug"),
            branch="work.coder",
        )

        self.assertEqual(agent._unseen(ctx)[0], "fix the bug\n\n[planner] step 1")


if __name__ == "__main__":
    unittest.main()
