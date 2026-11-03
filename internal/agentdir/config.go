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
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Config is the agent definition in .a2a/config.yaml. It holds no secrets:
// values like bot tokens are references of the form secret:<NAME>, resolved
// from the secrets file.
type Config struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Harness     string `yaml:"harness" json:"harness"`
	// Command replaces the harness profile's command line, for any ACP agent.
	Command []string `yaml:"command,omitempty" json:"command,omitempty"`
	// Image overrides the container image of the harness profile.
	Image string `yaml:"image,omitempty" json:"image,omitempty"`
	// Sandbox is where the harness runs: docker (default) or local.
	Sandbox string `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
	// Workspace is a src[:dst][:ro] spec. Empty means the directory holding
	// .a2a; a relative source is relative to it.
	Workspace string `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	// Mounts are src[:dst][:ro] specs mounted besides the workspace.
	Mounts []string `yaml:"mounts,omitempty" json:"mounts,omitempty"`
	// Secrets are the names of secrets passed to the harness as environment variables.
	Secrets []string `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	// Env are extra NAME=value pairs for the harness.
	Env []string `yaml:"env,omitempty" json:"env,omitempty"`
	// Port is the localhost port. Zero picks a free port on first run.
	Port int `yaml:"port,omitempty" json:"port,omitempty"`

	Network   NetworkConfig   `yaml:"network,omitempty" json:"network,omitzero"`
	Channels  ChannelsConfig  `yaml:"channels,omitempty" json:"channels,omitzero"`
	Approvals ApprovalsConfig `yaml:"approvals,omitempty" json:"approvals,omitzero"`
	Limits    LimitsConfig    `yaml:"limits,omitempty" json:"limits,omitzero"`
	Auth      AuthConfig      `yaml:"auth,omitempty" json:"auth,omitzero"`
	Backup    BackupConfig    `yaml:"backup,omitempty" json:"backup,omitzero"`
}

// AuthConfig lets people call the agent after signing in with Google.
type AuthConfig struct {
	// AllowEmails are the emails, or *@domain for a Google Workspace domain,
	// of the people who may sign in. Empty turns sign-in off.
	AllowEmails []string `yaml:"allow_emails,omitempty" json:"allowEmails,omitempty"`
	// GoogleClientID and GoogleClientSecret are the owner's own Google app.
	// The secret is a secret reference.
	GoogleClientID     string `yaml:"google_client_id,omitempty" json:"googleClientID,omitempty"`
	GoogleClientSecret string `yaml:"google_client_secret,omitempty" json:"googleClientSecret,omitempty"`
}

// BackupConfig configures the agent's backup snapshots.
type BackupConfig struct {
	// To is a directory, gs://bucket/prefix or s3://bucket/prefix.
	To string `yaml:"to,omitempty" json:"to,omitempty"`
	// Key references the secret snapshots are encrypted with.
	Key string `yaml:"key,omitempty" json:"key,omitempty"`
	// Interval is the time between snapshots, such as 1h. Default 24h.
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"`
	// Retention is how long snapshots are kept, such as 7d. Default 7d.
	Retention string `yaml:"retention,omitempty" json:"retention,omitempty"`
}

// Backup defaults.
const (
	DefaultBackupInterval  = 24 * time.Hour
	DefaultBackupRetention = 7 * 24 * time.Hour
)

// ParseDays parses a Go duration, or a number of days such as 7d.
func ParseDays(v string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid duration %q: want a number of days such as 7d, or a duration such as 12h", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q: want a number of days such as 7d, or a duration such as 12h", v)
	}
	return d, nil
}

// BackupInterval returns the time between snapshots.
func (b BackupConfig) BackupInterval() time.Duration {
	if d, err := ParseDays(b.Interval); err == nil {
		return d
	}
	return DefaultBackupInterval
}

// BackupRetention returns how long snapshots are kept.
func (b BackupConfig) BackupRetention() time.Duration {
	if d, err := ParseDays(b.Retention); err == nil {
		return d
	}
	return DefaultBackupRetention
}

// NetworkConfig selects the providers which make the agent reachable beyond
// localhost.
type NetworkConfig struct {
	// Private is tailscale, lan or empty.
	Private string `yaml:"private,omitempty" json:"private,omitempty"`
	// Public is tailscale, ngrok or empty.
	Public string `yaml:"public,omitempty" json:"public,omitempty"`

	TailscaleControlURL string `yaml:"tailscale_control_url,omitempty" json:"tailscaleControlURL,omitempty"`
	// NgrokURL is a reserved ngrok domain, such as https://coder.ngrok.app.
	NgrokURL string `yaml:"ngrok_url,omitempty" json:"ngrokURL,omitempty"`
	// LANPort is the port of the LAN listener. Zero picks one on first run.
	LANPort int `yaml:"lan_port,omitempty" json:"lanPort,omitempty"`
	// Identities maps network logins of other users to roles.
	Identities map[string]string `yaml:"identities,omitempty" json:"identities,omitempty"`
}

