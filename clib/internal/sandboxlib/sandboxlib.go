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

// Package sandboxlib is the sandbox library behind the C ABI of
// clib/sandbox: every call takes and returns JSON, and the caller decides
// intercepted connections and requests by answering the questions of
// [Library.Calls].
package sandboxlib

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-serve/clib/internal/bridge"
	"github.com/a2aproject/a2a-serve/sandbox"
	"github.com/a2aproject/a2a-serve/sandbox/egress"
	"github.com/a2aproject/a2a-serve/sandbox/sandboxnet"
)

// Timeouts of the questions to the caller, and FileTimeout of the calls
// on the file system in memory. A connection waits for its answer before
// the sandbox dials, so it must come fast.
const (
	ConnectTimeout = 10 * time.Second
	RequestTimeout = 5 * time.Minute
	FileTimeout    = 2 * time.Minute
)

// Spec describes a sandbox, as [sandbox.Spec] does.
type Spec struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	Command struct {
		Path string   `json:"path"`
		Args []string `json:"args"`
		Dir  string   `json:"dir"`
		Env  []string `json:"env"`
	} `json:"command"`
	Mounts []struct {
		Src      string `json:"src"`
		Dst      string `json:"dst"`
		ReadOnly bool   `json:"read_only"`
	} `json:"mounts"`
	// Memory gives the sandbox a file system in memory; Files are base64.
	Memory *struct {
		Dir   string            `json:"dir"`
		Size  string            `json:"size"`
		Files map[string][]byte `json:"files"`
	} `json:"memory"`
	Limits struct {
		Memory string `json:"memory"`
		CPUs   string `json:"cpus"`
		PIDs   int    `json:"pids"`
	} `json:"limits"`
	Labels map[string]string `json:"labels"`
	// Network, when set, routes every connection through the egress
	// boundary; nil gives the sandbox the runtime's own network.
	Network *struct {
		// Mode is internet or allowlist, and Allow the rules, as
		// --egress-allow takes them.
		Mode  string   `json:"mode"`
		Allow []string `json:"allow"`
		// Intercept asks the caller about every connection, and about the
		// requests of those it inspects.
		Intercept bool `json:"intercept"`
		// InspectTLS are the hosts whose TLS the boundary may terminate
		// with a CA the sandbox trusts.
		InspectTLS []string `json:"inspect_tls"`
	} `json:"network"`
	// Launcher is the host path of a2a-sandbox-init, which a sandbox with
	// a boundary or a file system in memory needs.
	Launcher string `json:"launcher"`
	// Backend is docker, the default, or local.
	Backend string `json:"backend"`
}

// Library holds the sandboxes of a C caller.
type Library struct {
	Calls     *bridge.Calls
	sandboxes bridge.Handles[*box]
	pruned    sync.Once
}

// New returns an empty library.
func New() *Library { return &Library{Calls: bridge.NewCalls(nil)} }

type box struct {
	sb         sandbox.Sandbox
	boundaries *sandbox.Boundaries
	dir        string
	stderr     *tail

	// procs are the processes of the sandbox that may run.
	mu    sync.Mutex
	procs []io.Closer
}

// result is the JSON of a call's result: its fields, or an error.
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

// Open prepares the sandbox of the JSON spec and returns {"handle", "info"}.
func (l *Library) Open(specJSON string) string {
	var spec Spec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		return result(nil, fmt.Errorf("reading the spec: %w", err))
	}
	h, info, err := l.open(spec)
	return result(map[string]any{"handle": h, "info": info}, err)
}

