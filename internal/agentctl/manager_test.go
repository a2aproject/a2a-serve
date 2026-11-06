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

package agentctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// holdLock locks the agent directory in d as a starting server does.
func holdLock(t *testing.T, d agentdir.Dir) {
	t.Helper()
	if err := d.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	release, err := fsutil.TryLock(d.LockPath())
	if err != nil {
		t.Fatalf("fsutil.TryLock() error = %v", err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release() error = %v", err)
		}
	})
}

func TestOpen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		serving     bool
		locked      bool
		wantRunning bool
		wantErr     error
	}{
		{name: "stopped"},
		{name: "running", serving: true, locked: true, wantRunning: true},
		{name: "starting", locked: true, wantErr: &agenterr.BusyError{Name: "coder"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
			if tc.serving {
				serveFake(t, d, &fakeServer{})
			}
			if tc.locked {
				holdLock(t, d)
			}

			// Open waits for the lock until the deadline, which leaves time
			// for the status call under the load of a parallel test run.
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			m, closer, err := Open(ctx, d, controlapi.NewClient(d))

			if diff := cmp.Diff(tc.wantErr, err, cmp.Comparer(func(a, b error) bool { return a.Error() == b.Error() })); diff != "" {
				t.Fatalf("Open() wrong error (-want +got) diff = %s", diff)
			}
			if err != nil {
				return
			}
			if got := m.Running(); got != tc.wantRunning {
				t.Fatalf("Open().Running() = %v, want %v", got, tc.wantRunning)
			}
			if err := closer.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		})
	}
}

func TestOpenLocksStoppedAgent(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	_, closer, err := Open(t.Context(), d, controlapi.NewClient(d))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	_, lockErr := fsutil.TryLock(d.LockPath())
	if err := closer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	release, unlockedErr := fsutil.TryLock(d.LockPath())
	if unlockedErr == nil {
		if err := release(); err != nil {
			t.Fatalf("release() error = %v", err)
		}
	}

	if !errors.Is(lockErr, fsutil.ErrLocked) {
		t.Fatalf("fsutil.TryLock() while open error = %v, want %v", lockErr, fsutil.ErrLocked)
	}
	if unlockedErr != nil {
		t.Fatalf("fsutil.TryLock() after Close() error = %v, want nil", unlockedErr)
	}
}

func TestOpenRemovesStalePIDFile(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	if err := d.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := os.WriteFile(d.PIDPath(), []byte("12345"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(PIDPath) error = %v", err)
	}

	_, closer, err := Open(t.Context(), d, controlapi.NewClient(d))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	_, statErr := os.Stat(d.PIDPath())
	if err := closer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("os.Stat(PIDPath) while open error = %v, want %v", statErr, os.ErrNotExist)
	}
}

func TestSetWhileStarting(t *testing.T) {
	t.Parallel()
	want := &agentdir.Config{Name: "coder", Harness: "opencode"}
	d := newAgent(t, want, nil)
	holdLock(t, d)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	_, err := Set(ctx, d, controlapi.NewClient(d), agentdir.Settings{Sandbox: "local"}, false)

	var busy *agenterr.BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("Set() error = %v, want a BusyError", err)
	}
	if diff := cmp.Diff(want, loadConfig(t, d)); diff != "" {
		t.Fatalf("Set() saved the config (-want +got) diff = %s", diff)
	}
}

func TestOfflineDeleteMessenger(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		kind    string
		wantErr error
	}{
		{name: "configured", kind: "telegram"},
		{name: "not configured", kind: "discord", wantErr: &agenterr.NotFoundError{Kind: "messenger", Name: "discord"}},
		{name: "unknown kind", kind: "fax", wantErr: agenterr.Invalidf(`unknown messenger "fax": want telegram, discord or email`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode"}
			if err := cfg.SetMessenger("telegram", "secret:TELEGRAM_TOKEN", nil); err != nil {
				t.Fatal(err)
			}
			d := newAgent(t, cfg, nil)
			m, closer, err := Open(t.Context(), d, controlapi.NewClient(d))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			t.Cleanup(func() {
				if err := closer.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			})

			err = m.DeleteMessenger(t.Context(), tc.kind)

			if diff := cmp.Diff(tc.wantErr, err, cmp.Comparer(func(a, b error) bool { return fmt.Sprintf("%T %v", a, a) == fmt.Sprintf("%T %v", b, b) })); diff != "" {
				t.Fatalf("DeleteMessenger() wrong error (-want +got) diff = %s", diff)
			}
			if got := loadConfig(t, d).HasMessenger("telegram"); got != (tc.kind != "telegram") {
				t.Fatalf("config.yaml has telegram = %v after DeleteMessenger(%s)", got, tc.kind)
			}
		})
	}
}
