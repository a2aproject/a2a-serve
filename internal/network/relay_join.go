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

// InviteFix is the next step after the relay refused an agent's invite.
const InviteFix = "ask the relay's admin for a new invite (`a2a-serve relay invite <name>`), then run `a2a-serve config set --relay-token -` and `a2a-serve restart`"

// InviteError is a join the relay refused because of the agent's invite
// (--relay-token).
type InviteError struct {
	// Problem says what is wrong with the invite.
	Problem string
}

func (e *InviteError) Error() string { return e.Problem + "; " + InviteFix }

// inviteProblems map the enrollment errors of Agent Mesh to what they mean
// for an invite. A longer error comes before the errors it contains.
var inviteProblems = []struct{ mesh, problem string }{
	{"Bootstrap token expired, revoked or exhausted", "the invite expired, was revoked or was already used"},
	{"Bootstrap token max usages exceeded", "the invite was already used"},
	{"Bootstrap token expired", "the invite expired"},
	{"Bootstrap token revoked", "the invite was revoked"},
	{"Bootstrap token owner is banned", "the relay removed the member who created the invite"},
	{"Invalid bootstrap token", "the relay doesn't know this invite; check that it was copied whole"},
}

// explainJoin returns an [InviteError] for a join refused because of the
// invite, and err otherwise.
func explainJoin(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, p := range inviteProblems {
		if strings.Contains(msg, p.mesh) {
			return &InviteError{Problem: p.problem}
		}
	}
	return err
}

// inviteProblemFile keeps the problem of the invite of the agent's last
// join, for doctor.
const inviteProblemFile = "invite-problem"

// RecordJoin records the outcome of a join of the relay whose state is in
// stateDir: it keeps the problem of a refused invite, and forgets it after
// a join that succeeded. Other failures leave it as it is.
func RecordJoin(stateDir string, err error) error {
	path := filepath.Join(stateDir, inviteProblemFile)
	var invite *InviteError
	switch {
	case err == nil:
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	case errors.As(err, &invite):
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return err
		}
		return fsutil.WriteAtomic(path, []byte(invite.Problem), 0o600)
	}
	return nil
}

// InviteProblem returns the problem of the invite the relay refused at the
// last join of the agent whose relay state is in stateDir, or "".
func InviteProblem(stateDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, inviteProblemFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the relay's last join: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ErrPeerUnavailable is returned for a call to an agent on the relay which
// doesn't answer, such as one that is stopped or restarting.
var ErrPeerUnavailable = errors.New("the agent doesn't answer on the relay: it is stopped or restarting; try again later, or check `a2a-serve peers`")

// checkPeerResponse turns the plain 502 and 503 responses with which the
// relay reports an agent that doesn't answer into [ErrPeerUnavailable].
// Agent servers answer errors in JSON, which pass.
func checkPeerResponse(resp *http.Response, err error) (*http.Response, error) {
	if err != nil || (resp.StatusCode != http.StatusBadGateway && resp.StatusCode != http.StatusServiceUnavailable) {
		return resp, err
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/json" {
		return resp, nil
	}
	if err := resp.Body.Close(); err != nil {
		slog.Debug("closing a relay response", "error", err)
	}
	return nil, ErrPeerUnavailable
}
