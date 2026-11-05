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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/secretref"
)

func newMailboxCmd(cfg *globalConfig) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mailbox",
		Short: "Answer tasks as a webhook node hosted by an agent server",
		Long: `Commands a webhook node runs: an agent which only runs when woken, such as a
Grok Bot routine, hosted by an agent server ("a2a-serve pair invite --webhook" on its side).
The agent server holds the node's tasks in a mailbox until the node wakes. The node
pairs once with "a2a-serve pair join <agent-url> <code>", then answers each wake
with reply.

The node token comes from --token, $` + mailboxapi.EnvNodeToken + `, or the token
"a2a-serve pair join" saved. It includes the agent's URL.

` + secretref.Help,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.PersistentFlags().DurationVar(&cfg.timeout, "timeout", 30*time.Second, "Timeout of each request to the agent server, a `duration` such as 30s or 2m")
	cmd.AddCommand(
		newMailboxListCmd(cfg),
		newMailboxGetCmd(cfg),
		newMailboxReplyCmd(cfg),
		newMailboxVerifyCmd(cfg),
	)
	return cmd
}

// nodeClientFlags select the node token of a node command.
type nodeClientFlags struct {
	token string
}

func (r *nodeClientFlags) register(f *pflag.FlagSet) {
	f.StringVar(&r.token, "token", "", secretUsage("Node token (default: $"+mailboxapi.EnvNodeToken+" or the token saved by a2a-serve pair join)"))
}

func (r *nodeClientFlags) nodeClient(cfg *globalConfig) (*mailboxapi.Client, error) {
	token, err := r.nodeToken(cfg)
	if err != nil {
		return nil, err
	}
	return mailboxapi.NodeClient(token)
}

func (r *nodeClientFlags) nodeToken(cfg *globalConfig) (string, error) {
	if r.token != "" {
		return secretref.Resolve("token", r.token, cfg.secretReader(cfg.stderr()))
	}
	if v := mailboxapi.EnvToken(); v != "" {
		return v, nil
	}
	store, err := cfg.credentialStore()
	if err != nil {
		return "", err
	}
	c, err := store.Node()
	if errors.Is(err, credstore.ErrNoNode) {
		return "", clierr.Usage(fmt.Sprintf("no node token: run `a2a-serve pair join <agent-url> <code>`, or set %s", mailboxapi.EnvNodeToken))
	}
	if err != nil {
		return "", err
	}
	return c.Token, nil
}

func nodeContext(cmd *cobra.Command, cfg *globalConfig) (context.Context, context.CancelFunc) {
	return context.WithTimeout(cmd.Context(), cfg.timeout)
}

func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		s = channel.Truncate(s, limit) + "…"
	}
	return s
}
