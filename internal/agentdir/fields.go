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
	"os"
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
	// placeAgent is --agent of `config set`, which `start` takes as its
	// argument.
	placeAgent
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

// Help sections of the settings flags.
const (
	SectionGeneral   = "General"
	SectionWorkspace = "Workspace"
	SectionSandbox   = "Sandbox"
	SectionNetwork   = "Network"
	SectionAccess    = "Access"
	SectionApprovals = "Approvals"
)

// field is a setting, named by its flag. A scalar has at most one entry.
type field struct {
	access
	flag string
	// path is the dotted key in config.yaml, empty for a stored secret.
	path    string
	usage   string
	section string
	place   placement
	scope   scope
	// live settings are applied by a running server without a restart.
	live bool
	// split says commas also separate the entries of a list flag.
	split bool
	// secret is the name a secret value of the flag is stored under.
	secret string
	// secretOnly settings are kept in the secrets file alone.
	secretOnly bool
	// values are the completions of the flag.
	values []string
	// key returns the key of an entry, or of an argument naming it.
	key func(string) string
	// parse normalizes an entry of the flag before it is merged.
	parse func(d Dir, v string) (string, error)
	// merge replaces the default merge of the flag's entries into cfg.
	merge func(s *Settings, cfg *Config, d Dir, secrets Secrets) error
}

// access reads and writes a setting of a config, and points to the value of
// its flag in [Settings].
type access struct {
	list bool
	get  func(*Config) []string
	// put replaces the entries with what get returns; nil resets the setting.
	put func(*Config, []string)
	// arg points to the flag value: a *string, *int or *[]string.
	arg func(*Settings) any
}

func str(p func(*Config) *string, arg func(*Settings) *string) access {
	return access{
		get: func(c *Config) []string { return nonEmpty(*p(c)) },
		put: func(c *Config, v []string) { *p(c) = first(v) },
		arg: func(s *Settings) any { return arg(s) },
	}
}

func num(p func(*Config) *int, arg func(*Settings) *int) access {
	return access{
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
		arg: func(s *Settings) any { return arg(s) },
	}
}

func strs(p func(*Config) *[]string, arg func(*Settings) *[]string) access {
	return access{
		list: true,
		get:  func(c *Config) []string { return *p(c) },
		put:  func(c *Config, v []string) { *p(c) = v },
		arg:  func(s *Settings) any { return arg(s) },
	}
}

func email(p func(*EmailConfig) *string, arg func(*EmailSettings) *string) access {
	return access{
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
		arg: func(s *Settings) any {
			if s.Email == nil {
				s.Email = &EmailSettings{}
			}
			return arg(s.Email)
		},
	}
}

func token(kind string, set func(*Config, string), arg func(*Settings) *string) access {
	return access{
		get: func(c *Config) []string { return nonEmpty(c.ChannelToken(kind)) },
		put: func(c *Config, v []string) { set(c, first(v)) },
		arg: func(s *Settings) any { return arg(s) },
	}
}

