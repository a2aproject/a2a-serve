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

// Package agentcfg maps the agent's configuration to the core's inputs,
// and words what the core reports in terms of the settings and commands
// that change it.
package agentcfg

import (
	"github.com/a2aproject/a2a-serve/core/agent"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// Profile returns the profile of the agent of cfg.
func Profile(cfg *agentdir.Config) (agent.Profile, error) {
	p, err := agent.Lookup(cfg.Harness, cfg.Command)
	return p, Explain(err)
}

// Egress returns the settings of cfg that decide its egress.
func Egress(cfg *agentdir.Config) agent.Egress {
	return agent.Egress{Sandbox: cfg.Sandbox, Mode: cfg.Egress.Mode, Allow: cfg.Egress.Allow, Inject: cfg.Inject}
}
