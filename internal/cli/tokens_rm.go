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

	"github.com/spf13/cobra"
)

func newTokensRmCmd(cfg *globalConfig) *cobra.Command {
	return &cobra.Command{
		Use:               "rm <agent-url>",
		Short:             "Delete the token saved for an agent",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: savedTokenCompletions(cfg),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := cfg.credentialStore()
			if err != nil {
				return err
			}
			ok, err := store.Delete(args[0])
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no saved token for %s", args[0])
			}
			return cfg.PrintRemoved("token", args[0], "✓ deleted the token for "+args[0])
		},
	}
}
