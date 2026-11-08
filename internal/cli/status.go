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
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/project"
	"github.com/a2aproject/a2a-serve/internal/recipe"
)

// urlFurthest is the value of a bare --url: the URL that reaches furthest.
const urlFurthest = "furthest"

func newStatusCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the agent runs, its URLs, messengers and who may call it, or print one of its URLs",
		Long: `Show whether the agent runs, its URLs, messengers and who may call it.

--url prints only the agent's URL that reaches furthest: public, then
private, then local. --url local, --url private and --url public pick one;
--url=local does the same. --url local also works while the agent is
stopped, once its first start picked its port.`,
		Example: `  a2a-serve status
  a2a send -a "$(a2a-serve status --url)" "hello"
  a2a-serve status --url=private`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && scope == urlFurthest {
				scope = args[0]
				return nil
			}
			return cobra.NoArgs(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if scope != urlFurthest {
				if err := checkScope(scope); err != nil {
					return clierr.Usage(err.Error())
				}
			}
			d, err := sf.serverDir()
			if err != nil {
				return err
			}
			client := sf.controlClient(d)
			st, err := client.Status(cmd.Context())
			if scope != "" {
				return printStatusURL(cfg, d, st, err, scope)
			}
			problem := configProblem(d)
			if errors.Is(err, agenterr.ErrNotRunning) {
				return printStopped(cfg, d, problem)
			}
			if err != nil {
				return err
			}
			var pending []string
			if problem == "" {
				if pending, _, err = agentctl.Pending(cmd.Context(), d, client); err != nil {
					return err
				}
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(statusJSON{Name: st.Name, Harness: st.Harness, Dir: st.Dir, Running: true, Status: st, Pending: pending, Recipe: recipeOf(d), ConfigError: problem})
			}
			next, err := restartVars(d, problem)
			if err != nil {
				return err
			}
			return printStatus(cfg, st, pending, next, recipeOf(d), problem)
		},
	}
	f := cmd.Flags()
	f.StringVar(&scope, "url", "", "Print only the agent's `scope` URL: local, private or public; without a value, the one that reaches furthest")
	f.Lookup("url").NoOptDefVal = urlFurthest
	hideBareValue(f, "url")
	must(cmd.RegisterFlagCompletionFunc("url", fixedCompletions(scopeLocal, scopePrivate, scopePublic)))
	sf.addControlFlags(f)
	cmd.SetUsageFunc(sectionedUsage)
	return cmd
}

// printStopped prints the status of the stopped agent in d, whose
// config.yaml doesn't load if problem is set.
func printStopped(cfg *globalConfig, d agentdir.Dir, problem string) error {
	name, harness := d.NameOrDefault(), ""
	if conf, err := d.ReadConfig(); err == nil {
		harness = conf.Harness
	}
	if cfg.IsJSON() {
		return cfg.PrintJSON(statusJSON{Name: name, Harness: harness, Dir: d.Root, Recipe: recipeOf(d), ConfigError: problem})
	}
	var sb strings.Builder
	sb.WriteString(name)
	if harness != "" {
		fmt.Fprintf(&sb, " (%s)", harness)
	}
	fmt.Fprintf(&sb, " in %s is not running; start it with `a2a-serve start --detach`\n", d.Root)
	writeConfigProblem(&sb, problem)
	_, err := fmt.Fprint(cfg.Out, sb.String())
	return err
}

