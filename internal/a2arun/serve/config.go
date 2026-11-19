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

package serve

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/shlex"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/a2arun/hosttools"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// Sandboxes.
const (
	SandboxDocker = "docker"
	SandboxLocal  = "local"
)

const (
	// DefaultListen is the address of the A2A endpoint without --listen.
	DefaultListen = "127.0.0.1:0"
	// DefaultMaxProcesses bounds the agent processes without --max-processes.
	DefaultMaxProcesses = 4
	// maxMaxProcesses is the largest --max-processes.
	maxMaxProcesses = 64
)

// Config is what the flags of a2a-run give.
type Config struct {
	// Agent is a known agent, or "" for a custom one that Command starts.
	Agent string
	// Command is the command line of the agent as shell words: it replaces
	// the command of Agent, or starts a custom ACP agent.
	Command string
	Image   string
	Model   string
	// Sandbox is docker or local; "" is docker.
	Sandbox string
	// Egress is the egress mode; "" when unset, which is internet.
	Egress      string
	EgressAllow []string
	// EgressProxy is the URL of the proxy that decides every connection
	// of the Docker sandbox, in place of Egress, EgressAllow and ModelKeys.
	EgressProxy string
	// Env are NAME=value pairs the agent gets.
	Env []string
	// Secrets are the names of variables of this process that the agent
	// gets.
	Secrets []string
	// ModelKeys are provider=NAME pairs: the boundary adds the key in the
	// variable NAME of this process to the requests to the provider.
	ModelKeys []string
	// Workspace is dir:<path> or memory.
	Workspace string
	// WorkspaceSync is turn or live; "" is turn.
	WorkspaceSync string
	// Tools is the URL of the host's tools endpoint, or "".
	Tools string
	// Listen is unix:///path or host:port; "" is DefaultListen.
	Listen string
	// MaxProcesses bounds the agent processes; 0 is DefaultMaxProcesses.
	MaxProcesses int
	// Home is the agent home directory; "" is a temporary directory,
	// removed at Close.
	Home     string
	StateDir string
	// Version is the version of a2a-run, for the agent card.
	Version string

	// Getenv reads the variables of Secrets and ModelKeys.
	Getenv func(string) string
	// Stderr receives the logs, the image pulls and the stderr of the
	// agent.
	Stderr io.Writer
}

// UsageError is an error of the flags.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }

func (e *UsageError) Unwrap() error { return e.Err }

func usagef(format string, args ...any) error {
	return &UsageError{Err: fmt.Errorf(format, args...)}
}

// settings are a Config resolved into what the core takes.
type settings struct {
	profile      a2aserve.Profile
	image        string
	model        string
	sandbox      string
	policy       egress.Policy
	egressProxy  string
	env          []string
	credentials  []egress.Credential
	workspace    workspace
	tools        *hosttools.Endpoint
	listen       address
	maxProcesses int
	home         string
	stateDir     string
}

