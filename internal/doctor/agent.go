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
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/backup"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/project"
)

const (
	fixRestart = "a2a-serve restart"
	fixEdit    = "a2a-serve config edit"
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
		results = append(results, checkModel(cfg)...)
		if r, ok := checkConfigKeys(d); ok {
			results = append(results, r)
		}
		results = append(results, checkUnknownKeys(d)...)
		secrets, err := d.LoadSecrets()
		if err != nil {
			results = append(results, Result{ID: "secrets", Status: StatusFail, Message: err.Error(), Fix: fixEdit})
		} else {
			results = append(results, checkSecrets(cfg, secrets)...)
			results = append(results, checkMCP(d, cfg, secrets)...)
		}
		results = append(results, checkMounts(d, cfg)...)
		results = append(results, checkRelay(ctx, env, cfg, online)...)
		if r, ok := checkRelayInvite(d, cfg); ok {
			results = append(results, r)
		}
		if r, ok := checkBackupCLI(env, cfg); ok {
			results = append(results, r)
		}
	}
	state, stateOpen := checkState(d.StateDir(), d.SecretsPath())
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

// checkModel warns about a model setting that leaves the agent working with
// another model or without the provider's key, such as in an agent created
// before the server warned about it.
func checkModel(cfg *agentdir.Config) []Result {
	var results []Result
	for _, w := range harness.Warnings(cfg) {
		results = append(results, Result{ID: "model", Status: StatusWarn, Message: w})
	}
	return results
}

// checkConfigKeys warns about the keys of older versions in config.yaml,
// which the fix renames.
func checkConfigKeys(d agentdir.Dir) (Result, bool) {
	data, err := os.ReadFile(d.ConfigPath())
	if err != nil {
		return Result{ID: "config-keys", Status: StatusFail, Message: err.Error()}, true
	}
	keys, err := agentdir.LegacyKeys(data)
	if err != nil || len(keys) == 0 {
		return Result{}, false
	}
	return Result{
		ID:      "config-keys",
		Status:  StatusWarn,
		Message: "config.yaml uses old keys: " + strings.Join(keys, ", "),
		Fix:     fixCommand,
		apply: func() (string, error) {
			if err := d.UpdateConfig(func(*agentdir.Config) error { return nil }); err != nil {
				return "", err
			}
			return "renamed the old keys of " + d.ConfigPath(), nil
		},
	}, true
}

// checkUnknownKeys warns about the keys of config.yaml that the server
// ignores, such as a misspelled setting.
func checkUnknownKeys(d agentdir.Dir) []Result {
	keys, err := d.UnknownConfigKeys()
	if err != nil {
		return nil
	}
	var results []Result
	for _, k := range keys {
		results = append(results, Result{ID: "config-unknown-keys", Status: StatusWarn, Message: k.String(), Fix: fixEdit})
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

// secretRef is a config value which references a stored secret, what sets
// it, and the command which stores it again.
type secretRef struct{ what, value, fix string }

func secretRefs(cfg *agentdir.Config) []secretRef {
	refs := []secretRef{
		{"telegram token", cfg.MessengerToken(agentdir.MessengerTelegram), "a2a-serve messenger add telegram --token -"},
		{"discord token", cfg.MessengerToken(agentdir.MessengerDiscord), "a2a-serve messenger add discord --token -"},
	}
	if e := cfg.Messengers.Email; e != nil {
		refs = append(refs,
			secretRef{"email password", e.Password, "a2a-serve messenger add email --email-password -"},
			secretRef{"email OAuth client secret", e.OAuthClientSecret, "a2a-serve messenger add email --email-oauth-client-secret -"},
			secretRef{"email OAuth refresh token", e.OAuthRefreshToken, "a2a-serve messenger add email --email-oauth-refresh-token -"},
		)
	}
	refs = append(refs, secretRef{"--google-client-secret", cfg.Auth.GoogleClientSecret, "a2a-serve config set --google-client-secret -"})
	refs = append(refs, secretRef{"--backup-key", cfg.Backup.Key, "a2a-serve config set --backup-key -"})
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
				Detail:  "the agent gets it as an environment variable",
				Fix:     "a2a-serve config set --secret " + name,
			})
		}
	}
	for _, err := range cfg.ContainerEnvErrors(secrets) {
		fix := fixEdit
		var lb *agentdir.LineBreakError
		if errors.As(err, &lb) && slices.Contains(cfg.Secrets, lb.Name) {
			fix = "a2a-serve config set --secret " + lb.Name
		}
		results = append(results, Result{ID: "secrets", Status: StatusFail, Message: err.Error(), Fix: fix})
	}
	for _, ref := range secretRefs(cfg) {
		name := strings.TrimPrefix(ref.value, agentdir.SecretPrefix)
		if secrets[name] == "" {
			results = append(results, Result{
				ID:      "secrets",
				Status:  StatusFail,
				Message: fmt.Sprintf("secret %s (%s) not stored", name, ref.what),
				Fix:     ref.fix,
			})
		}
	}
	n := len(cfg.Secrets) + len(secretRefs(cfg))
	if len(results) == 0 && n > 0 {
		results = append(results, Result{ID: "secrets", Status: StatusOK, Message: fmt.Sprintf("%d %s stored", n, plural(n, "secret", "secrets"))})
	}
	return results
}

