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

"""Loads the C libraries and turns their JSON results into Python values."""

from __future__ import annotations

import ctypes
import json
import os
import pathlib
import threading
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from typing import Any

ENV_LIB_DIR = "A2A_SERVE_LIB_DIR"


class NativeError(RuntimeError):
    """An error a library call returned."""


def lib_dir() -> pathlib.Path:
    """Returns the directory of the libraries: $A2A_SERVE_LIB_DIR or _lib."""
    env = os.environ.get(ENV_LIB_DIR)
    if env:
        return pathlib.Path(env)
    return pathlib.Path(__file__).parent / "_lib"


def launcher_path() -> str:
    """Returns the path of a2a-sandbox-init, built by clib/build.sh."""
    path = lib_dir() / "a2a-sandbox-init"
    if not path.exists():
        raise NativeError(f"{path} is missing; run clib/build.sh")
    return str(path)


class Library:
    """A C library whose functions take strings and return JSON."""

    def __init__(self, filename: str, prefix: str, signatures: dict[str, list]):
        path = lib_dir() / filename
        if not path.exists():
            raise NativeError(f"{path} is missing; run clib/build.sh")
        self._lib = ctypes.CDLL(str(path))
        self._free = getattr(self._lib, prefix + "free")
        self._free.argtypes = [ctypes.c_void_p]
        self._free.restype = None
        self._fns = {}
        for name, argtypes in signatures.items():
            fn = getattr(self._lib, prefix + name)
            fn.argtypes = argtypes
            fn.restype = ctypes.c_void_p
            self._fns[name] = fn

    def call(self, name: str, *args: Any) -> dict[str, Any]:
        """Calls name and returns its result, or raises its error."""
        conv = [a.encode() if isinstance(a, str) else a for a in args]
        ptr = self._fns[name](*conv)
        try:
            data = ctypes.string_at(ptr).decode()
        finally:
            self._free(ptr)
        result = json.loads(data)
        if isinstance(result, dict) and "error" in result:
            raise NativeError(result["error"])
        return result


class EventPump:
    """Takes the questions of a library and answers each with a handler.

    Each question runs on a worker thread, so a slow answer doesn't hold up
    the others. A handler returns the JSON answer, or raises to send
    {"error": ...}.
    """

    def __init__(
        self, lib: Library, handler: Callable[[str, dict], dict], workers: int = 64
    ):
        self._lib = lib
        self._handler = handler
        self._pool = ThreadPoolExecutor(
            max_workers=workers, thread_name_prefix="a2a-native"
        )
        self._stop = threading.Event()
        self._thread = threading.Thread(
            target=self._run, name="a2a-native-pump", daemon=True
        )
        self._thread.start()

    def _run(self) -> None:
        while not self._stop.is_set():
            event = self._lib.call("next_event", 250)
            if not event:
                continue
            self._pool.submit(self._answer, event)

    def _answer(self, event: dict) -> None:
        try:
            answer = self._handler(event["type"], event["data"])
        except Exception as e:  # pylint: disable=broad-exception-caught
            answer = {"error": f"{type(e).__name__}: {e}"}
        try:
            self._lib.call("reply", event["id"], json.dumps(answer))
        except NativeError:
            # The library stopped waiting, as after a timeout, so nobody
            # takes over the descriptor of the answer.
            if isinstance(answer.get("fd"), int):
                os.close(answer["fd"])

    def close(self) -> None:
        self._stop.set()
        self._thread.join(timeout=2)
        self._pool.shutdown(wait=False, cancel_futures=True)
