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

// Package agentsrv is the agent server behind `a2a-serve`: one coding agent
// per process, started from its project directory, reachable over A2A on
// localhost, a private network and the internet, and from messengers.
package agentsrv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/limiter"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/inspector"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/nodes"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/oauth"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/turns"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/gossip"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Options configure [Run].
type Options struct {
	// GoogleEndpoints replace Google's OAuth endpoints, for tests.
	GoogleEndpoints *googleauth.Endpoints
	// SharedGoogleClientID is the client ID of the shared Google app whose
	// ID tokens the token exchange accepts; empty turns the exchange off.
	SharedGoogleClientID string
	Dir                  agentdir.Dir
	Config               *agentdir.Config
	Secrets              agentdir.Secrets
	// Out receives progress lines for the user.
	Out io.Writer
	// HarnessLog receives the harness's stderr. Discarded if nil.
	HarnessLog io.Writer
	// ImageProgress receives the output of pulling or building the harness
	// image. HarnessLog if nil.
	ImageProgress io.Writer
	// Executor replaces the ACP executor, for tests.
	Executor a2asrv.AgentExecutor
	// Channels replace the channels from the config, for tests.
	Channels []channel.Channel
	// Credentials, if set, receive an owner token for the local URL, so the
	// user running the server can call it from this machine.
	Credentials *credstore.Store
	// TelegramServerURL overrides the Telegram Bot API URL, for tests.
	TelegramServerURL string
	// SetupProblems, if set, returns the problems of this machine's setup
	// to print on start, such as an a2a CLI without the credential helper.
	SetupProblems func(context.Context) []string
	// Ready is called with the server once it serves requests.
	Ready func(*Server)
	// Private and Public replace the networks of the config, for tests.
	Private network.Provider
	Public  network.Provider
	// NewRelay replaces network.NewRelay, for tests.
	NewRelay func(network.RelayConfig) (network.Provider, error)
	// Now replaces time.Now for schedules, for tests.
	Now func() time.Time
	// Timings replace the delays and timeouts of the server, for tests.
	Timings Timings
}

// Server is a running agent server.
type Server struct {
	dir     agentdir.Dir
	out     io.Writer
	started time.Time
	cancel  context.CancelFunc

	// cfgMu serializes changes of rt.
	cfgMu sync.Mutex
	rt    atomic.Pointer[runtimeConfig]

	store   *store.Store
	tasks   *store.TaskStore
	handler a2asrv.RequestHandler
	sender  *turns.Sender
	chain   *auth.Chain
	bus     *eventBus

	private  network.Provider
	public   network.Provider
	newRelay func(network.RelayConfig) (network.Provider, error)
	// harness has the endpoints through which agent processes reach the
	// server, and mainHarness is the one of the owner's process.
	harness     *harnessListeners
	mainHarness *harnessEndpoint
	bridge      harnessBridge
	// pins are the mount sources and hidden paths as they resolved at the
	// start, which containers of later agent processes are checked against.
	pins harness.Pins

	credentials       *credstore.Store
	setupProblems     func(context.Context) []string
	telegramServerURL string

	urlMu      sync.Mutex
	localURL   string
	privateURL string
	publicURL  string

	supervisor    *ChannelSupervisor
	chatApprovals *ChatApprovals
	chatTurns     *ChatTurns
	claims        *CallClaims
	clients       *ContactClients
	watches       *Watches
	proxy         *HarnessProxy
	schedules     *schedules
	nodes         *nodes.Host
	pairings      *pairings.Service
	approvals     *approvals.Service
	oauth         *oauth.Server
	// ads are the advertised pairings of the private network.
	ads       *gossip.Table
	backups   backups
	inspector *inspector.Inspector
	// split runs the conversations of anonymous callers and of each
	// authenticated user in their own agent processes, and budget limits
	// the tasks of anonymous callers.
	split  *splitExecutor
	budget *anonymousBudget
	// users are the agent processes of users that run now.
	users userAgents
	// background runs the server's loops, which Run waits for.
	background sync.WaitGroup

	// instructionsMu serializes rewrites of the harness instruction files.
	instructionsMu sync.Mutex
	timings        Timings
}