// printStatusURL prints one URL of the agent in d, whose status is st or
// err. A stopped agent has only its local URL, from the saved port.
func printStatusURL(cfg *globalConfig, d agentdir.Dir, st *controlapi.Status, err error, scope string) error {
	if scope == urlFurthest {
		scope = ""
	}
	if errors.Is(err, agenterr.ErrNotRunning) && scope == scopeLocal {
		conf, cerr := d.LoadConfig()
		if cerr != nil {
			return cerr
		}
		if conf.LocalURL() == "" {
			return &agenterr.NotRunningError{Msg: "no port has been picked yet: the first start picks it", Hint: agenterr.HintStart}
		}
		st, err = &controlapi.Status{Local: conf.LocalURL()}, nil
	}
	if err != nil {
		return err
	}
	u, err := statusURL(st, scope)
	if err != nil {
		return clierr.Usage(err.Error())
	}
	if u == st.Private && st.PrivateNote != "" {
		if _, err := fmt.Fprintf(cfg.stderr(), "note: this private URL %s\n", st.PrivateNote); err != nil {
			return err
		}
	}
	for _, p := range []struct {
		url     string
		problem *controlapi.NetworkProblem
	}{{st.Private, st.PrivateProblem}, {st.Public, st.PublicProblem}} {
		if p.problem == nil || u != p.url {
			continue
		}
		if _, err := fmt.Fprintf(cfg.stderr(), "note: this URL doesn't work now: %s\n", p.problem.Line()); err != nil {
			return err
		}
		break
	}
	if cfg.IsJSON() {
		return cfg.PrintJSON(map[string]string{"url": u})
	}
	_, err = fmt.Fprintln(cfg.Out, u)
	return err
}

// statusJSON is the status of a running or stopped agent; the fields of
// [controlapi.Status] are only set while it runs.
type statusJSON struct {
	Name    string `json:"name"`
	Harness string `json:"agent"`
	Dir     string `json:"dir"`
	Running bool   `json:"running"`
	*controlapi.Status
	// Pending are the settings saved since the agent started.
	Pending []string `json:"pending,omitempty"`
	// Recipe is the recipe the agent came from, if any.
	Recipe string `json:"recipe,omitempty"`
	// ConfigError is why config.yaml doesn't load, if it doesn't.
	ConfigError string `json:"configError,omitempty"`
}

// recipeOf describes the recipe the agent d came from and its commit, or
// returns "" for an agent created without one.
func recipeOf(d agentdir.Dir) string {
	rec, err := recipe.ReadRecord(d)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("reading the record of the agent's recipe", "error", err)
		}
		return ""
	}
	src := rec.Source()
	if src.Local() {
		return src.Dir
	}
	return src.Describe() + " (" + shortCommit(rec.Commit) + ")"
}

// writeNetworkProblem writes why the URL u of the label scope doesn't work,
// under u, or in its place when the server left it out.
func writeNetworkProblem(sb *strings.Builder, label, u string, p *controlapi.NetworkProblem) {
	if p == nil {
		return
	}
	if u != "" {
		label = ""
	}
	fmt.Fprintf(sb, "  %-8s ! %s\n", label, p.Line())
}

// restartVars returns the variables of the MCP servers that the agent in d
// gets once it restarts, or nil if its config has a problem.
func restartVars(d agentdir.Dir, problem string) (func(string) (string, bool), error) {
	if problem != "" {
		return nil, nil
	}
	saved, err := d.ReadConfig()
	if err != nil {
		return nil, err
	}
	stored, err := d.LoadSecrets()
	if err != nil {
		return nil, err
	}
	return project.Vars(saved, stored), nil
}

