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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/relay"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/signin"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// GroupRelay heads the checks of a relay directory.
const GroupRelay = "Relay"

// relayOnline checks that the relay at relayURL is ready, and compares the
// Agent Mesh version it reports with the pin. Agent Mesh allows breaking
// changes between 0.x minor versions, so a different minor version fails
// and a different patch version warns.
func relayOnline(ctx context.Context, hc *http.Client, relayURL string) []Result {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	base := strings.TrimSuffix(relayURL, "/")
	// Google's frontend swallows /healthz on run.app.
	status, _, err := fetch(ctx, hc, base+"/readyz")
	if err != nil {
		return []Result{{ID: "relay", Status: StatusFail, Message: fmt.Sprintf("relay %s is unreachable: %v", relayURL, err)}}
	}
	if status != http.StatusOK {
		return []Result{{ID: "relay", Status: StatusFail, Message: fmt.Sprintf("relay %s isn't ready: %d %s", relayURL, status, http.StatusText(status))}}
	}
	status, body, err := fetch(ctx, hc, base+agentmesh.InfoPath)
	if err != nil {
		return []Result{{ID: "relay", Status: StatusFail, Message: fmt.Sprintf("relay %s is unreachable: %v", relayURL, err)}}
	}
	if status == http.StatusNotFound {
		return []Result{{ID: "relay", Status: StatusWarn, Message: fmt.Sprintf("relay %s answers, but doesn't report its Agent Mesh version", relayURL),
			Detail: "it isn't run by `a2a-serve relay run`; run Agent Mesh " + agentmesh.Version + " there"}}
	}
	var info agentmesh.Info
	if status != http.StatusOK || json.Unmarshal(body, &info) != nil {
		return []Result{{ID: "relay", Status: StatusFail, Message: fmt.Sprintf("relay %s answers %s with %d", relayURL, agentmesh.InfoPath, status)}}
	}
	ok, err := agentmesh.Compatible(info.AgentMesh)
	switch {
	case err != nil || !ok:
		return []Result{{ID: "relay", Status: StatusFail,
			Message: fmt.Sprintf("relay %s runs Agent Mesh %s, which agents on %s can't join", relayURL, info.AgentMesh, agentmesh.Version),
			Fix:     "run the relay with an a2a-serve that pins Agent Mesh " + agentmesh.Version + ", or upgrade this one"}}
	case info.AgentMesh != agentmesh.Version:
		return []Result{{ID: "relay", Status: StatusWarn,
			Message: fmt.Sprintf("relay %s runs Agent Mesh %s, and this a2a-serve %s", relayURL, info.AgentMesh, agentmesh.Version),
			Fix:     "run the same a2a-serve on the relay and the agents"}}
	}
	return []Result{{ID: "relay", Status: StatusOK, Message: fmt.Sprintf("relay %s is ready and runs Agent Mesh %s", relayURL, info.AgentMesh)}}
}

func fetch(ctx context.Context, hc *http.Client, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing a relay response", "error", err)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, body, err
}

// RelayOptions select the checks of [RunRelay].
type RelayOptions struct {
	Dir relaydir.Dir
	// Binary is where the pinned agentmesh-one is, or would be downloaded to.
	Binary string
	Online bool
}

// RunRelay runs the checks of the relay in opts.Dir. A relay doesn't use
// the a2a CLI, so its checks are left out.
func RunRelay(ctx context.Context, env Env, opts RelayOptions) []Result {
	checks := relayChecks(ctx, env, opts)
	for i := range checks {
		checks[i].Group = GroupRelay
	}
	return checks
}

func relayChecks(ctx context.Context, env Env, opts RelayOptions) []Result {
	d := opts.Dir
	conf, err := d.ReadConfig()
	if err != nil {
		return []Result{{ID: "relay-config", Status: StatusFail, Message: err.Error()}}
	}
	results := []Result{checkRelayConfig(d, conf)}
	if state, open := checkState(d.StateDir(), d.SecretsPath()); open {
		results = append(results, state)
	}
	results = append(results, checkRelayBinary(ctx, opts.Binary), checkDataVersion(d))
	if env.RelayStatus != nil {
		results = append(results, checkRelayRunning(ctx, env, d))
	}
	if opts.Online && env.HTTP != nil && conf.Tunnel == "" && conf.URL != "" {
		results = append(results, relayOnline(ctx, env.HTTP, conf.URL)...)
		if conf.SignIn() {
			results = append(results, relaySignIn(ctx, env.HTTP, conf.URL))
		}
	}
	return results
}