// runtimeConfig is the config and secrets the server runs with. Changes
// replace it whole, so readers never see a partial update.
type runtimeConfig struct {
	cfg     *agentdir.Config
	secrets agentdir.Secrets
}

// maxExecutions bounds the tasks running at once, and
// maxCallerExecutions those of one caller other than an owner.
const (
	maxExecutions       = 64
	maxCallerExecutions = 8
)

// maxExecutionsOf is the execution limit of a scope, which only callers
// other than owners get.
func maxExecutionsOf(string) int { return maxCallerExecutions }

// stack holds what to close when the server stops, closed in reverse order
// of opening. Only Run owns one.
type stack []func(context.Context) error

func (st *stack) push(f func(context.Context) error) { *st = append(*st, f) }

func (st stack) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := len(st) - 1; i >= 0; i-- {
		if err := st[i](ctx); err != nil {
			slog.Warn("shutdown", "error", err)
		}
	}
}

// Run starts the agent server and serves until ctx ends or the server is
// stopped through its control socket. It opens the agent's state, builds
// the components, then serves; on the way out, background work stops first
// and everything opened is closed in reverse order.
func Run(ctx context.Context, opts Options) error {
	ctx, cancel := context.WithCancel(ctx)
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{
		dir:     opts.Dir,
		out:     opts.Out,
		started: time.Now(),
		cancel:  cancel,
		bus:     newEventBus(),

		credentials:       opts.Credentials,
		telegramServerURL: opts.TelegramServerURL,
		setupProblems:     opts.SetupProblems,
		private:           opts.Private,
		public:            opts.Public,
		newRelay:          opts.NewRelay,

		timings: opts.Timings.withDefaults(),
	}
	s.rt.Store(&runtimeConfig{cfg: opts.Config, secrets: opts.Secrets})
	var closers stack
	defer func() {
		cancel()
		closers.close()
	}()

	if err := s.open(ctx, &closers, opts.Executor == nil); err != nil {
		return startupError(ctx, err)
	}
	if err := s.build(ctx, &closers, opts); err != nil {
		return startupError(ctx, err)
	}
	chans := opts.Channels
	if chans == nil {
		var err error
		if chans, err = s.buildChannels(); err != nil {
			return err
		}
	}
	errc, err := s.serve(ctx, &closers, chans)
	if err != nil {
		return startupError(ctx, err)
	}
	if opts.Ready != nil {
		opts.Ready(s)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// startupError is the error a startup step failed with, or nil when a
// signal or a stop through the control socket ended ctx: then the step may
// fail with an error that doesn't wrap the cancellation, as a killed image
// pull does, and a service manager would restart a server that exits with it.
func startupError(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return err
	}
	slog.Debug("startup interrupted", "error", err)
	return nil
}

// open locks the agent directory and opens its database, the network
// providers and the listeners agent processes reach the server through.
func (s *Server) open(ctx context.Context, closers *stack, sandboxed bool) error {
	if err := s.dir.Init(); err != nil {
		return err
	}
	release, err := s.dir.Lock()
	if errors.Is(err, fsutil.ErrLocked) {
		return fmt.Errorf("an agent server is already running in %s (a2a-serve status)", s.dir.Root)
	}
	if err != nil {
		return fmt.Errorf("locking the agent directory: %w", err)
	}
	closers.push(func(context.Context) error { return release() })
	// Written before the slow startup steps, so that a stop can signal a
	// server whose control socket doesn't exist yet.
	if err := fsutil.WriteAtomic(s.dir.PIDPath(), []byte(strconv.Itoa(pid())), 0o600); err != nil {
		return fmt.Errorf("writing the pid file: %w", err)
	}
	closers.push(func(context.Context) error { return removeIfExists(s.dir.PIDPath()) })
	mounts, err := s.dir.Mounts(s.config())
	if err != nil {
		return err
	}
	s.pins = harness.Pin(mounts, s.hidden())
	st, err := store.Open(s.dir.DBPath())
	if err != nil {
		return err
	}
	closers.push(func(context.Context) error { return st.Close() })
	s.store = st
	st.SetTaskTTL(s.config().TaskTTLDuration())
	s.tasks = st.Tasks("", s.viewer)
	n, err := s.tasks.FailUnfinished(ctx, "The agent server restarted before this task finished. Send the message again.")
	if err != nil {
		return err
	}
	if n > 0 {
		slog.Info("failed tasks interrupted by the previous run", "count", n)
	}
	if err := s.setupNetwork(closers); err != nil {
		return err
	}
	hl, err := s.listenHarness(sandboxed)
	if err != nil {
		return err
	}
	closers.push(hl.close)
	main, err := hl.open("")
	if err != nil {
		return err
	}
	closers.push(func(context.Context) error { return main.close() })
	s.harness, s.mainHarness, s.bridge = hl, main, hl.bridge(main)
	return nil
}

// build creates the components and starts the harness. Nothing serves yet.
// Each component gets the ones it uses, so they are created in dependency
// order; the agent's handler runs them all, so it comes last and the
// components which send it turns hold s.sender, which it is set into.
func (s *Server) build(ctx context.Context, closers *stack, opts Options) error {
	agentName := func() string { return s.config().Name }
	ads, err := gossip.Open(filepath.Join(s.dir.StateDir(), "ads.json"), s.config().Name)
	if err != nil {
		return err
	}
	s.ads = ads
	s.chain = &auth.Chain{Store: s.store, Net: s.private, Policy: s.policy}
	google := googleauth.Google
	if opts.GoogleEndpoints != nil {
		google = *opts.GoogleEndpoints
	}
	s.oauth = oauth.New(oauth.Deps{
		Google:         google,
		SharedClientID: opts.SharedGoogleClientID,
		Tokens:         s.store,
		Config:         s.config,
		Secrets:        s.secrets,
	})
	s.nodes = nodes.New(ctx, nodes.Config{
		Store:          s.store,
		Viewer:         s.viewer,
		AgentName:      agentName,
		BaseURL:        s.bestURL,
		Authenticate:   s.authenticate,
		WebhookRetries: s.timings.WebhookRetries,
		Advertised:     s.ads.Advertised,
	})
	s.supervisor = newChannelSupervisor(ctx, SupervisorDeps{Store: s.store, Logf: s.logf, RetryMin: s.timings.ChannelRetryMin})
	s.pairings = &pairings.Service{
		Store:     s.store,
		BaseURL:   s.bestURL,
		Ads:       s.ads,
		LinkHints: s.supervisor.linkHints,
		Mailbox:   s.nodes.Mailbox,
		Changed:   s.refreshInstructions,
		Access:    s.access,
		Limiter:   pairings.NewLimiter(time.Now),
		Users:     stopUsers{s},
		Homes:     s.dir,
	}
	s.clients = newContactClients(ContactClientsDeps{Store: s.store, Private: s.private, Nodes: s.nodes, Ads: s.ads, AgentName: agentName})
	closers.push(s.clients.close)
	s.claims = newCallClaims(CallClaimsDeps{
		Store:  s.store,
		Bridge: s.bridge,
		Chain:  s.chain,
		// The executor's turns use the claims, so they come first; split
		// exists before the first turn.
		AgentKey: func(ctx context.Context, contextID string) (string, error) { return s.split.agentKey(ctx, contextID) },
		Sessions: s.store.Sessions(),
	})
	s.refreshInstructions(ctx)
	if err := s.buildExecutor(ctx, closers, opts); err != nil {
		return err
	}

	s.sender = &turns.Sender{}
	records := approvals.NewRecords(approvals.RecordsDeps{Store: s.store, Tasks: s.tasks, Chain: s.chain, BaseURL: s.bestURL})
	s.chatApprovals = newChatApprovals(ctx, ChatApprovalsDeps{
		Store:      s.store,
		Tasks:      s.tasks,
		Records:    records,
		Supervisor: s.supervisor,
		Bus:        s.bus,
		AgentName:  agentName,
		OwnerChat:  func() string { return s.config().Approvals.OwnerChat },
		Anonymous:  s.split.Anonymous,
		Timings:    s.timings,
	})
	unattended := &contextSet{}
	s.approvals = approvals.New(approvals.Deps{
		Records:          records,
		Sender:           s.sender,
		Notifier:         s.chatApprovals,
		Remote:           s.clients,
		Unattended:       unattended.has,
		Anonymous:        s.split.Anonymous,
		AnonymousTimeout: s.timings.AnonymousQuestionTimeout,
		AgentName:        agentName,
	})
	s.chatTurns = newChatTurns(ChatTurnsDeps{
		Store:      s.store,
		Tasks:      s.tasks,
		Chain:      s.chain,
		Pairings:   s.pairings,
		Approvals:  s.approvals,
		Chat:       s.chatApprovals,
		Supervisor: s.supervisor,
		Queue:      newConversationQueue(),
		Sender:     s.sender,
		Bus:        s.bus,
		Unattended: unattended,
		AgentName:  agentName,
	})
	s.schedules = newSchedules(ctx, schedulesDeps{
		Store:      s.store,
		Turns:      s.chatTurns,
		Supervisor: s.supervisor,
		Chain:      s.chain,
		Now:        opts.Now,
		MinWait:    s.timings.ScheduleMinWait,
	})
	s.watches = newWatches(ctx, WatchesDeps{
		Store:   s.store,
		Clients: s.clients,
		Records: records,
		Chat:    s.chatApprovals,
		Turns:   s.chatTurns,
		Chain:   s.chain,
		Timings: s.timings,
	})
	s.proxy = newHarnessProxy(HarnessProxyDeps{Store: s.store, Claims: s.claims, Clients: s.clients, Watches: s.watches, Schedules: s.schedules})
	s.inspector = inspector.New(inspector.Deps{
		Store:        s.store,
		Tasks:        s.tasks,
		Approvals:    s.approvals,
		ListPairings: s.pairings.List,
		Sender:       s.sender,
		AgentName:    agentName,
		BaseURL:      s.bestURL,
		Status:       s.status,
		MarkAnswered: s.chatApprovals.MarkAnswered,
	})

	s.handler = turns.RetryingCancel(a2asrv.NewHandler(&observedExecutor{inner: s.split, bus: s.bus, claims: s.claims, approvals: s.approvals, unattended: unattended},
		a2asrv.WithTaskStore(s.tasks),
		a2asrv.WithUpdateMaterializer(s.tasks.UpdateMaterializer()),
		a2asrv.WithCallInterceptors(&callerInterceptor{approvals: s.approvals, tasks: s.tasks, budget: s.budget}),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
		a2asrv.WithConcurrencyConfig(limiter.ConcurrencyConfig{MaxExecutions: maxExecutions, GetMaxExecutions: maxExecutionsOf}),
		a2asrv.WithLogger(slog.Default()),
	))
	s.sender.Set(s.handler)
	return nil
}

// buildExecutor starts the harness, and the executor which runs anonymous
// conversations in their own agent process.
func (s *Server) buildExecutor(ctx context.Context, closers *stack, opts Options) error {
	exec := opts.Executor
	if exec == nil {
		progress := opts.ImageProgress
		if progress == nil {
			progress = opts.HarnessLog
		}
		acp, closeACP, err := s.newExecutor(ctx, opts.HarnessLog, progress)
		if err != nil {
			return err
		}
		closers.push(closeACP)
		exec = acp
	}
	s.budget = newAnonymousBudget()
	s.split = &splitExecutor{main: exec, sessions: s.store.Sessions(), budget: s.budget, ctx: ctx}
	if opts.Executor == nil {
		s.split.start = func(ctx context.Context, pool string) (a2asrv.AgentExecutor, func(context.Context) error, error) {
			if pool == anonymousPool {
				return s.newAnonymousExecutor(ctx, opts.HarnessLog)
			}
			return s.newUsersExecutor(ctx, opts.HarnessLog)
		}
	}
	closers.push(s.split.close)
	return nil
}

// serve starts the chat turns, serves every listener and the control socket,
// starts the channels and resumes watches. Errors of listeners after the
// start arrive on the returned channel.
func (s *Server) serve(ctx context.Context, closers *stack, chans []channel.Channel) (<-chan error, error) {
	errc := make(chan error, 1)
	s.chatTurns.start(ctx)
	closers.push(s.supervisor.stopAll)
	closers.push(s.harness.serve(ctx, s.proxy.routes(s.harness.processOf), errc))
	if err := s.listenLocal(ctx, closers, errc); err != nil {
		return nil, err
	}
	if err := s.listenNetwork(ctx, closers, errc); err != nil {
		return nil, err
	}
	// Commands polling the control socket for the local URL use the owner
	// token right away, so it is saved before the socket serves.
	var ownerErr error
	if s.credentials != nil {
		ownerErr = s.saveLocalOwnerToken(ctx)
	}
	if err := s.serveControl(ctx, closers, errc); err != nil {
		return nil, err
	}
	for _, ch := range chans {
		s.supervisor.add(ch)
	}
	// Background work stops before the store closes: a query that starts
	// during shutdown could otherwise reopen the database after it closed.
	closers.push(func(context.Context) error {
		s.background.Wait()
		return nil
	})
	s.background.Go(func() { s.approvals.CloseStale(ctx) })
	s.watches.resume(ctx)
	s.background.Go(func() { s.pruneLoop(ctx) })
	s.background.Go(func() { s.schedules.run(ctx) })
	s.background.Go(func() { s.runGossip(ctx) })
	s.startBackups(ctx)
	return errc, s.printSummary(ctx, ownerErr)
}

// Stop shuts the server down.
func (s *Server) Stop() { s.cancel() }

func closeListener(ln net.Listener) error {
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func (s *Server) config() *agentdir.Config {
	c := *s.rt.Load().cfg
	return &c
}

// secrets are the secrets in use. Callers must not change them.
func (s *Server) secrets() agentdir.Secrets { return s.rt.Load().secrets }

// setSecrets replaces the secrets in use.
func (s *Server) setSecrets(secrets agentdir.Secrets) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.rt.Store(&runtimeConfig{cfg: s.rt.Load().cfg, secrets: secrets})
}

// reloadConfig applies the live settings of saved to the config in use.
func (s *Server) reloadConfig(saved *agentdir.Config) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	cur := s.rt.Load()
	next := *cur.cfg
	agentdir.CopyLive(&next, saved)
	s.rt.Store(&runtimeConfig{cfg: &next, secrets: cur.secrets})
}

