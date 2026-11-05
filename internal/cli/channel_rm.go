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
	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func newChannelRmCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:       "rm <telegram|discord|email>",
		Aliases:   []string{"delete"},
		Short:     "Remove a messenger",
		Args:      namedArgs,
		ValidArgs: agentdir.ChannelKinds,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			m, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			if err := m.DeleteChannel(cmd.Context(), args[0]); err != nil {
				return err
			}
			return cfg.PrintRemoved("channel", args[0], "✓ removed "+args[0])
		},
	}
}