func (l *Library) open(spec Spec) (int64, sandbox.Info, error) {
	if spec.Name == "" {
		return 0, sandbox.Info{}, errors.New("the sandbox needs a name")
	}
	// Docker takes a relative bind source for the name of a volume.
	if spec.Launcher != "" {
		abs, err := filepath.Abs(spec.Launcher)
		if err != nil {
			return 0, sandbox.Info{}, err
		}
		spec.Launcher = abs
	}
	for i, m := range spec.Mounts {
		if m.Src == "" || m.Dst == "" {
			return 0, sandbox.Info{}, errors.New("a mount needs src and dst")
		}
		abs, err := filepath.Abs(m.Src)
		if err != nil {
			return 0, sandbox.Info{}, err
		}
		spec.Mounts[i].Src = abs
	}
	spec.Labels = maps.Clone(spec.Labels)
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	if self, ok := owner(os.Getpid()); ok {
		spec.Labels[OwnerLabel] = self
	}
	if cmp.Or(spec.Backend, "docker") == "docker" {
		l.pruned.Do(func() {
			if err := prune(context.Background()); err != nil {
				slog.Warn("removing the sandboxes of processes that are gone", "error", err)
			}
		})
	}
	dir, err := os.MkdirTemp("", "a2asb-")
	if err != nil {
		return 0, sandbox.Info{}, err
	}
	b := &box{dir: dir, stderr: &tail{max: 64 << 10}}
	b.boundaries = sandbox.NewBoundaries(filepath.Join(dir, "b"), 12, "abcdefghijklmnopqrstuvwxyz", &sandboxnet.Blocks{})
	fail := func(err error) (int64, sandbox.Info, error) {
		return 0, sandbox.Info{}, errors.Join(err, b.close(context.Background()))
	}
	ss := sandbox.Spec{
		Name:    spec.Name,
		Image:   spec.Image,
		Command: sandbox.Command{Path: spec.Command.Path, Args: spec.Command.Args, Dir: spec.Command.Dir, Env: spec.Command.Env},
		Limits:  sandbox.Limits{Memory: spec.Limits.Memory, CPUs: spec.Limits.CPUs, PIDs: spec.Limits.PIDs},
		Labels:  spec.Labels,
		Stderr:  b.stderr,
	}
	for _, m := range spec.Mounts {
		ss.Mounts = append(ss.Mounts, sandbox.Mount{Src: m.Src, Dst: m.Dst, ReadOnly: m.ReadOnly})
	}
	if spec.Memory != nil {
		ss.Memory = &sandbox.Memory{Dir: spec.Memory.Dir, Size: spec.Memory.Size, Files: spec.Memory.Files}
	}
	h := l.sandboxes.Add(b)
	if n := spec.Network; n != nil {
		policy, err := egress.NewPolicy(n.Mode, n.Allow)
		if err != nil {
			l.sandboxes.Remove(h)
			return fail(err)
		}
		rules := sandbox.BoundaryRules{Policy: func() egress.Policy { return policy }}
		if n.Intercept {
			rules.Interceptor = &interceptor{calls: l.Calls, sandbox: h}
		}
		if len(n.InspectTLS) > 0 {
			ca, err := sandboxnet.NewCA(n.InspectTLS)
			if err != nil {
				l.sandboxes.Remove(h)
				return fail(err)
			}
			rules.CA = ca
			ss.Network.CA = ca.PEM()
		}
		ss.Network.Boundary = b.boundaries.Opener(spec.Name, rules)
	}
	var backend sandbox.Backend
	switch cmp.Or(spec.Backend, "docker") {
	case "docker":
		launcher := func() string { return spec.Launcher }
		backend = sandbox.NewDocker(filepath.Join(dir, "env"), launcher)
	case "local":
		backend = sandbox.Local{}
	default:
		l.sandboxes.Remove(h)
		return fail(fmt.Errorf("unknown backend %q: want docker or local", spec.Backend))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sb, err := backend.Prepare(ctx, ss)
	if err != nil {
		l.sandboxes.Remove(h)
		return fail(err)
	}
	b.sb = sb
	return h, sb.Info(), nil
}

// close stops the processes, whose docker run may still be creating its
// container, then removes the sandbox.
func (b *box) close(ctx context.Context) error {
	b.mu.Lock()
	procs := b.procs
	b.procs = nil
	b.mu.Unlock()
	var errs []error
	for _, p := range procs {
		errs = append(errs, p.Close())
	}
	if b.sb != nil {
		errs = append(errs, b.sb.Close(ctx))
	}
	errs = append(errs, b.boundaries.Close(ctx), os.RemoveAll(b.dir))
	return errors.Join(errs...)
}

// Close removes the sandbox h and what it set up.
func (l *Library) Close(h int64) string {
	b, ok := l.sandboxes.Remove(h)
	if !ok {
		return result(nil, fmt.Errorf("no sandbox %d", h))
	}
	return result(map[string]any{}, b.close(context.Background()))
}

// Start starts the command of sandbox h and returns {"fd"}: a Unix socket
// that carries its stdio, which the caller owns. Closing it stops the
// command; it reads EOF once the command exits.
func (l *Library) Start(h int64) string {
	b, ok := l.sandboxes.Get(h)
	if !ok {
		return result(nil, fmt.Errorf("no sandbox %d", h))
	}
	rwc, err := b.sb.Start(context.Background())
	if err != nil {
		return result(nil, err)
	}
	fd, err := b.pipe(rwc)
	if err != nil {
		return result(nil, errors.Join(err, rwc.Close()))
	}
	return result(map[string]any{"fd": fd}, nil)
}

// pipe connects rwc to one end of a new socket pair and returns the other.
func (b *box) pipe(rwc io.ReadWriteCloser) (int, error) {
	fds, err := socketpair()
	if err != nil {
		return -1, fmt.Errorf("making the socket of the process: %w", err)
	}
	f := os.NewFile(uintptr(fds[0]), "sandbox-process")
	conn, err := net.FileConn(f)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return -1, errors.Join(err, syscall.Close(fds[1]))
	}
	uc := conn.(*net.UnixConn)
	p := &proc{rwc: rwc, conn: uc}
	b.mu.Lock()
	b.procs = append(b.procs, p)
	b.mu.Unlock()
	go func() {
		if _, err := io.Copy(uc, rwc); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Debug("copying the output of the sandbox's process", "error", err)
		}
		if err := uc.CloseWrite(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Debug("ending the output of the sandbox's process", "error", err)
		}
	}()
	go func() {
		if _, err := io.Copy(rwc, uc); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Debug("copying the input of the sandbox's process", "error", err)
		}
		if err := p.Close(); err != nil {
			slog.Debug("stopping the sandbox's process", "error", err)
		}
		b.forget(p)
	}()
	return fds[1], nil
}