// address is where the A2A endpoint listens.
type address struct {
	network string
	addr    string
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// resolve checks cfg and reads the variables it names. Its errors are
// [UsageError]s.
func (cfg Config) resolve() (settings, error) {
	s := settings{model: cfg.Model, image: cfg.Image, home: cfg.Home, stateDir: cfg.StateDir}
	var err error
	if s.sandbox, err = resolveSandbox(cfg); err != nil {
		return settings{}, err
	}
	if s.profile, err = resolveProfile(cfg, s.sandbox); err != nil {
		return settings{}, err
	}
	if s.egressProxy, err = resolveEgressProxy(cfg); err != nil {
		return settings{}, err
	}
	if s.policy, err = resolvePolicy(cfg, s.sandbox); err != nil {
		return settings{}, err
	}
	if s.env, err = resolveEnv(cfg); err != nil {
		return settings{}, err
	}
	if s.egressProxy != "" {
		s.env = append(placeholderKeys(s.env), s.env...)
	}
	if s.credentials, err = resolveModelKeys(cfg); err != nil {
		return settings{}, err
	}
	if s.workspace, err = parseWorkspace(cfg.Workspace, cfg.WorkspaceSync, s.sandbox); err != nil {
		return settings{}, err
	}
	if s.tools, err = parseTools(cfg.Tools, s.sandbox); err != nil {
		return settings{}, err
	}
	if s.listen, err = parseListen(cfg.Listen); err != nil {
		return settings{}, err
	}
	s.maxProcesses = cfg.MaxProcesses
	if s.maxProcesses == 0 {
		s.maxProcesses = DefaultMaxProcesses
	}
	if s.maxProcesses < 1 || s.maxProcesses > maxMaxProcesses {
		return settings{}, usagef("invalid --max-processes %d: want 1 to %d", cfg.MaxProcesses, maxMaxProcesses)
	}
	return s, nil
}

func resolveSandbox(cfg Config) (string, error) {
	switch cfg.Sandbox {
	case "", SandboxDocker:
		return SandboxDocker, nil
	case SandboxLocal:
		var docker []string
		for flag, set := range map[string]bool{"--egress": cfg.Egress != "", "--egress-allow": len(cfg.EgressAllow) > 0, "--inject-model-key": len(cfg.ModelKeys) > 0, "--egress-proxy": cfg.EgressProxy != "", "--image": cfg.Image != ""} {
			if set {
				docker = append(docker, flag)
			}
		}
		if len(docker) > 0 {
			slices.Sort(docker)
			return "", usagef("%s need --sandbox docker: --sandbox local runs the agent on this machine, with its network and without an image", strings.Join(docker, ", "))
		}
		return SandboxLocal, nil
	}
	return "", usagef("invalid --sandbox %q: want %s or %s", cfg.Sandbox, SandboxDocker, SandboxLocal)
}

func resolveProfile(cfg Config, sandbox string) (a2aserve.Profile, error) {
	var argv []string
	if cfg.Command != "" {
		var err error
		argv, err = shlex.Split(cfg.Command)
		if err != nil || len(argv) == 0 {
			return a2aserve.Profile{}, usagef("invalid --command %q: want the command line of the agent, as shell words", cfg.Command)
		}
	}
	if cfg.Agent == "" {
		if len(argv) == 0 {
			return a2aserve.Profile{}, usagef("no agent: pass --agent %s, or --command with the command line of an ACP agent", strings.Join(a2aserve.ProfileNames(), " or --agent "))
		}
		if sandbox == SandboxDocker && cfg.Image == "" {
			return a2aserve.Profile{}, usagef("--command without --agent needs --image with --sandbox docker, or --sandbox local")
		}
		return a2aserve.Command(argv...), nil
	}
	if !slices.Contains(a2aserve.ProfileNames(), cfg.Agent) {
		return a2aserve.Profile{}, usagef("unknown --agent %q: want %s, or --command for another ACP agent", cfg.Agent, strings.Join(a2aserve.ProfileNames(), ", "))
	}
	p, err := a2aserve.LookupProfile(cfg.Agent, argv)
	if err != nil {
		return a2aserve.Profile{}, &UsageError{Err: err}
	}
	return p, nil
}

func resolvePolicy(cfg Config, sandbox string) (egress.Policy, error) {
	if sandbox != SandboxDocker {
		return egress.Policy{}, nil
	}
	p := egress.Policy{Mode: egress.ModeInternet}
	switch egress.Mode(cfg.Egress) {
	case "", egress.ModeInternet:
	case egress.ModeAllowlist:
		p.Mode = egress.ModeAllowlist
	default:
		return egress.Policy{}, usagef("invalid --egress %q: want %s or %s", cfg.Egress, egress.ModeInternet, egress.ModeAllowlist)
	}
	for _, v := range cfg.EgressAllow {
		r, err := egressrules.ParseRule(v)
		if err != nil {
			return egress.Policy{}, &UsageError{Err: err}
		}
		p.Allow = append(p.Allow, r.String())
	}
	if err := p.Validate(); err != nil {
		return egress.Policy{}, &UsageError{Err: err}
	}
	return p, nil
}

func resolveEnv(cfg Config) ([]string, error) {
	var out []string
	for _, kv := range cfg.Env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !envName.MatchString(name) {
			return nil, usagef("invalid --env %q: want NAME=value", kv)
		}
		out = append(out, kv)
	}
	for _, name := range cfg.Secrets {
		if !envName.MatchString(name) {
			return nil, usagef("invalid --secret %q: want the name of a variable of your environment, such as GITHUB_TOKEN", name)
		}
		v := cfg.getenv(name)
		if v == "" {
			return nil, usagef("--secret %s: the variable %s is unset or empty; export it before you start a2a-run", name, name)
		}
		out = append(out, name+"="+v)
	}
	return out, nil
}

func resolveModelKeys(cfg Config) ([]egress.Credential, error) {
	var out []egress.Credential
	var seen []string
	for _, kv := range cfg.ModelKeys {
		provider, name, ok := strings.Cut(kv, "=")
		if !ok || !envName.MatchString(name) {
			return nil, usagef("invalid --inject-model-key %q: want provider=NAME, with NAME a variable of your environment, such as anthropic=ANTHROPIC_API_KEY", kv)
		}
		if _, ok := egressrules.LookupProvider(provider); !ok {
			return nil, usagef("invalid --inject-model-key %q: %w", kv, egressrules.UnknownProvider(provider))
		}
		if slices.Contains(seen, provider) {
			return nil, usagef("--inject-model-key names %s twice; give each provider one key", provider)
		}
		seen = append(seen, provider)
		key := cfg.getenv(name)
		if key == "" {
			return nil, usagef("--inject-model-key %s: the variable %s is unset or empty; export it before you start a2a-run", kv, name)
		}
		out = append(out, egress.ModelKey{Provider: provider, Key: key})
	}
	return out, nil
}

func (cfg Config) getenv(name string) string {
	if cfg.Getenv == nil {
		return ""
	}
	return cfg.Getenv(name)
}

// parseListen parses unix:///path, or host:port with a loopback host:
// the endpoint has no authentication.
func parseListen(v string) (address, error) {
	if v == "" {
		v = DefaultListen
	}
	if path, ok := strings.CutPrefix(v, "unix://"); ok {
		if !filepath.IsAbs(path) {
			return address{}, usagef("invalid --listen %q: want unix:///absolute/path", v)
		}
		return address{network: "unix", addr: filepath.Clean(path)}, nil
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return address{}, usagef("invalid --listen %q: want unix:///path or host:port, such as %s", v, DefaultListen)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n > 65535 {
		return address{}, usagef("invalid --listen %q: the port isn't a number from 0 to 65535", v)
	}
	if !loopback(host) {
		return address{}, usagef("invalid --listen %q: a2a-run doesn't authenticate callers, so it listens only on a loopback address, such as %s, or a Unix socket", v, DefaultListen)
	}
	return address{network: "tcp", addr: v}, nil
}

// loopback reports whether host names this machine's loopback interface.
func loopback(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}
