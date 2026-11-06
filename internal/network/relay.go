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
	"slices"
	"strings"
	"time"
	"unicode"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// AgentMeshVersion is the Agent Mesh version this a2a-serve joins relays
// with. Relays should run the same version.
const AgentMeshVersion = agentmesh.Version

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

// refreshFile keeps the refresh token the relay gave the agent at its
// sign-in, so that renewing the agent's credential doesn't need the person.
const refreshFile = "refresh-token"

func savedRefreshToken(stateDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, refreshFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

func saveRefreshToken(stateDir, token string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(stateDir, refreshFile), []byte(token), 0o600)
}

func removeRefreshToken(stateDir string) error {
	if err := os.Remove(filepath.Join(stateDir, refreshFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
// returns the ID token the relay enrolls the agent with, and keeps the
// refresh token that comes with it for later ID tokens.
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
	// banned are the peer IDs of the agents the relay removed.
	banned []string
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

// decodeRelayInfo reads oidc_issuer (1), client_id (2), audience (3) and
// banned_peer_ids (5) from a serialized api.ControlPlaneInfoResponse
// without pulling generated mesh protos into builds without -tags agentmesh.
func decodeRelayInfo(b []byte) (relayInfo, error) {
	var info relayInfo
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return relayInfo{}, protowire.ParseError(n)
		}
		b = b[n:]
		if typ == protowire.BytesType && (num == 1 || num == 2 || num == 3 || num == 5) {
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
			case 5:
				info.banned = append(info.banned, v)
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

// relayRemoval returns an error that says how to get back when the relay
// in cfg removed the agent with peerID, and nil otherwise, also when it
// can't tell.
func relayRemoval(ctx context.Context, client *http.Client, cfg RelayConfig, peerID string) error {
	info, err := fetchRelayInfo(ctx, client, cfg.URL)
	if err != nil {
		slog.Debug("checking whether the relay removed this agent", "error", err)
		return nil
	}
	if !slices.Contains(info.banned, peerID) {
		return nil
	}
	if cfg.Token == "" {
		return fmt.Errorf("the relay %s removed this agent (peer %s); ask its operator to let you back on --allow-email and restart the relay", cfg.URL, peerID)
	}
	return fmt.Errorf("the relay %s removed this agent (peer %s); ask its operator to let it back with `a2a-serve relay invite %s`, then run `a2a-serve restart`", cfg.URL, peerID, cfg.AgentName)
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

// errSignInAgain means the relay refused the saved refresh token, or there
// is none, so the person has to sign in.
var errSignInAgain = errors.New("the agent has to sign in to the relay")

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	Desc         string `json:"error_description"`
}

// Token returns an ID token for the agent: with the saved refresh token
// when the relay accepts it, and otherwise with the device authorization
// flow, which prints a link and waits until the person signs in. It records
// the email signed in with.
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
	if disc.TokenEndpoint == "" {
		return "", errors.New("the relay's identity provider has no token endpoint; join it with --relay-token")
	}
	tok, err := l.refresh(ctx, info, disc.TokenEndpoint)
	if !errors.Is(err, errSignInAgain) {
		return tok, err
	}
	if disc.DeviceEndpoint == "" {
		return "", errors.New("the relay's identity provider has no device sign-in; join it with --relay-token")
	}
	return l.deviceSignIn(ctx, info, disc.DeviceEndpoint, disc.TokenEndpoint)
}

// refresh trades the saved refresh token for an ID token. It returns
// errSignInAgain when there is none or the relay refuses it, and other
// errors when the relay may accept it later.
func (l *RelayLogin) refresh(ctx context.Context, info relayInfo, tokenURL string) (string, error) {
	refresh, err := savedRefreshToken(l.StateDir)
	if err != nil {
		return "", fmt.Errorf("reading the relay's refresh token: %w", err)
	}
	if refresh == "" {
		return "", errSignInAgain
	}
	var tok tokenResponse
	status, err := l.postForm(ctx, tokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {info.clientID},
	}, &tok)
	if err != nil {
		return "", fmt.Errorf("renewing the relay sign-in: %w", err)
	}
	switch {
	case status == http.StatusOK && tok.IDToken != "":
		return tok.IDToken, l.signedIn(tok, false)
	case tok.Error == "invalid_grant":
		l.logf("! relay: %s", relayText(tok.Desc))
		if err := removeRefreshToken(l.StateDir); err != nil {
			return "", err
		}
		return "", errSignInAgain
	}
	return "", fmt.Errorf("renewing the relay sign-in: status %d: %s %s", status, relayText(tok.Error), relayText(tok.Desc))
}

// signedIn records who the agent signed in as, and the refresh token.
func (l *RelayLogin) signedIn(tok tokenResponse, announce bool) error {
	if tok.RefreshToken != "" {
		if err := saveRefreshToken(l.StateDir, tok.RefreshToken); err != nil {
			return fmt.Errorf("saving the relay's refresh token: %w", err)
		}
	}
	email := tokenEmail(tok.IDToken)
	if email == "" {
		return nil
	}
	if err := saveOwner(l.StateDir, email); err != nil {
		return err
	}
	if announce {
		l.logf("✓ signed in to the relay as %s", email)
	}
	return nil
}

func (l *RelayLogin) deviceSignIn(ctx context.Context, info relayInfo, deviceURL, tokenURL string) (string, error) {
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
	if status, err := l.postForm(ctx, deviceURL, form, &dev); err != nil || status != http.StatusOK || dev.DeviceCode == "" {
		return "", fmt.Errorf("starting the relay sign-in: status %d: %v", status, err)
	}
	link := dev.Complete
	if link == "" {
		link = dev.VerificationURI
	}
	l.logf("→ sign in to the relay %s: %s (code %s)", l.RelayURL, relayText(link), relayText(dev.UserCode))
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
		var tok tokenResponse
		_, err := l.postForm(ctx, tokenURL, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {dev.DeviceCode},
			"client_id":   {info.clientID},
		}, &tok)
		if err != nil {
			// The person may still be signing in; ask again until the link
			// expires.
			slog.Debug("polling the relay sign-in", "error", err)
			continue
		}
		switch tok.Error {
		case "":
			if tok.IDToken == "" {
				return "", errors.New("the relay's identity provider returned no ID token")
			}
			return tok.IDToken, l.signedIn(tok, true)
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return "", fmt.Errorf("the relay refused the sign-in: %s", relayText(tok.Desc))
		default:
			return "", fmt.Errorf("the relay sign-in failed: %s %s", relayText(tok.Error), relayText(tok.Desc))
		}
	}
	return "", errors.New("the relay sign-in link expired; start the agent again for a new one")
}

// maxRelayText bounds the text from a relay the agent prints.
const maxRelayText = 300

// relayText returns text from a relay without control characters, so that
// it can't rewrite the terminal, and at most maxRelayText runes long.
func relayText(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		if n == maxRelayText {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
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
