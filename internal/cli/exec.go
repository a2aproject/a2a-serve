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

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/localsrv"
)

func newExecCmd(cfg *globalConfig) *cobra.Command {
	var (
		tf testServerFlags
		s  localsrv.ScriptOptions
	)
	cmd := &cobra.Command{
		Use:   "exec <command>",
		Short: "Run a test server which runs a shell command per message",
		Long: `Run a test server, an agent driven by a script. For each message:

  - the command runs with sh -c, with the message text on stdin, and
    ` + localsrv.EnvTaskID + ` and ` + localsrv.EnvContextID + ` in its environment, so it can keep
    state per task or conversation;
  - its stdout is the reply artifact, sent when it exits, or streamed with
    --stream (line by line) or --chunk (split at a delimiter);
  - a non-zero exit fails the task, with stderr as the message.`,
		Example: `  a2a-serve start exec 'tr a-z A-Z'
  a2a-serve start exec --stream 'for i in 1 2 3; do echo "step $i"; sleep 1; done'
  a2a-serve start exec 'mkdir -p "/tmp/$A2A_CONTEXT_ID" && tee -a "/tmp/$A2A_CONTEXT_ID/log"'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if s.Stream && s.Chunk != "" {
				return clierr.Usage("--stream and --chunk are mutually exclusive")
			}
			s.Command = args[0]
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			return localsrv.RunScript(ctx, tf.options(cfg), s)
		},
	}
	tf.register(cmd)
	f := cmd.Flags()
	f.StringVar(&s.Chunk, "chunk", "", "Stream stdout in chunks split at this delimiter")
	f.BoolVar(&s.Stream, "stream", false, "Stream stdout line by line")
	return cmd
}
