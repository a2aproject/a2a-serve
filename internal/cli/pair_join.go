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
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/doctor"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
)

func newPairJoinCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var replace bool
	cmd := &cobra.Command{
		Use:   "join <agent-url> <code>",
		Short: "Join another agent with a pairing code from its owner",
		Long: `Exchange a pairing code from an agent's owner (a2a-serve pair invite) for a token.
The token is saved to ~/.config/a2a-serve/credentials.json. The a2a CLI
sends it with later requests to the same agent, such as a2a send, through the
credential helper that a2a-serve doctor --fix configures.

Joining an agent you joined before replaces your saved token, so you lose
access to what you did with it. The command refuses that without --replace.
A code for a webhook node replaces nothing, so --replace is safe with it.

A code for a webhook node (a2a-serve pair invite --webhook) returns a node token
instead, which the a2a-serve mailbox commands use.`,
		Example: "  a2a-serve pair join https://assistant.tail1234.ts.net 3MZ8-LQ0D",
		Args:    namedArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkAgentURL("the agent URL", args[0], "https://assistant.tail1234.ts.net"); err != nil {
				return err
			}
			store, err := cfg.credentialStore()
			if err != nil {
				return err
			}
			if !replace {
				if err := checkNotJoined(store, args[0]); err != nil {
					return err
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), sf.timeout)
			defer cancel()
			c, err := credstore.Pair(ctx, &http.Client{Transport: cfg.httpTransport}, args[0], args[1], pairapi.PairingPerson)
			if err != nil {
				return err
			}
			if err := store.Put(c); err != nil {
				return err
			}
			setup := needsHelperSetup(cfg)
			if c.Kind == credstore.KindNode {
				return printNodePaired(cfg, c, store.Path, setup)
			}
			return printJoinedAs(cfg, c, store.Path, setup)
		},
	}
	cmd.Flags().BoolVar(&replace, "replace", false, "Join an agent you joined before, replacing the saved token")
	sf.addTimeoutFlag(cmd.Flags(), "Timeout of the request to the other agent, a `duration` such as 30s or 2m")
	// Joining another agent uses no agent directory of this one.
	var dir string
	cmd.Flags().StringVar(&dir, "dir", "", "")
	must(cmd.Flags().MarkHidden("dir"))
	return cmd
}

// checkNotJoined refuses a join that would replace the token of an earlier
// join. The check runs before the code is exchanged, so a refused join
// leaves the code unused.
func checkNotJoined(store *credstore.Store, agentURL string) error {
	c, ok, err := store.Joined(agentURL)
	if err != nil || !ok {
		return err
	}
	who := c.Principal
	if c.Agent != "" {
		who = c.Agent + " as " + who
	}
	e := clierr.Usage(fmt.Sprintf("you already joined %s; joining again replaces that token in %s, and you lose access to the conversations and tasks of %s", who, store.Path, c.Principal))
	e.Hint = "pass --replace to join anyway; a code for a webhook node replaces nothing"
	return e
}

// needsHelperSetup reports whether the a2a CLI lacks the credential helper
// that sends the saved token, which `a2a-serve doctor --fix` configures.
func needsHelperSetup(cfg *globalConfig) bool {
	env, err := doctorEnv(cfg)
	return err != nil || !doctor.CredentialHelperConfigured(env)
}

func printJoinedAs(cfg *globalConfig, c credstore.Credential, saved string, helperSetup bool) error {
	if cfg.IsJSON() {
		return cfg.PrintJSON(map[string]string{"url": c.URL, "agent": c.Agent, "principal": c.Principal, "saved": saved})
	}
	hint := ""
	if helperSetup {
		hint = "  (" + credstore.HelperSetup + ")\n"
	}
	_, err := fmt.Fprintf(cfg.Out, "✓ joined %s as %s; token saved to %s\n%s", c.Agent, c.Principal, saved, hint)
	return err
}

func printNodePaired(cfg *globalConfig, c credstore.Credential, saved string, helperSetup bool) error {
	if cfg.IsJSON() {
		return cfg.PrintJSON(map[string]string{"url": c.URL, "node": c.Agent, "principal": c.Principal, "token": c.Token, "saved": saved})
	}
	agent, _ := mailboxapi.AgentURL(c.Token)
	hint := ""
	if helperSetup {
		hint = "(" + credstore.HelperSetup + ")\n"
	}
	_, err := fmt.Fprintf(cfg.Out, "✓ paired as node %s at %s; token saved to %s\n\n  Node token: %s\n\nKeep the token as the secret %s if the node runs elsewhere; it includes the agent's URL.\nThe node calls the agent with: a2a send -a %s \"…\"\n%s",
		c.Agent, c.URL, saved, c.Token, mailboxapi.EnvNodeToken, agent, hint)
	return err
}
