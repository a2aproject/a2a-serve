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

package relaydir

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// TunnelCloudflare runs a Cloudflare quick tunnel to the relay.
const TunnelCloudflare = "cloudflare"

// DefaultListen is where the relay serves HTTP unless --listen says otherwise.
const DefaultListen = "127.0.0.1:8443"

// Names of the secrets a relay stores when a flag gives a value itself.
const (
	AdminTokenSecret         = "RELAY_ADMIN_TOKEN"
	DatabaseSecret           = "RELAY_DATABASE"
	GoogleClientSecretSecret = "RELAY_GOOGLE_CLIENT_SECRET"
)

// Config is the content of relay.yaml. Secret fields hold secret:NAME
// references into the relay's secrets.
type Config struct {
	// URL is the relay's address, which agents and pairing tokens name.
	URL string `yaml:"url,omitempty"`
	// Listen is the address the relay serves HTTP on.
	Listen string `yaml:"listen,omitempty"`
	// Tunnel publishes the relay through a tunnel, which gives it its URL.
	Tunnel     string `yaml:"tunnel,omitempty"`
	AdminToken string `yaml:"admin_token,omitempty"`
	// Database is a PostgreSQL DSN; without it the relay uses SQLite.
	Database string `yaml:"database,omitempty"`
	// AllowEmails are the people who may sign in to the relay with Google:
	// emails, or *@domain for the accounts of a Google Workspace domain.
	AllowEmails []string `yaml:"allow_email,omitempty"`
	// GoogleClientID and GoogleClientSecret are the Google app people sign
	// in with.
	GoogleClientID     string `yaml:"google_client_id,omitempty"`
	GoogleClientSecret string `yaml:"google_client_secret,omitempty"`
}

// SignIn reports whether people may sign in to the relay.
func (c *Config) SignIn() bool { return len(c.AllowEmails) > 0 }

// ListenAddr returns the address the relay serves HTTP on.
func (c *Config) ListenAddr() string {
	if c.Listen != "" {
		return c.Listen
	}
	return DefaultListen
}

// Settings are the flags of `relay run` which change relay.yaml. Empty
// fields keep the saved value; "none" clears Tunnel and Database, and
// AllowEmails of just "none" turns sign-in off and drops the Google app.
// AllowEmails replaces the saved list.
type Settings struct {
	URL                string
	Listen             string
	Tunnel             string
	AdminToken         string
	Database           string
	AllowEmails        []string
	GoogleClientID     string
	GoogleClientSecret string
}

const none = "none"

// ReadConfig reads relay.yaml.
func (d Dir) ReadConfig() (*Config, error) {
	data, err := os.ReadFile(d.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, NotFoundIn(d.Root)
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", d.ConfigPath(), err)
	}
	return &c, nil
}

// readConfigOrEmpty reads relay.yaml, or returns an empty config before the
// first run.
func (d Dir) readConfigOrEmpty() (*Config, error) {
	if !d.Exists() {
		return &Config{}, nil
	}
	return d.ReadConfig()
}

// SaveConfig writes relay.yaml.
func (d Dir) SaveConfig(c *Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(d.ConfigPath(), data, 0o644)
}

// ApplySettings returns relay.yaml with s applied and the secrets to store.
func (d Dir) ApplySettings(s Settings) (*Config, agentdir.Secrets, error) {
	c, err := d.readConfigOrEmpty()
	if err != nil {
		return nil, nil, err
	}
	secrets := agentdir.Secrets{}
	if s.URL != "" {
		c.URL = strings.TrimSuffix(s.URL, "/")
	}
	if s.Listen != "" {
		c.Listen = s.Listen
	}
	switch s.Tunnel {
	case "":
	case none:
		c.Tunnel = ""
	default:
		c.Tunnel = s.Tunnel
	}
	if s.AdminToken != "" {
		if c.AdminToken, err = secretInput("admin-token", s.AdminToken, AdminTokenSecret, secrets); err != nil {
			return nil, nil, err
		}
		if v, ok := secrets[strings.TrimPrefix(c.AdminToken, agentdir.SecretPrefix)]; ok {
			if err := checkAdminToken(v); err != nil {
				return nil, nil, err
			}
		}
	}
	switch s.Database {
	case "":
	case none:
		c.Database = ""
	default:
		if c.Database, err = secretInput("database", s.Database, DatabaseSecret, secrets); err != nil {
			return nil, nil, err
		}
	}
	switch {
	case len(s.AllowEmails) == 0:
	case len(s.AllowEmails) == 1 && s.AllowEmails[0] == none:
		c.AllowEmails, c.GoogleClientID, c.GoogleClientSecret = nil, "", ""
	default:
		c.AllowEmails = nil
		for _, e := range s.AllowEmails {
			c.AllowEmails = append(c.AllowEmails, strings.ToLower(strings.TrimSpace(e)))
		}
	}
	switch s.GoogleClientID {
	case "":
	case none:
		c.GoogleClientID, c.GoogleClientSecret = "", ""
	default:
		c.GoogleClientID = s.GoogleClientID
	}
	if s.GoogleClientSecret != "" {
		if c.GoogleClientSecret, err = secretInput("google-client-secret", s.GoogleClientSecret, GoogleClientSecretSecret, secrets); err != nil {
			return nil, nil, err
		}
	}
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	return c, secrets, nil
}

func secretInput(flag, v, fallback string, secrets agentdir.Secrets) (string, error) {
	ref, s, err := agentdir.ResolveInput(v, fallback)
	if err != nil {
		return "", agenterr.Invalidf("--%s: %v", flag, err)
	}
	maps.Copy(secrets, s)
	return ref, nil
}

