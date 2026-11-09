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
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Settings are the flags of `a2a-serve start <agent>` and `a2a-serve config
// set`. Set scalars override the config; list entries are added, replacing
// the entry with the same key; unset fields keep what earlier runs saved.
type Settings struct {
	Harness     string
	Name        string
	Description string
	// Command is the command line of the agent, as shell words.
	Command string
	// Model is the agent's model, in the agent's own format.
	Model string
	// Secrets are names of environment variables passed to the harness.
	// Their values are taken from SecretValues, copied from the stored
	// secret SecretSources names, or else read from the CLI's environment,
	// and stored.
	Secrets       []string
	SecretValues  Secrets
	SecretSources map[string]string
	// Workspace is a src[:dst][:ro] spec; Mounts are mounted besides it.
	Workspace string
	Mounts    []string
	// AllowBroadMounts are the paths the Docker sandbox may mount although
	// they expose the user's home directory.
	AllowBroadMounts []string
	Env              []string
	Private          string
	Public           string
	// PrivateAllowIncoming is everyone or paired, PublicAllowIncoming paired
	// or anyone, and PrivateAllowOutgoing everyone or contacts.
	PrivateAllowIncoming string
	PublicAllowIncoming  string
	PrivateAllowOutgoing string
	// AllowEmails are added to the people who may call by a verified email.
	AllowEmails []string
	// GuestStorage and GuestSecrets set the sandbox rule of the callers
	// that no more specific rule matches.
	GuestStorage string
	GuestSecrets []string
	// Telegram and Discord are bot tokens: env:VAR, secret:NAME or the value.
	Telegram string
	Discord  string
	Email    *EmailSettings
	Sandbox  string
	Image    string
	Port     int
	// Egress is public, allowlist or unrestricted, and EgressAllow are
	// added to the hosts the Docker sandbox may reach.
	Egress      string
	EgressAllow []string
	// InjectHeaders are host=Name: value rules, and InjectModelKeys
	// provider=NAME rules, of headers the server adds to the sandbox's
	// requests.
	InjectHeaders   []string
	InjectModelKeys []string

	Memory     string
	CPUs       string
	PIDs       int
	AllowTools []string
	DenyTools  []string
	// OwnerChat is <messenger>[:<who>].
	OwnerChat string
	// DisableMCP are MCP servers of the project's .mcp.json to turn off.
	DisableMCP []string

	TailscaleControlURL string
	// TailscaleAuthKey is env:VAR, secret:NAME or the value.
	TailscaleAuthKey string
	// RelayToken joins an Agent Mesh relay without signing in: env:VAR,
	// secret:NAME or the value.
	RelayToken string

	GoogleClientID string
	// GoogleClientSecret is env:VAR, secret:NAME or the value.
	GoogleClientSecret string

	Backup string
	// BackupKey is env:VAR, secret:NAME or the value.
	BackupKey       string
	BackupInterval  string
	BackupRetention string

	TaskTTL        string
	ContactRefresh string
}

// EmailSettings configure the email messenger. Password, OAuthClientSecret
// and OAuthRefreshToken are env:VAR, secret:NAME or the value. Set fields
// override the configured messenger.
type EmailSettings struct {
	Address           string
	IMAP              string
	SMTP              string
	Username          string
	Password          string
	Mailbox           string
	TrustedAuthServ   string
	OAuthTokenURL     string
	OAuthClientID     string
	OAuthClientSecret string
	OAuthRefreshToken string
}

// Secret names under which flag values are stored.
const (
	SecretTelegram      = "TELEGRAM_BOT_TOKEN"
	SecretDiscord       = "DISCORD_BOT_TOKEN"
	SecretEmailPassword = "EMAIL_PASSWORD"
	SecretEmailOAuth    = "EMAIL_OAUTH_REFRESH_TOKEN"
	SecretEmailClient   = "EMAIL_OAUTH_CLIENT_SECRET"
	SecretTailscaleKey  = "TS_AUTHKEY"
	SecretGoogleClient  = "GOOGLE_CLIENT_SECRET"
	SecretRelayToken    = "RELAY_TOKEN"
	SecretBackupKey     = "BACKUP_KEY"
)

// Apply merges the settings into cfg, an agent's config in d, and returns
// the secrets to store. It doesn't validate the result.
func (s Settings) Apply(cfg *Config, d Dir) (Secrets, error) {
	secrets := Secrets{}
	for _, f := range fields {
		entries := f.entries(&s)
		if len(entries) == 0 {
			continue
		}
		var err error
		if f.merge != nil {
			err = f.merge(&s, cfg, d, secrets)
		} else {
			err = f.mergeEntries(cfg, d, entries, secrets)
		}
		if err != nil {
			return nil, err
		}
	}
	if cfg.Name == "" {
		cfg.Name = d.DefaultName()
	}
	cfg.Name = SanitizeName(cfg.Name)
	cfg.Policy.normalize()
	return secrets, nil
}

// entries returns the entries the flag of f sets in s.
func (f field) entries(s *Settings) []string {
	switch v := f.arg(s).(type) {
	case *string:
		return nonEmpty(*v)
	case *int:
		if *v == 0 {
			return nil
		}
		return []string{strconv.Itoa(*v)}
	case *[]string:
		if f.splitEntry == nil {
			return *v
		}
		var out []string
		for _, e := range *v {
			out = append(out, f.splitEntry(e)...)
		}
		return out
	}
	return nil
}

