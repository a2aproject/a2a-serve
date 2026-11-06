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
	"maps"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// SecretPrefix marks a config value as a reference to a stored secret.
const SecretPrefix = "secret:"

// Secrets are the resolved tokens and API keys of an agent, kept in
// state/secrets with mode 0600 so that a detached or service process
// doesn't depend on the environment of the shell which started it.
type Secrets map[string]string

// LoadSecrets reads the secrets file. A missing file is empty.
func (d Dir) LoadSecrets() (Secrets, error) { return LoadSecretsFile(d.SecretsPath()) }

// LoadSecretsFile reads the secrets file at path. A missing file is empty.
func LoadSecretsFile(path string) (Secrets, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Secrets{}, nil
	}
	if err != nil {
		return nil, err
	}
	s := Secrets{}
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return s, nil
}

// SaveSecrets merges s into the secrets file.
func (d Dir) SaveSecrets(s Secrets) error {
	if len(s) == 0 {
		return nil
	}
	if err := d.Init(); err != nil {
		return err
	}
	return SaveSecretsFile(d.SecretsPath(), s)
}

// SaveSecretsFile merges s into the secrets file at path, whose directory
// exists.
func SaveSecretsFile(path string, s Secrets) error {
	if len(s) == 0 {
		return nil
	}
	return fsutil.WithLock(path, func() error {
		cur, err := LoadSecretsFile(path)
		if err != nil {
			return err
		}
		maps.Copy(cur, s)
		data, err := yaml.Marshal(cur)
		if err != nil {
			return err
		}
		return fsutil.WriteAtomic(path, data, 0o600)
	})
}

// DeleteSecrets removes the named secrets from the secrets file.
func (d Dir) DeleteSecrets(names ...string) error {
	if len(names) == 0 {
		return nil
	}
	if _, err := os.Stat(d.SecretsPath()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fsutil.WithLock(d.SecretsPath(), func() error {
		cur, err := d.LoadSecrets()
		if err != nil {
			return err
		}
		for _, name := range names {
			delete(cur, name)
		}
		data, err := yaml.Marshal(cur)
		if err != nil {
			return err
		}
		return fsutil.WriteAtomic(d.SecretsPath(), data, 0o600)
	})
}

// Resolve returns the value of a config field: the secret it references, or
// the value itself.
func (s Secrets) Resolve(v string) (string, error) {
	name, ok := strings.CutPrefix(v, SecretPrefix)
	if !ok {
		return v, nil
	}
	val, ok := s[name]
	if !ok || val == "" {
		return "", fmt.Errorf("secret %s is not set; pass it again with the flag that set it", name)
	}
	return val, nil
}

// ResolveInput turns a flag value into a secret reference and the secret to
// store. env:VAR reads VAR from the environment of the CLI and stores it
// under VAR; any other value is stored under fallbackName.
func ResolveInput(v, fallbackName string) (ref string, s Secrets, err error) {
	if name, ok := strings.CutPrefix(v, "env:"); ok {
		val := os.Getenv(name)
		if val == "" {
			return "", nil, fmt.Errorf("environment variable %s is empty", name)
		}
		return SecretPrefix + name, Secrets{name: val}, nil
	}
	if name, ok := strings.CutPrefix(v, SecretPrefix); ok {
		return SecretPrefix + name, nil, nil
	}
	if v == "" {
		return "", nil, errors.New("empty secret value")
	}
	return SecretPrefix + fallbackName, Secrets{fallbackName: v}, nil
}

// StoreSecretValues moves the values of the secret settings that next, a
// config.yaml edited by hand, changed since old to the secrets to store, as
// the flags of the settings do. next keeps references to them. Messengers
// are left alone, since only `a2a-serve channel add` changes them.
func StoreSecretValues(old, next *Config) (Secrets, error) {
	secrets := Secrets{}
	for _, f := range fields {
		if f.secret == "" || f.secretOnly || f.place == placeChannel {
			continue
		}
		v := first(f.get(next))
		if v == "" || v == first(f.get(old)) {
			continue
		}
		ref, sec, err := ResolveInput(v, f.secret)
		if err != nil {
			return nil, &FieldError{Flag: f.flag, Entry: v, Reason: err.Error()}
		}
		maps.Copy(secrets, sec)
		f.put(next, []string{ref})
	}
	return secrets, nil
}
