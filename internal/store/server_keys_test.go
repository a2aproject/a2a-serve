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

package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestServerKeyStaysAcrossRestarts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "server.db")
	ctx := context.Background()
	first, err := openStoreAt(t, path).ServerKey(ctx, "callers")
	if err != nil {
		t.Fatalf("ServerKey() error = %v", err)
	}
	other, err := openStoreAt(t, path).ServerKey(ctx, "other")
	if err != nil {
		t.Fatalf("ServerKey() of another name error = %v", err)
	}

	again, err := openStoreAt(t, path).ServerKey(ctx, "callers")
	if err != nil {
		t.Fatalf("ServerKey() after a restart error = %v", err)
	}

	if len(first) != 32 || !bytes.Equal(first, again) {
		t.Fatalf("ServerKey() after a restart = %x, want %x", again, first)
	}
	if bytes.Equal(first, other) {
		t.Fatalf("ServerKey() of two names = %x, want different keys", first)
	}
}