// checkMCP reports the MCP servers of the project's .mcp.json which reference
// variables without a value, since the harness doesn't get them, and
// servers the config turns off which .mcp.json doesn't have.
func checkMCP(d agentdir.Dir, cfg *agentdir.Config, secrets agentdir.Secrets) []Result {
	ws, err := d.WorkspaceMount(cfg)
	if err != nil {
		return nil
	}
	servers, err := project.LoadMCP(ws.Src)
	if err != nil {
		return []Result{{ID: "mcp", Status: StatusFail, Message: err.Error(), Detail: "the agent gets none of the project's MCP servers"}}
	}
	var results []Result
	for _, name := range cfg.MCP.Disabled {
		if !slices.ContainsFunc(servers, func(s project.MCPServer) bool { return s.Name == name }) {
			results = append(results, Result{
				ID:      "mcp",
				Status:  StatusWarn,
				Message: fmt.Sprintf("--disable-mcp %s names no server of %s", name, project.MCPFile),
				Fix:     "a2a-serve config unset disable-mcp " + name,
			})
		}
	}
	if len(servers) == 0 {
		return results
	}
	vars := project.Vars(cfg, secrets)
	var off int
	for _, srv := range servers {
		if cfg.MCPDisabled(srv.Name) {
			off++
			continue
		}
		_, missing := srv.Resolve(vars)
		if len(missing) == 0 {
			continue
		}
		fix := make([]string, len(missing))
		for i, name := range missing {
			fix[i] = "--secret " + name
		}
		results = append(results, Result{
			ID:      "mcp",
			Status:  StatusWarn,
			Message: fmt.Sprintf("MCP server %s needs %s", srv.Name, strings.Join(missing, ", ")),
			Detail:  "the agent doesn't get it until they have a value",
			Fix:     "a2a-serve config set " + strings.Join(fix, " "),
		})
	}
	if off > 0 && cfg.Sandbox == agentdir.SandboxLocal && cfg.Harness == "claude" {
		results = append(results, Result{
			ID:      "mcp",
			Status:  StatusWarn,
			Message: fmt.Sprintf("Claude Code without a sandbox may start the servers of %s that are off", project.MCPFile),
			Detail:  "the docker sandbox hides " + project.MCPFile + " from the agent",
			Fix:     "a2a-serve config unset sandbox",
		})
	}
	if len(results) > 0 {
		return results
	}
	msg := fmt.Sprintf("%d MCP servers in %s", len(servers), project.MCPFile)
	if off > 0 {
		msg += fmt.Sprintf(", %d off", off)
	}
	return []Result{{ID: "mcp", Status: StatusOK, Message: msg}}
}

// checkRelay checks an Agent Mesh relay the agent joins: that this
// a2a-serve can join one, and with --online that the relay answers.
func checkRelay(ctx context.Context, env Env, cfg *agentdir.Config, online bool) []Result {
	relay := cfg.Network.Private
	if !network.IsRelayURL(relay) {
		relay = cfg.Network.Public
	}
	if !network.IsRelayURL(relay) {
		return nil
	}
	if !network.AgentMeshBuilt {
		return []Result{{ID: "relay", Status: StatusFail, Message: "this a2a-serve was built without Agent Mesh, so it can't join " + relay,
			Fix: "go install -tags agentmesh github.com/a2aproject/a2a-serve@latest"}}
	}
	version := Result{ID: "relay", Status: StatusOK, Message: fmt.Sprintf("joins %s with Agent Mesh %s", relay, network.AgentMeshVersion)}
	if !online || env.HTTP == nil {
		return []Result{version}
	}
	return relayOnline(ctx, env.HTTP, relay)
}

// checkRelayInvite reports the invite the relay refused at the agent's
// last join, which the server recorded.
func checkRelayInvite(d agentdir.Dir, cfg *agentdir.Config) (Result, bool) {
	if !network.IsRelayURL(cfg.Network.Private) && !network.IsRelayURL(cfg.Network.Public) {
		return Result{}, false
	}
	problem, err := network.InviteProblem(d.AgentMeshDir())
	if err != nil {
		return Result{ID: "relay-invite", Status: StatusFail, Message: err.Error()}, true
	}
	if problem == "" {
		return Result{}, false
	}
	return Result{ID: "relay-invite", Status: StatusFail, Message: "the relay refused the agent's invite at the last start: " + problem, Fix: network.InviteFix}, true
}