// storedSecret is a setting kept in the secrets file alone.
func storedSecret(arg func(*Settings) *string) access {
	return access{
		get: func(*Config) []string { return nil },
		put: func(*Config, []string) {},
		arg: func(s *Settings) any { return arg(s) },
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

func trimSpace(_ Dir, v string) (string, error) { return strings.TrimSpace(v), nil }

// parseNetwork reads "none" as no provider.
func parseNetwork(_ Dir, v string) (string, error) {
	if v == NetworkNone {
		return "", nil
	}
	return v, nil
}

func parseCommand(_ Dir, v string) (string, error) {
	argv, err := shlex.Split(v)
	if err != nil || len(argv) == 0 {
		return "", fmt.Errorf("invalid command line %q", v)
	}
	return ShellJoin(argv), nil
}

// parseWorkspace returns the spec of a workspace, empty for the directory
// holding .a2a.
func parseWorkspace(d Dir, v string) (string, error) {
	m, err := ParseMount(v)
	if err != nil {
		return "", err
	}
	if m == (Mount{Src: d.Root, Dst: d.Root}) {
		return "", nil
	}
	return m.String(), nil
}

// parsePath returns v as an absolute path, with ~ expanded.
func parsePath(_ Dir, v string) (string, error) {
	p, err := expandHome(v)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

func parseMountArg(_ Dir, v string) (string, error) {
	m, err := ParseMount(v)
	if err != nil {
		return "", err
	}
	return m.String(), nil
}

// parseEnvRef reads env:VAR from the environment of the CLI.
func parseEnvRef(_ Dir, v string) (string, error) {
	name, ok := strings.CutPrefix(v, "env:")
	if !ok {
		return v, nil
	}
	if v = os.Getenv(name); v == "" {
		return "", fmt.Errorf("environment variable %s is empty", name)
	}
	return v, nil
}

var fields = []field{
	{
		access: str(func(c *Config) *string { return &c.Harness }, func(s *Settings) *string { return &s.Harness }),
		flag:   "agent", path: "agent", place: placeAgent, scope: scopeAgent, section: SectionGeneral,
		usage:  "Agent: claude, opencode, or acp with --command",
		values: append(slices.Clone(Harnesses), HarnessACP),
	},
	{
		access: str(func(c *Config) *string { return &c.Name }, func(s *Settings) *string { return &s.Name }),
		flag:   "name", path: "name", scope: scopeOwner, section: SectionGeneral,
		usage: "Agent name, also its tailnet machine name (default: the directory name)",
	},
	{
		access: str(func(c *Config) *string { return &c.Description }, func(s *Settings) *string { return &s.Description }),
		flag:   "description", path: "description", scope: scopeAgent, live: true, section: SectionGeneral,
		usage: "Agent description for its card",
	},
	{
		access: access{
			get: func(c *Config) []string { return nonEmpty(ShellJoin(c.Command)) },
			put: func(c *Config, v []string) { c.Command = shellSplit(first(v)) },
			arg: func(s *Settings) any { return &s.Command },
		},
		flag: "command", path: "command", scope: scopeAgent, section: SectionSandbox, parse: parseCommand,
		usage: "Command line of the agent, as shell `words`, such as \"cloudcode acp\"; replaces its default command",
	},
	{
		access: str(func(c *Config) *string { return &c.Model }, func(s *Settings) *string { return &s.Model }),
		flag:   "model", path: "model", scope: scopeAgent, section: SectionGeneral,
		usage: "Model of the agent, in its own format: provider/model for opencode, such as openrouter/~google/gemini-flash-latest, or a Claude model for claude, such as sonnet (default: the agent's)",
	},
	{
		access: str(func(c *Config) *string { return &c.Image }, func(s *Settings) *string { return &s.Image }),
		flag:   "image", path: "image", scope: scopeAgent, section: SectionSandbox,
		usage: "Container image of the agent; only applies to --sandbox docker",
	},
	{
		access: str(func(c *Config) *string { return &c.Sandbox }, func(s *Settings) *string { return &s.Sandbox }),
		flag:   "sandbox", path: "sandbox", scope: scopeAgent, section: SectionSandbox,
		usage:  "Where the agent runs: docker (default) or local",
		values: []string{SandboxDocker, SandboxLocal},
	},
	{
		access: str(func(c *Config) *string { return &c.Workspace }, func(s *Settings) *string { return &s.Workspace }),
		flag:   "workspace", path: "workspace", scope: scopeOwner, section: SectionWorkspace, parse: parseWorkspace,
		usage: "Workspace directory: `src[:dst][:ro]`, such as ~/src/app (default: the directory holding .a2a)",
	},
	{
		access: strs(func(c *Config) *[]string { return &c.Mounts }, func(s *Settings) *[]string { return &s.Mounts }),
		flag:   "mount", path: "mounts", scope: scopeAgent, section: SectionWorkspace, key: mountArgKey, parse: parseMountArg,
		usage: "Extra directory to mount: `src[:dst][:ro]`, such as ~/docs:/docs:ro; keyed by dst",
	},
	{
		access: strs(func(c *Config) *[]string { return &c.AllowBroadMounts }, func(s *Settings) *[]string { return &s.AllowBroadMounts }),
		flag:   "allow-broad-mount", path: "allowBroadMounts", scope: scopeOwner, section: SectionWorkspace, key: identity, parse: parsePath,
		usage: "Let the Docker sandbox mount a `path` that is your home directory, / or a directory that holds your home directory; the agent can then read your credentials and the secrets of the agents in it",
	},
	{
		access: strs(func(c *Config) *[]string { return &c.Secrets }, func(s *Settings) *[]string { return &s.Secrets }),
		flag:   "secret", path: "secrets", scope: scopeAgent, section: SectionWorkspace, key: identity, merge: mergeSecrets,
		usage: "Environment variable to pass to the agent, such as an API key: `NAME[=value]`, stored in .a2a/state/secrets; without a value, read from your environment or prompted",
	},
	{
		access: strs(func(c *Config) *[]string { return &c.Env }, func(s *Settings) *[]string { return &s.Env }),
		flag:   "env", path: "env", scope: scopeAgent, section: SectionWorkspace, key: envKey,
		usage: "Extra variable for the agent: `NAME=value`, such as DEBUG=1; keyed by NAME",
	},
	{
		access: num(func(c *Config) *int { return &c.Port }, func(s *Settings) *int { return &s.Port }),
		flag:   "port", path: "port", scope: scopeOwner, section: SectionNetwork,
		usage: "Localhost port, 1-65535 (default: a free port, kept across runs)",
	},
	{
		access: str(func(c *Config) *string { return &c.Network.Private }, func(s *Settings) *string { return &s.Private }),
		flag:   "private", path: "network.private", scope: scopeOwner, section: SectionNetwork, parse: parseNetwork,
		usage:  "Private network: tailscale, none, or the URL of an Agent Mesh relay such as https://relay.example.com",
		values: []string{NetworkTailscale, NetworkNone},
	},
	{
		access: str(func(c *Config) *string { return &c.Network.Public }, func(s *Settings) *string { return &s.Public }),
		flag:   "public", path: "network.public", scope: scopeOwner, section: SectionNetwork, parse: parseNetwork,
		usage:  "Public URL: tailscale (Funnel), none, or the URL of an Agent Mesh relay",
		values: []string{NetworkTailscale, NetworkNone},
	},
	{
		access: str(func(c *Config) *string { return &c.Policy.PrivateAllowIncoming }, func(s *Settings) *string { return &s.PrivateAllowIncoming }),
		flag:   "private-allow-incoming", path: "policy.privateAllowIncoming", scope: scopeOwner, live: true, section: SectionAccess,
		usage:  "Who on the private network may call: everyone (or anyone), or paired for only you and your pairings (default everyone)",
		values: []string{AccessEveryone, AccessPaired},
	},
	{
		access: str(func(c *Config) *string { return &c.Policy.PublicAllowIncoming }, func(s *Settings) *string { return &s.PublicAllowIncoming }),
		flag:   "public-allow-incoming", path: "policy.publicAllowIncoming", scope: scopeOwner, live: true, section: SectionAccess,
		usage:  "Who on the public URL and in direct chats may call: paired for only you and your pairings, or anyone (everyone works too) (default paired)",
		values: []string{AccessPaired, AccessAnyone},
	},
	{
		access: str(func(c *Config) *string { return &c.Policy.PrivateAllowOutgoing }, func(s *Settings) *string { return &s.PrivateAllowOutgoing }),
		flag:   "private-allow-outgoing", path: "policy.privateAllowOutgoing", scope: scopeOwner, live: true, section: SectionAccess,
		usage:  "Whom on the private network the agent may call: everyone, or contacts for only its contacts (default contacts)",
		values: []string{AccessEveryone, AccessContacts},
	},
	{
		access: strs(func(c *Config) *[]string { return &c.Policy.AllowEmails }, func(s *Settings) *[]string { return &s.AllowEmails }),
		flag:   "allow-email", path: "policy.allowEmails", scope: scopeOwner, live: true, section: SectionAccess, split: true,
		key: strings.ToLower, parse: trimSpace,
		usage: "People who may call the agent by a verified email, on any network: an `email`, or *@domain for Google Workspace sign-in or mail that passes DMARC; turns on Google sign-in at the public URL",
	},
	{
		access: str(func(c *Config) *string { return &c.Network.TailscaleControlURL }, func(s *Settings) *string { return &s.TailscaleControlURL }),
		flag:   "tailscale-control-url", path: "network.tailscaleControlURL", place: placeOperational, scope: scopeOwner, section: SectionNetwork,
		usage: "Headscale URL, such as https://headscale.example.com, instead of Tailscale's control server",
	},
	{
		access: storedSecret(func(s *Settings) *string { return &s.TailscaleAuthKey }),
		flag:   "tailscale-authkey", place: placeOperational, scope: scopeOwner, section: SectionNetwork,
		secret: SecretTailscaleKey, secretOnly: true,
		usage: "Tailscale auth key, to join without the login link",
	},
	{
		access: storedSecret(func(s *Settings) *string { return &s.RelayToken }),
		flag:   "relay-token", place: placeOperational, scope: scopeOwner, section: SectionNetwork,
		secret: SecretRelayToken, secretOnly: true,
		usage: "Join token of an Agent Mesh relay, to join without the sign-in link",
	},
	{
		access: token(ChannelTelegram, func(c *Config, v string) {
			c.Channels.Telegram = nil
			if v != "" {
				c.Channels.Telegram = &TelegramConfig{Token: v}
			}
		}, func(s *Settings) *string { return &s.Telegram }),
		flag: ChannelTelegram, path: "channels.telegram.token", place: placeChannel, scope: scopeOwner, live: true, secret: SecretTelegram,
	},
	{
		access: token(ChannelDiscord, func(c *Config, v string) {
			c.Channels.Discord = nil
			if v != "" {
				c.Channels.Discord = &DiscordConfig{Token: v}
			}
		}, func(s *Settings) *string { return &s.Discord }),
		flag: ChannelDiscord, path: "channels.discord.token", place: placeChannel, scope: scopeOwner, live: true, secret: SecretDiscord,
	},
	emailField("address", "address", "Email channel: the agent's address",
		email(func(e *EmailConfig) *string { return &e.Address }, func(e *EmailSettings) *string { return &e.Address })),
	emailField("imap", "imap", "Email channel: IMAP host:port, such as imap.example.com:993",
		email(func(e *EmailConfig) *string { return &e.IMAP }, func(e *EmailSettings) *string { return &e.IMAP })),
	emailField("smtp", "smtp", "Email channel: SMTP host:port, such as smtp.example.com:465",
		email(func(e *EmailConfig) *string { return &e.SMTP }, func(e *EmailSettings) *string { return &e.SMTP })),
	emailField("username", "username", "Email channel: login (default: the address)",
		email(func(e *EmailConfig) *string { return &e.Username }, func(e *EmailSettings) *string { return &e.Username })),
	emailSecret(emailField("password", "password", "Email channel: password",
		email(func(e *EmailConfig) *string { return &e.Password }, func(e *EmailSettings) *string { return &e.Password })), SecretEmailPassword),
	emailField("mailbox", "mailbox", "Email channel: mailbox to watch (default INBOX)",
		email(func(e *EmailConfig) *string { return &e.Mailbox }, func(e *EmailSettings) *string { return &e.Mailbox })),
	emailField("trusted-authserv", "trustedAuthServ", "Email channel: authserv-id of your mail server's Authentication-Results",
		email(func(e *EmailConfig) *string { return &e.TrustedAuthServ }, func(e *EmailSettings) *string { return &e.TrustedAuthServ })),
	emailField("oauth-token-url", "oauthTokenURL", "Email channel: OAuth token endpoint, such as https://login.microsoftonline.com/common/oauth2/v2.0/token",
		email(func(e *EmailConfig) *string { return &e.OAuthTokenURL }, func(e *EmailSettings) *string { return &e.OAuthTokenURL })),
	emailField("oauth-client-id", "oauthClientID", "Email channel: OAuth client ID",
		email(func(e *EmailConfig) *string { return &e.OAuthClientID }, func(e *EmailSettings) *string { return &e.OAuthClientID })),
	emailSecret(emailField("oauth-client-secret", "oauthClientSecret", "Email channel: OAuth client secret, if the client has one",
		email(func(e *EmailConfig) *string { return &e.OAuthClientSecret }, func(e *EmailSettings) *string { return &e.OAuthClientSecret })), SecretEmailClient),
	emailSecret(emailField("oauth-refresh-token", "oauthRefreshToken", "Email channel: OAuth refresh token, which signs in with OAuth (XOAUTH2); the password is then unused",
		email(func(e *EmailConfig) *string { return &e.OAuthRefreshToken }, func(e *EmailSettings) *string { return &e.OAuthRefreshToken })), SecretEmailOAuth),
	{
		access: strs(func(c *Config) *[]string { return &c.Approvals.Allow }, func(s *Settings) *[]string { return &s.AllowTools }),
		flag:   "allow-tools", path: "approvals.allow", scope: scopeAgent, live: true, section: SectionApprovals, split: true, key: identity,
		usage:  "ACP tool `kinds` approved without asking: " + strings.Join(ToolKinds, ", ") + ", or a glob",
		values: ToolKinds,
	},
	{
		access: strs(func(c *Config) *[]string { return &c.Approvals.Deny }, func(s *Settings) *[]string { return &s.DenyTools }),
		flag:   "deny-tools", path: "approvals.deny", scope: scopeAgent, live: true, section: SectionApprovals, split: true, key: identity,
		usage:  "ACP tool `kinds` denied without asking, like --allow-tools",
		values: ToolKinds,
	},
	{
		access: strs(func(c *Config) *[]string { return &c.MCP.Disabled }, func(s *Settings) *[]string { return &s.DisableMCP }),
		flag:   "disable-mcp", path: "mcp.disabled", scope: scopeOwner, live: true, section: SectionWorkspace, split: true, key: identity, parse: trimSpace,
		usage: "MCP `servers` of the project's .mcp.json the agent doesn't get",
	},
	{
		access: str(func(c *Config) *string { return &c.Approvals.OwnerChat }, func(s *Settings) *string { return &s.OwnerChat }),
		flag:   "owner-chat", path: "approvals.ownerChat", scope: scopeOwner, live: true, section: SectionApprovals,
		usage:  "Where an owner answers the approvals of tasks nobody watches: `channel[:who]`, with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner)",
		values: ChannelKinds,
	},
	{
		access: str(func(c *Config) *string { return &c.Limits.Memory }, func(s *Settings) *string { return &s.Memory }),
		flag:   "memory", path: "limits.memory", place: placeOperational, scope: scopeAgent, section: SectionSandbox,
		usage: "Container memory limit, such as 4g or 512m (default 4g)",
	},
	{
		access: str(func(c *Config) *string { return &c.Limits.CPUs }, func(s *Settings) *string { return &s.CPUs }),
		flag:   "cpus", path: "limits.cpus", place: placeOperational, scope: scopeAgent, section: SectionSandbox,
		usage: "Container CPU limit, such as 2 or 1.5 (default 2)",
	},
	{
		access: num(func(c *Config) *int { return &c.Limits.PIDs }, func(s *Settings) *int { return &s.PIDs }),
		flag:   "pids", path: "limits.pids", place: placeOperational, scope: scopeAgent, section: SectionSandbox,
		usage: "Container process limit (default 512)",
	},
	{
		access: str(func(c *Config) *string { return &c.Auth.GoogleClientID }, func(s *Settings) *string { return &s.GoogleClientID }),
		flag:   "google-client-id", path: "auth.googleClientID", place: placeOperational, scope: scopeOwner, section: SectionAccess, parse: parseEnvRef,
		usage: "Client ID of your own Google app for sign-in (optional)",
	},
	{
		access: str(func(c *Config) *string { return &c.Auth.GoogleClientSecret }, func(s *Settings) *string { return &s.GoogleClientSecret }),
		flag:   "google-client-secret", path: "auth.googleClientSecret", place: placeOperational, scope: scopeOwner, section: SectionAccess,
		secret: SecretGoogleClient,
		usage:  "Client secret of your own Google app",
	},
	{
		access: str(func(c *Config) *string { return &c.Backup.To }, func(s *Settings) *string { return &s.Backup }),
		flag:   "backup", path: "backup.to", place: placeOperational, scope: scopeOwner, section: SectionGeneral,
		usage: "Take backup snapshots to a directory, gs://bucket/prefix or s3://bucket/prefix",
	},
	{
		access: str(func(c *Config) *string { return &c.Backup.Key }, func(s *Settings) *string { return &s.BackupKey }),
		flag:   "backup-key", path: "backup.key", place: placeOperational, scope: scopeOwner, section: SectionGeneral,
		secret: SecretBackupKey,
		usage:  "Passphrase the snapshots are encrypted with; keep a copy away from the backups",
	},
	{
		access: str(func(c *Config) *string { return &c.Backup.Interval }, func(s *Settings) *string { return &s.BackupInterval }),
		flag:   "backup-interval", path: "backup.interval", place: placeOperational, scope: scopeOwner, section: SectionGeneral,
		usage: "Time between snapshots, a `duration` such as 30m or 1h (default 24h)",
	},
	{
		access: str(func(c *Config) *string { return &c.Backup.Retention }, func(s *Settings) *string { return &s.BackupRetention }),
		flag:   "backup-retention", path: "backup.retention", place: placeOperational, scope: scopeOwner, section: SectionGeneral,
		usage: "How long snapshots are kept, a `duration` in hours or days such as 48h or 7d; the latest is always kept (default 7d)",
	},
	{
		access: str(func(c *Config) *string { return &c.TaskTTL }, func(s *Settings) *string { return &s.TaskTTL }),
		flag:   "task-ttl", path: "taskTTL", place: placeOperational, scope: scopeOwner, section: SectionGeneral,
		usage: "How long tasks are kept after their last update, a `duration` in hours or days such as 12h or 30d (default 30d); a chat conversation starts over when its last task is deleted",
	},
}

// emailField is the --email-<name> setting of the email channel, with the
// key in channels.email.
func emailField(name, key, usage string, a access) field {
	return field{
		access: a, flag: ChannelEmail + "-" + name, path: "channels.email." + key,
		place: placeChannel, scope: scopeOwner, live: true, usage: usage,
	}
}

func emailSecret(f field, name string) field {
	f.secret = name
	return f
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
	var out []string
	for _, f := range fields {
		if f.place != placeChannel {
			out = append(out, f.flag)
		}
	}
	slices.Sort(out)
	return out
}

// CheckUnset reports why `config unset` refuses a setting by its name alone,
// before the agent's config is read.
func CheckUnset(setting string) error {
	f, ok := lookupField(setting)
	switch {
	case !ok:
		return &SettingsError{Err: fmt.Errorf("unknown setting %q: want one of %s", setting, strings.Join(SettingNames(), ", "))}
	case f.place == placeChannel:
		kind, _, _ := strings.Cut(setting, "-")
		return &SettingsError{Err: fmt.Errorf("%s is a channel setting: change it with `a2a-serve channel add %s`, or remove the channel with `a2a-serve channel rm %s`", setting, kind, kind)}
	case f.place == placeAgent:
		return &SettingsError{Err: fmt.Errorf("the agent can't be unset; switch it with `a2a-serve config set --agent <name>`")}
	}
	return nil
}

// liveSetting reports whether f is a setting of `config set` that a running
// server applies without a restart.
func (f field) liveSetting() bool { return f.live && f.place != placeChannel }

// LiveSettings returns the names of the settings of `config set` that a
// running server applies without a restart, in the order of the fields.
func LiveSettings() []string {
	var out []string
	for _, f := range fields {
		if f.liveSetting() {
			out = append(out, f.flag)
		}
	}
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
	if err := CheckUnset(setting); err != nil {
		return nil, err
	}
	before := cfg.referencedSecrets()
	var deleted []string
	f, _ := lookupField(setting)
	switch {
	case f.secretOnly && len(entries) > 0:
		return nil, fmt.Errorf("%s takes no entries: run `a2a-serve config unset %s`", setting, setting)
	case f.secretOnly:
		deleted = append(deleted, f.secret)
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
		if f.flag == "name" && c.Name == "" {
			continue
		}
		switch {
		case f.secretOnly:
			if stored[f.secret] != "" {
				set = appendFlag(set, f.flag, SecretPrefix+f.secret)
			}
		case f.place == placeCore:
			start = appendFlag(start, f.flag, f.get(c)...)
		case f.place == placeOperational:
			set = appendFlag(set, f.flag, f.get(c)...)
		case f.place == placeChannel:
			kind, _, _ := strings.Cut(f.flag, "-")
			flag := f.flag
			if kind != ChannelEmail {
				flag = "token"
			}
			channels[kind] = appendFlag(channels[kind], flag, f.get(c)...)
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
	for _, f := range fields {
		if f.liveSetting() {
			f.put(dst, slices.Clone(f.get(src)))
		}
	}
}

// IsAccess reports whether the setting changes who may call the agent: an
// access setting, or a network.
func IsAccess(setting string) bool {
	return slices.Contains([]string{"private", "public", "private-allow-incoming", "public-allow-incoming", "allow-email"}, setting)
}

// label names the flag of f in errors.
func (f field) label() string {
	if f.flag == ChannelTelegram || f.flag == ChannelDiscord {
		return f.flag + " token"
	}
	return "--" + f.flag
}

// Flag is a settings flag of the CLI.
type Flag struct {
	Name    string
	Usage   string
	Section string
	// Value points to the value of the flag in [Settings]: a *string, *int
	// or *[]string.
	Value any
	// Split says commas also separate the values of a list.
	Split bool
	// Secret says the flag takes secret values.
	Secret bool
	// Values are the completions of the flag.
	Values []string
}

// FlagSet selects the settings flags of a command.
type FlagSet int

const (
	// AgentFlag is --agent, which `start` takes as its argument.
	AgentFlag FlagSet = 1 << iota
	// CoreFlags are the flags of `start <agent>` and `config set`.
	CoreFlags
	// OperationalFlags are the flags of `config set` only: limits,
	// network-provider options, backups and Google sign-in.
	OperationalFlags
	// EmailFlags are the --email-* flags of `channel add email`.
	EmailFlags
)

func (p placement) flagSet() FlagSet {
	switch p {
	case placeAgent:
		return AgentFlag
	case placeOperational:
		return OperationalFlags
	case placeChannel:
		return EmailFlags
	}
	return CoreFlags
}

// Flags returns the flags of sets, bound to the fields of s.
func (s *Settings) Flags(sets FlagSet) []Flag {
	var out []Flag
	for _, f := range fields {
		// The bot tokens have no usage: they are the --token flag of
		// `channel add`.
		if sets&f.place.flagSet() == 0 || f.usage == "" {
			continue
		}
		out = append(out, Flag{
			Name: f.flag, Usage: f.usage, Section: f.section, Value: f.arg(s), Split: f.split, Values: f.values,
			// --secret takes NAME=value, stored under NAME.
			Secret: f.secret != "" || f.flag == "secret",
		})
	}
	return out
}

// SettingDoc describes a setting for the configuration reference.
type SettingDoc struct {
	// Flag is the flag which changes the setting, and Commands the commands
	// which take it.
	Flag     string
	Commands []string
	// Key is the dotted key in config.yaml. A setting without one is kept
	// in the secrets file as Secret.
	Key    string
	Secret string
}

// SettingDocs describes the settings.
func SettingDocs() []SettingDoc {
	var out []SettingDoc
	for _, f := range fields {
		d := SettingDoc{Flag: f.flag, Key: f.path}
		if f.secretOnly {
			d.Secret = f.secret
		}
		switch f.place {
		case placeCore:
			d.Commands = []string{"start <agent>", "config set"}
		case placeAgent, placeOperational:
			d.Commands = []string{"config set"}
		case placeChannel:
			kind, _, _ := strings.Cut(f.flag, "-")
			d.Commands = []string{"channel add " + kind}
			if kind != ChannelEmail {
				d.Flag = "token"
			}
		}
		out = append(out, d)
	}
	return out
}
