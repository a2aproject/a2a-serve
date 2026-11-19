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

package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
)

// contextsPool is the pool of the agent processes of A2A contexts.
const contextsPool = "contexts"

// maxContextID bounds the length of an A2A context ID.
const maxContextID = 256

// contexts runs each A2A context in an agent process of its own, whose key
// is the context ID, so that what the process does can be told apart by
// context.
type contexts struct {
	// base is the environment of every process, which each gets with its
	// own name.
	base a2aserve.Environment
}

func (c contexts) Key(_ context.Context, contextID string) (string, error) {
	if err := checkContextID(contextID); err != nil {
		return "", err
	}
	return contextID, nil
}

func (c contexts) Environment(_ context.Context, key string) (a2aserve.Environment, error) {
	env := c.base
	sum := sha256.Sum256([]byte(key))
	env.Name += "-" + hex.EncodeToString(sum[:6])
	env.Pool = contextsPool
	return env, nil
}

func (c contexts) Admit(context.Context, string, *a2asrv.ExecutorContext) (func(), error) {
	return func() {}, nil
}

// checkContextID refuses a context ID that is empty, too long, or has
// characters other than printable ASCII without spaces.
func checkContextID(id string) error {
	if id == "" || len(id) > maxContextID {
		return fmt.Errorf("invalid context ID: want 1 to %d characters", maxContextID)
	}
	for i := range len(id) {
		if id[i] <= ' ' || id[i] > '~' {
			return fmt.Errorf("invalid context ID %q: want printable ASCII characters without spaces", id)
		}
	}
	return nil
}
