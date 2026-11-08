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
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/clierr"
)

// SandboxRule sets what the agent containers of the callers it matches get:
// their storage and their secrets.
type SandboxRule struct {
	// Match lists the callers: self, private, guests, email:<address>,
	// email:*@<domain> or pairing:<name>.
	Match []string `yaml:"match,flow" json:"match"`
	// Storage is one of the Storage* modes. Empty takes it from a broader
	// matching rule, or the caller's default.
	Storage string `yaml:"storage,omitempty" json:"storage,omitempty"`
	// Secrets are names from the agent's secrets, all, none, or
	// NAME=secret:OTHER, which passes the secret OTHER as NAME. Empty takes
	// them from a broader matching rule, or the caller's default.
	Secrets []string `yaml:"secrets,omitempty,flow" json:"secrets,omitempty"`
}

// Storage modes of a sandbox rule.
const (
	// StorageShared mounts the workspace and mounts as configured.
	StorageShared = "shared"
	// StorageReadOnly mounts the workspace and mounts read-only.
	StorageReadOnly = "read-only"
	// StorageOwn mounts a private copy of the workspace, and the mounts
	// read-only.
	StorageOwn = "own"
	// StorageEmpty mounts an empty private workspace, and no mounts.
	StorageEmpty = "empty"
)

// StorageModes are the storage modes, from the most access to the least.
var StorageModes = []string{StorageShared, StorageOwn, StorageReadOnly, StorageEmpty}

// SecretsAll and SecretsNone in the secrets of a rule pass every secret of
// the agent, or none. Each goes alone.
const (
	SecretsAll  = "all"
	SecretsNone = "none"
)

// Sandbox rule matchers besides the members of the policy.
const (
	// MatchPairingPrefix starts pairing:<name>, which matches a pairing or
	// a webhook node.
	MatchPairingPrefix = "pairing:"
	// MatchGuests matches guests: the callers nobody vouched for, whom the
	// guest role holds.
	MatchGuests = "guests"
	// MatchSelf matches the owners, who share the container of the user
	// running the agent.
	MatchSelf    = "self"
	matchPrivate = "private"
	matchEmail   = "email:"
)

// secretRefPrefix starts the source of NAME=secret:OTHER.
const secretRefPrefix = "secret:"

// ParseSecretGrant splits an entry of a rule's secrets into the variable the
// agent gets and the stored secret it holds: NAME is NAME=secret:NAME.
func ParseSecretGrant(entry string) (name, from string, ok bool) {
	name, src, hasSrc := strings.Cut(entry, "=")
	if !hasSrc {
		return entry, entry, secretName.MatchString(entry)
	}
	from, ok = strings.CutPrefix(src, secretRefPrefix)
	return name, from, ok && secretName.MatchString(name) && secretName.MatchString(from)
}

// PairingMatcher is the matcher of the pairing or webhook node name.
func PairingMatcher(name string) string { return MatchPairingPrefix + name }

// SandboxRuleOf returns the rule whose only matcher is m.
func (p PolicyConfig) SandboxRuleOf(m string) (SandboxRule, bool) {
	i := slices.IndexFunc(p.Sandbox, func(r SandboxRule) bool { return slices.Equal(r.Match, []string{m}) })
	if i < 0 {
		return SandboxRule{}, false
	}
	return p.Sandbox[i], true
}

// SetSandboxRule replaces the rule whose only matcher is r's only matcher,
// or adds r. A rule with neither storage nor secrets removes it.
func (p *PolicyConfig) SetSandboxRule(r SandboxRule) {
	i := slices.IndexFunc(p.Sandbox, func(o SandboxRule) bool { return slices.Equal(o.Match, r.Match) })
	switch {
	case r.Storage == "" && len(r.Secrets) == 0 && i >= 0:
		p.Sandbox = slices.Delete(slices.Clone(p.Sandbox), i, i+1)
	case r.Storage == "" && len(r.Secrets) == 0:
	case i >= 0:
		p.Sandbox = slices.Clone(p.Sandbox)
		p.Sandbox[i] = r
	default:
		p.Sandbox = append(slices.Clone(p.Sandbox), r)
	}
	if len(p.Sandbox) == 0 {
		p.Sandbox = nil
	}
}

// RemoveSandboxMatcher removes the matcher m from every rule, and the rules
// it leaves without matchers. It reports whether it removed any.
func (p *PolicyConfig) RemoveSandboxMatcher(m string) bool {
	var out []SandboxRule
	removed := false
	for _, r := range p.Sandbox {
		match := slices.DeleteFunc(slices.Clone(r.Match), func(e string) bool { return e == m })
		if len(match) < len(r.Match) {
			removed = true
		}
		if len(match) == 0 {
			continue
		}
		r.Match = match
		out = append(out, r)
	}
	p.Sandbox = out
	return removed
}

// Clone returns a deep copy of p.
func (p PolicyConfig) Clone() PolicyConfig { return p.clone() }

