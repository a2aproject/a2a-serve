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

package agentprofile

import (
	"errors"
	"fmt"
	"strings"
)

// The errors and warnings of this package say what is wrong, and the
// caller words what to do about it, since only it knows its settings and
// commands.

// UnknownAgentError is returned for an agent without a profile.
type UnknownAgentError struct {
	Name  string
	Known []string
}

func (e *UnknownAgentError) Error() string {
	return fmt.Sprintf("unknown agent %q: known agents are %v", e.Name, e.Known)
}

// NoImageError is returned for an agent in a container without an image.
type NoImageError struct{ Agent string }

func (e *NoImageError) Error() string {
	return fmt.Sprintf("agent %q has no container image", e.Agent)
}

// NotInstalledError is returned for an agent without a sandbox whose
// command isn't installed.
type NotInstalledError struct{ Command string }

func (e *NotInstalledError) Error() string { return e.Command + " is not installed" }

// WorkspacePathError is returned when the first mount, the workspace,
// doesn't keep its path in the sandbox.
type WorkspacePathError struct{ Src, Dst string }

func (e *WorkspacePathError) Error() string {
	return fmt.Sprintf("the first mount is the workspace and must keep its path (%s), got %s", e.Src, e.Dst)
}

// NestedAgentsError is returned when another agent was created in a mount
// after the sandbox was prepared, which doesn't hide it.
type NestedAgentsError struct{ Roots []string }

func (e *NestedAgentsError) Error() string {
	return fmt.Sprintf("another agent was created in %s after this agent started, and its sandbox doesn't hide it yet", strings.Join(e.Roots, ", "))
}

// AppearedError is returned when a configuration file of the agent was
// created after the sandbox was prepared, which doesn't hide it.
type AppearedError struct{ Path string }

func (e *AppearedError) Error() string {
	return e.Path + " appeared after this agent started, and its sandbox doesn't hide it yet"
}

// NestedAgentsWarning reports the agents found in the mounts, which the
// sandbox hides unless Exposed.
type NestedAgentsWarning struct {
	Roots   []string
	Exposed bool
}

func (w *NestedAgentsWarning) Error() string {
	if w.Exposed {
		return fmt.Sprintf("found other agents in %s; the sandbox doesn't hide their .a2a directories, so the agent can read their secrets", strings.Join(w.Roots, ", "))
	}
	return fmt.Sprintf("found other agents in %s; the sandbox hides their .a2a directories", strings.Join(w.Roots, ", "))
}

// EgressError is returned when the egress settings need the boundary,
// which can't work for Reason. Local is set for an agent without a sandbox.
type EgressError struct {
	Egress Egress
	Reason string
	Local  bool
}

func (e *EgressError) Error() string {
	reason := e.Reason
	if e.Local {
		reason = "the agent runs without a sandbox"
	}
	return "the egress settings can't apply: " + reason
}

// NoBoundaryWarning reports that the container reaches the network
// without the boundary, for Reason.
type NoBoundaryWarning struct{ Reason string }

func (w *NoBoundaryWarning) Error() string { return "egress control is off: " + w.Reason }

// ErrNoAllowRules warns about an allowlist that allows nothing.
var ErrNoAllowRules = errors.New("the egress allowlist has no rules: the agent can't reach its model provider")

// EgressIgnoredWarning reports egress settings that have no effect, since
// the agent runs without a sandbox: the mode, the allow rules, or both.
type EgressIgnoredWarning struct{ Mode, Allow bool }

func (w *EgressIgnoredWarning) Error() string {
	return "the egress settings have no effect without a sandbox"
}

// EgressWarnings are the warnings of one egress decision, which belong
// together.
type EgressWarnings []error

func (w EgressWarnings) Error() string {
	texts := make([]string, len(w))
	for i, err := range w {
		texts[i] = err.Error()
	}
	return strings.Join(texts, "; ")
}
