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
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/speech"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
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
	NetworkNone      = "none"
)

var (
	secretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	memory     = regexp.MustCompile(`^([0-9]+)([bkmgBKMG]?)$`)
)

// minMemory is the smallest memory limit that docker accepts.
const minMemory = 6 << 20

// memoryBytes returns the size of a --memory value in bytes. ok is false if
// the value isn't a size or doesn't fit in 63 bits.
func memoryBytes(v string) (n int64, ok bool) {
	m := memory.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	unit := int64(1)
	switch strings.ToLower(m[2]) {
	case "k":
		unit = 1 << 10
	case "m":
		unit = 1 << 20
	case "g":
		unit = 1 << 30
	}
	if n > math.MaxInt64/unit {
		return 0, false
	}
	return n * unit, true
}

// Validate checks the settings which don't depend on the machine: enums,
// mount specs, the command of an acp agent and the completeness of messengers.
// Each problem names its setting by its flag.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	invalid := func(flag, entry, format string, args ...any) {
		errs = append(errs, &FieldError{Flag: flag, Entry: entry, Reason: fmt.Sprintf(format, args...)})
	}
	switch {
	case c.Harness == "":
		add("no agent: run `a2a-serve start <agent>`")
	case slices.Contains(ReservedHarnesses, c.Harness):
		add("invalid agent %q: %s are test servers", c.Harness, strings.Join(ReservedHarnesses, ", "))
	case c.Harness == HarnessACP && len(c.Command) == 0:
		add(`agent acp needs a command, such as --command "gemini --experimental-acp"`)
	case len(c.Command) == 0 && !slices.Contains(Harnesses, c.Harness):
		add("unknown agent %q: want %s, or acp with --command", c.Harness, strings.Join(Harnesses, ", "))
	}
	if c.Model != "" && !slices.Contains(Harnesses, c.Harness) {
		invalid("model", c.Model, "needs the agent %s; set the model of agent %s in its --command", strings.Join(Harnesses, " or "), c.Harness)
	}
	if c.Name == "" {
		add("no name: set one with --name")
	}
	checkNetwork := func(flag, v string) {
		if v != "" && v != NetworkTailscale && !isRelayURL(v) {
			invalid(flag, v, "want %s, a relay URL such as https://relay.example.com, or none", NetworkTailscale)
		}
	}
	if c.Sandbox != "" && c.Sandbox != SandboxDocker && c.Sandbox != SandboxLocal {
		invalid("sandbox", c.Sandbox, "want docker or local")
	}
	if c.Egress.Mode != "" && !slices.Contains(egress.Modes, c.Egress.Mode) {
		invalid("egress", c.Egress.Mode, "want %s", strings.Join(egress.Modes, ", "))
	}
	for _, r := range c.Egress.Allow {
		if _, err := egress.ParseRule(r); err != nil {
			add("%v", err)
		}
	}
	if c.Network.Private == "lan" {
		invalid("private", c.Network.Private, "lan was removed; want tailscale, a relay URL or none")
	} else {
		checkNetwork("private", c.Network.Private)
	}
	checkNetwork("public", c.Network.Public)
	for flag, v := range map[string]string{"private": c.Network.Private, "public": c.Network.Public} {
		if !isRelayURL(v) {
			continue
		}
		if err := agentmesh.CheckRelayURL(v); err != nil {
			invalid(flag, v, "%v", err)
		}
	}
	if isRelayURL(c.Network.Private) && isRelayURL(c.Network.Public) && strings.TrimSuffix(c.Network.Private, "/") != strings.TrimSuffix(c.Network.Public, "/") {
		add("--private and --public name different relays (%s, %s): an agent joins one relay", c.Network.Private, c.Network.Public)
	}
	if c.Workspace != "" {
		if _, err := parseMount(c.Workspace, "/"); err != nil {
			invalid("workspace", c.Workspace, "%s", reasonOf(err))
		}
	}
	for _, spec := range c.Mounts {
		if _, err := parseMount(spec, "/"); err != nil {
			invalid("mount", spec, "%s", reasonOf(err))
		}
	}
	for _, name := range c.Secrets {
		if !secretName.MatchString(name) {
			invalid("secret", name, "want an environment variable name")
		}
	}
	for _, kv := range c.Env {
		if name, _, ok := strings.Cut(kv, "="); !ok || !secretName.MatchString(name) {
			invalid("env", kv, "want NAME=value")
		}
	}
	for _, name := range c.MCP.Disabled {
		if strings.TrimSpace(name) == "" {
			invalid("disable-mcp", name, "want the name of an MCP server in .mcp.json")
		}
	}
	if c.Port < 0 || c.Port > 65535 {
		invalid("port", strconv.Itoa(c.Port), "want 1-65535")
	}
	if m := c.Limits.Memory; m != "" {
		switch n, ok := memoryBytes(m); {
		case !ok:
			invalid("memory", m, "want a size such as 4g or 512m")
		case n < minMemory:
			invalid("memory", m, "want at least 6m, such as 4g or 512m")
		}
	}
	if v := c.Limits.CPUs; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err != nil || n <= 0 {
			invalid("cpus", v, "want a positive number such as 2 or 1.5")
		}
	}
	if c.TaskTTL != "" {
		if d, err := ParseDays(c.TaskTTL); err != nil {
			invalid("task-ttl", c.TaskTTL, "%s", reasonOf(err))
		} else if d < MinTaskTTL {
			invalid("task-ttl", c.TaskTTL, "want at least %s", MinTaskTTL)
		}
	}
	if c.ContactRefresh != "" {
		if d, err := ParseDays(c.ContactRefresh); err != nil {
			invalid("contact-refresh", c.ContactRefresh, "%s", reasonOf(err))
		} else if d < MinContactRefresh {
			invalid("contact-refresh", c.ContactRefresh, "want at least %s", MinContactRefresh)
		}
	}
	errs = append(errs, c.Card.validate()...)
	if c.Limits.PIDs < 0 {
		invalid("pids", strconv.Itoa(c.Limits.PIDs), "want a positive number")
	}
	if t := c.Messengers.Telegram; t != nil && t.Token == "" {
		add("the telegram messenger needs a token: a2a-serve messenger add telegram --token <token>")
	}
	if dc := c.Messengers.Discord; dc != nil && dc.Token == "" {
		add("the discord messenger needs a token: a2a-serve messenger add discord --token <token>")
	}
	if e := c.Messengers.Email; e != nil {
		var missing []string
		required := map[string]string{"email-address": e.Address, "email-imap": e.IMAP, "email-smtp": e.SMTP, "email-password": e.Password, "email-trusted-authserv": e.TrustedAuthServ}
		if e.OAuthRefreshToken != "" {
			delete(required, "email-password")
			required["email-oauth-token-url"] = e.OAuthTokenURL
			required["email-oauth-client-id"] = e.OAuthClientID
		}
		for flag, v := range required {
			if v == "" {
				missing = append(missing, "--"+flag)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			add("the email messenger needs %s", strings.Join(missing, ", "))
		}
	}
	errs = append(errs, c.Policy.validate()...)
	errs = append(errs, c.Policy.validateSandbox(c.Secrets)...)
	errs = append(errs, c.validateInject()...)
	for _, l := range []struct {
		flag     string
		patterns []string
	}{{"allow-tools", c.Approvals.Allow}, {"deny-tools", c.Approvals.Deny}} {
		for _, p := range l.patterns {
			if err := toolrules.Validate(p); err != nil {
				invalid(l.flag, p, "%v", err)
			}
		}
	}
	if len(c.Approvals.RemovedApprovers) > 0 {
		add("approvals.approvers was removed: whoever starts a task answers its approvals, and owners can answer any; delete it from config.yaml")
	}
	if v := c.Approvals.OwnerChat; v != "" {
		if kind, who, hasWho := strings.Cut(v, ":"); !slices.Contains(MessengerKinds, kind) || (hasWho && who == "") {
			invalid("owner-chat", v, "want messenger[:who], such as telegram or telegram:anna, with messenger one of %s", strings.Join(MessengerKinds, ", "))
		}
	}
	if (c.Auth.GoogleClientID == "") != (c.Auth.GoogleClientSecret == "") {
		add("--google-client-id and --google-client-secret go together: they are your Google app")
	}
	if c.Auth.GoogleClientID != "" && len(c.Policy.AllowEmails) == 0 {
		add("--google-client-id needs --allow-email: people sign in with your Google app only when the agent allows their email")
	}
	errs = append(errs, c.Backup.validate()...)
	errs = append(errs, c.Voice.validate()...)
	return errors.Join(errs...)
}