func cloneSandbox(rules []SandboxRule) []SandboxRule {
	if rules == nil {
		return nil
	}
	out := make([]SandboxRule, len(rules))
	for i, r := range rules {
		out[i] = SandboxRule{Match: slices.Clone(r.Match), Storage: r.Storage, Secrets: slices.Clone(r.Secrets)}
	}
	return out
}

// loadedRules returns rules without the grants of secrets that aren't in
// secrets. A rule that loses all its grants gets none, so that it doesn't
// take the secrets of a broader rule.
func loadedRules(rules []SandboxRule, secrets []string) []SandboxRule {
	out := cloneSandbox(rules)
	for i, r := range out {
		kept := slices.DeleteFunc(r.Secrets, func(e string) bool {
			_, from, ok := ParseSecretGrant(e)
			return ok && e != SecretsAll && e != SecretsNone && !slices.Contains(secrets, from)
		})
		if len(kept) < len(rules[i].Secrets) && len(kept) == 0 {
			kept = []string{SecretsNone}
		}
		out[i].Secrets = kept
	}
	return out
}

// validSandboxMatcher reports whether m is a matcher of a sandbox rule.
func validSandboxMatcher(m string) bool {
	switch m {
	case MatchSelf, matchPrivate, MatchGuests:
		return true
	}
	if e, ok := strings.CutPrefix(m, matchEmail); ok {
		return ValidEmailPattern(e)
	}
	name, ok := strings.CutPrefix(m, MatchPairingPrefix)
	return ok && name != ""
}

// validateSandbox checks the sandbox rules of p against the agent's secrets.
// A problem of the rule of --guest-storage and --guest-secrets names the
// flag, and that of a pairing's rule names the pairing.
func (p PolicyConfig) validateSandbox(secrets []string) []error {
	var errs []error
	seen := map[string]int{}
	for i, r := range p.Sandbox {
		guests := slices.Equal(r.Match, []string{MatchGuests})
		label := fmt.Sprintf("policy.sandbox rule %d", i+1)
		drop := "remove it from the rule"
		if name, ok := strings.CutPrefix(first(r.Match), MatchPairingPrefix); ok && len(r.Match) == 1 {
			label, drop = name+"'s sandbox rule", "drop it from --secrets"
		}
		invalid := func(flag, entry, format string, args ...any) {
			if guests && flag != "" {
				errs = append(errs, &FieldError{Flag: flag, Entry: entry, Reason: fmt.Sprintf(format, args...)})
				return
			}
			errs = append(errs, fmt.Errorf("%s: %s", label, fmt.Sprintf(format, args...)))
		}
		if len(r.Match) == 0 {
			invalid("", "", "want at least one matcher in match")
		}
		for _, m := range r.Match {
			switch j, dup := seen[m]; {
			case !validSandboxMatcher(m):
				invalid("", m, "unknown matcher %q: want self, private, guests, email:<address>, email:*@<domain> or pairing:<name>", m)
			case dup && j != i:
				invalid("", m, "%s is in rule %d too; a matcher appears in one rule at most", m, j+1)
			}
			seen[m] = i
		}
		if r.Storage != "" && !slices.Contains(StorageModes, r.Storage) {
			invalid("guest-storage", r.Storage, "unknown storage: want shared, read-only, own or empty")
		}
		for _, e := range r.Secrets {
			if e == SecretsAll || e == SecretsNone {
				if len(r.Secrets) > 1 {
					invalid("guest-secrets", strings.Join(r.Secrets, ","), "%s goes alone: pass it without names", e)
				}
				continue
			}
			_, from, ok := ParseSecretGrant(e)
			switch {
			case !ok:
				invalid("guest-secrets", e, "invalid secret: want a name from secrets, all, or NAME=secret:OTHER")
			case !slices.Contains(secrets, from):
				invalid("guest-secrets", e, "secret %s is not one of the agent's secrets (%s); add it with `a2a-serve config set --secret %s`, or %s", from, orNone(secrets), from, drop)
			}
		}
	}
	return errs
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "it has none"
	}
	return strings.Join(names, ", ")
}

// checkSecretsUnused refuses to remove a secret of names that a sandbox
// rule names, and says how to change that rule first.
func (p PolicyConfig) checkSecretsUnused(names []string) error {
	for _, r := range p.Sandbox {
		for _, e := range r.Secrets {
			_, from, ok := ParseSecretGrant(e)
			if !ok || !slices.Contains(names, from) {
				continue
			}
			fix := "edit it with `a2a-serve config edit` first"
			who := "a sandbox rule"
			switch name, pairing := strings.CutPrefix(first(r.Match), MatchPairingPrefix); {
			case slices.Equal(r.Match, []string{MatchGuests}):
				who, fix = "the rule of guests", "run `a2a-serve config unset guest-secrets` first"
			case pairing && len(r.Match) == 1:
				who, fix = name+"'s sandbox rule", fmt.Sprintf("change it with `a2a-serve pair set %s --secrets` first", name)
			}
			return clierr.UsageNoHint(fmt.Sprintf("%s is used by %s; %s", from, who, fix))
		}
	}
	return nil
}

