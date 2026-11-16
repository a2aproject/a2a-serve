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

// Package acplib is the a2aacp library behind the C ABI of clib/acp: it
// runs an ACP agent as an A2A executor, called in process. Every call takes
// and returns JSON. An agent that the caller starts itself, such as in a
// sandbox of the sandbox library, is a "start" question that the caller
// answers with the file descriptor of the agent's stdio.
package acplib

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/clib/internal/bridge"
)

// Limits of the library.
const (
	// StartTimeout bounds the wait for the caller to start an agent.
	StartTimeout = 5 * time.Minute
	// MaxStreams bounds the streams that are open at once.
	MaxStreams = 256
	// streamBuffer is how many events a stream holds before the agent
	// waits for the caller to take them.
	streamBuffer = 64
)

// Config configures an executor.
type Config struct {
	// ID is the caller's name of the executor, which its "start"
	// questions carry, since the first comes before NewExecutor returns
	// without PerContext.
	ID string `json:"id"`
	// Agent runs a local command; External makes every agent a "start"
	// question to the caller instead.
	Agent *struct {
		Argv []string `json:"argv"`
		Dir  string   `json:"dir"`
		Env  []string `json:"env"`
	} `json:"agent"`
	External bool `json:"external"`
	// PerContext runs an agent per A2A context, started on its first
	// message and stopped after IdleTimeoutMS without tasks.
	PerContext    bool `json:"per_context"`
	IdleTimeoutMS int  `json:"idle_timeout_ms"`
	MaxAgents     int  `json:"max_agents"`
	// Session sets the working directory and the MCP servers of the ACP
	// sessions, in the JSON of the ACP McpServer schema.
	Session struct {
		Cwd        string            `json:"cwd"`
		MCPServers []json.RawMessage `json:"mcp_servers"`
	} `json:"session"`
	// AskSession makes every new ACP session a "session" question to the
	// caller, whose answer {"cwd", "mcp_servers"} replaces Session's.
	AskSession bool `json:"ask_session"`
	// Allow and Deny answer permission requests by ACP tool kind, as
	// a2aacp.ToolRules does; others pause the task in input-required.
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
	// Settings are the session settings applied before each message,
	// such as {"model": "provider/model"}.
	Settings map[string]any `json:"settings"`

	InactivityTimeoutMS int `json:"inactivity_timeout_ms"`
	QuestionTimeoutMS   int `json:"question_timeout_ms"`
}

// Library holds the executors and streams of a C caller.
type Library struct {
	Calls     *bridge.Calls
	executors bridge.Handles[*executor]
	streams   bridge.Handles[*stream]

	mu    sync.Mutex
	nopen int
}

// New returns an empty library.
func New() *Library { return &Library{Calls: bridge.NewCalls(closeLateFD)} }

// closeLateFD closes the descriptor of an answer to start that came after
// the library stopped waiting, which nobody else would close.
func closeLateFD(typ string, answer json.RawMessage) {
	var a struct {
		FD *int `json:"fd"`
	}
	if typ != "start" || json.Unmarshal(answer, &a) != nil || a.FD == nil || *a.FD < 0 {
		return
	}
	if err := syscall.Close(*a.FD); err != nil {
		slog.Warn("closing the descriptor of a late start", "fd", *a.FD, "error", err)
	}
}

type executor struct {
	exec    *a2aacp.Executor
	handler a2asrv.RequestHandler
}

func result(v any, err error) string {
	if err != nil {
		data, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(data)
	}
	data, merr := json.Marshal(v)
	if merr != nil {
		return result(nil, merr)
	}
	return string(data)
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// NewExecutor starts the executor of the JSON config and returns
// {"handle"}. Without PerContext, it starts the agent first.
func (l *Library) NewExecutor(cfgJSON string) string {
	var cfg Config
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		return result(nil, fmt.Errorf("reading the config: %w", err))
	}
	h, err := l.newExecutor(cfg)
	return result(map[string]any{"handle": h}, err)
}

