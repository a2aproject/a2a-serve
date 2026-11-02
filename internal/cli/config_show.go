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
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

type showFlags struct {
	defaults  bool
	asCommand bool
}

func (f *showFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.defaults, "defaults", false, "Also show the settings left at their defaults, with the default values")
	cmd.Flags().BoolVar(&f.asCommand, "as-command", false, "Print the `a2a serve <harness>` command which recreates the agent, with secrets as env:NAME")
}

func newConfigShowCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var show showFlags
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show the effective settings, and those waiting for a restart",
		Example: `  a2a serve config show
  a2a serve config show --defaults -o yaml
  a2a serve config show --as-command`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigShow(cmd, cfg, sf, show)
		},
	}
	show.register(cmd)
	return cmd
}

func runConfigShow(cmd *cobra.Command, cfg *globalConfig, sf *serverFlags, f showFlags) error {
	d, err := sf.agentDir()
	if err != nil {
		return err
	}
	v, err := agentctl.Show(cmd.Context(), d)
	if err != nil {
		return err
	}
	defaults := v.Defaults
	if !f.defaults {
		v.Defaults = nil
	}
	switch {
	case f.asCommand && cfg.IsJSON():
		return cfg.PrintJSON(map[string][]string{"command": v.Command})
	case f.asCommand:
		_, err := fmt.Fprintln(cfg.Out, agentdir.ShellJoin(v.Command))
		return err
	case cfg.output == outputYAML:
		data, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = cfg.Out.Write(data)
		return err
	case cfg.IsJSON():
		return cfg.PrintJSON(v)
	}
	v.Defaults = defaults
	_, err = fmt.Fprint(cfg.Out, renderConfig(v, f.defaults))
	return err
}

// renderConfig formats the settings grouped like the flags of `a2a serve
// <harness>`. all also lists settings left unset.
func renderConfig(v *agentctl.View, all bool) string {
	c, def := v.Config, v.Defaults
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-38s %s\n", fmt.Sprintf("%s (%s)", c.Name, c.Harness), v.Path)
	line := func(label, value string) {
		if value == "" && !all {
			return
		}
		if value == "" {
			value = "none"
		}
		fmt.Fprintf(&sb, "  %-12s %s\n", label, value)
	}
	withDefault := func(v, d string) string {
		if v != "" {
			return v
		}
		if d == "" {
			return ""
		}
		return d + " (default)"
	}
	line("description", c.Description)
	line("command", agentdir.ShellJoin(c.Command))
	line("workspace", withDefault(formatMount(c.Workspace), def.Workspace))
	var mounts []string
	for _, m := range c.Mounts {
		mounts = append(mounts, formatMount(m))
	}
	line("mounts", strings.Join(mounts, ", "))
	sandbox := withDefault(c.Sandbox, def.Sandbox)
	if img := withDefault(c.Image, def.Image); img != "" && sandbox != agentdir.SandboxLocal {
		sandbox = fmt.Sprintf("%-23s image %s", sandbox, img)
	}
	line("sandbox", sandbox)
	var secrets []string
	for _, name := range c.Secrets {
		secrets = append(secrets, name+" "+v.Secrets[name])
	}
	line("secrets", strings.Join(secrets, ", "))
	line("env", strings.Join(c.Env, ", "))
	network := []string{fmt.Sprintf("%-23s public %s", "private "+orNone(c.Network.Private), orNone(c.Network.Public))}
	for _, extra := range []struct{ label, value string }{
		{"tailscale-control-url", c.Network.TailscaleControlURL},
		{"ngrok-url", c.Network.NgrokURL},
		{agentdir.SecretTailscaleKey, v.Secrets[agentdir.SecretTailscaleKey]},
		{agentdir.SecretNgrokToken, v.Secrets[agentdir.SecretNgrokToken]},
		{agentdir.SecretRelayToken, v.Secrets[agentdir.SecretRelayToken]},
	} {
		if extra.value != "" {
			network = append(network, extra.label+" "+extra.value)
		}
	}
	line("network", strings.Join(network, ", "))
	var channels []string
	secretState := func(ref string) string {
		name, ok := strings.CutPrefix(ref, agentdir.SecretPrefix)
		if !ok {
			return "in config.yaml"
		}
		return v.Secrets[name]
	}
	if t := c.Channels.Telegram; t != nil {
		channels = append(channels, "telegram token "+secretState(t.Token))
	}
	if dc := c.Channels.Discord; dc != nil {
		channels = append(channels, "discord token "+secretState(dc.Token))
	}
	if e := c.Channels.Email; e != nil {
		channels = append(channels, fmt.Sprintf("email %s via %s, %s, password %s", e.Address, e.IMAP, e.SMTP, secretState(e.Password)))
	}
	line("channels", strings.Join(channels, "; "))
	var approvals []string
	for _, a := range []struct {
		label  string
		values []string
	}{{"allow", c.Approvals.Allow}, {"deny", c.Approvals.Deny}, {"approvers", c.Approvals.Approvers}} {
		if len(a.values) > 0 {
			approvals = append(approvals, a.label+" "+strings.Join(a.values, ", "))
		}
	}
	line("approvals", strings.Join(approvals, "; "))
	var identities []string
	for _, login := range slices.Sorted(maps.Keys(c.Network.Identities)) {
		identities = append(identities, login+"="+c.Network.Identities[login])
	}
	line("identities", strings.Join(identities, ", "))
	line("limits", formatLimits(c.Limits, def.Limits))
	port := "a free port, picked on the first start"
	if c.Port != 0 {
		port = strconv.Itoa(c.Port)
	}
	line("port", port)
	if v.Invalid != "" {
		fmt.Fprintf(&sb, "✗ invalid: %s\n", strings.ReplaceAll(v.Invalid, "\n", "; "))
	}
	if len(v.Pending) > 0 {
		fmt.Fprintf(&sb, "! restart to apply %s: a2a serve restart\n", strings.Join(v.Pending, ", "))
	}
	return sb.String()
}

func formatMount(spec string) string {
	if spec == "" {
		return ""
	}
	src, dst, ro := spec, "", false
	if s, ok := strings.CutSuffix(src, ":ro"); ok {
		src, ro = s, true
	}
	if s, d, ok := strings.Cut(src, ":"); ok {
		src, dst = s, d
	}
	out := src
	if dst != "" && dst != src {
		out += " → " + dst
	}
	if ro {
		out += " (ro)"
	}
	return out
}

func formatLimits(l, def agentdir.LimitsConfig) string {
	var parts []string
	defaults := 0
	add := func(label, v, d string) {
		if v == "" {
			v = d
			defaults++
		}
		parts = append(parts, label+" "+v)
	}
	pids := ""
	if l.PIDs != 0 {
		pids = strconv.Itoa(l.PIDs)
	}
	add("memory", l.Memory, def.Memory)
	add("cpus", l.CPUs, def.CPUs)
	add("pids", pids, strconv.Itoa(def.PIDs))
	out := strings.Join(parts, ", ")
	switch defaults {
	case 3:
		out += " (defaults)"
	case 0:
	default:
		out += " (unset ones are defaults)"
	}
	return out
}

func orNone(v string) string {
	if v == "" {
		return agentdir.NetworkNone
	}
	return v
}
