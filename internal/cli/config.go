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
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/lifecycle"
	"github.com/a2aproject/a2a-serve/internal/output"
)

// outputYAML is the -o value only `config show` supports.
const outputYAML = "yaml"

func newConfigCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	show := newConfigShowCmd(cfg, sf)
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show and change the agent's settings",
		Long: `Show and change the settings in .a2a/config.yaml with the flags of
` + "`a2a-serve start <agent>`" + `. Every change is validated and printed as a diff. A
running agent applies the description, the access settings and the
approval settings at once; other settings wait for ` + "`a2a-serve restart`" + `.

Without a subcommand, shows the settings.`,
		Args: cobra.NoArgs,
		// Replaces the parent's check, to also accept -o yaml.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			cfg.setupLogging()
			switch cfg.output {
			case string(output.ModeText), string(output.ModeJson), string(output.ModeJSONL):
				cfg.Mode = output.Mode(cfg.output)
			case outputYAML:
				if cmd.Name() != "config" && cmd.Name() != "show" {
					return clierr.Usage("-o yaml only applies to `a2a-serve config show`")
				}
			default:
				return clierr.Usage(fmt.Sprintf("invalid --output %q (want text, json, jsonl or yaml)", cfg.output))
			}
			return checkTextOnly(cmd, cfg)
		},
		RunE: show.RunE,
	}
	cmd.Flags().AddFlagSet(show.Flags())
	sf.addControlFlags(cmd.PersistentFlags())
	cmd.AddCommand(
		show,
		newConfigSetCmd(cfg, sf),
		newConfigUnsetCmd(cfg, sf),
		newConfigEditCmd(cfg, sf),
	)
	return cmd
}

const restartUsage = "Restart the running agent if the change, or one saved before, needs it"

// printConfigChange prints the result of `config set` or `config unset`.
// With restart, it restarts the running agent if settings wait for it,
// including those saved by an earlier change.
func printConfigChange(ctx context.Context, cfg *globalConfig, sf *serverFlags, d agentdir.Dir, res *agentctl.Result, restart bool) error {
	if restart {
		waiting, err := agentctl.WaitingForRestart(ctx, d, sf.controlClient(d), res)
		if err != nil {
			return err
		}
		res.Restart = waiting
	}
	if err := printConfigResult(cfg, res, restart); err != nil {
		return err
	}
	if !restart || len(res.Restart) == 0 {
		return nil
	}
	return lifecycle.Restart(ctx, d, sf.controlClient(d), false, cfg.progress())
}

// printConfigResult prints the diff of a change, and whether it applies now
// or after a restart. With restarting, the restart follows, so it doesn't
// ask for one.
func printConfigResult(cfg *globalConfig, res *agentctl.Result, restarting bool) error {
	if cfg.IsJSON() {
		return cfg.PrintJSON(res)
	}
	var sb strings.Builder
	secrets := map[string]string{}
	for _, s := range res.Secrets {
		secrets[s.Name] = s.Action
	}
	for _, c := range res.Changes {
		fmt.Fprintf(&sb, "  %-12s %s\n", c.Setting, formatChange(c, secrets))
	}
	for _, s := range res.Secrets {
		if !inSecretDiff(res.Changes, s.Name) {
			fmt.Fprintf(&sb, "  %-12s %s %s\n", "secret value", s.Name, secretAction(s.Action))
		}
	}
	rel := res.Path
	if wd, err := filepath.Abs("."); err == nil {
		if r, err := filepath.Rel(wd, res.Path); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}
	switch {
	case res.DryRun && res.Empty():
		sb.WriteString("  no changes\n")
	case res.DryRun:
		sb.WriteString("  (dry run: nothing saved)\n")
	case res.Empty():
		sb.WriteString("  no changes\n")
	default:
		fmt.Fprintf(&sb, "✓ saved %s\n", rel)
	}
	if len(res.Applied) > 0 {
		fmt.Fprintf(&sb, "✓ applied to the running agent: %s\n", strings.Join(res.Applied, ", "))
	}
	if len(res.Restart) > 0 && !restarting {
		fmt.Fprintf(&sb, "! %s is running; restart to apply %s: a2a-serve restart\n", res.Name, strings.Join(res.Restart, ", "))
	}
	writeAccess(&sb, res.Access, res.Restart)
	writeWarnings(&sb, res.Warnings)
	_, err := fmt.Fprint(cfg.Out, sb.String())
	return err
}

