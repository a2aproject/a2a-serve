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

// Package httpserver makes the HTTP servers of a2a-serve, with limits on
// what one listener spends on its callers.
package httpserver

import (
	"context"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

const (
	// MaxHeaderBytes bounds the request line and headers of a request.
	MaxHeaderBytes = 64 << 10
	// MaxConcurrent is how many requests one server handles at once,
	// including open streams.
	MaxConcurrent = 1024
	// MaxUnauthenticated is how many of a server's requests may be on
	// routes that serve callers before they authenticate.
	MaxUnauthenticated = 256
	// MaxUnauthenticatedPerClient is how many of those one client may make.
	MaxUnauthenticatedPerClient = 32
	// MaxMemberRequests is how many of a server's requests the callers of
	// the member role may make together once they authenticate.
	MaxMemberRequests = 512
	// MaxMemberRequestsPerCaller is how many of those one caller may make,
	// so that one member leaves the others room.
	MaxMemberRequestsPerCaller = 32
	// MaxGuestRequests is how many the callers of the guest role may make.
	// With the unauthenticated requests, members and guests leave the
	// owner at least 128 of MaxConcurrent, and guests can't take the
	// requests of members.
	MaxGuestRequests = 128

	// BusyMessage is the error of a request refused for lack of room.
	BusyMessage = "the server is busy; try again later"
	// CallerBusyMessage is the error of a request over its caller's share.
	CallerBusyMessage = "you have too many requests open at once; close one or try again later"

	readHeaderTimeout = 30 * time.Second
	idleTimeout       = 2 * time.Minute
)

// Option configures a server made by New.
type Option func(*http.Server)

// WithConnContext derives the context of the requests of a connection from
// the context of its server with f.
func WithConnContext(f func(context.Context, net.Conn) context.Context) Option {
	return func(s *http.Server) { s.ConnContext = f }
}

// New returns a server of h whose requests derive their context from ctx.
func New(ctx context.Context, h http.Handler, opts ...Option) *http.Server {
	srv := &http.Server{
		Handler:           LimitConcurrency(MaxConcurrent, h),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	for _, o := range opts {
		o(srv)
	}
	return srv
}

// LimitConcurrency handles at most n requests at once and answers the rest
// with 503.
func LimitConcurrency(n int, next http.Handler) http.Handler {
	slots := make(chan struct{}, n)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
		default:
			w.Header().Set("Retry-After", "1")
			jsonhttp.Error(w, http.StatusServiceUnavailable, BusyMessage)
			return
		}
		defer func() { <-slots }()
		next.ServeHTTP(w, r)
	})
}

// KeyedLimiter limits the requests of the handlers it wraps, in total and
// for each key of a request.
type KeyedLimiter struct {
	total, perKey int
	key           func(*http.Request) string

	mu    sync.Mutex
	inUse int
	// keys counts the requests of each key; a key leaves it when its last
	// request ends.
	keys map[string]int
}

// NewKeyedLimiter returns a limiter of total requests at once, and of
// perKey requests at once with the same key. Requests with the empty key
// come from clients that can't be told apart, and count only toward the
// total.
func NewKeyedLimiter(total, perKey int, key func(*http.Request) string) *KeyedLimiter {
	return &KeyedLimiter{total: total, perKey: perKey, key: key, keys: map[string]int{}}
}

// Handler serves next within the limits, and answers requests over the
// limit of their key with 429 and others over the total with 503.
func (l *KeyedLimiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := l.key(r)
		if code := l.acquire(key); code != 0 {
			msg := BusyMessage
			if code == http.StatusTooManyRequests {
				msg = "too many requests at once from this client; try again later"
			}
			SetBusy(w.Header())
			jsonhttp.Error(w, code, msg)
			return
		}
		defer l.release(key)
		next.ServeHTTP(w, r)
	})
}

// acquire takes a slot for key, or returns the status of a refusal.
func (l *KeyedLimiter) acquire(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case key != "" && l.keys[key] >= l.perKey:
		return http.StatusTooManyRequests
	case l.inUse >= l.total:
		return http.StatusServiceUnavailable
	}
	l.inUse++
	if key != "" {
		l.keys[key]++
	}
	return 0
}

func (l *KeyedLimiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inUse--
	if key == "" {
		return
	}
	if l.keys[key]--; l.keys[key] == 0 {
		delete(l.keys, key)
	}
}

// streamChunk is the most bytes of a stream write that one write deadline
// covers, so that a large event can take as long as its client reads it.
const streamChunk = 64 << 10

// WriteTimeout bounds how long a client that doesn't read the response
// holds the request. A response gets total to be written whole. A response
// stream (text/event-stream) can stay open as long as its client reads it:
// each write gets streamIdle. Writes fail after their deadline, and the
// server closes the connection.
func WriteTimeout(total, streamIdle time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dw := &deadlineWriter{ResponseWriter: w, rc: http.NewResponseController(w), total: total, idle: streamIdle}
		next.ServeHTTP(dw, r)
		dw.start()
		if dw.stream {
			dw.setDeadline(time.Now().Add(dw.idle))
		}
		// The connection may serve the next request, so the deadline ends
		// once the rest of the response is sent within it.
		if err := dw.rc.Flush(); err != nil {
			slog.Debug("sending the response", "error", err)
			return
		}
		dw.setDeadline(time.Time{})
	})
}

