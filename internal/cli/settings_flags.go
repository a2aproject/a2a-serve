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

	"github.com/google/shlex"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/secretref"
)

// settingsFlags are the agent settings shared by `a2a serve <harness>` and
// `a2a serve config set`, so both speak one vocabulary.
type settingsFlags struct {
	s       agentdir.Settings
	email   agentdir.EmailSettings
	command string
}

// secretUsage is the help of a flag which takes a secret value.
func secretUsage(usage string) string {
	return usage + " (" + secretref.Syntax + ")"
}

func (sf *settingsFlags) register(cmd *cobra.Command, withHarness bool) {
	f := cmd.Flags()
	s := &sf.s
	if withHarness {
		f.StringVar(&s.Harness, "harness", "", "Harness: claude, opencode, or acp with --command")
		must(cmd.RegisterFlagCompletionFunc("harness", fixedCompletions(append(slices.Clone(agentdir.Harnesses), agentdir.HarnessACP)...)))
	}
	f.StringVar(&s.Name, "name", "", "Agent name, also its tailnet machine name (default: the directory name)")
	f.StringVar(&s.Description, "description", "", "Agent description for its card")
	f.StringArrayVar(&s.Secrets, "secret", nil, "Name of an environment variable to pass to the harness, such as an API key; its value is read from your environment (or prompted) and stored in .a2a/state/secrets (repeatable)")
	f.StringVar(&s.Workspace, "workspace", "", "Workspace directory: src[:dst][:ro] (default: the directory holding .a2a)")
	f.StringArrayVar(&s.Mounts, "mount", nil, "Extra directory to mount: src[:dst][:ro], keyed by dst (repeatable)")
	f.StringArrayVar(&s.Env, "env", nil, "Extra NAME=value for the harness, keyed by NAME (repeatable)")
	f.StringVar(&s.Private, "private", "", "Private network: tailscale, lan, the URL of an Agent Mesh relay, or none")
	f.StringVar(&s.Public, "public", "", "Public URL: tailscale (Funnel), ngrok, the URL of an Agent Mesh relay, or none")
	f.StringVar(&s.Telegram, "telegram", "", secretUsage("Telegram bot token"))
	f.StringVar(&s.Discord, "discord", "", secretUsage("Discord bot token"))
	registerEmailFlags(f, &sf.email)
	f.StringVar(&s.Sandbox, "sandbox", "", "Where the harness runs: docker (default) or local")
	f.StringVar(&s.Image, "image", "", "Container image of the harness")
	f.StringVar(&sf.command, "command", "", "Command line of the ACP agent, replacing the profile's")
	f.IntVar(&s.Port, "port", 0, "Localhost port (default: a free port, kept across runs)")
	f.StringSliceVar(&s.AllowTools, "allow-tools", nil, "ACP tool kinds approved without asking, such as read,search")
	f.StringSliceVar(&s.DenyTools, "deny-tools", nil, "ACP tool kinds denied without asking")
	f.StringSliceVar(&s.Approvers, "approver", nil, "Principals besides the owner who may approve tool calls, such as pair:anna")
	f.StringArrayVar(&s.Identities, "identity", nil, "Role of another user's network login: login=member or login=owner (repeatable)")
	f.StringVar(&s.Memory, "memory", "", "Container memory limit, such as 4g (default 4g)")
	f.StringVar(&s.CPUs, "cpus", "", "Container CPU limit, such as 2 or 1.5 (default 2)")
	f.IntVar(&s.PIDs, "pids", 0, "Container process limit (default 512)")
	f.StringVar(&s.TailscaleControlURL, "tailscale-control-url", "", "Headscale URL, instead of Tailscale's control server")
	f.StringVar(&s.TailscaleAuthKey, "tailscale-authkey", "", secretUsage("Tailscale auth key, to join without the login link"))
	f.StringVar(&s.NgrokURL, "ngrok-url", "", "Reserved ngrok domain, such as https://coder.ngrok.app")
	f.StringVar(&s.NgrokAuthToken, "ngrok-authtoken", "", secretUsage("ngrok authtoken"))
	f.StringSliceVar(&s.AllowEmails, "allow-email", nil, "People who may call the agent after signing in with Google: emails, or *@domain for a Google Workspace domain")
	f.StringVar(&s.GoogleClientID, "google-client-id", "", "Client ID of your own Google app for sign-in (optional)")
	f.StringVar(&s.GoogleClientSecret, "google-client-secret", "", secretUsage("Client secret of your own Google app"))
	f.StringVar(&s.RelayToken, "relay-token", "", secretUsage("Join token of an Agent Mesh relay, to join without the sign-in link"))
	f.StringVar(&s.Backup, "backup", "", "Take backup snapshots to a directory, gs://bucket/prefix or s3://bucket/prefix")
	f.StringVar(&s.BackupKey, "backup-key", "", secretUsage("Passphrase the snapshots are encrypted with; keep a copy away from the backups"))
	f.StringVar(&s.BackupInterval, "backup-interval", "", "Time between snapshots, such as 1h (default 24h)")
	f.StringVar(&s.BackupRetention, "backup-retention", "", "How long snapshots are kept, such as 7d; the latest is always kept (default 7d)")
	must(cmd.RegisterFlagCompletionFunc("private", fixedCompletions(agentdir.NetworkTailscale, agentdir.NetworkLAN, agentdir.NetworkNone)))
	must(cmd.RegisterFlagCompletionFunc("public", fixedCompletions(agentdir.NetworkTailscale, agentdir.NetworkNgrok, agentdir.NetworkNone)))
	must(cmd.RegisterFlagCompletionFunc("sandbox", fixedCompletions(agentdir.SandboxDocker, agentdir.SandboxLocal)))
}

// registerEmailFlags adds the --email-* flags of the email channel, shared
// by `a2a serve <harness>`, `config set` and `channel add email`.
func registerEmailFlags(f *pflag.FlagSet, email *agentdir.EmailSettings) {
	f.StringVar(&email.Address, "email-address", "", "Email channel: the agent's address")
	f.StringVar(&email.IMAP, "email-imap", "", "Email channel: IMAP host:port")
	f.StringVar(&email.SMTP, "email-smtp", "", "Email channel: SMTP host:port")
	f.StringVar(&email.Username, "email-username", "", "Email channel: login (default: the address)")
	f.StringVar(&email.Password, "email-password", "", secretUsage("Email channel: password"))
	f.StringVar(&email.Mailbox, "email-mailbox", "", "Email channel: mailbox to watch (default INBOX)")
	f.StringVar(&email.TrustedAuthServ, "email-trusted-authserv", "", "Email channel: authserv-id of your mail server's Authentication-Results")
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
	email := sf.email
	for _, v := range []struct {
		flag string
		dst  *string
	}{
		{"telegram", &s.Telegram},
		{"discord", &s.Discord},
		{"email-password", &email.Password},
		{"tailscale-authkey", &s.TailscaleAuthKey},
		{"ngrok-authtoken", &s.NgrokAuthToken},
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
	if email != (agentdir.EmailSettings{}) {
		s.Email = &email
	}
	for _, name := range s.Secrets {
		if os.Getenv(name) != "" || !r.Terminal() {
			continue
		}
		val, err := r.Read(name)
		if err != nil {
			return s, err
		}
		if s.SecretValues == nil {
			s.SecretValues = agentdir.Secrets{}
		}
		s.SecretValues[name] = val
	}
	return s, nil
}