// updateConfig applies fn to the config and saves it. A config.yaml that
// fails to parse is left alone and returned as the error.
func (s *Server) updateConfig(fn func(*agentdir.Config) error) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	cur := s.rt.Load()
	next := *cur.cfg
	if err := fn(&next); err != nil {
		return err
	}
	err := s.dir.LockConfig(func() error {
		// config.yaml may hold changes waiting for a restart; keep them.
		saved, err := s.dir.ReadConfig()
		switch {
		case errors.Is(err, agentdir.ErrNotFound):
			saved = &next
		case err != nil:
			return err
		default:
			if err := fn(saved); err != nil {
				return err
			}
		}
		return s.dir.SaveConfig(saved)
	})
	if err != nil {
		return err
	}
	s.rt.Store(&runtimeConfig{cfg: &next, secrets: cur.secrets})
	return nil
}

func (s *Server) logf(format string, args ...any) {
	if _, err := fmt.Fprintf(s.out, format+"\n", args...); err != nil {
		slog.Debug("writing progress", "error", err)
	}
}

// listenLocal opens the localhost listener. The first run picks a free port
// and saves it, so the local URL stays the same.
func (s *Server) listenLocal(ctx context.Context, closers *stack, errc chan<- error) error {
	port := s.config().Port
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listening on localhost:%d: %w", port, err)
	}
	if port == 0 {
		port = ln.Addr().(*net.TCPAddr).Port
		if err := s.updateConfig(func(c *agentdir.Config) error { c.Port = port; return nil }); err != nil {
			return errors.Join(err, ln.Close())
		}
	}
	u := "http://" + ln.Addr().String()
	s.urlMu.Lock()
	s.localURL = u
	s.urlMu.Unlock()
	closers.push(s.serveHTTP(ctx, ln, listener{kind: network.ListenerLocal, baseURL: u, inspector: true}, errc))
	return nil
}

