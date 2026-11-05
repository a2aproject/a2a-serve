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
			jsonhttp.Error(w, http.StatusServiceUnavailable, "the server is busy; try again later")
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
// perKey requests at once with the same key.
func NewKeyedLimiter(total, perKey int, key func(*http.Request) string) *KeyedLimiter {
	return &KeyedLimiter{total: total, perKey: perKey, key: key, keys: map[string]int{}}
}

// Handler serves next within the limits, and answers requests over the
// limit of their key with 429 and others over the total with 503.
func (l *KeyedLimiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := l.key(r)
		if code := l.acquire(key); code != 0 {
			msg := "the server is busy; try again later"
			if code == http.StatusTooManyRequests {
				msg = "too many requests at once from this client; try again later"
			}
			w.Header().Set("Retry-After", "1")
			// Closing spares the server from draining a body the caller
			// may never finish, which no deadline bounds yet.
			w.Header().Set("Connection", "close")
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
	case l.keys[key] >= l.perKey:
		return http.StatusTooManyRequests
	case l.inUse >= l.total:
		return http.StatusServiceUnavailable
	}
	l.inUse++
	l.keys[key]++
	return 0
}

func (l *KeyedLimiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inUse--
	if l.keys[key]--; l.keys[key] == 0 {
		delete(l.keys, key)
	}
}
