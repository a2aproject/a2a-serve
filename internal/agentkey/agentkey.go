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
// conversations run in the main agent process, which has no key, the
// conversations of guests in one process, and the conversations of each
// member in a process of their own. A member that is another agent gets a
// process for each person behind its calls, which it names with an opaque
// caller ID. Processes don't share a home, so a conversation can't read
// what another user's left there.
package agentkey

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/auth"
)

// Guest is the key of the agent process of guests. Sessions store it, so
// it keeps the name of the role before guests were called guests.
const Guest = "anonymous"

const userPrefix = "user:"

// Of returns the agent key of a new conversation that p starts. Calls
// without a principal come from the server itself.
func Of(p auth.Principal, ok bool) string {
	if !ok {
		return ""
	}
	switch p.Role {
	case auth.RoleGuest:
		return Guest
	case auth.RoleMember:
		if p.Caller != "" {
			return Caller(p.ID, p.Caller)
		}
		return User(p.ID)
	}
	return ""
}

// User returns the key of the agent process of the principal id.
func User(id string) string { return userPrefix + id }

// callerLen is the length of the part of a key that names a caller.
const callerLen = 16

// Caller returns the key of the agent process that the principal id, an
// agent, gets for the person it names caller.
func Caller(id, caller string) string {
	sum := sha256.Sum256([]byte(caller))
	return User(id) + "#" + hex.EncodeToString(sum[:])[:callerLen]
}

// UserOf returns the principal ID of a user's agent key, also of one that
// names a caller.
func UserOf(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, userPrefix)
	if !ok {
		return "", false
	}
	if id, _, ok := splitCaller(rest); ok {
		return id, true
	}
	return rest, true
}

// CallerOf returns the part of a user's agent key that names a caller, or
// "". It is safe in paths and container names.
func CallerOf(key string) string {
	rest, ok := strings.CutPrefix(key, userPrefix)
	if !ok {
		return ""
	}
	_, caller, _ := splitCaller(rest)
	return caller
}

// CallerPrefix starts the keys of the agent processes that the principal
// id gets for the people behind its calls.
func CallerPrefix(id string) string { return User(id) + "#" }

// splitCaller splits the principal ID and caller of a key without its
// prefix. Principal IDs never end in '#' and 16 hex digits.
func splitCaller(rest string) (string, string, bool) {
	i := len(rest) - callerLen - 1
	if i <= 0 || rest[i] != '#' {
		return "", "", false
	}
	caller := rest[i+1:]
	if _, err := hex.DecodeString(caller); err != nil || strings.ToLower(caller) != caller {
		return "", "", false
	}
	return rest[:i], caller, true
}