// ChannelsConfig configures the messengers the agent is reachable from.
type ChannelsConfig struct {
	Telegram *TelegramConfig `yaml:"telegram,omitempty" json:"telegram,omitempty"`
	Discord  *DiscordConfig  `yaml:"discord,omitempty" json:"discord,omitempty"`
	Email    *EmailConfig    `yaml:"email,omitempty" json:"email,omitempty"`
}

// TelegramConfig configures the Telegram bot of the agent.
type TelegramConfig struct {
	Token string `yaml:"token" json:"token"`
}

// DiscordConfig configures the Discord bot of the agent.
type DiscordConfig struct {
	Token string `yaml:"token" json:"token"`
}

// EmailConfig configures the mailbox of the agent.
type EmailConfig struct {
	// Address is the agent's own address.
	Address string `yaml:"address" json:"address"`
	// IMAP and SMTP are host:port. Port 993 and 465 use implicit TLS, others STARTTLS.
	IMAP     string `yaml:"imap" json:"imap"`
	SMTP     string `yaml:"smtp" json:"smtp"`
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"password" json:"password"`
	Mailbox  string `yaml:"mailbox,omitempty" json:"mailbox,omitempty"`
	// TrustedAuthServ is the authserv-id of the Authentication-Results header
	// added by the receiving mail server. Only its verdicts are trusted.
	TrustedAuthServ string `yaml:"trusted_authserv" json:"trustedAuthServ"`
}

// Channel kinds.
const (
	ChannelTelegram = "telegram"
	ChannelDiscord  = "discord"
	ChannelEmail    = "email"
)

// ChannelKinds are the messengers an agent can be reached from.
var ChannelKinds = []string{ChannelTelegram, ChannelDiscord, ChannelEmail}

// SetChannel configures the channel of kind. A bot token is a secret
// reference; an empty token or nil email removes the channel.
func (c *Config) SetChannel(kind, token string, email *EmailConfig) error {
	switch kind {
	case ChannelTelegram:
		c.Channels.Telegram = nil
		if token != "" {
			c.Channels.Telegram = &TelegramConfig{Token: token}
		}
	case ChannelDiscord:
		c.Channels.Discord = nil
		if token != "" {
			c.Channels.Discord = &DiscordConfig{Token: token}
		}
	case ChannelEmail:
		c.Channels.Email = email
	default:
		return fmt.Errorf("unknown channel %q: want telegram, discord or email", kind)
	}
	return nil
}

// HasChannel reports whether the channel of kind is configured.
func (c *Config) HasChannel(kind string) bool {
	switch kind {
	case ChannelTelegram:
		return c.Channels.Telegram != nil
	case ChannelDiscord:
		return c.Channels.Discord != nil
	case ChannelEmail:
		return c.Channels.Email != nil
	}
	return false
}

// ChannelToken returns the bot token reference of a telegram or discord
// channel, or "" if it isn't configured.
func (c *Config) ChannelToken(kind string) string {
	switch {
	case kind == ChannelTelegram && c.Channels.Telegram != nil:
		return c.Channels.Telegram.Token
	case kind == ChannelDiscord && c.Channels.Discord != nil:
		return c.Channels.Discord.Token
	}
	return ""
}

// ApprovalsConfig decides who answers tool permission requests.
type ApprovalsConfig struct {
	// Allow and Deny are ACP tool kind globs answered without asking.
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty" json:"deny,omitempty"`
	// Approvers are principal IDs, besides owners, who may answer approvals.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty"`
}

// LimitsConfig are container resource limits.
type LimitsConfig struct {
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
	CPUs   string `yaml:"cpus,omitempty" json:"cpus,omitempty"`
	PIDs   int    `yaml:"pids,omitempty" json:"pids,omitempty"`
}

// DefaultLimits are the limits of a container when the config sets none.
var DefaultLimits = LimitsConfig{Memory: "4g", CPUs: "2", PIDs: 512}

// defaultWorkspace is how config.yaml spells the default workspace when it
// also lists mounts, which tells it apart from a config in which the first
// mount was the workspace.
const defaultWorkspace = "."

// LoadConfig reads and validates config.yaml.
func (d Dir) LoadConfig() (*Config, error) {
	cfg, err := d.ReadConfig()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, &SettingsError{Err: fmt.Errorf("%s: %w; fix it with `a2a serve config set` or `a2a serve config edit`", d.ConfigPath(), err)}
	}
	return cfg, nil
}

// ReadConfig reads config.yaml without validating it, so that an invalid
// config can be fixed.
func (d Dir) ReadConfig() (*Config, error) {
	data, err := os.ReadFile(d.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s does not exist: %w", d.ConfigPath(), ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", d.ConfigPath(), err)
	}
	return cfg, nil
}

