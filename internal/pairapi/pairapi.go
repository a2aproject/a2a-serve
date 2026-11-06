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

// Package pairapi is the pairing endpoint of an agent server, POST
// <agent URL>/pair, which exchanges a one-time code for a token.
package pairapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// Path is the path of the pairing endpoint under the agent URL.
const Path = "/pair"

// Request is the body of a pairing request.
type Request struct {
	Code string `json:"code"`
	// Kind is what the redeemer is, one of the Pairing* kinds. It sets the
	// kind of a pairing redeemed for the first time.
	Kind string `json:"kind,omitempty"`
}

// Kinds of pairings.
const (
	PairingPerson  = "person"
	PairingDevice  = "device"
	PairingAgent   = "agent"
	PairingWebhook = "webhook"
)

// PairingKinds are the kinds of pairings, in the order they are listed.
var PairingKinds = []string{PairingPerson, PairingDevice, PairingAgent, PairingWebhook}

// Kinds of tokens a pairing returns.
const (
	KindCaller = "caller"
	KindNode   = "node"
)

// Response is returned for a valid code. The token is shown once. URL is
// where the token applies: the agent for callers, the node for nodes.
type Response struct {
	Kind      string `json:"kind"`
	Token     string `json:"token"`
	Principal string `json:"principal"`
	Agent     string `json:"agent"`
	URL       string `json:"url"`
}

// maxBackoffWait bounds the backoff Pair waits out before it tries once
// more. The agent backs off after invalid codes, including those of other
// callers behind the same address or relay.
const maxBackoffWait = 15 * time.Second

// Pair exchanges code at the agent at agentURL for a token, as a redeemer
// of kind. It waits out a short backoff of the agent once.
func Pair(ctx context.Context, hc *http.Client, agentURL, code, kind string) (Response, error) {
	out, wait, err := pair(ctx, hc, agentURL, code, kind)
	if err == nil || wait < 0 {
		return out, err
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return Response{}, err
	case <-t.C:
	}
	out, _, err = pair(ctx, hc, agentURL, code, kind)
	return out, err
}

// pair makes one pairing request. When the agent backs off for at most
// maxBackoffWait, it returns how long to wait, and -1 otherwise.
func pair(ctx context.Context, hc *http.Client, agentURL, code, kind string) (Response, time.Duration, error) {
	agentURL = strings.TrimSuffix(agentURL, "/")
	body, err := json.Marshal(Request{Code: code, Kind: kind})
	if err != nil {
		return Response{}, -1, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL+Path, bytes.NewReader(body))
	if err != nil {
		return Response{}, -1, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return Response{}, -1, err
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := errors.Join(readErr, resp.Body.Close()); err != nil {
		return Response{}, -1, err
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, retryAfter(resp), fmt.Errorf("pairing with %s: %s", agentURL, jsonhttp.ErrorMessage(resp.Status, data))
	}
	var out Response
	if err := json.Unmarshal(data, &out); err != nil {
		return Response{}, -1, fmt.Errorf("pairing with %s: %w", agentURL, err)
	}
	return out, -1, nil
}

// retryAfter returns the backoff of a 429 response when it is at most
// maxBackoffWait, or -1.
func retryAfter(resp *http.Response) time.Duration {
	if resp.StatusCode != http.StatusTooManyRequests {
		return -1
	}
	n, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || n < 0 || n > int(maxBackoffWait/time.Second) {
		return -1
	}
	return time.Duration(n) * time.Second
}
