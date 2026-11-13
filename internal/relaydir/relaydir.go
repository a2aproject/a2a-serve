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

// Package relaydir manages the .a2a directory of a relay: its settings in
// relay.yaml, which can be committed, and its state in state/relay, which
// never is.
package relaydir

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// ErrNotFound is returned by [Find] when no relay directory is found. It
// exits like a missing agent.
var ErrNotFound error = clierr.NoAgent(&notFoundError{msg: "no relay here or in any parent directory; run one with `a2a-serve relay run --url <url>`, or pass --dir or --url"})

// NotFoundIn returns an error, matching [ErrNotFound], for a root without a
// relay.
func NotFoundIn(root string) error {
	return clierr.NoAgent(&notFoundError{msg: root + " has no .a2a/relay.yaml; run a relay there with `a2a-serve relay run --url <url>`"})
}

type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

func (e *notFoundError) Is(target error) bool { return target == ErrNotFound }

// ErrAgentDir is returned when a relay would be created in an agent directory.
var ErrAgentDir = agenterr.Invalid(errors.New("this directory has an agent (.a2a/config.yaml); run the relay in another directory"))

// ErrRunning is returned when a command that writes the relay directory
// finds the relay running.
var ErrRunning = errors.New("the relay is running")

// Dir is the .a2a directory of one relay.
type Dir struct {
	// Root is the directory which holds .a2a.
	Root string
}

// Open returns the relay directory rooted at root without checking that it
// exists.
func Open(root string) (Dir, error) {
	if rest, ok := strings.CutPrefix(root, "~"); ok && (rest == "" || rest[0] == '/') {
		home, err := os.UserHomeDir()
		if err != nil {
			return Dir{}, err
		}
		root = home + rest
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Dir{}, err
	}
	return Dir{Root: abs}, nil
}

// Find walks up from start to the first directory holding .a2a/relay.yaml.
func Find(start string) (Dir, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return Dir{}, err
	}
	for {
		if d := (Dir{Root: dir}); d.Exists() {
			return d, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Dir{}, ErrNotFound
		}
		dir = parent
	}
}

// Path returns the .a2a directory.
func (d Dir) Path() string { return filepath.Join(d.Root, agentdir.Name) }

// ConfigPath returns the path of relay.yaml.
func (d Dir) ConfigPath() string { return filepath.Join(d.Path(), agentdir.RelayConfigFile) }

// StateDir returns the directory of the relay's state.
func (d Dir) StateDir() string { return filepath.Join(d.Path(), "state", "relay") }

// SecretsPath returns the path of the secrets file.
func (d Dir) SecretsPath() string { return filepath.Join(d.StateDir(), "secrets") }

// MeshDir returns the data directory of agentmesh-one.
func (d Dir) MeshDir() string { return filepath.Join(d.StateDir(), "agentmesh") }

// MeshDBPath returns the SQLite database of agentmesh-one.
func (d Dir) MeshDBPath() string { return filepath.Join(d.MeshDir(), "agentmesh.db") }

// SlimDir returns the data directory of a SLIM relay: its node's config and
// its members.
func (d Dir) SlimDir() string { return filepath.Join(d.StateDir(), "slim") }

// RemovedAgentsPath is where the relay remembers the agents it removed.
func (d Dir) RemovedAgentsPath() string { return filepath.Join(d.StateDir(), "removed-agents.json") }

// InviteNamesPath is where the relay remembers the invites its agents
// joined with.
func (d Dir) InviteNamesPath() string { return filepath.Join(d.StateDir(), "invite-names.json") }

// VersionPath returns the file which records the Agent Mesh version that
// last ran on the data.
func (d Dir) VersionPath() string { return filepath.Join(d.StateDir(), "agentmesh-version") }

// BackupsDir returns the directory of the backups taken before upgrades.
func (d Dir) BackupsDir() string { return filepath.Join(d.StateDir(), "backups") }

// LockPath returns the file the running relay holds locked.
func (d Dir) LockPath() string { return filepath.Join(d.StateDir(), "relay.lock") }

// LogPath returns the log of a relay that runs as a service.
func (d Dir) LogPath() string { return filepath.Join(d.StateDir(), "relay.log") }

// SocketPath returns the control socket of the running relay.
func (d Dir) SocketPath() string {
	return agentdir.SocketPath(filepath.Join(d.StateDir(), "relay.sock"), "a2a-relay-"+d.ID()+".sock")
}

// ID is a short stable identifier of the relay directory.
func (d Dir) ID() string {
	sum := sha256.Sum256([]byte(d.Root))
	return hex.EncodeToString(sum[:])[:10]
}

// ServiceName is the name of the relay's background service.
func (d Dir) ServiceName() string { return "a2a-relay-" + d.ID() }

// Exists reports whether the relay has a relay.yaml.
func (d Dir) Exists() bool {
	_, err := os.Stat(d.ConfigPath())
	return err == nil
}

// mkdirState creates the .a2a and state directories, closed to others. A
// cloned repository could ship the state directories as symlinks.
func (d Dir) mkdirState() error {
	if err := os.MkdirAll(d.Path(), 0o700); err != nil {
		return err
	}
	for _, p := range []string{filepath.Dir(d.StateDir()), d.StateDir()} {
		if err := fsutil.MkdirReal(p); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// lockConfig runs fn holding the advisory lock of relay.yaml, kept in the
// state directory, which is never committed.
func (d Dir) lockConfig(fn func() error) error {
	if err := d.mkdirState(); err != nil {
		return err
	}
	return fsutil.WithLock(filepath.Join(d.StateDir(), agentdir.RelayConfigFile), fn)
}

// Init creates the .a2a and state directories, closed to others, and the
// .gitignore which keeps state out of version control. It refuses an agent
// directory.
func (d Dir) Init() error {
	if (agentdir.Dir{Root: d.Root}).Exists() {
		return ErrAgentDir
	}
	if err := d.mkdirState(); err != nil {
		return err
	}
	return agentdir.WriteGitignore(d.Path())
}

// Lock takes the lock the running relay holds, and returns the function
// that releases it. It returns [ErrRunning] while the relay runs.
func (d Dir) Lock() (release func() error, err error) {
	release, err = fsutil.TryLock(d.LockPath())
	if errors.Is(err, fsutil.ErrLocked) {
		return nil, fmt.Errorf("%w in %s", ErrRunning, d.Root)
	}
	return release, err
}

// LoadSecrets reads the relay's secrets.
func (d Dir) LoadSecrets() (agentdir.Secrets, error) {
	return agentdir.LoadSecretsFile(d.SecretsPath())
}

// SaveSecrets merges s into the relay's secrets.
func (d Dir) SaveSecrets(s agentdir.Secrets) error {
	return agentdir.SaveSecretsFile(d.SecretsPath(), s)
}

// RecordedVersion returns the Agent Mesh version that last ran on the data,
// or "" before the first run.
func (d Dir) RecordedVersion() (string, error) {
	data, err := os.ReadFile(d.VersionPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// RecordVersion records v as the Agent Mesh version that runs on the data.
func (d Dir) RecordVersion(v string) error {
	return fsutil.WriteAtomic(d.VersionPath(), []byte(v+"\n"), 0o600)
}
