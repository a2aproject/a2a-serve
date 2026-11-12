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

// Package host starts a2a-serve network providers and drives them over the
// protocol of package netprovider: the server side of it.
package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/a2aproject/a2a-serve/netprovider"
)

// Config configures a provider process.
type Config struct {
	// Path is the provider binary.
	Path string
	// RunDir is a directory only this user can enter, for the sockets.
	RunDir string
	// Env is the provider's whole environment.
	Env []string
	// Stderr receives the provider's logs.
	Stderr io.Writer
	// OnNotice and OnHealth receive the provider's notifications.
	OnNotice func(netprovider.Notice)
	OnHealth func(netprovider.Health)
}

// AllowedEnv are the variables of the server's environment a provider gets:
// what finding files, certificates and proxies needs, and no secrets.
var AllowedEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TEMP", "TMP", "LANG", "LC_ALL", "TZ",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	"SYSTEMROOT", "APPDATA", "LOCALAPPDATA", "USERPROFILE",
}

// Environ returns the variables of environ named in [AllowedEnv].
func Environ(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if name, _, _ := strings.Cut(kv, "="); slices.Contains(AllowedEnv, name) {
			out = append(out, kv)
		}
	}
	return out
}

// Process is a running provider.
type Process struct {
	cfg    Config
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *os.File
	stderr *os.File
	tail   *tailWriter
	notes  chan *netprovider.Message

	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan *netprovider.Message
	exited  bool
	exitErr error

	done chan struct{}

	connectMu    sync.Mutex
	connectPath  string
	connectToken string
	listeners    []*Listener
	closeOnce    sync.Once
	closeErr     error
}

const (
	// maxQueuedNotes bounds the notifications waiting for their callback;
	// the provider's notifications past it are dropped.
	maxQueuedNotes = 256
	// pipeGrace is how long the output of an exited provider is read
	// before its pipes are closed, which children it left may hold open.
	pipeGrace = 2 * time.Second
	// startBusyRetry is how long a start that fails with ETXTBSY is retried.
	startBusyRetry = 5 * time.Second
)

// Start starts the provider at cfg.Path with `serve`.
func Start(cfg Config) (*Process, error) {
	if err := checkRunDir(cfg.RunDir); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(cfg.Path)
	if err != nil {
		return nil, err
	}
	// A child that another goroutine forks while the provider's binary is
	// still open for writing holds that descriptor until it execs, which
	// fails the start with ETXTBSY (golang.org/issue/22315).
	deadline := time.Now().Add(startBusyRetry)
	var (
		cmd        *exec.Cmd
		stdin      io.WriteCloser
		outR, errR *os.File
	)
	for {
		cmd, stdin, outR, errR, err = startProcess(cfg, path)
		if !errors.Is(err, syscall.ETXTBSY) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("starting the network provider %s: %w", cfg.Path, err)
	}
	p := &Process{
		cfg: cfg, cmd: cmd, stdin: stdin, stdout: outR, stderr: errR, tail: &tailWriter{w: cfg.Stderr},
		notes: make(chan *netprovider.Message, maxQueuedNotes), pending: map[int64]chan *netprovider.Message{}, done: make(chan struct{}),
	}
	readDone, logDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(readDone)
		p.read(outR)
	}()
	go func() {
		defer close(logDone)
		if _, err := io.Copy(p.tail, errR); err != nil && !errors.Is(err, os.ErrClosed) {
			slog.Debug("reading the network provider's logs", "error", err)
		}
	}()
	go p.notifyLoop()
	go p.wait(readDone, logDone)
	return p, nil
}

// startProcess starts the provider at path with `serve`, with stdout and
// stderr on pipes of the server's own, so that waiting for the process
// doesn't wait for children that hold them.
func startProcess(cfg Config, path string) (*exec.Cmd, io.WriteCloser, *os.File, *os.File, error) {
	cmd := exec.Command(path, "serve")
	cmd.Env = cfg.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = cfg.RunDir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, nil, errors.Join(err, stdin.Close())
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, nil, errors.Join(err, stdin.Close(), outR.Close(), outW.Close())
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	if cerr := errors.Join(outW.Close(), errW.Close()); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		// A failed Start closes stdin.
		return nil, nil, nil, nil, errors.Join(err, outR.Close(), errR.Close())
	}
	return cmd, stdin, outR, errR, nil
}

