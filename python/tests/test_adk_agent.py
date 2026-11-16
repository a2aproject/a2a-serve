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
import tempfile
import threading
import types as pytypes
import unittest

FAKEACP = os.environ.get("FAKEACP", "")


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
        agent = AcpAgent(name="coder", argv=[FAKEACP], session_cwd=d, **kwargs)
        self.addCleanup(agent.close)
        return agent, InMemoryRunner(agent=agent, app_name="test")

    async def session(self, runner):
        return await runner.session_service.create_session(app_name="test", user_id="u")

    async def turn(self, runner, session, text, on_partial=None):
        """Runs one invocation and returns its final events."""
        events = []
        async for ev in runner.run_async(
            user_id="u", session_id=session.id, new_message=message(text)
        ):
            if ev.partial:
                if on_partial is not None:
                    on_partial()
                    on_partial = None
                continue
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
        agent, runner = self.agent(tools=[self.ticket_tool()])
        session = await self.session(runner)

        events = runner.run_async(
            user_id="u", session_id=session.id, new_message=message("slow 60s")
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
        )

        self.assertEqual(agent._unseen(ctx), "[planner] split it in two\n\ngo")
        history.append(self.event("user", "and test it"))
        ctx.user_content = message("and test it")
        self.assertEqual(agent._unseen(ctx), "and test it")


if __name__ == "__main__":
    unittest.main()
