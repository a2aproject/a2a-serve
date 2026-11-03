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

// Package wizard asks the questions of the interactive first run of
// `a2a serve`, for people who start it without flags.
package wizard

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/project"
)

// Run modes.
const (
	ModeForeground = "foreground"
	ModeDetach     = "detach"
	ModeService    = "service"
)

// Result is what the user chose.
type Result struct {
	Settings agentdir.Settings
	Mode     string
}

// apiKeys are the variables harnesses commonly need, in order of preference.
var apiKeys = map[string][]string{
	"claude":   {"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
	"opencode": {"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY"},
}

type prompter struct {
	r   *bufio.Reader
	out io.Writer
}

func (p *prompter) ask(question, def string) (string, error) {
	prompt := question + ": "
	if def != "" {
		prompt = fmt.Sprintf("%s [%s]: ", question, def)
	}
	if _, err := io.WriteString(p.out, prompt); err != nil {
		return "", err
	}
	line, err := p.r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		if errors.Is(err, io.EOF) {
			return def, nil
		}
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (p *prompter) choose(question string, options []string, def string) (string, error) {
	for {
		v, err := p.ask(fmt.Sprintf("%s (%s)", question, strings.Join(options, ", ")), def)
		if err != nil {
			return "", err
		}
		if slices.Contains(options, v) {
			return v, nil
		}
		if _, err := fmt.Fprintf(p.out, "  choose one of: %s\n", strings.Join(options, ", ")); err != nil {
			return "", err
		}
	}
}

// ErrCanceled is returned by [Run] when the user doesn't confirm the settings.
var ErrCanceled = errors.New("canceled; nothing was saved")

// Options configure [Run].
type Options struct {
	DefaultName string
	Harnesses   []string
	Getenv      func(string) string
	// DockerErr is why Docker can't run the agent, or nil if it can.
	DockerErr error
	// ReadSecret prints prompt and reads an answer without echoing it; nil
	// reads it like any other answer.
	ReadSecret func(prompt string) (string, error)
	// Command validates the settings and returns the `a2a serve <harness>`
	// command which creates the same agent, shown before saving.
	Command func(agentdir.Settings) (string, error)
	// Workspace is the project directory, whose MCP servers and skills are
	// shown. Empty skips them.
	Workspace string
}

// Run asks for the settings of a new agent.
func Run(in io.Reader, out io.Writer, opts Options) (Result, error) {
	harnesses, getenv := opts.Harnesses, opts.Getenv
	if len(harnesses) == 0 {
		return Result{}, errors.New("no harnesses to choose from")
	}
	p := &prompter{r: bufio.NewReader(in), out: out}
	if _, err := fmt.Fprintln(out, "No agent here yet. A few questions to set one up (Enter keeps the default):"); err != nil {
		return Result{}, err
	}
	var res Result
	s := &res.Settings
	var err error
	def := harnesses[0]
	if slices.Contains(harnesses, "opencode") {
		def = "opencode"
	}
	if s.Harness, err = p.choose("Harness", harnesses, def); err != nil {
		return res, err
	}
	if s.Name, err = p.ask("Agent name", opts.DefaultName); err != nil {
		return res, err
	}
	if s.Sandbox, err = p.sandbox(opts.DockerErr); err != nil {
		return res, err
	}

	var defKey string
	for _, k := range apiKeys[s.Harness] {
		if getenv(k) != "" {
			defKey = k
			break
		}
	}
	for {
		keys, err := p.ask("Environment variables with API keys to pass to the agent (comma-separated)", defKey)
		if err != nil {
			return res, err
		}
		s.Secrets = nil
		var missing []string
		for k := range strings.SplitSeq(keys, ",") {
			if k = strings.TrimSpace(k); k == "" {
				continue
			}
			if getenv(k) == "" {
				missing = append(missing, k)
			}
			s.Secrets = append(s.Secrets, k)
		}
		if len(missing) == 0 {
			break
		}
		if _, err := fmt.Fprintf(out, "  not set in this shell: %s\n", strings.Join(missing, ", ")); err != nil {
			return res, err
		}
	}

	if err := p.project(opts, s); err != nil {
		return res, err
	}

	if s.Private, err = p.choose("Private network for your machines and team", []string{"none", "tailscale", "lan"}, "none"); err != nil {
		return res, err
	}
	if s.Public, err = p.choose("Public URL on the internet", []string{"none", "tailscale", "ngrok"}, "none"); err != nil {
		return res, err
	}
	if s.Public == "ngrok" {
		def := ""
		if getenv("NGROK_AUTHTOKEN") != "" {
			def = "env:NGROK_AUTHTOKEN"
		}
		if s.NgrokAuthToken, err = p.ask("ngrok authtoken (env:VAR or the token)", def); err != nil {
			return res, err
		}
	}
	const telegram = "Telegram bot token from @BotFather (env:VAR, the token, or empty to skip)"
	if opts.ReadSecret != nil {
		s.Telegram, err = opts.ReadSecret(telegram)
	} else {
		s.Telegram, err = p.ask(telegram, "")
	}
	if err != nil {
		return res, err
	}
	if res.Mode, err = p.choose("Run", []string{ModeForeground, ModeDetach, ModeService}, ModeForeground); err != nil {
		return res, err
	}
	if opts.Command == nil {
		return res, nil
	}
	return res, p.confirm(res, opts.Command)
}

// project shows the MCP servers and skills of the project, and asks for the
// variables its MCP servers need which aren't passed to the agent yet.
func (p *prompter) project(opts Options, s *agentdir.Settings) error {
	if opts.Workspace == "" {
		return nil
	}
	servers, err := project.LoadMCP(opts.Workspace)
	if err != nil {
		_, err = fmt.Fprintf(p.out, "  ! %v; the agent gets none of its MCP servers until it is fixed\n", err)
		return err
	}
	skills, err := project.Skills(opts.Workspace)
	if err != nil {
		return err
	}
	if len(skills) > 0 {
		names := make([]string, len(skills))
		for i, sk := range skills {
			names[i] = sk.Name
		}
		if _, err := fmt.Fprintf(p.out, "  The project's skills work with every harness: %s\n", strings.Join(names, ", ")); err != nil {
			return err
		}
	}
	if len(servers) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(p.out, "  The project's %s has MCP servers, which run in the sandbox:\n", project.MCPFile); err != nil {
		return err
	}
	passed := func(name string) (string, bool) {
		if slices.Contains(s.Secrets, name) {
			return "", true
		}
		return "", false
	}
	for _, srv := range servers {
		if _, err := fmt.Fprintf(p.out, "    %s: %s\n", srv.Name, srv.Summary()); err != nil {
			return err
		}
		_, missing := srv.Resolve(passed)
		for _, name := range missing {
			if slices.Contains(s.Secrets, name) {
				continue
			}
			if err := p.mcpVar(opts, s, srv.Name, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// mcpVar asks for the value of a variable an MCP server needs: from this
// shell if it is set there, or typed in.
func (p *prompter) mcpVar(opts Options, s *agentdir.Settings, server, name string) error {
	if opts.Getenv(name) != "" {
		v, err := p.choose(fmt.Sprintf("  Pass %s from this shell for %s", name, server), []string{"y", "n"}, "y")
		if err != nil || v != "y" {
			return err
		}
		s.Secrets = append(s.Secrets, name)
		return nil
	}
	question := fmt.Sprintf("  %s for %s (empty to skip)", name, server)
	var v string
	var err error
	if opts.ReadSecret != nil {
		v, err = opts.ReadSecret(question)
	} else {
		v, err = p.ask(question, "")
	}
	if err != nil || v == "" {
		return err
	}
	if s.SecretValues == nil {
		s.SecretValues = agentdir.Secrets{}
	}
	s.SecretValues[name] = v
	s.Secrets = append(s.Secrets, name)
	return nil
}

// sandbox asks where the agent runs, defaulting to local without Docker.
func (p *prompter) sandbox(dockerErr error) (string, error) {
	def := agentdir.SandboxDocker
	if dockerErr != nil {
		def = agentdir.SandboxLocal
		if _, err := fmt.Fprintf(p.out, "  Docker is not available (%v).\n  local runs the agent on this machine, unsandboxed, with your permissions; `a2a serve doctor` helps set up Docker.\n", dockerErr); err != nil {
			return "", err
		}
	}
	v, err := p.choose("Sandbox", []string{agentdir.SandboxDocker, agentdir.SandboxLocal}, def)
	if err != nil || v == agentdir.SandboxDocker {
		return "", err
	}
	return v, nil
}

// confirm shows the command which creates the same agent and asks to save it.
func (p *prompter) confirm(res Result, command func(agentdir.Settings) (string, error)) error {
	cmd, err := command(res.Settings)
	if err != nil {
		return err
	}
	if res.Mode != ModeForeground {
		cmd += " --" + res.Mode
	}
	if _, err := fmt.Fprintf(p.out, "\nThis is the same as running:\n  %s\n\n", cmd); err != nil {
		return err
	}
	ok, err := p.choose("Save .a2a/config.yaml and start the agent", []string{"y", "n"}, "y")
	if err != nil {
		return err
	}
	if ok != "y" {
		return ErrCanceled
	}
	return nil
}
