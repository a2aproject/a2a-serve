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

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func newURLCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var local, private, public bool
	cmd := &cobra.Command{
		Use:   "url",
		Short: "Print the agent's URL",
		Long: `Print the agent's URL which reaches furthest: public, then private, then
local. --local, --private and --public pick one. --local also works while
the agent is stopped, once its first start picked its port.`,
		Example: `  a2a send -a "$(a2a serve url)" "hello"
  a2a serve url --private`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			st, err := sf.controlClient(d).Status(cmd.Context())
			if errors.Is(err, controlapi.ErrNotRunning) && local {
				conf, cerr := d.LoadConfig()
				if cerr != nil {
					return cerr
				}
				if conf.LocalURL() == "" {
					return err
				}
				st = &controlapi.Status{Local: conf.LocalURL()}
			} else if err != nil {
				return err
			}
			var scope string
			switch {
			case local:
				scope = controlapi.ScopeLocal
			case private:
				scope = controlapi.ScopePrivate
			case public:
				scope = controlapi.ScopePublic
			}
			u, err := st.URL(scope)
			if err != nil {
				return err
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
		},
	}
	f := cmd.Flags()
	f.BoolVar(&local, "local", false, "Print the URL on this machine")
	f.BoolVar(&private, "private", false, "Print the URL on the private network")
	f.BoolVar(&public, "public", false, "Print the URL on the internet")
	cmd.MarkFlagsMutuallyExclusive("local", "private", "public")
	sf.addControlFlags(f)
	return cmd
}