func (l *Library) newExecutor(cfg Config) (int64, error) {
	h := l.executors.Reserve()
	var agent a2aacp.Agent
	switch {
	case cfg.External && cfg.Agent != nil:
		return 0, errors.New("set agent or external, not both")
	case cfg.External:
		agent = a2aacp.AgentFunc(func(ctx context.Context, key string) (io.ReadWriteCloser, error) {
			return l.startExternal(ctx, cfg.ID, key)
		})
	case cfg.Agent != nil:
		agent = a2aacp.Process{Argv: cfg.Agent.Argv, Dir: cfg.Agent.Dir, Env: cfg.Agent.Env, Stderr: os.Stderr}
	default:
		return 0, errors.New("the config needs agent or external")
	}
	ac := &a2aacp.Config{
		AutoAnswer:        a2aacp.ToolRules{Allow: cfg.Allow, Deny: cfg.Deny},
		InactivityTimeout: ms(cfg.InactivityTimeoutMS),
		QuestionTimeout:   ms(cfg.QuestionTimeoutMS),
		Session: func(ctx context.Context, execCtx *a2asrv.ExecutorContext) (a2aacp.SessionParams, error) {
			if cfg.AskSession {
				return l.askSession(ctx, cfg, execCtx.ContextID)
			}
			return a2aacp.SessionParams{Cwd: cfg.Session.Cwd, MCPServers: cfg.Session.MCPServers}, nil
		},
	}
	if cfg.PerContext {
		ac.Pool = &a2aacp.PoolConfig{IdleTimeout: ms(cfg.IdleTimeoutMS), MaxAgents: cfg.MaxAgents}
	}
	if len(cfg.Settings) > 0 {
		ac.Settings = func(context.Context, *a2asrv.ExecutorContext) (map[string]any, error) { return cfg.Settings, nil }
	}
	ctx, cancel := context.WithTimeout(context.Background(), StartTimeout)
	defer cancel()
	exec, err := a2aacp.NewExecutor(ctx, agent, ac)
	if err != nil {
		return 0, err
	}
	handler := a2asrv.NewHandler(exec, a2asrv.WithTaskStore(newBoundedStore(MaxTasks)))
	l.executors.Set(h, &executor{exec: exec, handler: handler})
	return h, nil
}

// askSession asks the caller for the parameters of the ACP session of the
// A2A context key, with Session's working directory when the answer has none.
func (l *Library) askSession(ctx context.Context, cfg Config, key string) (a2aacp.SessionParams, error) {
	answer, err := l.Calls.Ask(ctx, "session", map[string]any{"id": cfg.ID, "key": key}, StartTimeout)
	if err != nil {
		return a2aacp.SessionParams{}, fmt.Errorf("asking the caller for the session: %w", err)
	}
	var a struct {
		Cwd        string            `json:"cwd"`
		MCPServers []json.RawMessage `json:"mcp_servers"`
		Error      string            `json:"error"`
	}
	if err := json.Unmarshal(answer, &a); err != nil {
		return a2aacp.SessionParams{}, fmt.Errorf("reading the session answer: %w", err)
	}
	if a.Error != "" {
		return a2aacp.SessionParams{}, fmt.Errorf("the caller refused the session: %s", a.Error)
	}
	if a.Cwd == "" {
		a.Cwd = cfg.Session.Cwd
	}
	return a2aacp.SessionParams{Cwd: a.Cwd, MCPServers: a.MCPServers}, nil
}

