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
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Config is the agent definition in .a2a/config.yaml. It holds no secrets:
// values like bot tokens are references of the form secret:<NAME>, resolved
// from the secrets file. Keys are camelCase; [ParseConfig] also reads the
// snake_case keys of older versions.
type Config struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Harness     string `yaml:"agent" json:"agent"`
	// LegacyHarness reads the agent from config files written before the
	// key was renamed; it is never written.
	LegacyHarness string `yaml:"harness,omitempty" json:"-"`
	// Command replaces the harness profile's command line, for any ACP agent.
	Command []string `yaml:"command,omitempty" json:"command,omitempty"`
	// Model is the model of the harness profile, in the agent's own format.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Image overrides the container image of the harness profile.
	Image string `yaml:"image,omitempty" json:"image,omitempty"`
	// Sandbox is where the harness runs: docker (default) or local.
	Sandbox string `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
	// Workspace is a src[:dst][:ro] spec. Empty means the directory holding
	// .a2a; a relative source is relative to it.
	Workspace string `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	// Mounts are src[:dst][:ro] specs mounted besides the workspace.
	Mounts []string `yaml:"mounts,omitempty" json:"mounts,omitempty"`
	// AllowBroadMounts are the paths the Docker sandbox may mount although
	// they are the user's home directory, / or a directory that holds it.
	AllowBroadMounts []string `yaml:"allowBroadMounts,omitempty" json:"allowBroadMounts,omitempty"`
	// Secrets are the names of secrets passed to the harness as environment variables.
	Secrets []string `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	// Env are extra NAME=value pairs for the harness.
	Env []string `yaml:"env,omitempty" json:"env,omitempty"`
	// Port is the localhost port. Zero picks a free port on first run.
	Port int `yaml:"port,omitempty" json:"port,omitempty"`
	// TaskTTL is how long tasks are kept after their last update, such as
	// 30d. Default 30d.
	TaskTTL string `yaml:"taskTTL,omitempty" json:"taskTTL,omitempty"`

	Network   NetworkConfig   `yaml:"network,omitempty" json:"network,omitzero"`
	Channels  ChannelsConfig  `yaml:"channels,omitempty" json:"channels,omitzero"`
	Policy    PolicyConfig    `yaml:"policy,omitempty" json:"policy,omitzero"`
	Approvals ApprovalsConfig `yaml:"approvals,omitempty" json:"approvals,omitzero"`
	Limits    LimitsConfig    `yaml:"limits,omitempty" json:"limits,omitzero"`
	Auth      AuthConfig      `yaml:"auth,omitempty" json:"auth,omitzero"`
	Backup    BackupConfig    `yaml:"backup,omitempty" json:"backup,omitzero"`
	MCP       MCPConfig       `yaml:"mcp,omitempty" json:"mcp,omitzero"`
}

// MCPConfig decides which MCP servers of the project's .mcp.json the agent
// gets.
type MCPConfig struct {
	// Disabled are the names of servers left out of the agent's sessions.
	Disabled []string `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// MCPDisabled reports whether the MCP server name is turned off.
func (c *Config) MCPDisabled(name string) bool {
	return slices.Contains(c.MCP.Disabled, name)
}

// PolicyConfig decides who may call the agent besides the user running it,
// who is always an owner.
type PolicyConfig struct {
	// PrivateAccess is everyone or paired. Empty is everyone.
	PrivateAccess string `yaml:"privateAccess,omitempty" json:"privateAccess,omitempty"`
	// PublicAccess is paired or anyone. Empty is paired.
	PublicAccess string `yaml:"publicAccess,omitempty" json:"publicAccess,omitempty"`
	// AllowEmails are the emails, or *@domain, of people who may call.
	AllowEmails []string `yaml:"allowEmails,omitempty" json:"allowEmails,omitempty"`
}

// Values of --private-access and --public-access.
const (
	AccessEveryone = "everyone"
	AccessPaired   = "paired"
	AccessAnyone   = "anyone"
)

// normalize replaces the access word of the other network with the one the
// setting uses, so that --private-access anyone and --public-access everyone
// work. everyone and anyone mean the same.
func (p *PolicyConfig) normalize() {
	if p.PrivateAccess == AccessAnyone {
		p.PrivateAccess = AccessEveryone
	}
	if p.PublicAccess == AccessEveryone {
		p.PublicAccess = AccessAnyone
	}
}

func (p PolicyConfig) clone() PolicyConfig {
	p.AllowEmails = slices.Clone(p.AllowEmails)
	return p
}

