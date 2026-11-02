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
	"strings"
)

// Settings are the flags of `a2a serve <harness>` and `a2a serve config
// set`. Set scalars override the config; list entries are added, replacing
// the entry with the same key; unset fields keep what earlier runs saved.
type Settings struct {
	Harness     string
	Name        string
	Description string
	Command     []string
	// Secrets are names of environment variables passed to the harness.
	// Their values are taken from SecretValues, or else read from the CLI's
	// environment, and stored.
	Secrets      []string
	SecretValues Secrets
	// Workspace is a src[:dst][:ro] spec; Mounts are mounted besides it.
	Workspace string
	Mounts    []string
	Env       []string
	Private   string
	Public    string
	// Telegram and Discord are bot tokens: env:VAR, secret:NAME or the value.
	Telegram string
	Discord  string
	Email    *EmailSettings
	Sandbox  string
	Image    string
	Port     int

	Memory string
	CPUs   string
	PIDs   int
	// Identities are login=role pairs for network logins of other users.
	Identities []string

	AllowTools []string
	DenyTools  []string
	Approvers  []string

	TailscaleControlURL string
	NgrokURL            string
	// TailscaleAuthKey and NgrokAuthToken are env:VAR, secret:NAME or the value.
	TailscaleAuthKey string
	NgrokAuthToken   string

	// AllowEmails are added to the people who may sign in with Google.
	AllowEmails    []string
	GoogleClientID string
	// GoogleClientSecret is env:VAR, secret:NAME or the value.
	GoogleClientSecret string
}

// EmailSettings configure the email channel. Password is env:VAR,
// secret:NAME or the value. Set fields override the configured channel.
type EmailSettings struct {
	Address         string
	IMAP            string
	SMTP            string
	Username        string
	Password        string
	Mailbox         string
	TrustedAuthServ string
}

// Secret names under which flag values are stored.
const (
	SecretTelegram      = "TELEGRAM_BOT_TOKEN"
	SecretDiscord       = "DISCORD_BOT_TOKEN"
	SecretEmailPassword = "EMAIL_PASSWORD"
	SecretTailscaleKey  = "TS_AUTHKEY"
	SecretNgrokToken    = "NGROK_AUTHTOKEN"
	SecretGoogleClient  = "GOOGLE_CLIENT_SECRET"
)

