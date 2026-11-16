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

"""Translates OpenAI chat completions to ADK model calls and back.

Coding agents such as opencode speak the OpenAI chat completions API to
any provider they are configured with. `to_llm_request` turns such a
request into an ADK `LlmRequest`, so that an ADK model answers it, and
`to_completion` and `to_stream` turn the model's `LlmResponse` into the
answer the agent expects.
"""

from __future__ import annotations

import base64
import json
import time
import uuid
from typing import Any

from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.genai import types


class BadRequest(ValueError):
    """A request the translation can't take."""


# Gemini 3 needs the thought signature of each function call when the call
# comes back in the history. A chat completion has no place for it, so it
# rides in the tool call's ID, which the client sends back as it got it.
_SIGNATURE = "__sig_"


def _call_id(base: str, signature: bytes | None) -> str:
    if not signature:
        return base
    return base + _SIGNATURE + base64.urlsafe_b64encode(signature).decode().rstrip("=")


def _split_call_id(call_id: str) -> tuple[str, bytes | None]:
    base, sep, encoded = call_id.partition(_SIGNATURE)
    if not sep:
        return call_id, None
    try:
        return base, base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4))
    except ValueError:
        return call_id, None


def to_llm_request(body: dict[str, Any], model: str) -> LlmRequest:
    """Returns the LlmRequest of an OpenAI chat completions request for
    model, the name of the ADK model that answers it."""
    messages = body.get("messages")
    if not isinstance(messages, list) or not messages:
        raise BadRequest("the request has no messages")
    system: list[str] = []
    contents: list[types.Content] = []
    names: dict[str, str] = {}
    for m in messages:
        role = m.get("role")
        if role in ("system", "developer"):
            system.append(_text_of(m.get("content")))
        elif role == "user":
            contents.append(
                types.Content(role="user", parts=_parts_of(m.get("content")))
            )
        elif role == "assistant":
            parts = [types.Part(text=t) for t in [_text_of(m.get("content"))] if t]
            for call in m.get("tool_calls") or []:
                fn = call.get("function") or {}
                base, signature = _split_call_id(call.get("id", ""))
                names[call.get("id", "")] = fn.get("name", "")
                parts.append(
                    types.Part(
                        function_call=types.FunctionCall(
                            id=base or None,
                            name=fn.get("name"),
                            args=_arguments(fn.get("arguments")),
                        ),
                        thought_signature=signature,
                    )
                )
            if parts:
                contents.append(types.Content(role="model", parts=parts))
        elif role == "tool":
            call_id = m.get("tool_call_id", "")
            part = types.Part(
                function_response=types.FunctionResponse(
                    id=_split_call_id(call_id)[0] or None,
                    name=names.get(call_id, m.get("name", "")),
                    response={"result": _text_of(m.get("content"))},
                )
            )
            # The responses to one turn of calls make one content.
            if (
                contents
                and contents[-1].role == "user"
                and all(p.function_response for p in contents[-1].parts or [])
            ):
                contents[-1].parts.append(part)
            else:
                contents.append(types.Content(role="user", parts=[part]))
        else:
            raise BadRequest(f"unknown message role {role!r}")
    config = types.GenerateContentConfig()
    if system:
        config.system_instruction = "\n\n".join(s for s in system if s)
    tools = [
        t.get("function")
        for t in body.get("tools") or []
        if t.get("type") == "function"
    ]
    if tools:
        config.tools = [
            types.Tool(
                function_declarations=[
                    types.FunctionDeclaration(
                        name=f["name"],
                        description=f.get("description"),
                        parameters_json_schema=f.get("parameters")
                        or {"type": "object"},
                    )
                    for f in tools
                ]
            )
        ]
        choice = body.get("tool_choice")
        if choice == "none":
            config.tool_config = types.ToolConfig(
                function_calling_config=types.FunctionCallingConfig(
                    mode=types.FunctionCallingConfigMode.NONE
                )
            )
        elif choice == "required":
            config.tool_config = types.ToolConfig(
                function_calling_config=types.FunctionCallingConfig(
                    mode=types.FunctionCallingConfigMode.ANY
                )
            )
        elif isinstance(choice, dict) and (choice.get("function") or {}).get("name"):
            config.tool_config = types.ToolConfig(
                function_calling_config=types.FunctionCallingConfig(
                    mode=types.FunctionCallingConfigMode.ANY,
                    allowed_function_names=[choice["function"]["name"]],
                )
            )
    if body.get("temperature") is not None:
        config.temperature = body["temperature"]
    if body.get("top_p") is not None:
        config.top_p = body["top_p"]
    max_tokens = body.get("max_completion_tokens") or body.get("max_tokens")
    if max_tokens:
        config.max_output_tokens = max_tokens
    stop = body.get("stop")
    if stop:
        config.stop_sequences = [stop] if isinstance(stop, str) else list(stop)
    return LlmRequest(model=model, contents=contents, config=config)


def _text_of(content: Any) -> str:
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    return "".join(
        c.get("text", "")
        for c in content
        if isinstance(c, dict) and c.get("type") == "text"
    )