// serveHTTP serves the routes of a listener in the background and returns
// the function which shuts the listener down.
func (s *Server) serveHTTP(ctx context.Context, ln net.Listener, l listener, errc chan<- error) func(context.Context) error {
	srv := httpserver.New(ctx, s.routes(l), httpserver.WithConnContext(connContext))
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			reportFatal(errc, fmt.Errorf("%s listener: %w", l.kind, err))
		}
	}()
	return func(ctx context.Context) error {
		err := srv.Shutdown(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			return srv.Close()
		}
		return err
	}
}

// reportFatal hands err to Run, which stops on the first one; later errors
// are only logged.
func reportFatal(errc chan<- error, err error) {
	select {
	case errc <- err:
	default:
		slog.Error("server error after shutdown started", "error", err)
	}
}

// URLs returns the URLs the agent is reachable at.
func (s *Server) URLs() (local, private, public string) {
	s.urlMu.Lock()
	defer s.urlMu.Unlock()
	return s.localURL, s.privateURL, s.publicURL
}

// bestURL is the URL others should use: public, then private, then local.
func (s *Server) bestURL() string {
	local, private, public := s.URLs()
	switch {
	case public != "":
		return public
	case private != "":
		return private
	}
	return local
}

// pruneInterval is how often the server drops expired and old records.
const pruneInterval = time.Hour

