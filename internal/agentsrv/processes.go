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

package agentsrv

import (
	"context"
	"log/slog"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/auth"
)

// Pools of agent processes besides the main one.
const (
	guestPool = "guest"
	usersPool = "users"
)

// maxUserAgents limits the agent processes of members that run at once;
// the least recently used idle one stops to start another.
const maxUserAgents = 10

// conversationKey returns the key of the agent process a conversation runs
// in, logging a failure to look it up.
func (s *Server) conversationKey(ctx context.Context, contextID string) string {
	key, err := s.runner.KeyOf(ctx, contextID)
	if err != nil {
		slog.Warn("looking up the session of a conversation", "context", contextID, "error", err)
	}
	return key
}

// guestConversation reports whether a conversation runs in the agent
// process of guests.
func (s *Server) guestConversation(ctx context.Context, contextID string) bool {
	return s.conversationKey(ctx, contextID) == agentkey.Guest
}

// userConversation reports whether a conversation runs in the agent process
// of a member.
func (s *Server) userConversation(ctx context.Context, contextID string) bool {
	_, ok := agentkey.UserOf(s.conversationKey(ctx, contextID))
	return ok
}

// userProcessName names the agent process of a member's agent key in paths
// and container names.
func userProcessName(key string) string {
	id, _ := agentkey.UserOf(key)
	if caller := agentkey.CallerOf(key); caller != "" {
		return agentdir.CallerName(id, caller)
	}
	return agentdir.UserKey(id)
}

// userHomeDir is the agent home of a member's agent key.
func (s *Server) userHomeDir(key string) string {
	id, _ := agentkey.UserOf(key)
	if caller := agentkey.CallerOf(key); caller != "" {
		return s.dir.CallerHomeDir(id, caller)
	}
	return s.dir.UserHomeDir(id)
}

// roleRank orders roles by what they may do; no role ranks lowest.
func roleRank(role string) int {
	switch role {
	case auth.RoleOwner:
		return 3
	case auth.RoleMember:
		return 2
	case auth.RoleGuest:
		return 1
	}
	return 0
}

// capToProcessUser returns who a turn of user acts as through the agents
// URL of a conversation in the agent process of process. Other
// conversations in that process can read the URL, so a turn acts as the
// process's user unless its own user may do less.
func capToProcessUser(user, process auth.Principal) auth.Principal {
	if user.ID == process.ID || roleRank(user.Role) < roleRank(process.Role) {
		return user
	}
	process.Hops = max(process.Hops, user.Hops)
	return process
}