def _parts_of(content: Any) -> list[types.Part]:
    if isinstance(content, str) or content is None:
        return [types.Part(text=content or "")]
    parts = []
    for c in content:
        if c.get("type") == "text":
            parts.append(types.Part(text=c.get("text", "")))
        elif c.get("type") == "image_url":
            url = (c.get("image_url") or {}).get("url", "")
            if url.startswith("data:") and ";base64," in url:
                mime, data = url[5:].split(";base64,", 1)
                parts.append(
                    types.Part(
                        inline_data=types.Blob(
                            mime_type=mime, data=base64.b64decode(data)
                        )
                    )
                )
            else:
                parts.append(types.Part(file_data=types.FileData(file_uri=url)))
    return parts or [types.Part(text="")]


def _arguments(arguments: Any) -> dict[str, Any]:
    if isinstance(arguments, dict):
        return arguments
    if not arguments:
        return {}
    try:
        value = json.loads(arguments)
    except json.JSONDecodeError as e:
        raise BadRequest(f"tool call arguments aren't JSON: {e}") from e
    return value if isinstance(value, dict) else {"value": value}


_FINISH = {
    types.FinishReason.STOP: "stop",
    types.FinishReason.MAX_TOKENS: "length",
    types.FinishReason.SAFETY: "content_filter",
    types.FinishReason.RECITATION: "content_filter",
    types.FinishReason.BLOCKLIST: "content_filter",
    types.FinishReason.PROHIBITED_CONTENT: "content_filter",
    types.FinishReason.SPII: "content_filter",
}


def _message(response: LlmResponse) -> tuple[str, list[dict[str, Any]]]:
    text, calls = [], []
    for p in (response.content.parts if response.content else None) or []:
        if p.thought:
            continue
        if p.text:
            text.append(p.text)
        if p.function_call:
            calls.append(
                {
                    "id": _call_id(p.function_call.id or "call_" + uuid.uuid4().hex[:24], p.thought_signature),
                    "type": "function",
                    "function": {
                        "name": p.function_call.name,
                        "arguments": json.dumps(p.function_call.args or {}),
                    },
                }
            )
    return "".join(text), calls


def _usage(response: LlmResponse) -> dict[str, Any]:
    u = response.usage_metadata
    if u is None:
        return {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
    prompt = u.prompt_token_count or 0
    # Thinking counts as completion, as OpenAI counts reasoning tokens.
    completion = (u.candidates_token_count or 0) + (u.thoughts_token_count or 0)
    usage: dict[str, Any] = {"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": u.total_token_count or prompt + completion}
    if u.thoughts_token_count:
        usage["completion_tokens_details"] = {"reasoning_tokens": u.thoughts_token_count}
    if u.cached_content_token_count:
        usage["prompt_tokens_details"] = {"cached_tokens": u.cached_content_token_count}
    return usage


def finish_of_error(error_code: str | None) -> str | None:
    """Returns the finish reason of an error code that ADK sets when the
    model ended without content, such as SAFETY, or None for other codes."""
    # FinishReason takes any string, so membership decides.
    if not error_code or error_code not in types.FinishReason.__members__:
        return None
    return _FINISH.get(types.FinishReason[error_code], "stop")


def to_completion(response: LlmResponse, model: str) -> dict[str, Any]:
    """Returns the chat completion of a model response."""
    text, calls = _message(response)
    message: dict[str, Any] = {"role": "assistant", "content": text or None}
    if calls:
        message["tool_calls"] = calls
    finish = "tool_calls" if calls else (finish_of_error(response.error_code) or _FINISH.get(response.finish_reason, "stop"))
    return {
        "id": "chatcmpl-" + uuid.uuid4().hex,
        "object": "chat.completion",
        "created": int(time.time()),
        "model": model,
        "choices": [{"index": 0, "message": message, "finish_reason": finish}],
        "usage": _usage(response),
    }


def to_stream(response: LlmResponse, model: str) -> bytes:
    """Returns a model response as the server-sent events of a streamed
    chat completion, in one go."""
    completion = to_completion(response, model)
    choice = completion["choices"][0]
    delta: dict[str, Any] = {"role": "assistant"}
    if choice["message"]["content"]:
        delta["content"] = choice["message"]["content"]
    if choice["message"].get("tool_calls"):
        delta["tool_calls"] = [
            dict(c, index=i) for i, c in enumerate(choice["message"]["tool_calls"])
        ]
    base = {
        "id": completion["id"],
        "object": "chat.completion.chunk",
        "created": completion["created"],
        "model": model,
    }
    chunks = [
        dict(base, choices=[{"index": 0, "delta": delta, "finish_reason": None}]),
        dict(
            base,
            choices=[
                {"index": 0, "delta": {}, "finish_reason": choice["finish_reason"]}
            ],
        ),
        dict(base, choices=[], usage=completion["usage"]),
    ]
    return (
        b"".join(b"data: " + json.dumps(c).encode() + b"\n\n" for c in chunks)
        + b"data: [DONE]\n\n"
    )


def error_body(message: str, code: str = "model_error") -> bytes:
    """Returns the body of an OpenAI API error."""
    return json.dumps(
        {"error": {"message": message, "type": code, "code": code}}
    ).encode()
