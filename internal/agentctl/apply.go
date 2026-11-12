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

// Package agentctl changes the settings of an agent, applying them to the
// running server where it can, and restarts the agent.
package agentctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/project"
)

// Secret actions of a [SecretChange].
const (
	SecretStored  = "stored"
	SecretRotated = "rotated"
	SecretDeleted = "deleted"
)

// SecretChange is a change of a stored secret value.
type SecretChange struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

// Result describes a change of settings.
type Result struct {
	Name    string            `json:"name"`
	Path    string            `json:"path"`
	Changes []agentdir.Change `json:"changes"`
	Secrets []SecretChange    `json:"secrets,omitempty"`
	DryRun  bool              `json:"dryRun,omitempty"`
	Created bool              `json:"created,omitempty"`
	Running bool              `json:"running"`
	// Applied are the settings the running server applied.
	Applied []string `json:"applied,omitempty"`
	// Restart are the settings which wait for a restart of the running server.
	Restart []string `json:"restart,omitempty"`
	// Access is who may call the agent with the saved settings, set when
	// that changed.
	Access *controlapi.Access `json:"access,omitempty"`
	// Warnings are problems of the saved settings that don't stop the agent.
	Warnings []string `json:"warnings,omitempty"`
}

// Empty reports whether nothing changed.
func (r *Result) Empty() bool { return len(r.Changes) == 0 && len(r.Secrets) == 0 }

// Mutation changes cfg and returns the secrets to store and the names of
// the secrets to delete.
type Mutation func(cfg *agentdir.Config) (store agentdir.Secrets, del []string, err error)

// Step is a write of [ApplyThen] made after config.yaml is saved, such as
// files of the agent's workspace that belong with its settings. It gets
// the saved config.
type Step func(cfg *agentdir.Config) error

// Set merges settings into the agent's config.
func Set(ctx context.Context, d agentdir.Dir, client *controlapi.Client, s agentdir.Settings, dryRun bool) (*Result, error) {
	return Apply(ctx, d, client, func(cfg *agentdir.Config) (agentdir.Secrets, []string, error) {
		old := cfg.Clone()
		store, err := s.Apply(cfg, d)
		return store, agentdir.DroppedSecrets(old, cfg), err
	}, dryRun)
}

// Unset removes entries of a list setting, or resets a setting.
func Unset(ctx context.Context, d agentdir.Dir, client *controlapi.Client, setting string, entries []string, dryRun bool) (*Result, error) {
	return Apply(ctx, d, client, func(cfg *agentdir.Config) (agentdir.Secrets, []string, error) {
		del, err := d.Unset(cfg, setting, entries)
		return nil, del, err
	}, dryRun)
}

// errMessengers refuses an edit of config.yaml that changes messengers,
// which `a2a-serve messenger add` checks with the messenger first.
var errMessengers = agenterr.WithHint(errors.New("messengers are changed with a2a-serve messenger add and a2a-serve messenger rm"), agenterr.HintMessengerList, "")

// ErrConfigChanged refuses to save an edit of config.yaml when the file
// changed since the edit read it.
var ErrConfigChanged = errors.New("config.yaml changed")

// Replace replaces the agent's config with data, the content of
// config.yaml as edited by hand, which is saved as it is. Values typed into
// secret settings are stored, and replaced by references in the saved
// file. A name that isn't a lowercase DNS label is normalized, as `config
// set --name` does, in the saved file too. A change of messengers is refused.
// If config.yaml no longer holds base, the content the edit started from,
// nothing is saved and the error is [ErrConfigChanged].
func Replace(ctx context.Context, d agentdir.Dir, client *controlapi.Client, data, base []byte, dryRun bool) (*Result, error) {
	next, err := agentdir.ParseConfig(data)
	if err != nil {
		return nil, &agentdir.SettingsError{Err: agentdir.FileProblems(err)}
	}
	if name := agentdir.SanitizeName(next.Name); next.Name != "" && name != next.Name {
		next.Name = name
		if data, err = agentdir.MergeConfig(data, next); err != nil {
			return nil, err
		}
	}
	return apply(ctx, d, client, func(cfg *agentdir.Config) (agentdir.Secrets, []string, error) {
		if agentdir.MessengersDiffer(cfg, next) {
			return nil, nil, errMessengers
		}
		store, err := agentdir.StoreSecretValues(cfg, next)
		if err != nil {
			return nil, nil, agentdir.FileProblems(err)
		}
		*cfg = *next
		return store, nil, nil
	}, nil, dryRun, &rawEdit{data: data, base: base})
}

// rawEdit is a hand edit of config.yaml: data is saved as it is if the
// file still holds base.
type rawEdit struct{ data, base []byte }