// Apply merges the settings into cfg, an agent's config in d, and returns
// the secrets to store. It doesn't validate the result.
func (s Settings) Apply(cfg *Config, d Dir) (Secrets, error) {
	secrets := Secrets{}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&cfg.Harness, s.Harness)
	set(&cfg.Name, s.Name)
	if cfg.Name == "" {
		cfg.Name = d.DefaultName()
	}
	cfg.Name = SanitizeName(cfg.Name)
	set(&cfg.Description, s.Description)
	set(&cfg.Sandbox, s.Sandbox)
	set(&cfg.Image, s.Image)
	setNetwork(&cfg.Network.Private, s.Private)
	setNetwork(&cfg.Network.Public, s.Public)
	set(&cfg.Network.TailscaleControlURL, s.TailscaleControlURL)
	set(&cfg.Network.NgrokURL, s.NgrokURL)
	set(&cfg.Limits.Memory, s.Memory)
	set(&cfg.Limits.CPUs, s.CPUs)
	if s.PIDs != 0 {
		cfg.Limits.PIDs = s.PIDs
	}
	if len(s.Command) > 0 {
		cfg.Command = s.Command
	}
	if s.Port != 0 {
		cfg.Port = s.Port
	}
	if s.Workspace != "" {
		m, err := ParseMount(s.Workspace)
		if err != nil {
			return nil, fmt.Errorf("--workspace: %w", err)
		}
		cfg.Workspace = m.String()
		if m == (Mount{Src: d.Root, Dst: d.Root}) {
			cfg.Workspace = ""
		}
	}
	for _, spec := range s.Mounts {
		m, err := ParseMount(spec)
		if err != nil {
			return nil, fmt.Errorf("--mount: %w", err)
		}
		cfg.Mounts = putKeyed(cfg.Mounts, m.String(), mountKey)
	}
	for _, kv := range s.Env {
		cfg.Env = putKeyed(cfg.Env, kv, envKey)
	}
	for _, v := range s.AllowTools {
		cfg.Approvals.Allow = putKeyed(cfg.Approvals.Allow, v, nil)
	}
	for _, v := range s.DenyTools {
		cfg.Approvals.Deny = putKeyed(cfg.Approvals.Deny, v, nil)
	}
	for _, v := range s.Approvers {
		cfg.Approvals.Approvers = putKeyed(cfg.Approvals.Approvers, v, nil)
	}
	for _, v := range s.AllowEmails {
		cfg.Auth.AllowEmails = putKeyed(cfg.Auth.AllowEmails, strings.TrimSpace(v), strings.ToLower)
	}
	if name, ok := strings.CutPrefix(s.GoogleClientID, "env:"); ok {
		if s.GoogleClientID = os.Getenv(name); s.GoogleClientID == "" {
			return nil, fmt.Errorf("--google-client-id: environment variable %s is empty", name)
		}
	}
	set(&cfg.Auth.GoogleClientID, s.GoogleClientID)
	for _, kv := range s.Identities {
		login, role, ok := strings.Cut(kv, "=")
		if !ok || login == "" {
			return nil, fmt.Errorf("invalid --identity %q: want login=role", kv)
		}
		if cfg.Network.Identities == nil {
			cfg.Network.Identities = map[string]string{}
		}
		cfg.Network.Identities[login] = role
	}
	for _, name := range s.Secrets {
		v := s.SecretValues[name]
		if v == "" {
			v = os.Getenv(name)
		}
		if v == "" {
			return nil, fmt.Errorf("--secret %s: the environment variable is not set", name)
		}
		secrets[name] = v
		cfg.Secrets = putKeyed(cfg.Secrets, name, nil)
	}
	resolve := func(v, fallback string) (string, error) {
		ref, sec, err := ResolveInput(v, fallback)
		if err != nil {
			return "", err
		}
		maps.Copy(secrets, sec)
		return ref, nil
	}
	if s.Telegram != "" {
		ref, err := resolve(s.Telegram, SecretTelegram)
		if err != nil {
			return nil, fmt.Errorf("--telegram: %w", err)
		}
		cfg.Channels.Telegram = &TelegramConfig{Token: ref}
	}
	if s.Discord != "" {
		ref, err := resolve(s.Discord, SecretDiscord)
		if err != nil {
			return nil, fmt.Errorf("--discord: %w", err)
		}
		cfg.Channels.Discord = &DiscordConfig{Token: ref}
	}
	if s.GoogleClientSecret != "" {
		ref, err := resolve(s.GoogleClientSecret, SecretGoogleClient)
		if err != nil {
			return nil, fmt.Errorf("--google-client-secret: %w", err)
		}
		cfg.Auth.GoogleClientSecret = ref
	}
	if e := s.Email; e != nil && *e != (EmailSettings{}) {
		next := EmailConfig{}
		if cfg.Channels.Email != nil {
			next = *cfg.Channels.Email
		}
		set(&next.Address, e.Address)
		set(&next.IMAP, e.IMAP)
		set(&next.SMTP, e.SMTP)
		set(&next.Username, e.Username)
		set(&next.Mailbox, e.Mailbox)
		set(&next.TrustedAuthServ, e.TrustedAuthServ)
		if e.Password != "" {
			ref, err := resolve(e.Password, SecretEmailPassword)
			if err != nil {
				return nil, fmt.Errorf("--email-password: %w", err)
			}
			next.Password = ref
		}
		cfg.Channels.Email = &next
	}
	// Network providers read their secrets by well-known names.
	for name, v := range map[string]string{SecretTailscaleKey: s.TailscaleAuthKey, SecretNgrokToken: s.NgrokAuthToken} {
		if v == "" {
			continue
		}
		if ref, ok := strings.CutPrefix(v, SecretPrefix); ok && ref != name {
			return nil, fmt.Errorf("%s: the network provider reads the secret %s; pass the value or env:VAR instead of %s", name, name, v)
		}
		_, sec, err := ResolveInput(v, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, val := range sec {
			secrets[name] = val
		}
	}
	return secrets, nil
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
	secrets, err := s.Apply(conf, d)
	if err != nil {
		return nil, nil, &SettingsError{Err: err}
	}
	stored, err := d.LoadSecrets()
	if err != nil {
		return nil, nil, err
	}
	maps.Copy(stored, secrets)
	if err := d.CheckConfig(conf, stored); err != nil {
		return nil, nil, &SettingsError{Err: err}
	}
	return conf, secrets, nil
}

// SaveSettings merges settings into the agent's config and secrets.
func (d Dir) SaveSettings(s Settings) error {
	conf, secrets, err := d.ApplySettings(s)
	if err != nil {
		return err
	}
	if err := d.SaveSecrets(secrets); err != nil {
		return err
	}
	return d.SaveConfig(conf)
}

// setNetwork sets a provider; "none" removes it.
func setNetwork(dst *string, v string) {
	switch v {
	case "":
	case NetworkNone:
		*dst = ""
	default:
		*dst = v
	}
}
