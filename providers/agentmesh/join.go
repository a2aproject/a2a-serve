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

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/agentmesh/sdk/meshnet"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/netprovider"
)

// routersRefused starts the error of a join no router of the relay
// admitted. Routers refuse a revoked credential without saying why.
const routersRefused = "failed to authenticate with any router"

// join is one join of the relay with req.
type join struct {
	req    netprovider.JoinRequest
	notice func(string)
	serve  func(*meshnet.Server) meshServer
}

func (j *join) logf(format string, args ...any) {
	j.notice(fmt.Sprintf(format, args...))
}

func hasMeshIdentity(stateDir string) bool {
	_, cred, err := meshnet.ReadStateDir(stateDir)
	return err == nil && len(cred.GetBiscuit()) > 0
}

// savedPeerID returns the peer ID of the mesh identity in stateDir, or "".
func savedPeerID(stateDir string) string {
	key, _, err := meshnet.ReadStateDir(stateDir)
	if err != nil {
		return ""
	}
	priv, err := crypto.UnmarshalPrivateKey(key)
	if err != nil {
		return ""
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return ""
	}
	return id.String()
}

func (j *join) newMeshServer() meshServer {
	srv := &meshnet.Server{
		StateDir:        j.req.StateDir,
		ControlPlaneURL: j.req.URL,
		BootstrapToken:  j.req.Token,
		AllowInsecure:   agentmesh.InsecureLoopback(j.req.URL),
		OnCredentialLapse: func(err error) {
			slog.Warn("relay credential lapsed; retrying", "error", err)
			j.logf("! relay: credential lapsed (%v)", err)
		},
	}
	// Without a token, the agent signs in with an account; the first run
	// prints the link, later runs use the stored identity.
	if j.req.Token == "" {
		login := &relayLogin{RelayURL: j.req.URL, StateDir: j.req.StateDir, HTTP: &http.Client{Timeout: 30 * time.Second}, Logf: j.logf, Poll: time.Second}
		srv.TokenSource = login.Token
	}
	return j.serve(srv)
}

// up joins the relay. It returns a token error for a refused invite and a
// removed error for an agent the relay removed.
func (j *join) up(ctx context.Context) (meshServer, *meshnet.Status, error) {
	var invite *inviteSwitch
	if j.req.Token != "" {
		var err error
		if invite, err = useInvite(j.req.StateDir, j.req.Token); err != nil {
			return nil, nil, fmt.Errorf("joining the relay %s: %w", j.req.URL, err)
		}
	}
	resumed := hasMeshIdentity(j.req.StateDir)
	if j.req.Token == "" {
		refresh, err := savedRefreshToken(j.req.StateDir)
		if err != nil {
			slog.Warn("reading the relay's refresh token", "error", err)
		}
		if !resumed && refresh.Token == "" {
			j.logf("… joining the relay %s; this needs a sign-in on the first run (or --relay-token)", j.req.URL)
		}
	}
	srv := j.newMeshServer()
	st, err := srv.Up(ctx)
	err = explainJoin(err)
	srv, st, err = j.settleInvite(ctx, invite, srv, st, err)
	if err != nil {
		if peerID := savedPeerID(j.req.StateDir); peerID != "" {
			if rerr := relayRemoval(ctx, &http.Client{Timeout: 10 * time.Second}, j.req, peerID); rerr != nil {
				closeServer(srv)
				return nil, nil, netprovider.RemovedError(rerr.Error(), "")
			}
		}
		srv, st, err = j.signInAgain(ctx, resumed, srv, st, err)
	}
	if err == nil {
		return srv, st, nil
	}
	var refused *inviteError
	if errors.As(err, &refused) {
		closeServer(srv)
		return nil, nil, netprovider.TokenError(refused.Problem, fmt.Sprintf("ask the relay's admin for a new invite (`a2a-serve relay invite %s`)", j.req.Agent))
	}
	return nil, nil, errors.Join(fmt.Errorf("joining the relay %s: %w", j.req.URL, err), srv.Close())
}

// closeServer closes a server whose join failed, for errors that report
// only why it failed.
func closeServer(srv meshServer) {
	if err := srv.Close(); err != nil {
		slog.Warn("closing the failed relay join", "error", err)
	}
}

// signInAgain retries the failed join err of a signed-in agent that resumed
// its saved credential: it sets the credential aside, keeps the identity
// key, and enrolls again with the agent's sign-in. Routers keep refusing
// the credential of a removed agent for a day after the relay lets it back,
// and meshnet resumes a saved credential before it signs in. When the relay
// doesn't enroll the agent again, it keeps its saved credential.
func (j *join) signInAgain(ctx context.Context, resumed bool, srv meshServer, st *meshnet.Status, err error) (meshServer, *meshnet.Status, error) {
	if j.req.Token != "" {
		return srv, st, err
	}
	if !resumed {
		// A join after a start that stopped while signing in again.
		if _, rerr := restoreMeshCredential(j.req.StateDir); rerr != nil {
			return srv, st, errors.Join(err, fmt.Errorf("restoring the saved relay credential: %w", rerr))
		}
		return srv, st, err
	}
	if !strings.Contains(err.Error(), routersRefused) {
		return srv, st, err
	}
	if rerr := setAsideMeshCredential(j.req.StateDir); rerr != nil {
		return srv, st, errors.Join(err, fmt.Errorf("setting aside the refused relay credential: %w", rerr))
	}
	if cerr := srv.Close(); cerr != nil {
		slog.Warn("closing the refused relay join", "error", cerr)
	}
	slog.Warn("no router admitted the saved relay credential; signing in again", "error", err)
	j.logf("! relay: no router admitted the agent's saved credential; signing in again")
	srv = j.newMeshServer()
	st, rerr := srv.Up(ctx)
	if rerr == nil {
		if rerr := removeFile(filepath.Join(j.req.StateDir, setAsideCredentialFile)); rerr != nil {
			slog.Warn("removing the refused relay credential", "error", rerr)
		}
		return srv, st, nil
	}
	slog.Warn("signing in to the relay again", "error", rerr)
	err = errors.Join(err, fmt.Errorf("signing in again: %w", explainJoin(rerr)))
	// The relay renews only the newest credential it gave the agent, so a
	// sign-in that enrolled keeps the new credential though the join failed.
	if hasMeshIdentity(j.req.StateDir) {
		if xerr := removeFile(filepath.Join(j.req.StateDir, setAsideCredentialFile)); xerr != nil {
			slog.Warn("removing the refused relay credential", "error", xerr)
		}
		return srv, st, err
	}
	if _, xerr := restoreMeshCredential(j.req.StateDir); xerr != nil {
		return srv, st, errors.Join(err, fmt.Errorf("restoring the saved relay credential: %w", xerr))
	}
	return srv, st, err
}