// printStatus prints the status st of the running agent. next returns the
// variables of its MCP servers once it restarts, if not nil.
func printStatus(cfg *globalConfig, st *controlapi.Status, pending []string, next func(string) (string, bool), fromRecipe, configProblem string) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%s) running for %s, pid %d\n", st.Name, st.Harness, time.Since(st.Started).Round(time.Second), st.PID)
	fmt.Fprintf(&sb, "  dir      %s\n", st.Dir)
	if fromRecipe != "" {
		fmt.Fprintf(&sb, "  recipe   %s\n", fromRecipe)
	}
	fmt.Fprintf(&sb, "  local    %s\n", st.Local)
	if st.Private != "" {
		fmt.Fprintf(&sb, "  private  %s\n", st.Private)
		if st.PrivateNote != "" {
			fmt.Fprintf(&sb, "  %-8s (%s)\n", "", st.PrivateNote)
		}
	}
	writeNetworkProblem(&sb, "private", st.Private, st.PrivateProblem)
	if st.Public != "" {
		fmt.Fprintf(&sb, "  public   %s\n", st.Public)
	}
	writeNetworkProblem(&sb, "public", st.Public, st.PublicProblem)
	for _, c := range st.Messengers {
		fmt.Fprintf(&sb, "  %-8s %s\n", c.Name, c.Account)
		if c.Error != "" {
			fmt.Fprintf(&sb, "  %-8s ! failing, retrying: %s\n", "", c.Error)
		}
	}
	if len(st.Owners) > 0 {
		fmt.Fprintf(&sb, "  owner    %s\n", strings.Join(st.Owners, ", "))
	}
	if a := st.Access; a != nil {
		for i, line := range a.Summary {
			label := ""
			if i == 0 {
				label = "access"
			}
			fmt.Fprintf(&sb, "  %-8s %s\n", label, line)
		}
		for i, line := range a.Sandbox {
			label := ""
			if i == 0 {
				label = "sandbox"
			}
			fmt.Fprintf(&sb, "  %-8s %s\n", label, line)
		}
		for _, note := range a.SandboxNotes {
			fmt.Fprintf(&sb, "  %-8s ! %s\n", "", note)
		}
		if a.GuestNote != "" {
			fmt.Fprintf(&sb, "  %-8s ! %s\n", "", a.GuestNote)
		}
	}
	states := make([]string, 0, len(st.Tasks))
	for _, s := range slices.Sorted(maps.Keys(st.Tasks)) {
		states = append(states, fmt.Sprintf("%d %s", st.Tasks[s], s))
	}
	if len(states) > 0 {
		fmt.Fprintf(&sb, "  tasks    %s\n", strings.Join(states, ", "))
	}
	fmt.Fprintf(&sb, "  contacts %d, webhook nodes %d\n", st.Contacts, st.Nodes)
	if st.Approvals > 0 {
		fmt.Fprintf(&sb, "  ! %d approval(s) waiting for an answer: a2a-serve inspect\n", st.Approvals)
	}
	for _, m := range st.MCPServers {
		line := m.Name + ": " + m.Summary
		if m.Disabled {
			line += fmt.Sprintf(" (off; turn it on with `a2a-serve config unset disable-mcp %s`)", m.Name)
		}
		var unset, saved []string
		for _, name := range m.Missing {
			if next == nil {
				unset = append(unset, name)
				continue
			}
			if _, ok := next(name); ok {
				saved = append(saved, name)
				continue
			}
			unset = append(unset, name)
		}
		var problems []string
		if len(unset) > 0 {
			problems = append(problems, fmt.Sprintf("not passed to the agent, unset: %s (pass them with --secret)", strings.Join(unset, ", ")))
		}
		if len(saved) > 0 {
			problems = append(problems, fmt.Sprintf("stored, passed after `a2a-serve restart`: %s", strings.Join(saved, ", ")))
		}
		if len(problems) > 0 {
			line += " ! " + strings.Join(problems, "; ")
		}
		fmt.Fprintf(&sb, "  mcp      %s\n", line)
	}
	if len(st.Skills) > 0 {
		names := make([]string, len(st.Skills))
		for i, sk := range st.Skills {
			names[i] = sk.Name
		}
		fmt.Fprintf(&sb, "  skills   %s\n", strings.Join(names, ", "))
	}
	if st.ProjectError != "" {
		fmt.Fprintf(&sb, "  project  ! %s\n", strings.ReplaceAll(st.ProjectError, "\n", "; "))
	}
	label := "tools"
	for _, l := range []struct {
		name    string
		entries []string
	}{{"allow", st.AllowTools}, {"deny", st.DenyTools}} {
		if len(l.entries) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "  %-8s %s %s\n", label, l.name, strings.Join(l.entries, ", "))
		label = ""
	}
	if b := st.Backup; b != nil {
		last := "none yet"
		if !b.Last.IsZero() {
			last = "last " + b.Last.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&sb, "  backup   %s, %s, next %s\n", b.To, last, b.Next.Local().Format("2006-01-02 15:04"))
		if b.LastError != "" {
			fmt.Fprintf(&sb, "  %-8s ! the last snapshot failed: %s\n", "", b.LastError)
		}
	}
	if len(pending) > 0 {
		fmt.Fprintf(&sb, "  pending  %s (a2a-serve restart)\n", strings.Join(pending, ", "))
	}
	writeConfigProblem(&sb, configProblem)
	_, err := fmt.Fprint(cfg.Out, sb.String())
	return err
}
