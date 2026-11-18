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
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentcfg"
	"github.com/a2aproject/a2a-serve/internal/agentrun"
)

// changedFrom reports whether a process that started with a state got
// another sandbox than want.
func changedFrom(want access.Sandbox) func(state any) bool {
	return func(state any) bool { return !stateOf(state).Equal(want) }
}

// admitTurn lets the turn of execCtx run in the container of key, and
// counts it there until release:
//
//   - a container whose sandbox now gives less ends its tasks and stops, so
//     that the turn starts it with the sandbox in force;
//   - a container whose sandbox changed otherwise stops if it is idle. If
//     it is busy, only answers to its waiting tasks run in it until it
//     restarts.
//
// The owner's container keeps its sandbox until the restart.
func (s *Server) admitTurn(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (func(), error) {
	if key == "" {
		return func() {}, nil
	}
	_, want, err := s.sandboxes.grant(ctx, key)
	if err != nil {
		return nil, err
	}
	parent := parentOf(key)
	answer := agentrun.AnswersWaiting(execCtx)
	if running, ok := s.startedWith(key); ok && !want.Covers(running) {
		// An answer fails its own task here; sandboxLoop then ends the
		// others, since stopping the container now would fail the task
		// without saying why.
		if answer {
			return nil, agentcfg.ErrGrantNarrowed
		}
		var asking a2a.TaskID
		if execCtx != nil {
			asking = execCtx.TaskID
		}
		if err := s.endNarrowed(ctx, key, asking); err != nil {
			return nil, err
		}
	}
	counted, err := s.engine.AdmitTurn(ctx, key, parent, changedFrom(want), answer)
	if errors.Is(err, agentrun.ErrStale) {
		counted, err = s.admitAfterEndedTurns(ctx, key, parent, want, execCtx)
	}
	if err != nil {
		return nil, err
	}
	return func() {
		counted()
		s.closeStale(ctx, key)
	}, nil
}

// turnEndGrace bounds the wait of a turn for the turns of tasks that
// ended to finish.
const turnEndGrace = 5 * time.Second

// admitAfterEndedTurns admits a turn that the container of key refused
// while turns used it, once they end, if none of its tasks is unfinished.
// A turn ends a moment after its caller saw its task end, so the caller's
// next message would be refused although no task holds the container.
func (s *Server) admitAfterEndedTurns(ctx context.Context, key, parent string, want access.Sandbox, execCtx *a2asrv.ExecutorContext) (func(), error) {
	ids, err := s.tasks.UnfinishedIn(ctx, key)
	if err != nil {
		return nil, err
	}
	var own a2a.TaskID
	if execCtx != nil {
		own = execCtx.TaskID
	}
	if slices.ContainsFunc(ids, func(id a2a.TaskID) bool { return id != own }) || !s.engine.WaitIdle(ctx, key, turnEndGrace) {
		return nil, agentcfg.ErrGrantChanged
	}
	counted, err := s.engine.AdmitTurn(ctx, key, parent, changedFrom(want), false)
	if errors.Is(err, agentrun.ErrStale) {
		return nil, agentcfg.ErrGrantChanged
	}
	return counted, err
}

// endNarrowed cancels the unfinished tasks of the container of key, but
// the task of the turn that asks, and stops it, if its sandbox now gives
// less than it got: its tasks would keep the access they got.
func (s *Server) endNarrowed(ctx context.Context, key string, asking a2a.TaskID) error {
	_, want, err := s.sandboxes.grant(ctx, key)
	if err != nil {
		return err
	}
	if running, ok := s.startedWith(key); !ok || want.Covers(running) {
		return nil
	}
	ids, err := s.tasks.UnfinishedIn(ctx, key)
	if err != nil {
		return err
	}
	// An agent can take long to stop a task, or never; the process closes
	// after the grace whatever the cancels report, which ends them.
	canceled := make(chan struct{})
	go func() {
		defer close(canceled)
		for _, t := range ids {
			if t == asking {
				continue
			}
			if err := (runnerTasks{s}).Cancel(context.WithoutCancel(ctx), t); err != nil {
				slog.Warn("canceling a task of an agent process whose sandbox rules now give less", "task", t, "error", err)
			}
		}
	}()
	select {
	case <-canceled:
	case <-time.After(s.timings.NarrowedCancelGrace):
	}
	stopped, err := s.engine.CloseIf(ctx, key, func(state any) bool { return !want.Covers(stateOf(state)) })
	if stopped {
		slog.Info("stopped an agent process whose sandbox rules now give less", "key", key, "canceled", len(ids))
	}
	return err
}

// closeStale stops the container of key if no turn uses it and its
// sandbox changed. The owner's container keeps its sandbox until the
// restart.
func (s *Server) closeStale(ctx context.Context, key string) {
	_, want, err := s.sandboxes.grant(ctx, key)
	if err != nil {
		slog.Warn("looking up the sandbox of an agent process", "key", key, "error", err)
		return
	}
	stopped, err := s.engine.CloseIdleIf(key, changedFrom(want))
	if err != nil {
		slog.Warn("stopping an agent process whose sandbox changed", "key", key, "error", err)
	}
	if stopped {
		slog.Info("stopped an agent process whose sandbox changed; its next turn starts it again", "key", key)
	}
}

// sandboxLoop applies changes of the sandbox rules to the containers that
// no turn starts: an idle one stops, and a busy one whose sandbox now gives
// less ends its tasks, so that a removed secret doesn't wait for them.
func (s *Server) sandboxLoop(ctx context.Context) {
	if s.timings.SandboxCheckInterval <= 0 {
		return
	}
	t := time.NewTicker(s.timings.SandboxCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var wg sync.WaitGroup
		for _, p := range s.agent.Processes() {
			if p.Key == "" {
				continue
			}
			wg.Go(func() { s.applyRuleChange(ctx, p.Key) })
		}
		wg.Wait()
	}
}

func (s *Server) applyRuleChange(ctx context.Context, key string) {
	_, want, err := s.sandboxes.grant(ctx, key)
	if err != nil {
		slog.Warn("looking up the sandbox of an agent process", "key", key, "error", err)
		return
	}
	if running, ok := s.startedWith(key); ok && !want.Covers(running) {
		if err := s.endNarrowed(ctx, key, ""); err != nil {
			slog.Warn("stopping an agent process whose sandbox rules now give less", "key", key, "error", err)
		}
		return
	}
	s.closeStale(ctx, key)
}