// Apply changes the agent's config with mutate, validates the result and
// saves it. A running server applies messengers, the description and
// approval settings at once, through client; the result lists the settings
// which wait for a restart. The directory of a stopped agent stays locked
// against a start while Apply saves. Invalid settings are a
// [agentdir.SettingsError] and nothing is saved.
func Apply(ctx context.Context, d agentdir.Dir, client *controlapi.Client, mutate Mutation, dryRun bool) (*Result, error) {
	return apply(ctx, d, client, mutate, nil, dryRun, nil)
}

// ApplyThen is [Apply], running then after config.yaml is saved and before
// a running server applies the live settings. The directory of a stopped
// agent stays locked while then runs. A dry run skips it.
func ApplyThen(ctx context.Context, d agentdir.Dir, client *controlapi.Client, mutate Mutation, then Step, dryRun bool) (*Result, error) {
	return apply(ctx, d, client, mutate, then, dryRun, nil)
}

// apply is [ApplyThen], saving the hand edit raw as config.yaml if set.
func apply(ctx context.Context, d agentdir.Dir, client *controlapi.Client, mutate Mutation, then Step, dryRun bool, raw *rawEdit) (_ *Result, err error) {
	created := !d.Exists()
	if created {
		if err := CheckEnclosing(ctx, d); err != nil {
			return nil, err
		}
	}
	old, err := d.LoadOrNewConfig()
	var se *agentdir.SettingsError
	if raw != nil && errors.As(err, &se) {
		// An edit repairs a config.yaml that doesn't parse, and is
		// compared against an empty config.
		old, err = &agentdir.Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	next := old.Clone()
	store, del, err := mutate(next)
	if err != nil {
		return nil, &agentdir.SettingsError{Err: err}
	}
	stored, err := d.LoadSecrets()
	if err != nil {
		return nil, err
	}
	all := maps.Clone(stored)
	maps.Copy(all, store)
	for _, name := range del {
		delete(all, name)
	}
	if err := d.CheckConfig(next, all); err != nil {
		return nil, &agentdir.SettingsError{Err: fileProblems(d, err, old, stored, created, raw != nil)}
	}
	if next.Approvals.OwnerChat != old.Approvals.OwnerChat {
		if err := next.CheckOwnerChat(); err != nil {
			return nil, &agentdir.SettingsError{Err: err}
		}
	}
	if raw != nil && len(store) > 0 {
		// The edited file keeps the references to the stored values.
		if raw.data, err = agentdir.MergeConfig(raw.data, next); err != nil {
			return nil, err
		}
	}
	res := &Result{
		Name: next.Name, Path: d.ConfigPath(), Changes: agentdir.Diff(old, next),
		Secrets: secretChanges(stored, store, del), DryRun: dryRun, Created: created,
		Warnings: append(harness.Warnings(next), mcpWarnings(d, old, next)...),
	}
	if old == nil || old.Sandbox != next.Sandbox || old.Egress.Mode != next.Egress.Mode || !slices.Equal(old.Egress.Allow, next.Egress.Allow) || !slices.Equal(old.Inject, next.Inject) {
		if w := harness.HostEgress(next).Warning; w != "" {
			res.Warnings = append(res.Warnings, w)
		}
	}
	if dryRun {
		return res, nil
	}
	if !created {
		release, err := connect(ctx, d, client)
		if err != nil {
			return nil, err
		}
		res.Running = release == nil
		if release != nil {
			defer func() { err = errors.Join(err, release()) }()
		}
	}
	if res.Running {
		if err := applyMessengers(ctx, client, next, store, res); err != nil {
			return nil, err
		}
	}
	if err := saveConfig(d, mutate, raw, store, del); err != nil {
		return nil, err
	}
	if then != nil {
		if err := then(next); err != nil {
			return nil, err
		}
	}
	if res.Running {
		if err := reload(ctx, client, next, res); err != nil {
			return nil, err
		}
	}
	res.Access = changedAccess(ctx, d, old, next, client, res.Running)
	return res, nil
}

// fileProblems names the problems of err, the error of the changed config,
// that config.yaml has by their key: all of them for an edited file, else
// those the saved config old has too.
func fileProblems(d agentdir.Dir, err error, old *agentdir.Config, stored agentdir.Secrets, created, edited bool) error {
	switch {
	case edited:
		return agentdir.EditProblems(err)
	case created:
		return err
	}
	return agentdir.FileProblemsOf(err, d.CheckConfig(old, stored))
}

// CheckEnclosing refuses to create an agent in d while an agent whose
// directory holds d runs. Its sandbox hides the .a2a directories of the
// agents it holds only from its next start.
func CheckEnclosing(ctx context.Context, d agentdir.Dir) error {
	dir := filepath.Dir(d.Root)
	for {
		outer, err := agentdir.FindEnclosing(dir)
		if errors.Is(err, agentdir.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = controlapi.NewClient(outer).Status(ctx)
		if err == nil || !errors.Is(err, agenterr.ErrNotRunning) {
			return agenterr.Conflictf("%s is inside the agent in %s, which is running, and its sandbox would expose the new agent's secrets; stop it with `a2a-serve stop --dir %s` first", d.Root, outer.Root, outer.Root)
		}
		parent := filepath.Dir(outer.Root)
		if parent == outer.Root {
			return nil
		}
		dir = parent
	}
}

// saveConfig stores the secrets store, deletes the secrets del, and saves
// the hand edit raw as config.yaml, or the config on disk with mutate
// applied. The config is read again under its lock, so that changes saved
// since apply read it, by other commands or the running server, are kept,
// or refuse the hand edit before anything is saved.
func saveConfig(d agentdir.Dir, mutate Mutation, raw *rawEdit, store agentdir.Secrets, del []string) error {
	if raw != nil {
		return d.LockConfig(func() error {
			cur, err := os.ReadFile(d.ConfigPath())
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if !bytes.Equal(cur, raw.base) {
				return ErrConfigChanged
			}
			if err := saveSecrets(d, store, del); err != nil {
				return err
			}
			return fsutil.WriteAtomic(d.ConfigPath(), raw.data, 0o644)
		})
	}
	if err := saveSecrets(d, store, del); err != nil {
		return err
	}
	return d.UpdateConfig(func(cfg *agentdir.Config) error {
		if _, _, err := mutate(cfg); err != nil {
			return &agentdir.SettingsError{Err: err}
		}
		return nil
	})
}

func saveSecrets(d agentdir.Dir, store agentdir.Secrets, del []string) error {
	if err := d.SaveSecrets(store); err != nil {
		return err
	}
	return d.DeleteSecrets(del...)
}

func secretChanges(stored, store agentdir.Secrets, del []string) []SecretChange {
	var out []SecretChange
	for _, name := range slices.Sorted(maps.Keys(store)) {
		switch stored[name] {
		case store[name]:
		case "":
			out = append(out, SecretChange{Name: name, Action: SecretStored})
		default:
			out = append(out, SecretChange{Name: name, Action: SecretRotated})
		}
	}
	for _, name := range del {
		if _, ok := stored[name]; ok {
			out = append(out, SecretChange{Name: name, Action: SecretDeleted})
		}
	}
	return out
}

var messengerKinds = []string{agentdir.MessengerTelegram, agentdir.MessengerDiscord, agentdir.MessengerEmail}

// messengerOf returns the messenger kind a setting configures, or "".
func messengerOf(setting string) string {
	if kind, _, _ := strings.Cut(setting, "-"); slices.Contains(messengerKinds, kind) {
		return kind
	}
	return ""
}

// messengerSecretsOf returns the names of the stored secrets the messenger of
// kind refers to.
func messengerSecretsOf(cfg *agentdir.Config, kind string) []string {
	refs := []string{cfg.MessengerToken(kind)}
	if e := cfg.Messengers.Email; kind == agentdir.MessengerEmail && e != nil {
		refs = []string{e.Password, e.OAuthClientSecret, e.OAuthRefreshToken}
	}
	var out []string
	for _, ref := range refs {
		if name, ok := strings.CutPrefix(ref, agentdir.SecretPrefix); ok && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// applyMessengers adds, replaces or removes the messengers which changed on the
// running server, which checks them first.
func applyMessengers(ctx context.Context, client *controlapi.Client, next *agentdir.Config, store agentdir.Secrets, res *Result) error {
	for _, kind := range messengerKinds {
		var settings []string
		for _, c := range res.Changes {
			if messengerOf(c.Setting) == kind {
				settings = append(settings, c.Setting)
			}
		}
		secrets := messengerSecretsOf(next, kind)
		if slices.ContainsFunc(res.Secrets, func(s SecretChange) bool { return slices.Contains(secrets, s.Name) && s.Action != SecretDeleted }) {
			settings = append(settings, kind)
		}
		if len(settings) == 0 {
			continue
		}
		req := controlapi.MessengerRequestFromConfig(next, kind)
		if req.Token == "" && req.Email == nil {
			if _, err := client.DeleteMessenger(ctx, kind); err != nil {
				return fmt.Errorf("removing %s from the running agent: %w", kind, err)
			}
		} else {
			for _, name := range secrets {
				if store[name] == "" {
					continue
				}
				if req.Secrets == nil {
					req.Secrets = agentdir.Secrets{}
				}
				req.Secrets[name] = store[name]
			}
			if _, err := client.AddMessenger(ctx, req); err != nil {
				return fmt.Errorf("applying %s to the running agent: %w", kind, err)
			}
		}
		res.Applied = appendNew(res.Applied, settings...)
	}
	return nil
}

// reload applies the other live settings, and lists the rest as waiting
// for a restart.
func reload(ctx context.Context, client *controlapi.Client, next *agentdir.Config, res *Result) error {
	var live []string
	for _, c := range res.Changes {
		switch {
		case messengerOf(c.Setting) != "":
		case c.Live:
			live = append(live, c.Setting)
		default:
			res.Restart = appendNew(res.Restart, c.Setting)
		}
	}
	for _, s := range res.Secrets {
		if slices.Contains(next.Secrets, s.Name) || s.Name == agentdir.SecretTailscaleKey {
			res.Restart = appendNew(res.Restart, "secret")
		}
	}
	if len(live) == 0 {
		return nil
	}
	_, err := client.ReloadConfig(ctx)
	if errors.Is(err, controlapi.ErrUnsupported) {
		res.Restart = appendNew(res.Restart, live...)
		return nil
	}
	if err != nil {
		return err
	}
	// The server can keep part of a live setting waiting, such as the grant
	// of a secret it loads at restart. When it can't say, the sandbox
	// settings count as waiting. Opening the public URL to anyone waits for
	// a public URL that waits.
	waiting := map[string]bool{}
	if slices.ContainsFunc(live, func(s string) bool { return agentdir.IsSandboxSetting(s) || s == settingPublicAllowIncoming }) {
		running, err := client.Config(ctx)
		switch {
		case err != nil:
			slog.Warn("reading the config of the running agent", "error", err)
			for _, s := range live {
				waiting[s] = agentdir.IsSandboxSetting(s)
			}
		default:
			for _, c := range agentdir.Diff(running, next) {
				waiting[c.Setting] = true
			}
			if waiting["public"] && next.Policy.PublicAllowIncoming == agentdir.AccessAnyone {
				waiting[settingPublicAllowIncoming] = true
			}
		}
	}
	for _, s := range live {
		if waiting[s] {
			res.Restart = appendNew(res.Restart, s)
		} else {
			res.Applied = appendNew(res.Applied, s)
		}
	}
	return nil
}

// settingPublicAllowIncoming says who may call on the public URL.
const settingPublicAllowIncoming = "public-allow-incoming"

// mcpWarnings returns a warning for each MCP server that next turns off and
// old didn't, which the project's .mcp.json has no server for.
func mcpWarnings(d agentdir.Dir, old, next *agentdir.Config) []string {
	added := slices.DeleteFunc(slices.Clone(next.MCP.Disabled), func(name string) bool { return old.MCPDisabled(name) })
	if len(added) == 0 {
		return nil
	}
	ws, err := d.WorkspaceMount(next)
	if err != nil {
		return nil
	}
	servers, err := project.LoadMCP(ws.Src)
	if err != nil {
		return nil
	}
	var out []string
	for _, u := range project.UnknownDisabled(servers, added) {
		out = append(out, u.String())
	}
	return out
}

func appendNew(list []string, items ...string) []string {
	for _, it := range items {
		if !slices.Contains(list, it) {
			list = append(list, it)
		}
	}
	return list
}

// WaitingForRestart returns the settings that wait for a restart of the
// running agent: those of res, the result of a change, and those saved
// before it. A stopped agent applies them on its next start, so none wait.
func WaitingForRestart(ctx context.Context, d agentdir.Dir, client *controlapi.Client, res *Result) ([]string, error) {
	if !res.Running {
		return nil, nil
	}
	pending, running, err := Pending(ctx, d, client)
	if err != nil || !running {
		return nil, err
	}
	return appendNew(slices.Clone(res.Restart), pending...), nil
}

// Pending returns the settings changed in config.yaml since the running
// server started, and whether it runs.
func Pending(ctx context.Context, d agentdir.Dir, client *controlapi.Client) ([]string, bool, error) {
	running, err := client.Config(ctx)
	if errors.Is(err, agenterr.ErrNotRunning) {
		return nil, false, nil
	}
	if errors.Is(err, controlapi.ErrUnsupported) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	saved, err := d.ReadConfig()
	if err != nil {
		return nil, true, err
	}
	var out []string
	for _, c := range agentdir.Diff(running, saved) {
		out = append(out, c.Setting)
	}
	return out, true, nil
}
