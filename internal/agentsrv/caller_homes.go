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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
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
		release, err := i.users.stopIdle(key)
		if err != nil {
			return err
		}
		if release == nil {
			continue
		}
		i.removeHome(home)
		err = i.dir.RemoveCallerHome(id, caller)
		i.users.forget(key)
		release()
		return err
	}
	return fmt.Errorf("%w: %w", a2a.ErrServerError, errCallerHomes)
}