// wait waits for the process to exit and for its output, closing pipes
// that stay open, and fails the calls still pending.
func (p *Process) wait(readDone, logDone chan struct{}) {
	err := p.cmd.Wait()
	grace, cancel := context.WithTimeout(context.Background(), pipeGrace)
	defer cancel()
	for _, w := range []struct {
		done chan struct{}
		f    *os.File
	}{{readDone, p.stdout}, {logDone, p.stderr}} {
		select {
		case <-w.done:
		case <-grace.Done():
			if cerr := w.f.Close(); cerr != nil {
				slog.Debug("closing a pipe of the network provider", "error", cerr)
			}
			<-w.done
		}
	}
	exitErr := p.describeExit(err)
	p.mu.Lock()
	p.exited, p.exitErr = true, exitErr
	pending := p.pending
	p.pending = map[int64]chan *netprovider.Message{}
	p.mu.Unlock()
	close(p.notes)
	for _, ch := range pending {
		close(ch)
	}
	for _, f := range []*os.File{p.stdout, p.stderr} {
		if cerr := f.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
			slog.Debug("closing a pipe of the network provider", "error", cerr)
		}
	}
	close(p.done)
}

func checkRunDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("the run directory of the network provider: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("the run directory %s isn't a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("the run directory %s is open to other users (mode %o); want 0700", dir, fi.Mode().Perm())
	}
	return nil
}

