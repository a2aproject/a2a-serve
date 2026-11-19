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

package agentprofile

import (
	"strings"

	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/fsysmask"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
)

// MaskServed returns tree, a workspace that a sandbox shows live, with
// the masks that Prepare lays over a workspace in a host directory:
// .mcp.json with no MCP servers and the configuration files of p empty,
// both read-only, the configuration folders of p private to the sandbox
// and empty, and the agent directories (.a2a) hidden at any depth. Unlike
// a container's mask, a private .opencode folder doesn't show its skills.
func MaskServed(p Profile, tree fsys.FS) fsys.FS {
	files := map[string][]byte{projectfiles.MCPFile: []byte(emptyMCP)}
	var private []string
	entries, _ := configFiles(p)
	for _, entry := range entries {
		if name, isDir := strings.CutSuffix(entry, "/"); isDir {
			private = append(private, name)
		} else {
			files[name] = []byte(emptyConfig)
		}
	}
	return fsysmask.New(tree, fsysmask.Rules{Files: files, Private: private, Hidden: projectfiles.IsAgentEntry})
}
