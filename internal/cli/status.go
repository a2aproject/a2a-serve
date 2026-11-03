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
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// urlFurthest is the value of a bare --url: the URL which reaches furthest.
const urlFurthest = "furthest"

func newStatusCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the agent runs, its URLs and channels, or print one of its URLs",
		Long: `Show whether the agent runs, its URLs and channels.

--url prints only the agent's URL which reaches furthest: public, then
private, then local. --url=local, --url=private and --url=public pick one.
--url=local also works while the agent is stopped, once its first start
picked its port.`,
		Example: `  a2a-serve status
  a2a send -a "$(a2a-serve status --url)" "hello"
  a2a-serve status --url=private`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			st, err := sf.controlClient(d).Status(cmd.Context())
			if scope != "" {
				return printStatusURL(cfg, d, st, err, scope)
			}
			if errors.Is(err, controlapi.ErrNotRunning) {
				conf, cerr := d.LoadConfig()
				if cerr != nil {
					return cerr
				}
				if cfg.IsJSON() {
					return cfg.PrintJSON(statusJSON{Name: conf.Name, Harness: conf.Harness, Dir: d.Root})
				}
				_, err := fmt.Fprintf(cfg.Out, "%s (%s) in %s is not running; start it with `a2a-serve start --detach`\n", conf.Name, conf.Harness, d.Root)
				return err
			}
			if err != nil {
				return err
			}
			pending, _, err := agentctl.Pending(cmd.Context(), d)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(statusJSON{Name: st.Name, Harness: st.Harness, Dir: st.Dir, Running: true, Status: st, Pending: pending})
			}
			return printStatus(cfg, st, pending)
		},
	}
	f := cmd.Flags()
	f.StringVar(&scope, "url", "", "Print only the agent's `scope` URL: local, private or public; without a value, the one which reaches furthest")
	f.Lookup("url").NoOptDefVal = urlFurthest
	must(cmd.RegisterFlagCompletionFunc("url", fixedCompletions(controlapi.ScopeLocal, controlapi.ScopePrivate, controlapi.ScopePublic)))
	sf.addControlFlags(f)
	return cmd
}

// printStatusURL prints one URL of the agent in d, whose status is st or
// err. A stopped agent has only its local URL, from the saved port.
func printStatusURL(cfg *globalConfig, d agentdir.Dir, st *controlapi.Status, err error, scope string) error {
	if scope == urlFurthest {
		scope = ""
	}
	if errors.Is(err, controlapi.ErrNotRunning) && scope == controlapi.ScopeLocal {
		conf, cerr := d.LoadConfig()
		if cerr != nil {
			return cerr
		}
		if conf.LocalURL() == "" {
			return err
		}
		st, err = &controlapi.Status{Local: conf.LocalURL()}, nil
	}
	if err != nil {
		return err
	}
	u, err := st.URL(scope)
	if err != nil {
		return clierr.Usage(err.Error())
	}
	if u == st.Private && st.PrivateNote != "" {
		if _, err := fmt.Fprintf(cfg.stderr(), "note: this private URL %s\n", st.PrivateNote); err != nil {
			return err
		}
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
	Harness string `json:"harness"`
	Dir     string `json:"dir"`
	Running bool   `json:"running"`
	*controlapi.Status
	// Pending are the settings saved since the agent started.
	Pending []string `json:"pending,omitempty"`
}

func printStatus(cfg *globalConfig, st *controlapi.Status, pending []string) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%s) running for %s, pid %d\n", st.Name, st.Harness, time.Since(st.Started).Round(time.Second), st.PID)
	fmt.Fprintf(&sb, "  dir      %s\n  local    %s\n", st.Dir, st.Local)
	if st.Private != "" {
		fmt.Fprintf(&sb, "  private  %s\n", st.Private)
		if st.PrivateNote != "" {
			fmt.Fprintf(&sb, "  %-8s (%s)\n", "", st.PrivateNote)
		}
	}
	if st.Public != "" {
		fmt.Fprintf(&sb, "  public   %s\n", st.Public)
	}
	for _, c := range st.Channels {
		fmt.Fprintf(&sb, "  %-8s %s\n", c.Name, c.Account)
		if c.Error != "" {
			fmt.Fprintf(&sb, "  %-8s ! failing, retrying: %s\n", "", c.Error)
		}
	}
	if len(st.Owners) > 0 {
		fmt.Fprintf(&sb, "  owner    %s\n", strings.Join(st.Owners, ", "))
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
		if len(m.Missing) > 0 {
			line += fmt.Sprintf(" ! not passed to the harness, unset: %s (pass them with --secret)", strings.Join(m.Missing, ", "))
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
		fmt.Fprintf(&sb, "  project  ! %s\n", st.ProjectError)
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
	_, err := fmt.Fprint(cfg.Out, sb.String())
	return err
}
