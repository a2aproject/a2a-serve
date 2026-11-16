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

// Command acp is the C library of a2aacp, which runs an agent of the Agent
// Client Protocol as an A2A executor. Build it with
//
//	go build -buildmode=c-shared -o liba2aacp.so ./clib/acp
//
// Every function returns a JSON object that the caller frees with
// a2a_acp_free; a failed call returns {"error": "..."}. See clib/README.md.
package main

/*
#include <stdlib.h>
#include <stdint.h>
*/
import "C"

import (
	"time"
	"unsafe"

	"github.com/a2aproject/a2a-serve/clib/internal/acplib"
)

var lib = acplib.New()

func out(s string) *C.char { return C.CString(s) }

//export a2a_acp_free
func a2a_acp_free(p *C.char) { C.free(unsafe.Pointer(p)) }

//export a2a_acp_new
func a2a_acp_new(cfg *C.char) *C.char { return out(lib.NewExecutor(C.GoString(cfg))) }

//export a2a_acp_close
func a2a_acp_close(h C.int64_t) *C.char { return out(lib.CloseExecutor(int64(h))) }

//export a2a_acp_send
func a2a_acp_send(h C.int64_t, req *C.char) *C.char { return out(lib.Send(int64(h), C.GoString(req))) }

//export a2a_acp_stream_next
func a2a_acp_stream_next(h C.int64_t, timeoutMS C.int32_t) *C.char {
	return out(lib.StreamNext(int64(h), time.Duration(timeoutMS)*time.Millisecond))
}

//export a2a_acp_stream_close
func a2a_acp_stream_close(h C.int64_t) *C.char { return out(lib.CloseStream(int64(h))) }

//export a2a_acp_cancel
func a2a_acp_cancel(h C.int64_t, taskID *C.char) *C.char {
	return out(lib.Cancel(int64(h), C.GoString(taskID)))
}

//export a2a_acp_next_event
func a2a_acp_next_event(timeoutMS C.int32_t) *C.char {
	return out(lib.NextEvent(time.Duration(timeoutMS) * time.Millisecond))
}

//export a2a_acp_reply
func a2a_acp_reply(id C.int64_t, answer *C.char) *C.char {
	return out(lib.Reply(int64(id), C.GoString(answer)))
}

func main() {}
