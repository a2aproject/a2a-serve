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
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// TunnelCloudflare runs a Cloudflare quick tunnel to the relay.
const TunnelCloudflare = "cloudflare"

// DefaultListen is where the relay serves HTTP unless --listen says otherwise.
const DefaultListen = "127.0.0.1:8443"

// Names of the secrets a relay stores when a flag gives a value itself.
const (
	AdminTokenSecret = "RELAY_ADMIN_TOKEN"
	DatabaseSecret   = "RELAY_DATABASE"
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
}

// ListenAddr returns the address the relay serves HTTP on.
func (c *Config) ListenAddr() string {
	if c.Listen != "" {
		return c.Listen
	}
	return DefaultListen
}

// Settings are the flags of `relay run` which change relay.yaml. Empty
// fields keep the saved value; "none" clears Tunnel and Database.
type Settings struct {
	URL        string
	Listen     string
	Tunnel     string
	AdminToken string
	Database   string
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
	for flag, v := range map[string]string{"admin-token": c.AdminToken, "database": c.Database} {
		if v != "" && !strings.HasPrefix(v, agentdir.SecretPrefix) {
			return agenterr.Invalidf("%s in relay.yaml must be a secret:NAME reference; set it with --%s", flag, flag)
		}
	}
	return nil
}

// CheckURL checks a relay URL: https, or http on loopback, with no path.
// Tokens travel over it.
func CheckURL(raw string) error {
	if raw == "" {
		return agenterr.Invalidf("the relay needs --url, the address agents reach it at, or --tunnel %s for a demo", TunnelCloudflare)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return agenterr.Invalidf("invalid relay URL %q", raw)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return agenterr.Invalidf("the relay URL %q must be a bare origin such as https://relay.example.com", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if loopback(u.Hostname()) {
			return nil
		}
	}
	return agenterr.Invalidf("the relay URL %q must use https, since tokens travel over it; serve the relay behind a reverse proxy or a tunnel that ends TLS", raw)
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Loopback reports whether the relay URL is on this machine.
func Loopback(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && loopback(u.Hostname())
}

// Resolved are the relay's settings with their secrets.
type Resolved struct {
	Config
	AdminToken string
	Database   string
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
