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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// a2aErrorBody is what a test checks of an A2A error response.
type a2aErrorBody struct {
	Status  int
	Code    int
	Reason  string
	Message string
}

// readA2AError decodes the error body of resp as JSON-RPC when jsonrpc is
// set, and as a REST google.rpc.Status otherwise.
func readA2AError(t *testing.T, resp *http.Response, jsonrpc bool) a2aErrorBody {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body error = %v", err)
	}
	type errorInfo struct {
		Reason string `json:"reason"`
	}
	var body struct {
		JSONRPC string `json:"jsonrpc"`
		Error   struct {
			Code    int         `json:"code"`
			Message string      `json:"message"`
			Data    []errorInfo `json:"data"`
			Details []errorInfo `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decoding %s error = %v", data, err)
	}
	info := body.Error.Details
	if jsonrpc {
		if body.JSONRPC != "2.0" {
			t.Fatalf("body %s isn't a JSON-RPC response", data)
		}
		info = body.Error.Data
	}
	got := a2aErrorBody{Status: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
	if len(info) > 0 {
		got.Reason = info[len(info)-1].Reason
	}
	return got
}

func TestA2AErrorBodies(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	owner := "Bearer " + ts.pair("me", auth.RoleOwner)
	tooLarge := `{"padding":"` + strings.Repeat("a", jsonhttp.MaxBody) + `"}`
	tests := []struct {
		name      string
		auth      string
		body      string
		chunked   bool
		wantRPC   a2aErrorBody
		wantREST  a2aErrorBody
		wantInMsg string
	}{
		{
			name:      "no token",
			body:      listTasksRPC,
			wantRPC:   a2aErrorBody{Status: http.StatusUnauthorized, Code: -31401, Reason: "UNAUTHENTICATED"},
			wantREST:  a2aErrorBody{Status: http.StatusUnauthorized, Code: http.StatusUnauthorized, Reason: "UNAUTHENTICATED"},
			wantInMsg: "this agent needs a token",
		},
		{
			name:      "unknown token",
			auth:      "Bearer " + auth.TokenPrefix + "revoked",
			body:      listTasksRPC,
			wantRPC:   a2aErrorBody{Status: http.StatusUnauthorized, Code: -31401, Reason: "UNAUTHENTICATED"},
			wantREST:  a2aErrorBody{Status: http.StatusUnauthorized, Code: http.StatusUnauthorized, Reason: "UNAUTHENTICATED"},
			wantInMsg: "this agent doesn't recognize the token, which may be mistyped, revoked or replaced; ask the agent's owner for a pairing code",
		},
		{
			name:      "body too large",
			auth:      owner,
			body:      tooLarge,
			wantRPC:   a2aErrorBody{Status: http.StatusRequestEntityTooLarge, Code: -32600, Reason: "INVALID_REQUEST"},
			wantREST:  a2aErrorBody{Status: http.StatusRequestEntityTooLarge, Code: http.StatusRequestEntityTooLarge, Reason: "INVALID_REQUEST"},
			wantInMsg: "request body too large",
		},
		{
			name:      "chunked body too large",
			auth:      owner,
			body:      tooLarge,
			chunked:   true,
			wantRPC:   a2aErrorBody{Status: http.StatusRequestEntityTooLarge, Code: -32600, Reason: "INVALID_REQUEST"},
			wantREST:  a2aErrorBody{Status: http.StatusRequestEntityTooLarge, Code: http.StatusRequestEntityTooLarge, Reason: "INVALID_REQUEST"},
			wantInMsg: "request body too large",
		},
	}
	for _, tc := range tests {
		for _, jsonrpc := range []bool{true, false} {
			path, want := "/message:send", tc.wantREST
			if jsonrpc {
				path, want = "/", tc.wantRPC
			}
			t.Run(tc.name+" "+path, func(t *testing.T) {
				t.Parallel()
				var body io.Reader = strings.NewReader(tc.body)
				if tc.chunked {
					body = io.MultiReader(body)
				}
				req, err := http.NewRequest(http.MethodPost, ts.localURL()+path, body)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Expect", "100-continue")
				if tc.auth != "" {
					req.Header.Set("Authorization", tc.auth)
				}
				resp, err := expectContinueHTTP.Do(req)
				if err != nil {
					t.Fatalf("POST %s error = %v", path, err)
				}
				defer func() {
					if err := resp.Body.Close(); err != nil {
						t.Errorf("closing the body: %v", err)
					}
				}()

				got := readA2AError(t, resp, jsonrpc)

				if !strings.Contains(got.Message, tc.wantInMsg) {
					t.Fatalf("POST %s message = %q, want it to contain %q", path, got.Message, tc.wantInMsg)
				}
				got.Message = ""
				if diff := cmp.Diff(want, got); diff != "" {
					t.Fatalf("POST %s wrong result (-want +got) diff = %s", path, diff)
				}
			})
		}
	}
}

func TestClientSeesUnauthorizedHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ts := startServer(t, serverOpts{})
	hc := contacts.TokenClient(testHTTP, store.Contact{URL: ts.localURL(), Token: auth.TokenPrefix + "revoked"})
	card, err := agentcard.NewResolver(hc).Resolve(ctx, ts.localURL())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	tests := []struct {
		name      string
		transport a2aclient.FactoryOption
		call      func(*a2aclient.Client) error
	}{
		{
			name:      "REST",
			transport: a2aclient.WithRESTTransport(hc),
			call: func(c *a2aclient.Client) error {
				_, err := c.ListTasks(ctx, &a2a.ListTasksRequest{})
				return err
			},
		},
		{
			name:      "JSON-RPC",
			transport: a2aclient.WithJSONRPCTransport(hc),
			call: func(c *a2aclient.Client) error {
				_, err := c.ListTasks(ctx, &a2a.ListTasksRequest{})
				return err
			},
		},
		{
			name:      "JSON-RPC stream",
			transport: a2aclient.WithJSONRPCTransport(hc),
			call: func(c *a2aclient.Client) error {
				for _, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", "", "")}) {
					if err != nil {
						return err
					}
				}
				return nil
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithDefaultsDisabled(), tc.transport)
			if err != nil {
				t.Fatalf("NewFromCard() error = %v", err)
			}
			t.Cleanup(func() {
				if err := c.Destroy(); err != nil {
					t.Errorf("Destroy() error = %v", err)
				}
			})

			err = tc.call(c)

			if !errors.Is(err, a2a.ErrUnauthenticated) || !strings.Contains(err.Error(), "this agent doesn't recognize the token") {
				t.Fatalf("client call error = %v, want %v with the server's hint", err, a2a.ErrUnauthenticated)
			}
		})
	}
}
