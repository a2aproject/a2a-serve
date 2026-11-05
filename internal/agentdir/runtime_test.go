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

package agentdir

import (
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func newRuntime(t *testing.T, cfg *Config) (*Runtime, Dir) {
	t.Helper()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := d.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	return NewRuntime(d, cfg.Clone(), Secrets{}), d
}

func TestRuntimeUpdateKeepsChangesWaitingForRestart(t *testing.T) {
	t.Parallel()
	rt, d := newRuntime(t, &Config{Name: "coder", Harness: "opencode"})
	if err := d.UpdateConfig(func(c *Config) error { c.Harness = "claude"; return nil }); err != nil {
		t.Fatalf("UpdateConfig() error = %v", err)
	}

	if err := rt.Update(func(c *Config) error { c.Description = "reviews code"; return nil }); err != nil {
		t.Fatalf("Runtime.Update() error = %v", err)
	}

	saved, err := d.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	cur := rt.Config()
	want := [][2]string{{"claude", "reviews code"}, {"opencode", "reviews code"}}
	if diff := cmp.Diff(want, [][2]string{{saved.Harness, saved.Description}, {cur.Harness, cur.Description}}); diff != "" {
		t.Fatalf("Runtime.Update() wrong saved and running config (-want +got) diff = %s", diff)
	}
}

func TestRuntimeUpdateKeepsUnparsableConfigFile(t *testing.T) {
	t.Parallel()
	rt, d := newRuntime(t, &Config{Name: "coder", Harness: "opencode"})
	broken := []byte("name: [unterminated\n")
	if err := os.WriteFile(d.ConfigPath(), broken, 0o644); err != nil {
		t.Fatal(err)
	}

	err := rt.Update(func(c *Config) error {
		c.Description = "changed"
		return nil
	})

	if err == nil {
		t.Fatalf("Runtime.Update() error = nil, want a parse error")
	}
	got, readErr := os.ReadFile(d.ConfigPath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if diff := cmp.Diff(string(broken), string(got)); diff != "" {
		t.Fatalf("Runtime.Update() overwrote config.yaml (-want +got) diff = %s", diff)
	}
	if got := rt.Config().Description; got == "changed" {
		t.Fatalf("Runtime.Update() applied the change to the running config despite the error")
	}
}

func TestRuntimeUpdateWaitsForConfigLock(t *testing.T) {
	t.Parallel()
	rt, d := newRuntime(t, &Config{Name: "coder", Harness: "opencode"})
	locked := make(chan struct{})
	unlock := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- d.LockConfig(func() error {
			close(locked)
			<-unlock
			return nil
		})
	}()
	<-locked
	done := make(chan error, 1)
	go func() {
		done <- rt.Update(func(c *Config) error {
			c.Description = "changed"
			return nil
		})
	}()

	select {
	case err := <-done:
		t.Fatalf("Runtime.Update() returned while the config was locked, error = %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(unlock)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Runtime.Update() error = %v", err)
	}
}

func TestRuntimeReloadAppliesOnlyLiveSettings(t *testing.T) {
	t.Parallel()
	rt, d := newRuntime(t, &Config{Name: "coder", Harness: "opencode"})
	if err := d.UpdateConfig(func(c *Config) error {
		c.Description, c.Harness = "reviews code", "claude"
		return nil
	}); err != nil {
		t.Fatalf("UpdateConfig() error = %v", err)
	}

	if err := rt.Reload(); err != nil {
		t.Fatalf("Runtime.Reload() error = %v", err)
	}

	cur := rt.Config()
	if diff := cmp.Diff([2]string{"reviews code", "opencode"}, [2]string{cur.Description, cur.Harness}); diff != "" {
		t.Fatalf("Runtime.Reload() wrong running config (-want +got) diff = %s", diff)
	}
}

func TestRuntimeSaveSecrets(t *testing.T) {
	t.Parallel()
	rt, d := newRuntime(t, &Config{Name: "coder", Harness: "opencode"})
	if err := d.SaveSecrets(Secrets{"SAVED": "1"}); err != nil {
		t.Fatalf("SaveSecrets() error = %v", err)
	}

	if err := rt.SaveSecrets(Secrets{"TOKEN": "2"}); err != nil {
		t.Fatalf("Runtime.SaveSecrets() error = %v", err)
	}

	if diff := cmp.Diff(Secrets{"SAVED": "1", "TOKEN": "2"}, rt.Secrets()); diff != "" {
		t.Fatalf("Runtime.Secrets() wrong result (-want +got) diff = %s", diff)
	}
}
