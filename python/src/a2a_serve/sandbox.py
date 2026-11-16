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

"""Python bindings of the a2a-serve sandbox library (liba2asandbox.so).

A sandbox runs a command in a Docker container. Its connections can go
through the egress boundary, where an `Interceptor` decides each one in
Python, and the requests of the connections it inspects. Its working
directory can live in memory, seeded and read back from Python.
"""

from __future__ import annotations

import base64
import ctypes
import dataclasses
import json
import os
import socket
import threading
from typing import Any, Protocol

from . import _native

_SIGNATURES = {
    "open": [ctypes.c_char_p],
    "start": [ctypes.c_int64],
    "close": [ctypes.c_int64],
    "stderr": [ctypes.c_int64],
    "read_file": [ctypes.c_int64, ctypes.c_char_p],
    "write_file": [ctypes.c_int64, ctypes.c_char_p, ctypes.c_char_p],
    "list": [ctypes.c_int64, ctypes.c_char_p],
    "exec": [ctypes.c_int64, ctypes.c_char_p],
    "next_event": [ctypes.c_int32],
    "reply": [ctypes.c_int64, ctypes.c_char_p],
}

_lib: _native.Library | None = None
_pump: _native.EventPump | None = None
_interceptors: dict[int, "Interceptor"] = {}
_lock = threading.Lock()


def _library() -> _native.Library:
    global _lib, _pump
    with _lock:
        if _lib is None:
            _lib = _native.Library("liba2asandbox.so", "a2a_sandbox_", _SIGNATURES)
            _pump = _native.EventPump(_lib, _dispatch)
        return _lib


@dataclasses.dataclass
class Request:
    """An HTTP request of an inspected connection."""

    sandbox: int
    method: str
    url: str
    headers: dict[str, list[str]]
    body: bytes

    def header(self, name: str) -> str:
        for k, v in self.headers.items():
            if k.lower() == name.lower() and v:
                return v[0]
        return ""


class Interceptor(Protocol):
    """Decides the connections of a sandbox, and the requests it inspects."""

    def connect(self, host: str, port: int) -> str:
        """Returns allow, deny, inspect, or policy to leave it to the rules."""
        ...

    def request(self, req: Request) -> dict[str, Any]:
        """Returns respond(), forward() or deny()."""
        ...


def respond(
    status: int = 200, body: bytes | str = b"", headers: dict[str, str] | None = None
) -> dict[str, Any]:
    """Answers a request without sending it on."""
    if isinstance(body, str):
        body = body.encode()
    return {
        "action": "respond",
        "status": status,
        "headers": {k: [v] for k, v in (headers or {}).items()},
        "body": base64.b64encode(body).decode(),
    }


def forward(
    url: str = "",
    set_headers: dict[str, str] | None = None,
    remove_headers: list[str] | None = None,
    body: bytes | None = None,
) -> dict[str, Any]:
    """Sends the request on, to url when it is set, with changed headers."""
    answer: dict[str, Any] = {
        "action": "forward",
        "url": url,
        "set_headers": set_headers or {},
        "remove_headers": remove_headers or [],
    }
    if body is not None:
        answer["body"] = base64.b64encode(body).decode()
    return answer


def deny(reason: str = "blocked by the sandbox", status: int = 403) -> dict[str, Any]:
    """Refuses a request with status and reason as its body."""
    return {
        "action": "deny",
        "status": status,
        "body": base64.b64encode(reason.encode()).decode(),
    }


def _dispatch(kind: str, data: dict) -> dict:
    interceptor = _interceptors.get(data["sandbox"])
    if interceptor is None:
        return (
            {"action": "deny"}
            if kind == "connect"
            else deny("the sandbox has no interceptor")
        )
    if kind == "connect":
        return {"action": interceptor.connect(data["host"], data["port"])}
    body = base64.b64decode(data.get("body") or "")
    return interceptor.request(
        Request(
            data["sandbox"],
            data["method"],
            data["url"],
            data.get("headers") or {},
            body,
        )
    )


@dataclasses.dataclass
class ExecResult:
    stdout: bytes
    stderr: bytes
    exit_code: int
    truncated: bool = False
    """Set when a stream had more output than the library keeps."""


