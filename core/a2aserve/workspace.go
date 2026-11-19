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

package a2aserve

import (
	"context"
	"errors"
)

// Workspace gives each process its project directory. [SharedDir], [Copies],
// [EmptyDir] and [TurnSynced] are the workspaces of this package.
type Workspace interface {
	// Attach returns the directory of the process of key, and keeps it until
	// Release. [Copies] prepares a copy here.
	Attach(ctx context.Context, key string) (Attachment, error)
}

// Attachment is the directory of a process.
type Attachment struct {
	// Dir is the directory on the host.
	Dir string
	// Path is where the process sees the directory; Dir if empty.
	Path     string
	ReadOnly bool
	// Release ends the attachment. Private directories stay for the next
	// Attach of the same key.
	Release func() error
	// BeforeTurn, if set, runs before the agent gets a message, other than
	// an answer to its question. Its error fails the turn, and the agent
	// doesn't get the message.
	BeforeTurn func(ctx context.Context) error
	// AfterTurn, if set, runs once the agent stopped working on a message:
	// when it finished, failed or was canceled, or its process exited. Its
	// error fails the task through Hooks.Explain, also one the agent
	// completed.
	AfterTurn func(ctx context.Context) error
}

// stateWorkspace is a workspace that keeps its directories in the state
// directory of the Agent unless it was given one.
type stateWorkspace interface {
	attachIn(ctx context.Context, key, stateDir string) (Attachment, error)
}

// SharedDir returns the workspace of the directory path, which all
// processes share and may change.
func SharedDir(path string) Workspace { return dirWorkspace(path) }

// ReadOnly returns w with the directories it attaches marked read-only.
func ReadOnly(w Workspace) Workspace { return readOnly{w} }

type dirWorkspace string

func (d dirWorkspace) Attach(ctx context.Context, key string) (Attachment, error) {
	if err := ctx.Err(); err != nil {
		return Attachment{}, err
	}
	return Attachment{Dir: string(d), Release: func() error { return nil }}, nil
}

type readOnly struct{ w Workspace }

func (r readOnly) Attach(ctx context.Context, key string) (Attachment, error) {
	return r.attachIn(ctx, key, "")
}

func (r readOnly) attachIn(ctx context.Context, key, stateDir string) (Attachment, error) {
	if r.w == nil {
		return Attachment{}, errors.New("a2aserve: no workspace to make read-only")
	}
	a, err := attachIn(ctx, r.w, key, stateDir)
	if err != nil {
		return Attachment{}, err
	}
	a.ReadOnly = true
	return a, nil
}

// attachIn attaches the directory of key in w, which keeps it in stateDir
// unless it was given a directory.
func attachIn(ctx context.Context, w Workspace, key, stateDir string) (Attachment, error) {
	if sw, ok := w.(stateWorkspace); ok {
		return sw.attachIn(ctx, key, stateDir)
	}
	return w.Attach(ctx, key)
}