// startExternal asks the caller to start the agent of key for the
// executor id, and takes the file descriptor of its stdio from the answer
// {"fd"}.
func (l *Library) startExternal(ctx context.Context, id, key string) (io.ReadWriteCloser, error) {
	answer, err := l.Calls.Ask(ctx, "start", map[string]any{"id": id, "key": key}, StartTimeout)
	if err != nil {
		return nil, fmt.Errorf("asking the caller to start the agent: %w", err)
	}
	var a struct {
		FD    *int   `json:"fd"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(answer, &a); err != nil {
		return nil, fmt.Errorf("reading the answer to start: %w", err)
	}
	if a.Error != "" {
		return nil, errors.New(a.Error)
	}
	if a.FD == nil || *a.FD < 0 {
		return nil, errors.New("the answer to start has no fd")
	}
	return fdConn(*a.FD)
}

// fdConn takes the socket or pipe fd, which the library then owns.
func fdConn(fd int) (io.ReadWriteCloser, error) {
	f := os.NewFile(uintptr(fd), "agent")
	if f == nil {
		return nil, fmt.Errorf("fd %d isn't valid", fd)
	}
	c, err := net.FileConn(f)
	if err != nil {
		// A pipe or a terminal isn't a socket; read and write it as a file.
		return f, nil
	}
	if err := f.Close(); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

// CloseExecutor stops the executor h and its agents.
func (l *Library) CloseExecutor(h int64) string {
	e, ok := l.executors.Remove(h)
	if !ok {
		return result(nil, fmt.Errorf("no executor %d", h))
	}
	return result(map[string]any{}, e.exec.Close())
}

// stream is the events of one request.
type stream struct {
	events chan a2a.Event
	errc   chan error
	cancel context.CancelFunc
	done   chan struct{}
}

// Send sends the JSON A2A SendMessageRequest to the executor h and returns
// {"stream"}, whose events [Library.StreamNext] returns.
func (l *Library) Send(h int64, reqJSON string) string {
	e, ok := l.executors.Get(h)
	if !ok {
		return result(nil, fmt.Errorf("no executor %d", h))
	}
	var req a2a.SendMessageRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		return result(nil, fmt.Errorf("reading the request: %w", err))
	}
	if req.Message == nil {
		return result(nil, errors.New("the request has no message"))
	}
	if req.Message.ID == "" {
		req.Message.ID = a2a.NewMessageID()
	}
	if req.Message.Role == "" {
		req.Message.Role = a2a.MessageRoleUser
	}
	l.mu.Lock()
	if l.nopen >= MaxStreams {
		l.mu.Unlock()
		return result(nil, fmt.Errorf("%d streams are open", MaxStreams))
	}
	l.nopen++
	l.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	s := &stream{events: make(chan a2a.Event, streamBuffer), errc: make(chan error, 1), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer func() {
			l.mu.Lock()
			l.nopen--
			l.mu.Unlock()
			close(s.events)
			close(s.done)
		}()
		for ev, err := range e.handler.SendStreamingMessage(ctx, &req) {
			if err != nil {
				s.errc <- err
				return
			}
			select {
			case s.events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return result(map[string]any{"stream": l.streams.Add(s)}, nil)
}

// StreamNext returns the next event of the stream h, waiting up to
// timeout: {"event"} with an A2A StreamResponse, {"done": true} at the
// end, or {} when no event came in time.
func (l *Library) StreamNext(h int64, timeout time.Duration) string {
	s, ok := l.streams.Get(h)
	if !ok {
		return result(nil, fmt.Errorf("no stream %d", h))
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case ev, ok := <-s.events:
		if ok {
			return result(map[string]any{"event": a2a.StreamResponse{Event: ev}}, nil)
		}
		l.streams.Remove(h)
		select {
		case err := <-s.errc:
			return result(nil, err)
		default:
			return result(map[string]any{"done": true}, nil)
		}
	case <-t.C:
		return "{}"
	}
}

// CloseStream stops waiting for the events of the stream h. The task goes
// on; cancel it with [Library.Cancel].
func (l *Library) CloseStream(h int64) string {
	s, ok := l.streams.Remove(h)
	if !ok {
		return result(nil, fmt.Errorf("no stream %d", h))
	}
	s.cancel()
	go func() {
		for range s.events {
		}
	}()
	return result(map[string]any{}, nil)
}

// Cancel cancels the task id of the executor h and returns {"task"}.
func (l *Library) Cancel(h int64, id string) string {
	e, ok := l.executors.Get(h)
	if !ok {
		return result(nil, fmt.Errorf("no executor %d", h))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	task, err := e.handler.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(id)})
	return result(map[string]any{"task": task}, err)
}

// NextEvent returns the next question, waiting up to timeout, or {}.
func (l *Library) NextEvent(timeout time.Duration) string {
	e, ok := l.Calls.Next(timeout)
	if !ok {
		return "{}"
	}
	return result(e, nil)
}

// Reply answers the question id with the JSON answer.
func (l *Library) Reply(id int64, answer string) string {
	if !json.Valid([]byte(answer)) {
		return result(nil, errors.New("the answer isn't JSON"))
	}
	return result(map[string]any{}, l.Calls.Reply(id, json.RawMessage(answer)))
}

// SetLogLevel sets the level of the library's log on stderr: debug, info,
// warn or error.
func SetLogLevel(level string) error {
	var l slog.Level
	if err := l.UnmarshalText([]byte(cmp.Or(level, "info"))); err != nil {
		return err
	}
	slog.SetLogLoggerLevel(l)
	return nil
}
