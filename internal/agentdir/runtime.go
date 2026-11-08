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
	"maps"
	"sync"
	"sync/atomic"
)

// Runtime is the config and secrets an agent runs with, and the agent
// directory they are saved in. The config differs from config.yaml by the
// changes waiting for a restart. Changes replace the config and secrets
// whole, so readers never see a partial update.
type Runtime struct {
	dir Dir
	// mu serializes changes of cur.
	mu  sync.Mutex
	cur atomic.Pointer[runtimeState]
}

type runtimeState struct {
	cfg     *Config
	secrets Secrets
}

// NewRuntime returns the runtime of the agent in d, which runs with cfg and
// secrets.
func NewRuntime(d Dir, cfg *Config, secrets Secrets) *Runtime {
	r := &Runtime{dir: d}
	r.cur.Store(&runtimeState{cfg: cfg, secrets: secrets})
	return r
}

// Config returns a copy of the config in use.
func (r *Runtime) Config() *Config {
	c := *r.cur.Load().cfg
	return &c
}

// Secrets are the secrets in use. Callers must not change them.
func (r *Runtime) Secrets() Secrets { return r.cur.Load().secrets }

// Update applies fn to the config in use and to config.yaml, which may
// hold changes waiting for a restart. A config.yaml that fails to parse is
// left alone and returned as the error.
func (r *Runtime) Update(fn func(*Config) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.cur.Load()
	next := *cur.cfg
	if err := fn(&next); err != nil {
		return err
	}
	if err := r.dir.UpdateConfig(fn); err != nil {
		return err
	}
	r.cur.Store(&runtimeState{cfg: &next, secrets: cur.secrets})
	return nil
}

// UpdateSandbox applies fn to config.yaml, then uses its sandbox rules,
// which fn may change. The grants of secrets that the agent loads at
// restart wait for it, and the rules in use leave them out until then.
func (r *Runtime) UpdateSandbox(fn func(*Config) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var saved []SandboxRule
	if err := r.dir.UpdateConfig(func(c *Config) error {
		if err := fn(c); err != nil {
			return err
		}
		saved = cloneSandbox(c.Policy.Sandbox)
		return nil
	}); err != nil {
		return err
	}
	cur := r.cur.Load()
	next := *cur.cfg
	next.Policy.Sandbox = loadedRules(saved, next.Secrets)
	r.cur.Store(&runtimeState{cfg: &next, secrets: cur.secrets})
	return nil
}

// Reload applies the live settings of config.yaml, which [CopyLive]
// copies, to the config in use.
func (r *Runtime) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	saved, err := r.dir.LoadConfig()
	if err != nil {
		return err
	}
	cur := r.cur.Load()
	next := *cur.cfg
	CopyLive(&next, saved)
	r.cur.Store(&runtimeState{cfg: &next, secrets: cur.secrets})
	return nil
}

// SavedConfig reads config.yaml, which can hold changes waiting for a
// restart.
func (r *Runtime) SavedConfig() (*Config, error) { return r.dir.LoadConfig() }

// SavedSecrets reads the secrets file, which can hold secrets saved since
// the agent started.
func (r *Runtime) SavedSecrets() (Secrets, error) { return r.dir.LoadSecrets() }

// SaveSecrets merges s into the secrets file, and uses all the secrets it
// holds then.
func (r *Runtime) SaveSecrets(s Secrets) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all Secrets
	if err := r.dir.LockConfig(func() error {
		if err := r.dir.SaveSecrets(s); err != nil {
			return err
		}
		var err error
		all, err = r.dir.LoadSecrets()
		return err
	}); err != nil {
		return err
	}
	r.cur.Store(&runtimeState{cfg: r.cur.Load().cfg, secrets: all})
	return nil
}

// ReplaceSecret stores a secret that replaced the one in use, such as a
// token a messenger refreshed, and uses it from now on.
func (r *Runtime) ReplaceSecret(name, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	update := Secrets{name: value}
	if err := r.dir.LockConfig(func() error { return r.dir.SaveSecrets(update) }); err != nil {
		return err
	}
	cur := r.cur.Load()
	next := Secrets{}
	maps.Copy(next, cur.secrets)
	maps.Copy(next, update)
	r.cur.Store(&runtimeState{cfg: cur.cfg, secrets: next})
	return nil
}
