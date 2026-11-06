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

package agentops

import (
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// AccessInfo describes who may call the agent for the control API.
func AccessInfo(a *access.Description) *controlapi.Access {
	if a == nil {
		return nil
	}
	info := controlapi.Access(*a)
	return &info
}

func (s *Service) codeInfo(c *pairings.Code) *controlapi.PairCodeResponse {
	resp := &controlapi.PairCodeResponse{
		Code: c.Code, Expires: c.Expires, URL: c.URL, PairCommand: pairCommand(c.URL, c.Code),
		Advertised: c.Advertised, Hints: c.Hints, Messenger: s.deps.Runtime.Config().HasMessenger(),
	}
	if c.Node != nil {
		node := controlapi.NodeInfo(*c.Node)
		resp.Node = &node
		resp.Instructions = nodeInstructions(c.URL, c.Node.Name, c.Code)
	}
	return resp
}

// pairCommand is the command a caller or a node runs to pair with code.
func pairCommand(agentURL, code string) string {
	return fmt.Sprintf("a2a-serve pair join %s %s", agentURL, code)
}

// nodeInstructions is the message which tells the agent behind webhook node
// to pair with the agent at agentURL using code.
func nodeInstructions(agentURL, node, code string) string {
	agentURL = strings.TrimRight(agentURL, "/")
	return fmt.Sprintf("You are the webhook node %q of the agent at %s.\nRead %s/mailbox/skill.md and follow it.\nYour pairing code is %s.", node, agentURL, agentURL, code)
}

func pairingInfo(p pairings.Pairing) controlapi.Pairing {
	info := controlapi.Pairing{
		Name: p.Name, Kind: p.Kind, Role: p.Role, Principal: p.Principal, Created: p.Created, LastUsed: p.LastUsed,
		Tokens: make([]controlapi.PairingToken, len(p.Tokens)), Accounts: make([]controlapi.LinkedAccount, len(p.Accounts)),
		PendingCodes: p.PendingCodes, Allowed: p.Allowed,
	}
	for i, t := range p.Tokens {
		info.Tokens[i] = controlapi.PairingToken(t)
	}
	for i, a := range p.Accounts {
		info.Accounts[i] = controlapi.LinkedAccount(a)
	}
	if p.Node != nil {
		node := controlapi.NodeDetails(*p.Node)
		info.Node = &node
	}
	return info
}

func removalPreview(p *pairings.RemovalPreview) *controlapi.RemovalPreview {
	info := &controlapi.RemovalPreview{
		Kind: p.Kind, Kept: p.Kept, Tokens: p.Tokens, Accounts: make([]controlapi.LinkedAccount, len(p.Accounts)), Codes: p.Codes,
		Node: p.Node, Schedules: p.Schedules, Watches: p.Watches, Approvals: p.Approvals, Tasks: p.Tasks,
		RunningTasks: p.RunningTasks, Conversations: p.Conversations, Allowed: p.Allowed, AgentHomes: p.AgentHomes,
	}
	for i, a := range p.Accounts {
		info.Accounts[i] = controlapi.LinkedAccount(a)
	}
	return info
}

func contactInfo(c store.Contact) controlapi.ContactInfo {
	return controlapi.ContactInfo{Name: c.Name, URL: c.URL, Via: c.Via, HasToken: c.Token != "", Description: c.Card.Description, Skills: c.Card.Skills}
}

func scheduleInfo(s store.Schedule) controlapi.ScheduleInfo {
	info := controlapi.ScheduleInfo{
		Name: s.Name, Cron: s.Cron, At: s.At, TZ: s.TZ, Prompt: s.Prompt, User: s.UserID,
		Thread: s.Conversation, CreatedBy: s.CreatedBy, NextRun: s.NextRun, LastRun: s.LastRun, LastError: s.LastError,
	}
	if s.DeliverChannel != "" {
		info.Chat = s.DeliverChannel + ":" + auth.DisplayPrincipal(s.DeliverUser)
	}
	return info
}