// forget drops p, which stopped, from the processes of b.
func (b *box) forget(p *proc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.procs = slices.DeleteFunc(b.procs, func(c io.Closer) bool { return c == p })
}

// proc is a process of a sandbox and the library's end of its socket.
type proc struct {
	rwc  io.ReadWriteCloser
	conn *net.UnixConn

	once sync.Once
	err  error
}

// Close stops the process, waiting for it to exit, and closes the socket.
func (p *proc) Close() error {
	p.once.Do(func() {
		p.err = p.rwc.Close()
		if err := p.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			p.err = errors.Join(p.err, err)
		}
	})
	return p.err
}

// socketpair returns a pair of connected Unix sockets that processes this
// one starts don't inherit. The fork lock keeps a parallel fork from
// taking them before they are marked.
func socketpair() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	return fds, nil
}

// Stderr returns {"stderr"}: the last output of the sandbox's processes on
// stderr.
func (l *Library) Stderr(h int64) string {
	b, ok := l.sandboxes.Get(h)
	if !ok {
		return result(nil, fmt.Errorf("no sandbox %d", h))
	}
	return result(map[string]any{"stderr": b.stderr.String()}, nil)
}

func (l *Library) files(h int64) (sandbox.Files, error) {
	b, ok := l.sandboxes.Get(h)
	if !ok {
		return nil, fmt.Errorf("no sandbox %d", h)
	}
	f, ok := b.sb.(sandbox.Files)
	if !ok {
		return nil, errors.New("the sandbox has no file system in memory")
	}
	return f, nil
}

// ReadFile returns {"data"}, the base64 of the file name.
func (l *Library) ReadFile(h int64, name string) string {
	f, err := l.files(h)
	if err != nil {
		return result(nil, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), FileTimeout)
	defer cancel()
	data, err := f.ReadFile(ctx, name)
	return result(map[string]any{"data": data}, err)
}

