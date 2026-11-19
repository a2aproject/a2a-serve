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

// Command refhost serves the reference host, for trying a2a-run --tools
// and --egress-proxy by hand. One listener serves both:
//
//	go run ./internal/a2arun/refhost/cmd/refhost --listen unix:///tmp/host.sock --deny 'rm *'
//
// The tools endpoint allows every tool call but the shell commands that
// match a --deny or --ask glob, in which * matches any run of characters,
// and logs every message on stderr. The egress proxy intercepts every
// connection and forwards its requests, refusing private destinations.
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

	"github.com/a2aproject/a2a-serve/internal/a2arun/hosttools"
	"github.com/a2aproject/a2a-serve/internal/a2arun/refhost"
)

type globs []string

func (g *globs) String() string     { return strings.Join(*g, ",") }
func (g *globs) Set(v string) error { *g = append(*g, v); return nil }

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "unix:///path or host:port")
	noHooks := flag.Bool("no-hooks", false, "advertise no hooks")
	var deny, ask globs
	flag.Var(&deny, "deny", "shell command `glob` that beforeTool denies, repeated")
	flag.Var(&ask, "ask", "shell command `glob` that beforeTool leaves to the A2A caller, repeated")
	flag.Parse()
	if err := run(*listen, !*noHooks, deny, ask); err != nil {
		fmt.Fprintln(os.Stderr, "refhost:", err)
		os.Exit(1)
	}
}

func run(listen string, hooks bool, deny, ask []string) error {
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
	host := refhost.New(refhost.Config{Tools: refhost.NewTools(cfg)})
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return host.Serve(ctx, ln)
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
