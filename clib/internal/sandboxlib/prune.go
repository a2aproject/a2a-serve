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

package sandboxlib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/sandbox"
)

// OwnerLabel marks the containers and volumes of the library with the
// process that made them, as <host>/<pid namespace>/<uid>/<pid>/<start
// time>, so that a later process removes them once their owner is gone,
// such as after a crash.
const OwnerLabel = "a2a.sandboxlib.owner"

// owner returns the value of [OwnerLabel] of the process pid, or false
// where processes can't be told apart, such as on hosts without /proc.
func owner(pid int) (string, bool) {
	scope, ok := ownerScope()
	if !ok {
		return "", false
	}
	start, err := startTime(pid)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%s/%d/%s", scope, pid, start), true
}

// ownerScope returns the host, the PID namespace and the user of this
// process: only owners in the same scope can be seen to be gone, as the
// processes of other namespaces and, with hidepid, of other users are
// hidden.
func ownerScope() (string, bool) {
	host, err := os.Hostname()
	if err != nil {
		return "", false
	}
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "", false
	}
	ns = strings.TrimSuffix(strings.TrimPrefix(ns, "pid:["), "]")
	return fmt.Sprintf("%s/%s/%d", host, ns, os.Getuid()), true
}

// startTime returns when the process pid started, in clock ticks after
// boot, which tells it from a later process with the same ID.
func startTime(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// The command name in parentheses may hold spaces; the fields after it
	// start with the state, the third field.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return "", fmt.Errorf("reading /proc/%d/stat: no command name", pid)
	}
	fields := strings.Fields(string(data[i+1:]))
	const startField = 22 - 3
	if len(fields) <= startField {
		return "", fmt.Errorf("reading /proc/%d/stat: %d fields", pid, len(fields))
	}
	return fields[startField], nil
}

// orphaned reports whether the owner of value, a value of [OwnerLabel], is
// a process in scope that no longer runs.
func orphaned(value, scope string) bool {
	rest, ok := strings.CutPrefix(value, scope+"/")
	if !ok {
		return false
	}
	pidText, start, ok := strings.Cut(rest, "/")
	if !ok {
		return false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		return false
	}
	got, err := startTime(pid)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return err == nil && got != start
}

// prune removes the containers, then the volumes, of owners that no
// longer run on this host.
func prune(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	scope, ok := ownerScope()
	if !ok {
		return nil
	}
	containers, err := labeled(ctx, scope, "ps", "-a", "--filter", "label="+OwnerLabel, "--format", `{{.ID}} {{.Label "`+OwnerLabel+`"}}`)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range containers {
		errs = append(errs, sandbox.RemoveContainer(ctx, id))
	}
	volumes, err := labeled(ctx, scope, "volume", "ls", "--filter", "label="+OwnerLabel, "--format", `{{.Name}} {{.Label "`+OwnerLabel+`"}}`)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, name := range volumes {
		out, err := exec.CommandContext(ctx, "docker", "volume", "rm", "-f", name).CombinedOutput()
		if err != nil {
			errs = append(errs, fmt.Errorf("docker volume rm %s: %w: %s", name, err, bytes.TrimSpace(out)))
		}
	}
	return errors.Join(errs...)
}

// labeled runs the docker listing args, whose lines are a name and an
// owner, and returns the names of the orphaned owners in scope.
func labeled(ctx context.Context, scope string, args ...string) ([]string, error) {
	out, err := sandbox.DockerQuery(ctx, args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for line := range strings.Lines(string(out)) {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && orphaned(value, scope) {
			names = append(names, name)
		}
	}
	return names, nil
}
