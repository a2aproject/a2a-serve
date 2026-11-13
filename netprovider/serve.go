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

package netprovider

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Provider is a network provider. The SDK calls its methods for the
// server's requests, each on its own goroutine.
type Provider interface {
	// Describe says what the provider is.
	Describe() Description
	// Join joins the network. It may take as long as a sign-in takes, and
	// tells the user what to do with [Host.Notice]. It returns when ctx is
	// done, so blocking calls that ignore ctx need bounds of their own.
	Join(ctx context.Context, req JoinRequest) (JoinResult, error)
	// Listen starts forwarding the network's connections to the server
	// with in, and returns the agent's URLs. ConnectToken is filled in by
	// the SDK.
	Listen(ctx context.Context, in *Inbound) (ListenResult, error)
	// Member returns the machine node of the network as the provider would
	// identify its connections now, [ErrNotMember] once the network no
	// longer has it, or [ErrUnsupported].
	Member(ctx context.Context, node string) (Caller, error)
	// Peers lists other agents on the network, or returns [ErrUnsupported].
	Peers(ctx context.Context) ([]PeerInfo, error)
	// Outbound serves the requests the server sends to the hosts of
	// [Description.Hosts] through the CONNECT tunnel.
	Outbound() http.Handler
	// Close leaves the network. The server kills the provider 10 seconds
	// after it closes stdin, so Close has to finish before.
	Close() error
}

// Host is the server, as the provider sees it: where its notifications go.
type Host struct {
	out *writer
}

// Notice shows message to the user, such as a sign-in link.
func (h *Host) Notice(message string) {
	h.out.notify(NotifyNotice, Notice{Message: message})
}

// Health tells the server whether the network reaches the agent now.
func (h *Host) Health(health Health) {
	h.out.notify(NotifyHealth, health)
}

// Main runs the provider that newProvider returns as the server expects:
// `<binary> serve`, with the protocol on stdin and stdout. It exits when
// stdin closes or on SIGTERM. Anything else written to stdout breaks the
// protocol: a provider that links a library that prints there points
// os.Stdout at a duplicate of it, and stdout at stderr, before Main.
func Main(newProvider func(*Host) Provider) {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintf(os.Stderr, "usage: %s serve\n\nThis is an a2a-serve network provider; a2a-serve runs it.\n", os.Args[0])
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Serve(ctx, os.Stdin, os.Stdout, newProvider); err != nil {
		slog.Error("network provider stopped", "error", err)
		stop()
		os.Exit(1)
	}
}

// Serve runs the protocol on r and w until r ends or ctx is done, then
// closes the provider.
func Serve(ctx context.Context, r io.Reader, w io.Writer, newProvider func(*Host) Provider) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &writer{w: w}
	s := &session{ctx: ctx, out: out, p: newProvider(&Host{out: out})}
	lines := make(chan *Message)
	readErr := make(chan error, 1)
	go func() {
		br := bufio.NewReaderSize(r, 64<<10)
		for {
			m, err := ReadMessage(br)
			var rpcErr *RPCError
			if errors.As(err, &rpcErr) {
				id := json.RawMessage("null")
				if m != nil && m.ID != nil {
					id = m.ID
				}
				out.write(&Message{ID: id, Error: rpcErr})
				continue
			}
			if errors.Is(err, ErrMessageTooLong) {
				out.write(&Message{ID: json.RawMessage("null"), Error: &RPCError{Code: CodeInvalidRequest, Message: err.Error()}})
				continue
			}
			if err != nil {
				readErr <- err
				return
			}
			select {
			case lines <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	var wg sync.WaitGroup
	var err error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err = <-readErr:
			if errors.Is(err, io.EOF) {
				err = nil
			}
			break loop
		case m := <-lines:
			wg.Go(func() { s.handle(m) })
		}
	}
	cancel()
	closeErr := s.close()
	wg.Wait()
	return errors.Join(err, closeErr)
}

type session struct {
	ctx context.Context
	out *writer
	p   Provider

	mu       sync.Mutex
	listened bool
	connect  *http.Server
	in       *Inbound
}

func (s *session) handle(m *Message) {
	if m.Method == "" {
		// The server sends no responses; drop them.
		return
	}
	result, err := s.call(m)
	if m.ID == nil {
		return
	}
	if err != nil {
		s.out.write(&Message{ID: m.ID, Error: RPCErrorOf(err)})
		return
	}
	b, err := json.Marshal(result)
	if err != nil {
		s.out.write(&Message{ID: m.ID, Error: &RPCError{Code: CodeInternalError, Message: err.Error()}})
		return
	}
	s.out.write(&Message{ID: m.ID, Result: b})
}

func params[T any](m *Message) (T, error) {
	var v T
	if len(m.Params) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(m.Params, &v); err != nil {
		return v, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}
	return v, nil
}

