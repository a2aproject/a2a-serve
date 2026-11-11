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

//go:build unix

// Command slim-a2a-send sends a message to an A2A agent on a SLIM node by
// its SLIM name, over SLIMRPC, and prints the agent's reply. It knows
// nothing about a2a-serve: it shows that any SLIM A2A client reaches an
// agent that the SLIM network provider put on the node.
//
//	slim-a2a-send [-node URL] [-from org/namespace/name] <org/namespace/agent> <text>
//
// The shared secret of the node's agents comes from $SLIM_SECRET.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2aslimrpc "github.com/agntcy/slim-a2a-go/a2aslimrpc/v1"
	slim_bindings "github.com/agntcy/slim-bindings-go/v3"
	"github.com/agntcy/slim-bindings-go/v3/slim_rpc"
	"golang.org/x/sys/unix"
)

func main() {
	node := flag.String("node", "http://127.0.0.1:46357", "SLIM node")
	from := flag.String("from", "", "SLIM name of this client (default: <org>/<namespace>/slim-a2a-send of the agent)")
	timeout := flag.Duration("timeout", 5*time.Minute, "how long to wait for the reply")
	flag.Usage = func() {
		if _, err := fmt.Fprint(flag.CommandLine.Output(), "usage: slim-a2a-send [flags] <org/namespace/agent> <text>\n\nThe shared secret comes from $SLIM_SECRET.\n\n"); err != nil {
			os.Exit(2)
		}
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*node, *from, flag.Arg(0), flag.Arg(1), *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "slim-a2a-send: %v\n", err)
		os.Exit(1)
	}
}

func parseName(s string) (*slim_bindings.Name, []string, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, nil, fmt.Errorf("want a SLIM name such as a2a/demo/coder, got %q", s)
	}
	return slim_bindings.NewName(parts[0], parts[1], parts[2]), parts, nil
}

func run(node, from, to, text string, timeout time.Duration) error {
	secret := os.Getenv("SLIM_SECRET")
	if len(secret) < 32 {
		return errors.New("set SLIM_SECRET to the shared secret of the node's agents, at least 32 bytes")
	}
	target, parts, err := parseName(to)
	if err != nil {
		return err
	}
	if from == "" {
		from = parts[0] + "/" + parts[1] + "/slim-a2a-send"
	}
	self, _, err := parseName(from)
	if err != nil {
		return err
	}
	// The native library logs to stdout; keep the reply apart.
	out, err := keepStdout()
	if err != nil {
		return err
	}
	tracing := slim_bindings.NewTracingConfig()
	tracing.LogLevel = "error"
	if err := slim_bindings.InitializeWithConfigs(slim_bindings.NewRuntimeConfig(), tracing, []slim_bindings.ServiceConfig{slim_bindings.NewServiceConfig()}); err != nil {
		return fmt.Errorf("starting SLIM: %w", err)
	}
	svc := slim_bindings.GetGlobalService()
	app, err := svc.CreateAppWithSecret(self, secret)
	if err != nil {
		return fmt.Errorf("creating the SLIM app %s: %w", from, err)
	}
	defer app.Destroy()
	cfg := slim_bindings.NewInsecureClientConfig(node)
	if strings.HasPrefix(node, "https://") {
		cfg = slim_bindings.NewSecureClientConfig(node)
	}
	conn, err := svc.Connect(cfg)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", node, err)
	}
	if err := app.Subscribe(self, &conn); err != nil {
		return fmt.Errorf("subscribing to %s: %w", from, err)
	}
	tr := a2aslimrpc.NewTransport(slim_rpc.ChannelNewWithConnection(app, target, &conn))
	defer func() {
		if err := tr.Destroy(); err != nil {
			fmt.Fprintf(os.Stderr, "slim-a2a-send: closing the channel: %v\n", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	card, err := tr.GetExtendedAgentCard(ctx, nil, &a2a.GetExtendedAgentCardRequest{})
	if err != nil {
		return fmt.Errorf("getting the card of %s: %w", to, err)
	}
	p := &printer{w: out}
	p.printf("→ %s (%s)\n", card.Name, to)
	req := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))}
	var state a2a.TaskState
	for ev, err := range tr.SendStreamingMessage(ctx, nil, req) {
		if err != nil {
			return fmt.Errorf("sending to %s: %w", to, err)
		}
		switch e := ev.(type) {
		case *a2a.TaskArtifactUpdateEvent:
			p.printf("%s", textOf(e.Artifact.Parts))
		case *a2a.Message:
			p.printf("%s", textOf(e.Parts))
		case *a2a.TaskStatusUpdateEvent:
			state = e.Status.State
			if e.Status.Message != nil && state != a2a.TaskStateWorking {
				p.printf("%s", textOf(e.Status.Message.Parts))
			}
		case *a2a.Task:
			state = e.Status.State
		}
	}
	if state != "" {
		p.printf("\n✓ task %s\n", strings.TrimPrefix(string(state), "TASK_STATE_"))
	}
	return p.err
}

// printer writes the reply and keeps the first error.
type printer struct {
	w   *os.File
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

func textOf(parts a2a.ContentParts) string {
	var b strings.Builder
	for _, p := range parts {
		if t, ok := p.Content.(a2a.Text); ok {
			b.WriteString(string(t))
		}
	}
	return b.String()
}

// keepStdout returns a duplicate of stdout, and points stdout at stderr.
func keepStdout() (*os.File, error) {
	fd, err := unix.Dup(1)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	if err := unix.Dup2(2, 1); err != nil {
		return nil, errors.Join(err, unix.Close(fd))
	}
	return os.NewFile(uintptr(fd), "/dev/stdout"), nil
}
