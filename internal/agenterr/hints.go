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

package agenterr

import "fmt"

// hintTexts word the hints as the commands that fix them. A %s
// stands for the hint's argument.
var hintTexts = map[Hint]string{
	HintStart:             "start it with `a2a-serve start --detach`",
	HintStartOrURL:        "start it with `a2a-serve start --detach`, or pass the agent's URL with --url",
	HintRelayRun:          "start it with `a2a-serve relay run`",
	HintPairInvite:        "pair them first with `a2a-serve pair invite`",
	HintLinkChat:          "link it with a code from `a2a-serve pair invite`",
	HintPairSet:           "change it with `a2a-serve pair set %s`",
	HintPairSetOwner:      "make it an owner with `a2a-serve pair set %s --owner`",
	HintPairRm:            "pick another name, or run `a2a-serve pair rm %s` first",
	HintPairRmLocal:       "`a2a-serve pair rm %s` revokes its token and chat links, and the next start issues a new token",
	HintPairInviteWebhook: "webhook nodes come from `a2a-serve pair invite --webhook`",
}

// Text returns the message of err followed by its worded hint, for
// places that keep only text, such as a schedule's last error.
func Text(err error) string {
	if err == nil {
		return ""
	}
	if hint := HintText(err); hint != "" {
		return err.Error() + ": " + hint
	}
	return err.Error()
}

// HintText words the hint of err, or returns "" if it has none.
func HintText(err error) string {
	h, arg, ok := HintOf(err)
	if !ok {
		return ""
	}
	text, ok := hintTexts[h]
	if !ok {
		return ""
	}
	if arg == "" {
		return text
	}
	return fmt.Sprintf(text, arg)
}
