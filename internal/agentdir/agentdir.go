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

// Package agentdir manages the .a2a directory of an agent server: the agent
// definition in config.yaml, which can be committed, and the state directory
// with the database, secrets, network identity and harness home, which never is.
package agentdir

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Name is the name of the directory which marks an agent.
const Name = ".a2a"

// ErrNotFound is returned by [Find] when no agent directory is found.
var ErrNotFound error = clierr.NoAgent(&notFoundError{msg: "no .a2a directory found here or in any parent directory; start an agent with `a2a-serve start <agent>` or pass --dir"})

// NotFoundIn returns an error, matching [ErrNotFound], for a root without
// an agent.
func NotFoundIn(root string) error {
	d := Dir{Root: root}
	if d.HasRelay() {
		return relayNotAgent(root)
	}
	if d.HasServerState() {
		return missingConfig(d)
	}
	return clierr.NoAgent(&notFoundError{msg: root + " has no .a2a/config.yaml; start an agent there with `a2a-serve start <agent>`"})
}

// relayNotAgent returns an error, matching [ErrNotFound], for a root that
// holds a relay.
func relayNotAgent(root string) error {
	return clierr.NoAgent(&notFoundError{msg: root + " has a relay (.a2a/relay.yaml) and no agent; manage the relay with `a2a-serve relay` commands, or pass --dir with an agent directory"})
}

// missingConfig returns an error, matching [ErrNotFound], for an agent
// whose config.yaml is gone but whose server left its state.
func missingConfig(d Dir) error {
	return clierr.NoAgent(&notFoundError{msg: d.ConfigPath() + " is missing; create it again with `a2a-serve start <agent>` in " + d.Root})
}

type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

func (e *notFoundError) Is(target error) bool { return target == ErrNotFound }

// Dir is the .a2a directory of one agent.
type Dir struct {
	// Root is the directory which holds .a2a, the default workspace.
	Root string
}

// Open returns the agent directory rooted at root without checking that it exists.
func Open(root string) (Dir, error) {
	root, err := expandHome(root)
	if err != nil {
		return Dir{}, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Dir{}, err
	}
	return Dir{Root: abs}, nil
}

// Find walks up from start to the first directory holding .a2a/config.yaml,
// the way git finds a repository. The walk stops at a relay directory.
func Find(start string) (Dir, error) {
	return find(start, Dir.Exists, true)
}

// FindEnclosing is [Find] that walks past the agents whose config.yaml is
// gone, for the checks that refuse to create an agent inside another one.
func FindEnclosing(start string) (Dir, error) {
	return find(start, Dir.Exists, false)
}

// FindWithState is [Find] that also stops at an agent whose config.yaml is
// gone but whose server left its state, so that the server can still be
// found and stopped.
func FindWithState(start string) (Dir, error) {
	return find(start, func(d Dir) bool { return d.Exists() || d.HasServerState() }, true)
}

// find walks up from start to the first directory that match accepts. With
// stopAtStale, it stops at an agent whose config.yaml is gone.
func find(start string, match func(Dir) bool, stopAtStale bool) (Dir, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return Dir{}, err
	}
	for {
		d := Dir{Root: dir}
		if match(d) {
			return d, nil
		}
		if d.HasRelay() {
			return Dir{}, relayNotAgent(dir)
		}
		// An outer agent isn't the one meant here.
		if stopAtStale && d.HasServerState() {
			return Dir{}, missingConfig(d)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Dir{}, ErrNotFound
		}
		dir = parent
	}
}

const configFile = "config.yaml"

// RelayConfigFile is the file in .a2a which marks a relay directory. A
// directory holds an agent or a relay, so relay secrets stay out of agent
// backups.
const RelayConfigFile = "relay.yaml"

// ErrRelayDir is returned when an agent would be created in a relay directory.
var ErrRelayDir = agenterr.Invalid(errors.New("this directory has a relay (.a2a/relay.yaml); start the agent in another directory"))

// HasRelay reports whether the directory holds a relay.
func (d Dir) HasRelay() bool {
	_, err := os.Stat(filepath.Join(d.Path(), RelayConfigFile))
	return err == nil
}

// Path returns the .a2a directory.
func (d Dir) Path() string { return filepath.Join(d.Root, Name) }

// ConfigPath returns the path of config.yaml.
func (d Dir) ConfigPath() string { return filepath.Join(d.Path(), configFile) }

// ConfigDraftPath returns the path of the last edit of config.yaml that
// `config edit` rejected.
func (d Dir) ConfigDraftPath() string { return filepath.Join(d.StateDir(), "config-draft.yaml") }

