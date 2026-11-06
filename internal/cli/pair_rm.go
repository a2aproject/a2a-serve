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
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
)

func newPairRmCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var (
		email  string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:     "rm <name|agent-url>",
		Aliases: []string{"delete"},
		Short:   "Revoke a pairing, remove a webhook node, or forget an agent you joined",
		Long: `Delete a pairing with its tokens, linked messenger accounts, pending
codes, the schedules that run as it, and its tasks, approvals and
conversations, or remove a webhook node with its schedules. Its running
tasks are canceled, its agent process stops, and its agent home directory
is deleted. A new pairing with the same name starts with none of them. The
person may still call the agent through the private network or an
--allow-email rule; the command says so, and prints who may call
afterwards.

` + "`a2a-serve pair rm local`" + ` revokes the token of this machine and your linked
chat accounts, and keeps the pairing; the next start issues a new token.

With an agent URL, forget the token you saved with ` + "`a2a-serve pair join`" + `.

With --email, revoke the Google sign-ins of a person. They can sign in
again while an --allow-email rule matches their email; remove the rule with
` + "`a2a-serve config unset allow-email`" + ` to stop that.`,
		Example: `  a2a-serve pair rm anna
  a2a-serve pair rm https://coder.tail1234.ts.net
  a2a-serve pair rm --email anna@example.com
  a2a-serve pair rm anna --dry-run`,
		Args: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("email") {
				return namedArgs(cmd, args)
			}
			if len(args) > 0 {
				return clierr.Usage("--email takes no pairing name or agent URL")
			}
			if dryRun {
				return clierr.Usage("--dry-run takes a pairing name or agent URL, not --email")
			}
			return nil
		},
		ValidArgsFunction: pairingCompletions(cfg, sf),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if cmd.Flags().Changed("email") {
				return signOut(cmd.Context(), cfg, sf, email)
			}
			if isAgentURL(args[0]) {
				return forgetJoined(cfg, args[0], dryRun)
			}
			c, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			if dryRun {
				preview, err := c.PreviewRevoke(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return printRemovalPreview(cfg, args[0], preview)
			}
			resp, err := c.Revoke(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				out := map[string]any{"removed": true, "kind": resp.Kind, "name": args[0]}
				if resp.Kept {
					out["kept"] = true
				}
				if len(resp.StillAllowed) > 0 {
					out["stillAllowed"] = resp.StillAllowed
				}
				if resp.Access != nil {
					out["access"] = resp.Access
				}
				return cfg.PrintJSON(out)
			}
			if _, err := fmt.Fprint(cfg.Out, removedText(args[0], resp)); err != nil {
				return err
			}
			return printAccess(cfg, resp.Access)
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "Revoke the Google sign-ins of the email `address`")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would be removed without removing it")
	return cmd
}

func forgetJoined(cfg *globalConfig, agentURL string, dryRun bool) error {
	store, err := cfg.credentialStore()
	if err != nil {
		return err
	}
	if dryRun {
		ok, err := store.Has(agentURL)
		if err != nil {
			return err
		}
		if !ok {
			return notJoined(agentURL)
		}
		if cfg.IsJSON() {
			return cfg.PrintJSON(map[string]any{"dryRun": true, "kind": "joined", "name": agentURL})
		}
		_, err = fmt.Fprintf(cfg.Out, "would delete the token you saved for %s\n%s\n", agentURL, dryRunNote)
		return err
	}
	ok, err := store.Delete(agentURL)
	if err != nil {
		return err
	}
	if !ok {
		return notJoined(agentURL)
	}
	return cfg.PrintRemoved("joined", agentURL, "✓ deleted the token for "+agentURL)
}

const dryRunNote = "(dry run: nothing removed)"

// removalPreviewJSON is the JSON output of `pair rm --dry-run`.
type removalPreviewJSON struct {
	DryRun bool   `json:"dryRun"`
	Name   string `json:"name"`
	*controlapi.RemovalPreview
}

func printRemovalPreview(cfg *globalConfig, name string, p *controlapi.RemovalPreview) error {
	if cfg.IsJSON() {
		return cfg.PrintJSON(removalPreviewJSON{DryRun: true, Name: name, RemovalPreview: p})
	}
	_, err := fmt.Fprint(cfg.Out, removalPreviewText(name, p))
	return err
}

