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

// Channel states.
const (
	ChannelRunning = "running"
	ChannelFailing = "failing"
	ChannelStopped = "stopped"
)

// Channel is a messenger of the agent.
type Channel struct {
	Name    string `json:"name"`
	Account string `json:"account,omitempty"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

// Channels lists the channels of the running agent, or those config.yaml
// configures when it doesn't run.
func Channels(ctx context.Context, d agentdir.Dir, client *controlapi.Client) ([]Channel, error) {
	st, err := client.Status(ctx)
	if errors.Is(err, agenterr.ErrNotRunning) {
		return configuredChannels(d)
	}
	if err != nil {
		return nil, err
	}
	out := make([]Channel, 0, len(st.Channels))
	for _, c := range st.Channels {
		state := ChannelRunning
		if c.Error != "" {
			state = ChannelFailing
		}
		out = append(out, Channel{Name: c.Name, Account: c.Account, State: state, Error: c.Error})
	}
	return out, nil
}

func configuredChannels(d agentdir.Dir) ([]Channel, error) {
	conf, err := d.ReadConfig()
	if err != nil {
		return nil, err
	}
	out := []Channel{}
	for _, kind := range agentdir.ChannelKinds {
		if !conf.HasChannel(kind) {
			continue
		}
		c := Channel{Name: kind, State: ChannelStopped}
		if kind == agentdir.ChannelEmail {
			c.Account = conf.Channels.Email.Address
		}
		out = append(out, c)
	}
	return out, nil
}