// ConfigDraftBasePath returns the path of the content of config.yaml that
// the draft at [Dir.ConfigDraftPath] started from.
func (d Dir) ConfigDraftBasePath() string {
	return filepath.Join(d.StateDir(), "config-draft-base.yaml")
}

// StateDir returns the directory which is never committed.
func (d Dir) StateDir() string { return filepath.Join(d.Path(), "state") }

// DBPath returns the path of the SQLite database.
func (d Dir) DBPath() string { return filepath.Join(d.StateDir(), "server.db") }

// SecretsPath returns the path of the secrets file.
func (d Dir) SecretsPath() string { return filepath.Join(d.StateDir(), "secrets") }

// TSNetDir returns the directory of the Tailscale node identity.
func (d Dir) TSNetDir() string { return filepath.Join(d.StateDir(), "tsnet") }

// AgentMeshDir returns the directory of the agent's identity on an Agent
// Mesh relay.
func (d Dir) AgentMeshDir() string { return filepath.Join(d.StateDir(), "agentmesh") }

// HomeDir returns the harness home, mounted at /home/agent in a container.
func (d Dir) HomeDir() string { return filepath.Join(d.StateDir(), "home") }

// GuestHomeDir is the home of the separate agent process of guests. It
// keeps the name it had before guests were called guests.
func (d Dir) GuestHomeDir() string { return filepath.Join(d.StateDir(), "home-anonymous") }

// UserHomesDir holds the homes of the agent processes of members, one per
// member.
func (d Dir) UserHomesDir() string { return filepath.Join(d.StateDir(), "homes") }

// UserHomeDir is the home of the agent process of the member with the
// principal ID id.
func (d Dir) UserHomeDir(id string) string {
	return filepath.Join(d.UserHomesDir(), UserKey(id))
}

// WorkspacesDir holds the private workspaces of the agent containers whose
// sandbox gives them their own storage, one directory each: owner,
// anonymous for guests, or the [UserKey] of a member. Backups leave it out.
func (d Dir) WorkspacesDir() string { return filepath.Join(d.StateDir(), "workspaces") }

// UserWorkspacesDir holds the private workspaces of the agent container of
// the user with the principal ID id.
func (d Dir) UserWorkspacesDir(id string) string {
	return filepath.Join(d.WorkspacesDir(), UserKey(id))
}

// RemoveUserHome deletes the home and the private workspaces of the agent
// process of the user with the principal ID id, if there are any.
func (d Dir) RemoveUserHome(id string) error {
	return errors.Join(os.RemoveAll(d.UserHomeDir(id)), os.RemoveAll(d.UserWorkspacesDir(id)))
}

// UserKey is a short stable name of the principal ID id, for paths and
// container names.
func UserKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:16]
}

// LogPath returns the path of the server log.
func (d Dir) LogPath() string { return filepath.Join(d.StateDir(), "server.log") }

// MCPMaskPath is the file with no MCP servers which a sandbox mounts over
// the workspace's .mcp.json.
func (d Dir) MCPMaskPath() string { return filepath.Join(d.StateDir(), "mcp-mask.json") }

// ConfigMaskPath is the empty configuration file which a sandbox mounts
// over the agent's own configuration files in the workspace.
func (d Dir) ConfigMaskPath() string { return filepath.Join(d.StateDir(), "config-mask.json") }

// RecipeLockPath returns the path of the record of the recipe the agent
// was created from.
func (d Dir) RecipeLockPath() string { return filepath.Join(d.Path(), RecipeLockFile) }

// RecipeLockFile is the name of the recipe lock in the agent directory.
const RecipeLockFile = "recipe.lock"

// LockPath returns the path of the file the running server holds locked.
func (d Dir) LockPath() string { return filepath.Join(d.StateDir(), "server.lock") }

// PIDPath returns the path of the file holding the PID of the running server.
func (d Dir) PIDPath() string { return filepath.Join(d.StateDir(), "server.pid") }

// Lock takes the lock the running server holds, and returns the function
// that releases it. It returns [fsutil.ErrLocked] while another process
// holds it. It removes the PID file a crashed server left behind, so that a
// holder other than the server isn't taken for a starting server.
func (d Dir) Lock() (release func() error, err error) {
	release, err = fsutil.TryLock(d.LockPath())
	if err != nil {
		return nil, err
	}
	if err := os.Remove(d.PIDPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(fmt.Errorf("removing a stale PID file: %w", err), release())
	}
	return release, nil
}

// ProbeWait is how long [Dir.LockWithin] waits out a command that holds the
// lock to check that no server runs.
const ProbeWait = 2 * time.Second

