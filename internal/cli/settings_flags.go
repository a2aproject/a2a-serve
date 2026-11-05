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

package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/google/shlex"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/secretref"
)

// settingsFlags are the agent settings shared by `a2a-serve start <agent>` and
// `a2a-serve config set`, so both speak one vocabulary.
type settingsFlags struct {
	s       agentdir.Settings
	command string
}

// settingsSet selects the settings flags a command takes.
type settingsSet int

const (
	// coreSettings are the flags of `start <agent>` and `config set`.
	coreSettings settingsSet = 1 << iota
	// operationalSettings are the flags of `config set` only: limits,
	// network-provider options, backups and Google sign-in.
	operationalSettings
	// harnessSetting is --agent, which `start` takes as its argument.
	harnessSetting
)

// secretUsage is the help of a flag which takes a secret value.
func secretUsage(usage string) string {
	return usage + " (" + secretref.Syntax + ")"
}

// listUsage labels the help of a list flag. split says whether commas also
// separate values.
func listUsage(usage string, split bool) string {
	if split {
		return usage + " (repeatable; commas also separate values)"
	}
	return usage + " (repeatable)"
}

func (sf *settingsFlags) register(cmd *cobra.Command, sets settingsSet) {
	f := cmd.Flags()
	s := &sf.s
	if sets&harnessSetting != 0 {
		f.StringVar(&s.Harness, "agent", "", "Agent: claude, opencode, or acp with --command")
		must(cmd.RegisterFlagCompletionFunc("agent", fixedCompletions(append(slices.Clone(agentdir.Harnesses), agentdir.HarnessACP)...)))
		setSection(f, sectionGeneral, "agent")
	}
	if sets&coreSettings != 0 {
		sf.registerCore(cmd)
	}
	if sets&operationalSettings != 0 {
		sf.registerOperational(cmd)
	}
}

