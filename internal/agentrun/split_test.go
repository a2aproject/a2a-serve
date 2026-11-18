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

package agentrun

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
	"github.com/google/go-cmp/cmp"
)

// noSessions is a session store without sessions.
type noSessions struct{}

func (noSessions) Get(context.Context, string) (a2aacp.Session, bool, error) {
	return a2aacp.Session{}, false, nil
}

func (noSessions) Put(context.Context, string, a2aacp.Session) error { return nil }

// testSplitRunner returns a runner whose new conversations run under key,
// in the pool users unless key is "".
func testSplitRunner(key string) *Runner {
	known := fakeProcesses{"": {}}
	if key != "" {
		known[key] = Process{Pool: "users"}
	}
	return &Runner{
		cfg:   Config{Processes: known},
		keys:  newKeys(noSessions{}, func(context.Context, string) (string, error) { return key, nil }),
		pools: map[string]*procs{"users": {}},
	}
}

func TestSplitExecutorClosePoolsInParallel(t *testing.T) {
	t.Parallel()
	var running sync.WaitGroup
	running.Add(2)
	stop := func(context.Context) error {
		running.Done()
		done := make(chan struct{})
		go func() { running.Wait(); close(done) }()
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the other pool did not stop at the same time")
		}
	}
	e := &splitExecutor{pools: map[string]*startedPool{
		"guests": {stop: stop},
		"users":  {stop: stop},
	}}

	if err := e.close(context.Background()); err != nil {
		t.Fatalf("splitExecutor.close() error = %v", err)
	}
}

// emptyExecutor ends every turn without events, as a turn that fails before
// the agent stores a session.
type emptyExecutor struct{ a2asrv.AgentExecutor }

func (emptyExecutor) Execute(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func TestSplitExecutorForgetsPendingKeyAfterTurnWithoutSession(t *testing.T) {
	t.Parallel()
	r := testSplitRunner("user:anna")
	e := newSplitExecutor(context.Background(), r, emptyExecutor{}, func(context.Context, string) (a2asrv.AgentExecutor, func(context.Context) error, error) {
		return emptyExecutor{}, func(context.Context) error { return nil }, nil
	})

	for range e.Execute(context.Background(), &a2asrv.ExecutorContext{ContextID: "c1", TaskID: "t1"}) {
	}

	r.keys.mu.Lock()
	defer r.keys.mu.Unlock()
	if got := len(r.keys.pending); got != 0 {
		t.Fatalf("keys.pending has %d entries after the turn, want 0", got)
	}
}

func TestSplitExecutorStartsNoPoolAfterClose(t *testing.T) {
	t.Parallel()
	started := false
	e := newSplitExecutor(context.Background(), testSplitRunner("user:anna"), emptyExecutor{}, func(context.Context, string) (a2asrv.AgentExecutor, func(context.Context) error, error) {
		started = true
		return emptyExecutor{}, func(context.Context) error { return nil }, nil
	})
	if err := e.close(context.Background()); err != nil {
		t.Fatalf("splitExecutor.close() error = %v", err)
	}

	_, err := e.pool("users")

	if err == nil {
		t.Fatalf("splitExecutor.pool() after close error = nil, want error")
	}
	if started {
		t.Fatalf("splitExecutor.pool() after close started the pool, want it not started")
	}
}

// blockingExecutor runs every turn until its context ends.
type blockingExecutor struct {
	a2asrv.AgentExecutor
	started chan struct{}
	ended   atomic.Bool
}

func (b *blockingExecutor) Execute(ctx context.Context, _ *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {
		close(b.started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		b.ended.Store(true)
	}
}

// lockingExecutor runs one turn at a time, as a2aacp runs the turns of a
// conversation: a turn waits for the turn before it, and doesn't start once
// its context ended. The first turn runs until its context ends; later ones
// complete at once.
type lockingExecutor struct {
	a2asrv.AgentExecutor
	lock    chan struct{}
	holding chan struct{}
	waiting chan struct{}
}

func (l *lockingExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		first := execCtx.TaskID == "t1"
		if !first {
			close(l.waiting)
		}
		select {
		case l.lock <- struct{}{}:
		case <-ctx.Done():
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, nil), nil)
			return
		}
		defer func() { <-l.lock }()
		if ctx.Err() != nil {
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, nil), nil)
			return
		}
		if first {
			close(l.holding)
			<-ctx.Done()
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, nil), nil)
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func TestSplitExecutorStopEndsTheTurnsWaitingBehindARunningTurn(t *testing.T) {
	t.Parallel()
	for range 200 {
		serverCtx, stopServer := context.WithCancel(context.Background())
		inner := &lockingExecutor{lock: make(chan struct{}, 1), holding: make(chan struct{}), waiting: make(chan struct{})}
		e := newSplitExecutor(serverCtx, testSplitRunner(""), inner, nil)
		go func() {
			for range e.Execute(context.Background(), &a2asrv.ExecutorContext{ContextID: "c1", TaskID: "t1"}) {
			}
		}()
		<-inner.holding
		states := make(chan []a2a.TaskState, 1)
		go func() {
			var got []a2a.TaskState
			for ev := range e.Execute(context.Background(), &a2asrv.ExecutorContext{ContextID: "c1", TaskID: "t2", StoredTask: &a2a.Task{}}) {
				if st, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
					got = append(got, st.Status.State)
				}
			}
			states <- got
		}()
		<-inner.waiting

		stopServer()

		if diff := cmp.Diff([]a2a.TaskState{a2a.TaskStateFailed}, <-states); diff != "" {
			t.Fatalf("splitExecutor.Execute() of a waiting turn after the stop wrong states (-want +got) diff = %s", diff)
		}
	}
}

