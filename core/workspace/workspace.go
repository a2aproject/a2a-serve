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

// Package workspace gives each agent process the project directory it works
// in: a directory shared by all processes, a private copy per process, or an
// empty directory per process.
package workspace

import (
	"context"
	"errors"
)

// Workspace gives each process its project directory.
type Workspace interface {
	// Attach returns the directory of the process of key, and keeps it until
	// Release. Copies prepare a copy here.
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
}

// Dir returns the workspace of the directory path, which all processes share
// and may change.
func Dir(path string) Workspace { return dirWorkspace(path) }

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
	if r.w == nil {
		return Attachment{}, errors.New("workspace: no workspace to make read-only")
	}
	a, err := r.w.Attach(ctx, key)
	if err != nil {
		return Attachment{}, err
	}
	a.ReadOnly = true
	return a, nil
}
