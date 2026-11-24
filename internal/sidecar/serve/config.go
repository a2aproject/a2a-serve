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
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/shlex"
	"golang.org/x/net/http/httpguts"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/contact"
	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/sidecar/hosttools"
)

// Sandboxes.
const (
	SandboxDocker = "docker"
	// SandboxGVisor runs Docker containers under gVisor's runtime runsc.
	SandboxGVisor = "gvisor"
	SandboxBwrap  = "bwrap"
	SandboxLocal  = "local"
)

// gVisorRuntime is the Docker runtime of SandboxGVisor.
const gVisorRuntime = "runsc"

const (
	// DefaultListen is the address of the A2A endpoint without --listen.
	DefaultListen = "127.0.0.1:0"
	// DefaultMaxProcesses bounds the agent processes without --max-processes.
	DefaultMaxProcesses = 4
	// maxMaxProcesses is the largest --max-processes.
	maxMaxProcesses = 64
)

// Config is what the flags of a2a-serve-sidecar give.
type Config struct {
	// Agent is a known agent, or "" for a custom one that Command starts.
	Agent string
	// Command is the command line of the agent as shell words: it replaces
	// the command of Agent, or starts a custom ACP agent.
	Command string
	Image   string
	Model   string
	// Sandbox is docker, gvisor, bwrap or local; "" is docker.
	Sandbox string
	// Egress is the egress mode; "" when unset, which is internet.
	Egress      string
	EgressAllow []string
	// EgressMiddleware are host[:port]=URL pairs of HTTP middlewares for
	// individual hosts.
	EgressMiddleware []string
	// EgressProxy is the URL of the proxy that decides every connection
	// of the sandbox, in place of Egress, EgressAllow, EgressMiddleware,
	// ModelKeys and InjectHeaders.
	EgressProxy string
	// Env are NAME=value pairs the agent gets.
	Env []string
	// Secrets are the names of variables of this process that the agent
	// gets.
	Secrets []string
	// ModelKeys are provider=NAME pairs: the boundary adds the key in the
	// variable NAME of this process to the requests to the provider.
	ModelKeys []string
	// InjectHeaders are host[:port]=Name: value pairs: the boundary adds
	// the header to HTTPS requests to that host.
	InjectHeaders []string
	// Workspace is dir:<path> or memory.
	Workspace string
	// WorkspaceSync is turn or live; "" is turn.
	WorkspaceSync string
	// Tools is the URL of the host's tools endpoint, or "".
	Tools string
	// Contacts are name=url pairs of the A2A agents the agent may call,
	// and ContactAuth name=value pairs of the Authorization headers of
	// their requests, whose last word may be env:VAR or -.
	Contacts    []string
	ContactAuth []string
	// ContactTransport sends the requests to https:// contacts; nil uses
	// a clone of http.DefaultTransport.
	ContactTransport http.RoundTripper
	// Listen is unix:///path or host:port; "" is DefaultListen.
	Listen string
	// MaxProcesses bounds the agent processes; 0 is DefaultMaxProcesses.
	MaxProcesses int
	// Home is the agent home directory; "" is a temporary directory,
	// removed at Close.
	Home     string
	StateDir string
	// Version is the version of a2a-serve-sidecar, for the agent card.
	Version string

	// Getenv reads the variables of Secrets, ModelKeys and ContactAuth.
	Getenv func(string) string
	// Stdin is where the loopback host token or a ContactAuth value of - is
	// read from.
	Stdin io.Reader
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
	profile          a2aserve.Profile
	image            string
	model            string
	sandbox          string
	policy           egress.Policy
	egressMiddleware []egress.MiddlewareEntry
	egressProxy      string
	env              []string
	credentials      []egress.Credential
	hasInjectHeaders bool
	workspace        workspace
	tools            *hosttools.Endpoint
	contacts         []contact.Entry
	hostToken        string
	listen           address
	maxProcesses     int
	home             string
	stateDir         string
}

