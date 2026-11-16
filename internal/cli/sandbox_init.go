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
	"strconv"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/sandbox/sandboxnet"
)

func newSandboxInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "sandbox-init " + sandboxnet.InitUsage,
		Hidden: true,
		Short:  "Run a command in the Docker sandbox with the server as its only network path",
		Long: `Runs as PID 1 of an agent's container, which the server starts with no
network. It routes every connection of the container through the egress
socket, drops every capability, and runs the command as uid:gid. With
--check, it sets up the network and drops the capabilities, then exits.
When the setup fails, it exits with status ` + strconv.Itoa(sandboxnet.SetupFailedExit) + `.`,
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if code := sandboxnet.RunInit(args, cmd.ErrOrStderr()); code != 0 {
				return clierr.Exit(code, "")
			}
			return nil
		},
	}
}
