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

package agentsrv

import (
	"context"
	"io"
	"net/http"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

const (
	// maxGuestBodies is how many bytes of guest request bodies a listener
	// buffers at once: a body of the largest size for each guest task that
	// may run.
	maxGuestBodies = guestConcurrentTasks * jsonhttp.MaxBody
	// guestBodyChunk is what a body without a Content-Length is charged at
	// a time.
	guestBodyChunk = 1 << 20
)

// guestBodies limits the bytes of guest request bodies that a listener
// buffers at once. The guest task budget applies only once a body is read,
// so without it guests could make the server hold any number of bodies.
type guestBodies struct {
	sem  *semaphore.Weighted
	wait time.Duration
}

// newGuestBodies returns a budget of size bytes, for which a request waits
// up to wait.
func newGuestBodies(size int64, wait time.Duration) *guestBodies {
	return &guestBodies{sem: semaphore.NewWeighted(size), wait: wait}
}

// guestBodiesBusyError is the error of a guest's body that the listener
// couldn't buffer in time.
type guestBodiesBusyError struct {
	retryAfter time.Duration
}

func (e *guestBodiesBusyError) Error() string { return errGuestsBusy.Error() }

// handler charges the body of a guest's request until next returns: its
// Content-Length before next runs, or each chunk of a body without one as
// next reads it. A request that the listener can't buffer gets 503.
func (g *guestBodies) handler(p a2aProtocol, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, _ := auth.PrincipalFrom(r.Context())
		// jsonhttp.Limit wraps every body, so an empty one isn't http.NoBody.
		if caller.Role != auth.RoleGuest || r.ContentLength == 0 {
			next.ServeHTTP(w, r)
			return
		}
		body := &guestBody{ReadCloser: r.Body, ctx: r.Context(), g: g, chunked: r.ContentLength < 0}
		defer body.release()
		if !body.chunked {
			if err := body.charge(r.ContentLength); err != nil {
				p.busy(w, err)
				return
			}
		}
		r.Body = body
		next.ServeHTTP(w, r)
	})
}

// guestBody is a request body charged to [guestBodies]. It is read and
// released by the goroutine of the handler.
type guestBody struct {
	io.ReadCloser
	ctx     context.Context
	g       *guestBodies
	chunked bool
	read    int64
	charged int64
}

func (b *guestBody) Read(p []byte) (int, error) {
	if b.chunked && len(p) > 0 {
		if b.read == b.charged {
			// One byte tells the end of the body from more of it, which
			// costs another chunk.
			n, err := b.ReadCloser.Read(p[:1])
			if n == 0 {
				return 0, err
			}
			if err := b.charge(guestBodyChunk); err != nil {
				return 0, err
			}
			b.read += int64(n)
			return n, err
		}
		p = p[:min(int64(len(p)), b.charged-b.read)]
	}
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	return n, err
}

func (b *guestBody) charge(n int64) *guestBodiesBusyError {
	ctx, cancel := context.WithTimeout(b.ctx, b.g.wait)
	defer cancel()
	if err := b.g.sem.Acquire(ctx, n); err != nil {
		return &guestBodiesBusyError{retryAfter: b.g.wait}
	}
	b.charged += n
	return nil
}

func (b *guestBody) release() {
	b.g.sem.Release(b.charged)
	b.charged = 0
}
