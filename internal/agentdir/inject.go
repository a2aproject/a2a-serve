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
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/egress"
)

// InjectRule is a header the server adds to the Docker sandbox's HTTPS
// requests to a host. The secrets it carries are the agent's secrets, which
// the sandbox rules grant as they grant the environment; the agent's
// environment doesn't get them.
type InjectRule struct {
	// Host and Header add a header to the requests to host[:port], as
	// "Name: value", where $NAME stands for the secret NAME.
	Host   string `yaml:"host,omitempty" json:"host,omitempty"`
	Header string `yaml:"header,omitempty" json:"header,omitempty"`
	// Model and Key add the secret Key as the API key of the model
	// provider Model. The agent's environment gets a placeholder as Key.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	Key   string `yaml:"key,omitempty" json:"key,omitempty"`
}

// Settings of the inject rules.
const (
	InjectHeaderSetting   = "inject-header"
	InjectModelKeySetting = "inject-model-key"
)

// IsModel reports whether r sets the key of a model provider.
func (r InjectRule) IsModel() bool { return r.Model != "" }

// Setting names the flag that sets r.
func (r InjectRule) Setting() string {
	if r.IsModel() {
		return InjectModelKeySetting
	}
	return InjectHeaderSetting
}

// Entry returns r as its flag takes it.
func (r InjectRule) Entry() string {
	if r.IsModel() {
		return r.Model + "=" + r.Key
	}
	return r.Host + "=" + r.Header
}

// HeaderRule returns the header rule of r. A model rule adds the key of
// its provider where the provider takes it.
func (r InjectRule) HeaderRule() (egress.HeaderRule, error) {
	if !r.IsModel() {
		return egress.ParseHeaderRule(r.Entry())
	}
	p, ok := egress.LookupProvider(r.Model)
	if !ok {
		return egress.HeaderRule{}, egress.UnknownProvider(r.Model)
	}
	header, scheme := p.Credential(r.Key)
	return egress.HeaderRule{Host: p.Host, Port: egress.HTTPSPort, Name: header, Value: scheme + "${" + r.Key + "}"}, nil
}

// Target returns the host the requests of r go to, as host or host:port.
func (r InjectRule) Target() string {
	h, err := r.HeaderRule()
	if err != nil {
		return r.Host
	}
	return h.Target()
}

// Secrets returns the names of the secrets r adds.
func (r InjectRule) Secrets() []string {
	if r.IsModel() {
		return []string{r.Key}
	}
	h, err := egress.ParseHeaderRule(r.Entry())
	if err != nil {
		return nil
	}
	return h.Secrets()
}

// Show returns r for status and changes, with secret names only, such as
// "openrouter.ai gets OPENROUTER_API_KEY".
func (r InjectRule) Show() string {
	return r.Target() + " gets " + strings.Join(r.Secrets(), ", ")
}

