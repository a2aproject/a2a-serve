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
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "sandbox-init" {
		fmt.Fprintln(os.Stderr, "launcher: want sandbox-init")
		return 2
	}
	args = args[1:]
	check := len(args) > 0 && args[0] == "--check"
	if check {
		args = args[1:]
	}
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "launcher: want <socket> <uid> <gid>")
		return 2
	}
	uid, uerr := strconv.Atoi(args[1])
	gid, gerr := strconv.Atoi(args[2])
	if uerr != nil || gerr != nil {
		fmt.Fprintln(os.Stderr, "launcher: invalid uid or gid")
		return 2
	}
	code, err := sandboxnet.Launch(sandboxnet.LaunchConfig{Boundary: args[0], UID: uid, GID: gid, Argv: args[3:], Env: os.Environ(), Check: check})
	if setupErr, ok := errors.AsType[*sandboxnet.SetupError](err); ok {
		fmt.Fprintf(os.Stderr, "a2a-init: setting up the sandbox's network: %v\n", setupErr)
		return sandboxnet.SetupFailedExit
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "launcher:", err)
		return 1
	}
	return code
}