func removalPreviewText(name string, p *controlapi.RemovalPreview) string {
	var sb strings.Builder
	switch {
	case p.Kept:
		fmt.Fprintf(&sb, "would revoke the credentials of %s and keep the pairing:\n", name)
	case p.Kind == pairapi.PairingWebhook:
		fmt.Fprintf(&sb, "would remove the node %s:\n", name)
	default:
		fmt.Fprintf(&sb, "would remove the pairing %s:\n", name)
	}
	line := func(n int, one, many string) {
		if n > 0 {
			fmt.Fprintf(&sb, "  %d %s\n", n, plural(n, one, many))
		}
	}
	line(p.Tokens, "token", "tokens")
	if n := len(p.Accounts); n > 0 {
		accounts := make([]string, n)
		for i, a := range p.Accounts {
			accounts[i] = a.Channel + " " + a.Account
		}
		fmt.Fprintf(&sb, "  %d %s: %s\n", n, plural(n, "linked chat account", "linked chat accounts"), strings.Join(accounts, ", "))
	}
	line(p.Codes, "pending code", "pending codes")
	if p.Node {
		sb.WriteString("  the webhook node\n")
	}
	line(p.Schedules, "schedule that runs as it", "schedules that run as it")
	line(p.Watches, "watch", "watches")
	line(p.Approvals, "approval", "approvals")
	line(p.Tasks, "task", "tasks")
	if p.RunningTasks > 0 {
		fmt.Fprintf(&sb, "    %d of them running, canceled first\n", p.RunningTasks)
	}
	line(p.Conversations, "conversation", "conversations")
	line(p.Allowed, "tool call allowed in all its conversations", "tool calls allowed in all its conversations")
	for _, dir := range p.AgentHomes {
		fmt.Fprintf(&sb, "  the agent home directory %s\n", dir)
	}
	sb.WriteString(dryRunNote + "\n")
	return sb.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func signOut(ctx context.Context, cfg *globalConfig, sf *serverFlags, email string) (err error) {
	c, closer, err := sf.manager(ctx)
	if err != nil {
		return err
	}
	defer joinClose(&err, closer)
	resp, err := c.SignOut(ctx, email)
	if err != nil {
		return err
	}
	if cfg.IsJSON() {
		out := map[string]any{"removed": true, "kind": "google", "name": resp.Email, "tokens": resp.Tokens}
		if resp.Access != nil {
			out["access"] = resp.Access
		}
		return cfg.PrintJSON(out)
	}
	if _, err := fmt.Fprintf(cfg.Out, "✓ revoked %d Google sign-in token(s) of %s\n", resp.Tokens, resp.Email); err != nil {
		return err
	}
	return printAccess(cfg, resp.Access)
}

func removedText(name string, resp *controlapi.RevokeResponse) string {
	switch {
	case resp.Kind == pairapi.PairingWebhook:
		return fmt.Sprintf("✓ removed node %s; the agent can no longer wake it\n", name)
	case resp.Kept:
		return fmt.Sprintf("✓ revoked the token and chat links of %s; the next start issues a new token for this machine\n", name)
	case len(resp.StillAllowed) > 0:
		return fmt.Sprintf("✓ removed the pairing %s\n! %s may still call the agent as one of: %s\n", name, name, strings.Join(resp.StillAllowed, "; "))
	}
	return fmt.Sprintf("✓ %s can no longer call the agent\n", name)
}

// pairingCompletions completes the pairings of the running agent, and the
// agents this user joined.
func pairingCompletions(cfg *globalConfig, sf *serverFlags) cobra.CompletionFunc {
	pairings := controlCompletions(sf, func(ctx context.Context, c *controlapi.Client) ([]string, error) {
		all, err := c.Pairings(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(all))
		for _, p := range all {
			names = append(names, p.Name)
		}
		return names, nil
	})
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		names, directive := pairings(cmd, args, toComplete)
		if len(args) > 0 {
			return names, directive
		}
		urls, err := joinedCompletions(cfg)
		if err != nil {
			cobra.CompDebugln(err.Error(), false)
		}
		return append(names, urls...), cobra.ShellCompDirectiveNoFileComp
	}
}
