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
	"io"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
)

// agentProcesses are the running agent processes of all executors. An
// agent remembers the tool calls a caller allowed always until its process
// exits, so tighter deny rules restart them.
type agentProcesses struct {
	mu   sync.Mutex
	live map[io.ReadWriteCloser]struct{}
}

func newAgentProcesses() *agentProcesses {
	return &agentProcesses{live: map[io.ReadWriteCloser]struct{}{}}
}

// track returns agent, whose processes stay in p while they run.
func (p *agentProcesses) track(agent a2aacp.Agent) a2aacp.Agent {
	return a2aacp.AgentFunc(func(ctx context.Context, key string) (io.ReadWriteCloser, error) {
		rwc, err := agent.Start(ctx, key)
		if err != nil {
			return nil, err
		}
		var proc io.ReadWriteCloser
		proc = a2aacp.WithCleanup(rwc, func() error {
			p.mu.Lock()
			defer p.mu.Unlock()
			delete(p.live, proc)
			return nil
		})
		p.mu.Lock()
		defer p.mu.Unlock()
		p.live[proc] = struct{}{}
		return proc, nil
	})
}

// restart stops every running agent process. Its executor starts it again
// for the next task, and the turns in progress in it fail. It returns how
// many processes it stopped.
func (p *agentProcesses) restart() (int, error) {
	p.mu.Lock()
	procs := make([]io.ReadWriteCloser, 0, len(p.live))
	for proc := range p.live {
		procs = append(procs, proc)
	}
	p.mu.Unlock()
	var errs []error
	for _, proc := range procs {
		errs = append(errs, proc.Close())
	}
	return len(procs), errors.Join(errs...)
}

// tightened reports whether after denies a pattern that before doesn't.
func tightened(before, after []string) bool {
	return slices.ContainsFunc(after, func(p string) bool { return !slices.Contains(before, p) })
}
