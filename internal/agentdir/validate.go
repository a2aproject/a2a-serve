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
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// HarnessACP is the harness which runs any ACP agent given as a command.
const HarnessACP = "acp"

// Harnesses are the harnesses with a profile, which need no command. They
// mirror the profiles of the harness package, which imports this one.
var Harnesses = []string{"claude", "opencode"}

// ReservedHarnesses are the names of the test servers, `a2a-serve <name>`,
// which no harness may take.
var ReservedHarnesses = []string{"echo", "proxy", "script"}

// Values of the enum settings.
const (
	SandboxDocker    = "docker"
	SandboxLocal     = "local"
	NetworkTailscale = "tailscale"
	NetworkLAN       = "lan"
	NetworkNgrok     = "ngrok"
	NetworkNone      = "none"
	RoleOwner        = "owner"
	RoleMember       = "member"
)

var (
	secretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	memory     = regexp.MustCompile(`^[0-9]+[bkmgBKMG]?$`)
)

// Validate checks the settings which don't depend on the machine: enums,
// mount specs, the command of an acp agent and the completeness of channels.
// Each problem names its setting by its flag.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	switch {
	case c.Harness == "":
		add("no harness: run `a2a-serve start <harness>`")
	case slices.Contains(ReservedHarnesses, c.Harness):
		add("invalid harness %q: %s are test servers, not harnesses", c.Harness, strings.Join(ReservedHarnesses, ", "))
	case c.Harness == HarnessACP && len(c.Command) == 0:
		add(`harness acp needs a command, such as --command "gemini --experimental-acp"`)
	case len(c.Command) == 0 && !slices.Contains(Harnesses, c.Harness):
		add("unknown harness %q: want %s, or acp with --command", c.Harness, strings.Join(Harnesses, ", "))
	}
	if c.Name == "" {
		add("no name: set one with --name")
	}
	checkEnum := func(flag, v string, want ...string) {
		if v != "" && !slices.Contains(want, v) {
			add("invalid --%s %q: want %s or none", flag, v, strings.Join(want, ", "))
		}
	}
	if c.Sandbox != "" && c.Sandbox != SandboxDocker && c.Sandbox != SandboxLocal {
		add("invalid --sandbox %q: want docker or local", c.Sandbox)
	}
	if !isRelayURL(c.Network.Private) {
		checkEnum("private", c.Network.Private, NetworkTailscale, NetworkLAN)
	}
	if !isRelayURL(c.Network.Public) {
		checkEnum("public", c.Network.Public, NetworkTailscale, NetworkNgrok)
	}
	for flag, v := range map[string]string{"private": c.Network.Private, "public": c.Network.Public} {
		if !isRelayURL(v) {
			continue
		}
		if u, err := url.Parse(v); err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			add("invalid --%s %q: want a relay URL such as https://relay.example.com", flag, v)
		}
	}
	if isRelayURL(c.Network.Private) && isRelayURL(c.Network.Public) && strings.TrimSuffix(c.Network.Private, "/") != strings.TrimSuffix(c.Network.Public, "/") {
		add("--private and --public name different relays (%s, %s): an agent joins one relay", c.Network.Private, c.Network.Public)
	}
	if c.Workspace != "" {
		if _, err := parseMount(c.Workspace, "/"); err != nil {
			add("--workspace: %w", err)
		}
	}
	for _, spec := range c.Mounts {
		if _, err := parseMount(spec, "/"); err != nil {
			add("--mount: %w", err)
		}
	}
	for _, name := range c.Secrets {
		if !secretName.MatchString(name) {
			add("invalid --secret %q: want an environment variable name", name)
		}
	}
	for _, kv := range c.Env {
		if name, _, ok := strings.Cut(kv, "="); !ok || !secretName.MatchString(name) {
			add("invalid --env %q: want NAME=value", kv)
		}
	}
	checkPort := func(flag string, p int) {
		if p < 0 || p > 65535 {
			add("invalid %s %d: want 1-65535", flag, p)
		}
	}
	checkPort("--port", c.Port)
	checkPort("network.lan_port", c.Network.LANPort)
	for login, role := range c.Network.Identities {
		if login == "" || (role != RoleOwner && role != RoleMember) {
			add("invalid --identity %s=%s: want login=member or login=owner", login, role)
		}
	}
	if m := c.Limits.Memory; m != "" && !memory.MatchString(m) {
		add("invalid --memory %q: want a size such as 4g or 512m", m)
	}
	if v := c.Limits.CPUs; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err != nil || n <= 0 {
			add("invalid --cpus %q: want a positive number such as 2 or 1.5", v)
		}
	}
	if c.Limits.PIDs < 0 {
		add("invalid --pids %d: want a positive number", c.Limits.PIDs)
	}
	if t := c.Channels.Telegram; t != nil && t.Token == "" {
		add("the telegram channel needs a token: a2a-serve channel add telegram --token <token>")
	}
	if dc := c.Channels.Discord; dc != nil && dc.Token == "" {
		add("the discord channel needs a token: a2a-serve channel add discord --token <token>")
	}
	if e := c.Channels.Email; e != nil {
		var missing []string
		for flag, v := range map[string]string{"email-address": e.Address, "email-imap": e.IMAP, "email-smtp": e.SMTP, "email-password": e.Password, "email-trusted-authserv": e.TrustedAuthServ} {
			if v == "" {
				missing = append(missing, "--"+flag)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			add("the email channel needs %s", strings.Join(missing, ", "))
		}
	}
	for _, e := range c.Auth.AllowEmails {
		if err := validEmailEntry(e); err != nil {
			errs = append(errs, err)
		}
	}
	for flag, patterns := range map[string][]string{"allow-tools": c.Approvals.Allow, "deny-tools": c.Approvals.Deny} {
		for _, p := range patterns {
			if !matchesToolKind(p) {
				add("invalid --%s %q: want ACP tool kinds (%s), or a glob such as *", flag, p, strings.Join(ToolKinds, ", "))
			}
		}
	}
	if len(c.Approvals.RemovedApprovers) > 0 {
		add("approvals.approvers was removed: whoever starts a task answers its approvals, and owners can answer any; delete it from config.yaml")
	}
	if v := c.Approvals.OwnerChat; v != "" {
		if kind, who, hasWho := strings.Cut(v, ":"); !slices.Contains(ChannelKinds, kind) || (hasWho && who == "") {
			add("invalid --owner-chat %q: want channel[:who], such as telegram or telegram:anna, with channel one of %s", v, strings.Join(ChannelKinds, ", "))
		}
	}
	if (c.Auth.GoogleClientID == "") != (c.Auth.GoogleClientSecret == "") {
		add("--google-client-id and --google-client-secret go together: they are your Google app")
	}
	errs = append(errs, c.Backup.validate()...)
	return errors.Join(errs...)
}

