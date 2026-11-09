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
	"bytes"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/errordetails"

	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// a2aProtocol is the transport of an A2A route. It decides the shape of
// the errors the route answers before a2a-go handles the request.
type a2aProtocol int

const (
	protocolJSONRPC a2aProtocol = iota
	protocolREST
)

// protocolOf returns the transport of a request to an A2A route: JSON-RPC
// is served at the root.
func protocolOf(r *http.Request) a2aProtocol {
	if r.Method == http.MethodPost && r.URL.Path == "/" {
		return protocolJSONRPC
	}
	return protocolREST
}

// a2aCodes are the JSON-RPC code and the google.rpc.Status name of the
// errors the A2A routes answer themselves, as a2a-go maps them.
var a2aCodes = map[error]struct {
	jsonrpc int
	status  string
}{
	a2a.ErrUnauthenticated: {jsonrpc: -31401, status: "UNAUTHENTICATED"},
	a2a.ErrUnauthorized:    {jsonrpc: -31403, status: "PERMISSION_DENIED"},
	a2a.ErrInvalidRequest:  {jsonrpc: -32600, status: "INVALID_ARGUMENT"},
	a2a.ErrInternalError:   {jsonrpc: -32603, status: "INTERNAL"},
}

type rpcErrorResponse struct {
	JSONRPC string   `json:"jsonrpc"`
	ID      any      `json:"id"`
	Error   rpcError `json:"error"`
}

type rpcError struct {
	Code    int                   `json:"code"`
	Message string                `json:"message"`
	Data    []*errordetails.Typed `json:"data"`
}

type restErrorResponse struct {
	Error restError `json:"error"`
}

type restError struct {
	Code    int                   `json:"code"`
	Status  string                `json:"status"`
	Message string                `json:"message"`
	Details []*errordetails.Typed `json:"details"`
}

// writeError answers with status and an A2A error of kind, one of
// [a2aCodes], so that A2A clients show msg.
func (p a2aProtocol) writeError(w http.ResponseWriter, status int, kind error, msg string) {
	codes := a2aCodes[kind]
	info := []*errordetails.Typed{errordetails.NewErrorInfo(a2a.ErrorReason(kind), a2a.ProtocolDomain, nil)}
	if p == protocolJSONRPC {
		jsonhttp.Write(w, status, rpcErrorResponse{JSONRPC: "2.0", Error: rpcError{Code: codes.jsonrpc, Message: msg, Data: info}})
		return
	}
	jsonhttp.Write(w, status, restErrorResponse{Error: restError{Code: status, Status: codes.status, Message: msg, Details: info}})
}

// tooLarge answers a request body over the limit.
func (p a2aProtocol) tooLarge(w http.ResponseWriter) {
	p.writeError(w, http.StatusRequestEntityTooLarge, a2a.ErrInvalidRequest, "request body too large")
}

// busy answers a request that waited too long for the server to take it.
func (p a2aProtocol) busy(w http.ResponseWriter, err *guestBodiesBusyError) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(err.retryAfter.Seconds())))))
	p.writeError(w, http.StatusServiceUnavailable, a2a.ErrInternalError, err.Error())
}

// readBody reads the whole request body before next, so that a body over
// the limit gets 413 also without a Content-Length, which a2a-go would
// answer as a parse error.
func (p a2aProtocol) readBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		var tooLarge *http.MaxBytesError
		var busy *guestBodiesBusyError
		switch {
		case errors.As(err, &tooLarge):
			p.tooLarge(w)
			return
		case errors.As(err, &busy):
			p.busy(w, busy)
			return
		case err != nil:
			slog.Debug("reading an A2A request body", "error", err)
			p.writeError(w, http.StatusBadRequest, a2a.ErrInvalidRequest, "reading the request body failed")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}
