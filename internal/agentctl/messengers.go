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
	"errors"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// Messenger states.
const (
	MessengerRunning = "running"
	MessengerFailing = "failing"
	MessengerStopped = "stopped"
)

// Messenger is a messenger of the agent.
type Messenger struct {
	Name    string `json:"name"`
	Account string `json:"account,omitempty"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

// Messengers lists the messengers of the running agent, or those config.yaml
// configures when it doesn't run.
func Messengers(ctx context.Context, d agentdir.Dir, client *controlapi.Client) ([]Messenger, error) {
	st, err := client.Status(ctx)
	if errors.Is(err, agenterr.ErrNotRunning) {
		return configuredMessengers(d)
	}
	if err != nil {
		return nil, err
	}
	out := make([]Messenger, 0, len(st.Messengers))
	for _, c := range st.Messengers {
		state := MessengerRunning
		if c.Error != "" {
			state = MessengerFailing
		}
		out = append(out, Messenger{Name: c.Name, Account: c.Account, State: state, Error: c.Error})
	}
	return out, nil
}

func configuredMessengers(d agentdir.Dir) ([]Messenger, error) {
	conf, err := d.ReadConfig()
	if err != nil {
		return nil, err
	}
	out := []Messenger{}
	for _, kind := range agentdir.MessengerKinds {
		if !conf.HasMessenger(kind) {
			continue
		}
		c := Messenger{Name: kind, State: MessengerStopped}
		if kind == agentdir.MessengerEmail {
			c.Account = conf.Messengers.Email.Address
		}
		out = append(out, c)
	}
	return out, nil
}
