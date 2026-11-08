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
)

// SandboxRule sets what the agent containers of the callers it matches get:
// their storage and their secrets.
type SandboxRule struct {
	// Match lists the callers: self, private, anyone, email:<address>,
	// email:*@<domain> or pairing:<name>.
	Match []string `yaml:"match,flow" json:"match"`
	// Storage is one of the Storage* modes. Empty is read-only.
	Storage string `yaml:"storage,omitempty" json:"storage,omitempty"`
	// Secrets are names from the agent's secrets, all, or NAME=secret:OTHER,
	// which passes the secret OTHER as NAME.
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

// SecretsAll in the secrets of a rule passes every secret of the agent.
const SecretsAll = "all"

// Sandbox rule matchers besides the members of the policy.
const (
	// MatchPairingPrefix starts pairing:<name>, which matches a pairing or
	// a webhook node.
	MatchPairingPrefix = "pairing:"
	// MatchAnyone matches every caller who isn't an owner.
	MatchAnyone = "anyone"
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

// validSandboxMatcher reports whether m is a matcher of a sandbox rule.
func validSandboxMatcher(m string) bool {
	switch m {
	case MatchSelf, matchPrivate, MatchAnyone:
		return true
	}
	if e, ok := strings.CutPrefix(m, matchEmail); ok {
		return ValidEmailPattern(e)
	}
	name, ok := strings.CutPrefix(m, MatchPairingPrefix)
	return ok && name != ""
}

// validateSandbox checks the sandbox rules of p against the agent's secrets.
// A problem of the rule of --caller-storage and --caller-secrets names the
// flag, and that of a pairing's rule names the pairing.
func (p PolicyConfig) validateSandbox(secrets []string) []error {
	var errs []error
	seen := map[string]int{}
	for i, r := range p.Sandbox {
		caller := slices.Equal(r.Match, []string{MatchAnyone})
		label := fmt.Sprintf("policy.sandbox rule %d", i+1)
		if len(r.Match) == 1 && strings.HasPrefix(r.Match[0], MatchPairingPrefix) {
			label = "the sandbox rule of " + r.Match[0]
		}
		invalid := func(flag, entry, format string, args ...any) {
			if caller && flag != "" {
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
				invalid("", m, "unknown matcher %q: want self, private, anyone, email:<address>, email:*@<domain> or pairing:<name>", m)
			case dup && j != i:
				invalid("", m, "%s is in rule %d too; a caller is in one rule at most", m, j+1)
			}
			seen[m] = i
		}
		if r.Storage != "" && !slices.Contains(StorageModes, r.Storage) {
			invalid("caller-storage", r.Storage, "unknown storage: want %s", strings.Join(StorageModes, ", "))
		}
		for _, e := range r.Secrets {
			if e == SecretsAll {
				continue
			}
			_, from, ok := ParseSecretGrant(e)
			switch {
			case !ok:
				invalid("caller-secrets", e, "invalid secret: want a name from secrets, all, or NAME=secret:OTHER")
			case !slices.Contains(secrets, from):
				invalid("caller-secrets", e, "secret %s is not one of the agent's secrets (%s); add it with `a2a-serve config set --secret %s`, or remove it from the rule", from, orNone(secrets), from)
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

// callerRule is the rule of the callers who match nothing more specific,
// which --caller-storage and --caller-secrets set.
func callerRule(c *Config) SandboxRule {
	r, _ := c.Policy.SandboxRuleOf(MatchAnyone)
	return r
}

func setCallerRule(c *Config, fn func(*SandboxRule)) {
	r := callerRule(c)
	r.Match = []string{MatchAnyone}
	fn(&r)
	c.Policy.SetSandboxRule(r)
}

// sandboxRules reads and writes the sandbox rules as one JSON entry each,
// except the rule of --caller-storage and --caller-secrets.
func sandboxRules() access {
	isCaller := func(r SandboxRule) bool { return slices.Equal(r.Match, []string{MatchAnyone}) }
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

// ShowSandboxRule shows r as its matchers, storage and secrets, such as
// "pairing:ben: own, secrets KEY".
func ShowSandboxRule(r SandboxRule) string {
	storage := r.Storage
	if storage == "" {
		storage = StorageReadOnly
	}
	out := strings.Join(r.Match, ",") + ": " + storage
	if len(r.Secrets) > 0 {
		out += ", secrets " + strings.Join(r.Secrets, ",")
	}
	return out
}

// SandboxSetting names the sandbox rules, which config.yaml and the pair
// commands set.
const SandboxSetting = "sandbox-rules"

// parseSecretGrant refuses none as an entry of --caller-secrets, which
// would read as a secret's name.
func parseSecretGrant(_ Dir, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "none" {
		return "", fmt.Errorf("to give callers no secrets, run `a2a-serve config unset caller-secrets`")
	}
	return v, nil
}

// secretGrantKey is the variable name of an entry of --caller-secrets.
func secretGrantKey(e string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(e), "=")
	return name
}
