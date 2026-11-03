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

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func newContactRmCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"delete"},
		Short:   "Remove a contact",
		Args:    cobra.ExactArgs(1),
		ValidArgsFunction: controlCompletions(sf, func(ctx context.Context, c *controlapi.Client) ([]string, error) {
			cs, err := c.Contacts(ctx)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(cs))
			for _, x := range cs {
				names = append(names, x.Name)
			}
			return names, nil
		}),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			c, _, done, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, done)
			if err := c.DeleteContact(cmd.Context(), args[0]); err != nil {
				return err
			}
			return cfg.PrintRemoved("contact", args[0], "✓ removed contact "+args[0])
		},
	}
}
