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

// Package agentkey names the agent process a conversation runs in. a2aacp
// stores the key with the session of each conversation: the owner's
// conversations run in the main agent process, which has no key, all
// anonymous conversations in one process, and the conversations of each
// authenticated user in a process of their own. Processes don't share a
// home, so a conversation can't read what another user's left there.
package agentkey

import (
	"strings"

	"github.com/a2aproject/a2a-serve/internal/auth"
)

// Anonymous is the key of the agent process of anonymous callers.
const Anonymous = "anonymous"

const userPrefix = "user:"

// Of returns the agent key of a new conversation that p starts. Calls
// without a principal come from the server itself.
func Of(p auth.Principal, ok bool) string {
	if !ok {
		return ""
	}
	switch p.Role {
	case auth.RoleAnonymous:
		return Anonymous
	case auth.RoleAuthenticated:
		return User(p.ID)
	}
	return ""
}

// User returns the key of the agent process of the principal id.
func User(id string) string { return userPrefix + id }

// UserOf returns the principal ID of a user's agent key.
func UserOf(key string) (string, bool) {
	return strings.CutPrefix(key, userPrefix)
}
