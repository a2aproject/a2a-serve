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

package agent

import (
	"slices"

	"github.com/a2aproject/a2a-serve/core/egress"
)

// Mount is a host directory the agent works on, as a src[:dst][:ro] spec
// describes it.
type Mount struct {
	Src      string
	Dst      string
	ReadOnly bool
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

// InjectRule is a header the egress boundary adds to the HTTPS requests of
// the agent's container to a host. The secrets it carries stay out of the
// agent's environment.
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

// IsModel reports whether r sets the key of a model provider.
func (r InjectRule) IsModel() bool { return r.Model != "" }

// Entry returns r as one host=header or provider=key entry.
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

// InjectedSecrets returns the names of the secrets that rules add, in the
// order of the rules.
func InjectedSecrets(rules []InjectRule) []string {
	var out []string
	for _, r := range rules {
		for _, name := range r.Secrets() {
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}
