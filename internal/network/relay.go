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

package network

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// AgentMeshVersion is the Agent Mesh version this a2a-serve joins relays
// with. Relays should run the same version.
const AgentMeshVersion = "v0.2.0"

// AgentMeshHost is the virtual host of private URLs on a relay, which only
// agents on the relay reach, through their in-process mesh node.
const AgentMeshHost = "agentmesh.local"

// IsRelayURL reports whether a --private or --public value names an Agent
// Mesh relay by its URL.
func IsRelayURL(v string) bool {
	return strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")
}

// RelayConfig configures joining an Agent Mesh relay.
type RelayConfig struct {
	AgentName   string
	Description string
	// URL is the relay's URL.
	URL string
	// StateDir keeps the agent's mesh identity, so it keeps its URL.
	StateDir string
	// Token joins without signing in, for servers.
	Token string
	// Private and Public select the URLs the relay gives the agent.
	Private bool
	Public  bool
	// Logf prints progress for the user, such as the sign-in link.
	Logf func(string, ...any)
}

// ownerFile records the email the agent signed in to the relay with, so
// agents of the same person treat each other as the owner.
const ownerFile = "owner.json"

type relayOwner struct {
	Email string `json:"email"`
}

// SavedOwner returns the email the agent in stateDir signed in with, or "".
func SavedOwner(stateDir string) string {
	data, err := os.ReadFile(filepath.Join(stateDir, ownerFile))
	if err != nil {
		return ""
	}
	var o relayOwner
	if err := json.Unmarshal(data, &o); err != nil {
		slog.Warn("reading the relay owner", "error", err)
		return ""
	}
	return o.Email
}

func saveOwner(stateDir, email string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(relayOwner{Email: email})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, ownerFile), data, 0o600)
}

// RelayLogin signs the agent in to a relay with the relay's identity
// provider, printing a sign-in link, as Tailscale does on a first run. It
// returns the ID token the relay enrolls the agent with.
type RelayLogin struct {
	RelayURL string
	StateDir string
	HTTP     *http.Client
	Logf     func(string, ...any)
	// Poll is the shortest wait between token requests; tests shorten it.
	Poll time.Duration
}

type relayInfo struct {
	issuer, clientID, audience string
}

// info reads the relay's identity provider, which agents sign in with.
func (l *RelayLogin) info(ctx context.Context) (relayInfo, error) {
	info, err := fetchRelayInfo(ctx, l.HTTP, l.RelayURL)
	if err != nil {
		return relayInfo{}, err
	}
	if info.issuer == "" || info.clientID == "" {
		return relayInfo{}, errors.New("the relay doesn't let agents sign in; join it with --relay-token")
	}
	return info, nil
}

// RelayAdmitsBySignIn reports whether the relay at relayURL admits people by
// signing in with an identity provider: its /info names an issuer. A relay
// whose /info can't be read is taken to do so.
func RelayAdmitsBySignIn(ctx context.Context, client *http.Client, relayURL string) bool {
	info, err := fetchRelayInfo(ctx, client, relayURL)
	if err != nil {
		slog.Warn("reading the relay's info; treating it as a sign-in relay", "relay", relayURL, "error", err)
		return true
	}
	return info.issuer != ""
}

// fetchRelayInfo reads the relay's /info, which Agent Mesh serves as binary
// protobuf (api.ControlPlaneInfoResponse).
func fetchRelayInfo(ctx context.Context, client *http.Client, relayURL string) (relayInfo, error) {
	u := strings.TrimSuffix(relayURL, "/") + "/info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return relayInfo{}, fmt.Errorf("reading the relay's info: %w", err)
	}
	req.Header.Set("Accept", "application/x-protobuf")
	resp, err := client.Do(req)
	if err != nil {
		return relayInfo{}, fmt.Errorf("reading the relay's info: %w", err)
	}
	defer closeResp(resp)
	if resp.StatusCode != http.StatusOK {
		return relayInfo{}, fmt.Errorf("reading the relay's info: %s: %s", u, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return relayInfo{}, fmt.Errorf("reading the relay's info: %w", err)
	}
	info, err := decodeRelayInfo(body)
	if err != nil {
		return relayInfo{}, fmt.Errorf("reading the relay's info: %w", err)
	}
	return info, nil
}

