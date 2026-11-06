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

	"github.com/a2aproject/a2a-serve/internal/localsrv"
)

func newEchoCmd(cfg *globalConfig) *cobra.Command {
	var tf testServerFlags
	cmd := &cobra.Command{
		Use:     "echo",
		Short:   "Run a test server that replies with the message",
		Example: "  a2a-serve start echo --port 9000\n  a2a send -a http://127.0.0.1:9000 hello",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			return localsrv.RunEcho(ctx, tf.options(cfg))
		},
	}
	tf.register(cmd)
	return cmd
}
