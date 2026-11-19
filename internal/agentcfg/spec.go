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
	"errors"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// ToolRules returns the tool rules of cfg.
func ToolRules(cfg *agentdir.Config) toolrules.Rules {
	return toolrules.Rules{Allow: cfg.Approvals.Allow, Deny: cfg.Approvals.Deny}
}

// Refusals of turns in processes whose sandbox rules changed.
var (
	// ErrGrantChanged refuses a turn in a busy process whose grant changed,
	// which restarts once its turns end.
	ErrGrantChanged = errors.New("the grant of this conversation's process changed, and the process restarts with it once its current tasks end; send the message again then")
	// ErrGrantNarrowed refuses an answer to a waiting task of a process
	// whose grant now gives less, which ends its waiting tasks.
	ErrGrantNarrowed = errors.New("the grant of this conversation's process changed and gives less, so its waiting tasks ended; send the message again")
)

// MissingSecretError is returned when a process gets a secret that has no
// stored value. Main is set for the main process.
type MissingSecretError struct {
	Name string
	Main bool
}

func (e *MissingSecretError) Error() string { return "secret " + e.Name + " is not set" }

// CopyError is returned when the private copy of the workspace can't be
// made, such as for a workspace over the limits of a copy.
type CopyError struct{ Err error }

func (e *CopyError) Error() string { return e.Err.Error() }

func (e *CopyError) Unwrap() error { return e.Err }

// InjectRuleError is the error of an inject rule that can't apply.
type InjectRuleError struct {
	Rule agentprofile.InjectRule
	Err  error
}

func (e *InjectRuleError) Error() string {
	return "inject rule " + e.Rule.Entry() + ": " + e.Err.Error()
}

func (e *InjectRuleError) Unwrap() error { return e.Err }