// deadlineWriter sets the write deadlines of [WriteTimeout] as a response
// is written.
type deadlineWriter struct {
	http.ResponseWriter
	rc          *http.ResponseController
	total, idle time.Duration
	// started is set once the response starts, and stream if it is a
	// stream.
	started, stream bool
}

// start sets the deadline of a whole response when the response starts
// and isn't a stream.
func (d *deadlineWriter) start() {
	if d.started {
		return
	}
	d.started = true
	mediaType, _, err := mime.ParseMediaType(d.Header().Get("Content-Type"))
	d.stream = err == nil && mediaType == "text/event-stream"
	if !d.stream {
		d.setDeadline(time.Now().Add(d.total))
	}
}

func (d *deadlineWriter) setDeadline(t time.Time) {
	if err := d.rc.SetWriteDeadline(t); err != nil {
		slog.Debug("setting the write deadline of the response", "error", err)
	}
}

func (d *deadlineWriter) WriteHeader(code int) {
	if code >= http.StatusOK {
		d.start()
	}
	d.ResponseWriter.WriteHeader(code)
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	d.start()
	if !d.stream {
		return d.ResponseWriter.Write(p)
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), streamChunk)
		d.setDeadline(time.Now().Add(d.idle))
		m, err := d.ResponseWriter.Write(p[:n])
		written += m
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// FlushError sends the buffered response, as [http.ResponseController]
// does.
func (d *deadlineWriter) FlushError() error {
	d.start()
	if d.stream {
		d.setDeadline(time.Now().Add(d.idle))
	}
	return d.rc.Flush()
}

// Flush is [deadlineWriter.FlushError] for callers of [http.Flusher], such
// as the stream writers of a2a-go.
func (d *deadlineWriter) Flush() {
	if err := d.FlushError(); err != nil {
		slog.Debug("sending the response", "error", err)
	}
}

func (d *deadlineWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

// SetBusy sets the headers of a request refused for lack of room: try
// again in a second, on a new connection. Closing spares the server from
// draining a body the caller may never finish.
func SetBusy(h http.Header) {
	h.Set("Retry-After", "1")
	h.Set("Connection", "close")
}

// Share is how many requests a class of callers may make at once.
type Share struct {
	Total int
	// PerCaller is how many of those one caller may make; 0 sets no limit.
	PerCaller int
}

// Shares limits the requests in progress of each class of callers, such as
// a role, and of each caller in it. Classes without a share are left to
// the limit of the server.
type Shares struct {
	shares map[string]Share

	mu      sync.Mutex
	inUse   map[string]int
	callers map[shareCaller]int
}

// shareCaller is a caller of a class. A caller leaves [Shares] when its
// last request ends.
type shareCaller struct{ class, caller string }

// NewShares returns the shares of each class.
func NewShares(shares map[string]Share) *Shares {
	return &Shares{shares: shares, inUse: map[string]int{}, callers: map[shareCaller]int{}}
}

// Acquire takes a request of class's share for caller and returns the
// function that gives it back. When there is no room, it returns the
// status of the refusal: 429 when the caller's part of the share is in
// use, and 503 when the share is.
func (s *Shares) Acquire(class, caller string) (release func(), status int) {
	share, capped := s.shares[class]
	if !capped {
		return func() {}, 0
	}
	key := shareCaller{class: class, caller: caller}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case share.PerCaller > 0 && s.callers[key] >= share.PerCaller:
		return nil, http.StatusTooManyRequests
	case s.inUse[class] >= share.Total:
		return nil, http.StatusServiceUnavailable
	}
	s.inUse[class]++
	s.callers[key]++
	return sync.OnceFunc(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.inUse[class]--
		if s.callers[key]--; s.callers[key] == 0 {
			delete(s.callers, key)
		}
	}), 0
}

// RefusalMessage is the error of a request that [Shares.Acquire] refused
// with status.
func RefusalMessage(status int) string {
	if status == http.StatusTooManyRequests {
		return CallerBusyMessage
	}
	return BusyMessage
}

// Handler serves next within the share of class and its caller's part, and
// answers requests over them as [Shares.Acquire] refuses them.
func (s *Shares) Handler(class, caller string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, status := s.Acquire(class, caller)
		if status != 0 {
			SetBusy(w.Header())
			jsonhttp.Error(w, status, RefusalMessage(status))
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// PublicCORS lets pages of any origin read the public resource that next
// serves, and answers CORS preflights for it. It allows no credentials.
func PublicCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// a2a-go answers a request with an Origin by allowing that origin
		// with credentials, and one without it by allowing any origin.
		r = r.Clone(r.Context())
		r.Header.Del("Origin")
		next.ServeHTTP(w, r)
	})
}
