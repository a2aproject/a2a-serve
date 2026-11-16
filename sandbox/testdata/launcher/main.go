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

// Command launcher runs as PID 1 of the containers in the tests of the
// sandbox package, as a2a-serve sandbox-init does for agents.
package main

import (
	"fmt"
	"os"

	"github.com/a2aproject/a2a-serve/sandbox/sandboxnet"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "sandbox-init" {
		fmt.Fprintln(os.Stderr, "launcher: want sandbox-init")
		os.Exit(2)
	}
	os.Exit(sandboxnet.RunInit(os.Args[2:], os.Stderr))
}
