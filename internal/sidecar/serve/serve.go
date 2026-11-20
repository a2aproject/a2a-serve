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

// Package serve serves a coding agent in a sandbox over A2A, built only
// from the core, with one agent process per A2A context.
package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

const (
	// maxExecutions bounds the tasks that run at once.
	maxExecutions = 64
	// questionTimeout stops a turn whose question gets no answer.
	questionTimeout = 4 * time.Hour
)

// limits are the resources of each container.
var limits = sandbox.Limits{Memory: "4g", CPUs: "2", PIDs: 512}

// Ready is the line a2a-serve-sidecar prints once the agent is up, or when it fails
// to start.
type Ready struct {
	Ready bool `json:"ready"`
	// A2A is the endpoint: unix:///path or http://host:port.
	A2A       string `json:"a2a,omitempty"`
	Sandbox   string `json:"sandbox,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// WorkspaceSync is turn or live.
	WorkspaceSync string `json:"workspace_sync,omitempty"`
	// Egress is how the Docker sandbox's connections are decided: by the
	// egress policy or by the egress proxy.
	Egress string `json:"egress,omitempty"`
	// Contacts are the states of the contacts' cards at start.
	Contacts []ContactStatus `json:"contacts,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Server serves the agent over A2A until Close.
type Server struct {
	log      *slog.Logger
	agent    *a2aserve.Agent
	boundary *egress.Boundary
	proxy    *egress.ProxyRouter
	tools    *hostTools
	contacts *contacts
	ln       net.Listener
	http     *http.Server
	// cancelRequests ends the requests that Close didn't see finish.
	cancelRequests context.CancelFunc
	served         chan error
	// temp are the temporary directories that Close removes.
	temp  []string
	ready Ready
}

// Start checks cfg, starts the agent and serves it on the address of
// cfg.Listen. The agent's turns end when ctx does; the requests last until
// Close, so that their callers get the end of the tasks. Errors of the
// flags are [UsageError]s.
func Start(ctx context.Context, cfg Config) (_ *Server, err error) {
	s, err := cfg.resolve(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.workspace.check(ctx); err != nil {
		return nil, err
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	srv := &Server{log: slog.New(slog.NewTextHandler(stderr, nil)), served: make(chan error, 1)}
	defer func() {
		if err != nil {
			err = errors.Join(err, srv.release())
		}
	}()
	home, err := srv.dir(s.home, "a2a-serve-sidecar-home-")
	if err != nil {
		return nil, fmt.Errorf("creating the agent home directory: %w", err)
	}
	stateDir, err := srv.dir(s.stateDir, "a2a-serve-sidecar-state-")
	if err != nil {
		return nil, fmt.Errorf("creating the state directory: %w", err)
	}
	rt, summary, caps, err := srv.runtime(ctx, s, stderr)
	if err != nil {
		return nil, err
	}
	boundary := caps.Boundary
	s.workspace.liveOK = s.workspace.kind == WorkspaceDir || slices.Contains(caps.Mounts, sandbox.MountServed)
	if s.tools != nil {
		if srv.tools, err = startTools(ctx, *s.tools, cfg.Version, srv.log); err != nil {
			return nil, err
		}
	}
	turns := newTurns()
	if srv.contacts, err = startContacts(ctx, s.contacts, cfg.ContactTransport, turns, srv.log); err != nil {
		return nil, err
	}
	if s.sandbox != SandboxLocal {
		if srv.boundary, srv.proxy, err = newBoundary(s, srv.log, turns, srv.tools.wrapRouter()); err != nil {
			return nil, err
		}
	}
	if srv.ln, err = listen(s.listen); err != nil {
		return nil, err
	}
	runID := cryptoutil.RandomString(8, cryptoutil.Unambiguous)
	tree := s.workspace.tree()
	mem := newMemory(defaultMemoryLimits, time.Now)
	changes := newChangesets(tree, defaultChangesLimits, mem, srv.log)
	workspace, mainWorkspace, err := s.workspace.build(tree, stateDir, changes.synced)
	if err != nil {
		return nil, err
	}
	base := a2aserve.Environment{
		Workspace:   workspace,
		Home:        home,
		Env:         s.env,
		Credentials: s.credentials,
		Image:       s.image,
		Name:        "a2a-serve-sidecar-" + runID,
		Labels:      map[string]string{"a2a.run": runID},
	}
	if s.sandbox == SandboxDocker {
		base.Limits = limits
	}
	if srv.tools != nil {
		base.MCPServers = []a2aserve.MCPServer{srv.tools.server(s.sandbox)}
	}
	mainEnv := base
	mainEnv.Workspace = mainWorkspace
	srv.agent, err = a2aserve.New(ctx, a2aserve.Config{
		Profile:  s.profile,
		Model:    s.model,
		Runtime:  rt,
		Egress:   srv.boundary,
		Tools:    srv.tools.deciders(),
		Main:     mainEnv,
		Callers:  contexts{base: base},
		Pools:    map[string]a2aserve.Pool{contextsPool: {MaxProcesses: s.maxProcesses, QuestionTimeout: questionTimeout}},
		Sessions: mem.sessions(),
		AgentAPI: srv.contacts.agentAPI(),
		Hooks:    a2aserve.Hooks{Turn: turns.turn, Prompt: srv.contacts.prompt(), ToolDone: srv.tools.toolDone(), Explain: s.workspace.explain},
		A2A: a2aserve.A2A{
			TaskStore:     mem,
			Middleware:    srv.contacts.middleware(turns, srv.log, changes.middleware),
			MaxExecutions: maxExecutions,
			Capabilities:  &a2a.AgentCapabilities{Streaming: true},
		},
		StateDir: stateDir,
		Log:      srv.log,
		Stderr:   stderr,
	})
	if err != nil {
		return nil, err
	}
	name, version := s.profile.Name(), cfg.Version
	card := func(baseURL string) *a2a.AgentCard { return agentCard(name, version, baseURL) }
	reqCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	srv.cancelRequests = cancelRequests
	srv.http = httpserver.New(reqCtx, routes(srv.agent.Handler(), card, s.listen.network == "tcp"))
	go func() {
		err := srv.http.Serve(srv.ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		srv.served <- err
	}()
	srv.ready = Ready{Ready: true, A2A: url(srv.ln), Sandbox: summary, Workspace: s.workspace.kind, WorkspaceSync: s.workspace.sync, Contacts: srv.contacts.status()}
	switch {
	case s.egressProxy != "":
		srv.ready.Egress = EgressProxy
	case s.sandbox != SandboxLocal && boundary:
		srv.ready.Egress = EgressPolicy
	case s.sandbox != SandboxLocal:
		srv.ready.Egress = EgressNone
	}
	return srv, nil
}

// Ready returns the ready line of the running server.
func (s *Server) Ready() Ready { return s.ready }

// Done receives the error that stopped the A2A endpoint, or nil once Close
// stopped it.
func (s *Server) Done() <-chan error { return s.served }

// Close stops accepting requests and stops the agent processes, whose
// tasks fail. It waits within ctx for the requests to send the callers the
// end of their tasks, then ends the rest. It removes the Unix socket and
// the temporary directories.
func (s *Server) Close(ctx context.Context) error {
	s.contacts.stop()
	shutdown := make(chan error, 1)
	go func() { shutdown <- s.http.Shutdown(ctx) }()
	err := s.agent.Close(ctx)
	if serr := <-shutdown; serr != nil {
		err = errors.Join(err, serr, s.http.Close())
	}
	s.cancelRequests()
	s.agent, s.ln = nil, nil
	return errors.Join(err, s.closeTools(ctx), s.release())
}

// release closes and removes what Start set up and Close didn't.
func (s *Server) release() error {
	var errs []error
	if s.agent != nil {
		errs = append(errs, s.agent.Close(context.Background()))
	}
	if s.ln != nil {
		errs = append(errs, httpserver.CloseListener(s.ln))
	}
	if s.boundary != nil {
		errs = append(errs, s.boundary.Close())
	}
	if s.proxy != nil {
		s.proxy.CloseIdleConnections()
	}
	errs = append(errs, s.contacts.close())
	ctx, cancel := context.WithTimeout(context.Background(), toolsCloseTimeout)
	defer cancel()
	errs = append(errs, s.closeTools(ctx))
	for _, dir := range s.temp {
		errs = append(errs, fsutil.RemoveAll(dir))
	}
	s.agent, s.ln, s.boundary, s.proxy, s.contacts, s.temp = nil, nil, nil, nil, nil, nil
	return errors.Join(errs...)
}

// dir returns dir, creating it, or a temporary directory named after
// pattern, which Close removes.
func (s *Server) dir(dir, pattern string) (string, error) {
	if dir != "" {
		return dir, fsutil.MkdirPrivate(dir)
	}
	tmp, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	s.temp = append(s.temp, tmp)
	return tmp, nil
}

// runtime returns the sandbox runtime of set, how the ready line describes
// it, and its capabilities, whose Boundary is set only where the egress
// boundary decides the sandbox's connections. It checks that Docker
// answers before anything starts.
func (s *Server) runtime(ctx context.Context, set settings, stderr io.Writer) (_ sandbox.Runtime, summary string, _ sandbox.Capabilities, _ error) {
	switch set.sandbox {
	case SandboxLocal:
		return sandbox.Local(), SandboxLocal, sandbox.Capabilities{Mounts: []sandbox.MountKind{sandbox.MountHostDir}}, nil
	case SandboxBwrap:
		return bwrapRuntime(ctx, set)
	}
	rt := sandbox.Docker(sandbox.DockerOptions{Progress: stderr})
	caps, err := rt.Probe(ctx)
	if err != nil {
		return nil, "", sandbox.Capabilities{}, err
	}
	if !caps.Boundary && set.egressProxy != "" {
		return nil, "", sandbox.Capabilities{}, fmt.Errorf("--egress-proxy needs a sandbox that routes every connection of the agent through a2a-serve-sidecar, which Docker here can't: %s", caps.Problem)
	}
	if !caps.Boundary && set.tools != nil {
		return nil, "", sandbox.Capabilities{}, fmt.Errorf("--tools with --sandbox docker needs a sandbox that routes the agent's connections through a2a-serve-sidecar, which Docker here can't: %s; use --sandbox local with an http:// URL", caps.Problem)
	}
	if set.workspace.served() && !slices.Contains(caps.Mounts, sandbox.MountServed) {
		return nil, "", sandbox.Capabilities{}, fmt.Errorf("--workspace-sync live with --workspace %s needs a sandbox that mounts the workspace, which Docker here can't: %s; %s", set.workspace.shown, caps.MountProblems[sandbox.MountServed], liveHint)
	}
	if !caps.Boundary {
		s.log.Warn("the sandbox can't route its connections through the egress boundary", "problem", caps.Problem)
	}
	rootless, err := sandboxrt.CheckDocker(ctx)
	if err != nil {
		return nil, "", sandbox.Capabilities{}, err
	}
	if rootless {
		return rt, SandboxDocker + " (rootless)", caps, nil
	}
	return rt, SandboxDocker, caps, nil
}

// bwrapRuntime returns the bubblewrap runtime of set, which runs the agent
// that this machine has, and its a2a CLI when the agent has contacts. The
// sandbox makes every connection through a2a-serve-sidecar, so it fails where the
// boundary can't work.
func bwrapRuntime(ctx context.Context, set settings) (_ sandbox.Runtime, summary string, _ sandbox.Capabilities, _ error) {
	var programs []string
	if len(set.contacts) > 0 {
		programs = []string{"a2a"}
	}
	rt := sandbox.Bubblewrap(sandbox.BubblewrapOptions{Programs: programs})
	caps, err := rt.Probe(ctx)
	if err != nil {
		return nil, "", sandbox.Capabilities{}, err
	}
	if !caps.Boundary {
		return nil, "", sandbox.Capabilities{}, fmt.Errorf("--sandbox bwrap routes every connection of the agent through a2a-serve-sidecar, which can't work here: %s", caps.Problem)
	}
	if set.workspace.served() && !slices.Contains(caps.Mounts, sandbox.MountServed) {
		return nil, "", sandbox.Capabilities{}, fmt.Errorf("--workspace-sync live with --workspace %s needs a sandbox that mounts the workspace, which bubblewrap here can't: %s; %s", set.workspace.shown, caps.MountProblems[sandbox.MountServed], liveHint)
	}
	return rt, SandboxBwrap, caps, nil
}