// ToolKinds are the ACP tool kinds which --allow-tools and --deny-tools
// match.
var ToolKinds = []string{"read", "edit", "delete", "move", "search", "execute", "think", "fetch", "switch_mode", "other"}

// matchesToolKind reports whether the glob p matches an ACP tool kind.
func matchesToolKind(p string) bool {
	return slices.ContainsFunc(ToolKinds, func(kind string) bool {
		ok, err := path.Match(p, kind)
		return err == nil && ok
	})
}

// validEmailEntry checks an --allow-email entry: an email or *@domain.
func validEmailEntry(entry string) error {
	local, domain, ok := strings.Cut(entry, "@")
	if !ok || local == "" || domain == "" || strings.ContainsAny(domain, "@* ") || (strings.Contains(local, "*") && local != "*") {
		return fmt.Errorf("invalid --allow-email %q: want an email, or *@domain for a Google Workspace domain", entry)
	}
	return nil
}

// isRelayURL reports whether a --private or --public value names an Agent
// Mesh relay by its URL.
func isRelayURL(v string) bool {
	return strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")
}

func (b BackupConfig) validate() []error {
	var errs []error
	if b.To != "" && b.Key == "" {
		errs = append(errs, errors.New("--backup needs --backup-key: snapshots are always encrypted"))
	}
	if b.To != "" && strings.Contains(b.To, "://") && !strings.HasPrefix(b.To, "gs://") && !strings.HasPrefix(b.To, "s3://") && !strings.HasPrefix(b.To, "file://") {
		errs = append(errs, fmt.Errorf("invalid --backup %q: want a directory, gs://bucket/prefix or s3://bucket/prefix", b.To))
	}
	if b.Interval != "" {
		if d, err := ParseDays(b.Interval); err != nil {
			errs = append(errs, fmt.Errorf("--backup-interval: %w", err))
		} else if d < time.Minute {
			errs = append(errs, fmt.Errorf("--backup-interval %s: want at least 1m", b.Interval))
		}
	}
	if b.Retention != "" {
		if _, err := ParseDays(b.Retention); err != nil {
			errs = append(errs, fmt.Errorf("--backup-retention: %w", err))
		}
	}
	return errs
}