// AuthConfig is the owner's own Google app, which people sign in with.
type AuthConfig struct {
	// GoogleClientID and GoogleClientSecret are the owner's own Google app.
	// The secret is a secret reference.
	GoogleClientID     string `yaml:"googleClientID,omitempty" json:"googleClientID,omitempty"`
	GoogleClientSecret string `yaml:"googleClientSecret,omitempty" json:"googleClientSecret,omitempty"`
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

// DefaultTaskTTL is how long tasks are kept after their last update.
const DefaultTaskTTL = 30 * 24 * time.Hour

// MinTaskTTL is the shortest task TTL, so that tasks waiting for input or
// still running don't expire.
const MinTaskTTL = time.Hour

// TaskTTLDuration returns how long tasks are kept after their last update.
func (c *Config) TaskTTLDuration() time.Duration {
	if d, err := ParseDays(c.TaskTTL); err == nil {
		return d
	}
	return DefaultTaskTTL
}

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
	// Private is tailscale, a relay URL or empty.
	Private string `yaml:"private,omitempty" json:"private,omitempty"`
	// Public is tailscale, a relay URL or empty.
	Public string `yaml:"public,omitempty" json:"public,omitempty"`

	TailscaleControlURL string `yaml:"tailscaleControlURL,omitempty" json:"tailscaleControlURL,omitempty"`
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
	TrustedAuthServ string `yaml:"trustedAuthServ" json:"trustedAuthServ"`
	// OAuthRefreshToken, if set, signs in with OAuth (XOAUTH2), and Password
	// is unused. The access tokens come from OAuthTokenURL. The refresh token
	// and the client secret are secret references.
	OAuthTokenURL     string `yaml:"oauthTokenURL,omitempty" json:"oauthTokenURL,omitempty"`
	OAuthClientID     string `yaml:"oauthClientID,omitempty" json:"oauthClientID,omitempty"`
	OAuthClientSecret string `yaml:"oauthClientSecret,omitempty" json:"oauthClientSecret,omitempty"`
	OAuthRefreshToken string `yaml:"oauthRefreshToken,omitempty" json:"oauthRefreshToken,omitempty"`
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
	// OwnerChat is the chat, <channel>[:<who>], where an owner answers the
	// questions of tasks nobody watches. Empty picks the first messenger
	// with a linked owner.
	OwnerChat string `yaml:"ownerChat,omitempty" json:"ownerChat,omitempty"`
	// RemovedApprovers holds an approvers key, which Validate rejects: who
	// started a task answers its questions now.
	RemovedApprovers []string `yaml:"approvers,omitempty" json:"-"`
}

// LimitsConfig are container resource limits.
type LimitsConfig struct {
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
	CPUs   string `yaml:"cpus,omitempty" json:"cpus,omitempty"`
	PIDs   int    `yaml:"pids,omitempty" json:"pids,omitempty"`
}

// DefaultLimits are the limits of a container when the config sets none.
var DefaultLimits = LimitsConfig{Memory: "4g", CPUs: "2", PIDs: 512}

// defaultWorkspace is how config.yaml may spell the default workspace.
const defaultWorkspace = "."

// LocalURL is the agent's URL on this machine, empty before the first start
// picked its port.
func (c *Config) LocalURL() string {
	if c.Port == 0 {
		return ""
	}
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(c.Port))
}