// LockWithin is [Dir.Lock] retried for up to wait while a command holds
// the lock to check that no server runs, so that such a check doesn't make
// a starting server fail. A server writes its PID file once it holds the
// lock, and a lock held with a PID file fails at once.
func (d Dir) LockWithin(wait time.Duration) (release func() error, err error) {
	deadline := time.Now().Add(wait)
	for {
		release, err = d.Lock()
		if !errors.Is(err, fsutil.ErrLocked) || time.Now().After(deadline) {
			return release, err
		}
		if _, statErr := os.Stat(d.PIDPath()); statErr == nil {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// maxSocketPath stays under the sun_path limit of Linux (108) and macOS (104).
const maxSocketPath = 100

// SocketPath returns the path of the control socket. It is state/server.sock
// unless that path is too long for a Unix socket, in which case it is a path
// in the runtime directory derived from the agent directory.
func (d Dir) SocketPath() string {
	return SocketPath(filepath.Join(d.StateDir(), "server.sock"), "a2a-"+d.ID()+".sock")
}

// SocketPath returns the socket path p, or the file name in the runtime
// directory when p is too long for a Unix socket.
func SocketPath(p, name string) string {
	if len(p) <= maxSocketPath {
		return p
	}
	return filepath.Join(runtimeDir(), name)
}

func runtimeDir() string {
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		return base
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("a2a-%d", os.Getuid()))
}

// HarnessSocketIDLen is the length of the names of the directories in
// [Dir.HarnessSocketDir].
const HarnessSocketIDLen = 12

// HarnessSocketDir returns the directory that holds a directory per agent
// process, of [HarnessSocketIDLen] characters, with the a2a.sock through
// which the process reaches the server, and one per container with its
// egress.sock. Like the control socket, it moves
// to the runtime directory when the path is too long for a Unix socket.
func (d Dir) HarnessSocketDir() string {
	p := filepath.Join(d.StateDir(), "harness")
	// egress.sock is the longest socket name in it.
	if len(p)+1+HarnessSocketIDLen+len("/egress.sock") <= maxSocketPath {
		return p
	}
	return filepath.Join(runtimeDir(), "a2a-"+d.ID()+"-harness")
}

// ID is a short stable identifier of the agent directory, used in container
// and service names.
func (d Dir) ID() string {
	sum := sha256.Sum256([]byte(d.Root))
	return hex.EncodeToString(sum[:])[:10]
}

// ServiceName is the name of the agent's background service.
func (d Dir) ServiceName() string { return "a2a-" + d.ID() }

// Init creates the .a2a and state directories and the .gitignore which keeps
// state out of version control. The state directory and the harness home
// must be real directories, since a cloned repository could ship them as
// symlinks.
func (d Dir) Init() error {
	if d.HasRelay() {
		return ErrRelayDir
	}
	if err := d.mkdirState(); err != nil {
		return err
	}
	if err := fsutil.MkdirReal(d.HomeDir()); err != nil {
		return err
	}
	return WriteGitignore(d.Path())
}

// mkdirState creates the .a2a and state directories, and refuses a state
// directory that another user owns or that others can open. Call it before
// the first write to the state directory.
func (d Dir) mkdirState() error {
	if err := os.MkdirAll(d.Path(), 0o700); err != nil {
		return err
	}
	return fsutil.MkdirOwned(d.StateDir())
}

// WriteGitignore writes the .gitignore which keeps state out of version
// control into dir, an .a2a directory, unless dir has one. It leaves a
// symlink in its place alone, even one to a missing file.
func WriteGitignore(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, ".gitignore"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString("state/\n"); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// Exists reports whether the agent has a config.yaml.
func (d Dir) Exists() bool {
	_, err := os.Stat(d.ConfigPath())
	return err == nil
}

// HasServerState reports whether a server ran in d, which leaves the file
// of its lock behind.
func (d Dir) HasServerState() bool {
	_, err := os.Stat(d.LockPath())
	return err == nil
}

// NameOrDefault returns the name in config.yaml, or [Dir.DefaultName] if
// config.yaml has none or doesn't parse.
func (d Dir) NameOrDefault() string {
	if cfg, err := d.ReadConfig(); err == nil && cfg.Name != "" {
		return cfg.Name
	}
	return d.DefaultName()
}

// DefaultName derives an agent name from the root directory name.
func (d Dir) DefaultName() string {
	return SanitizeName(filepath.Base(d.Root))
}

// SanitizeName turns s into a lowercase DNS label, usable as a tailnet
// hostname and in container names.
func SanitizeName(s string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			dash = false
		case !dash && sb.Len() > 0:
			sb.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimSuffix(sb.String(), "-")
	if len(out) > 63 {
		out = strings.TrimSuffix(out[:63], "-")
	}
	if out == "" {
		return "agent"
	}
	return out
}
