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
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func newContactRefreshCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh [name]",
		Short: "Fetch the agent cards of contacts again",
		Long: `Fetch the agent card of a contact, or of every contact, and keep its
description and skills, which the agent's instructions list. The running
server also does this every --contact-refresh (default 1h). While the agent
is stopped, only contacts reached directly can be refreshed.`,
		Example: "  a2a-serve contact refresh\n  a2a-serve contact refresh coder",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			c, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			var req controlapi.ContactRefreshRequest
			if len(args) == 1 {
				req.Name = args[0]
			}
			refreshed, err := c.RefreshContacts(cmd.Context(), req)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return output.PrintList(cfg.Printer, refreshed)
			}
			if len(refreshed) == 0 {
				_, err := fmt.Fprintln(cfg.Out, "no contacts; add one with `a2a-serve contact add <name>`")
				return err
			}
			var sb strings.Builder
			for _, info := range refreshed {
				fmt.Fprintf(&sb, "✓ refreshed %s\n", info.Name)
				writeContactCard(&sb, &info)
			}
			_, err = fmt.Fprint(cfg.Out, sb.String())
			return err
		},
	}
}
