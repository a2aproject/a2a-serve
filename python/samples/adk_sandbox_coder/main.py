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

"""Runs the sample, then shows the workspace in memory, the session state
and what the guard decided.

  python main.py                      # one turn with the default prompt
  python main.py "Fix the failing tests in this project."
  python main.py console              # a conversation, one line per message

In the console, a question of the agent, such as a permission it needs,
ends the turn, and your next line answers it.
"""

from __future__ import annotations

import asyncio
import sys

from google.adk.runners import InMemoryRunner
from google.genai import types

import agent

DEFAULT_PROMPT = (
    "Run the tests with `python3 -m unittest -q`, fix the bugs in stats.py until they pass, "
    "then file one ticket with file_ticket for a missing test you would add. "
    "Reply with a one-paragraph summary and the ticket ID."
)


async def turn(runner: InMemoryRunner, session_id: str, prompt: str) -> None:
    """Sends prompt and prints the turn."""
    message = types.Content(role="user", parts=[types.Part(text=prompt)])
    async for event in runner.run_async(user_id="user", session_id=session_id, new_message=message):
        if event.partial:
            continue
        if event.error_message:
            # The content repeats the error.
            print(f"[error] {event.error_message}")
            continue
        for part in (event.content.parts if event.content else []):
            if part.function_call:
                print(f"[adk tool call] {part.function_call.name}({part.function_call.args})")
            elif part.function_response:
                print(f"[adk tool result] {part.function_response.response}")
            elif part.text:
                print(f"Agent -> {part.text}\n")
        tool = (event.custom_metadata or {}).get("acp_tool_call")
        if tool:
            print(f"[agent tool] {tool.get('title')} ({tool.get('status')})")


async def main(args: list[str]) -> None:
    runner = InMemoryRunner(agent=agent.root_agent, app_name="adk_sandbox_coder")
    session = await runner.session_service.create_session(app_name="adk_sandbox_coder", user_id="user")
    try:
        if args == ["console"]:
            while True:
                try:
                    line = await asyncio.to_thread(input, "User -> ")
                except EOFError:
                    print()
                    break
                if line.strip():
                    await turn(runner, session.id, line)
        else:
            prompt = " ".join(args) or DEFAULT_PROMPT
            print(f"User -> {prompt}\n")
            await turn(runner, session.id, prompt)

        sandbox = agent.root_agent.sandbox_of(session.id)
        if sandbox is None:
            return
        print("\nWorkspace in memory:")
        for name in sandbox.list_files():
            print(f"  {name}")
        print("\nstats.py after the run:\n" + sandbox.read_file("stats.py").decode())
        result = sandbox.exec(["python3", "-m", "unittest", "-q"])
        print(f"Tests in the sandbox: exit {result.exit_code}\n{result.stderr.decode().strip()}\n")
        final = await runner.session_service.get_session(app_name="adk_sandbox_coder", user_id="user", session_id=session.id)
        print(f"Tickets in the session state: {final.state.get('tickets')}")
        print(f"Model calls in the session state: {final.state.get('model_calls')}\n")
        print("Guard log:")
        for line in agent.guard.log:
            print(f"  {line}")
    finally:
        agent.root_agent.close()


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1:]))
