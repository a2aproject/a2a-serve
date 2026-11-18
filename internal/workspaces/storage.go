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

// Package workspaces keeps the private workspaces of agent containers, and
// names how a container sees its workspace.
package workspaces

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
