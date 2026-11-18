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
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/boundary"
)

func newSandboxRelayCmd() *cobra.Command {
	var flags boundary.RelayFlags
	cmd := &cobra.Command{
		Use:    "sandbox-relay",
		Hidden: true,
		Short:  "Relay the sockets of an agent's container to the server",
		Long: `Runs in a container next to an agent's container with Docker Desktop, which
can't mount the server's sockets. It serves the egress socket, and the
socket through which the agent reaches the server, in a volume both
containers share, and forwards their connections to the server. The key of
the egress socket is in $` + boundary.EnvRelayKey + `.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			return boundary.RunRelay(ctx, flags, os.Getenv(boundary.EnvRelayKey), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&flags.EgressListen, "egress-listen", "", "path of the egress socket")
	cmd.Flags().StringVar(&flags.EgressTo, "egress-to", "", "where the egress socket's connections go: tcp://host:port or unix:///path")
	cmd.Flags().StringVar(&flags.ServerListen, "server-listen", "", "path of the socket through which the agent reaches the server")
	cmd.Flags().StringVar(&flags.ServerTo, "server-to", "", "where the server socket's connections go: tcp://host:port or unix:///path")
	for _, f := range []string{"egress-listen", "egress-to"} {
		if err := cmd.MarkFlagRequired(f); err != nil {
			panic(err)
		}
	}
	cmd.MarkFlagsRequiredTogether("server-listen", "server-to")
	return cmd
}
