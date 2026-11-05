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
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/shlex"
)

// placement is the command whose flags change a setting.
type placement int

const (
	// placeCore settings are flags of `start <agent>` and `config set`.
	placeCore placement = iota
	// placeOperational settings are flags of `config set` only.
	placeOperational
	// placeChannel settings are flags of `channel add` only.
	placeChannel
)

// scope tells who decides a setting.
type scope int

const (
	scopeUnset scope = iota
	// scopeAgent settings define what the agent is, which a recipe sets.
	scopeAgent
	// scopeOwner settings decide where the agent is reachable, who may call
	// it and how it is operated, which only the owner sets.
	scopeOwner
)

// field is a setting of config.yaml, named by its flag. A scalar has at
// most one entry.
type field struct {
	flag  string
	place placement
	scope scope
	list  bool
	// live settings are applied by a running server without a restart.
	live bool
	get  func(*Config) []string
	// put replaces the entries with what get returns; nil resets the setting.
	put func(*Config, []string)
	// key returns the key of an entry, or of an argument naming it.
	key func(string) string
}

func scalar(flag string, live bool, p func(*Config) *string) field {
	return field{
		flag: flag, live: live,
		get: func(c *Config) []string { return nonEmpty(*p(c)) },
		put: func(c *Config, v []string) { *p(c) = first(v) },
	}
}

func intField(flag string, p func(*Config) *int) field {
	return field{
		flag: flag,
		get: func(c *Config) []string {
			if *p(c) == 0 {
				return nil
			}
			return []string{strconv.Itoa(*p(c))}
		},
		put: func(c *Config, v []string) {
			n, err := strconv.Atoi(first(v))
			if err != nil {
				n = 0
			}
			*p(c) = n
		},
	}
}

func list(flag string, live bool, p func(*Config) *[]string, key func(string) string) field {
	return field{
		flag: flag, list: true, live: live, key: key,
		get: func(c *Config) []string { return *p(c) },
		put: func(c *Config, v []string) { *p(c) = v },
	}
}

// operational marks f as a flag of `config set` only.
func operational(f field) field {
	f.place = placeOperational
	return f
}

func agentScope(f field) field {
	f.scope = scopeAgent
	return f
}

func ownerScope(f field) field {
	f.scope = scopeOwner
	return f
}

func emailField(flag string, p func(*EmailConfig) *string) field {
	return field{
		flag: flag, live: true, place: placeChannel, scope: scopeOwner,
		get: func(c *Config) []string {
			if c.Channels.Email == nil {
				return nil
			}
			return nonEmpty(*p(c.Channels.Email))
		},
		put: func(c *Config, v []string) {
			if c.Channels.Email == nil {
				c.Channels.Email = &EmailConfig{}
			}
			*p(c.Channels.Email) = first(v)
		},
	}
}

func tokenField(kind string, set func(*Config, string)) field {
	return field{
		flag: kind, live: true, place: placeChannel, scope: scopeOwner,
		get: func(c *Config) []string { return nonEmpty(c.ChannelToken(kind)) },
		put: func(c *Config, v []string) { set(c, first(v)) },
	}
}

