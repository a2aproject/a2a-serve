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

package runner

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/agent"
	"github.com/a2aproject/a2a-serve/core/sandbox/boundary"
	"github.com/a2aproject/a2a-serve/core/workspace"
)

// Refusals of turns in processes whose grant changed.
var (
	// ErrGrantChanged refuses a turn in a busy process whose grant changed,
	// which restarts once its turns end.
	ErrGrantChanged = errors.New("the grant of this conversation's process changed, and the process restarts with it once its current tasks end; send the message again then")
	// ErrGrantNarrowed refuses an answer to a waiting task of a process
	// whose grant now gives less, which ends its waiting tasks.
	ErrGrantNarrowed = errors.New("the grant of this conversation's process changed and gives less, so its waiting tasks ended; send the message again")
)

// MissingSecretError is returned when a process gets a secret that has no
// stored value. Main is set for the main process.
type MissingSecretError struct {
	Name string
	Main bool
}

func (e *MissingSecretError) Error() string { return "secret " + e.Name + " is not set" }

// CopyError is returned when the private copy of the workspace can't be
// made, such as for a workspace over the limits of a copy.
type CopyError struct{ Err error }

func (e *CopyError) Error() string { return e.Err.Error() }

func (e *CopyError) Unwrap() error { return e.Err }

// admitTurn lets the turn of execCtx run in the process of key, and counts
// it there until release:
//
//   - a process whose grant now gives less ends its tasks and stops, so
//     that the turn starts it with the grant in force;
//   - a process whose grant changed otherwise stops if it is idle. If it
//     is busy, only answers to its waiting tasks run in it until it
//     restarts.
func (r *Runner) admitTurn(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (func(), error) {
	procs, err := r.poolOf(ctx, key)
	if err != nil || procs == nil {
		return func() {}, err
	}
	proc, err := r.cfg.Processes.Process(ctx, key)
	if err != nil {
		return nil, err
	}
	procs.setParent(procs.get(key), proc.Parent)
	want := proc.Grant
	answer := AnswersWaiting(execCtx)
	if running, ok := procs.grantOf(key); ok && !want.Covers(running) {
		// An answer fails its own task here; sandboxLoop then ends the
		// others, since stopping the container now would fail the task
		// without saying why.
		if answer {
			return nil, ErrGrantNarrowed
		}
		var asking a2a.TaskID
		if execCtx != nil {
			asking = execCtx.TaskID
		}
		if err := r.endNarrowed(ctx, procs, key, asking); err != nil {
			return nil, err
		}
	}
	counted, err := procs.admit(key, want, answer)
	if errors.Is(err, ErrGrantChanged) {
		counted, err = r.admitAfterEndedTurns(ctx, procs, key, want, execCtx)
	}
	if err != nil {
		return nil, err
	}
	return func() {
		counted()
		r.closeStale(ctx, procs, key)
	}, nil
}

// turnEndGrace bounds the wait of a turn for the turns of tasks that
// ended to finish.
const turnEndGrace = 5 * time.Second

// admitAfterEndedTurns admits a turn that the process of key refused while
// turns used it, once they end, if none of its tasks is unfinished. A turn
// ends a moment after its caller saw its task end, so the caller's next
// message would be refused although no task holds the process.
func (r *Runner) admitAfterEndedTurns(ctx context.Context, procs *procs, key string, want Grant, execCtx *a2asrv.ExecutorContext) (func(), error) {
	if r.cfg.Tasks == nil {
		return nil, ErrGrantChanged
	}
	ids, err := r.cfg.Tasks.UnfinishedIn(ctx, key)
	if err != nil {
		return nil, err
	}
	var own a2a.TaskID
	if execCtx != nil {
		own = execCtx.TaskID
	}
	if slices.ContainsFunc(ids, func(id a2a.TaskID) bool { return id != own }) || !procs.waitIdle(ctx, key, turnEndGrace) {
		return nil, ErrGrantChanged
	}
	return procs.admit(key, want, false)
}

// AnswersWaiting reports whether execCtx answers a task that waits for
// input, which continues in the process that asked.
func AnswersWaiting(execCtx *a2asrv.ExecutorContext) bool {
	return execCtx != nil && execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired
}

// endNarrowed cancels the unfinished tasks of the process of key, but the
// task of the turn that asks, and stops it, if its grant now gives less
// than it got: its tasks would keep the access they got.
func (r *Runner) endNarrowed(ctx context.Context, procs *procs, key string, asking a2a.TaskID) error {
	proc, err := r.cfg.Processes.Process(ctx, key)
	if err != nil {
		return err
	}
	want := proc.Grant
	if running, ok := procs.grantOf(key); !ok || want.Covers(running) {
		return nil
	}
	var ids []a2a.TaskID
	if r.cfg.Tasks != nil {
		if ids, err = r.cfg.Tasks.UnfinishedIn(ctx, key); err != nil {
			return err
		}
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
			if err := r.cfg.Tasks.Cancel(context.WithoutCancel(ctx), t); err != nil {
				slog.Warn("canceling a task of an agent process whose sandbox rules now give less", "task", t, "error", err)
			}
		}
	}()
	select {
	case <-canceled:
	case <-time.After(r.cfg.Timings.NarrowedCancelGrace):
	}
	stopped, err := procs.closeNarrowed(key, want)
	if stopped {
		slog.Info("stopped an agent process whose sandbox rules now give less", "key", key, "canceled", len(ids))
	}
	return err
}