// relaySignIn checks that the relay serves its identity provider at its URL,
// where agents and the relay's agentmesh-one read it.
func relaySignIn(ctx context.Context, hc *http.Client, relayURL string) Result {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := signin.DiscoveryURL(relayURL)
	status, body, err := fetch(ctx, hc, u)
	if err != nil {
		return Result{ID: "relay-signin", Status: StatusFail, Message: fmt.Sprintf("the relay's sign-in at %s is unreachable: %v", u, err)}
	}
	var disc struct {
		Issuer string `json:"issuer"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &disc) != nil || disc.Issuer != signin.URL(relayURL) {
		return Result{ID: "relay-signin", Status: StatusFail, Message: fmt.Sprintf("the relay doesn't serve its sign-in at %s (status %d)", u, status),
			Fix: "a2a-serve relay run, and check that the relay's URL leads to it"}
	}
	return Result{ID: "relay-signin", Status: StatusOK, Message: "agents sign in at " + signin.URL(relayURL)}
}

func checkRelayConfig(d relaydir.Dir, conf *relaydir.Config) Result {
	if err := conf.Validate(); err != nil {
		return Result{ID: "relay-config", Status: StatusFail, Message: err.Error(), Fix: "a2a-serve relay run --url <url>"}
	}
	secrets, err := d.LoadSecrets()
	if err != nil {
		return Result{ID: "relay-config", Status: StatusFail, Message: err.Error()}
	}
	for flag, ref := range map[string]string{"admin-token": conf.AdminToken, "database": conf.Database, "google-client-secret": conf.GoogleClientSecret} {
		if _, err := secrets.Resolve(ref); err != nil {
			return Result{ID: "relay-config", Status: StatusFail, Message: fmt.Sprintf("the relay's %s: %v", flag, err), Fix: "a2a-serve relay run --" + flag + " <value>"}
		}
	}
	where := conf.URL
	if conf.Tunnel != "" {
		where = "a " + conf.Tunnel + " quick tunnel"
	}
	db := "SQLite"
	if conf.Database != "" {
		db = "PostgreSQL"
	}
	r := Result{ID: "relay-config", Status: StatusOK, Message: fmt.Sprintf("relay at %s, serving HTTP on %s, data in %s", where, conf.ListenAddr(), db)}
	if conf.SignIn() {
		r.Message += "; " + strings.Join(conf.AllowEmails, ", ") + " sign in with Google"
		r.Detail = "the Google app's redirect URI is " + signin.CallbackURL(conf.URL)
	}
	return r
}

func checkRelayBinary(ctx context.Context, path string) Result {
	if path == "" {
		return Result{ID: "agentmesh-one", Status: StatusSkip, Message: "no agentmesh-one to check"}
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return Result{ID: "agentmesh-one", Status: StatusOK, Message: fmt.Sprintf("agentmesh-one %s is downloaded on the next relay run", agentmesh.Version)}
	}
	if err := relay.CheckBinary(ctx, path); err != nil {
		return Result{ID: "agentmesh-one", Status: StatusFail, Message: err.Error(), Fix: "rm " + path + "; `a2a-serve relay run` downloads it again"}
	}
	return Result{ID: "agentmesh-one", Status: StatusOK, Message: fmt.Sprintf("agentmesh-one %s at %s", agentmesh.Version, path)}
}

func checkDataVersion(d relaydir.Dir) Result {
	rec, err := d.RecordedVersion()
	if err != nil {
		return Result{ID: "relay-data", Status: StatusFail, Message: err.Error()}
	}
	if rec == "" || rec == agentmesh.Version {
		return Result{ID: "relay-data", Status: StatusOK, Message: "the relay's data is at Agent Mesh " + agentmesh.Version}
	}
	got, perr := agentmesh.Parse(rec)
	want, _ := agentmesh.Parse(agentmesh.Version)
	if perr != nil || got.Compare(want) > 0 {
		return Result{ID: "relay-data", Status: StatusFail,
			Message: fmt.Sprintf("the relay's data was last used by Agent Mesh %s, newer than this a2a-serve's %s", rec, agentmesh.Version),
			Fix:     "run the relay with a newer a2a-serve"}
	}
	return Result{ID: "relay-data", Status: StatusWarn,
		Message: fmt.Sprintf("the relay's data is at Agent Mesh %s; the next run upgrades it to %s after a backup", rec, agentmesh.Version),
		Fix:     "a2a-serve relay upgrade"}
}

func checkRelayRunning(ctx context.Context, env Env, d relaydir.Dir) Result {
	st, err := env.RelayStatus(ctx, d)
	if errors.Is(err, control.ErrNotRunning) {
		return Result{ID: "relay-running", Status: StatusWarn, Message: "not running", Fix: "a2a-serve relay run"}
	}
	if err != nil {
		return Result{ID: "relay-running", Status: StatusFail, Message: err.Error()}
	}
	return Result{ID: "relay-running", Status: StatusOK, Message: fmt.Sprintf("running at %s, pid %d, %d members", st.URL, st.PID, st.Members)}
}