func (p *Process) read(stdout io.Reader) {
	br := bufio.NewReaderSize(stdout, 64<<10)
	for {
		m, err := netprovider.ReadMessage(br)
		if err != nil {
			var rpcErr *netprovider.RPCError
			if errors.As(err, &rpcErr) || errors.Is(err, netprovider.ErrMessageTooLong) {
				slog.Warn("the network provider sent an invalid message", "error", err)
				continue
			}
			break
		}
		switch {
		case m.Method != "":
			// Callbacks run on their own goroutine, so that one that waits
			// for a call doesn't keep its answer from being read.
			select {
			case p.notes <- m:
			default:
				slog.Warn("the network provider sends notifications faster than the server takes them; dropping one", "method", m.Method)
			}
		case m.ID != nil:
			// The server sends numbers; accept them back as strings too.
			id, err := strconv.ParseInt(strings.Trim(string(m.ID), `"`), 10, 64)
			if err != nil {
				continue
			}
			p.mu.Lock()
			ch := p.pending[id]
			delete(p.pending, id)
			p.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	// Drain stdout so that the process isn't blocked writing.
	if _, err := io.Copy(io.Discard, br); err != nil && !errors.Is(err, os.ErrClosed) {
		slog.Debug("draining the network provider's output", "error", err)
	}
}

func (p *Process) describeExit(err error) error {
	msg := "the network provider exited"
	if err != nil {
		msg += " (" + err.Error() + ")"
	}
	if t := p.tail.String(); t != "" {
		msg += ": " + t
	}
	return errors.New(msg)
}

func (p *Process) notifyLoop() {
	for m := range p.notes {
		p.notify(m)
	}
}

func (p *Process) notify(m *netprovider.Message) {
	switch m.Method {
	case netprovider.NotifyNotice:
		var n netprovider.Notice
		if err := json.Unmarshal(m.Params, &n); err != nil || p.cfg.OnNotice == nil {
			return
		}
		p.cfg.OnNotice(n)
	case netprovider.NotifyHealth:
		var h netprovider.Health
		if err := json.Unmarshal(m.Params, &h); err != nil || p.cfg.OnHealth == nil {
			return
		}
		p.cfg.OnHealth(h)
	}
}

// Done is closed once the process exits.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err says why the process exited, once Done is closed.
func (p *Process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

// Call sends a request and decodes its result into out.
func (p *Process) Call(ctx context.Context, method string, params, out any) error {
	id := p.nextID.Add(1)
	m := &netprovider.Message{ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		m.Params = b
	}
	line, err := netprovider.EncodeMessage(m)
	if err != nil {
		return err
	}
	ch := make(chan *netprovider.Message, 1)
	p.mu.Lock()
	if p.exited {
		err := p.exitErr
		p.mu.Unlock()
		return err
	}
	p.pending[id] = ch
	p.mu.Unlock()
	p.writeMu.Lock()
	_, err = p.stdin.Write(line)
	p.writeMu.Unlock()
	if err != nil {
		p.forget(id)
		if exitErr := p.Err(); exitErr != nil {
			return exitErr
		}
		return fmt.Errorf("sending %s to the network provider: %w", method, err)
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return p.Err()
		}
		if resp.Error != nil {
			return cleanError(resp.Error).Err()
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("the network provider's %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		p.forget(id)
		return ctx.Err()
	}
}

func (p *Process) forget(id int64) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

// Describe asks the provider what it is.
func (p *Process) Describe(ctx context.Context) (netprovider.Description, error) {
	var d netprovider.Description
	if err := p.Call(ctx, netprovider.MethodDescribe, nil, &d); err != nil {
		return d, err
	}
	if d.Protocol != netprovider.ProtocolVersion {
		return d, fmt.Errorf("the network provider speaks protocol %d; this a2a-serve speaks %d", d.Protocol, netprovider.ProtocolVersion)
	}
	return d, nil
}

// Join asks the provider to join the network.
func (p *Process) Join(ctx context.Context, req netprovider.JoinRequest) (netprovider.JoinResult, error) {
	var res netprovider.JoinResult
	err := p.Call(ctx, netprovider.MethodJoin, req, &res)
	return res, err
}

// Member asks the provider for a machine of the network.
func (p *Process) Member(ctx context.Context, node string) (netprovider.Caller, error) {
	var c netprovider.Caller
	err := p.Call(ctx, netprovider.MethodMember, netprovider.MemberRequest{Node: node}, &c)
	return c, err
}

// Peers asks the provider for the other agents on the network.
func (p *Process) Peers(ctx context.Context) ([]netprovider.PeerInfo, error) {
	var res netprovider.PeersResult
	err := p.Call(ctx, netprovider.MethodPeers, nil, &res)
	return res.Peers, err
}

// Endpoints are the listeners and URLs of a provider that listens.
type Endpoints struct {
	Private, Public       *Listener
	PrivateURL, PublicURL string
	PrivateNote           string
}

// Listen opens the server's sockets for the URLs the agent wants and asks
// the provider to forward connections to them.
func (p *Process) Listen(ctx context.Context, private, public bool) (*Endpoints, error) {
	token, err := netprovider.NewToken()
	if err != nil {
		return nil, err
	}
	req := netprovider.ListenRequest{Token: token, ConnectSocket: filepath.Join(p.cfg.RunDir, "connect.sock")}
	if err := removeSocket(req.ConnectSocket); err != nil {
		return nil, err
	}
	ep := &Endpoints{}
	var opened []*Listener
	open := func(kind netprovider.Kind) (*Listener, string, error) {
		path := filepath.Join(p.cfg.RunDir, kind.String()+".sock")
		l, err := listen(ctx, path, kind, token)
		if err != nil {
			return nil, "", err
		}
		opened = append(opened, l)
		return l, path, nil
	}
	fail := func(err error) (*Endpoints, error) {
		for _, l := range opened {
			err = errors.Join(err, l.Close())
		}
		return nil, err
	}
	if private {
		if ep.Private, req.PrivateSocket, err = open(netprovider.Private); err != nil {
			return fail(err)
		}
	}
	if public {
		if ep.Public, req.PublicSocket, err = open(netprovider.Public); err != nil {
			return fail(err)
		}
	}
	var res netprovider.ListenResult
	if err := p.Call(ctx, netprovider.MethodListen, req, &res); err != nil {
		return fail(err)
	}
	if res.ConnectToken == "" {
		return fail(errors.New("the network provider returned no connect token"))
	}
	ep.PrivateURL, ep.PublicURL, ep.PrivateNote = res.PrivateURL, res.PublicURL, res.PrivateNote
	p.connectMu.Lock()
	p.connectPath, p.connectToken = req.ConnectSocket, res.ConnectToken
	p.listeners = append(p.listeners, opened...)
	p.connectMu.Unlock()
	return ep, nil
}

func removeSocket(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// maxConnectResponse bounds the response to a CONNECT.
const maxConnectResponse = 16 << 10

// DialContext opens a tunnel to addr, a host:port, through the provider.
func (p *Process) DialContext(ctx context.Context, _, addr string) (net.Conn, error) {
	p.connectMu.Lock()
	path, token := p.connectPath, p.connectToken
	p.connectMu.Unlock()
	if path == "" {
		return nil, errors.New("the network provider doesn't listen yet")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("reaching the network provider: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, errors.Join(err, conn.Close())
		}
	} else if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Bearer %s\r\n\r\n", addr, addr, token)
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, errors.Join(fmt.Errorf("reaching the network provider: %w", err), conn.Close())
	}
	br := bufio.NewReader(io.LimitReader(conn, maxConnectResponse))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("reaching the network provider: %w", err), conn.Close())
	}
	if err := resp.Body.Close(); err != nil {
		slog.Debug("closing a CONNECT response", "error", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Join(fmt.Errorf("the network provider refused the tunnel to %s: %s", addr, resp.Status), conn.Close())
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	if br.Buffered() > 0 {
		buffered, err := br.Peek(br.Buffered())
		if err != nil {
			return nil, errors.Join(err, conn.Close())
		}
		return &prefixConn{Conn: conn, prefix: bytes.Clone(buffered)}, nil
	}
	return conn, nil
}

type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(b, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// Transport returns a transport that sends every request through the
// provider.
func (p *Process) Transport() *http.Transport {
	return &http.Transport{
		DialContext:         p.DialContext,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
}

// closeTimeout is how long the provider has to exit after its stdin closes.
const closeTimeout = 10 * time.Second

// Close stops the provider: it closes its stdin and kills it if it doesn't
// exit in time, and closes the sockets.
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		var errs []error
		if err := p.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, err)
		}
		select {
		case <-p.done:
		case <-time.After(closeTimeout):
			slog.Warn("the network provider didn't exit; killing it")
			if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				errs = append(errs, err)
			}
			<-p.done
		}
		p.connectMu.Lock()
		for _, l := range p.listeners {
			errs = append(errs, l.Close())
		}
		p.listeners = nil
		if p.connectPath != "" {
			errs = append(errs, removeSocket(p.connectPath))
		}
		p.connectMu.Unlock()
		p.closeErr = errors.Join(errs...)
	})
	return p.closeErr
}

