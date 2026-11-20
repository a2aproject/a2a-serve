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

// Package sidecar implements the command line of a2a-serve-sidecar, which serves a
// coding agent in a sandbox over A2A for a host process. It is an a2a CLI
// command plugin, and imports only the core and the packages the core
// shares.
package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
)

// pluginName is the name the a2a CLI runs this binary under: a2a-<name>
// on PATH becomes `a2a <name>`.
const pluginName = "serve-sidecar"

// infoSubcommand is how the a2a CLI asks a command plugin for its metadata.
const infoSubcommand = "info"

// Exit codes, as in the error contract of the a2a CLI.
const (
	exitFailure = 1
	exitUsage   = 2
)

// IO is what the command uses of its process.
type IO struct {
	// Stdin is where a --contact-auth value of - is read from.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// StdoutIsTerminal tells whether stdout is a terminal: the a2a CLI
	// reads the plugin's metadata through a pipe.
	StdoutIsTerminal bool
	Getenv           func(string) string
	Version          string
}

// Main runs a2a-serve-sidecar with args, the command line without the program name,
// until ctx ends, and returns the exit code.
func Main(ctx context.Context, args []string, std IO) int {
	if len(args) == 1 && args[0] == infoSubcommand && !std.StdoutIsTerminal {
		return printInfo(std)
	}
	cmd := newRunCmd(std)
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	err := cmd.Execute()
	if err == nil {
		return 0
	}
	code := exitFailure
	if _, usage := errors.AsType[*serve.UsageError](err); usage {
		code = exitUsage
	}
	printError(std.Stderr, err)
	// A server that printed its ready line reports later errors on stderr
	// only, so that stdout has one line.
	if errors.Is(err, errAfterReady) {
		return code
	}
	if werr := writeReady(std.Stdout, serve.Ready{Error: err.Error()}); werr != nil {
		printError(std.Stderr, werr)
	}
	return code
}

// pluginInfo is the document the a2a CLI reads from `a2a-serve-sidecar info`.
type pluginInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

func printInfo(std IO) int {
	if err := json.NewEncoder(std.Stdout).Encode(pluginInfo{Name: pluginName, Version: std.Version, Description: runShort}); err != nil {
		printError(std.Stderr, err)
		return exitFailure
	}
	return 0
}

func printError(w io.Writer, err error) {
	if _, werr := fmt.Fprintf(w, "Error: %v\n", err); werr != nil {
		slog.Debug("writing an error", "error", werr)
	}
}

// writeReady writes the ready line r.
func writeReady(w io.Writer, r serve.Ready) error {
	return json.NewEncoder(w).Encode(r)
}
