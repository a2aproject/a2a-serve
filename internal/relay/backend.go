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

// Package relay runs a relay: one a2a-serve process that serves the relay's
// public port, its control API and its sign-in, and runs a backend behind
// them, such as Agent Mesh.
package relay

import (
	"context"
	"net/url"
	"time"

	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/front"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// Backend is the network a relay serves behind its front, and the members
// it keeps.
type Backend interface {
	// Name says what the relay runs, such as Agent Mesh.
	Name() string
	// Version is the version of the backend the relay runs.
	Version() string
	// Prepare readies the backend's data in the relay directory for
	// Version, and reports the upgrade it made.
	Prepare(ctx context.Context, d relaydir.Dir, conf *relaydir.Resolved, now time.Time) (Prepared, error)
	// Info is what the front reports about the relay at its info path.
	Info(a2aServe string, signIn bool) any
	// Routes describes the backend's routes to the front.
	Routes() front.Routes
	// Start starts the backend and returns once it serves. It returns nil
	// and an error when ctx ends first.
	Start(ctx context.Context, s Start) (Running, error)
}

// Prepared reports an upgrade of a backend's data: the versions it went
// from and to, and the backup taken first. From is empty when nothing was
// upgraded.
type Prepared struct {
	From, To, Backup string
}

// Start is what a backend starts with.
type Start struct {
	Dir    relaydir.Dir
	Config *relaydir.Resolved
	// URL is the relay's public URL.
	URL string
	// Issuer is the URL of the relay's identity provider, or "" when
	// agents can't sign in.
	Issuer string
	// Status returns the static status of the relay.
	Status func() control.Status
	// Admits says which members that signed in may stay.
	Admits control.Admits
}

// Running is a started backend.
type Running interface {
	// Upstream is where the front proxies the backend's routes.
	Upstream() *url.URL
	// Service implements the control API.
	Service() control.Service
	// Reconcile makes the members that signed in match the allowlist, and
	// returns the ones it removed and let back.
	Reconcile(ctx context.Context) (removed, readmitted []control.Member, err error)
	// Exited is closed when the backend stops on its own.
	Exited() <-chan struct{}
	// Err says why the backend stopped once Exited is closed.
	Err() error
	// Stop stops the backend and waits for it.
	Stop() error
}
