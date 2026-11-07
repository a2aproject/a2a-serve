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

// Package signin is the identity provider of a relay. Agents sign in with
// the device flow: the person opens a link on the relay, signs in with the
// relay's Google app, and the relay checks the email against its allowlist
// before it issues an ID token, which Agent Mesh enrolls the agent with.
//
// Agent Mesh v0.2.0 can't use Google's device flow, and any identity its
// issuer accepts may enroll, so the relay is the issuer and checks the
// allowlist itself.
package signin

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/a2aproject/a2a-serve/internal/backoff"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// Path is where a relay serves its identity provider. Agent Mesh serves its
// own token service at the relay's root, so the issuer has a path.
const Path = "/a2a-serve/signin"

// ClientID is the client ID agents sign in with and the audience of the ID
// tokens, which Agent Mesh accepts.
const ClientID = "a2a-serve"

// Limits of the identity provider.
const (
	// IDTokenTTL is how long an ID token lasts. Agent Mesh caps an agent's
	// credential at it, so an agent renews it with its refresh token.
	IDTokenTTL = time.Hour
	// DeviceTTL is how long a sign-in link lasts.
	DeviceTTL = 10 * time.Minute
	// PollInterval is how often an agent may ask whether the person signed in.
	PollInterval = 5 * time.Second
	// MaxPending bounds the sign-ins in progress. Each has at most three
	// trips to Google and approvals open.
	MaxPending = 1000

	maxBody     = 64 << 10
	maxParamLen = 4096
)

const (
	discoveryPath = "/.well-known/openid-configuration"
	jwksPath      = "/jwks"
	authorizePath = "/device/authorize"
	devicePath    = "/device"
	callbackPath  = "/callback"
	approvePath   = "/approve"
	tokenPath     = "/token"

	grantDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"
)

// Options configure an [Issuer].
type Options struct {
	// URL is the relay's public URL.
	URL string
	// Secret is the relay's admin token, which the issuer's keys derive from.
	Secret string
	// AllowEmails are who may sign in, as --allow-email takes them.
	AllowEmails []string
	// ClientID and ClientSecret are the relay's Google app.
	ClientID, ClientSecret string
	Google                 googleauth.Endpoints
	HTTP                   *http.Client
	Now                    func() time.Time
}

// Issuer is the identity provider of a relay.
type Issuer struct {
	opts     Options
	url      string
	keys     *keys
	verifier *googleauth.Verifier
	// starts bounds new sign-ins and refreshes the calls to Google that
	// renewals make; agents behind the relay's proxy share one address.
	starts, refreshes *rate.Limiter
	// lookups slows down guessing of user codes.
	lookups *backoff.Failures

	mu        sync.Mutex
	devices   map[string]*device
	byUser    map[string]string
	byLink    map[string]string
	signins   map[string]*pendingSignIn
	approvals map[string]*approval
	seq       uint64
	// lastRefresh is when each refresh token was last used, by its hash.
	lastRefresh map[string]time.Time
}

// New returns the identity provider of the relay at opts.URL.
func New(opts Options) (*Issuer, error) {
	k, err := deriveKeys(opts.Secret)
	if err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return &Issuer{
		opts:        opts,
		url:         URL(opts.URL),
		keys:        k,
		verifier:    &googleauth.Verifier{Endpoints: opts.Google, HTTP: opts.HTTP, Now: opts.Now},
		starts:      rate.NewLimiter(rate.Every(500*time.Millisecond), 100),
		refreshes:   rate.NewLimiter(rate.Every(100*time.Millisecond), 100),
		lookups:     backoff.NewFailures(time.Second, 10*time.Second, opts.Now),
		devices:     map[string]*device{},
		byUser:      map[string]string{},
		byLink:      map[string]string{},
		signins:     map[string]*pendingSignIn{},
		approvals:   map[string]*approval{},
		lastRefresh: map[string]time.Time{},
	}, nil
}

// URL returns the issuer URL of the relay at relayURL.
func URL(relayURL string) string {
	return strings.TrimSuffix(relayURL, "/") + Path
}

// DiscoveryURL returns where the relay at relayURL serves the OpenID
// configuration of its identity provider.
func DiscoveryURL(relayURL string) string {
	return URL(relayURL) + discoveryPath
}

// CallbackURL returns where Google sends people back to the relay at
// relayURL, the redirect URI of the relay's Google app.
func CallbackURL(relayURL string) string {
	return URL(relayURL) + callbackPath
}

// ServeHTTP serves the identity provider at paths with [Path] stripped.
func (i *Issuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	jsonhttp.LimitUnauthenticatedBody(maxBody, http.HandlerFunc(i.serve)).ServeHTTP(w, r)
}

func (i *Issuer) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == discoveryPath && r.Method == http.MethodGet:
		jsonhttp.Write(w, http.StatusOK, i.discovery())
	case r.URL.Path == jwksPath && r.Method == http.MethodGet:
		jsonhttp.Write(w, http.StatusOK, i.keys.jwks())
	case r.URL.Path == authorizePath && r.Method == http.MethodPost:
		i.authorize(w, r)
	case r.URL.Path == devicePath && r.Method == http.MethodGet:
		i.devicePage(w, r)
	case r.URL.Path == callbackPath && r.Method == http.MethodGet:
		i.callback(w, r)
	case r.URL.Path == approvePath && r.Method == http.MethodPost:
		i.approve(w, r)
	case r.URL.Path == tokenPath && r.Method == http.MethodPost:
		i.token(w, r)
	default:
		jsonhttp.Error(w, http.StatusNotFound, "not found")
	}
}

func (i *Issuer) discovery() map[string]any {
	return map[string]any{
		"issuer":                                i.url,
		"jwks_uri":                              i.url + jwksPath,
		"device_authorization_endpoint":         i.url + authorizePath,
		"token_endpoint":                        i.url + tokenPath,
		"grant_types_supported":                 []string{grantDeviceCode, "refresh_token"},
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"EdDSA"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"openid", "email"},
		"claims_supported":                      []string{"iss", "sub", "aud", "exp", "iat", "email", "email_verified", "hd"},
	}
}

// allowed reports whether the allowlist admits the account.
func (i *Issuer) allowed(c googleauth.Claims) bool {
	return googleauth.Allowed(i.opts.AllowEmails, c)
}

// issue returns an ID token of the account.
func (i *Issuer) issue(c googleauth.Claims) (string, error) {
	now := i.opts.Now()
	claims := map[string]any{
		"iss": i.url, "sub": c.Subject, "aud": ClientID,
		"email": strings.ToLower(c.Email), "email_verified": true,
		"iat": now.Unix(), "exp": now.Add(IDTokenTTL).Unix(),
	}
	if c.HostedDomain != "" {
		claims["hd"] = c.HostedDomain
	}
	return i.keys.idToken(claims)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	jsonhttp.Write(w, status, map[string]string{"error": code, "error_description": desc})
}

// longParam reports the first of names whose form value is too long.
func longParam(r *http.Request, names ...string) (string, bool) {
	i := slices.IndexFunc(names, func(n string) bool { return len(r.FormValue(n)) > maxParamLen })
	if i < 0 {
		return "", false
	}
	return names[i], true
}
