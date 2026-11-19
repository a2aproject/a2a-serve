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

//go:build linux

package fusefs

import (
	"context"
	"io"
	"io/fs"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// detached is a tree whose operations the kernel's interrupts don't
// cancel. A signal to a process in a file operation interrupts it, and the
// Go runtime signals its threads to preempt them, so a canceled operation
// would fail the close or the rename of a Go program with EINTR, which it
// doesn't retry.
type detached struct{ tree fsys.FS }

var (
	_ fsys.FS          = detached{}
	_ fsys.RangeOpener = detached{}
)

func (d detached) Stat(ctx context.Context, p string) (fsys.Info, error) {
	return d.tree.Stat(context.WithoutCancel(ctx), p)
}

func (d detached) ReadDir(ctx context.Context, p string) ([]fsys.Info, error) {
	return d.tree.ReadDir(context.WithoutCancel(ctx), p)
}

func (d detached) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	return d.tree.Open(context.WithoutCancel(ctx), p)
}

func (d detached) OpenRange(ctx context.Context, p string, offset, length int64) (io.ReadCloser, error) {
	return fsys.OpenRange(context.WithoutCancel(ctx), d.tree, p, offset, length)
}

func (d detached) Write(ctx context.Context, p string, r io.Reader, mode fs.FileMode, ifVersion string) (fsys.Info, error) {
	return d.tree.Write(context.WithoutCancel(ctx), p, r, mode, ifVersion)
}

func (d detached) Mkdir(ctx context.Context, p string, mode fs.FileMode) error {
	return d.tree.Mkdir(context.WithoutCancel(ctx), p, mode)
}

func (d detached) Symlink(ctx context.Context, target, p string) error {
	return d.tree.Symlink(context.WithoutCancel(ctx), target, p)
}

func (d detached) Rename(ctx context.Context, from, to string) error {
	return d.tree.Rename(context.WithoutCancel(ctx), from, to)
}

func (d detached) Remove(ctx context.Context, p string) error {
	return d.tree.Remove(context.WithoutCancel(ctx), p)
}

func (d detached) Changes(ctx context.Context, cursor string) ([]fsys.Info, string, error) {
	return d.tree.Changes(ctx, cursor)
}
