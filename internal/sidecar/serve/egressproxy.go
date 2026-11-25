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
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// How the ready line names what decides the Docker sandbox's connections:
// the egress policy, the egress proxy, or nothing, when the sandbox can't
// route them through a2a-serve-sidecar and uses Docker's own network.
const (
	EgressPolicy = "policy"
	EgressProxy  = "proxy"
	EgressNone   = "none"
)

// resolveEgressProxy checks the --egress-proxy URL of cfg, which replaces
// the egress rules and the model keys.
func resolveEgressProxy(cfg Config) (string, error) {
	if cfg.EgressProxy == "" {
		return "", nil
	}
	var conflicts []string
	for flag, set := range map[string]bool{
		"--egress":               cfg.Egress != "",
		"--egress-allow":         len(cfg.EgressAllow) > 0,
		"--egress-middleware":    len(cfg.EgressMiddleware) > 0,
		"--inject-model-key":     len(cfg.ModelKeys) > 0,
		"--egress-inject-header": len(cfg.InjectHeaders) > 0,
	} {
		if set {
			conflicts = append(conflicts, flag)
		}
	}
	if len(conflicts) > 0 {
		slices.Sort(conflicts)
		return "", usagef("--egress-proxy can't be used with %s: the proxy decides every connection of the agent and adds the credentials", strings.Join(conflicts, ", "))
	}
	if _, err := egress.NewProxyRouter(egress.ProxyConfig{URL: cfg.EgressProxy}); err != nil {
		return "", usagef("invalid --egress-proxy: %w", err)
	}
	return cfg.EgressProxy, nil
}

// placeholderKeys returns the placeholder API keys of the model providers
// that env doesn't set, so that the agent starts; the egress proxy adds
// the real keys.
func placeholderKeys(env []string) []string {
	var out []string
	for _, name := range egressrules.ProviderNames() {
		p, _ := egressrules.LookupProvider(name)
		if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, p.KeyEnv+"=") }) {
			continue
		}
		out = append(out, p.KeyEnv+"="+p.Placeholder(p.KeyEnv))
	}
	return out
}

// newBoundary returns the egress boundary of the Docker sandbox: one whose
// router is the egress proxy, with the proxy router, or one of the policy
// (wrapped by any --egress-middleware). A non-nil wrap wraps the router,
// which makes the policy a router too.
func newBoundary(s settings, log *slog.Logger, turns *turns, wrap func(egress.Router) egress.Router) (*egress.Boundary, *egress.ProxyRouter, *egress.MiddlewareRouter, error) {
	if s.egressProxy == "" && len(s.egressMiddleware) == 0 && wrap == nil {
		policy := s.policy
		b, err := egress.New(egress.Config{Policy: func() egress.Policy { return policy }, Log: log})
		return b, nil, nil, err
	}
	var (
		r     egress.Router
		proxy *egress.ProxyRouter
		mw    *egress.MiddlewareRouter
		err   error
	)
	if s.egressProxy != "" {
		if proxy, err = egress.NewProxyRouter(egress.ProxyConfig{URL: s.egressProxy, Task: turns.task, Token: hostTokenIfLoopback(s.egressProxy, s.hostToken)}); err != nil {
			return nil, nil, nil, err
		}
		r = proxy
	} else {
		if r, err = s.policy.Router(s.credentials...); err != nil {
			// The router allows the hosts of the credentials in every mode; the
			// endpoints of the processes add the credentials themselves.
			return nil, nil, nil, err
		}
		if len(s.egressMiddleware) > 0 {
			if mw, err = egress.NewMiddlewareRouter(egress.MiddlewareConfig{Entries: s.egressMiddleware, Next: r, Task: turns.task}); err != nil {
				return nil, nil, nil, err
			}
			r = mw
		}
	}
	if wrap != nil {
		r = wrap(r)
	}
	b, err := egress.New(egress.Config{Router: r, Log: log})
	if err != nil {
		return nil, nil, nil, err
	}
	return b, proxy, mw, nil
}

// turns knows the task that each context's process works on, for the
// requests that the egress proxy and the contacts get.
type turns struct {
	mu sync.Mutex
	// running holds the turn of each context with a turn in progress.
	running map[string]runningTurn
}

// runningTurn is the task of a turn in progress, and the hop count of the
// request that started it.
type runningTurn struct {
	task string
	hops int
}

func newTurns() *turns { return &turns{running: map[string]runningTurn{}} }

// turn records the task of execCtx until the turn ends.
func (t *turns) turn(_ context.Context, execCtx *a2asrv.ExecutorContext) func() {
	contextID, task := execCtx.ContextID, string(execCtx.TaskID)
	t.mu.Lock()
	t.running[contextID] = runningTurn{task: task, hops: hopsOf(execCtx)}
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.running[contextID].task == task {
			delete(t.running, contextID)
		}
	}
}

// task returns the task of the process of key, a context ID, or "".
func (t *turns) task(key string) string {
	cur, _ := t.current(key)
	return cur.task
}

// current returns the turn in progress of the process of key, a context
// ID, if one runs.
func (t *turns) current(key string) (runningTurn, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, ok := t.running[key]
	return cur, ok
}
