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

// Command a2a-serve-sidecar serves a coding agent in a sandbox over A2A, for a host
// process. It is an a2a CLI command plugin: `a2a serve-sidecar`.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/sidecar"
)

// version is set by the release build.
var version = "dev"

func main() {
	// Sandboxes run this binary as their launcher.
	if code, ok := sandbox.Main(os.Args[1:]); ok {
		os.Exit(code)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := sidecar.Main(ctx, os.Args[1:], sidecar.IO{
		Stdin:            os.Stdin,
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
		StdoutIsTerminal: term.IsTerminal(int(os.Stdout.Fd())),
		Getenv:           os.Getenv,
		Version:          version,
	})
	stop()
	os.Exit(code)
}
