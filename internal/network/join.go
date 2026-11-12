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

package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// TokenStep is what the user runs after getting a new network token.
const TokenStep = "run `a2a-serve config set --network-token -` and `a2a-serve restart`"

// TokenError is a join the network refused because of the agent's network
// token (--network-token).
type TokenError struct {
	// Problem says what is wrong with the token.
	Problem string
	// Fix is the next step, which ends with [TokenStep].
	Fix string
}

func (e *TokenError) Error() string { return e.Problem + "; " + e.Fix }

// tokenFix returns the next step after a refused token: how to get a new
// one, as the provider says, and then [TokenStep].
func tokenFix(howToGet string) string {
	if howToGet == "" {
		return "get a new network token, then " + TokenStep
	}
	return howToGet + ", then " + TokenStep
}

// tokenProblemFile keeps the problem of the token of the agent's last
// join, for doctor. legacyInviteProblemFile kept it as text before network
// providers.
const (
	tokenProblemFile        = "token-problem.json"
	legacyInviteProblemFile = "invite-problem"
)

// legacyInviteFix was the next step of the problems in
// legacyInviteProblemFile, which only Agent Mesh relays wrote.
const legacyInviteFix = "ask the relay's admin for a new invite (`a2a-serve relay invite <name>`), then " + TokenStep

type tokenProblem struct {
	Problem string `json:"problem"`
	Fix     string `json:"fix"`
}

// RecordJoin records the outcome of a join of the network whose state is
// in stateDir: it keeps the problem of a refused token, and forgets it
// after a join that succeeded. Other failures leave it as it is.
func RecordJoin(stateDir string, err error) error {
	path := filepath.Join(stateDir, tokenProblemFile)
	var te *TokenError
	switch {
	case err == nil:
		return errors.Join(removeIfExists(path), removeIfExists(filepath.Join(stateDir, legacyInviteProblemFile)))
	case errors.As(err, &te):
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return err
		}
		data, err := json.Marshal(tokenProblem{Problem: te.Problem, Fix: te.Fix})
		if err != nil {
			return err
		}
		return fsutil.WriteAtomic(path, data, 0o600)
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// TokenProblem returns the problem the network found with the agent's
// token at its last join, and the fix, or "" when there was none. stateDir
// is where the network's state is.
func TokenProblem(stateDir string) (problem, fix string, err error) {
	data, err := os.ReadFile(filepath.Join(stateDir, tokenProblemFile))
	if errors.Is(err, os.ErrNotExist) {
		legacy, lerr := os.ReadFile(filepath.Join(stateDir, legacyInviteProblemFile))
		if errors.Is(lerr, os.ErrNotExist) {
			return "", "", nil
		}
		if lerr != nil {
			return "", "", fmt.Errorf("reading the network's last join: %w", lerr)
		}
		return strings.TrimSpace(string(legacy)), legacyInviteFix, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("reading the network's last join: %w", err)
	}
	var p tokenProblem
	if err := json.Unmarshal(data, &p); err != nil {
		return "", "", fmt.Errorf("reading the network's last join: %w", err)
	}
	return p.Problem, p.Fix, nil
}

// ErrPeerUnavailable is returned for a call to an agent on the private
// network which doesn't answer, such as one that is stopped or restarting.
var ErrPeerUnavailable = errors.New("the agent doesn't answer on the network: it is stopped or restarting; try again later, or check `a2a-serve peers`")

// checkPeerResponse turns the plain 502 and 503 responses with which a
// network reports an agent that doesn't answer into [ErrPeerUnavailable].
// Agent servers answer errors in JSON, which pass.
func checkPeerResponse(resp *http.Response, err error) (*http.Response, error) {
	if err != nil || (resp.StatusCode != http.StatusBadGateway && resp.StatusCode != http.StatusServiceUnavailable) {
		return resp, err
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/json" {
		return resp, nil
	}
	if err := resp.Body.Close(); err != nil {
		slog.Debug("closing a network response", "error", err)
	}
	return nil, ErrPeerUnavailable
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