// CheckConfig validates cfg, and checks that its mount sources exist, that
// mounts have distinct destinations, and that secrets holds every secret
// it names or references.
func (d Dir) CheckConfig(cfg *Config, secrets Secrets) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	var errs []error
	mounts, err := d.Mounts(cfg)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, m := range mounts {
		flag := "--mount"
		if i == 0 {
			flag = "--workspace"
		}
		if seen[m.Dst] {
			errs = append(errs, fmt.Errorf("%s %s: another mount already uses the destination %s", flag, m, m.Dst))
		}
		seen[m.Dst] = true
		if _, err := os.Stat(m.Src); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: the source does not exist", flag, m))
		}
	}
	for _, name := range cfg.Secrets {
		if secrets[name] == "" {
			errs = append(errs, fmt.Errorf("secret %s is not stored: run `a2a-serve config set --secret %s` with the variable set", name, name))
		}
	}
	for _, ref := range cfg.secretRefs() {
		if _, err := secrets.Resolve(ref.value); err != nil {
			label := "--" + ref.flag
			if ref.flag == ChannelTelegram || ref.flag == ChannelDiscord {
				label = ref.flag + " token (a2a-serve channel add " + ref.flag + " --token)"
			}
			errs = append(errs, fmt.Errorf("%s: %w", label, err))
		}
	}
	return errors.Join(errs...)
}

type secretRef struct{ flag, value string }

// secretRefs returns the secret references of the channels.
func (c *Config) secretRefs() []secretRef {
	var refs []secretRef
	if t := c.Channels.Telegram; t != nil && strings.HasPrefix(t.Token, SecretPrefix) {
		refs = append(refs, secretRef{"telegram", t.Token})
	}
	if dc := c.Channels.Discord; dc != nil && strings.HasPrefix(dc.Token, SecretPrefix) {
		refs = append(refs, secretRef{"discord", dc.Token})
	}
	if e := c.Channels.Email; e != nil && strings.HasPrefix(e.Password, SecretPrefix) {
		refs = append(refs, secretRef{"email-password", e.Password})
	}
	if strings.HasPrefix(c.Auth.GoogleClientSecret, SecretPrefix) {
		refs = append(refs, secretRef{"google-client-secret", c.Auth.GoogleClientSecret})
	}
	if strings.HasPrefix(c.Backup.Key, SecretPrefix) {
		refs = append(refs, secretRef{"backup-key", c.Backup.Key})
	}
	return refs
}

// referencedSecrets returns the names of the stored secrets cfg uses.
func (c *Config) referencedSecrets() map[string]bool {
	out := map[string]bool{}
	for _, name := range c.Secrets {
		out[name] = true
	}
	for _, ref := range c.secretRefs() {
		out[strings.TrimPrefix(ref.value, SecretPrefix)] = true
	}
	return out
}

// CheckHarness returns an error if the agent runs another harness than name.
func (c *Config) CheckHarness(name string) error {
	if c.Harness == "" || c.Harness == name {
		return nil
	}
	return &SettingsError{Err: fmt.Errorf("this agent runs %s; switch with `a2a-serve config set --harness %s`", c.Harness, name)}
}
