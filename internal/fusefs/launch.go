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

package fusefs

import (
	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/boundary"
)

// LaunchMount is the [boundary.MountFunc] of the launcher: it mounts the
// tree of the file server on m.Socket at m.Dst.
func LaunchMount(m boundary.Mount, uid, gid int) error {
	tree, err := fsys.Client("unix://" + m.Socket)
	if err != nil {
		return err
	}
	return Mount(m.Dst, tree, Options{ReadOnly: m.ReadOnly, UID: uint32(uid), GID: uint32(gid)})
}
