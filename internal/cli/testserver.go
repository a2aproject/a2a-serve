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

package cli

import (
	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/localsrv"
)

// testServerFlags are the flags of the echo, proxy and script test servers.
type testServerFlags struct {
	o localsrv.Options
}

func (t *testServerFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&t.o.Host, "host", "127.0.0.1", "Bind address")
	f.IntVar(&t.o.Port, "port", 8080, "Listen port")
	f.StringVar(&t.o.Card.AgentName, "name", "", "Agent name for the generated card")
	f.StringVar(&t.o.Card.AgentDesc, "description", "", "Agent description for the generated card")
	f.StringVar(&t.o.Card.CardPath, "card", "", "Serve a custom agent card JSON file")
	f.BoolVar(&t.o.CardCompat, "card-compat", false, "Serve the agent card in a dual v0.3/v1.0 format")
	f.StringVar(&t.o.Protocol, "protocol", "latest", `Protocol version: "latest" or "0.3"`)
	f.StringVar(&t.o.Transport, "transport", "rest", "Transport to serve: rest, jsonrpc or grpc")
	f.StringVar(&t.o.Card.AdvertiseAddress, "advertise-address", "", "Agent endpoint to put in the agent card (default: the listen address)")
	f.BoolVar(&t.o.Quiet, "quiet", false, "Print less on the terminal: don't log the traffic")
	must(cmd.RegisterFlagCompletionFunc("protocol", fixedCompletions("latest", "0.3")))
	must(cmd.RegisterFlagCompletionFunc("transport", fixedCompletions("rest", "jsonrpc", "grpc")))
}

func (t *testServerFlags) options(cfg *globalConfig) localsrv.Options {
	o := t.o
	o.Logger = cfg.logf
	return o
}
