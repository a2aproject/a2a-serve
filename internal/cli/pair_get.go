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
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func newPairGetCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:               "get <name|agent-url>",
		Short:             "Show a pairing: its kind, role, tokens, linked accounts and pending codes, or an agent you joined",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: pairingCompletions(cfg, sf),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if isAgentURL(args[0]) {
				c, err := joinedCredential(cfg, args[0])
				if err != nil {
					return err
				}
				j := joinedOf(c)
				if cfg.IsJSON() {
					return cfg.PrintJSON(j)
				}
				_, err = fmt.Fprintf(cfg.Out, "%s (joined %s)\n  agent        %s\n  as           %s\n  joined       %s\n", j.URL, j.Kind, j.Agent, j.Principal, j.Joined.Format(time.DateTime))
				return err
			}
			c, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			p, err := c.Pairing(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(p)
			}
			_, err = fmt.Fprint(cfg.Out, renderPairing(p))
			return err
		},
	}
}

func renderPairing(p *controlapi.Pairing) string {
	var sb strings.Builder
	line := func(label, value string) { fmt.Fprintf(&sb, "  %-12s %s\n", label, value) }
	when := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.Format(time.DateTime)
	}
	if p.Node != nil {
		fmt.Fprintf(&sb, "%s (webhook node)\n", p.Name)
	} else {
		fmt.Fprintf(&sb, "%s (%s, %s)\n", p.Name, cmp.Or(p.Kind, "not joined yet"), p.Role)
	}
	line("principal", p.Principal)
	if !p.Created.IsZero() {
		line("created", p.Created.Format(time.DateTime))
	}
	if n := p.Node; n != nil {
		line("url", n.URL)
		line("webhook", n.Webhook)
		if n.Description != "" {
			line("description", n.Description)
		}
		if len(n.HeaderNames) > 0 {
			line("headers", strings.Join(n.HeaderNames, ", "))
		}
		line("advertised", yesNo(n.Advertised))
		line("paired", yesNo(n.Paired))
		line("mailbox", fmt.Sprintf("%d waiting, %d open", n.Waiting, n.Open))
	} else {
		line("tokens", strconv.Itoa(len(p.Tokens)))
		for _, t := range p.Tokens {
			fmt.Fprintf(&sb, "  %-12s created %s, last used %s\n", "", t.Created.Format(time.DateTime), when(t.LastUsed))
		}
		accounts := make([]string, 0, len(p.Accounts))
		for _, a := range p.Accounts {
			accounts = append(accounts, a.Channel+" "+a.Account)
		}
		line("accounts", orNone(strings.Join(accounts, ", ")))
	}
	codes := make([]string, 0, len(p.PendingCodes))
	for _, e := range p.PendingCodes {
		codes = append(codes, "expires "+e.Format(time.TimeOnly))
	}
	line("codes", orNone(strings.Join(codes, ", ")))
	return sb.String()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
