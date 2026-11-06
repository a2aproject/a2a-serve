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

package agentctl

import (
	"context"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/harness"
)

// Secret states of a [View].
const (
	SecretStateStored  = "stored"
	SecretStateMissing = "missing"
)

// View is the effective config of an agent.
type View struct {
	Dir    string           `json:"dir" yaml:"dir"`
	Path   string           `json:"path" yaml:"path"`
	Config *agentdir.Config `json:"config" yaml:"config"`
	// Defaults are the values of settings the config leaves unset.
	Defaults *agentdir.Config `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	// Secrets maps the name of every secret the config uses to its state.
	Secrets map[string]string `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Running bool              `json:"running" yaml:"running"`
	// Pending are the settings changed since the running server started.
	Pending []string `json:"pending,omitempty" yaml:"pending,omitempty"`
	// Invalid is why the config is invalid.
	Invalid string `json:"invalid,omitempty" yaml:"invalid,omitempty"`
	// Commands are the `a2a-serve` commands which recreate the agent.
	Commands [][]string `json:"commands" yaml:"commands"`
}

// Show returns the effective config of the agent in d.
func Show(ctx context.Context, d agentdir.Dir, client *controlapi.Client) (*View, error) {
	cfg, err := d.ReadConfig()
	if err != nil {
		return nil, err
	}
	stored, err := d.LoadSecrets()
	if err != nil {
		return nil, err
	}
	v := &View{Dir: d.Root, Path: d.ConfigPath(), Config: cfg, Secrets: map[string]string{}, Commands: cfg.Commands(stored)}
	if err := d.CheckConfig(cfg, stored); err != nil {
		v.Invalid = agenterr.Text(agentdir.FileProblems(err))
	}
	for _, name := range append(cfg.Secrets, messengerSecrets(cfg)...) {
		v.Secrets[name] = SecretStateMissing
		if stored[name] != "" {
			v.Secrets[name] = SecretStateStored
		}
	}
	if stored[agentdir.SecretTailscaleKey] != "" {
		v.Secrets[agentdir.SecretTailscaleKey] = SecretStateStored
	}
	v.Defaults = defaults(d, cfg)
	if v.Pending, v.Running, err = Pending(ctx, d, client); err != nil {
		return nil, err
	}
	return v, nil
}

func messengerSecrets(cfg *agentdir.Config) []string {
	var out []string
	for _, kind := range messengerKinds {
		out = append(out, messengerSecretsOf(cfg, kind)...)
	}
	return out
}

// defaults returns the values the agent uses for the settings cfg leaves
// unset.
func defaults(d agentdir.Dir, cfg *agentdir.Config) *agentdir.Config {
	out := &agentdir.Config{}
	if cfg.Workspace == "" {
		out.Workspace = d.Root
	}
	if cfg.Sandbox == "" {
		out.Sandbox = agentdir.SandboxDocker
	}
	if p, err := harness.Lookup(cfg.Harness, cfg.Command); err == nil && cfg.Image == "" {
		out.Image = p.Image()
	}
	if cfg.Limits.Memory == "" {
		out.Limits.Memory = agentdir.DefaultLimits.Memory
	}
	if cfg.Limits.CPUs == "" {
		out.Limits.CPUs = agentdir.DefaultLimits.CPUs
	}
	if cfg.Limits.PIDs == 0 {
		out.Limits.PIDs = agentdir.DefaultLimits.PIDs
	}
	return out
}