func (sf *settingsFlags) registerCore(cmd *cobra.Command) {
	f := cmd.Flags()
	s := &sf.s
	commandUsage := "Command line of the agent, as shell `words`, such as \"cloudcode acp\"; replaces its default command"
	if cmd.Name() == agentdir.HarnessACP {
		commandUsage = "Command line of the ACP agent, as shell `words`, such as \"gemini --experimental-acp\""
	}
	f.StringVar(&s.Name, "name", "", "Agent name, also its tailnet machine name (default: the directory name)")
	f.StringVar(&s.Description, "description", "", "Agent description for its card")
	f.StringVar(&s.Workspace, "workspace", "", "Workspace directory: `src[:dst][:ro]`, such as ~/src/app (default: the directory holding .a2a)")
	f.StringArrayVar(&s.Mounts, "mount", nil, listUsage("Extra directory to mount: `src[:dst][:ro]`, such as ~/docs:/docs:ro; keyed by dst", false))
	f.StringArrayVar(&s.Env, "env", nil, listUsage("Extra variable for the agent: `NAME=value`, such as DEBUG=1; keyed by NAME", false))
	f.StringArrayVar(&s.Secrets, "secret", nil, listUsage(secretUsage("Environment variable to pass to the agent, such as an API key: `NAME[=value]`, stored in .a2a/state/secrets; without a value, read from your environment or prompted"), false))
	f.StringVar(&s.Sandbox, "sandbox", "", "Where the agent runs: docker (default) or local")
	f.StringVar(&s.Image, "image", "", "Container image of the agent; only applies to --sandbox docker")
	f.StringVar(&sf.command, "command", "", commandUsage)
	f.IntVar(&s.Port, "port", 0, "Localhost port, 1-65535 (default: a free port, kept across runs)")
	f.StringVar(&s.Private, "private", "", "Private network: tailscale, none, or the URL of an Agent Mesh relay such as https://relay.example.com")
	f.StringVar(&s.Public, "public", "", "Public URL: tailscale (Funnel), none, or the URL of an Agent Mesh relay")
	f.StringVar(&s.PrivateAccess, "private-access", "", "Who on the private network may call: everyone, or paired for only you and your pairings (default everyone; paired on a relay that admits people by sign-in)")
	f.StringVar(&s.PublicAccess, "public-access", "", "Who on the public URL and in direct chats may call: paired for only you and your pairings, or anyone (default paired)")
	f.StringSliceVar(&s.AllowEmails, "allow-email", nil, listUsage("People who may call the agent by a verified email, on any network: an `email`, or *@domain for Google Workspace sign-in or mail that passes DMARC; turns on Google sign-in at the public URL", true))
	f.StringSliceVar(&s.AllowTools, "allow-tools", nil, listUsage("ACP tool `kinds` approved without asking: "+strings.Join(agentdir.ToolKinds, ", ")+", or a glob", true))
	f.StringSliceVar(&s.DenyTools, "deny-tools", nil, listUsage("ACP tool `kinds` denied without asking, like --allow-tools", true))
	f.StringVar(&s.OwnerChat, "owner-chat", "", "Where an owner answers the approvals of tasks nobody watches: `channel[:who]`, with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner)")
	must(cmd.RegisterFlagCompletionFunc("private", fixedCompletions(agentdir.NetworkTailscale, agentdir.NetworkNone)))
	must(cmd.RegisterFlagCompletionFunc("public", fixedCompletions(agentdir.NetworkTailscale, agentdir.NetworkNone)))
	must(cmd.RegisterFlagCompletionFunc("private-access", fixedCompletions(agentdir.AccessEveryone, agentdir.AccessPaired)))
	must(cmd.RegisterFlagCompletionFunc("public-access", fixedCompletions(agentdir.AccessPaired, agentdir.AccessAnyone)))
	must(cmd.RegisterFlagCompletionFunc("sandbox", fixedCompletions(agentdir.SandboxDocker, agentdir.SandboxLocal)))
	must(cmd.RegisterFlagCompletionFunc("allow-tools", fixedCompletions(agentdir.ToolKinds...)))
	must(cmd.RegisterFlagCompletionFunc("deny-tools", fixedCompletions(agentdir.ToolKinds...)))
	setSection(f, sectionGeneral, "name", "description")
	setSection(f, sectionWorkspace, "workspace", "mount", "env", "secret")
	setSection(f, sectionSandbox, "sandbox", "image", "command")
	setSection(f, sectionNetwork, "port", "private", "public")
	setSection(f, sectionAccess, "private-access", "public-access", "allow-email")
	setSection(f, sectionApprovals, "allow-tools", "deny-tools", "owner-chat")
	must(cmd.RegisterFlagCompletionFunc("owner-chat", fixedCompletions(agentdir.ChannelKinds...)))
}

func (sf *settingsFlags) registerOperational(cmd *cobra.Command) {
	f := cmd.Flags()
	s := &sf.s
	f.StringVar(&s.Memory, "memory", "", "Container memory limit, such as 4g or 512m (default 4g)")
	f.StringVar(&s.CPUs, "cpus", "", "Container CPU limit, such as 2 or 1.5 (default 2)")
	f.IntVar(&s.PIDs, "pids", 0, "Container process limit (default 512)")
	f.StringVar(&s.TailscaleControlURL, "tailscale-control-url", "", "Headscale URL, such as https://headscale.example.com, instead of Tailscale's control server")
	f.StringVar(&s.TailscaleAuthKey, "tailscale-authkey", "", secretUsage("Tailscale auth key, to join without the login link"))
	f.StringVar(&s.RelayToken, "relay-token", "", secretUsage("Join token of an Agent Mesh relay, to join without the sign-in link"))
	f.StringVar(&s.GoogleClientID, "google-client-id", "", "Client ID of your own Google app for sign-in (optional)")
	f.StringVar(&s.GoogleClientSecret, "google-client-secret", "", secretUsage("Client secret of your own Google app"))
	f.StringVar(&s.Backup, "backup", "", "Take backup snapshots to a directory, gs://bucket/prefix or s3://bucket/prefix")
	f.StringVar(&s.BackupKey, "backup-key", "", secretUsage("Passphrase the snapshots are encrypted with; keep a copy away from the backups"))
	f.StringVar(&s.BackupInterval, "backup-interval", "", "Time between snapshots, a `duration` such as 30m or 1h (default 24h)")
	f.StringVar(&s.BackupRetention, "backup-retention", "", "How long snapshots are kept, a `duration` in hours or days such as 48h or 7d; the latest is always kept (default 7d)")
	f.StringVar(&s.TaskTTL, "task-ttl", "", "How long tasks are kept after their last update, a `duration` in hours or days such as 12h or 30d (default 30d)")
	setSection(f, sectionGeneral, "backup", "backup-key", "backup-interval", "backup-retention", "task-ttl")
	setSection(f, sectionSandbox, "memory", "cpus", "pids")
	setSection(f, sectionNetwork, "tailscale-control-url", "tailscale-authkey", "relay-token")
	setSection(f, sectionAccess, "google-client-id", "google-client-secret")
}

