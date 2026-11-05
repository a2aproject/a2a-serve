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
	"fmt"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
)

func newContactAddCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var req controlapi.ContactRequest
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Let the agent call another agent",
		Long: `Let the agent call another agent with the a2a CLI. Without --url the
agent is looked up by name on the private network, where it recognizes this
agent by its network identity. With --url, pair with a code from the other
agent's owner, or pass a token. A pairing another agent of the private
network advertises, such as grok@ben from ` + "`a2a-serve peers`" + `, needs neither:
calls to it go through that agent.

` + flagparse.SecretHelp,
		Example: "  a2a-serve contact add coder\n  a2a-serve contact add grok@ben\n  a2a-serve contact add grok --url https://grok.example.com --code K7Q2-9XF4",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			c, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			req.Name = args[0]
			if req.Token != "" {
				if req.Token, err = flagparse.ResolveSecret("token", req.Token, cfg.secretReader(cfg.stderr())); err != nil {
					return err
				}
			}
			info, err := c.AddContact(cmd.Context(), req)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(info)
			}
			_, err = fmt.Fprintf(cfg.Out, "✓ the agent can call %s (%s, via %s)\n", info.Name, info.URL, info.Via)
			return err
		},
	}
	cmd.Flags().StringVar(&req.URL, "url", "", "The agent's URL")
	cmd.Flags().StringVar(&req.Code, "code", "", "A pairing code from the other agent's owner")
	cmd.Flags().StringVar(&req.Token, "token", "", secretUsage("A token for the other agent"))
	return cmd
}