// SaveSettings applies s to relay.yaml and stores its secrets. It returns
// the URL saved before, so callers can tell a changed URL.
func (d Dir) SaveSettings(s Settings) (oldURL string, err error) {
	if err := d.Init(); err != nil {
		return "", err
	}
	err = d.lockConfig(func() error {
		old, err := d.readConfigOrEmpty()
		if err != nil {
			return err
		}
		oldURL = old.URL
		c, secrets, err := d.ApplySettings(s)
		if err != nil {
			return err
		}
		if err := d.SaveSecrets(secrets); err != nil {
			return err
		}
		return d.SaveConfig(c)
	})
	return oldURL, err
}

// Validate checks the settings of a relay.
func (c *Config) Validate() error {
	switch c.Tunnel {
	case "", TunnelCloudflare:
	default:
		return agenterr.Invalidf("unknown tunnel %q; want %s", c.Tunnel, TunnelCloudflare)
	}
	if c.Tunnel != "" && c.URL != "" {
		return agenterr.Invalidf("a quick tunnel gives the relay a new URL on every start; drop --url, or drop the tunnel with --tunnel none")
	}
	if c.Tunnel == "" {
		if err := CheckURL(c.URL); err != nil {
			return err
		}
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr()); err != nil {
		return agenterr.Invalidf("invalid --listen %q: want host:port", c.Listen)
	}
	for flag, v := range map[string]string{"admin-token": c.AdminToken, "database": c.Database, "google-client-secret": c.GoogleClientSecret} {
		if v != "" && !strings.HasPrefix(v, agentdir.SecretPrefix) {
			return agenterr.Invalidf("%s in relay.yaml must be a secret:NAME reference; set it with --%s", flag, flag)
		}
	}
	return c.validateSignIn()
}

func (c *Config) validateSignIn() error {
	for _, e := range c.AllowEmails {
		if !agentdir.ValidEmailPattern(e) {
			return agenterr.Invalidf("invalid --allow-email %q: want an email, or *@domain", e)
		}
	}
	app := c.GoogleClientID != "" || c.GoogleClientSecret != ""
	switch {
	case (c.GoogleClientID == "") != (c.GoogleClientSecret == ""):
		return agenterr.Invalidf("--google-client-id and --google-client-secret go together: they are the relay's Google app")
	case c.SignIn() && !app:
		return agenterr.Invalidf("--allow-email needs --google-client-id and --google-client-secret: people sign in to the relay with its Google app")
	case app && !c.SignIn():
		return agenterr.Invalidf("--google-client-id needs --allow-email: people sign in to the relay only when it allows their email")
	case c.SignIn() && c.Tunnel != "":
		return agenterr.Invalidf("sign-in needs a URL that doesn't change, since the Google app names the relay's sign-in address; give --url, or join agents with invites")
	}
	return nil
}

// CheckURL checks the relay's URL with [agentmesh.CheckRelayURL].
func CheckURL(raw string) error {
	if raw == "" {
		return agenterr.Invalidf("the relay needs --url, the address agents reach it at, or --tunnel %s for a demo", TunnelCloudflare)
	}
	err := agentmesh.CheckRelayURL(raw)
	switch {
	case errors.Is(err, agentmesh.ErrCleartextRelayURL):
		return agenterr.Invalidf("invalid relay URL %q: %v; serve the relay behind a reverse proxy or a tunnel that ends TLS", raw, err)
	case err != nil:
		return agenterr.Invalidf("invalid relay URL %q: %v", raw, err)
	}
	return nil
}

// Resolved are the relay's settings with their secrets.
type Resolved struct {
	Config
	AdminToken         string
	Database           string
	GoogleClientSecret string
}

// Resolve returns the settings of relay.yaml with their secrets, creating
// the admin token on the first run. It stores the token before returning
// it, so the relay never runs with a token it would lose.
func (d Dir) Resolve() (*Resolved, error) {
	var r *Resolved
	err := d.lockConfig(func() error {
		c, err := d.ReadConfig()
		if err != nil {
			return err
		}
		if c.AdminToken == "" {
			tok, err := newToken()
			if err != nil {
				return err
			}
			if err := d.SaveSecrets(agentdir.Secrets{AdminTokenSecret: tok}); err != nil {
				return err
			}
			c.AdminToken = agentdir.SecretPrefix + AdminTokenSecret
			if err := d.SaveConfig(c); err != nil {
				return err
			}
		}
		secrets, err := d.LoadSecrets()
		if err != nil {
			return err
		}
		r = &Resolved{Config: *c}
		if r.AdminToken, err = secrets.Resolve(c.AdminToken); err != nil {
			return fmt.Errorf("the relay's admin token: %w", err)
		}
		if err := checkAdminToken(r.AdminToken); err != nil {
			return err
		}
		if r.Database, err = secrets.Resolve(c.Database); err != nil {
			return fmt.Errorf("the relay's database: %w", err)
		}
		if r.GoogleClientSecret, err = secrets.Resolve(c.GoogleClientSecret); err != nil {
			return fmt.Errorf("the relay's Google client secret: %w", err)
		}
		return nil
	})
	return r, err
}

// MinAdminToken is the length an admin token needs, so that guessing it at
// the rate the relay answers fails.
const MinAdminToken = 32

func checkAdminToken(v string) error {
	if len(v) < MinAdminToken {
		return agenterr.Invalidf("the relay's admin token has %d characters and needs at least %d; generate one with `openssl rand -hex 32`", len(v), MinAdminToken)
	}
	return nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
