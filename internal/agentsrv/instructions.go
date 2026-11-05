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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/skills"
)

// agentsLinePrefix starts the line added to the first prompt of an ACP
// session with the agents the harness can call and the agents URL of the
// conversation.
const agentsLinePrefix = "[a2a agents]"

// instructionFiles are the global instruction files of the known harnesses,
// relative to the harness home. Each gets the same section.
var instructionFiles = []string{
	filepath.Join(".config", "opencode", "AGENTS.md"),
	filepath.Join(".claude", "CLAUDE.md"),
}

const (
	sectionBegin = "<!-- a2a-server:begin (managed by a2a serve; edits are overwritten) -->"
	sectionEnd   = "<!-- a2a-server:end -->"
)

// AgentRef is an agent this agent can call.
type AgentRef struct {
	Name        string
	Kind        string
	Description string
}

// callableAgents returns the built-in schedules agent, the contacts and the
// hosted nodes.
func callableAgents(ctx context.Context, st *store.Store) ([]AgentRef, error) {
	contacts, err := st.Contacts(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := st.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AgentRef, 0, len(contacts)+len(nodes)+1)
	out = append(out, AgentRef{Name: schedulesAgent, Kind: "built-in", Description: "schedule prompts for later in this conversation, such as reminders; send it help"})
	for _, c := range contacts {
		out = append(out, AgentRef{Name: c.Name, Kind: "contact"})
	}
	for _, n := range nodes {
		out = append(out, AgentRef{Name: n.ID, Kind: "webhook node", Description: n.Description})
	}
	return out, nil
}

// writeInstructions puts the a2a-agents skill and the agents the harness can
// call into the global instruction files in the homes of the owner's and the
// users' agent processes.
func (s *Server) writeInstructions(ctx context.Context) error {
	homes := []string{s.dir.HomeDir()}
	entries, err := os.ReadDir(s.dir.UserHomesDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			homes = append(homes, filepath.Join(s.dir.UserHomesDir(), e.Name()))
		}
	}
	return s.writeInstructionsIn(ctx, homes...)
}

// writeInstructionsIn writes the instruction files into homes. A local
// harness uses your own home, which is left alone. A home that fails the
// sandbox checks is skipped, since the agent may have redirected it.
func (s *Server) writeInstructionsIn(ctx context.Context, homes ...string) error {
	cfg := s.config()
	if cfg.Sandbox == harness.SandboxLocal {
		return nil
	}
	mounts, err := s.dir.Mounts(cfg)
	if err != nil {
		return err
	}
	s.instructionsMu.Lock()
	defer s.instructionsMu.Unlock()
	agents, err := callableAgents(ctx, s.store)
	if err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(skills.A2AAgents))
	sb.WriteString("\n\nThe agents you can call:\n\n")
	if len(agents) == 0 {
		sb.WriteString("- none yet\n")
	}
	for _, a := range agents {
		fmt.Fprintf(&sb, "- %s (%s)", a.Name, a.Kind)
		if a.Description != "" {
			sb.WriteString(": " + a.Description)
		}
		sb.WriteString("\n")
	}
	var errs []error
	for _, home := range homes {
		if err := s.checkHome(cfg, mounts, home); err != nil {
			slog.Warn("skipped writing the instructions into an agent home", "home", home, "error", err)
			continue
		}
		for _, f := range instructionFiles {
			errs = append(errs, writeSection(home, f, sb.String()))
		}
	}
	return errors.Join(errs...)
}

// refreshInstructions rewrites the instruction files after the agents the
// harness can call changed.
func (s *Server) refreshInstructions(ctx context.Context) {
	if err := s.writeInstructions(ctx); err != nil {
		s.logf("! updating the harness instructions: %v", err)
	}
}

// writeSection replaces the managed section of the file name in home,
// keeping whatever else the file holds. The harness can change its home, so
// symlinks that lead out of it are refused.
func writeSection(home, name, body string) (err error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	data, err := root.ReadFile(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	return root.WriteFile(name, []byte(withSection(string(data), body)), 0o600)
}

func withSection(text, body string) string {
	section := sectionBegin + "\n" + body + sectionEnd + "\n"
	i := strings.Index(text, sectionBegin)
	if i < 0 {
		if text != "" && !strings.HasSuffix(text, "\n\n") {
			text = strings.TrimRight(text, "\n") + "\n\n"
		}
		return text + section
	}
	end := len(text)
	if j := strings.Index(text[i:], sectionEnd); j >= 0 {
		end = i + j + len(sectionEnd)
		end += len(text[end:]) - len(strings.TrimPrefix(text[end:], "\n"))
	}
	return text[:i] + section + text[end:]
}
