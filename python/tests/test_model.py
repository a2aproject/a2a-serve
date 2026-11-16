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

"""Tests of the agent's model: the translation of OpenAI chat completions,
and AcpAgent.handle_model with an ADK model."""

import json
import os
import shutil
import tempfile
import threading
import unittest
from typing import AsyncGenerator, ClassVar

from google.adk.models.base_llm import BaseLlm
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.genai import types

from a2a_serve import openai_chat

FAKEACP = os.environ.get("FAKEACP", "")


class FakeLlm(BaseLlm):
    """Answers with a text, or with a call of a tool when the request
    offers tools, and records the requests."""

    requests: ClassVar[list] = []

    async def generate_content_async(
        self, llm_request: LlmRequest, stream: bool = False
    ) -> AsyncGenerator[LlmResponse, None]:
        self.requests.append(llm_request)
        tools = llm_request.config.tools or []
        if tools and not any(
            p.function_response for c in llm_request.contents for p in c.parts or []
        ):
            name = tools[0].function_declarations[0].name
            part = types.Part(
                function_call=types.FunctionCall(name=name, args={"path": "a.txt"})
            )
        else:
            part = types.Part(text="answer from " + self.model)
        yield LlmResponse(
            content=types.Content(role="model", parts=[part]),
            finish_reason=types.FinishReason.STOP,
            usage_metadata=types.GenerateContentResponseUsageMetadata(
                prompt_token_count=3, candidates_token_count=2, total_token_count=5
            ),
        )


class TranslationTest(unittest.TestCase):
    def test_request(self):
        body = {
            "model": "ignored",
            "messages": [
                {"role": "system", "content": "be brief"},
                {"role": "user", "content": [{"type": "text", "text": "read a.txt"}]},
                {
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [
                        {
                            "id": "c1",
                            "type": "function",
                            "function": {
                                "name": "read",
                                "arguments": '{"path": "a.txt"}',
                            },
                        },
                        {
                            "id": "c2",
                            "type": "function",
                            "function": {
                                "name": "read",
                                "arguments": '{"path": "b.txt"}',
                            },
                        },
                    ],
                },
                {"role": "tool", "tool_call_id": "c1", "content": "A"},
                {"role": "tool", "tool_call_id": "c2", "content": "B"},
            ],
            "tools": [
                {
                    "type": "function",
                    "function": {
                        "name": "read",
                        "description": "Reads a file.",
                        "parameters": {
                            "type": "object",
                            "properties": {"path": {"type": "string"}},
                        },
                    },
                }
            ],
            "tool_choice": "required",
            "temperature": 0.2,
            "max_tokens": 100,
            "stop": "END",
        }
        req = openai_chat.to_llm_request(body, "gemini-test")

        self.assertEqual(req.model, "gemini-test")
        self.assertEqual(req.config.system_instruction, "be brief")
        self.assertEqual([c.role for c in req.contents], ["user", "model", "user"])
        calls = [p.function_call for p in req.contents[1].parts]
        self.assertEqual(
            [(c.id, c.name, c.args) for c in calls],
            [("c1", "read", {"path": "a.txt"}), ("c2", "read", {"path": "b.txt"})],
        )
        responses = [p.function_response for p in req.contents[2].parts]
        self.assertEqual(
            [(r.id, r.name, r.response) for r in responses],
            [("c1", "read", {"result": "A"}), ("c2", "read", {"result": "B"})],
        )
        decl = req.config.tools[0].function_declarations[0]
        self.assertEqual(
            (decl.name, decl.parameters_json_schema["properties"]),
            ("read", {"path": {"type": "string"}}),
        )
        self.assertEqual(
            req.config.tool_config.function_calling_config.mode,
            types.FunctionCallingConfigMode.ANY,
        )
        self.assertEqual(
            (
                req.config.temperature,
                req.config.max_output_tokens,
                req.config.stop_sequences,
            ),
            (0.2, 100, ["END"]),
        )

    def test_bad_requests(self):
        for body in [
            {},
            {"messages": []},
            {"messages": [{"role": "robot", "content": "x"}]},
            {
                "messages": [
                    {
                        "role": "assistant",
                        "tool_calls": [
                            {"id": "c", "function": {"name": "f", "arguments": "{"}}
                        ],
                    }
                ]
            },
        ]:
            with self.subTest(body), self.assertRaises(openai_chat.BadRequest):
                openai_chat.to_llm_request(body, "m")

    def test_completion(self):
        response = LlmResponse(
            content=types.Content(
                role="model",
                parts=[
                    types.Part(text="thinking", thought=True),
                    types.Part(text="calling"),
                    types.Part(
                        function_call=types.FunctionCall(
                            id="x1", name="read", args={"path": "a"}
                        )
                    ),
                ],
            ),
            finish_reason=types.FinishReason.STOP,
        )
        got = openai_chat.to_completion(response, "m")
        choice = got["choices"][0]

        self.assertEqual(choice["finish_reason"], "tool_calls")
        self.assertEqual(choice["message"]["content"], "calling")
        self.assertEqual(
            choice["message"]["tool_calls"],
            [
                {
                    "id": "x1",
                    "type": "function",
                    "function": {"name": "read", "arguments": '{"path": "a"}'},
                }
            ],
        )

    def test_stream(self):
        response = LlmResponse(
            content=types.Content(role="model", parts=[types.Part(text="hi")]),
            finish_reason=types.FinishReason.MAX_TOKENS,
        )
        events = openai_chat.to_stream(response, "m").decode().split("\n\n")
        chunks = [
            json.loads(e[len("data: ") :]) for e in events if e.startswith("data: {")
        ]

        self.assertEqual(events[-2], "data: [DONE]")
        self.assertEqual(
            chunks[0]["choices"][0]["delta"], {"role": "assistant", "content": "hi"}
        )
        self.assertEqual(chunks[1]["choices"][0]["finish_reason"], "length")