// WriteFile writes the base64 data b64 to the file name.
func (l *Library) WriteFile(h int64, name, b64 string) string {
	f, err := l.files(h)
	if err != nil {
		return result(nil, err)
	}
	var data []byte
	if err := json.Unmarshal([]byte(`"`+b64+`"`), &data); err != nil {
		return result(nil, fmt.Errorf("decoding the data: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), FileTimeout)
	defer cancel()
	return result(map[string]any{}, f.WriteFile(ctx, name, data))
}

// List returns {"files"}, the paths under dir.
func (l *Library) List(h int64, dir string) string {
	f, err := l.files(h)
	if err != nil {
		return result(nil, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), FileTimeout)
	defer cancel()
	names, err := f.List(ctx, dir)
	if names == nil {
		names = []string{}
	}
	return result(map[string]any{"files": names}, err)
}

// Exec runs {"argv", "stdin", "timeout_ms"} in the sandbox and returns
// {"stdout", "stderr", "exit_code", "truncated"}, with base64 streams of
// up to [sandbox.MaxExecOutput] bytes each.
func (l *Library) Exec(h int64, reqJSON string) string {
	f, err := l.files(h)
	if err != nil {
		return result(nil, err)
	}
	var req struct {
		Argv      []string `json:"argv"`
		Stdin     []byte   `json:"stdin"`
		TimeoutMS int      `json:"timeout_ms"`
	}
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		return result(nil, fmt.Errorf("reading the request: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cmp.Or(req.TimeoutMS, 120_000))*time.Millisecond)
	defer cancel()
	res, err := f.Exec(ctx, req.Argv, req.Stdin)
	return result(map[string]any{"stdout": res.Stdout, "stderr": res.Stderr, "exit_code": res.ExitCode, "truncated": res.Truncated}, err)
}

// NextEvent returns the next question, waiting up to timeout, or {} when
// there is none.
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

// interceptor asks the caller about the connections of one sandbox.
type interceptor struct {
	calls   *bridge.Calls
	sandbox int64
}

func (i *interceptor) Connect(ctx context.Context, host string, port uint16) sandboxnet.ConnectAction {
	answer, err := i.calls.Ask(ctx, "connect", map[string]any{"sandbox": i.sandbox, "host": host, "port": port}, ConnectTimeout)
	if err != nil {
		slog.Warn("asking about a connection of the sandbox; refusing it", "host", host, "port", port, "error", err)
		return sandboxnet.ConnectDeny
	}
	var a struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(answer, &a); err != nil {
		slog.Warn("reading the answer about a connection; refusing it", "error", err)
		return sandboxnet.ConnectDeny
	}
	switch a.Action {
	case "allow":
		return sandboxnet.ConnectAllow
	case "inspect":
		return sandboxnet.ConnectInspect
	case "policy", "":
		return sandboxnet.ConnectPolicy
	default:
		return sandboxnet.ConnectDeny
	}
}

// requestAnswer is how the caller answers an intercepted request.
type requestAnswer struct {
	// Action is respond, forward or deny.
	Action string `json:"action"`
	// Status, Headers and Body make the response of respond and deny.
	Status  int         `json:"status"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
	// URL, SetHeaders, RemoveHeaders and Body change the request that
	// forward sends; a nil Body keeps its own.
	URL           string            `json:"url"`
	SetHeaders    map[string]string `json:"set_headers"`
	RemoveHeaders []string          `json:"remove_headers"`
}

func (i *interceptor) Request(ctx context.Context, r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	answer, err := i.calls.Ask(ctx, "request", map[string]any{
		"sandbox": i.sandbox, "method": r.Method, "url": r.URL.String(), "headers": r.Header, "body": body,
	}, RequestTimeout)
	if err != nil {
		return nil, err
	}
	var a requestAnswer
	if err := json.Unmarshal(answer, &a); err != nil {
		return nil, fmt.Errorf("reading the answer about a request: %w", err)
	}
	switch a.Action {
	case "respond", "deny":
		status := cmp.Or(a.Status, http.StatusOK)
		if a.Action == "deny" {
			status = cmp.Or(a.Status, http.StatusForbidden)
		}
		if a.Headers == nil {
			a.Headers = http.Header{}
		}
		resp := &http.Response{StatusCode: status, Header: a.Headers, Body: io.NopCloser(bytes.NewReader(a.Body)), ContentLength: int64(len(a.Body))}
		resp.Header.Set("Content-Length", fmt.Sprint(len(a.Body)))
		return resp, nil
	case "forward":
		if a.URL != "" {
			u, err := url.Parse(a.URL)
			if err != nil {
				return nil, fmt.Errorf("the answer forwards to an invalid URL: %w", err)
			}
			r.URL = u
		}
		for _, k := range a.RemoveHeaders {
			r.Header.Del(k)
		}
		for k, v := range a.SetHeaders {
			r.Header.Set(k, v)
		}
		if a.Body != nil {
			body = a.Body
		}
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		r.Header.Del("Content-Length")
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown action %q: want respond, forward or deny", a.Action)
	}
}

// tail keeps the last max bytes written to it.
type tail struct {
	max int
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	if os.Getenv("A2A_SANDBOX_STDERR") == "1" {
		if _, err := os.Stderr.Write(p); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.buf), "?")
}
