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
	"errors"
	"fmt"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
)

func newMailboxReplyCmd(cfg *globalConfig) *cobra.Command {
	var (
		rf            nodeClientFlags
		status        string
		text          string
		working       string
		reject        string
		final         string
		inputRequired string
		fail          string
		artifacts     []string
		key           string
	)
	cmd := &cobra.Command{
		Use:   "reply <task-id>",
		Short: "Post progress, a question or the result of a task",
		Long:  "Post progress, a question or the result of a task.\n\n" + flagparse.SecretHelp,
		Example: `  a2a-serve mailbox reply <task> --working "on it"
  a2a-serve mailbox reply <task> --input-required "Which branch?"
  a2a-serve mailbox reply <task> --final "Done" --artifact report.pdf
  a2a-serve mailbox reply <task> --fail "Could not reach the repo"
  a2a-serve mailbox reply <task> --reject "Not something this node does"`,
		Args: namedArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID := args[0]
			var ev mailboxapi.Event
			set := 0
			for _, f := range []struct{ val, state string }{
				{working, "working"}, {final, "completed"}, {inputRequired, "input-required"}, {fail, "failed"}, {reject, "rejected"},
			} {
				if f.val != "" {
					set++
					ev.State, ev.Text = f.state, f.val
				}
			}
			if status != "" {
				set++
				ev.State, ev.Text = status, text
			} else if text != "" {
				return clierr.Usage("--text goes with --status")
			}
			if set == 0 && len(artifacts) > 0 {
				ev.State, set = "completed", 1
			}
			if set != 1 {
				return clierr.Usage("set exactly one of --working, --final, --input-required, --fail, --reject or --status")
			}
			if _, ok := mailboxapi.ParseState(ev.State); !ok {
				return clierr.Usage(fmt.Sprintf("unknown state %q", ev.State))
			}
			for _, path := range artifacts {
				art, err := mailboxapi.FileArtifact(path)
				if errors.Is(err, fs.ErrNotExist) {
					e := clierr.Usage(fmt.Sprintf("--artifact %s: %v", path, err))
					e.Hint = "pass the path of an existing file to --artifact"
					return e
				}
				if err != nil {
					return err
				}
				ev.Artifacts = append(ev.Artifacts, art)
			}
			ev.IdempotencyKey = key
			if ev.IdempotencyKey == "" {
				ev.IdempotencyKey = mailboxapi.ContentKey(taskID, ev)
			}
			ctx, cancel := nodeContext(cmd, cfg)
			defer cancel()
			c, err := rf.nodeClient(cfg)
			if err != nil {
				return err
			}
			res, err := c.PostEvent(ctx, taskID, ev)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(res)
			}
			note := ""
			if res.Duplicate {
				note = " (already applied, ignored the retry)"
			}
			_, err = fmt.Fprintf(cfg.Out, "task %s: %s%s\n", res.TaskID, res.State, note)
			return err
		},
	}
	rf.register(cmd.Flags())
	f := cmd.Flags()
	f.StringVar(&status, "status", "", "Task state: working, input-required, completed, failed, rejected; each also has a flag of its own")
	f.StringVar(&text, "text", "", "Status message, or the result for completed (with --status)")
	f.StringVar(&working, "working", "", "Report progress with this message")
	f.StringVar(&final, "final", "", "Complete the task with this result")
	f.StringVar(&inputRequired, "input-required", "", "Ask the caller this question")
	f.StringVar(&fail, "fail", "", "Fail the task with this reason")
	f.StringVar(&reject, "reject", "", "Reject the task with this reason")
	f.StringArrayVar(&artifacts, "artifact", nil, "Attach a file as an artifact (repeatable)")
	f.StringVar(&key, "key", "", "Idempotency key (default: derived from the task and the event content)")
	return cmd
}
