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

//go:build agentmesh

package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/google/agentmesh/sdk/meshnet"
)

// routersRefused starts the error of a join no router of the relay
// admitted. Routers refuse a revoked credential without saying why.
const routersRefused = "failed to authenticate with any router"

// signInAgain retries the failed join err of a signed-in agent that resumed
// its saved credential: it sets the credential aside, keeps the identity
// key, and enrolls again with the agent's sign-in. Routers keep refusing
// the credential of a removed agent for a day after the relay lets it back,
// and meshnet resumes a saved credential before it signs in. When the relay
// doesn't enroll the agent again, it keeps its saved credential.
func (a *agentMesh) signInAgain(ctx context.Context, resumed bool, srv meshServer, st *meshnet.Status, err error) (meshServer, *meshnet.Status, error) {
	if a.cfg.Token != "" {
		return srv, st, err
	}
	if !resumed {
		// A join after a start that stopped while signing in again.
		if _, rerr := restoreMeshCredential(a.cfg.StateDir); rerr != nil {
			return srv, st, errors.Join(err, fmt.Errorf("restoring the saved relay credential: %w", rerr))
		}
		return srv, st, err
	}
	if !strings.Contains(err.Error(), routersRefused) {
		return srv, st, err
	}
	if rerr := setAsideMeshCredential(a.cfg.StateDir); rerr != nil {
		return srv, st, errors.Join(err, fmt.Errorf("setting aside the refused relay credential: %w", rerr))
	}
	if cerr := srv.Close(); cerr != nil {
		slog.Warn("closing the refused relay join", "error", cerr)
	}
	slog.Warn("no router admitted the saved relay credential; signing in again", "error", err)
	a.logf("! relay: no router admitted the agent's saved credential; signing in again")
	srv = a.newMeshServer()
	st, rerr := srv.Up(ctx)
	if rerr == nil {
		if rerr := removeFile(filepath.Join(a.cfg.StateDir, setAsideCredentialFile)); rerr != nil {
			slog.Warn("removing the refused relay credential", "error", rerr)
		}
		return srv, st, nil
	}
	slog.Warn("signing in to the relay again", "error", rerr)
	err = errors.Join(err, fmt.Errorf("signing in again: %w", explainJoin(rerr)))
	// The relay renews only the newest credential it gave the agent, so a
	// sign-in that enrolled keeps the new credential though the join failed.
	if hasMeshIdentity(a.cfg.StateDir) {
		if xerr := removeFile(filepath.Join(a.cfg.StateDir, setAsideCredentialFile)); xerr != nil {
			slog.Warn("removing the refused relay credential", "error", xerr)
		}
		return srv, st, err
	}
	if _, xerr := restoreMeshCredential(a.cfg.StateDir); xerr != nil {
		return srv, st, errors.Join(err, fmt.Errorf("restoring the saved relay credential: %w", xerr))
	}
	return srv, st, err
}
