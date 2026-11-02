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

package doctor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

const (
	fixRestart = "a2a serve restart"
	fixEdit    = "a2a serve config edit"
)

// agentChecks checks the agent's config, secrets, mounts, state, server
// and service. It returns the config when it could be read.
func agentChecks(ctx context.Context, env Env, d agentdir.Dir, online bool) ([]Result, *agentdir.Config) {
	group := fmt.Sprintf("Agent (%s)", d.Root)
	var results []Result
	cfg, err := d.ReadConfig()
	if err != nil {
		results = append(results, Result{ID: "config", Status: StatusFail, Message: err.Error(), Fix: fixEdit})
	} else {
		group = fmt.Sprintf("Agent %s (%s)", cfg.Name, d.Root)
		results = append(results, checkConfig(cfg)...)
		secrets, err := d.LoadSecrets()
		if err != nil {
			results = append(results, Result{ID: "secrets", Status: StatusFail, Message: err.Error(), Fix: fixEdit})
		} else {
			results = append(results, checkSecrets(cfg, secrets)...)
		}
		results = append(results, checkMounts(d, cfg)...)
	}
	state, stateOpen := checkState(d)
	if stateOpen {
		results = append(results, state)
	}
	if cfg != nil {
		// The control client doesn't trust a socket in a state directory
		// others can read, so the server can't be checked until it's fixed.
		if !stateOpen {
			results = append(results, checkServer(ctx, env, d, cfg)...)
		}
		if r, ok := checkService(env, d); ok {
			results = append(results, r)
		}
	}
	for i := range results {
		results[i].Group = group
	}
	return results, cfg
}

func checkConfig(cfg *agentdir.Config) []Result {
	err := cfg.Validate()
	if err == nil {
		return []Result{{ID: "config", Status: StatusOK, Message: "config valid"}}
	}
	var results []Result
	for _, e := range unjoin(err) {
		results = append(results, Result{ID: "config", Status: StatusFail, Message: e.Error(), Fix: fixEdit})
	}
	return results
}

