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
	"errors"
	"fmt"
	"regexp"

	slim_bindings "github.com/agntcy/slim-bindings-go/v3"

	"github.com/a2aproject/a2a-serve/netprovider"
)

// identity is how the agent proves who it is on the node, and how it reads
// who its callers are.
type identity interface {
	createApp(svc *slim_bindings.Service, n *slim_bindings.Name) (*slim_bindings.App, error)
	// callerName turns the verified subject of a caller into its name.
	callerName(subject string) string
}

// minSecretLen is the shortest shared secret SLIM accepts, in bytes.
const minSecretLen = 32

const secretFix = "pass the SLIM node's shared secret, at least 32 bytes long, with --network-token"

func newIdentity(token string) (identity, error) {
	if token == "" {
		return nil, netprovider.TokenError("the SLIM network needs the node's shared secret as the network token", secretFix)
	}
	if len(token) < minSecretLen {
		return nil, netprovider.TokenError(fmt.Sprintf("the network token is %d bytes long; SLIM needs a shared secret of at least %d bytes", len(token), minSecretLen), secretFix)
	}
	return sharedSecret(token), nil
}

// sharedSecret is the identity of a secret every agent on the node holds.
// Anyone with the secret can claim any name.
type sharedSecret string

func (s sharedSecret) createApp(svc *slim_bindings.Service, n *slim_bindings.Name) (*slim_bindings.App, error) {
	app, err := svc.CreateAppWithSecret(n, string(s))
	if errors.Is(err, slim_bindings.ErrSlimErrorAuthError) {
		return nil, netprovider.TokenError("SLIM refused the network token as a shared secret: "+err.Error(), secretFix)
	}
	return app, err
}

// secretSuffix is what SLIM appends to an app's name to make the subject of
// its shared-secret identity unique.
var secretSuffix = regexp.MustCompile(`^(.+)_[A-Za-z0-9]{8}$`)

func (sharedSecret) callerName(subject string) string {
	if m := secretSuffix.FindStringSubmatch(subject); m != nil {
		return m[1]
	}
	return subject
}