// LoadConfig reads and validates config.yaml.
func (d Dir) LoadConfig() (*Config, error) {
	cfg, err := d.ReadConfig()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, &SettingsError{Err: fmt.Errorf("%s: %w; fix it with `a2a-serve config set` or `a2a-serve config edit`", d.ConfigPath(), err)}
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

// ParseConfig parses the content of config.yaml.
func ParseConfig(data []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var cfg Config
	if len(doc.Content) > 0 {
		renameLegacyKeys(doc.Content[0], reflect.TypeFor[Config](), "", nil)
		if err := doc.Decode(&cfg); err != nil {
			return nil, err
		}
	}
	if cfg.Harness == "" {
		cfg.Harness = cfg.LegacyHarness
	}
	cfg.LegacyHarness = ""
	if cfg.Workspace == defaultWorkspace {
		cfg.Workspace = ""
	}
	if len(cfg.Mounts) == 0 {
		cfg.Mounts = nil
	}
	cfg.Policy.normalize()
	return &cfg, nil
}

// UnknownKey is a key of config.yaml that Config doesn't have.
type UnknownKey struct {
	// Path is the dotted path of the key, such as network.allowlist.
	Path string
	// Suggestion is the dotted path of the known key closest to it, or "".
	Suggestion string
}

func (k UnknownKey) String() string {
	if k.Suggestion == "" {
		return fmt.Sprintf("config.yaml has an unknown key %s, which the server ignores", k.Path)
	}
	return fmt.Sprintf("config.yaml has an unknown key %s: did you mean %s?", k.Path, k.Suggestion)
}

// UnknownKeys returns the keys of the config.yaml content data that Config
// doesn't have, sorted by path.
func UnknownKeys(data []byte) ([]UnknownKey, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	var out []UnknownKey
	renameLegacyKeys(doc.Content[0], reflect.TypeFor[Config](), "", nil)
	unknownKeys(doc.Content[0], reflect.TypeFor[Config](), "", &out)
	slices.SortFunc(out, func(a, b UnknownKey) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// UnknownConfigKeys returns the keys of config.yaml that Config doesn't
// have, sorted by path.
func (d Dir) UnknownConfigKeys() ([]UnknownKey, error) {
	data, err := os.ReadFile(d.ConfigPath())
	if err != nil {
		return nil, err
	}
	return UnknownKeys(data)
}

// LegacyKeys returns the snake_case keys of older versions in the
// config.yaml content data, as dotted paths, sorted. Saving the config
// renames them.
func LegacyKeys(data []byte) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	var out []string
	renameLegacyKeys(doc.Content[0], reflect.TypeFor[Config](), "", &out)
	slices.Sort(out)
	return out, nil
}

// yamlFields returns the types of the fields of a struct by their YAML keys.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	out := map[string]reflect.Type{}
	for f := range t.Fields() {
		if name, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); name != "" && name != "-" {
			out[name] = f.Type
		}
	}
	return out
}

func unknownKeys(n *yaml.Node, t reflect.Type, prefix string, out *[]UnknownKey) {
	known := yamlFields(t)
	if known == nil || n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		ft, ok := known[key]
		if !ok {
			k := UnknownKey{Path: prefix + key}
			if s := closestKey(key, t); s != "" {
				k.Suggestion = prefix + s
			}
			*out = append(*out, k)
			continue
		}
		unknownKeys(n.Content[i+1], ft, prefix+key+".", out)
	}
}

// closestKey returns the key of the struct t which key most likely
// misspells, or "". Removed keys, hidden from JSON, aren't suggested.
func closestKey(key string, t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	best, bestDist := "", max(1, len(key)/3)+1
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" || f.Tag.Get("json") == "-" {
			continue
		}
		if d := editDistance(foldKey(key), foldKey(name)); d < bestDist {
			best, bestDist = name, d
		}
	}
	return best
}

// editDistance returns the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// renameLegacyKeys renames the snake_case keys in n, a node of type t, to
// the camelCase keys they match, such as task_ttl to taskTTL, and appends
// their dotted paths to renamed if it is set. A key whose camelCase key is
// also set is left alone.
func renameLegacyKeys(n *yaml.Node, t reflect.Type, prefix string, renamed *[]string) {
	known := yamlFields(t)
	if known == nil || n.Kind != yaml.MappingNode {
		return
	}
	set := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		set[n.Content[i].Value] = true
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i]
		if _, ok := known[key.Value]; !ok && strings.Contains(key.Value, "_") {
			for name := range known {
				if foldKey(name) == foldKey(key.Value) && !set[name] {
					if renamed != nil {
						*renamed = append(*renamed, prefix+key.Value)
					}
					key.Value, set[name] = name, true
					break
				}
			}
		}
		if ft, ok := known[key.Value]; ok {
			renameLegacyKeys(n.Content[i+1], ft, prefix+key.Value+".", renamed)
		}
	}
}

func foldKey(k string) string { return strings.ToLower(strings.ReplaceAll(k, "_", "")) }

// MarshalConfig returns the content of config.yaml for cfg.
func MarshalConfig(cfg *Config) ([]byte, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return append([]byte(configHeader), data...), nil
}

const configHeader = "# Agent definition for `a2a-serve`. Secrets are kept in state/secrets, never here.\n"