// closeStale stops the process of key if no turn uses it and its grant
// changed. The main process keeps its grant until it restarts.
func (r *Runner) closeStale(ctx context.Context, procs *procs, key string) {
	proc, err := r.cfg.Processes.Process(ctx, key)
	if err != nil {
		slog.Warn("looking up the sandbox of an agent process", "key", key, "error", err)
		return
	}
	stopped, err := procs.closeStale(key, proc.Grant)
	if err != nil {
		slog.Warn("stopping an agent process whose sandbox changed", "key", key, "error", err)
	}
	if stopped {
		slog.Info("stopped an agent process whose sandbox changed; its next turn starts it again", "key", key)
	}
}

// sandboxLoop applies changes of the grants to the processes that no turn
// starts: an idle one stops, and a busy one whose grant now gives less ends
// its tasks, so that a removed secret doesn't wait for them.
func (r *Runner) sandboxLoop(ctx context.Context) {
	if r.cfg.Timings.SandboxCheck <= 0 {
		return
	}
	t := time.NewTicker(r.cfg.Timings.SandboxCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var wg sync.WaitGroup
		for _, procs := range r.pools {
			for _, key := range procs.runningKeys() {
				wg.Go(func() { r.applyGrantChange(ctx, procs, key) })
			}
		}
		wg.Wait()
	}
}

func (r *Runner) applyGrantChange(ctx context.Context, procs *procs, key string) {
	proc, err := r.cfg.Processes.Process(ctx, key)
	if err != nil {
		slog.Warn("looking up the sandbox of an agent process", "key", key, "error", err)
		return
	}
	if running, ok := procs.grantOf(key); ok && !proc.Grant.Covers(running) {
		if err := r.endNarrowed(ctx, procs, key, ""); err != nil {
			slog.Warn("stopping an agent process whose sandbox rules now give less", "key", key, "error", err)
		}
		return
	}
	r.closeStale(ctx, procs, key)
}

// grantMounts returns the mounts of a container with storage: as they are
// for shared, read-only for read-only and own, and only the workspace for
// empty. Own and empty mount a private workspace in place of the workspace.
func grantMounts(storage workspace.Storage, mounts []agent.Mount) []agent.Mount {
	switch storage {
	case workspace.StorageShared:
		return mounts
	case workspace.StorageEmpty:
		ws := mounts[0]
		ws.ReadOnly = false
		return []agent.Mount{ws}
	}
	out := slices.Clone(mounts)
	for i := range out {
		out[i].ReadOnly = storage != workspace.StorageOwn || i > 0
	}
	return out
}

// grantSandbox gives spec the storage and secrets of the grant of proc, the
// process of key, and returns the headers its boundary adds. Without the
// Docker sandbox, only the secrets apply.
func (r *Runner) grantSandbox(ctx context.Context, spec *agent.Spec, cfg Spec, key string, proc Process) ([]boundary.Injection, error) {
	g := proc.Grant
	values, inject, missing, err := secretEnv(cfg.Inject, g, r.cfg.Secrets())
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, &MissingSecretError{Name: missing[0], Main: key == ""}
	}
	spec.Env = append(spec.Env, values...)
	if spec.Sandbox == agent.SandboxLocal {
		return inject, nil
	}
	ws := spec.Mounts[0].Src
	spec.Mounts = grantMounts(g.Storage, spec.Mounts)
	if g.Storage != workspace.StorageOwn && g.Storage != workspace.StorageEmpty {
		return inject, nil
	}
	dir, err := r.copies.Prepare(ctx, proc.CopyName, proc.Owner, g.Storage, ws)
	if errors.Is(err, workspace.ErrTooLarge) || errors.Is(err, workspace.ErrTooMany) {
		return nil, &CopyError{Err: err}
	}
	if err != nil {
		return nil, err
	}
	spec.WorkspaceSrc = dir
	return inject, nil
}
