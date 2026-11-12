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

package sandbox

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

// TestLauncherCopyRunsAsAnotherUser checks the modes that let container
// root, without CAP_DAC_OVERRIDE and so as another user than the owner,
// run the launcher on rootful Docker, while the sockets stay private.
func TestLauncherCopyRunsAsAnotherUser(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("the launcher is copied on Linux only")
	}
	dir := filepath.Join(t.TempDir(), "harness")
	b := NewBoundaries(dir, 12, "abc", &sandboxnet.Blocks{})
	launcher := b.Launcher()
	if launcher == "" {
		t.Fatalf("Boundaries.Launcher() = \"\", want a copy")
	}
	for _, p := range []string{launcher, filepath.Dir(launcher)} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o005 != 0o005 {
			t.Fatalf("mode of %s = %v, want others to read and execute it", p, perm)
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("mode of the socket directory %s = %v, want it private", dir, perm)
	}
	if got := b.Launcher(); got != launcher {
		t.Fatalf("second Boundaries.Launcher() = %q, want the same copy %q", got, launcher)
	}
}

func TestBoundariesOpener(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "sockets")
	b := NewBoundaries(dir, 12, "abc", &sandboxnet.Blocks{})
	t.Cleanup(func() {
		if err := b.Close(context.Background()); err != nil {
			t.Errorf("Boundaries.Close() error = %v", err)
		}
	})
	open := b.Opener("a2a-coder-1", BoundaryRules{Policy: func() egress.Policy { return egress.Policy{Mode: egress.ModeAllowlist} }})
	ep, err := open(func(context.Context, uint16) (netip.AddrPort, bool) { return netip.AddrPort{}, false }, false)
	if err != nil {
		t.Fatalf("OpenEgress() error = %v", err)
	}
	sockDir := filepath.Dir(ep.Socket)
	if name := filepath.Base(sockDir); filepath.Dir(sockDir) != dir || len(name) != 12 || strings.Trim(name, "abc") != "" {
		t.Fatalf("OpenEgress().Socket = %q, want it alone in a directory of %s named with 12 of abc", ep.Socket, dir)
	}
	if _, err := os.Stat(ep.Socket); err != nil {
		t.Fatalf("egress socket error = %v", err)
	}
	if err := ep.Close(); err != nil {
		t.Fatalf("EgressEndpoint.Close() error = %v", err)
	}
	if _, err := os.Stat(sockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket directory after EgressEndpoint.Close() error = %v, want %v", err, os.ErrNotExist)
	}
}
