// Copyright 2026 The A2A Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build cgo

// Command sandbox is the C library of the a2a-serve sandbox. Build it with
//
//	go build -buildmode=c-shared -o liba2asandbox.so ./clib/sandbox
//
// Every function returns a JSON object that the caller frees with
// a2a_sandbox_free; a failed call returns {"error": "..."}. See
// clib/README.md for the calls and their JSON.
package main

/*
#include <stdlib.h>
#include <stdint.h>
*/
import "C"

import (
	"time"
	"unsafe"

	"github.com/a2aproject/a2a-serve/clib/internal/sandboxlib"
)

var lib = sandboxlib.New()

func out(s string) *C.char { return C.CString(s) }

//export a2a_sandbox_free
func a2a_sandbox_free(p *C.char) { C.free(unsafe.Pointer(p)) }

//export a2a_sandbox_open
func a2a_sandbox_open(spec *C.char) *C.char { return out(lib.Open(C.GoString(spec))) }

//export a2a_sandbox_start
func a2a_sandbox_start(h C.int64_t) *C.char { return out(lib.Start(int64(h))) }

//export a2a_sandbox_close
func a2a_sandbox_close(h C.int64_t) *C.char { return out(lib.Close(int64(h))) }

//export a2a_sandbox_stderr
func a2a_sandbox_stderr(h C.int64_t) *C.char { return out(lib.Stderr(int64(h))) }

//export a2a_sandbox_read_file
func a2a_sandbox_read_file(h C.int64_t, name *C.char) *C.char {
	return out(lib.ReadFile(int64(h), C.GoString(name)))
}

//export a2a_sandbox_write_file
func a2a_sandbox_write_file(h C.int64_t, name, b64 *C.char) *C.char {
	return out(lib.WriteFile(int64(h), C.GoString(name), C.GoString(b64)))
}

//export a2a_sandbox_list
func a2a_sandbox_list(h C.int64_t, dir *C.char) *C.char {
	return out(lib.List(int64(h), C.GoString(dir)))
}

//export a2a_sandbox_exec
func a2a_sandbox_exec(h C.int64_t, req *C.char) *C.char {
	return out(lib.Exec(int64(h), C.GoString(req)))
}

//export a2a_sandbox_next_event
func a2a_sandbox_next_event(timeoutMS C.int32_t) *C.char {
	return out(lib.NextEvent(time.Duration(timeoutMS) * time.Millisecond))
}

//export a2a_sandbox_reply
func a2a_sandbox_reply(id C.int64_t, answer *C.char) *C.char {
	return out(lib.Reply(int64(id), C.GoString(answer)))
}

func main() {}
