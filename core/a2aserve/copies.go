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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/workspaces"
)

// Errors of [Copies]. They are the errors the copying returns, wrapped with
// the reason.
var (
	// ErrWorkspaceTooLarge is returned for a directory over the limits of a
	// copy.
	ErrWorkspaceTooLarge = workspaces.ErrTooLarge
	// ErrTooManyCopies is returned for a new copy once Copies keeps the
	// most it may.
	ErrTooManyCopies = workspaces.ErrTooMany
)

// CopyOptions configures [Copies].
type CopyOptions struct {
	// Dir is where the copies live. It holds one directory per key, named
	// after it: the key with the characters outside letters, digits, "." and
	// "-" replaced by "_", cut to 40 characters, and the first 16 hex digits
	// of its SHA-256 hash. Keys that differ in any character get different
	// directories.
	Dir string
	// MaxBytes bounds the size of the regular files of a copy, MaxEntries
	// its files, directories and symlinks, and MaxCopies the copies in Dir.
	// Zero takes the default: 1 GiB, 200000 entries and 100 copies.
	MaxBytes   int64
	MaxEntries int
	MaxCopies  int
}

// Copies returns the workspace that gives each key a private copy of the
// directory src. The first Attach of a key makes the copy, which skips the
// agent directories (.a2a) at any depth. The process sees the copy at the
// path of src. Later Attach calls of the key return the same copy with its
// changes, and src stays unchanged. Release marks the copy as used and keeps
// it.
func Copies(src string, opts CopyOptions) Workspace {
	return &privateWorkspace{
		engine: &workspaces.Copies{Root: opts.Dir, MaxBytes: opts.MaxBytes, MaxEntries: opts.MaxEntries, MaxCopies: opts.MaxCopies},
		src:    src,
		at:     src,
		mode:   workspaces.StorageOwn,
	}
}

// EmptyDir returns the workspace that gives each key an empty directory
// under dir, named as the copies of Copies are. The process sees it at the path at,
// or at its place on the host if at is empty. Release marks the directory as
// used and keeps it.
func EmptyDir(dir string, at string) Workspace {
	return &privateWorkspace{
		engine: workspaces.NewCopies(dir),
		at:     at,
		mode:   workspaces.StorageEmpty,
	}
}

type privateWorkspace struct {
	engine *workspaces.Copies
	src    string
	at     string
	mode   string

	mu    sync.Mutex
	locks map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	users int
}

func (p *privateWorkspace) Attach(ctx context.Context, key string) (Attachment, error) {
	if key == "" {
		return Attachment{}, errors.New("a2aserve: empty workspace key")
	}
	name := dirName(key)
	unlock := p.lock(name)
	defer unlock()
	dir, err := p.engine.Prepare(ctx, name, "", p.mode, p.src)
	if err != nil {
		return Attachment{}, err
	}
	return Attachment{
		Dir:     dir,
		Path:    p.at,
		Release: func() error { return p.engine.Touch(name) },
	}, nil
}

// lock serializes the preparation of one directory, since the engine expects
// a single copy per directory at a time. It forgets the locks nobody holds.
func (p *privateWorkspace) lock(name string) (unlock func()) {
	p.mu.Lock()
	if p.locks == nil {
		p.locks = map[string]*keyLock{}
	}
	l := p.locks[name]
	if l == nil {
		l = &keyLock{}
		p.locks[name] = l
	}
	l.users++
	p.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		p.mu.Lock()
		defer p.mu.Unlock()
		if l.users--; l.users == 0 {
			delete(p.locks, name)
		}
	}
}

func dirName(key string) string {
	sum := sha256.Sum256([]byte(key))
	var b strings.Builder
	for _, r := range key {
		if b.Len() >= 40 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String() + "-" + hex.EncodeToString(sum[:8])
}
