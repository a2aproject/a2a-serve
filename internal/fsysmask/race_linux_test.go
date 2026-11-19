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

package fsysmask_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// TestMaskHoldsWhileAParentTurnsIntoASymlink swaps the directory p with a
// symlink to .a2a, as an agent with a symlink already in its tree could,
// while requests go through p. None may reach .a2a.
func TestMaskHoldsWhileAParentTurnsIntoASymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for p, data := range map[string]string{".a2a/secret": "SECRET", "p/secret": "public"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(".a2a", filepath.Join(root, "swap")); err != nil {
		t.Fatal(err)
	}
	c := serveMasked(t, fsys.Dir(root))

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !stop.Load() {
			if err := unix.Renameat2(unix.AT_FDCWD, filepath.Join(root, "p"), unix.AT_FDCWD, filepath.Join(root, "swap"), unix.RENAME_EXCHANGE); err != nil {
				t.Errorf("exchanging p and the symlink error = %v", err)
				return
			}
		}
	})
	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		// The requests fail whenever they meet the symlink; only what
		// lands in .a2a matters.
		if _, err := c.Write(ctx, "p/secret", strings.NewReader("overwritten"), 0o644, ""); err != nil {
			t.Logf("write %d: %v", i, err)
		}
		if rc, err := c.Open(ctx, "p/secret"); err == nil {
			data, rerr := io.ReadAll(rc)
			if cerr := rc.Close(); cerr != nil {
				t.Logf("close %d: %v", i, cerr)
			}
			if rerr == nil && string(data) == "SECRET" {
				stop.Store(true)
				wg.Wait()
				t.Fatalf("Client.Open(p/secret) read .a2a/secret while p was a symlink")
			}
		}
		if err := c.Remove(ctx, "p/secret"); err != nil {
			t.Logf("remove %d: %v", i, err)
		}
	}
	stop.Store(true)
	wg.Wait()

	entries, err := os.ReadDir(filepath.Join(root, ".a2a"))
	if err != nil {
		t.Fatalf("os.ReadDir(.a2a) error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret" {
		t.Fatalf("os.ReadDir(.a2a) = %v, want only secret", entries)
	}
	if got, err := os.ReadFile(filepath.Join(root, ".a2a", "secret")); err != nil || string(got) != "SECRET" {
		t.Fatalf("the host's .a2a/secret = %q, %v, want it unchanged", got, err)
	}
}
