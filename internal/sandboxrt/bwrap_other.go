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

//go:build !linux

package sandboxrt

import (
	"context"
	"errors"
)

// Bubblewrap is the backend of bubblewrap sandboxes, which run only on
// Linux.
type Bubblewrap struct{}

var _ Backend = (*Bubblewrap)(nil)

// NewBubblewrap returns the bubblewrap backend, which fails on this
// system.
func NewBubblewrap(BubblewrapConfig) *Bubblewrap { return &Bubblewrap{} }

var errNoBubblewrap = &BubblewrapError{Problem: "runs only on Linux", Fix: "use the Docker sandbox", Err: errors.ErrUnsupported}

// Probe implements [Backend].
func (*Bubblewrap) Probe(context.Context) (Capabilities, error) {
	return Capabilities{}, errNoBubblewrap
}

// Prepare implements [Backend].
func (*Bubblewrap) Prepare(context.Context, Spec) (Sandbox, error) { return nil, errNoBubblewrap }