// ToolKinds are the ACP tool kinds which --allow-tools and --deny-tools
// match.
var ToolKinds = toolrules.Kinds

func (p PolicyConfig) validate() []error {
	var errs []error
	if v := p.PrivateAllowIncoming; v != "" && v != AccessEveryone && v != AccessPaired {
		errs = append(errs, &FieldError{Flag: "private-allow-incoming", Entry: v, Reason: "want everyone (or anyone) or paired"})
	}
	if v := p.PublicAllowIncoming; v != "" && v != AccessPaired && v != AccessAnyone {
		errs = append(errs, &FieldError{Flag: "public-allow-incoming", Entry: v, Reason: "want paired or anyone (or everyone)"})
	}
	if v := p.PrivateAllowOutgoing; v != "" && v != AccessEveryone && v != AccessContacts {
		errs = append(errs, &FieldError{Flag: "private-allow-outgoing", Entry: v, Reason: "want everyone (or anyone) or contacts"})
	}
	for _, e := range p.AllowEmails {
		if !ValidEmailPattern(e) {
			errs = append(errs, &FieldError{Flag: "allow-email", Entry: e, Reason: "want an email, or *@domain"})
		}
	}
	return errs
}

// ValidEmailPattern reports whether an --allow-email entry is an email or
// *@domain.
func ValidEmailPattern(entry string) bool {
	local, domain, ok := strings.Cut(entry, "@")
	return ok && local != "" && domain != "" && !strings.ContainsAny(domain, "@* ") && (!strings.Contains(local, "*") || local == "*")
}