@unittest.skipUnless(FAKEACP, "set FAKEACP to the path of fakeacp")
class HandleModelTest(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        from google.adk.runners import InMemoryRunner

        from a2a_serve.adk import AcpAgent

        from test_adk_agent import start_fake

        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d)
        FakeLlm.requests = []
        self.calls = []

        def budget(callback_context, llm_request):
            n = callback_context.state.get("model_calls", 0) + 1
            callback_context.state["model_calls"] = n
            self.calls.append(n)
            if n > 2:
                return LlmResponse(
                    error_code="RESOURCE_EXHAUSTED", error_message="budget spent"
                )
            return None

        def tag(callback_context, llm_response):
            if llm_response.content and llm_response.content.parts[0].text:
                return LlmResponse(
                    content=types.Content(
                        role="model",
                        parts=[
                            types.Part(
                                text=llm_response.content.parts[0].text + " (checked)"
                            )
                        ],
                    )
                )
            return None

        self.agent = AcpAgent(
            name="coder",
            start=lambda key: start_fake(d),
            session_cwd=d,
            model=FakeLlm(model="fake-model"),
            before_model_callback=budget,
            after_model_callback=tag,
        )
        self.addCleanup(self.agent.close)
        self.runner = InMemoryRunner(agent=self.agent, app_name="test")
        self.session = await self.runner.session_service.create_session(
            app_name="test", user_id="u"
        )

    def post(self, body, path="/v1/chat/completions"):
        status, headers, out = self.agent.handle_model(
            self.session.id, "POST", path, json.dumps(body).encode()
        )
        return status, headers, out

    async def during_turn(self, requests):
        """Sends requests as the agent would while it works on a message,
        and returns their answers and the turn's events."""
        results = []

        def send():
            for _ in range(500):
                if self.session.id in self.agent.busy_sessions():
                    break
                threading.Event().wait(0.01)
            for r in requests:
                results.append(r())

        t = threading.Thread(target=send)
        t.start()
        events = [
            ev
            async for ev in self.runner.run_async(
                user_id="u",
                session_id=self.session.id,
                new_message=types.Content(
                    role="user", parts=[types.Part(text="slow 2s")]
                ),
            )
        ]
        t.join()
        return results, events

    async def test_model_calls_in_an_invocation(self):
        chat = {"messages": [{"role": "user", "content": "hi"}]}
        tool_chat = dict(
            chat,
            tools=[
                {
                    "type": "function",
                    "function": {"name": "read", "parameters": {"type": "object"}},
                }
            ],
            stream=True,
        )
        results, _ = await self.during_turn(
            [
                lambda: self.post(chat),
                lambda: self.post(tool_chat),
                lambda: self.post(chat),
            ]
        )
        stored = await self.runner.session_service.get_session(
            app_name="test", user_id="u", session_id=self.session.id
        )

        (s1, _, b1), (s2, h2, b2), (s3, _, b3) = results
        self.assertEqual(s1, 200)
        self.assertEqual(
            json.loads(b1)["choices"][0]["message"]["content"],
            "answer from fake-model (checked)",
        )
        self.assertEqual((s2, h2["Content-Type"]), (200, "text/event-stream"))
        self.assertIn('"tool_calls"', b2.decode())
        self.assertEqual(s3, 429)
        self.assertIn("budget spent", b3.decode())
        self.assertEqual(len(FakeLlm.requests), 2)
        self.assertEqual(FakeLlm.requests[0].model, "fake-model")
        self.assertEqual(stored.state.get("model_calls"), 3)

    async def test_requests_outside_an_invocation(self):
        tests = [
            (
                "no invocation",
                "POST",
                "/v1/chat/completions",
                {"messages": [{"role": "user", "content": "hi"}]},
                409,
            ),
            ("bad request", "POST", "/v1/chat/completions", {"messages": []}, 400),
            ("unknown route", "POST", "/v1/embeddings", {}, 404),
        ]
        for name, method, path, body, want in tests:
            with self.subTest(name):
                status, _, _ = self.agent.handle_model(
                    self.session.id, method, path, json.dumps(body).encode()
                )
                self.assertEqual(status, want)
        status, _, out = self.agent.handle_model(
            self.session.id, "GET", "/v1/models", b""
        )
        self.assertEqual(
            (status, json.loads(out)["data"][0]["id"]), (200, "fake-model")
        )


if __name__ == "__main__":
    unittest.main()
