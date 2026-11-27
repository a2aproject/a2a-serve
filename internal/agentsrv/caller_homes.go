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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// maxCallerHomes limits the agent homes that a member that is another
// agent gets for the people behind its calls.
const maxCallerHomes = 32

var errCallerHomes = fmt.Errorf("the agent homes of all %d people behind the calling agent are in use, send the message again later", maxCallerHomes)

// checkCallerHomes notes a new task of a person behind another agent in
// their agent home. Once that agent has maxCallerHomes homes for its
// callers, a new person takes the place of the one whose last task is the
// oldest and whose process doesn't run: that person's home and private
// workspaces go, and their conversations continue in new agent sessions.
func (i *callerInterceptor) checkCallerHomes(key string) error {
	caller := agentkey.CallerOf(key)
	if caller == "" {
		return nil
	}
	id, _ := agentkey.UserOf(key)
	now := time.Now()
	err := os.Chtimes(i.dir.CallerHomeDir(id, caller), now, now)
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	homes, err := i.dir.CallerHomeDirs(id)
	if err != nil {
		return fmt.Errorf("listing the agent homes of %s: %w", id, err)
	}
	if len(homes) < maxCallerHomes {
		return nil
	}
	return i.evictCallerHome(id, homes)
}

// evictCallerHome removes the least recently used of homes, the agent
// homes that the member id has for its callers, whose process doesn't run.
func (i *callerInterceptor) evictCallerHome(id string, homes []string) error {
	used := make(map[string]time.Time, len(homes))
	for _, home := range homes {
		info, err := os.Stat(home)
		if err != nil {
			return err
		}
		used[home] = info.ModTime()
	}
	slices.SortFunc(homes, func(a, b string) int { return used[a].Compare(used[b]) })
	prefix := agentdir.CallerName(id, "")
	for _, home := range homes {
		caller := strings.TrimPrefix(filepath.Base(home), prefix)
		key := agentkey.CallerKey(id, caller)
		release, ok, err := i.engine().HoldIdle(context.Background(), key)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		i.removeHome(home)
		err = i.dir.RemoveCallerHome(id, caller)
		i.engine().Forget(key)
		release()
		return err
	}
	return fmt.Errorf("%w: %w", a2a.ErrServerError, errCallerHomes)
}

// Member homes are named by the principal's hash, optionally followed by
// the hash of a caller behind that principal.
var memberHomeName = regexp.MustCompile(`^[0-9a-f]{16}(-[0-9a-f]{16})?$`)

func (s *Server) pruneMemberHomes(ctx context.Context) error {
	retired := filepath.Join(s.dir.StateDir(), "pruned-homes")
	if err := fsutil.RemoveAll(retired); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.dir.UserHomesDir())
	if errors.Is(err, fs.ErrNotExist) || err == nil && len(entries) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	err = s.stageUnusedMemberHomes(ctx, entries, retired)
	return errors.Join(err, fsutil.RemoveAll(retired))
}

func (s *Server) stageUnusedMemberHomes(ctx context.Context, entries []os.DirEntry, retired string) error {
	active, release := s.engine.HoldPoolStarts(usersPool)
	if release == nil {
		return nil
	}
	defer release()
	keys, owners, err := s.store.HomeReferences(ctx)
	if err != nil {
		return err
	}
	pairings, err := s.store.Pairings(ctx)
	if err != nil {
		return err
	}
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return err
	}

	keepHomes := make(map[string]bool, len(keys)+len(active))
	for _, key := range append(keys, active...) {
		if _, ok := agentkey.UserOf(key); ok {
			keepHomes[userProcessName(key)] = true
		}
	}
	keepOwners := make(map[string]bool, len(owners)+len(pairings)+len(nodes))
	for _, owner := range owners {
		keepOwners[agentdir.UserKey(owner)] = true
	}
	for _, p := range pairings {
		keepOwners[agentdir.UserKey(auth.PairPrincipal(p.Name))] = true
	}
	for _, n := range nodes {
		keepOwners[agentdir.UserKey(auth.NodePrincipal(n.ID))] = true
	}

	var errs []error
	staged := false
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		name := e.Name()
		owner, _, _ := strings.Cut(name, "-")
		if !e.IsDir() || !memberHomeName.MatchString(name) || keepHomes[name] || keepOwners[owner] {
			continue
		}
		if !staged {
			if err := os.Mkdir(retired, 0o700); err != nil {
				return err
			}
			staged = true
		}
		home := filepath.Join(s.dir.UserHomesDir(), name)
		// Move the home while starts and instruction rewrites are held.
		// Removing its contents can take longer and runs after release.
		s.instructionsMu.Lock()
		err := os.Rename(home, filepath.Join(retired, name))
		s.instructionsMu.Unlock()
		switch {
		case err == nil:
			s.forgetHome(home)
		case !errors.Is(err, fs.ErrNotExist):
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
