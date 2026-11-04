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
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/output"
)

// outputYAML is the -o value only `config show` supports.
const outputYAML = "yaml"

func newConfigCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var show showFlags
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show and change the agent's settings",
		Long: `Show and change the settings in .a2a/config.yaml with the flags of
` + "`a2a-serve start <agent>`" + `. Every change is validated and printed as a diff. A
running agent applies the description and the approval settings at once;
other settings wait for ` + "`a2a-serve restart`" + `.

Without a subcommand, shows the settings.`,
		Args: cobra.NoArgs,
		// Replaces the parent's check, to also accept -o yaml.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigShow(cmd, cfg, sf, show)
		},
	}
	show.register(cmd)
	sf.addControlFlags(cmd.PersistentFlags())
	cmd.AddCommand(
		newConfigShowCmd(cfg, sf),
		newConfigSetCmd(cfg, sf),
		newConfigUnsetCmd(cfg, sf),
		newConfigEditCmd(cfg, sf),
	)
	return cmd
}

// printConfigResult prints the diff of a change, and whether it applies now
// or after a restart.
func printConfigResult(cfg *globalConfig, res *agentctl.Result) error {
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
	if len(res.Restart) > 0 {
		fmt.Fprintf(&sb, "! %s is running; restart to apply %s: a2a-serve restart\n", res.Name, strings.Join(res.Restart, ", "))
	}
	_, err := fmt.Fprint(cfg.Out, sb.String())
	return err
}

// inSecretDiff reports whether the diff of --secret names the secret.
func inSecretDiff(changes []agentdir.Change, name string) bool {
	return slices.ContainsFunc(changes, func(c agentdir.Change) bool {
		return c.Setting == "secret" && (slices.Contains(c.Added, name) || slices.Contains(c.Removed, name))
	})
}

func formatChange(c agentdir.Change, secrets map[string]string) string {
	if c.Added == nil && c.Removed == nil {
		empty := "(default)"
		if kind, _, _ := strings.Cut(c.Setting, "-"); slices.Contains([]string{agentdir.ChannelTelegram, agentdir.ChannelDiscord, agentdir.ChannelEmail}, kind) {
			empty = "(none)"
		}
		return or(c.Old, empty) + " → " + or(c.New, empty)
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
