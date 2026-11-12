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

package slimbackend

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/relay"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// slimctl returns the pinned slimctl, downloading it into the user cache on
// the first run.
func slimctl(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs a real SLIM node")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := NewFetcher(filepath.Join(cache, "a2a-serve"), os.Getenv(BinaryEnv), http.DefaultClient, runtime.GOOS, runtime.GOARCH, t.Logf).Get(t.Context())
	if err != nil {
		t.Fatalf("getting slimctl: %v", err)
	}
	if err := CheckBinary(t.Context(), bin); err != nil {
		t.Fatalf("CheckBinary() error = %v", err)
	}
	return bin
}

func TestStartRunsTheNodeBehindTheGate(t *testing.T) {
	bin := slimctl(t)
	d := relaydir.Dir{Root: testutil.ShortTempDir(t)}
	if _, err := d.SaveSettings(relaydir.Settings{URL: testRelay, Backend: relaydir.BackendSLIM}); err != nil {
		t.Fatal(err)
	}
	conf, err := d.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	b := New(Options{Binary: bin, Log: io.Discard, Now: time.Now})

	r, err := b.Start(t.Context(), relay.Start{Dir: d, Config: conf, URL: conf.URL, Status: func() control.Status { return control.Status{URL: testRelay} }})
	if err != nil {
		t.Fatalf("Backend.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := r.Stop(); err != nil {
			t.Errorf("Running.Stop() error = %v", err)
		}
	})

	inv, err := r.Service().Invite(t.Context(), control.InviteRequest{Name: "coder"})
	if err != nil {
		t.Fatalf("Service.Invite() error = %v", err)
	}
	pub, priv := newKey(t)
	if _, err := r.(*running).svc.store.enroll(inv.Token, pub); err != nil {
		t.Fatalf("store.enroll() error = %v", err)
	}

	c, _, status := dialNode(t, r.Upstream().Host, memberTokenAt(t, priv, "coder", time.Now()))

	if status != http.StatusSwitchingProtocols {
		t.Fatalf("connecting to the node through the gate got status %d, want %d", status, http.StatusSwitchingProtocols)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