// InjectedSecrets returns the names of the secrets the inject rules of c
// add, in the order of the rules.
func (c *Config) InjectedSecrets() []string {
	var out []string
	for _, r := range c.Inject {
		for _, name := range r.Secrets() {
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// injectKey is the key of an entry of --inject-header or --inject-model-key,
// or of an argument that names one: the target of a header rule and the
// provider of a model rule.
func injectKey(model bool) func(string) string {
	return func(e string) string {
		k, _, _ := strings.Cut(strings.TrimSpace(e), "=")
		if model {
			return strings.TrimSpace(k)
		}
		host, port, err := egress.ParseInjectTarget(k)
		if err != nil {
			return k
		}
		return egress.HeaderRule{Host: host, Port: port}.Target()
	}
}

// injectRules reads and writes the rules of --inject-model-key, or of
// --inject-header, as one entry each, keeping the rules of the other.
func injectRules(model bool) access {
	return access{
		list: true,
		get: func(c *Config) []string {
			var out []string
			for _, r := range c.Inject {
				if r.IsModel() == model {
					out = append(out, r.Entry())
				}
			}
			return out
		},
		put: func(c *Config, v []string) {
			rules := slices.DeleteFunc(slices.Clone(c.Inject), func(r InjectRule) bool { return r.IsModel() == model })
			for _, e := range v {
				rules = append(rules, ruleOfEntry(model, e))
			}
			if len(rules) == 0 {
				rules = nil
			}
			c.Inject = rules
		},
		arg: func(s *Settings) any {
			if model {
				return &s.InjectModelKeys
			}
			return &s.InjectHeaders
		},
	}
}

func ruleOfEntry(model bool, e string) InjectRule {
	k, v, _ := strings.Cut(e, "=")
	if model {
		return InjectRule{Model: k, Key: v}
	}
	return InjectRule{Host: k, Header: v}
}

// mergeInject adds the rules of --inject-model-key, or of --inject-header,
// and the secrets they name. A secret's value comes from the environment,
// or else from the secret the agent already has.
func mergeInject(model bool) func(s *Settings, cfg *Config, d Dir, secrets Secrets) error {
	return func(s *Settings, cfg *Config, _ Dir, secrets Secrets) error {
		flag, values := InjectHeaderSetting, s.InjectHeaders
		if model {
			flag, values = InjectModelKeySetting, s.InjectModelKeys
		}
		before := cfg.InjectedSecrets()
		for _, e := range values {
			r, err := parseInjectEntry(model, e)
			if err != nil {
				return fmt.Errorf("--%s: %w", flag, err)
			}
			for _, name := range r.Secrets() {
				v := cmp.Or(secrets[name], s.SecretValues[name], os.Getenv(name))
				switch {
				case v != "":
					secrets[name] = v
				case !slices.Contains(cfg.Secrets, name):
					return fmt.Errorf("--%s %s: the environment variable %s is not set; set it, or store the secret first with `a2a-serve config set --secret %s`", flag, e, name, name)
				}
				cfg.Secrets = putKeyed(cfg.Secrets, name, nil)
			}
			cur := injectRules(model).get(cfg)
			injectRules(model).put(cfg, putKeyed(cur, r.Entry(), injectKey(model)))
		}
		// A replaced rule's secret leaves with it, unless --secret passes it
		// to the agent in the same command.
		return cfg.dropInjectedSecrets(before, s.Secrets)
	}
}

// parseInjectEntry parses an entry of --inject-model-key, or of
// --inject-header, into its rule in normal form.
func parseInjectEntry(model bool, e string) (InjectRule, error) {
	if !model {
		h, err := egress.ParseHeaderRule(e)
		if err != nil {
			return InjectRule{}, err
		}
		return InjectRule{Host: h.Target(), Header: h.Name + ": " + h.Value}, nil
	}
	provider, key, ok := strings.Cut(strings.TrimSpace(e), "=")
	provider, key = strings.TrimSpace(provider), strings.TrimSpace(key)
	if !ok || !secretName.MatchString(key) {
		return InjectRule{}, fmt.Errorf("invalid model key %q: want provider=NAME, such as openrouter=OPENROUTER_API_KEY, with NAME an environment variable", e)
	}
	if _, ok := egress.LookupProvider(provider); !ok {
		return InjectRule{}, egress.UnknownProvider(provider)
	}
	return InjectRule{Model: provider, Key: key}, nil
}

// validateInject checks the inject rules of c against its secrets and its
// sandbox.
func (c *Config) validateInject() []error {
	var errs []error
	targets := map[string]InjectRule{}
	for _, r := range c.Inject {
		invalid := func(format string, args ...any) {
			errs = append(errs, &FieldError{Flag: r.Setting(), Entry: r.Entry(), Reason: fmt.Sprintf(format, args...)})
		}
		if r.IsModel() == (r.Host != "" || r.Header != "") || r.IsModel() == (r.Key == "") {
			errs = append(errs, fmt.Errorf("inject: a rule has either host and header, or model and key"))
			continue
		}
		parsed, err := parseInjectEntry(r.IsModel(), r.Entry())
		if err != nil {
			invalid("%s", reasonOf(err))
			continue
		}
		if prev, dup := targets[parsed.Target()]; dup {
			invalid("%s already gets a header from %s %s; a host takes one rule", parsed.Target(), prev.Setting(), prev.Entry())
		}
		targets[parsed.Target()] = r
		for _, name := range parsed.Secrets() {
			if !slices.Contains(c.Secrets, name) {
				invalid("secret %s is not one of the agent's secrets; store it with `a2a-serve config set --secret %s`", name, name)
			}
		}
	}
	if len(c.Inject) == 0 {
		return errs
	}
	// A grant that passes an added secret under a name that no rule adds
	// would put the secret in the environment.
	injected := c.InjectedSecrets()
	for _, r := range c.Policy.Sandbox {
		for _, e := range r.Secrets {
			if name, from, ok := ParseSecretGrant(e); ok && slices.Contains(injected, from) && !slices.Contains(injected, name) {
				errs = append(errs, fmt.Errorf("the sandbox rule of %s passes %s as %s, which would put it in the agent's environment; the inject rules keep %s out of the sandbox, so pass it as %s", strings.Join(r.Match, ","), from, name, from, from))
			}
		}
	}
	switch {
	case c.Sandbox == SandboxLocal:
		errs = append(errs, fmt.Errorf("--%s needs the Docker sandbox: with --sandbox local, the agent can read the secrets in .a2a/state/secrets; use --sandbox docker, or remove the rules with `a2a-serve config unset %s`", c.Inject[0].Setting(), c.Inject[0].Setting()))
	case c.Egress.Mode == egress.ModeUnrestricted:
		errs = append(errs, fmt.Errorf("--%s needs the server to see the sandbox's connections, which --egress unrestricted turns off; use --egress internet, or remove the rules with `a2a-serve config unset %s`", c.Inject[0].Setting(), c.Inject[0].Setting()))
	}
	return errs
}

// checkInjectUnused refuses to remove a secret of names that an inject rule
// adds, and says how to remove that rule first.
func (c *Config) checkInjectUnused(names []string) error {
	for _, r := range c.Inject {
		for _, name := range r.Secrets() {
			if slices.Contains(names, name) {
				return clierr.UsageNoHint(fmt.Sprintf("%s is used by --%s %s; run `a2a-serve config unset %s %s` first", name, r.Setting(), injectKey(r.IsModel())(r.Entry()), r.Setting(), injectKey(r.IsModel())(r.Entry())))
			}
		}
	}
	return nil
}

// dropInjectedSecrets removes from c the secrets of before, which inject
// rules added, that the rules of c no longer add, so that a removed rule
// doesn't pass its secret to the agent's environment. It keeps the names
// of keep, and refuses when a sandbox rule names one it would remove.
func (c *Config) dropInjectedSecrets(before, keep []string) error {
	now := c.InjectedSecrets()
	var dropped []string
	for _, name := range before {
		if !slices.Contains(now, name) && !slices.Contains(keep, name) {
			dropped = append(dropped, name)
		}
	}
	if err := c.Policy.checkSecretsUnused(dropped); err != nil {
		return err
	}
	c.Secrets = slices.DeleteFunc(slices.Clone(c.Secrets), func(name string) bool { return slices.Contains(dropped, name) })
	if len(c.Secrets) == 0 {
		c.Secrets = nil
	}
	return nil
}
