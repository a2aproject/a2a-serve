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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/secretref"
)

// maxWakeSize bounds the wake body read from stdin.
const maxWakeSize = 16 << 20

func newMailboxVerifyCmd(cfg *globalConfig) *cobra.Command {
	var (
		rf        nodeClientFlags
		signature string
	)
	cmd := &cobra.Command{
		Use:   "verify --signature <header> < wake.json",
		Short: "Check that a wake on stdin was sent by the agent hosting the node",
		Long: `Check that the wake body on stdin was signed by the agent hosting the node,
within ` + mailboxapi.MaxSkew.String() + ` of now. Pass the value of the ` + mailboxapi.SignatureHeader + `
header with --signature. The key comes from the node token, so the command
makes no request. It fails for a wake that anyone else sent; don't act on one.

` + secretref.Help,
		Example: `  printf '%s' "$WAKE_BODY" | a2a-serve mailbox verify --signature "$WAKE_SIGNATURE"`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if signature == "" {
				return clierr.Usage("--signature is required: the value of the " + mailboxapi.SignatureHeader + " header")
			}
			token, err := rf.nodeToken(cfg)
			if err != nil {
				return err
			}
			body, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxWakeSize))
			if err != nil {
				return fmt.Errorf("reading the wake: %w", err)
			}
			if err := mailboxapi.Verify(mailboxapi.WakeKey(token), signature, body, time.Now(), mailboxapi.MaxSkew); err != nil {
				return errors.New("the wake isn't signed by the agent hosting this node, or was signed too long ago: don't act on it")
			}
			var env mailboxapi.Envelope
			if err := json.Unmarshal(body, &env); err != nil {
				return fmt.Errorf("decoding the wake: %w", err)
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(map[string]any{"valid": true, "id": env.ID, "kind": env.Kind, "taskId": env.TaskID})
			}
			_, err = fmt.Fprintf(cfg.Out, "✓ wake %s (%s, task %s) is from the agent hosting this node\n", env.ID, env.Kind, env.TaskID)
			return err
		},
	}
	cmd.Flags().StringVar(&signature, "signature", "", "Value of the "+mailboxapi.SignatureHeader+" header of the wake")
	rf.register(cmd.Flags())
	return cmd
}
