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

// Package workspace handles the workspace an agent works in: how a
// container sees it, the private copies of containers that get their own,
// and what a project brings for its agent, such as the MCP servers of its
// .mcp.json and the skills in its skill folders.
package workspace

import (
	"slices"
	"strings"
)

// Storage is how a container sees the workspace and the other mounts.
type Storage = string

// Storage modes.
const (
	// StorageShared mounts the workspace and mounts as configured.
	StorageShared Storage = "shared"
	// StorageReadOnly mounts the workspace and mounts read-only.
	StorageReadOnly Storage = "read-only"
	// StorageOwn mounts a private copy of the workspace, and the mounts
	// read-only.
	StorageOwn Storage = "own"
	// StorageEmpty mounts an empty private workspace, and no mounts.
	StorageEmpty Storage = "empty"
)

// StorageModes are the storage modes, from the most access to the least.
var StorageModes = []Storage{StorageShared, StorageOwn, StorageReadOnly, StorageEmpty}

// EnvVars returns the values ${VAR} in a project's .mcp.json can take in an
// agent container that gets the NAME=value pairs of env and the NAME=value
// pairs of the secrets it gets. env wins, and a secret with an empty value
// has none.
func EnvVars(env, secrets []string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		for _, kv := range slices.Backward(env) {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				return v, true
			}
		}
		for _, kv := range secrets {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				return v, v != ""
			}
		}
		return "", false
	}
}