func TestSplitExecutorCloseStopsTurnsFirst(t *testing.T) {
	t.Parallel()
	serverCtx, stopServer := context.WithCancel(context.Background())
	inner := &blockingExecutor{started: make(chan struct{})}
	e := newSplitExecutor(serverCtx, testSplitRunner(""), inner, nil)
	go func() {
		for range e.Execute(context.Background(), &a2asrv.ExecutorContext{ContextID: "c1", TaskID: "t1"}) {
		}
	}()
	<-inner.started

	stopServer()
	if err := e.close(context.Background()); err != nil {
		t.Fatalf("splitExecutor.close() error = %v", err)
	}

	if !inner.ended.Load() {
		t.Fatal("splitExecutor.close() returned while a turn ran, want it to stop the turns first")
	}
	var got []a2a.TaskState
	for ev, err := range e.Execute(context.Background(), &a2asrv.ExecutorContext{ContextID: "c2", TaskID: "t2"}) {
		if err != nil {
			t.Fatalf("splitExecutor.Execute() after close() error = %v", err)
		}
		if st, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
			got = append(got, st.Status.State)
		}
	}
	if diff := cmp.Diff([]a2a.TaskState{a2a.TaskStateFailed}, got); diff != "" {
		t.Fatalf("splitExecutor.Execute() after close() wrong states (-want +got) diff = %s", diff)
	}
}

// cancelCounter ends every turn without events and counts its cancels.
type cancelCounter struct {
	emptyExecutor
	canceled atomic.Int32
}

func (c *cancelCounter) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	c.canceled.Add(1)
	return func(func(a2a.Event, error) bool) {}
}

func TestRefusedTurnOfAWaitingTask(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused")
	refuse := func(context.Context, string, *a2asrv.ExecutorContext) (func(), error) { return nil, refused }
	tests := []struct {
		name         string
		hooks        Hooks
		wantCanceled int32
	}{
		{name: "admit refuses", hooks: Hooks{Admit: refuse}, wantCanceled: 1},
		{name: "hold refuses", hooks: Hooks{Hold: refuse}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := &cancelCounter{}
			r := testSplitRunner("")
			r.cfg.Hooks = tc.hooks
			e := newSplitExecutor(context.Background(), r, inner, nil)
			waiting := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}

			var got []a2a.TaskState
			for ev, err := range e.Execute(context.Background(), &a2asrv.ExecutorContext{ContextID: "c1", TaskID: "t1", StoredTask: waiting}) {
				if err != nil {
					t.Fatalf("splitExecutor.Execute() error = %v", err)
				}
				if st, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
					got = append(got, st.Status.State)
				}
			}

			if diff := cmp.Diff([]a2a.TaskState{a2a.TaskStateFailed}, got); diff != "" {
				t.Fatalf("splitExecutor.Execute() wrong states (-want +got) diff = %s", diff)
			}
			if n := inner.canceled.Load(); n != tc.wantCanceled {
				t.Fatalf("splitExecutor.Execute() canceled the waiting turn %d times, want %d", n, tc.wantCanceled)
			}
		})
	}
}
