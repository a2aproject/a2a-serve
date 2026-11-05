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
// `a2a-serve start`, for people who start it without arguments.
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

// Prompter asks questions, one per line. The first-run wizard and other
// commands that configure an agent share its questions.
type Prompter struct {
	r          *bufio.Reader
	out        io.Writer
	readSecret func(prompt string) (string, error)
}

// NewPrompter returns a prompter which reads answers from in and writes
// questions to out. readSecret prints a prompt and reads an answer without
// echoing it; nil reads it like any other answer.
func NewPrompter(in io.Reader, out io.Writer, readSecret func(prompt string) (string, error)) *Prompter {
	return &Prompter{r: bufio.NewReader(in), out: out, readSecret: readSecret}
}

// Ask asks a question and returns the answer, or def for an empty one.
func (p *Prompter) Ask(question, def string) (string, error) {
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

// Choose asks until the answer is one of options.
func (p *Prompter) Choose(question string, options []string, def string) (string, error) {
	for {
		v, err := p.Ask(fmt.Sprintf("%s (%s)", question, strings.Join(options, ", ")), def)
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

// Confirm asks a yes or no question.
func (p *Prompter) Confirm(question string, def bool) (bool, error) {
	d := "n"
	if def {
		d = "y"
	}
	v, err := p.Choose(question, []string{"y", "n"}, d)
	return v == "y", err
}

// Secret asks for a value without echoing it.
func (p *Prompter) Secret(question string) (string, error) {
	if p.readSecret != nil {
		return p.readSecret(question)
	}
	return p.Ask(question, "")
}

// APIKeys asks for the environment variables with the model API keys of
// the harness, until each is set in this shell.
func (p *Prompter) APIKeys(harness string, getenv func(string) string) ([]string, error) {
	var def string
	for _, k := range apiKeys[harness] {
		if getenv(k) != "" {
			def = k
			break
		}
	}
	for {
		answer, err := p.Ask("Environment variables with API keys to pass to the agent (comma-separated)", def)
		if err != nil {
			return nil, err
		}
		var keys, missing []string
		for k := range strings.SplitSeq(answer, ",") {
			if k = strings.TrimSpace(k); k == "" {
				continue
			}
			if getenv(k) == "" {
				missing = append(missing, k)
			}
			keys = append(keys, k)
		}
		if len(missing) == 0 {
			return keys, nil
		}
		if _, err := fmt.Fprintf(p.out, "  not set in this shell: %s\n", strings.Join(missing, ", ")); err != nil {
			return nil, err
		}
	}
}

// NeedsAPIKey reports whether the agent harness has known model API keys
// and secrets names none of them.
func NeedsAPIKey(harness string, secrets []string) bool {
	keys := apiKeys[harness]
	return len(keys) > 0 && !slices.ContainsFunc(secrets, func(s string) bool { return slices.Contains(keys, s) })
}

// Var is a variable an agent needs a value for.
type Var struct {
	Name        string
	Description string
	// Secret values are hidden as they are typed and stored as secrets;
	// others are saved with --env.
	Secret bool
}

// varTries is how many times VarValue asks before it gives up, such as
// when its input ends.
const varTries = 3

// VarValue asks for the value of v until it gets one: from this shell if
// it is set there, or typed in. It adds the value to s.
func (p *Prompter) VarValue(s *agentdir.Settings, getenv func(string) string, v Var) error {
	label := v.Name
	if v.Description != "" {
		label += " (" + v.Description + ")"
	}
	if val := getenv(v.Name); val != "" {
		ok, err := p.Confirm(fmt.Sprintf("  %s is set in this shell. Pass it to the agent?", v.Name), true)
		if err != nil {
			return err
		}
		if ok {
			AddVar(s, v, val)
			return nil
		}
	}
	for range varTries {
		var val string
		var err error
		if v.Secret {
			val, err = p.Secret("  " + label)
		} else {
			val, err = p.Ask("  "+label, "")
		}
		if err != nil {
			return err
		}
		if val != "" {
			AddVar(s, v, val)
			return nil
		}
		if _, err := fmt.Fprintf(p.out, "  %s is required\n", v.Name); err != nil {
			return err
		}
	}
	return fmt.Errorf("no value for %s", v.Name)
}

// AddVar adds the value of v to s: as a secret, or as an --env entry.
func AddVar(s *agentdir.Settings, v Var, value string) {
	if !v.Secret {
		s.Env = append(s.Env, v.Name+"="+value)
		return
	}
	if s.SecretValues == nil {
		s.SecretValues = agentdir.Secrets{}
	}
	s.SecretValues[v.Name] = value
	s.Secrets = append(s.Secrets, v.Name)
}

// SecretValue asks for the value of the variable name, which purpose
// needs, from this shell if it is set there, or typed in, and adds it to
// s. It reports whether the variable got a value.
func (p *Prompter) SecretValue(s *agentdir.Settings, getenv func(string) string, name, purpose string) (bool, error) {
	if getenv(name) != "" {
		ok, err := p.Confirm(fmt.Sprintf("  Pass %s from this shell %s", name, purpose), true)
		if err != nil || !ok {
			return false, err
		}
		s.Secrets = append(s.Secrets, name)
		return true, nil
	}
	v, err := p.Secret(fmt.Sprintf("  %s %s (empty to skip)", name, purpose))
	if err != nil || v == "" {
		return false, err
	}
	if s.SecretValues == nil {
		s.SecretValues = agentdir.Secrets{}
	}
	s.SecretValues[name] = v
	s.Secrets = append(s.Secrets, name)
	return true, nil
}

// MCPServers shows the MCP servers of a project, and asks for the
// variables they need which s doesn't pass to the agent yet. A server with
// a variable left without a value is turned off.
func (p *Prompter) MCPServers(s *agentdir.Settings, getenv func(string) string, servers []project.MCPServer) error {
	if len(servers) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(p.out, "  The project's %s has MCP servers, which run in the sandbox:\n", project.MCPFile); err != nil {
		return err
	}
	passed := func(name string) (string, bool) {
		return "", slices.Contains(s.Secrets, name)
	}
	for _, srv := range servers {
		if _, err := fmt.Fprintf(p.out, "    %s: %s\n", srv.Name, srv.Summary()); err != nil {
			return err
		}
		_, missing := srv.Resolve(passed)
		var skipped []string
		for _, name := range missing {
			if slices.Contains(s.Secrets, name) {
				continue
			}
			ok, err := p.SecretValue(s, getenv, name, "for "+srv.Name)
			if err != nil {
				return err
			}
			if !ok {
				skipped = append(skipped, name)
			}
		}
		if len(skipped) == 0 {
			continue
		}
		s.DisableMCP = append(s.DisableMCP, srv.Name)
		if _, err := fmt.Fprintf(p.out, "    %s is off without %s; turn it on with `a2a-serve config set --secret %s` and `a2a-serve config unset disable-mcp %s`\n", srv.Name, strings.Join(skipped, ", "), strings.Join(skipped, " --secret "), srv.Name); err != nil {
			return err
		}
	}
	return nil
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
	// Command validates the settings and returns the commands which create
	// the same agent, `a2a-serve start <agent>` first, shown before saving.
	Command func(agentdir.Settings) ([]string, error)
	// Workspace is the project directory, whose MCP servers and skills are
	// shown. Empty skips them.
	Workspace string
}

// Run asks for the settings of a new agent.
func Run(in io.Reader, out io.Writer, opts Options) (Result, error) {
	harnesses, getenv := opts.Harnesses, opts.Getenv
	if len(harnesses) == 0 {
		return Result{}, errors.New("no agents to choose from")
	}
	p := NewPrompter(in, out, opts.ReadSecret)
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
	if s.Harness, err = p.Choose("Coding agent", harnesses, def); err != nil {
		return res, err
	}
	if s.Name, err = p.Ask("Agent name", opts.DefaultName); err != nil {
		return res, err
	}
	if s.Sandbox, err = p.sandbox(opts.DockerErr); err != nil {
		return res, err
	}
	if s.Secrets, err = p.APIKeys(s.Harness, getenv); err != nil {
		return res, err
	}
	if err := p.project(opts, s); err != nil {
		return res, err
	}

	if s.Private, err = p.Choose("Private network for your machines and team", []string{"none", "tailscale"}, "none"); err != nil {
		return res, err
	}
	if s.Public, err = p.Choose("Public URL on the internet", []string{"none", "tailscale"}, "none"); err != nil {
		return res, err
	}
	if s.Telegram, err = p.Secret("Telegram bot token from @BotFather (env:VAR, the token, or empty to skip)"); err != nil {
		return res, err
	}
	if res.Mode, err = p.Choose("Run", []string{ModeForeground, ModeDetach, ModeService}, ModeForeground); err != nil {
		return res, err
	}
	if opts.Command == nil {
		return res, nil
	}
	return res, p.confirm(res, opts.Command)
}

// project shows the MCP servers and skills of the project, and asks for the
// variables its MCP servers need which aren't passed to the agent yet.
func (p *Prompter) project(opts Options, s *agentdir.Settings) error {
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
		if _, err := fmt.Fprintf(p.out, "  The project's skills work with every agent: %s\n", strings.Join(names, ", ")); err != nil {
			return err
		}
	}
	return p.MCPServers(s, opts.Getenv, servers)
}

// sandbox asks where the agent runs, defaulting to local without Docker.
func (p *Prompter) sandbox(dockerErr error) (string, error) {
	def := agentdir.SandboxDocker
	if dockerErr != nil {
		def = agentdir.SandboxLocal
		if _, err := fmt.Fprintf(p.out, "  Docker is not available (%v).\n  local runs the agent on this machine, unsandboxed, with your permissions; `a2a-serve doctor` helps set up Docker.\n", dockerErr); err != nil {
			return "", err
		}
	}
	v, err := p.Choose("Sandbox", []string{agentdir.SandboxDocker, agentdir.SandboxLocal}, def)
	if err != nil || v == agentdir.SandboxDocker {
		return "", err
	}
	return v, nil
}

// confirm shows the command which creates the same agent and asks to save it.
func (p *Prompter) confirm(res Result, command func(agentdir.Settings) ([]string, error)) error {
	lines, err := command(res.Settings)
	if err != nil {
		return err
	}
	if res.Mode != ModeForeground && len(lines) > 0 {
		lines[0] += " --" + res.Mode
	}
	if _, err := fmt.Fprintf(p.out, "\nThis is the same as running:\n  %s\n\n", strings.Join(lines, "\n  ")); err != nil {
		return err
	}
	ok, err := p.Confirm("Save .a2a/config.yaml and start the agent", true)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCanceled
	}
	return nil
}
