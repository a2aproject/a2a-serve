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
	"time"

	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

const (
	// MaxHeaderBytes bounds the request line and headers of a request.
	MaxHeaderBytes = 64 << 10
	// MaxConcurrent is how many requests one server handles at once,
	// including open streams.
	MaxConcurrent = 1024

	readHeaderTimeout = 30 * time.Second
	idleTimeout       = 2 * time.Minute
)

// New returns a server of h whose requests derive their context from ctx.
func New(ctx context.Context, h http.Handler) *http.Server {
	return &http.Server{
		Handler:           LimitConcurrency(MaxConcurrent, h),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
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
