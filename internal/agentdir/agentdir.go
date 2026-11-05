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

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Name is the name of the directory which marks an agent.
const Name = ".a2a"

// ErrNotFound is returned by [Find] when no agent directory is found.
var ErrNotFound error = &notFoundError{msg: "no .a2a directory found here or in any parent directory; start an agent with `a2a-serve start <agent>` or pass --dir"}

// NotFoundIn returns an error, matching [ErrNotFound], for a root without
// an agent.
func NotFoundIn(root string) error {
	return &notFoundError{msg: root + " has no .a2a/config.yaml; start an agent there with `a2a-serve start <agent>`"}
}

type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

func (e *notFoundError) Is(target error) bool { return target == ErrNotFound }

// NoAgent marks the error for the CLI's exit code.
func (e *notFoundError) NoAgent() bool { return true }

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
// the way git finds a repository.
func Find(start string) (Dir, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return Dir{}, err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, Name, configFile)); err == nil {
			return Dir{Root: dir}, nil
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

// Path returns the .a2a directory.
func (d Dir) Path() string { return filepath.Join(d.Root, Name) }

// ConfigPath returns the path of config.yaml.
func (d Dir) ConfigPath() string { return filepath.Join(d.Path(), configFile) }

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

// AnonymousHomeDir is the home of the separate agent process of anonymous
// callers.
func (d Dir) AnonymousHomeDir() string { return filepath.Join(d.StateDir(), "home-anonymous") }

// UserHomesDir holds the homes of the agent processes of authenticated
// users, one per user.
func (d Dir) UserHomesDir() string { return filepath.Join(d.StateDir(), "homes") }

// UserHomeDir is the home of the agent process of the authenticated user
// with the principal ID id.
func (d Dir) UserHomeDir(id string) string {
	return filepath.Join(d.UserHomesDir(), UserKey(id))
}

// RemoveUserHome deletes the home of the agent process of the user with
// the principal ID id, if there is one.
func (d Dir) RemoveUserHome(id string) error {
	return os.RemoveAll(d.UserHomeDir(id))
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

// RecipeLockPath returns the path of the record of the recipe the agent
// was created from.
func (d Dir) RecipeLockPath() string { return filepath.Join(d.Path(), "recipe.lock") }

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
// which the process reaches the server. Like the control socket, it moves
// to the runtime directory when the path is too long for a Unix socket.
func (d Dir) HarnessSocketDir() string {
	p := filepath.Join(d.StateDir(), "harness")
	if len(p)+1+HarnessSocketIDLen+len("/a2a.sock") <= maxSocketPath {
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
	if _, err := os.Stat(filepath.Join(d.Path(), RelayConfigFile)); err == nil {
		return ErrRelayDir
	}
	if err := os.MkdirAll(d.Path(), 0o700); err != nil {
		return err
	}
	for _, p := range []string{d.StateDir(), d.HomeDir()} {
		if err := fsutil.MkdirReal(p); err != nil {
			return err
		}
	}
	gi := filepath.Join(d.Path(), ".gitignore")
	if _, err := os.Stat(gi); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(gi, []byte("state/\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Exists reports whether the agent has a config.yaml.
func (d Dir) Exists() bool {
	_, err := os.Stat(d.ConfigPath())
	return err == nil
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
