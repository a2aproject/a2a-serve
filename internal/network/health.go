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

package network

import "errors"

// HealthState says whether a network reaches the agent now.
type HealthState string

// Health states. The zero value is a network that reaches the agent.
const (
	HealthOK HealthState = ""
	// HealthDisconnected is a network the server lost and reconnects to.
	HealthDisconnected HealthState = "disconnected"
	// HealthRemoved is a relay that removed the agent.
	HealthRemoved HealthState = "removed"
)

// Health says whether a provider reaches the agent now, and what the user
// can do when it doesn't.
type Health struct {
	State   HealthState
	Problem string
	Fix     string
}

// OK reports whether the network reaches the agent.
func (h Health) OK() bool { return h.State == HealthOK }

// Err returns the problem and its fix as an error, or nil when the network
// reaches the agent.
func (h Health) Err() error {
	if h.OK() {
		return nil
	}
	if h.Fix == "" {
		return errors.New(h.Problem)
	}
	return errors.New(h.Problem + "; " + h.Fix)
}

// HealthReporter is a [Provider] that can lose the network after it
// started listening.
type HealthReporter interface {
	Health() Health
}

// HealthOf returns the health of p. Providers that don't report it are
// healthy.
func HealthOf(p Provider) Health {
	if hr, ok := p.(HealthReporter); ok {
		return hr.Health()
	}
	return Health{}
}