// isRelayURL reports whether a --private or --public value names an Agent
// Mesh relay by its URL.
func isRelayURL(v string) bool {
	return strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")
}

func (c CardConfig) validate() []error {
	var errs []error
	invalid := func(entry, format string, args ...any) {
		errs = append(errs, &FieldError{Flag: CardSkillsSetting, Entry: entry, Reason: fmt.Sprintf(format, args...)})
	}
	if len(c.Skills) > MaxCardSkills {
		errs = append(errs, fmt.Errorf("card.skills has %d skills: want at most %d", len(c.Skills), MaxCardSkills))
	}
	seen := map[string]bool{}
	for _, sk := range c.Skills {
		switch {
		case strings.TrimSpace(sk.ID) == "":
			errs = append(errs, &FieldError{Flag: CardSkillsSetting, Entry: sk.Name, Desc: fmt.Sprintf("a skill named %q", sk.Name), Reason: "want a non-empty id"})
		case seen[sk.ID]:
			invalid(sk.ID, "the id is taken by another skill")
		case len([]rune(sk.Description)) > MaxCardSkillDescription:
			invalid(sk.ID, "the description is longer than %d characters", MaxCardSkillDescription)
		}
		seen[sk.ID] = true
	}
	return errs
}

func (b BackupConfig) validate() []error {
	var errs []error
	if b.To != "" && b.Key == "" {
		errs = append(errs, errors.New("--backup needs --backup-key: snapshots are always encrypted"))
	}
	if b.To != "" && strings.Contains(b.To, "://") && !strings.HasPrefix(b.To, "gs://") && !strings.HasPrefix(b.To, "s3://") && !strings.HasPrefix(b.To, "file://") {
		errs = append(errs, &FieldError{Flag: "backup", Entry: b.To, Reason: "want a directory, gs://bucket/prefix or s3://bucket/prefix"})
	}
	if b.Interval != "" {
		if d, err := ParseDays(b.Interval); err != nil {
			errs = append(errs, &FieldError{Flag: "backup-interval", Entry: b.Interval, Reason: reasonOf(err)})
		} else if d < time.Minute {
			errs = append(errs, &FieldError{Flag: "backup-interval", Entry: b.Interval, Reason: "want at least 1m"})
		}
	}
	if b.Retention != "" {
		if _, err := ParseDays(b.Retention); err != nil {
			errs = append(errs, &FieldError{Flag: "backup-retention", Entry: b.Retention, Reason: reasonOf(err)})
		}
	}
	return errs
}

