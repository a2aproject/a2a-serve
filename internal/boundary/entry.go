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
	"path"
	"strconv"
	"strings"
)

// EnvRelayKey holds the key that a relay sends to reach its boundary.
const EnvRelayKey = "A2A_SERVE_RELAY_KEY"

// RunInit runs the launcher with the arguments of sandbox-init:
// [--check] [--fs <dst>=<socket>[,ro]]... <socket> <uid> <gid> [<command>
// [args...]]. Each --fs is a live workspace that mount mounts. It returns
// the exit code of the command. A failed setup writes its reason to stderr
// and returns [SetupFailedExit], a failed mount [MountFailedExit], and a
// tree that a mount can't read [TreeFailedExit].
func RunInit(args, env []string, failSetup bool, mount MountFunc, stderr io.Writer) (int, error) {
	check := len(args) > 0 && args[0] == "--check"
	if check {
		args = args[1:]
	}
	var mounts []Mount
	for len(args) > 1 && args[0] == "--fs" {
		m, err := parseMount(args[1])
		if err != nil {
			return 0, err
		}
		mounts = append(mounts, m)
		args = args[2:]
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
	code, err := Launch(LaunchConfig{Boundary: args[0], UID: uid, GID: gid, Argv: args[3:], Env: env, Mounts: mounts, Mount: mount, Check: check, FailSetup: failSetup})
	if mountErr, ok := errors.AsType[*MountError](err); ok {
		if _, err := fmt.Fprintf(stderr, "%s%v\n", MountErrorPrefix, mountErr); err != nil {
			return 0, err
		}
		return MountFailedExit, nil
	}
	if treeErr, ok := errors.AsType[*TreeError](err); ok {
		if _, err := fmt.Fprintf(stderr, "%s%v\n", TreeErrorPrefix, treeErr); err != nil {
			return 0, err
		}
		return TreeFailedExit, nil
	}
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

// MountArg returns the value of --fs for m.
func MountArg(m Mount) string {
	v := m.Dst + "=" + m.Socket
	if m.ReadOnly {
		v += ",ro"
	}
	return v
}

func parseMount(v string) (Mount, error) {
	dst, socket, ok := strings.Cut(v, "=")
	socket, ro := strings.CutSuffix(socket, ",ro")
	if !ok || !path.IsAbs(dst) || !path.IsAbs(socket) {
		return Mount{}, fmt.Errorf("invalid --fs %q: want <dst>=<socket>[,ro] with absolute paths", v)
	}
	return Mount{Dst: path.Clean(dst), Socket: socket, ReadOnly: ro}, nil
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