func nonEmpty(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func identity(s string) string { return s }

func mountArgKey(s string) string {
	if strings.Contains(s, ":") {
		return mountKey(s)
	}
	return filepath.Clean(s)
}

var fields = []field{
	agentScope(scalar("agent", false, func(c *Config) *string { return &c.Harness })),
	ownerScope(scalar("name", false, func(c *Config) *string { return &c.Name })),
	agentScope(scalar("description", true, func(c *Config) *string { return &c.Description })),
	{
		flag:  "command",
		scope: scopeAgent,
		get:   func(c *Config) []string { return nonEmpty(ShellJoin(c.Command)) },
		put:   func(c *Config, v []string) { c.Command = shellSplit(first(v)) },
	},
	agentScope(scalar("image", false, func(c *Config) *string { return &c.Image })),
	agentScope(scalar("sandbox", false, func(c *Config) *string { return &c.Sandbox })),
	ownerScope(scalar("workspace", false, func(c *Config) *string { return &c.Workspace })),
	agentScope(list("mount", false, func(c *Config) *[]string { return &c.Mounts }, mountArgKey)),
	agentScope(list("secret", false, func(c *Config) *[]string { return &c.Secrets }, identity)),
	agentScope(list("env", false, func(c *Config) *[]string { return &c.Env }, envKey)),
	ownerScope(intField("port", func(c *Config) *int { return &c.Port })),
	ownerScope(scalar("private", false, func(c *Config) *string { return &c.Network.Private })),
	ownerScope(scalar("public", false, func(c *Config) *string { return &c.Network.Public })),
	ownerScope(scalar("private-access", true, func(c *Config) *string { return &c.Policy.PrivateAccess })),
	ownerScope(scalar("public-access", true, func(c *Config) *string { return &c.Policy.PublicAccess })),
	ownerScope(list("allow-email", true, func(c *Config) *[]string { return &c.Policy.AllowEmails }, strings.ToLower)),
	ownerScope(operational(scalar("tailscale-control-url", false, func(c *Config) *string { return &c.Network.TailscaleControlURL }))),
	tokenField(ChannelTelegram, func(c *Config, v string) {
		c.Channels.Telegram = nil
		if v != "" {
			c.Channels.Telegram = &TelegramConfig{Token: v}
		}
	}),
	tokenField(ChannelDiscord, func(c *Config, v string) {
		c.Channels.Discord = nil
		if v != "" {
			c.Channels.Discord = &DiscordConfig{Token: v}
		}
	}),
	emailField("email-address", func(e *EmailConfig) *string { return &e.Address }),
	emailField("email-imap", func(e *EmailConfig) *string { return &e.IMAP }),
	emailField("email-smtp", func(e *EmailConfig) *string { return &e.SMTP }),
	emailField("email-username", func(e *EmailConfig) *string { return &e.Username }),
	emailField("email-password", func(e *EmailConfig) *string { return &e.Password }),
	emailField("email-mailbox", func(e *EmailConfig) *string { return &e.Mailbox }),
	emailField("email-trusted-authserv", func(e *EmailConfig) *string { return &e.TrustedAuthServ }),
	emailField("email-oauth-token-url", func(e *EmailConfig) *string { return &e.OAuthTokenURL }),
	emailField("email-oauth-client-id", func(e *EmailConfig) *string { return &e.OAuthClientID }),
	emailField("email-oauth-client-secret", func(e *EmailConfig) *string { return &e.OAuthClientSecret }),
	emailField("email-oauth-refresh-token", func(e *EmailConfig) *string { return &e.OAuthRefreshToken }),
	agentScope(list("allow-tools", true, func(c *Config) *[]string { return &c.Approvals.Allow }, identity)),
	agentScope(list("deny-tools", true, func(c *Config) *[]string { return &c.Approvals.Deny }, identity)),
	ownerScope(scalar("owner-chat", true, func(c *Config) *string { return &c.Approvals.OwnerChat })),
	agentScope(operational(scalar("memory", false, func(c *Config) *string { return &c.Limits.Memory }))),
	agentScope(operational(scalar("cpus", false, func(c *Config) *string { return &c.Limits.CPUs }))),
	agentScope(operational(intField("pids", func(c *Config) *int { return &c.Limits.PIDs }))),
	ownerScope(operational(scalar("google-client-id", false, func(c *Config) *string { return &c.Auth.GoogleClientID }))),
	ownerScope(operational(scalar("google-client-secret", false, func(c *Config) *string { return &c.Auth.GoogleClientSecret }))),
	ownerScope(operational(scalar("backup", false, func(c *Config) *string { return &c.Backup.To }))),
	ownerScope(operational(scalar("backup-key", false, func(c *Config) *string { return &c.Backup.Key }))),
	ownerScope(operational(scalar("backup-interval", false, func(c *Config) *string { return &c.Backup.Interval }))),
	ownerScope(operational(scalar("backup-retention", false, func(c *Config) *string { return &c.Backup.Retention }))),
	ownerScope(operational(scalar("task-ttl", false, func(c *Config) *string { return &c.TaskTTL }))),
}

// unsetOnly are settings which `config unset` accepts besides the fields:
// the stored network secrets.
var unsetOnly = map[string]string{
	"tailscale-authkey": SecretTailscaleKey,
	"relay-token":       SecretRelayToken,
}

func lookupField(flag string) (field, bool) {
	i := slices.IndexFunc(fields, func(f field) bool { return f.flag == flag })
	if i < 0 {
		return field{}, false
	}
	return fields[i], true
}

// SettingNames returns the names `config unset` accepts.
func SettingNames() []string {
	out := slices.Collect(maps.Keys(unsetOnly))
	for _, f := range fields {
		if f.place != placeChannel {
			out = append(out, f.flag)
		}
	}
	slices.Sort(out)
	return out
}

// IsLive reports whether a running server applies the setting without a
// restart.
func IsLive(setting string) bool {
	f, ok := lookupField(setting)
	return ok && f.live
}

// IsAgentSetting reports whether the setting defines what the agent is, as
// opposed to an owner setting, which decides where the agent is reachable,
// who may call it and how it is operated. A recipe sets only agent settings.
func IsAgentSetting(setting string) bool {
	f, ok := lookupField(setting)
	return ok && f.scope == scopeAgent
}

// AgentSettings returns a config with only the agent settings of c.
func (c *Config) AgentSettings() *Config {
	out := &Config{}
	for _, f := range fields {
		if f.scope == scopeAgent {
			f.put(out, slices.Clone(f.get(c)))
		}
	}
	return out
}

// OwnerSettings returns the owner settings c sets, which [Config.AgentSettings]
// leaves out.
func (c *Config) OwnerSettings() []string {
	var out []string
	for _, f := range fields {
		if f.scope == scopeOwner && len(f.get(c)) > 0 {
			out = append(out, f.flag)
		}
	}
	return out
}

// Change is the difference of one setting between two configs.
type Change struct {
	Setting string `json:"setting"`
	// Old and New are set for scalars.
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
	// Added and Removed are set for lists.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Live    bool     `json:"live"`
}

// Diff returns the settings which differ from old to next.
func Diff(old, next *Config) []Change {
	var out []Change
	for _, f := range fields {
		o, n := f.get(old), f.get(next)
		if slices.Equal(o, n) {
			continue
		}
		c := Change{Setting: f.flag, Live: f.live}
		if f.list {
			for _, e := range n {
				if !slices.Contains(o, e) {
					c.Added = append(c.Added, e)
				}
			}
			for _, e := range o {
				if !slices.Contains(n, e) {
					c.Removed = append(c.Removed, e)
				}
			}
		} else {
			c.Old, c.New = first(o), first(n)
		}
		out = append(out, c)
	}
	return out
}

// Unset removes entries from a list setting, or the whole list without
// entries, or resets a scalar setting to its default. It returns the names
// of stored secrets which are no longer used, to delete.
func (d Dir) Unset(cfg *Config, setting string, entries []string) ([]string, error) {
	before := cfg.referencedSecrets()
	var deleted []string
	f, ok := lookupField(setting)
	secret, extra := unsetOnly[setting]
	switch {
	case extra && len(entries) > 0:
		return nil, fmt.Errorf("%s takes no entries: run `a2a-serve config unset %s`", setting, setting)
	case extra:
		deleted = append(deleted, secret)
	case ok && f.place == placeChannel:
		kind, _, _ := strings.Cut(setting, "-")
		return nil, fmt.Errorf("%s is a channel setting: change it with `a2a-serve channel add %s`, or remove the channel with `a2a-serve channel rm %s`", setting, kind, kind)
	case !ok:
		return nil, fmt.Errorf("unknown setting %q: want one of %s", setting, strings.Join(SettingNames(), ", "))
	case setting == "agent":
		return nil, fmt.Errorf("the agent can't be unset; switch it with `a2a-serve config set --agent <name>`")
	case !f.list && len(entries) > 0:
		return nil, fmt.Errorf("%s is not a list: run `a2a-serve config unset %s` to reset it", setting, setting)
	case setting == "name":
		cfg.Name = d.DefaultName()
	case f.list && len(entries) > 0:
		cur := f.get(cfg)
		for _, arg := range entries {
			k := f.key(arg)
			i := slices.IndexFunc(cur, func(e string) bool { return f.key(e) == k })
			if i < 0 {
				return nil, fmt.Errorf("%s %s is not set", setting, arg)
			}
			cur = slices.Delete(slices.Clone(cur), i, i+1)
		}
		f.put(cfg, cur)
	default:
		f.put(cfg, nil)
	}
	if e := cfg.Channels.Email; e != nil && *e == (EmailConfig{}) {
		cfg.Channels.Email = nil
	}
	after := cfg.referencedSecrets()
	for name := range before {
		if !after[name] {
			deleted = append(deleted, name)
		}
	}
	slices.Sort(deleted)
	return deleted, nil
}

// Commands returns the commands which recreate the agent in an empty
// directory: `a2a-serve start`, then `config set` for the settings only it
// takes, then `channel add` for each channel. Secrets are passed as
// env:NAME; stored is used to tell which network secrets are set.
func (c *Config) Commands(stored Secrets) [][]string {
	h := c.Harness
	if !slices.Contains(Harnesses, h) {
		h = HarnessACP
	}
	start := []string{"a2a-serve", "start", h}
	set := []string{"a2a-serve", "config", "set"}
	channels := map[string][]string{}
	for _, f := range fields {
		if f.flag == "agent" || (f.flag == "name" && c.Name == "") {
			continue
		}
		switch f.place {
		case placeCore:
			start = appendFlag(start, f.flag, f.get(c)...)
		case placeOperational:
			set = appendFlag(set, f.flag, f.get(c)...)
		case placeChannel:
			kind, _, _ := strings.Cut(f.flag, "-")
			flag := f.flag
			if kind != ChannelEmail {
				flag = "token"
			}
			channels[kind] = appendFlag(channels[kind], flag, f.get(c)...)
		}
	}
	for _, flag := range slices.Sorted(maps.Keys(unsetOnly)) {
		if name := unsetOnly[flag]; stored[name] != "" {
			set = appendFlag(set, flag, SecretPrefix+name)
		}
	}
	out := [][]string{start}
	if len(set) > 3 {
		out = append(out, set)
	}
	for _, kind := range ChannelKinds {
		if args := channels[kind]; len(args) > 0 {
			out = append(out, append([]string{"a2a-serve", "channel", "add", kind}, args...))
		}
	}
	return out
}

// appendFlag appends --flag for each value, with stored secrets as env:NAME.
func appendFlag(args []string, flag string, values ...string) []string {
	for _, v := range values {
		if name, ok := strings.CutPrefix(v, SecretPrefix); ok {
			v = "env:" + name
		}
		args = append(args, "--"+flag, v)
	}
	return args
}

// ShellJoin quotes args for a POSIX shell.
func ShellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}

// shellSplit splits a command line ShellJoin made.
func shellSplit(s string) []string {
	if s == "" {
		return nil
	}
	argv, err := shlex.Split(s)
	if err != nil {
		// ShellJoin quotes every word, so this only happens for a line
		// written by hand; it runs as one word.
		return []string{s}
	}
	return argv
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@,+%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CopyLive copies the settings a running server applies without a restart,
// other than channels, from src to dst.
func CopyLive(dst, src *Config) {
	dst.Description = src.Description
	dst.Approvals.OwnerChat = src.Approvals.OwnerChat
	dst.Approvals.Allow = slices.Clone(src.Approvals.Allow)
	dst.Approvals.Deny = slices.Clone(src.Approvals.Deny)
	dst.Policy = src.Policy.clone()
}

// IsAccess reports whether the setting changes who may call the agent: an
// access setting, or a network.
func IsAccess(setting string) bool {
	return slices.Contains([]string{"private", "public", "private-access", "public-access", "allow-email"}, setting)
}
