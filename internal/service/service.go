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

// Package service runs an agent server in the background: detached from the
// terminal, or as a launchd or systemd user service that starts at login.
package service

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Unit describes a background agent server process.
type Unit struct {
	// Name identifies the service, such as a2a-coder-1a2b3c.
	Name        string
	Description string
	Exe         string
	Args        []string
	Dir         string
	LogPath     string
	// Env holds NAME=value pairs, such as PATH so that docker is found.
	Env []string
}

// ErrUnsupported is returned on platforms without a supported service manager.
var ErrUnsupported = errors.New("--service is supported on Linux (systemd) and macOS (launchd); use --detach instead")

// Detach starts the unit's process in its own session, with output appended
// to the log, and returns its PID. The returned channel is closed when the
// process exits.
func Detach(u Unit) (pid int, exited <-chan struct{}, err error) {
	if err := os.MkdirAll(filepath.Dir(u.LogPath), 0o700); err != nil {
		return 0, nil, err
	}
	log, err := fsutil.OpenNoFollow(u.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, nil, err
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, nil, errors.Join(err, log.Close())
	}
	cmd := exec.Command(u.Exe, u.Args...)
	cmd.Dir = u.Dir
	cmd.Env = append(os.Environ(), u.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, log, log
	setsid(cmd)
	startErr := cmd.Start()
	closeErr := errors.Join(log.Close(), devnull.Close())
	if startErr != nil {
		return 0, nil, startErr
	}
	// Waiting reaps the process if it exits while this one runs, so a crash
	// isn't hidden by a zombie that still looks alive.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := cmd.Wait(); err != nil {
			slog.Debug("detached process exited", "error", err)
		}
	}()
	return cmd.Process.Pid, done, closeErr
}

// System is the service manager of this machine: systemd on Linux and
// launchd on macOS.
type System struct{}

// Installed reports whether the service name exists.
func (System) Installed(name string) bool { return Installed(name) }

// MainPID returns the PID of the process that the service name runs, or
// zero if it doesn't run.
func (System) MainPID(name string) (int, error) { return MainPID(name) }

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// output runs a command and returns its standard output.
func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var stderr string
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, stderr)
	}
	return string(out), nil
}
