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

package sidecar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
)

const runShort = "Serve a coding agent in a sandbox over A2A"

const runLong = `Serve a coding agent in a sandbox over A2A, for a host process that starts
a2a-serve-sidecar and talks to it.

a2a-serve-sidecar starts the agent, prints one JSON line on stdout once it is up, and
serves A2A until it gets SIGINT or SIGTERM. Each A2A context runs in an agent
process of its own. Logs go to stderr.`

const runExample = `a2a-serve-sidecar --agent opencode --workspace dir:$PWD \
  --inject-model-key openrouter=OPENROUTER_API_KEY \
  --listen unix:///tmp/a2a-serve-sidecar.sock`

// shutdownTimeout bounds the stop after a signal.
const shutdownTimeout = 30 * time.Second

// afterReadyError is an error of a server that printed its ready line.
type afterReadyError struct{ err error }

func (e *afterReadyError) Error() string { return e.err.Error() }

func (e *afterReadyError) Unwrap() error { return e.err }

// errAfterReady matches an [afterReadyError] with errors.Is.
var errAfterReady = errors.New("a2a-serve-sidecar failed after it was ready")

func (e *afterReadyError) Is(target error) bool { return target == errAfterReady }

// Command returns the a2a-serve-sidecar command, for its reference.
func Command() *cobra.Command { return newRunCmd(IO{}) }