func (v VoiceConfig) validate() []error {
	var errs []error
	if v.Service != "" {
		if err := speech.ParseService(v.Service); err != nil {
			errs = append(errs, &FieldError{Flag: "voice", Entry: v.Service, Reason: err.Error()})
		}
	}
	if v.Service == speech.OpenAI && v.Key == "" {
		errs = append(errs, errors.New("--voice openai needs --voice-key, such as --voice-key env:OPENAI_API_KEY"))
	}
	if v.Service == "" && v.Key != "" {
		errs = append(errs, errors.New("--voice-key needs --voice"))
	}
	if v.Service == "" && v.Name != "" {
		errs = append(errs, errors.New("--voice-name needs --voice"))
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
		if cfg.Sandbox == SandboxLocal {
			continue
		}
		label := flag + " " + m.String()
		if i == 0 && cfg.Workspace == "" {
			label = "the workspace " + m.Src
		}
		if err := checkBroadMount(label, m.Src, cfg.AllowBroadMounts); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range cfg.Secrets {
		if secrets[name] == "" {
			errs = append(errs, fmt.Errorf("secret %s is not stored: run `a2a-serve config set --secret %s` with the variable set", name, name))
		}
	}
	errs = append(errs, cfg.ContainerEnvErrors(secrets)...)
	for _, ref := range cfg.secretRefs() {
		if _, err := secrets.Resolve(ref.value); err != nil {
			label := "--" + ref.flag
			if ref.flag == MessengerTelegram || ref.flag == MessengerDiscord {
				label = ref.flag + " token (a2a-serve messenger add " + ref.flag + " --token)"
			}
			errs = append(errs, fmt.Errorf("%s: %w", label, err))
		}
	}
	return errors.Join(errs...)
}

// LineBreakError reports an environment variable of the agent whose value
// has a line break.
type LineBreakError struct{ Name string }

func (e *LineBreakError) Error() string {
	return fmt.Sprintf("the value of %s has a line break, which docker can't pass to the container", e.Name)
}

// CheckEnvValue returns a [*LineBreakError] if docker can't pass value as
// the environment variable name, since its env file holds one per line.
func CheckEnvValue(name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return &LineBreakError{Name: name}
	}
	return nil
}

// ContainerEnvErrors returns an error for each --env and --secret value
// which the docker sandbox can't pass to the container. Other sandboxes
// take any value.
func (c *Config) ContainerEnvErrors(secrets Secrets) []error {
	if c.Sandbox != "" && c.Sandbox != SandboxDocker {
		return nil
	}
	var errs []error
	for _, kv := range c.Env {
		name, value, _ := strings.Cut(kv, "=")
		if err := CheckEnvValue(name, value); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range c.Secrets {
		if err := CheckEnvValue(name, secrets[name]); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// checkBroadMount refuses a mount of src that exposes the user's home
// directory, unless allowed names src.
func checkBroadMount(label, src string, allowed []string) error {
	broad, err := BroadMount(src)
	if err != nil || !broad {
		return err
	}
	if slices.ContainsFunc(allowed, func(p string) bool { return resolved(p) == resolved(src) }) {
		return nil
	}
	return fmt.Errorf("%s includes your home directory, with your credentials and the secrets of other agents, and the sandbox would expose it to the agent; use a project directory, or pass --allow-broad-mount %s to mount it anyway", label, src)
}

type secretRef struct{ flag, value string }

// secretRefs returns the values of the settings which reference a stored
// secret.
func (c *Config) secretRefs() []secretRef {
	var refs []secretRef
	for _, f := range fields {
		if f.secret == "" {
			continue
		}
		if v := first(f.get(c)); strings.HasPrefix(v, SecretPrefix) {
			refs = append(refs, secretRef{f.flag, v})
		}
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
	return &SettingsError{Err: fmt.Errorf("this agent runs %s; switch with `a2a-serve config set --agent %s`", c.Harness, name)}
}

// CheckOwnerChat refuses an owner chat on a messenger the agent doesn't have.
// Only a change of the owner chat checks it, so that removing the messenger
// later doesn't make the config invalid.
func (c *Config) CheckOwnerChat() error {
	kind, _, _ := strings.Cut(c.Approvals.OwnerChat, ":")
	if kind == "" || c.HasMessenger(kind) {
		return nil
	}
	return fmt.Errorf("--owner-chat %s needs the %s messenger: add it first with a2a-serve messenger add %s", c.Approvals.OwnerChat, kind, kind)
}
