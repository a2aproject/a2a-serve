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

//go:build !agentmesh

package network

import "errors"

// AgentMeshBuilt reports whether this a2a-serve can join Agent Mesh relays.
const AgentMeshBuilt = false

// NewRelay returns the provider of an Agent Mesh relay.
func NewRelay(RelayConfig) (Provider, error) {
	return nil, errors.New("this a2a-serve was built without Agent Mesh support; build it with `go install -tags agentmesh github.com/a2aproject/a2a-serve@latest` to join a relay")
}