// printStartRunning prints the diff of a start on a running agent, then one
// line that says the agent keeps running and what waits for a restart.
func printStartRunning(cfg *globalConfig, res *agentctl.Result) error {
	shown := *res
	if !cfg.IsJSON() {
		shown.Restart = nil
	}
	if err := printConfigResult(cfg, &shown, false); err != nil {
		return err
	}
	if len(res.Restart) > 0 {
		_, err := fmt.Fprintf(cfg.Out, "✓ %s is already running; restart to apply %s (a2a-serve restart)\n", res.Name, strings.Join(res.Restart, ", "))
		return err
	}
	_, err := fmt.Fprintf(cfg.Out, "✓ %s is already running (a2a-serve status)\n", res.Name)
	return err
}

func writeWarnings(sb *strings.Builder, warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(sb, "! %s\n", w)
	}
}

// printAccess prints who may call the agent after a change, if known.
func printAccess(cfg *globalConfig, a *controlapi.Access) error {
	var sb strings.Builder
	writeAccess(&sb, a, nil)
	_, err := fmt.Fprint(cfg.Out, sb.String())
	return err
}

// writeAccess writes who may call the agent after a change, if known.
// Pending are the settings that wait for a restart.
func writeAccess(sb *strings.Builder, a *controlapi.Access, pending []string) {
	if a == nil {
		return
	}
	sb.WriteString("who may call:\n")
	for _, line := range a.Summary {
		fmt.Fprintf(sb, "  %s\n", line)
	}
	if len(a.Sandbox) > 0 {
		sb.WriteString("what their agent containers get:\n")
	}
	for _, line := range a.Sandbox {
		fmt.Fprintf(sb, "  %s\n", line)
	}
	for _, note := range a.SandboxNotes {
		fmt.Fprintf(sb, "! %s\n", note)
	}
	if note := guestNote(a, pending); note != "" {
		fmt.Fprintf(sb, "! %s\n", note)
	}
}

// guestNote is the model-key note of guests, unless a change of their rule
// that may give them a key waits for a restart.
func guestNote(a *controlapi.Access, pending []string) string {
	if slices.ContainsFunc(pending, agentdir.IsSandboxSetting) {
		return ""
	}
	return a.AnonymousNote
}

// inSecretDiff reports whether the diff of --secret names the secret.
func inSecretDiff(changes []agentdir.Change, name string) bool {
	return slices.ContainsFunc(changes, func(c agentdir.Change) bool {
		return c.Setting == "secret" && (slices.Contains(c.Added, name) || slices.Contains(c.Removed, name)) || c.New == agentdir.SecretPrefix+name
	})
}

func formatChange(c agentdir.Change, secrets map[string]string) string {
	if c.Added == nil && c.Removed == nil {
		empty := "(default)"
		if kind, _, _ := strings.Cut(c.Setting, "-"); slices.Contains([]string{agentdir.MessengerTelegram, agentdir.MessengerDiscord, agentdir.MessengerEmail}, kind) {
			empty = "(none)"
		}
		out := or(c.Old, empty) + " → " + or(c.New, empty)
		if name, ok := strings.CutPrefix(c.New, agentdir.SecretPrefix); ok && secrets[name] != "" {
			out += " (" + secrets[name] + ")"
		}
		return out
	}
	var parts []string
	for _, e := range c.Removed {
		p := "− " + e
		if c.Setting == "secret" && secrets[e] == agentctl.SecretDeleted {
			p += " (value deleted from state/secrets)"
		}
		parts = append(parts, p)
	}
	for _, e := range c.Added {
		p := "+ " + e
		if c.Setting == "secret" && secrets[e] != "" {
			p += " (" + secrets[e] + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

func or(v, empty string) string {
	if v == "" {
		return empty
	}
	return v
}

func secretAction(action string) string {
	if action == agentctl.SecretDeleted {
		return "deleted from state/secrets"
	}
	return action
}