// pruneLoop prunes the store now and every pruneInterval until ctx ends.
// It also deletes the Google sign-in tokens of emails that --allow-email
// no longer matches.
func (s *Server) pruneLoop(ctx context.Context) {
	t := time.NewTicker(pruneInterval)
	defer t.Stop()
	for {
		if err := s.store.Prune(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("pruning the database", "error", err)
		}
		if err := s.store.PruneSignIns(ctx, s.policy().AllowsSignIn); err != nil && ctx.Err() == nil {
			slog.Warn("pruning Google sign-ins", "error", err)
		}
		s.claims.forgetPruned(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) printSummary(ctx context.Context, ownerErr error) error {
	local, private, public := s.URLs()
	s.logf("✓ local    %s", local)
	if private != "" {
		s.logf("✓ private  %s", private)
	}
	if public != "" {
		s.logf("✓ public   %s", public)
	}
	if s.credentials != nil {
		if ownerErr != nil {
			s.logf("! saving an owner token for this machine: %v", ownerErr)
		} else {
			s.logf("→ call it from this machine: a2a send -a %s \"…\"", local)
		}
	}
	if s.setupProblems != nil {
		for _, p := range s.setupProblems(ctx) {
			s.logf("! %s", p)
		}
	}
	a, err := s.access(ctx)
	if err != nil {
		return err
	}
	if w := localSandboxWarning(s.config().Sandbox, a); w != "" {
		s.logf("! %s", w)
	}
	return s.supervisor.announce(ctx)
}

// localSandboxWarning warns when an agent without a sandbox may be called
// by others than you: their conversations run in separate processes, but
// on the host as you, so they can read each other's files and yours.
func localSandboxWarning(sandbox string, a *controlapi.Access) string {
	if sandbox != harness.SandboxLocal || a.Pairings == a.OwnerPairings && len(a.Others) == 0 {
		return ""
	}
	return "the agent runs without a sandbox, so everyone who may call it can read your files and the conversations of others; use --sandbox docker unless you trust them all"
}

// viewer is the task store's view of the caller: owners see every task,
// everyone else their own. Calls without a principal come from the server
// itself.
func (s *Server) viewer(ctx context.Context) (string, bool, error) {
	if p, ok := auth.Caller(ctx); ok {
		return p.ID, p.IsOwner(), nil
	}
	return "", true, nil
}

// policy is the access policy in force.
func (s *Server) policy() auth.Policy {
	return access.Policy(s.config().Policy)
}

// access describes who may call the agent now.
func (s *Server) access(ctx context.Context) (*controlapi.Access, error) {
	all, err := s.store.Pairings(ctx)
	if err != nil {
		return nil, err
	}
	networks := access.Network{Public: s.public != nil}
	if s.private != nil {
		networks.Private = s.private.Name()
	}
	return access.Describe(s.config().Policy, networks, access.CountPairings(all)), nil
}

func pid() int { return os.Getpid() }
