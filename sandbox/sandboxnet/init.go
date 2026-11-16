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

package sandboxnet

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// InitUsage is the usage of [RunInit].
const InitUsage = "[--check | --hold | --exec] <socket> <uid> <gid> [<command> [args...]]"

// RunInit runs the launcher with the arguments of the container's
// entrypoint after sandbox-init, and returns its exit code:
//
//   - <socket> <uid> <gid> <command>... runs the command as PID 1 does.
//   - --check sets up the network and drops the capabilities, then exits.
//   - --hold sets up nothing and waits for a signal, to keep the volumes
//     of the container mounted while its command restarts.
//   - --exec drops the capabilities and replaces itself with the command,
//     for commands that docker exec runs next to the agent; <socket> is
//     ignored.
func RunInit(args []string, stderr io.Writer) int {
	mode := ""
	if len(args) > 0 && (args[0] == "--check" || args[0] == "--hold" || args[0] == "--exec") {
		mode, args = args[0], args[1:]
	}
	if mode == "--hold" {
		return Hold()
	}
	if len(args) < 3 || mode != "--check" && len(args) < 4 {
		return fail(stderr, 2, "a2a-init: want "+InitUsage)
	}
	uid, uerr := strconv.Atoi(args[1])
	gid, gerr := strconv.Atoi(args[2])
	if uerr != nil || gerr != nil {
		return fail(stderr, 2, "a2a-init: invalid uid or gid")
	}
	if mode == "--exec" {
		env, timeout := execEnv(os.Environ())
		code, err := Exec(uid, gid, args[3:], env, timeout)
		if err != nil {
			return fail(stderr, cmp.Or(code, 126), "a2a-init: "+err.Error())
		}
		return code
	}
	code, err := Launch(LaunchConfig{
		Boundary: args[0], UID: uid, GID: gid, Argv: args[3:], Env: os.Environ(), Check: mode == "--check",
		FailSetup: os.Getenv(EnvFailSetup) != "",
	})
	if setupErr, ok := errors.AsType[*SetupError](err); ok {
		return fail(stderr, SetupFailedExit, fmt.Sprintf("a2a-init: setting up the sandbox's network: %v", setupErr))
	}
	if err != nil {
		return fail(stderr, 1, "a2a-init: starting the sandbox: "+err.Error())
	}
	return code
}

// EnvExecTimeout gives the commands of --exec a timeout in seconds.
const EnvExecTimeout = "A2A_EXEC_TIMEOUT"

// ExecTimedOutExit is the exit code of --exec when the command ran out of
// time, as with timeout(1).
const ExecTimedOutExit = 124

// execEnv returns the environment of a command of --exec, and its timeout.
// The command trusts the egress CA as the agent does, through the files
// that the launcher of the agent wrote.
func execEnv(env []string) ([]string, time.Duration) {
	var timeout time.Duration
	trusted := false
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		if v, ok := strings.CutPrefix(kv, EnvExecTimeout+"="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				timeout = time.Duration(n) * time.Second
			}
			return true
		}
		if strings.HasPrefix(kv, EnvEgressCA+"=") {
			trusted = true
			return true
		}
		return false
	})
	if trusted {
		if _, err := os.Stat(ContainerCABundle); err == nil {
			env = setEnv(setEnv(setEnv(env, "SSL_CERT_FILE", ContainerCABundle), "REQUESTS_CA_BUNDLE", ContainerCABundle), "NODE_EXTRA_CA_CERTS", ContainerCAFile)
		}
	}
	return env, timeout
}

// fail writes msg to w and returns code. When the write fails, the exit
// code is all that is left to tell what happened.
func fail(w io.Writer, code int, msg string) int {
	if _, err := io.WriteString(w, msg+"\n"); err != nil {
		return code
	}
	return code
}
