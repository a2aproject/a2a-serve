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

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
)

// newPairMovedCmd returns a hidden `pair <name>` command of the caller
// side, which moved to `a2a serve tokens <replacement>`, that fails naming
// the new command. usage stands for the arguments when none are given.
func newPairMovedCmd(name, replacement, usage string) *cobra.Command {
	return &cobra.Command{
		Use:                name,
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			line := "a2a serve tokens " + replacement
			switch {
			case len(args) > 0:
				line += " " + agentdir.ShellJoin(args)
			case usage != "":
				line += " " + usage
			}
			return clierr.Usage(fmt.Sprintf("`a2a serve pair %s` moved: run `%s`", name, line))
		},
	}
}
