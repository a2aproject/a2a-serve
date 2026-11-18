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

package agentcfg

import (
	"github.com/a2aproject/a2a-serve/core/agent"
	"github.com/a2aproject/a2a-serve/core/runner"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/core/toolrules"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// Spec returns what the runner runs for the agent of cfg in d. cli is the
// a2a binary of an agent without a sandbox. It is the one place that maps
// a setting to the core.
// On error, the spec still holds the other settings.
func Spec(cfg *agentdir.Config, d agentdir.Dir, cli string) (runner.Spec, error) {
	mounts, err := d.Mounts(cfg)
	spec := runner.Spec{
		Name:        "a2a-" + cfg.Name + "-" + d.ID(),
		Labels:      map[string]string{"a2a.agent": cfg.Name, "a2a.dir": d.Root},
		Agent:       cfg.Harness,
		Command:     cfg.Command,
		Image:       cfg.Image,
		Model:       cfg.Model,
		Env:         cfg.Env,
		Sandbox:     cfg.Sandbox,
		Limits:      sandbox.Limits{Memory: cfg.Limits.Memory, CPUs: cfg.Limits.CPUs, PIDs: cfg.Limits.PIDs},
		Mounts:      mounts,
		EgressMode:  cfg.Egress.Mode,
		EgressAllow: cfg.Egress.Allow,
		Inject:      cfg.Inject,
		Tools:       ToolRules(cfg),
		DisabledMCP: cfg.MCP.Disabled,
	}
	if cfg.Sandbox == agent.SandboxLocal {
		spec.CLI = cli
	}
	return spec, err
}

// ToolRules returns the tool rules of cfg.
func ToolRules(cfg *agentdir.Config) toolrules.Rules {
	return toolrules.Rules{Allow: cfg.Approvals.Allow, Deny: cfg.Approvals.Deny}
}