// registerEmailFlags adds the --email-* flags of `channel add email`.
func registerEmailFlags(f *pflag.FlagSet, email *agentdir.EmailSettings) {
	f.StringVar(&email.Address, "email-address", "", "Email channel: the agent's address")
	f.StringVar(&email.IMAP, "email-imap", "", "Email channel: IMAP host:port, such as imap.example.com:993")
	f.StringVar(&email.SMTP, "email-smtp", "", "Email channel: SMTP host:port, such as smtp.example.com:465")
	f.StringVar(&email.Username, "email-username", "", "Email channel: login (default: the address)")
	f.StringVar(&email.Password, "email-password", "", secretUsage("Email channel: password"))
	f.StringVar(&email.Mailbox, "email-mailbox", "", "Email channel: mailbox to watch (default INBOX)")
	f.StringVar(&email.TrustedAuthServ, "email-trusted-authserv", "", "Email channel: authserv-id of your mail server's Authentication-Results")
	f.StringVar(&email.OAuthRefreshToken, "email-oauth-refresh-token", "", secretUsage("Email channel: OAuth refresh token, which signs in with OAuth (XOAUTH2); the password is then unused"))
	f.StringVar(&email.OAuthTokenURL, "email-oauth-token-url", "", "Email channel: OAuth token endpoint, such as https://login.microsoftonline.com/common/oauth2/v2.0/token")
	f.StringVar(&email.OAuthClientID, "email-oauth-client-id", "", "Email channel: OAuth client ID")
	f.StringVar(&email.OAuthClientSecret, "email-oauth-client-secret", "", secretUsage("Email channel: OAuth client secret, if the client has one"))
}

// settings returns the parsed settings. Secret-valued flags set to - are
// read with r, and so are --secret values missing from the environment
// when a terminal can prompt for them.
func (sf *settingsFlags) settings(r *secretref.Reader) (agentdir.Settings, error) {
	s := sf.s
	if sf.command != "" {
		argv, err := shlex.Split(sf.command)
		if err != nil || len(argv) == 0 {
			return s, clierr.Usage(fmt.Sprintf("invalid --command %q", sf.command))
		}
		s.Command = argv
	}
	for _, v := range []struct {
		flag string
		dst  *string
	}{
		{"tailscale-authkey", &s.TailscaleAuthKey},
		{"google-client-secret", &s.GoogleClientSecret},
		{"relay-token", &s.RelayToken},
		{"backup-key", &s.BackupKey},
	} {
		if *v.dst == "" {
			continue
		}
		ref, err := secretref.Parse(v.flag, *v.dst, r)
		if err != nil {
			return s, err
		}
		// Env and stored references stay references, which the settings
		// store under their names.
		if ref.Kind == secretref.Literal || ref.Kind == secretref.Prompted {
			*v.dst = ref.Value
		}
	}
	s.Secrets = slices.Clone(s.Secrets)
	for i, entry := range s.Secrets {
		name, value, ok := strings.Cut(entry, "=")
		s.Secrets[i] = name
		if !ok && (os.Getenv(name) != "" || !r.Terminal()) {
			continue
		}
		if !ok {
			value = secretref.Stdin
		}
		ref, err := secretref.Parse("secret "+name, value, r)
		if err != nil {
			return s, err
		}
		if ref.Kind == secretref.Stored {
			if s.SecretSources == nil {
				s.SecretSources = map[string]string{}
			}
			s.SecretSources[name] = ref.Name
			continue
		}
		if s.SecretValues == nil {
			s.SecretValues = agentdir.Secrets{}
		}
		s.SecretValues[name] = ref.Value
	}
	return s, nil
}