// Kill kills the process, as if it crashed.
func (p *Process) Kill() error {
	return p.cmd.Process.Kill()
}

// tailWriter passes the provider's logs on and keeps the last lines, for
// the error when it exits.
type tailWriter struct {
	w     io.Writer
	mu    sync.Mutex
	lines []string
	part  []byte
}

const tailLines = 5

func (t *tailWriter) Write(b []byte) (int, error) {
	t.mu.Lock()
	t.part = append(t.part, b...)
	for {
		i := bytes.IndexByte(t.part, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(printableText(string(t.part[:i]))); line != "" {
			t.lines = append(t.lines, truncate(line, 300))
			if len(t.lines) > tailLines {
				t.lines = t.lines[1:]
			}
		}
		t.part = t.part[i+1:]
	}
	if len(t.part) > 4096 {
		t.part = t.part[:0]
	}
	t.mu.Unlock()
	if t.w != nil {
		if _, err := t.w.Write(b); err != nil {
			slog.Debug("writing the network provider's logs", "error", err)
		}
	}
	return len(b), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, " | ")
}

// maxErrorText bounds the text of an error from the provider.
const maxErrorText = 1000

// cleanError returns e with its text printable and bounded, so that a
// provider's error can't rewrite the terminal it is shown in.
func cleanError(e *netprovider.RPCError) *netprovider.RPCError {
	out := &netprovider.RPCError{Code: e.Code, Message: truncate(printableText(e.Message), maxErrorText)}
	if e.Data != nil {
		out.Data = &netprovider.ErrorData{
			Kind:    truncate(printableText(e.Data.Kind), 64),
			Problem: truncate(printableText(e.Data.Problem), maxErrorText),
			Fix:     truncate(printableText(e.Data.Fix), maxErrorText),
		}
	}
	return out
}

// printableText drops terminal escapes and control characters, so that the
// provider's logs can't rewrite the terminal through an error.
func printableText(s string) string {
	s = ansiEscape.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
