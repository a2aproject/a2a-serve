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

package clierr

import (
	"fmt"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
)

// agentHints word the hints of errors as the commands that fix them. A %s
// stands for the hint's argument.
var agentHints = map[agenterr.Hint]string{
	agenterr.HintStart:             "start it with `a2a-serve start --detach`",
	agenterr.HintStartOrURL:        "start it with `a2a-serve start --detach`, or pass the agent's URL with --url",
	agenterr.HintRelayRun:          "start it with `a2a-serve relay run`",
	agenterr.HintPairInvite:        "pair them first with `a2a-serve pair invite`",
	agenterr.HintLinkChat:          "link it with a code from `a2a-serve pair invite`",
	agenterr.HintPairSet:           "change it with `a2a-serve pair set %s`",
	agenterr.HintPairSetOwner:      "make it an owner with `a2a-serve pair set %s --owner`",
	agenterr.HintPairRm:            "pick another name, or run `a2a-serve pair rm %s` first",
	agenterr.HintPairRmLocal:       "`a2a-serve pair rm %s` revokes its token and chat links, and the next start issues a new token",
	agenterr.HintPairInviteWebhook: "webhook nodes come from `a2a-serve pair invite --webhook`",
}

// Text returns the message of err followed by its worded hint, for
// places that keep only text, such as a schedule's last error.
func Text(err error) string {
	if err == nil {
		return ""
	}
	if hint := hintText(err); hint != "" {
		return err.Error() + ": " + hint
	}
	return err.Error()
}

// hintText words the [agenterr.Hint] of err, or returns "" if it has none
// the CLI knows.
func hintText(err error) string {
	h, arg, ok := agenterr.HintOf(err)
	if !ok {
		return ""
	}
	text, ok := agentHints[h]
	if !ok {
		return ""
	}
	if arg == "" {
		return text
	}
	return fmt.Sprintf(text, arg)
}