// address is where the A2A endpoint listens.
type address struct {
	network string
	addr    string
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// resolve checks cfg and reads the variables it names. Its errors are
// [UsageError]s.
func (cfg Config) resolve(ctx context.Context) (settings, error) {
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
	modelCreds, credTargets, err := resolveModelKeys(cfg)
	if err != nil {
		return settings{}, err
	}
	headerCreds, headerPlaceholders, err := resolveInjectHeaders(cfg, credTargets)
	if err != nil {
		return settings{}, err
	}
	s.credentials = append(modelCreds, headerCreds...)
	s.hasInjectHeaders = len(headerCreds) > 0
	var mwPlaceholders []string
	if s.egressMiddleware, mwPlaceholders, err = resolveEgressMiddleware(cfg, credTargets); err != nil {
		return settings{}, err
	}
	if s.egressProxy != "" {
		s.env = prependPlaceholders(s.env, placeholderKeys(s.env))
	} else {
		s.env = prependPlaceholders(s.env, append(headerPlaceholders, mwPlaceholders...))
	}
	if s.workspace, err = parseWorkspace(cfg.Workspace, cfg.WorkspaceSync, s.sandbox); err != nil {
		return settings{}, err
	}
	if s.tools, err = parseTools(cfg.Tools, s.sandbox); err != nil {
		return settings{}, err
	}
	contacts, contactAuths, err := parseContacts(cfg)
	if err != nil {
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
	stdin := &stdinSecret{in: cfg.Stdin, prompt: cfg.Stderr}
	if needsHostToken(cfg, s, contacts, contactAuths) {
		token, err := stdin.read(ctx, "host-token", "loopback host token", "")
		if err != nil {
			return settings{}, err
		}
		if !httpguts.ValidHeaderFieldValue(token) {
			return settings{}, usagef("loopback host token: has a character that a header can't carry")
		}
		s.hostToken = token
	}
	if err := resolveContactAuths(ctx, cfg, contacts, contactAuths, s.hostToken, stdin); err != nil {
		return settings{}, err
	}
	s.contacts = contacts
	if s.hostToken != "" {
		for i := range s.egressMiddleware {
			if isLoopbackHTTP(s.egressMiddleware[i].URL) {
				s.egressMiddleware[i].Token = s.hostToken
			}
		}
		if s.workspace.kind == WorkspaceURL && isLoopbackHTTP(cfg.Workspace) {
			if err := s.workspace.withToken(cfg.Workspace, s.hostToken); err != nil {
				return settings{}, err
			}
		}
		if s.tools != nil && isLoopbackHTTP(cfg.Tools) {
			ep, err := s.tools.WithToken(s.hostToken)
			if err != nil {
				return settings{}, usagef("invalid loopback host token: %w", err)
			}
			s.tools = &ep
		}
	}
	return s, nil
}

// needsHostToken reports whether any configured host control endpoint is a
// loopback http:// URL that needs the host's run-scoped token from stdin.
func needsHostToken(cfg Config, s settings, contacts []contact.Entry, auths []parsedContactAuth) bool {
	if isLoopbackHTTP(s.egressProxy) {
		return true
	}
	for _, e := range s.egressMiddleware {
		if isLoopbackHTTP(e.URL) {
			return true
		}
	}
	if s.workspace.kind == WorkspaceURL && isLoopbackHTTP(cfg.Workspace) {
		return true
	}
	if s.tools != nil && isLoopbackHTTP(cfg.Tools) {
		return true
	}
	return contactsNeedHostToken(contacts, auths)
}

// isLoopbackHTTP reports whether rawURL is an http:// URL targeting a loopback
// host.
func isLoopbackHTTP(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := neturl.Parse(rawURL)
	return err == nil && u.Scheme == "http" && u.Host != "" && loopback(u.Hostname())
}

func hostTokenIfLoopback(rawURL, token string) string {
	if isLoopbackHTTP(rawURL) {
		return token
	}
	return ""
}

func resolveSandbox(cfg Config) (string, error) {
	switch cfg.Sandbox {
	case "", SandboxDocker:
		return SandboxDocker, nil
	case SandboxGVisor:
		return SandboxGVisor, nil
	case SandboxBwrap, SandboxLocal:
		if cfg.Image != "" {
			return "", usagef("--image needs --sandbox docker or gvisor: --sandbox %s runs the agent that this machine has", cfg.Sandbox)
		}
		if cfg.Sandbox == SandboxBwrap {
			return SandboxBwrap, nil
		}
		var sandboxed []string
		for flag, set := range map[string]bool{
			"--egress":               cfg.Egress != "",
			"--egress-allow":         len(cfg.EgressAllow) > 0,
			"--egress-middleware":    len(cfg.EgressMiddleware) > 0,
			"--inject-model-key":     len(cfg.ModelKeys) > 0,
			"--egress-inject-header": len(cfg.InjectHeaders) > 0,
			"--egress-proxy":         cfg.EgressProxy != "",
		} {
			if set {
				sandboxed = append(sandboxed, flag)
			}
		}
		if len(sandboxed) > 0 {
			slices.Sort(sandboxed)
			verb := "needs"
			if len(sandboxed) > 1 {
				verb = "need"
			}
			return "", usagef("%s %s --sandbox docker, gvisor or bwrap: --sandbox local runs the agent on this machine, with its network", strings.Join(sandboxed, ", "), verb)
		}
		return SandboxLocal, nil
	}
	return "", usagef("invalid --sandbox %q: want %s, %s, %s or %s", cfg.Sandbox, SandboxDocker, SandboxGVisor, SandboxBwrap, SandboxLocal)
}

// containers reports whether sandbox runs the agent in Docker containers.
func containers(sandbox string) bool { return sandbox == SandboxDocker || sandbox == SandboxGVisor }

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
		if containers(sandbox) && cfg.Image == "" {
			return a2aserve.Profile{}, usagef("--command without --agent needs --image with --sandbox %s, or --sandbox bwrap or local", sandbox)
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
	if sandbox == SandboxLocal {
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
			return nil, usagef("--secret %s: the variable %s is unset or empty; export it before you start a2a-serve-sidecar", name, name)
		}
		out = append(out, name+"="+v)
	}
	return out, nil
}

func resolveModelKeys(cfg Config) ([]egress.Credential, map[string]string, error) {
	var out []egress.Credential
	var seen []string
	targets := map[string]string{}
	for _, kv := range cfg.ModelKeys {
		provider, name, ok := strings.Cut(kv, "=")
		if !ok || !envName.MatchString(name) {
			return nil, nil, usagef("invalid --inject-model-key %q: want provider=NAME, with NAME a variable of your environment, such as anthropic=ANTHROPIC_API_KEY", kv)
		}
		p, ok := egressrules.LookupProvider(provider)
		if !ok {
			return nil, nil, usagef("invalid --inject-model-key %q: %w", kv, egressrules.UnknownProvider(provider))
		}
		if slices.Contains(seen, provider) {
			return nil, nil, usagef("--inject-model-key names %s twice; give each provider one key", provider)
		}
		seen = append(seen, provider)
		key := cfg.getenv(name)
		if key == "" {
			return nil, nil, usagef("--inject-model-key %s: the variable %s is unset or empty; export it before you start a2a-serve-sidecar", kv, name)
		}
		out = append(out, egress.ModelKey{Provider: provider, Key: key})
		targets[net.JoinHostPort(p.Host, strconv.Itoa(egressrules.HTTPSPort))] = "--inject-model-key"
	}
	return out, targets, nil
}

func prependPlaceholders(env, placeholders []string) []string {
	var add []string
	for _, kv := range placeholders {
		name, _, _ := strings.Cut(kv, "=")
		prefix := name + "="
		if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, prefix) }) ||
			slices.ContainsFunc(add, func(e string) bool { return strings.HasPrefix(e, prefix) }) {
			continue
		}
		add = append(add, kv)
	}
	if len(add) == 0 {
		return env
	}
	return append(add, env...)
}

func (cfg Config) getenv(name string) string {
	if cfg.Getenv == nil {
		return ""
	}
	return cfg.Getenv(name)
}

// parseListen parses unix:///path, or host:port with a loopback host.
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
		return address{}, usagef("invalid --listen %q: a2a-serve-sidecar listens only on a loopback address, such as %s, or a Unix socket", v, DefaultListen)
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