// ParseConfig parses the content of config.yaml. A config in which the first
// mount is the workspace is migrated to the workspace setting.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Workspace == "" && len(cfg.Mounts) > 0 {
		cfg.Workspace, cfg.Mounts = cfg.Mounts[0], cfg.Mounts[1:]
	}
	if cfg.Workspace == defaultWorkspace {
		cfg.Workspace = ""
	}
	if len(cfg.Mounts) == 0 {
		cfg.Mounts = nil
	}
	return &cfg, nil
}

// MarshalConfig returns the content of config.yaml for cfg.
func MarshalConfig(cfg *Config) ([]byte, error) {
	out := *cfg
	if out.Workspace == "" && len(out.Mounts) > 0 {
		out.Workspace = defaultWorkspace
	}
	data, err := yaml.Marshal(&out)
	if err != nil {
		return nil, err
	}
	return append([]byte(configHeader), data...), nil
}

const configHeader = "# Agent definition for `a2a serve`. Secrets are kept in state/secrets, never here.\n"

// SaveConfig writes config.yaml.
func (d Dir) SaveConfig(cfg *Config) error {
	if err := d.Init(); err != nil {
		return err
	}
	data, err := MarshalConfig(cfg)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(d.ConfigPath(), data, 0o644)
}

// Clone returns a deep copy of c.
func (c *Config) Clone() *Config {
	out := *c
	out.Command = slices.Clone(c.Command)
	out.Mounts = slices.Clone(c.Mounts)
	out.Secrets = slices.Clone(c.Secrets)
	out.Env = slices.Clone(c.Env)
	out.Network.Identities = maps.Clone(c.Network.Identities)
	out.Approvals.Allow = slices.Clone(c.Approvals.Allow)
	out.Approvals.Deny = slices.Clone(c.Approvals.Deny)
	out.Approvals.Approvers = slices.Clone(c.Approvals.Approvers)
	out.Auth.AllowEmails = slices.Clone(c.Auth.AllowEmails)
	if t := c.Channels.Telegram; t != nil {
		out.Channels.Telegram = &TelegramConfig{Token: t.Token}
	}
	if dc := c.Channels.Discord; dc != nil {
		out.Channels.Discord = &DiscordConfig{Token: dc.Token}
	}
	if e := c.Channels.Email; e != nil {
		email := *e
		out.Channels.Email = &email
	}
	return &out
}

// Mount is a parsed src[:dst][:ro] spec.
type Mount struct {
	Src      string
	Dst      string
	ReadOnly bool
}

// ParseMount parses a src[:dst][:ro] spec. A missing dst mirrors src; a
// relative src is relative to the current directory.
func ParseMount(spec string) (Mount, error) {
	return parseMount(spec, "")
}

// parseMount parses spec, resolving a relative src against base, or the
// current directory if base is empty.
func parseMount(spec, base string) (Mount, error) {
	parts := strings.Split(spec, ":")
	m := Mount{}
	if n := len(parts); n > 1 && parts[n-1] == "ro" {
		m.ReadOnly = true
		parts = parts[:n-1]
	}
	switch len(parts) {
	case 1:
		m.Src = parts[0]
	case 2:
		m.Src, m.Dst = parts[0], parts[1]
	default:
		return Mount{}, fmt.Errorf("invalid mount %q: want src[:dst][:ro]", spec)
	}
	if m.Src == "" {
		return Mount{}, fmt.Errorf("invalid mount %q: empty source", spec)
	}
	src, err := expandHome(m.Src)
	if err != nil {
		return Mount{}, err
	}
	if base != "" && !filepath.IsAbs(src) {
		src = filepath.Join(base, src)
	}
	if m.Src, err = filepath.Abs(src); err != nil {
		return Mount{}, err
	}
	if m.Dst == "" {
		m.Dst = m.Src
	}
	if !filepath.IsAbs(m.Dst) {
		return Mount{}, fmt.Errorf("invalid mount %q: destination must be absolute", spec)
	}
	m.Dst = filepath.Clean(m.Dst)
	return m, nil
}

// String formats m as a spec.
func (m Mount) String() string {
	s := m.Src
	if m.Dst != m.Src {
		s += ":" + m.Dst
	}
	if m.ReadOnly {
		s += ":ro"
	}
	return s
}

// WorkspaceMount returns the parsed workspace of the agent, by default the
// directory holding .a2a.
func (d Dir) WorkspaceMount(cfg *Config) (Mount, error) {
	if cfg.Workspace == "" {
		return Mount{Src: d.Root, Dst: d.Root}, nil
	}
	m, err := parseMount(cfg.Workspace, d.Root)
	if err != nil {
		return Mount{}, fmt.Errorf("workspace: %w", err)
	}
	return m, nil
}

// Mounts returns the parsed mounts of the agent, the workspace first.
func (d Dir) Mounts(cfg *Config) ([]Mount, error) {
	ws, err := d.WorkspaceMount(cfg)
	if err != nil {
		return nil, err
	}
	out := []Mount{ws}
	for _, spec := range cfg.Mounts {
		m, err := parseMount(spec, d.Root)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
