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

package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/backoff"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/ctlhttp"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// RemotePath is where a relay serves the control API at its URL.
const RemotePath = "/a2a-serve/control"

// maxRequest bounds a control request body.
const maxRequest = 64 << 10

// Handler serves the operations of svc. restart, when set, serves the
// restart that `relay upgrade` asks a local relay for.
func Handler(svc *Service, restart func()) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		st, err := svc.Status(r.Context())
		respond(w, http.StatusOK, st, err)
	})
	mux.HandleFunc("POST /invites", func(w http.ResponseWriter, r *http.Request) {
		var req InviteRequest
		if !read(w, r, &req) {
			return
		}
		inv, err := svc.Invite(r.Context(), req)
		respond(w, http.StatusCreated, inv, err)
	})
	mux.HandleFunc("GET /members", func(w http.ResponseWriter, r *http.Request) {
		banned, _ := strconv.ParseBool(r.URL.Query().Get("banned"))
		ms, err := svc.Members(r.Context(), banned)
		respond(w, http.StatusOK, ms, err)
	})
	mux.HandleFunc("POST /members/remove", func(w http.ResponseWriter, r *http.Request) {
		var req RemoveRequest
		if !read(w, r, &req) {
			return
		}
		res, err := svc.Remove(r.Context(), req)
		respond(w, http.StatusOK, res, err)
	})
	if restart != nil {
		mux.HandleFunc("POST /restart", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			restart()
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Error(w, http.StatusNotFound, "no such relay control call")
	})
	return mux
}

func read(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest)).Decode(v); err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func respond(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		slog.Debug("relay control call failed", "error", err)
		ctlhttp.WriteError(w, err)
		return
	}
	jsonhttp.Write(w, status, v)
}

// RequireToken lets through only requests that carry token as a bearer
// token. Callers reach a relay through its proxy or tunnel, so they share
// one backoff after failures. The backoff delays wrong tokens only, so
// nobody can lock the operator out; the admin token is long enough that
// guessing it at any rate fails.
func RequireToken(token string, failures *backoff.Failures, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if ok && subtle.ConstantTimeCompare(sum[:], want[:]) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if wait := failures.Wait(); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			jsonhttp.Error(w, http.StatusTooManyRequests, "too many failed attempts; try again later")
			return
		}
		failures.Fail()
		jsonhttp.Error(w, http.StatusUnauthorized, "the relay's control API needs its admin token")
	})
}

// ErrNotRunning is returned when no relay answers on the control socket.
var ErrNotRunning error = clierr.NotRunning(errors.New("the relay is not running; start it with `a2a-serve relay run`"))

// Client calls the control API of a relay.
type Client struct {
	base   string
	token  string
	socket bool
	hc     *http.Client
}

// NewSocketClient returns a client of the local relay whose control socket
// is at path.
func NewSocketClient(path string, timeout time.Duration) *Client {
	return &Client{base: "http://relay", socket: true, hc: &http.Client{Timeout: timeout, Transport: ctlhttp.SocketTransport(path)}}
}

// NewRemoteClient returns a client of the relay at relayURL, which
// authenticates with its admin token. A nil transport is the default one.
func NewRemoteClient(relayURL, adminToken string, timeout time.Duration, transport http.RoundTripper) *Client {
	return &Client{
		base:  strings.TrimSuffix(relayURL, "/") + RemotePath,
		token: adminToken,
		hc:    &http.Client{Timeout: timeout, Transport: transport},
	}
}

// Status returns the relay's status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var out Status
	return &out, c.do(ctx, http.MethodGet, "/status", nil, &out)
}

// Invite mints an invite.
func (c *Client) Invite(ctx context.Context, req InviteRequest) (*Invite, error) {
	var out Invite
	return &out, c.do(ctx, http.MethodPost, "/invites", req, &out)
}

// Members lists the relay's members.
func (c *Client) Members(ctx context.Context, banned bool) ([]Member, error) {
	var out []Member
	return out, c.do(ctx, http.MethodGet, "/members?banned="+strconv.FormatBool(banned), nil, &out)
}

// Remove bans members.
func (c *Client) Remove(ctx context.Context, req RemoveRequest) (*RemoveResponse, error) {
	var out RemoveResponse
	return &out, c.do(ctx, http.MethodPost, "/members/remove", req, &out)
}

// Restart asks a local relay to restart, which upgrades it to the Agent
// Mesh version of the a2a-serve binary on disk.
func (c *Client) Restart(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/restart", nil, nil)
}

// maxResponse bounds what the client reads of a response.
const maxResponse = 32 << 20

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if c.socket && ctlhttp.IsDialError(err) {
			return ErrNotRunning
		}
		return err
	}
	return c.read(resp, out)
}

func (c *Client) read(resp *http.Response, out any) error {
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing a relay control response", "error", err)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return errors.New("no relay control API at " + c.base + "; is this an a2a-serve relay?")
	}
	if resp.StatusCode/100 != 2 {
		return ctlhttp.ResponseError(resp.Status, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("reading the relay's answer: %w", err)
	}
	return nil
}
