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

// Package agentcard builds the agent card the server serves on each of its
// listeners, which `a2a-serve card` prints too.
package agentcard

import (
	"cmp"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/speech"
)

// Listener is the listener a card is served on.
type Listener struct {
	// Kind is one of the network.Listener* kinds.
	Kind string
	URL  string
	// SignIn is the security scheme of Google sign-in, which the public
	// listener names as an alternative to pairing. Nil without sign-in.
	SignIn a2a.SecurityScheme
}

// inputRequiredNote applies to every skill: a task pauses for its approvals.
const inputRequiredNote = "Tool calls may pause a task in input-required until whoever started it answers."

// Build returns the card of the agent cfg configures, served on l.
func Build(cfg *agentdir.Config, l Listener) *a2a.AgentCard {
	desc := cmp.Or(cfg.Description, cfg.Name+" is "+output.WithArticle(cfg.Harness)+" coding agent reached through `a2a-serve`.")
	card := &a2a.AgentCard{
		Name:        cfg.Name,
		Description: desc + " " + inputRequiredNote,
		Version:     "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(l.URL, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(l.URL, a2a.TransportProtocolHTTPJSON),
		},
		Capabilities: a2a.AgentCapabilities{
			Streaming:  true,
			Extensions: []a2a.AgentExtension{structuredinput.NewExtension(structuredinput.Draft202012)},
		},
		DefaultInputModes:  []string{"text/plain", "application/octet-stream"},
		DefaultOutputModes: []string{"text/plain", "application/json"},
		Skills:             skills(cfg),
	}
	if cfg.Voice.On() {
		card.DefaultInputModes = append(card.DefaultInputModes, speech.InputModes...)
		card.DefaultOutputModes = append(card.DefaultOutputModes, speech.OutputModes...)
	}
	if !NeedsPairing(cfg.Policy, l.Kind) {
		return card
	}
	card.SecuritySchemes = a2a.NamedSecuritySchemes{
		"pairing": a2a.HTTPAuthSecurityScheme{Scheme: "Bearer", Description: "A token from pairing: `a2a-serve pair join <agent-url> <code>`, with a code from the owner."},
	}
	card.SecurityRequirements = a2a.SecurityRequirementsOptions{{"pairing": {}}}
	if l.Kind != network.ListenerPublic || l.SignIn == nil {
		return card
	}
	card.SecuritySchemes["google"] = l.SignIn
	card.SecurityRequirements = append(card.SecurityRequirements, a2a.SecurityRequirements{"google": {}})
	return card
}

// skills returns the skills config.yaml sets, or the default skill.
func skills(cfg *agentdir.Config) []a2a.AgentSkill {
	if len(cfg.Card.Skills) == 0 {
		return []a2a.AgentSkill{{
			ID:          "coding",
			Name:        "Coding agent",
			Description: "Works in its project workspace with " + cfg.Harness + ".",
			Tags:        []string{"coding", cfg.Harness},
		}}
	}
	out := make([]a2a.AgentSkill, len(cfg.Card.Skills))
	for i, sk := range cfg.Card.Skills {
		out[i] = a2a.AgentSkill{ID: sk.ID, Name: cmp.Or(sk.Name, sk.ID), Description: sk.Description, Tags: append([]string{}, sk.Tags...)}
	}
	return out
}

// NeedsPairing reports whether callers at the listener kind need a pairing
// token, which the agent card then advertises: the local listener always,
// the private one with --private-allow-incoming paired, and the public one
// unless --public-allow-incoming anyone.
func NeedsPairing(p agentdir.PolicyConfig, kind string) bool {
	switch kind {
	case network.ListenerPrivate:
		return access.PrivateAllowIncoming(p) == agentdir.AccessPaired
	case network.ListenerPublic:
		return p.PublicAllowIncoming != agentdir.AccessAnyone
	}
	return true
}
