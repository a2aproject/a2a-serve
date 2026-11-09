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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/sdk/meshnet"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// inviteHashFile keeps a hash of the invite (--relay-token) the agent last
// joined the relay with.
const inviteHashFile = "invite-hash"

// setAsideCredentialFile keeps the mesh credential the agent had before it
// got a new invite, until the relay accepts or refuses that invite.
const setAsideCredentialFile = "set-aside-credential.json"

// inviteSwitch is a join with an invite other than the one the agent last
// joined with.
type inviteSwitch struct {
	stateDir string
	hash     string
	// recorded tells whether the agent recorded its last invite. Agents
	// that joined before a2a-serve recorded invites didn't.
	recorded bool
}

// useInvite prepares stateDir for a join with token. A new invite sets the
// saved mesh credential aside and keeps the identity key, so the agent
// enrolls again with the same peer ID. Routers keep refusing the credential
// of a removed agent after the relay lets it back, and meshnet resumes a
// saved credential before it looks at the token. useInvite returns nil
// for the invite the agent last joined with.
func useInvite(stateDir, token string) (*inviteSwitch, error) {
	sum := sha256.Sum256([]byte(token))
	sw := &inviteSwitch{stateDir: stateDir, hash: hex.EncodeToString(sum[:])}
	saved, err := os.ReadFile(filepath.Join(stateDir, inviteHashFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading the relay's last invite: %w", err)
	}
	sw.recorded = err == nil
	if sw.recorded && strings.TrimSpace(string(saved)) == sw.hash {
		return nil, nil
	}
	if err := setAsideMeshCredential(stateDir); err != nil {
		return nil, fmt.Errorf("setting aside the relay credential of the last invite: %w", err)
	}
	return sw, nil
}

// record keeps the invite as the one the agent last joined with.
func (sw *inviteSwitch) record() error {
	if err := os.MkdirAll(sw.stateDir, 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(sw.stateDir, inviteHashFile), []byte(sw.hash+"\n"), 0o600)
}

func setAsideMeshCredential(stateDir string) error {
	key, cred, err := meshnet.ReadStateDir(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(cred.GetBiscuit()) == 0 {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, meshnet.StateDirCredentialFile))
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(filepath.Join(stateDir, setAsideCredentialFile), raw, 0o600); err != nil {
		return err
	}
	return meshnet.WriteStateDir(stateDir, key, &api.MemberCredential{ControlPlaneUrl: cred.GetControlPlaneUrl()})
}

// restoreMeshCredential puts back the credential set aside for an invite,
// and reports whether there was one.
func restoreMeshCredential(stateDir string) (bool, error) {
	path := filepath.Join(stateDir, setAsideCredentialFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := fsutil.WriteAtomic(filepath.Join(stateDir, meshnet.StateDirCredentialFile), raw, 0o600); err != nil {
		return false, err
	}
	return true, removeFile(path)
}

// settleInvite ends the invite switch sw after the join srv tried. A join
// records the invite and drops the credential set aside for it. When the
// relay refuses the invite, the agent joins again with that credential,
// which it used before it got the invite, such as after a mistyped
// --relay-token. An agent that never recorded its invite adopts it then:
// it joined with that invite, which is used up.
func (a *agentMesh) settleInvite(ctx context.Context, sw *inviteSwitch, srv *meshnet.Server, st *meshnet.Status, err error) (*meshnet.Server, *meshnet.Status, error) {
	if err == nil {
		if sw != nil {
			if rerr := sw.record(); rerr != nil {
				slog.Warn("recording the relay invite", "error", rerr)
			}
		}
		if rerr := removeFile(filepath.Join(a.cfg.StateDir, setAsideCredentialFile)); rerr != nil {
			slog.Warn("removing the relay credential of the last invite", "error", rerr)
		}
		return srv, st, nil
	}
	var invite *InviteError
	if sw == nil || !errors.As(err, &invite) {
		return srv, st, err
	}
	restored, rerr := restoreMeshCredential(a.cfg.StateDir)
	if rerr != nil {
		return srv, st, errors.Join(err, fmt.Errorf("restoring the relay credential of the last invite: %w", rerr))
	}
	if !restored {
		return srv, st, err
	}
	if cerr := srv.Close(); cerr != nil {
		slog.Warn("closing the refused relay join", "error", cerr)
	}
	srv = a.newMeshServer()
	st, rerr = srv.Up(ctx)
	if rerr != nil {
		slog.Warn("joining the relay with the credential of the last invite", "error", rerr)
		return srv, st, err
	}
	if !sw.recorded {
		if rerr := errors.Join(sw.record(), RecordJoin(a.cfg.StateDir, nil)); rerr != nil {
			slog.Warn("recording the relay invite", "error", rerr)
		}
		return srv, st, nil
	}
	slog.Warn("the relay refused the new invite; joined with the credential of the last invite", "problem", invite.Problem)
	a.logf("! relay: %s; the agent joined with the credential of its last invite", invite.Problem)
	return srv, st, nil
}
