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

package sandbox

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/a2aproject/a2a-serve/internal/boundary"
)

// Main runs the commands that Docker sandboxes start this binary with. A
// binary that uses Docker with an egress endpoint calls it first in main,
// with os.Args[1:], and exits with code when ok. It returns ok false for
// any other arguments.
func Main(args []string) (code int, ok bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "sandbox-init":
		return runInit(args[1:], os.Stderr), true
	case "sandbox-relay":
		return runRelay(args[1:], os.Stdout, os.Stderr), true
	}
	return 0, false
}

func runInit(args []string, stderr io.Writer) int {
	if len(args) < 3 {
		return report(stderr, fmt.Errorf("requires at least 3 arg(s), only received %d", len(args)), 2)
	}
	code, err := boundary.RunInit(args, os.Environ(), os.Getenv(boundary.EnvFailSetup) != "", stderr)
	if err != nil {
		return report(stderr, err, 1)
	}
	return code
}

func runRelay(args []string, stdout, stderr io.Writer) int {
	var f boundary.RelayFlags
	fs := flag.NewFlagSet("sandbox-relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.EgressListen, "egress-listen", "", "path of the egress socket")
	fs.StringVar(&f.EgressTo, "egress-to", "", "where the egress socket's connections go: tcp://host:port or unix:///path")
	fs.StringVar(&f.ServerListen, "server-listen", "", "path of the socket through which the agent reaches the server")
	fs.StringVar(&f.ServerTo, "server-to", "", "where the server socket's connections go: tcp://host:port or unix:///path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch {
	case fs.NArg() > 0:
		return report(stderr, fmt.Errorf("unexpected argument %q", fs.Arg(0)), 2)
	case f.EgressListen == "" || f.EgressTo == "":
		return report(stderr, errors.New("--egress-listen and --egress-to are required"), 2)
	case (f.ServerListen == "") != (f.ServerTo == ""):
		return report(stderr, errors.New("--server-listen and --server-to go together"), 2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := boundary.RunRelay(ctx, f, os.Getenv(boundary.EnvRelayKey), stdout); err != nil {
		return report(stderr, err, 1)
	}
	return 0
}

// report writes err to stderr and returns code.
func report(stderr io.Writer, err error, code int) int {
	if _, werr := fmt.Fprintf(stderr, "Error: %v\n", err); werr != nil {
		slog.Debug("writing an error of the sandbox", "error", werr)
	}
	return code
}
