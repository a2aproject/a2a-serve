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

package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// The configuration of the agents in their homes which can change what they
// run and approve: settings, and definitions that grant tools or run code.
// opencode also merges the config of the servers that "wellknown" entries
// of its auth.json name. Directories end with a slash.
var (
	claudeHomeConfig   = []string{".claude/settings.json", ".claude/agents/", ".claude/commands/", ".claude/skills/"}
	opencodeHomeConfig = []string{".config/opencode/", ".opencode/", ".local/share/opencode/auth.json"}
)

// opencodeGitignore is what opencode writes into a configuration folder
// that lacks a .gitignore; it exits when it can't.
const opencodeGitignore = "node_modules\npackage.json\npackage-lock.json\nbun.lock\n.gitignore"

// homeConfigReader is a profile with configuration in its home.
type homeConfigReader interface {
	homeConfig() []string
	// resetPermissions puts the server's permission settings back into the
	// config file of home that the server writes.
	resetPermissions(home string) error
}

func (opencode) homeConfig() []string { return opencodeHomeConfig }
func (claude) homeConfig() []string   { return claudeHomeConfig }

// lockHomeConfig returns the mounts that make the agent's configuration in
// its home read-only. It creates the missing entries, since docker would
// otherwise create them as root, and refuses symlinks on the way, which the
// agent could have planted in an earlier container. It resets the
// permission settings that the server writes, which an agent could have
// changed before its home was locked.
//
// The folders between the home and an entry are mounted onto themselves,
// so that the agent can't rename them and put its own configuration in
// their place for the next container or the processes the running one
// starts. The configuration folders of opencode get the .gitignore it
// would write. An agent of unknown kind gets the locks of every kind.
func lockHomeConfig(spec Spec) ([]agentdir.Mount, error) {
	p := spec.Profile
	if c, ok := p.(custom); ok && c.base != nil {
		p = c.base
	}
	readers := []homeConfigReader{claude{}, opencode{}}
	if r, ok := p.(homeConfigReader); ok {
		readers = []homeConfigReader{r}
	}
	var mounts []agentdir.Mount
	add := func(rel string, readOnly bool) {
		dst := filepath.Join(ContainerHome, rel)
		if !slices.ContainsFunc(mounts, func(m agentdir.Mount) bool { return m.Dst == dst }) {
			mounts = append(mounts, agentdir.Mount{Src: filepath.Join(spec.Home, rel), Dst: dst, ReadOnly: readOnly})
		}
	}
	for _, r := range readers {
		for _, entry := range r.homeConfig() {
			name, isDir := strings.CutSuffix(entry, "/")
			if err := prepareConfig(spec.Home, name, isDir); err != nil {
				return nil, err
			}
			if isDir && slices.Contains(opencodeHomeConfig, entry) {
				if err := writeIfMissing(filepath.Join(spec.Home, name, ".gitignore"), []byte(opencodeGitignore)); err != nil {
					return nil, fmt.Errorf("writing the .gitignore of %s: %w", filepath.Join(spec.Home, name), err)
				}
			}
			parts := strings.Split(name, "/")
			for i := 1; i < len(parts); i++ {
				add(filepath.Join(parts[:i]...), false)
			}
			add(name, true)
		}
		if err := r.resetPermissions(spec.Home); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("resetting the permissions in %s: %w", spec.Home, err)
		}
	}
	return mounts, nil
}