// mergeEntries overrides a scalar with the entry, or adds the entries to a
// list, replacing the entries with the same key. Secret values are replaced
// by references, and added to secrets.
func (f field) mergeEntries(cfg *Config, d Dir, entries []string, secrets Secrets) error {
	parsed := make([]string, 0, len(entries))
	for _, v := range entries {
		v, err := f.parseEntry(d, v, secrets)
		if err != nil {
			return fmt.Errorf("%s: %w", f.label(), err)
		}
		parsed = append(parsed, v)
	}
	if !f.list {
		f.put(cfg, parsed)
		return nil
	}
	cur := f.get(cfg)
	for _, v := range parsed {
		cur = putKeyed(cur, v, f.key)
	}
	f.put(cfg, cur)
	return nil
}

func (f field) parseEntry(d Dir, v string, secrets Secrets) (string, error) {
	if f.parse != nil {
		var err error
		if v, err = f.parse(d, v); err != nil {
			return "", err
		}
	}
	switch {
	case f.secretOnly:
		// Network providers read their secrets by well-known names.
		if ref, ok := strings.CutPrefix(v, SecretPrefix); ok && ref != f.secret {
			return "", fmt.Errorf("the network provider reads the secret %s; pass the value or env:VAR instead of %s", f.secret, v)
		}
		_, sec, err := ResolveInput(v, f.secret)
		if err != nil {
			return "", err
		}
		for _, val := range sec {
			secrets[f.secret] = val
		}
		return "", nil
	case f.secret != "":
		ref, sec, err := ResolveInput(v, f.secret)
		if err != nil {
			return "", err
		}
		maps.Copy(secrets, sec)
		return ref, nil
	}
	return v, nil
}

// mergeSecrets adds the --secret names to cfg and their values to secrets.
func mergeSecrets(s *Settings, cfg *Config, d Dir, secrets Secrets) error {
	var stored Secrets
	for _, name := range s.Secrets {
		v := s.SecretValues[name]
		if src, ok := s.SecretSources[name]; ok {
			if stored == nil {
				var err error
				if stored, err = d.LoadSecrets(); err != nil {
					return err
				}
			}
			if v = stored[src]; v == "" {
				return fmt.Errorf("--secret %s: no stored secret %s", name, src)
			}
		}
		if v == "" {
			v = os.Getenv(name)
		}
		if v == "" {
			return fmt.Errorf("--secret %s: the environment variable is not set", name)
		}
		secrets[name] = v
		cfg.Secrets = putKeyed(cfg.Secrets, name, nil)
	}
	return nil
}

// putKeyed adds entry to list, replacing the entry with the same key. A nil
// key compares whole entries.
func putKeyed(list []string, entry string, key func(string) string) []string {
	if key == nil {
		key = func(s string) string { return s }
	}
	k := key(entry)
	for i, e := range list {
		if key(e) == k {
			out := slices.Clone(list)
			out[i] = entry
			return out
		}
	}
	return append(slices.Clone(list), entry)
}

// mountKey is the destination of a mount spec.
func mountKey(spec string) string {
	m, err := parseMount(spec, "/")
	if err != nil {
		return spec
	}
	return m.Dst
}

// envKey is the name of a NAME=value pair.
func envKey(kv string) string {
	name, _, _ := strings.Cut(kv, "=")
	return name
}

// SettingsError is returned for settings which can't be applied, such as
// an invalid config, as opposed to failures to read or write the agent
// directory. It is a usage error of the CLI.
type SettingsError struct{ Err error }

func (e *SettingsError) Error() string { return e.Err.Error() }

func (e *SettingsError) Unwrap() error { return e.Err }

// UsageError marks the error as a usage error for the CLI.
func (e *SettingsError) UsageError() bool { return true }

// LoadOrNewConfig reads the agent's config without validating it, or
// returns an empty one if the agent has none yet.
func (d Dir) LoadOrNewConfig() (*Config, error) {
	if !d.Exists() {
		return &Config{}, nil
	}
	return d.ReadConfig()
}

// ApplySettings returns the agent's config with the settings merged in,
// and the secrets to store with it, without saving either. The result is
// validated against the stored secrets and the new ones.
func (d Dir) ApplySettings(s Settings) (*Config, Secrets, error) {
	conf, err := d.LoadOrNewConfig()
	if err != nil {
		return nil, nil, err
	}
	secrets, err := d.applySettings(conf, s)
	if err != nil {
		return nil, nil, err
	}
	return conf, secrets, nil
}

// applySettings merges s into conf and validates the result.
func (d Dir) applySettings(conf *Config, s Settings) (Secrets, error) {
	secrets, err := s.Apply(conf, d)
	if err != nil {
		return nil, &SettingsError{Err: err}
	}
	stored, err := d.LoadSecrets()
	if err != nil {
		return nil, err
	}
	maps.Copy(stored, secrets)
	if err := d.CheckConfig(conf, stored); err != nil {
		return nil, &SettingsError{Err: err}
	}
	return secrets, nil
}

// SaveSettings merges settings into the agent's config and secrets.
func (d Dir) SaveSettings(s Settings) error {
	return d.UpdateConfig(func(conf *Config) error {
		secrets, err := d.applySettings(conf, s)
		if err != nil {
			return err
		}
		return d.SaveSecrets(secrets)
	})
}
