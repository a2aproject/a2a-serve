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

// Command netprovider-slim is an a2a-serve network provider that puts an
// agent on a SLIM node. Agents on the node call each other at
// http://slim.local/<org>/<namespace>/<agent>, and SLIM A2A clients call the
// agent by its SLIM name over SLIMRPC. See README.md.
package main

import (
	"fmt"
	"os"

	"github.com/a2aproject/a2a-serve/netprovider"
)

func main() {
	if err := protocolStdout(); err != nil {
		fmt.Fprintf(os.Stderr, "netprovider-slim: %v\n", err)
		os.Exit(1)
	}
	netprovider.Main(func(h *netprovider.Host) netprovider.Provider { return newProvider(h) })
}