// unjoin returns the errors joined in err, or err itself.
func unjoin(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

// secretRef is a config value which references a stored secret, and the
// flag which sets it.
type secretRef struct{ flag, value string }

func secretRefs(cfg *agentdir.Config) []secretRef {
	refs := []secretRef{
		{agentdir.ChannelTelegram, cfg.ChannelToken(agentdir.ChannelTelegram)},
		{agentdir.ChannelDiscord, cfg.ChannelToken(agentdir.ChannelDiscord)},
	}
	if e := cfg.Channels.Email; e != nil {
		refs = append(refs, secretRef{"email-password", e.Password})
	}
	refs = append(refs, secretRef{"google-client-secret", cfg.Auth.GoogleClientSecret})
	var out []secretRef
	for _, r := range refs {
		if strings.HasPrefix(r.value, agentdir.SecretPrefix) {
			out = append(out, r)
		}
	}
	return out
}

func checkSecrets(cfg *agentdir.Config, secrets agentdir.Secrets) []Result {
	var results []Result
	for _, name := range cfg.Secrets {
		if secrets[name] == "" {
			results = append(results, Result{
				ID:      "secrets",
				Status:  StatusFail,
				Message: "secret " + name + " not stored",
				Detail:  "the harness gets it as an environment variable",
				Fix:     "a2a serve config set --secret " + name,
			})
		}
	}
	for _, ref := range secretRefs(cfg) {
		name := strings.TrimPrefix(ref.value, agentdir.SecretPrefix)
		if secrets[name] == "" {
			results = append(results, Result{
				ID:      "secrets",
				Status:  StatusFail,
				Message: fmt.Sprintf("secret %s (--%s) not stored", name, ref.flag),
				Fix:     fmt.Sprintf("a2a serve config set --%s -", ref.flag),
			})
		}
	}
	n := len(cfg.Secrets) + len(secretRefs(cfg))
	if len(results) == 0 && n > 0 {
		results = append(results, Result{ID: "secrets", Status: StatusOK, Message: fmt.Sprintf("%d %s stored", n, plural(n, "secret", "secrets"))})
	}
	return results
}

// checkMounts reports mounts whose source doesn't exist.
func checkMounts(d agentdir.Dir, cfg *agentdir.Config) []Result {
	mounts, err := d.Mounts(cfg)
	if err != nil {
		return []Result{{ID: "mounts", Status: StatusFail, Message: err.Error(), Fix: fixEdit}}
	}
	var results []Result
	for i, m := range mounts {
		if _, err := os.Stat(m.Src); err == nil {
			continue
		}
		r := Result{ID: "mounts", Status: StatusFail, Message: fmt.Sprintf("mount source %s does not exist", m.Src), Fix: "a2a serve config unset mount " + m.Dst}
		if i == 0 {
			r.Message = fmt.Sprintf("workspace %s does not exist", m.Src)
			r.Fix = "a2a serve config set --workspace DIR"
		}
		results = append(results, r)
	}
	return results
}

// checkState reports a state directory or secrets file others can read.
func checkState(d agentdir.Dir) (Result, bool) {
	type target struct {
		path string
		mode os.FileMode
	}
	var open []target
	for _, t := range []target{{d.StateDir(), 0o700}, {d.SecretsPath(), 0o600}} {
		info, err := os.Stat(t.path)
		if err != nil || info.Mode().Perm()&0o077 == 0 {
			continue
		}
		open = append(open, t)
	}
	if len(open) == 0 {
		return Result{}, false
	}
	paths := make([]string, len(open))
	for i, t := range open {
		paths[i] = t.path
	}
	return Result{
		ID:      "state-permissions",
		Status:  StatusFail,
		Message: strings.Join(paths, " and ") + " readable by others",
		Fix:     fixCommand,
		apply: func() (string, error) {
			var done []string
			for _, t := range open {
				msg, err := chmod(t.path, t.mode)()
				if err != nil {
					return strings.Join(done, "; "), err
				}
				done = append(done, msg)
			}
			return strings.Join(done, "; "), nil
		},
	}, true
}

// checkServer checks that the running agent answers on its local URL and
// accepts the owner token of this machine, or, when it isn't running, that
// its port is free.
func checkServer(ctx context.Context, env Env, d agentdir.Dir, cfg *agentdir.Config) []Result {
	st, err := env.Status(ctx, d)
	if errors.Is(err, controlapi.ErrNotRunning) {
		results := []Result{{ID: "running", Status: StatusWarn, Message: "not running", Fix: "a2a serve --detach"}}
		if cfg.Port > 0 && !env.PortFree(cfg.Port) {
			results = append(results, Result{
				ID:      "port",
				Status:  StatusFail,
				Message: fmt.Sprintf("port %d is in use by another process", cfg.Port),
				Fix:     "a2a serve config unset port",
			})
		}
		return results
	}
	if err != nil {
		return []Result{{ID: "running", Status: StatusFail, Message: "control socket: " + err.Error(), Fix: fixRestart}}
	}
	results := []Result{{ID: "running", Status: StatusOK, Message: fmt.Sprintf("running at %s, pid %d", st.Local, st.PID)}}
	if err := get(ctx, env, st.Local+"/healthz", "", http.StatusOK); err != nil {
		return append(results, Result{ID: "local-url", Status: StatusFail, Message: fmt.Sprintf("%s doesn't answer: %v", st.Local, err), Fix: fixRestart})
	}
	return append(results, checkOwnerToken(ctx, env, st.Local))
}

func checkOwnerToken(ctx context.Context, env Env, local string) Result {
	r := Result{ID: "owner-token"}
	store := env.Credentials
	c, ok, err := store.Lookup(local)
	if err != nil {
		r.Status, r.Message = StatusFail, err.Error()
		return r
	}
	if !ok {
		r.Status, r.Message, r.Fix = StatusFail, fmt.Sprintf("no owner token for %s in %s", local, store.Path), fixRestart
		r.Detail = "the server saves one when it starts"
		return r
	}
	err = get(ctx, env, local+"/tasks", c.Token, 0)
	if errors.Is(err, errUnauthorized) {
		r.Status, r.Message, r.Fix = StatusFail, fmt.Sprintf("the owner token for %s in %s is rejected", local, store.Path), fixRestart
		return r
	}
	if err != nil {
		r.Status, r.Message, r.Fix = StatusFail, fmt.Sprintf("checking the owner token: %v", err), fixRestart
		return r
	}
	r.Status, r.Message = StatusOK, "owner token accepted"
	return r
}

var errUnauthorized = errors.New("401 Unauthorized")

// get requests url with token, if set, and fails on a 401 or on another
// status than want, unless want is 0.
func get(ctx context.Context, env Env, url, token string, want int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := env.HTTP.Do(req)
	if err != nil {
		return err
	}
	if err := resp.Body.Close(); err != nil {
		slog.Debug("closing response", "error", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if want != 0 && resp.StatusCode != want {
		return errors.New(resp.Status)
	}
	return nil
}

// checkService checks that an installed service runs this binary in the
// agent's directory.
func checkService(env Env, d agentdir.Dir) (Result, bool) {
	if env.ServiceUnit == nil {
		return Result{}, false
	}
	r := Result{ID: "service"}
	path, err := env.ServiceUnit(d)
	if err != nil {
		r.Status, r.Message = StatusWarn, "can't check the service: "+err.Error()
		return r, true
	}
	if path == "" {
		return Result{}, false
	}
	unit, err := os.ReadFile(path)
	if err != nil {
		r.Status, r.Message, r.Fix = StatusFail, err.Error(), "a2a serve --service"
		return r, true
	}
	exe, err := env.Executable()
	if err != nil {
		r.Status, r.Message = StatusWarn, "can't check the service: "+err.Error()
		return r, true
	}
	content := string(unit)
	runsSelf := strings.Contains(content, exe) || strings.Contains(content, resolve(exe))
	if !runsSelf || !strings.Contains(content, d.Root) {
		r.Status, r.Message, r.Fix = StatusFail, "the service runs another binary or directory", "a2a serve --service"
		r.Detail = fmt.Sprintf("%s doesn't run %s --dir %s", path, exe, d.Root)
		return r, true
	}
	r.Status, r.Message = StatusOK, "service "+filepath.Base(path)+" runs this binary"
	return r, true
}

// tailscaleState is the file in which tsnet keeps the node identity.
const tailscaleState = "tailscaled.state"

func networkChecks(d agentdir.Dir, cfg *agentdir.Config) []Result {
	secrets, err := d.LoadSecrets()
	if err != nil {
		return nil
	}
	var results []Result
	n := cfg.Network
	if n.Private == agentdir.NetworkTailscale || n.Public == agentdir.NetworkTailscale {
		r := Result{ID: "tailscale"}
		_, err := os.Stat(filepath.Join(d.TSNetDir(), tailscaleState))
		switch {
		case err == nil:
			r.Status, r.Message = StatusOK, "Tailscale identity present"
		case secrets[agentdir.SecretTailscaleKey] != "":
			r.Status, r.Message = StatusOK, "Tailscale auth key stored"
		default:
			r.Status, r.Message, r.Fix = StatusWarn, "no Tailscale identity yet; the first start prints a login link", "a2a serve config set --tailscale-authkey -"
		}
		results = append(results, r)
	}
	if n.Public == agentdir.NetworkNgrok {
		r := Result{ID: "ngrok", Status: StatusOK, Message: "ngrok authtoken stored"}
		if secrets[agentdir.SecretNgrokToken] == "" {
			r.Status, r.Message, r.Fix = StatusFail, "ngrok authtoken not stored", "a2a serve config set --ngrok-authtoken -"
		}
		results = append(results, r)
	}
	for i := range results {
		results[i].Group = GroupNetwork
	}
	return results
}

// channelChecks checks the credentials of the channels with their services.
func channelChecks(ctx context.Context, env Env, d agentdir.Dir, cfg *agentdir.Config) []Result {
	secrets, err := d.LoadSecrets()
	if err != nil {
		return nil
	}
	var results []Result
	for _, kind := range []string{agentdir.ChannelTelegram, agentdir.ChannelDiscord, agentdir.ChannelEmail} {
		if !cfg.HasChannel(kind) {
			continue
		}
		what, fix := kind+" bot token", fmt.Sprintf("a2a serve config set --%s -", kind)
		if kind == agentdir.ChannelEmail {
			what, fix = "email IMAP and SMTP login", "a2a serve config set --email-username <user> --email-password -"
		}
		r := Result{ID: kind}
		if env.ValidateChannel == nil {
			r.Status, r.Message = StatusSkip, what+" not checked"
			results = append(results, r)
			continue
		}
		if kind != agentdir.ChannelEmail {
			if _, err := secrets.Resolve(cfg.ChannelToken(kind)); err != nil {
				continue
			}
		}
		if err := env.ValidateChannel(ctx, cfg, secrets, kind); err != nil {
			r.Status, r.Message, r.Fix = StatusFail, kind+": "+err.Error(), fix
		} else {
			r.Status, r.Message = StatusOK, what+" valid"
		}
		results = append(results, r)
	}
	for i := range results {
		results[i].Group = GroupChannels
	}
	return results
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