func newRunCmd(std IO) *cobra.Command {
	cfg := serve.Config{Getenv: std.Getenv, Stdin: std.Stdin, Stderr: std.Stderr, Version: std.Version}
	cmd := &cobra.Command{
		Use:           "a2a-serve-sidecar",
		Short:         runShort,
		Long:          runLong,
		Example:       runExample,
		Version:       std.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return &serve.UsageError{Err: fmt.Errorf("a2a-serve-sidecar takes no arguments, got %q; pass the agent with --agent or --command", args[0])}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return run(cmd.Context(), cfg, std) },
	}
	cmd.SetOut(std.Stdout)
	cmd.SetErr(std.Stderr)
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &serve.UsageError{Err: err} })
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newAppArmorCmd(std))
	f := cmd.Flags()
	f.SortFlags = false
	f.StringVar(&cfg.Agent, "agent", "", "Agent to serve, a `name`: claude, copilot or opencode")
	f.StringVar(&cfg.Command, "command", "", "Command line of the agent, as shell `words`: replaces the command of --agent, or without --agent starts any agent that speaks ACP on stdio")
	f.StringVar(&cfg.Model, "model", "", "Model of the agent, in its own format: provider/model for opencode, such as openrouter/~google/gemini-flash-latest, a Claude model for claude, such as sonnet, or a Copilot model for copilot, such as gpt-6.2-sol (default: the agent's)")
	f.StringVar(&cfg.Image, "image", "", "Container image of the agent (default: the image of --agent); only applies to --sandbox docker and gvisor")
	f.StringVar(&cfg.Sandbox, "sandbox", serve.SandboxDocker, "Where the agent runs: docker, gvisor for Docker containers under gVisor's runtime runsc, bwrap for bubblewrap on this machine, with the agent and the a2a CLI that this machine has, or local for this machine without isolation")
	f.StringVar(&cfg.Egress, "egress", "", "Where the sandbox may connect, a `mode`: internet (the public internet and the --egress-allow hosts) or allowlist (only the --egress-allow hosts, the hosts of --inject-model-key and --egress-inject-header, and the --egress-middleware hosts) (default internet)")
	f.StringSliceVar(&cfg.EgressAllow, "egress-allow", nil, "Host the sandbox may also reach, private ones included: a `rule` such as nas.lan, *.example.com, 192.168.1.20 or 192.168.1.0/24, with an optional :port")
	f.StringArrayVar(&cfg.EgressMiddleware, "egress-middleware", nil, "HTTP middleware for a host: `host[:port]=URL`, with http://host:port[/path] or unix:///path; a2a-serve-sidecar terminates TLS for that host and forwards its requests in origin form with A2A-Sidecar-Context and A2A-Sidecar-Task headers")
	f.StringVar(&cfg.EgressProxy, "egress-proxy", "", "HTTP proxy that decides every connection of the sandbox and may intercept its requests, such as its model calls, a `URL`: http://host:port or unix:///path; the agent gets placeholder model keys, and the proxy adds the real ones. Replaces --egress, --egress-allow, --egress-middleware, --inject-model-key and --egress-inject-header")
	f.StringArrayVar(&cfg.Env, "env", nil, "Variable for the agent: `NAME=value`, such as DEBUG=1")
	f.StringArrayVar(&cfg.Secrets, "secret", nil, "Variable of your environment to pass to the agent, such as an API key: a `NAME`; the agent can read it")
	f.StringArrayVar(&cfg.ModelKeys, "inject-model-key", nil, "API key that a2a-serve-sidecar adds to the sandbox's requests to a model provider; the agent gets a placeholder in the variable: `provider=NAME`, with provider one of "+strings.Join(egressrules.ProviderNames(), ", ")+" and NAME a variable of your environment, such as anthropic=ANTHROPIC_API_KEY")
	f.StringArrayVar(&cfg.InjectHeaders, "egress-inject-header", nil, "Header that a2a-serve-sidecar adds to the sandbox's HTTPS requests to a host: `host[:port]=Name: value`, such as api.github.com=\"Authorization:Bearer env:GH_TOKEN\"; env:VAR and $VAR read from your environment and give the agent a placeholder")
	f.StringVar(&cfg.Workspace, "workspace", "", "Files of the agent, a `source`: dir:path for a directory, memory for files that a2a-serve-sidecar keeps in memory and drops at exit, or the URL of a file server of the host: http://, https:// or unix:///path")
	f.StringVar(&cfg.WorkspaceSync, "workspace-sync", serve.SyncTurn, "How the agent changes the workspace: turn gives each A2A context a private copy, written back after each of its turns, and returns the changed files with the task; live lets the agent change the workspace in place, and needs --sandbox docker or bwrap unless the workspace is a dir:")
	f.StringVar(&cfg.Tools, "tools", "", "Tools endpoint of the host, an MCP server over streamable HTTP: a `url`, http://, https:// or unix:///path; every agent session gets it as the MCP server host, and a2a-serve-sidecar calls the hooks it advertises")
	f.StringArrayVar(&cfg.Contacts, "contact", nil, "Other A2A agent the agent may call, through a2a-serve-sidecar: `name=url`, with the https:// URL of its agent card, or unix:///path of a socket that serves it, such as reviewer=https://reviewer.example.com")
	f.StringArrayVar(&cfg.ContactAuth, "contact-auth", nil, "Authorization header of the requests to a --contact, which the agent never sees: `name=value`; the value may end in env:VAR, which reads the variable VAR, or -, which reads one line from stdin, such as reviewer=\"Bearer env:REVIEWER_TOKEN\"")
	f.StringVar(&cfg.Listen, "listen", serve.DefaultListen, "Address of the A2A endpoint: unix:///path, or host:port with a loopback host; port 0 picks a free port")
	f.IntVar(&cfg.MaxProcesses, "max-processes", serve.DefaultMaxProcesses, "Agent processes that run at once, one per A2A context; an idle process stops to make room. One more process runs for the life of a2a-serve-sidecar, started to check that the agent starts")
	f.StringVar(&cfg.Home, "home", "", "Agent home `directory`, which every agent process shares (default: a temporary directory, removed at exit)")
	f.StringVar(&cfg.StateDir, "state-dir", "", "State `directory` for the files of the agent processes that no process may see, such as their sockets (default: a temporary directory, removed at exit)")
	return cmd
}

// run serves the agent of cfg until ctx ends or the A2A endpoint fails.
func run(ctx context.Context, cfg serve.Config, std IO) error {
	srv, err := serve.Start(ctx, cfg)
	if err != nil {
		return err
	}
	if err := writeReady(std.Stdout, srv.Ready()); err != nil {
		return &afterReadyError{err: errors.Join(fmt.Errorf("writing the ready line: %w", err), stop(ctx, srv))}
	}
	var served error
	select {
	case <-ctx.Done():
	case served = <-srv.Done():
	}
	if err := errors.Join(served, stop(ctx, srv)); err != nil {
		return &afterReadyError{err: err}
	}
	return nil
}

// stop closes srv within shutdownTimeout.
func stop(ctx context.Context, srv *serve.Server) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	return srv.Close(ctx)
}
