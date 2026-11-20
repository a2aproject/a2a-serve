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

// Command refhost serves the reference host, for trying a2a-serve-sidecar --tools,
// --egress-proxy and --workspace by hand. One listener serves all three:
//
//	go run ./internal/sidecar/refhost/cmd/refhost --listen unix:///tmp/host.sock --deny 'rm *' --workspace
//
// The tools endpoint allows every tool call but the shell commands that
// match a --deny or --ask glob, in which * matches any run of characters,
// and logs every message on stderr. The egress proxy intercepts every
// connection and forwards its requests, refusing private destinations.
// The agent of a2a-serve-sidecar --egress-proxy has placeholder model keys; with
// --model-key provider=NAME, the proxy replaces the placeholder in the
// requests to the provider's inference endpoints with the key in the
// variable NAME:
//
//	go run ./internal/sidecar/refhost/cmd/refhost --listen unix:///tmp/host.sock --model-key gemini=GEMINI_API_KEY
//
// With --workspace, the listener also serves a file tree in memory with
// the workspace protocol, and logs each change of it on stderr.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/sidecar/hosttools"
	"github.com/a2aproject/a2a-serve/internal/sidecar/refhost"
)

type globs []string

func (g *globs) String() string     { return strings.Join(*g, ",") }
func (g *globs) Set(v string) error { *g = append(*g, v); return nil }

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "unix:///path or host:port")
	noHooks := flag.Bool("no-hooks", false, "advertise no hooks")
	workspace := flag.Bool("workspace", false, "serve a file tree in memory for a2a-serve-sidecar --workspace")
	var deny, ask, modelKeys globs
	flag.Var(&deny, "deny", "shell command `glob` that beforeTool denies, repeated")
	flag.Var(&ask, "ask", "shell command `glob` that beforeTool leaves to the A2A caller, repeated")
	flag.Var(&modelKeys, "model-key", "API key that the egress proxy puts in place of the agent's placeholder in its requests to a model provider: `provider=NAME`, with provider one of "+strings.Join(egressrules.ProviderNames(), ", ")+" and NAME a variable of your environment, repeated")
	flag.Parse()
	keys := map[string]string{}
	for _, arg := range modelKeys {
		provider, key, err := refhost.ParseModelKey(arg, os.Getenv)
		if err != nil {
			fmt.Fprintln(os.Stderr, "refhost:", err)
			os.Exit(2)
		}
		keys[provider] = key
	}
	if err := run(*listen, !*noHooks, *workspace, deny, ask, keys); err != nil {
		fmt.Fprintln(os.Stderr, "refhost:", err)
		os.Exit(1)
	}
}

func run(listen string, hooks, workspace bool, deny, ask []string, modelKeys map[string]string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := refhost.ToolsConfig{
		Decide: func(p hosttools.BeforeToolParams) hosttools.BeforeToolResult {
			res := decide(p, deny, ask)
			log.Info("beforeTool decided", "call", p.Call.ID, "decision", res.Decision)
			return res
		},
		OnRecord: func(r refhost.Record) { log.Info(r.Method, "params", string(r.Params)) },
	}
	if hooks {
		cfg.Hooks = hosttools.HookSupport{BeforeTool: true, AfterTool: true}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hostCfg := refhost.Config{Tools: refhost.NewTools(cfg), ModelKeys: modelKeys}
	for provider := range modelKeys {
		log.Info("the egress proxy adds a model key", "provider", provider)
	}
	if workspace {
		tree := fsys.Memory()
		hostCfg.Workspace = fsys.Handler(tree)
		go logChanges(ctx, tree, log)
	}
	host := refhost.New(hostCfg)
	network, addr := "tcp", listen
	if path, ok := strings.CutPrefix(listen, "unix://"); ok {
		network, addr = "unix", path
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	shown := "http://" + ln.Addr().String() + "/"
	if network == "unix" {
		shown = "unix://" + addr
	}
	fmt.Println(shown)
	return host.Serve(ctx, ln)
}

// logChanges logs each change of tree until ctx ends.
func logChanges(ctx context.Context, tree fsys.FS, log *slog.Logger) {
	cursor := ""
	for ctx.Err() == nil {
		changes, next, err := tree.Changes(ctx, cursor)
		if err != nil && ctx.Err() == nil {
			log.Warn("reading the changes of the workspace", "error", err)
		}
		cursor = next
		for _, c := range changes {
			log.Info("workspace changed", "path", c.Path, "removed", c.Removed, "mode", c.Mode, "size", c.Size)
		}
	}
}

// decide denies or asks about the shell commands that match deny or ask,
// and allows every other call.
func decide(p hosttools.BeforeToolParams, deny, ask []string) hosttools.BeforeToolResult {
	if p.Call.Command == nil {
		return hosttools.BeforeToolResult{Decision: hosttools.DecisionAllow}
	}
	for _, g := range deny {
		if match(g, *p.Call.Command) {
			return hosttools.BeforeToolResult{Decision: hosttools.DecisionDeny, Message: "the host denies commands that match " + g}
		}
	}
	for _, g := range ask {
		if match(g, *p.Call.Command) {
			return hosttools.BeforeToolResult{Decision: hosttools.DecisionAsk}
		}
	}
	return hosttools.BeforeToolResult{Decision: hosttools.DecisionAllow}
}

func match(glob, s string) bool {
	parts := strings.Split(glob, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$").MatchString(s)
}
