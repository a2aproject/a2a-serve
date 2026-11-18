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

package agentrun

import "errors"

// Extensions are how core/a2aserve, whose types don't export the fields
// that only the frontend of this module sets, gives them to it.
type Extensions struct {
	// SetOptions sets the options of a *core/a2aserve.Config.
	SetOptions func(cfg any, o Options) bool
	// SetProcess sets the process options of a *core/a2aserve.Environment,
	// and ProcessOf reads those of a core/a2aserve.Environment.
	SetProcess func(env any, p ProcessOptions) bool
	ProcessOf  func(env any) (ProcessOptions, bool)
	// RunnerOf returns the runner of a *core/a2aserve.Agent.
	RunnerOf func(agent any) (*Runner, bool)
}

var extensions Extensions

// Register sets the extensions. It is called once, from the init of
// core/a2aserve.
func Register(e Extensions) { extensions = e }

var errNotRegistered = errors.New("core/a2aserve isn't linked in, or the value isn't one of its types")

// SetOptions sets the options of cfg, a *core/a2aserve.Config.
func SetOptions(cfg any, o Options) error {
	if extensions.SetOptions == nil || !extensions.SetOptions(cfg, o) {
		return errNotRegistered
	}
	return nil
}

// SetProcess sets the process options of env, a *core/a2aserve.Environment.
func SetProcess(env any, p ProcessOptions) error {
	if extensions.SetProcess == nil || !extensions.SetProcess(env, p) {
		return errNotRegistered
	}
	return nil
}

// ProcessOf returns the process options of env, a
// core/a2aserve.Environment, or none.
func ProcessOf(env any) ProcessOptions {
	if extensions.ProcessOf == nil {
		return ProcessOptions{}
	}
	p, _ := extensions.ProcessOf(env)
	return p
}

// Of returns the runner of agent, a *core/a2aserve.Agent.
func Of(agent any) (*Runner, error) {
	if extensions.RunnerOf != nil {
		if r, ok := extensions.RunnerOf(agent); ok {
			return r, nil
		}
	}
	return nil, errNotRegistered
}