// guestRule is the rule of guests, which
// --guest-storage and --guest-secrets set.
func guestRule(c *Config) SandboxRule {
	r, _ := c.Policy.SandboxRuleOf(MatchGuests)
	return r
}

func setGuestRule(c *Config, fn func(*SandboxRule)) {
	c.Policy = c.Policy.Clone()
	c.Policy.SetSandboxRule(c.Policy.OwnRule(MatchGuests, fn))
}

// OwnRule returns the rule whose only matcher is m, changed by fn. When m
// is in a rule with other matchers, it leaves that rule, and the new rule
// starts from it.
func (p *PolicyConfig) OwnRule(m string, fn func(*SandboxRule)) SandboxRule {
	r, ok := p.SandboxRuleOf(m)
	if !ok {
		if i := slices.IndexFunc(p.Sandbox, func(r SandboxRule) bool { return slices.Contains(r.Match, m) }); i >= 0 {
			r = SandboxRule{Storage: p.Sandbox[i].Storage, Secrets: slices.Clone(p.Sandbox[i].Secrets)}
			p.RemoveSandboxMatcher(m)
		}
	}
	r.Match = []string{m}
	fn(&r)
	return r
}

// sandboxRules reads and writes the sandbox rules as one JSON entry each,
// except the rule of --guest-storage and --guest-secrets.
func sandboxRules() access {
	isCaller := func(r SandboxRule) bool { return slices.Equal(r.Match, []string{MatchGuests}) }
	return access{
		list: true,
		get: func(c *Config) []string {
			var out []string
			for _, r := range c.Policy.Sandbox {
				if isCaller(r) {
					continue
				}
				data, err := json.Marshal(r)
				if err != nil {
					continue
				}
				out = append(out, string(data))
			}
			return out
		},
		put: func(c *Config, v []string) {
			var rules []SandboxRule
			for _, e := range v {
				var r SandboxRule
				if err := json.Unmarshal([]byte(e), &r); err == nil {
					rules = append(rules, r)
				}
			}
			rules = append(rules, slices.DeleteFunc(slices.Clone(c.Policy.Sandbox), func(r SandboxRule) bool { return !isCaller(r) })...)
			c.Policy.Sandbox = rules
		},
		arg: func(*Settings) any { return nil },
	}
}

// sandboxRuleKey is the matchers of a sandbox rule entry, or the argument
// that names one.
func sandboxRuleKey(e string) string {
	var r SandboxRule
	if err := json.Unmarshal([]byte(e), &r); err == nil {
		return strings.Join(r.Match, ",")
	}
	return e
}

// showSandboxRule shows a sandbox rule entry like [ShowSandboxRule].
func showSandboxRule(e string) string {
	var r SandboxRule
	if err := json.Unmarshal([]byte(e), &r); err != nil {
		return e
	}
	return ShowSandboxRule(r)
}

// ShowSandboxRule shows r as its callers, storage and secrets, such as
// "pairing:ben: own, secrets KEY" or "guests: default storage,
// secrets none".
func ShowSandboxRule(r SandboxRule) string {
	who := strings.Join(r.Match, ",")
	if slices.Equal(r.Match, []string{MatchGuests}) {
		who = "guests"
	}
	storage := r.Storage
	if storage == "" {
		storage = "default storage"
	}
	out := who + ": " + storage
	if len(r.Secrets) == 0 {
		return out
	}
	shown := make([]string, len(r.Secrets))
	for i, e := range r.Secrets {
		shown[i] = e
		if name, from, ok := ParseSecretGrant(e); ok && name != from {
			shown[i] = name + " (from " + from + ")"
		}
	}
	return out + ", secrets " + strings.Join(shown, ", ")
}

// IsSandboxSetting reports whether the setting changes the sandbox rules.
func IsSandboxSetting(setting string) bool {
	return setting == SandboxSetting || setting == "guest-storage" || setting == "guest-secrets"
}

// SandboxSetting names the sandbox rules, which config.yaml and the pair
// commands set.
const SandboxSetting = "sandbox-rules"

// mergeGuestSecrets replaces the secrets of the rule of --guest-secrets
// with the entries of the flag, so that all and none can go alone.
func mergeGuestSecrets(s *Settings, cfg *Config, _ Dir, _ Secrets) error {
	var entries []string
	for _, v := range s.GuestSecrets {
		for e := range strings.SplitSeq(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
	}
	if len(entries) > 0 {
		setGuestRule(cfg, func(r *SandboxRule) { r.Secrets = entries })
	}
	return nil
}

// secretGrantKey is the variable name of an entry of --guest-secrets.
func secretGrantKey(e string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(e), "=")
	return name
}
