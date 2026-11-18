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

package boundary

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// EnvRelayKey holds the key that a relay sends to reach its boundary.
const EnvRelayKey = "A2A_SERVE_RELAY_KEY"

// RunInit runs the launcher with the arguments of sandbox-init:
// [--check] <socket> <uid> <gid> [<command> [args...]]. It returns the exit
// code of the command. A failed setup writes its reason to stderr and
// returns [SetupFailedExit].
func RunInit(args, env []string, failSetup bool, stderr io.Writer) (int, error) {
	check := len(args) > 0 && args[0] == "--check"
	if check {
		args = args[1:]
	}
	if len(args) < 3 || !check && len(args) < 4 {
		return 0, fmt.Errorf("want <socket> <uid> <gid> and a command, or --check")
	}
	uid, err := strconv.Atoi(args[1])
	if err != nil {
		return 0, fmt.Errorf("invalid uid %q", args[1])
	}
	gid, err := strconv.Atoi(args[2])
	if err != nil {
		return 0, fmt.Errorf("invalid gid %q", args[2])
	}
	code, err := Launch(LaunchConfig{Boundary: args[0], UID: uid, GID: gid, Argv: args[3:], Env: env, Check: check, FailSetup: failSetup})
	if setupErr, ok := errors.AsType[*SetupError](err); ok {
		if _, err := fmt.Fprintf(stderr, "a2a-init: setting up the sandbox's network: %v\n", setupErr); err != nil {
			return 0, err
		}
		return SetupFailedExit, nil
	}
	if err != nil {
		return 0, fmt.Errorf("starting the sandbox: %w", err)
	}
	return code, nil
}

// RelayFlags are the flags of sandbox-relay.
type RelayFlags struct {
	EgressListen, EgressTo, ServerListen, ServerTo string
}

// RunRelay runs the relay of sandbox-relay with key, the value of
// [EnvRelayKey], until ctx ends.
func RunRelay(ctx context.Context, f RelayFlags, key string, ready io.Writer) error {
	if key == "" {
		return errors.New(EnvRelayKey + " is not set")
	}
	pairs := []RelayPair{{Listen: f.EgressListen, Target: f.EgressTo, Key: key}}
	if f.ServerListen != "" {
		pairs = append(pairs, RelayPair{Listen: f.ServerListen, Target: f.ServerTo})
	}
	return Relay(ctx, pairs, ready)
}