// decodeRelayInfo reads oidc_issuer (1), client_id (2) and audience (3) from
// a serialized api.ControlPlaneInfoResponse without pulling generated mesh
// protos into builds without -tags agentmesh.
func decodeRelayInfo(b []byte) (relayInfo, error) {
	var info relayInfo
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return relayInfo{}, protowire.ParseError(n)
		}
		b = b[n:]
		if typ == protowire.BytesType && (num == 1 || num == 2 || num == 3) {
			v, m := protowire.ConsumeString(b)
			if m < 0 {
				return relayInfo{}, protowire.ParseError(m)
			}
			b = b[m:]
			switch num {
			case 1:
				info.issuer = v
			case 2:
				info.clientID = v
			case 3:
				info.audience = v
			}
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return relayInfo{}, protowire.ParseError(m)
		}
		b = b[m:]
	}
	return info, nil
}

func (l *RelayLogin) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer closeResp(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func closeResp(resp *http.Response) {
	if err := resp.Body.Close(); err != nil {
		slog.Debug("closing a response", "error", err)
	}
}

func (l *RelayLogin) postForm(ctx context.Context, u string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer closeResp(resp)
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// Token signs in with the device authorization flow: it prints a link,
// waits until the person signs in, and returns the ID token. It records the
// email signed in with.
func (l *RelayLogin) Token(ctx context.Context) (string, error) {
	info, err := l.info(ctx)
	if err != nil {
		return "", err
	}
	var disc struct {
		DeviceEndpoint string `json:"device_authorization_endpoint"`
		TokenEndpoint  string `json:"token_endpoint"`
	}
	if err := l.getJSON(ctx, strings.TrimSuffix(info.issuer, "/")+"/.well-known/openid-configuration", &disc); err != nil {
		return "", fmt.Errorf("discovering the relay's sign-in: %w", err)
	}
	if disc.DeviceEndpoint == "" || disc.TokenEndpoint == "" {
		return "", errors.New("the relay's identity provider has no device sign-in; join it with --relay-token")
	}
	form := url.Values{"client_id": {info.clientID}, "scope": {"openid email"}}
	if info.audience != "" {
		form.Set("audience", info.audience)
	}
	var dev struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Complete        string `json:"verification_uri_complete"`
		Interval        int    `json:"interval"`
		ExpiresIn       int    `json:"expires_in"`
	}
	if status, err := l.postForm(ctx, disc.DeviceEndpoint, form, &dev); err != nil || status != http.StatusOK || dev.DeviceCode == "" {
		return "", fmt.Errorf("starting the relay sign-in: status %d: %v", status, err)
	}
	link := dev.Complete
	if link == "" {
		link = dev.VerificationURI + " (code " + dev.UserCode + ")"
	}
	l.logf("→ sign in to the relay %s: %s", l.RelayURL, link)
	interval := max(time.Duration(dev.Interval)*time.Second, l.Poll)
	deadline := time.Now().Add(time.Duration(dev.ExpiresIn) * time.Second)
	if dev.ExpiresIn == 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		var tok struct {
			IDToken string `json:"id_token"`
			Error   string `json:"error"`
		}
		_, err := l.postForm(ctx, disc.TokenEndpoint, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {dev.DeviceCode},
			"client_id":   {info.clientID},
		}, &tok)
		if err != nil {
			return "", fmt.Errorf("finishing the relay sign-in: %w", err)
		}
		switch tok.Error {
		case "":
			if tok.IDToken == "" {
				return "", errors.New("the relay's identity provider returned no ID token")
			}
			if email := tokenEmail(tok.IDToken); email != "" {
				if err := saveOwner(l.StateDir, email); err != nil {
					return "", err
				}
				l.logf("✓ signed in to the relay as %s", email)
			}
			return tok.IDToken, nil
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return "", fmt.Errorf("the relay sign-in failed: %s", tok.Error)
		}
	}
	return "", errors.New("the relay sign-in link expired; start the agent again for a new one")
}

func (l *RelayLogin) logf(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}

// tokenEmail returns the verified email of an ID token the agent got from
// its own sign-in. The relay verifies the token; this only names the owner.
func tokenEmail(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if json.Unmarshal(data, &claims) != nil || !claims.Verified {
		return ""
	}
	return strings.ToLower(claims.Email)
}
