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

// Package fusefs shows an fsys tree as a FUSE file system, which the
// launcher of a container mounts for a live workspace.
//
// Files are written whole, with close-to-open semantics: the writes to an
// open file go to a buffer, which replaces the file in the tree when the
// file is flushed, synced or closed. Every close of a descriptor of the
// file flushes it, also of a copy that a child process closes when it
// execs. The last write wins. Reads of a file
// that isn't being written go to the tree, so a file opened after the
// tree changed it shows the change. The kernel's interrupts don't cancel
// the operations, which programs that their runtime signals, such as Go
// programs, would see fail with EINTR.
package fusefs

// Options configure a mount.
type Options struct {
	ReadOnly bool
	// UID and GID own every entry, so that the agent, which runs as them,
	// owns its workspace.
	UID, GID uint32
	// MaxFileBytes bounds a file being written, and MaxOpenBytes all of
	// them together. Zero takes the default: fsys.MaxFileSize and 2 GiB.
	MaxFileBytes, MaxOpenBytes int64
	// TempDir holds the buffers of the files being written, unlinked; the
	// system's if empty. Where it can't, they are kept in memory.
	TempDir string
}