// SaveConfig writes config.yaml. The comments of the saved file and the keys
// Config doesn't have are kept.
func (d Dir) SaveConfig(cfg *Config) error {
	if err := d.Init(); err != nil {
		return err
	}
	old, err := os.ReadFile(d.ConfigPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := mergeConfig(old, cfg)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(d.ConfigPath(), data, 0o644)
}

// mergeConfig returns the content of config.yaml for cfg, editing the node
// tree of old, the saved content, so that its comments and unknown keys
// stay. Without a saved mapping, it returns [MarshalConfig].
func mergeConfig(old []byte, cfg *Config) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(old, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return MarshalConfig(cfg)
	}
	var next yaml.Node
	if err := next.Encode(cfg); err != nil {
		return nil, err
	}
	t := reflect.TypeFor[Config]()
	renameLegacyKeys(doc.Content[0], t, "", nil)
	mergeMapping(doc.Content[0], &next, t)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mergeMapping sets the keys of the mapping dst, a node of the struct t, to
// the values in src. Keys t doesn't have stay, and known keys missing from
// src are removed, unless they hold unknown keys.
func mergeMapping(dst, src *yaml.Node, t reflect.Type) {
	known := yamlFields(t)
	values := map[string]*yaml.Node{}
	for i := 0; i+1 < len(src.Content); i += 2 {
		values[src.Content[i].Value] = src.Content[i+1]
	}
	var content []*yaml.Node
	kept := map[string]bool{}
	for i := 0; i+1 < len(dst.Content); i += 2 {
		k, v := dst.Content[i], dst.Content[i+1]
		ft, ok := known[k.Value]
		if !ok {
			content = append(content, k, v)
			continue
		}
		if kept[k.Value] {
			continue
		}
		next, set := values[k.Value]
		if !set {
			// An empty struct is omitted, but its unknown keys stay.
			next = &yaml.Node{Kind: yaml.MappingNode}
		}
		v = mergeValue(v, next, ft)
		if !set && (v.Kind != yaml.MappingNode || len(v.Content) == 0) {
			continue
		}
		kept[k.Value] = true
		content = append(content, k, v)
	}
	for i := 0; i+1 < len(src.Content); i += 2 {
		if !kept[src.Content[i].Value] {
			content = append(content, src.Content[i], src.Content[i+1])
		}
	}
	dst.Content = content
}

// mergeValue returns the node for next, a value of type t, in place of
// old. An unchanged value keeps old, with its style and comments.
func mergeValue(old, next *yaml.Node, t reflect.Type) *yaml.Node {
	if old.Kind == yaml.MappingNode && next.Kind == yaml.MappingNode && yamlFields(t) != nil {
		mergeMapping(old, next, t)
		return old
	}
	a, b := reflect.New(t), reflect.New(t)
	if old.Decode(a.Interface()) == nil && next.Decode(b.Interface()) == nil && reflect.DeepEqual(a.Interface(), b.Interface()) {
		return old
	}
	next.HeadComment, next.LineComment, next.FootComment = old.HeadComment, old.LineComment, old.FootComment
	return next
}

// LockConfig runs fn holding the advisory lock of config.yaml, so that
// read-modify-write cycles of several processes don't lose changes. The
// lock file lives in the state directory, which is never committed.
func (d Dir) LockConfig(fn func() error) error {
	if err := os.MkdirAll(d.Path(), 0o700); err != nil {
		return err
	}
	if err := fsutil.MkdirReal(d.StateDir()); err != nil {
		return err
	}
	return fsutil.WithLock(filepath.Join(d.StateDir(), configFile), fn)
}

// UpdateConfig applies fn to the saved config and saves the result, holding
// the lock of config.yaml so that updates from several processes don't lose
// changes. A config.yaml that fails to parse is left alone and returned as
// the error.
func (d Dir) UpdateConfig(fn func(*Config) error) error {
	return d.LockConfig(func() error {
		cfg, err := d.LoadOrNewConfig()
		if err != nil {
			return err
		}
		if err := fn(cfg); err != nil {
			return err
		}
		return d.SaveConfig(cfg)
	})
}

// Clone returns a deep copy of c.
func (c *Config) Clone() *Config {
	out := *c
	out.Command = slices.Clone(c.Command)
	out.Mounts = slices.Clone(c.Mounts)
	out.AllowBroadMounts = slices.Clone(c.AllowBroadMounts)
	out.Secrets = slices.Clone(c.Secrets)
	out.Env = slices.Clone(c.Env)
	out.Approvals.Allow = slices.Clone(c.Approvals.Allow)
	out.Approvals.Deny = slices.Clone(c.Approvals.Deny)
	out.Policy = c.Policy.clone()
	out.MCP.Disabled = slices.Clone(c.MCP.Disabled)
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
