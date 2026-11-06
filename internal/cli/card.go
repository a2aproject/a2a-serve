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

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentcard"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/network"
)

func newCardCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "card",
		Short: "Print the agent card as callers see it",
		Long: `Print the agent card as callers see it, in JSON. Each URL serves its own
card, which differs in the URL and in the credentials it asks for. Without
--url, the card of the URL which reaches furthest: public, then private, then
local. --url local also works while the agent is stopped, once its first
start picked its port.

The skills of the card come from card.skills in config.yaml; see the
configuration reference.`,
		Example: `  a2a-serve card
  a2a-serve card --url=private`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkScope(scope); err != nil {
				return clierr.Usage(err.Error())
			}
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			card, err := agentCard(cmd.Context(), d, sf.controlClient(d), scope)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(card)
			}
			data, err := json.MarshalIndent(card, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cfg.Out, string(data))
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&scope, "url", "", "Print the card of the agent's `scope` URL: local, private or public (default: the one which reaches furthest)")
	must(cmd.RegisterFlagCompletionFunc("url", fixedCompletions(scopeLocal, scopePrivate, scopePublic)))
	sf.addControlFlags(f)
	return cmd
}

// agentCard returns the card the agent in d serves at its URL in scope. A
// stopped agent has only its local URL, from the saved port, and the card
// comes from its saved config.
func agentCard(ctx context.Context, d agentdir.Dir, client *controlapi.Client, scope string) (*a2a.AgentCard, error) {
	st, err := client.Status(ctx)
	if errors.Is(err, agenterr.ErrNotRunning) && (scope == "" || scope == scopeLocal) {
		conf, cerr := d.LoadConfig()
		if cerr != nil {
			return nil, cerr
		}
		if conf.LocalURL() == "" {
			return nil, err
		}
		return agentcard.Build(conf, agentcard.Listener{Kind: network.ListenerLocal, URL: conf.LocalURL()}), nil
	}
	if err != nil {
		return nil, err
	}
	u, err := statusURL(st, scope)
	if err != nil {
		return nil, clierr.Usage(err.Error())
	}
	kind := network.ListenerLocal
	switch u {
	case st.Public:
		kind = network.ListenerPublic
	case st.Private:
		kind = network.ListenerPrivate
	}
	return client.Card(ctx, kind)
}
