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

// Command a2a-sandbox-init is the launcher of the Docker sandbox for
// programs that embed the sandbox package, such as its C library, which
// can't run themselves in a container. Build it with CGO_ENABLED=0:
//
//	CGO_ENABLED=0 go build -o a2a-sandbox-init ./sandbox/cmd/a2a-sandbox-init
package main

import (
	"fmt"
	"os"

	"github.com/a2aproject/a2a-serve/sandbox/sandboxnet"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "sandbox-init" {
		fmt.Fprintln(os.Stderr, "a2a-sandbox-init: want sandbox-init "+sandboxnet.InitUsage)
		os.Exit(2)
	}
	os.Exit(sandboxnet.RunInit(os.Args[2:], os.Stderr))
}