func (s *session) call(m *Message) (any, error) {
	ctx := s.ctx
	switch m.Method {
	case MethodDescribe:
		d := s.p.Describe()
		d.Protocol = ProtocolVersion
		if d.Hosts == nil {
			d.Hosts = []string{}
		}
		return d, nil
	case MethodJoin:
		req, err := params[JoinRequest](m)
		if err != nil {
			return nil, err
		}
		return s.p.Join(ctx, req)
	case MethodListen:
		req, err := params[ListenRequest](m)
		if err != nil {
			return nil, err
		}
		return s.listen(ctx, req)
	case MethodMember:
		req, err := params[MemberRequest](m)
		if err != nil {
			return nil, err
		}
		c, err := s.p.Member(ctx, req.Node)
		if err != nil {
			return nil, err
		}
		c.ClientKey = ""
		return c, nil
	case MethodPeers:
		peers, err := s.p.Peers(ctx)
		if err != nil {
			return nil, err
		}
		if peers == nil {
			peers = []PeerInfo{}
		}
		return PeersResult{Peers: peers}, nil
	}
	return nil, &RPCError{Code: CodeMethodNotFound, Message: "unknown method " + m.Method}
}

func (s *session) listen(ctx context.Context, req ListenRequest) (ListenResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listened {
		return ListenResult{}, errors.New("listen was already called")
	}
	if req.ConnectSocket == "" || req.Token == "" {
		return ListenResult{}, &RPCError{Code: CodeInvalidParams, Message: "listen needs connect_socket and token"}
	}
	in := &Inbound{ctx: s.ctx, private: req.PrivateSocket, public: req.PublicSocket, token: req.Token}
	token, err := NewToken()
	if err != nil {
		return ListenResult{}, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", req.ConnectSocket)
	if err != nil {
		return ListenResult{}, fmt.Errorf("listening on the connect socket: %w", err)
	}
	res, err := s.p.Listen(ctx, in)
	if err != nil {
		return ListenResult{}, errors.Join(err, ln.Close())
	}
	srv := &http.Server{Handler: connectHandler(token, s.p.Outbound()), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("connect socket stopped", "error", err)
		}
	}()
	s.listened, s.connect, s.in = true, srv, in
	res.ConnectToken = token
	return res, nil
}

func (s *session) close() error {
	s.mu.Lock()
	srv := s.connect
	s.mu.Unlock()
	var errs []error
	if srv != nil {
		errs = append(errs, srv.Close())
	}
	errs = append(errs, s.p.Close())
	return errors.Join(errs...)
}

// NewToken returns a random token.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// connectHandler serves CONNECT requests with the token, and then the
// requests in the tunnel with outbound.
func connectHandler(token string, outbound http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Proxy-Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("Proxy-Authenticate", "Bearer")
			http.Error(w, "wrong connect token", http.StatusProxyAuthRequired)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "can't hijack", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			slog.Warn("hijacking a CONNECT", "error", err)
			return
		}
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
			closeConn(conn)
			return
		}
		c := conn
		if rw.Reader.Buffered() > 0 {
			c = &bufferedConn{Conn: conn, r: rw.Reader}
		}
		ln := newOneConnListener(c)
		srv := &http.Server{Handler: fullDuplex(outbound), ReadHeaderTimeout: time.Minute, ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateClosed || st == http.StateHijacked {
				closeConn(ln)
			}
		}}
		if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Debug("serving a tunnel", "error", err)
		}
	})
}

// fullDuplex serves next with the request body open once the response
// starts. An outbound handler is usually a reverse proxy, whose transport
// reads the body to its end after the peer may have answered; a closed
// body fails that read, and the transport drops the peer's response.
func fullDuplex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			slog.Debug("no full duplex for a tunneled request", "error", err)
		}
		next.ServeHTTP(w, r)
	})
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneConnListener accepts one connection, then blocks until closed.
type oneConnListener struct {
	conn chan net.Conn
	addr net.Addr
	done chan struct{}
	once sync.Once
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{conn: make(chan net.Conn, 1), addr: c.LocalAddr(), done: make(chan struct{})}
	l.conn <- c
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conn:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.addr }

func closeConn(c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("closing a connection", "error", err)
	}
}

// writer writes messages, one line each, from any goroutine.
type writer struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *writer) write(m *Message) {
	b, err := EncodeMessage(m)
	if err != nil {
		slog.Error("encoding a message", "error", err)
		if m.ID == nil {
			return
		}
		if b, err = EncodeMessage(&Message{ID: m.ID, Error: &RPCError{Code: CodeInternalError, Message: err.Error()}}); err != nil {
			return
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(b); err != nil {
		slog.Debug("writing to the server", "error", err)
	}
}

func (w *writer) notify(method string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("encoding a notification", "error", err)
		return
	}
	w.write(&Message{Method: method, Params: b})
}