class Sandbox:
    """A command in a Docker container, with an optional memory workspace."""

    def __init__(
        self,
        name: str,
        image: str,
        argv: list[str],
        *,
        cwd: str = "",
        env: dict[str, str] | None = None,
        memory_dir: str = "",
        memory_size: str = "",
        files: dict[str, bytes | str] | None = None,
        network: str | None = "allowlist",
        allow: list[str] | None = None,
        interceptor: Interceptor | None = None,
        inspect_tls: list[str] | None = None,
        labels: dict[str, str] | None = None,
        limits: dict[str, Any] | None = None,
    ):
        """Prepares the sandbox, without starting its command.

        Args:
          name: the name of the container, unique on the host.
          image: the image the container runs.
          argv: the command and its arguments.
          cwd: the working directory in the container.
          env: the environment of the command.
          memory_dir: where the container mounts a file system in memory.
          memory_size: bounds the file system in memory, such as "256m".
          files: the files to write into the file system in memory, by their
            paths relative to memory_dir.
          network: internet, allowlist or None for the runtime's own network.
          allow: the rules of the allowlist, as --egress-allow takes them.
          interceptor: decides every connection, and the requests of those it
            inspects.
          inspect_tls: the hosts whose TLS an inspected connection terminates
            with a CA the sandbox trusts.
          labels: the labels of the containers.
          limits: memory, cpus and pids of the container.
        """
        lib = _library()
        spec: dict[str, Any] = {
            "name": name,
            "image": image,
            "command": {
                "path": argv[0],
                "args": argv[1:],
                "dir": cwd,
                "env": [f"{k}={v}" for k, v in (env or {}).items()],
            },
            "labels": labels or {},
            "limits": limits or {},
            "launcher": _native.launcher_path(),
        }
        if memory_dir:
            spec["memory"] = {
                "dir": memory_dir,
                "size": memory_size,
                "files": {
                    k: base64.b64encode(
                        v.encode() if isinstance(v, str) else v
                    ).decode()
                    for k, v in (files or {}).items()
                },
            }
        if network is not None:
            spec["network"] = {
                "mode": network,
                "allow": allow or [],
                "intercept": interceptor is not None,
                "inspect_tls": inspect_tls or [],
            }
        self._interceptor = interceptor
        result = lib.call("open", json.dumps(spec))
        self.handle: int = result["handle"]
        self.info: dict[str, Any] = result["info"]
        if interceptor is not None:
            _interceptors[self.handle] = interceptor

    def start(self) -> socket.socket:
        """Starts the command and returns a socket that carries its stdio.

        Closing the socket stops the command. Use fileno() and detach() to
        hand it to the ACP library.
        """
        fd = _library().call("start", self.handle)["fd"]
        return socket.socket(fileno=fd)

    def start_fd(self) -> int:
        """Starts the command and returns the descriptor of its stdio."""
        return _library().call("start", self.handle)["fd"]

    def stderr(self) -> str:
        return _library().call("stderr", self.handle)["stderr"]

    def read_file(self, name: str) -> bytes:
        return base64.b64decode(
            _library().call("read_file", self.handle, name)["data"] or ""
        )

    def write_file(self, name: str, data: bytes | str) -> None:
        if isinstance(data, str):
            data = data.encode()
        _library().call(
            "write_file", self.handle, name, base64.b64encode(data).decode()
        )

    def list_files(self, directory: str = ".") -> list[str]:
        return sorted(_library().call("list", self.handle, directory)["files"])

    def exec(
        self, argv: list[str], stdin: bytes = b"", timeout: float = 120
    ) -> ExecResult:
        """Runs argv next to the command, which must run."""
        req = {
            "argv": argv,
            "stdin": base64.b64encode(stdin).decode(),
            "timeout_ms": int(timeout * 1000),
        }
        r = _library().call("exec", self.handle, json.dumps(req))
        return ExecResult(
            base64.b64decode(r["stdout"] or ""),
            base64.b64decode(r["stderr"] or ""),
            r["exit_code"],
            r.get("truncated", False),
        )

    def close(self) -> None:
        """Removes the containers and the file system in memory."""
        _interceptors.pop(self.handle, None)
        _library().call("close", self.handle)

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


def short_name(prefix: str) -> str:
    """Returns prefix with a random suffix, for container names."""
    return f"{prefix}-{os.urandom(4).hex()}"