// checkBackupCLI checks that the CLI the backup destination needs is
// installed.
func checkBackupCLI(env Env, cfg *agentdir.Config) (Result, bool) {
	cli := backup.CLI(cfg.Backup.To)
	if cli == "" {
		return Result{}, false
	}
	if _, err := env.LookPath(cli); err != nil {
		return Result{ID: "backup", Status: StatusFail, Message: fmt.Sprintf("backups to %s need the %s CLI, which is not on PATH", cfg.Backup.To, cli),
			Detail: "install it and sign in, so it finds credentials the usual way"}, true
	}
	return Result{ID: "backup", Status: StatusOK, Message: fmt.Sprintf("backups to %s use %s", cfg.Backup.To, cli)}, true
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
		r := Result{ID: "mounts", Status: StatusFail, Message: fmt.Sprintf("mount source %s does not exist", m.Src), Fix: "a2a-serve config unset mount " + m.Dst}
		if i == 0 {
			r.Message = fmt.Sprintf("workspace %s does not exist", m.Src)
			r.Fix = "a2a-serve config set --workspace DIR"
		}
		results = append(results, r)
	}
	return results
}

// checkState reports a state directory or secrets file others can read.
func checkState(stateDir, secrets string) (Result, bool) {
	type target struct {
		path string
		mode os.FileMode
	}
	var open []target
	for _, t := range []target{{stateDir, 0o700}, {secrets, 0o600}} {
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
	if errors.Is(err, agenterr.ErrNotRunning) {
		results := []Result{{ID: "running", Status: StatusWarn, Message: "not running", Fix: "a2a-serve start --detach"}}
		if cfg.Port > 0 && !env.PortFree(cfg.Port) {
			results = append(results, Result{
				ID:      "port",
				Status:  StatusFail,
				Message: fmt.Sprintf("port %d is in use by another process", cfg.Port),
				Fix:     "a2a-serve config unset port",
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
	results = append(results, checkOwnerToken(ctx, env, st.Local))
	return append(results, checkNetworks(st)...)
}

// checkNetworks reports the networks that don't reach the running agent,
// once for a relay that serves both its URLs.
func checkNetworks(st *controlapi.Status) []Result {
	var results []Result
	var seen []controlapi.NetworkProblem
	for _, p := range []*controlapi.NetworkProblem{st.PrivateProblem, st.PublicProblem} {
		if p == nil || slices.Contains(seen, *p) {
			continue
		}
		seen = append(seen, *p)
		status := StatusWarn
		if p.State == string(network.HealthRemoved) {
			status = StatusFail
		}
		results = append(results, Result{ID: "network", Status: status, Message: p.Problem, Fix: p.Fix})
	}
	return results
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
		r.Status, r.Message, r.Fix = StatusFail, err.Error(), "a2a-serve start --service"
		return r, true
	}
	exe, err := env.Executable()
	if err != nil {
		r.Status, r.Message = StatusWarn, "can't check the service: "+err.Error()
		return r, true
	}
	content := string(unit)
	runsSelf := strings.Contains(content, exe) || strings.Contains(content, resolve(exe))
	// systemd units separate arguments with spaces, launchd plists put each in a <string>.
	runsStart := strings.Contains(content, " start ") || strings.Contains(content, "<string>start</string>")
	if !runsSelf || !runsStart || !strings.Contains(content, d.Root) {
		r.Status, r.Message, r.Fix = StatusFail, "the service runs another binary, command or directory", "a2a-serve start --service"
		r.Detail = fmt.Sprintf("%s doesn't run %s start --dir %s", path, exe, d.Root)
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
			r.Status, r.Message, r.Fix = StatusWarn, "no Tailscale identity yet; the first start prints a login link", "a2a-serve config set --tailscale-authkey -"
		}
		results = append(results, r)
	}
	for i := range results {
		results[i].Group = GroupNetwork
	}
	return results
}

// messengerChecks checks the credentials of the messengers with their services.
func messengerChecks(ctx context.Context, env Env, d agentdir.Dir, cfg *agentdir.Config) []Result {
	secrets, err := d.LoadSecrets()
	if err != nil {
		return nil
	}
	var results []Result
	for _, kind := range []string{agentdir.MessengerTelegram, agentdir.MessengerDiscord, agentdir.MessengerEmail} {
		if !cfg.HasMessenger(kind) {
			continue
		}
		what, fix := kind+" bot token", fmt.Sprintf("a2a-serve messenger add %s --token -", kind)
		if kind == agentdir.MessengerEmail {
			what, fix = "email IMAP and SMTP login", "a2a-serve messenger add email --email-username <user> --email-password -"
		}
		r := Result{ID: kind}
		if env.ValidateMessenger == nil {
			r.Status, r.Message = StatusSkip, what+" not checked"
			results = append(results, r)
			continue
		}
		if kind != agentdir.MessengerEmail {
			if _, err := secrets.Resolve(cfg.MessengerToken(kind)); err != nil {
				continue
			}
		}
		if err := env.ValidateMessenger(ctx, cfg, secrets, kind); err != nil {
			r.Status, r.Message, r.Fix = StatusFail, kind+": "+err.Error(), fix
		} else {
			r.Status, r.Message = StatusOK, what+" valid"
		}
		results = append(results, r)
	}
	for i := range results {
		results[i].Group = GroupMessengers
	}
	return results
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
