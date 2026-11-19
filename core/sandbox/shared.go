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

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

// shared is what the sandboxes of a runtime share while any of them is
// being created or not yet closed: the state directory, the egress
// boundaries, which keep their sockets in its egress directory, and the
// backend that newBackend makes for them.
type shared[B any] struct {
	// stateDir is the state directory of the options, or "" for a
	// temporary one, and launcher the launcher of the options, or "" for
	// a copy of this binary.
	stateDir, launcher string
	newBackend         func(dir string, launcher func() string) B

	mu         sync.Mutex
	dir        string
	backend    B
	boundaries *sandboxrt.Boundaries
	users      int
}

// acquire returns the backend and the boundaries, setting them up for the
// first user. The caller calls release when done.
func (s *shared[B]) acquire() (B, *sandboxrt.Boundaries, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.boundaries == nil {
		dir := s.stateDir
		if dir == "" {
			tmp, err := os.MkdirTemp("", "a2a-sandbox-")
			if err != nil {
				var zero B
				return zero, nil, fmt.Errorf("creating the directory of the sandboxes' files: %w", err)
			}
			dir = tmp
		}
		s.dir = dir
		s.boundaries = sandboxrt.NewBoundaries(filepath.Join(dir, "egress"), sandboxrt.SocketIDLen, cryptoutil.Unambiguous, nil)
		launcher := s.boundaries.Launcher
		if s.launcher != "" {
			launcher = func() string { return s.launcher }
		}
		s.backend = s.newBackend(dir, launcher)
	}
	s.users++
	return s.backend, s.boundaries, nil
}

// release ends a use of acquire. The last one closes the listener of the
// egress containers and removes a temporary state directory.
func (s *shared[B]) release(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users--
	if s.users > 0 {
		return nil
	}
	err := s.boundaries.Close(ctx)
	if s.stateDir == "" {
		err = errors.Join(err, os.RemoveAll(s.dir))
	}
	var zero B
	s.backend, s.boundaries, s.dir = zero, nil, ""
	return err
}

// releasing closes a sandbox of a runtime, and then releases its use of
// the shared state once.
type releasing struct {
	sandboxrt.Sandbox
	release func(context.Context) error
	once    sync.Once
}

func (s *releasing) Close(ctx context.Context) error {
	err := s.Sandbox.Close(ctx)
	s.once.Do(func() { err = errors.Join(err, s.release(ctx)) })
	return err
}
