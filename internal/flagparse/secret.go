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

package flagparse

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/a2aproject/a2a-serve/internal/clierr"
)

// SecretSyntax is the help text of a flag which takes a secret value.
const SecretSyntax = "secret value: env:VAR, -, or the value"

// SecretHelp is the paragraph on secret values in the long help of commands
// with secret-valued flags.
const SecretHelp = `Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.`

const (
	envPrefix    = "env:"
	storedPrefix = "secret:"
	// SecretStdin is the value which reads the secret from a prompt or stdin.
	SecretStdin = "-"
)

// SecretKind is the form of a secret value.
type SecretKind int

const (
	// SecretLiteral is the value itself.
	SecretLiteral SecretKind = iota
	// SecretEnv is env:VAR.
	SecretEnv
	// SecretPrompted is -, read from a prompt or stdin.
	SecretPrompted
	// SecretStored is secret:NAME.
	SecretStored
)

// SecretRef is a parsed secret value.
type SecretRef struct {
	Kind SecretKind
	// Name is the variable of SecretEnv, or the stored secret of SecretStored.
	Name string
	// Value is the secret, empty for SecretStored.
	Value string
}

// ParseSecret parses value, the value of --flag, reading - with r. Stored
// values are not read: only an agent's settings resolve them.
func ParseSecret(flag, value string, r *SecretReader) (SecretRef, error) {
	switch {
	case value == SecretStdin:
		v, err := r.Read("--" + flag)
		if err != nil {
			return SecretRef{}, err
		}
		if v == "" {
			return SecretRef{}, clierr.Usage(fmt.Sprintf("--%s: empty secret", flag))
		}
		return SecretRef{Kind: SecretPrompted, Value: v}, nil
	case strings.HasPrefix(value, envPrefix):
		name := strings.TrimPrefix(value, envPrefix)
		v := os.Getenv(name)
		if v == "" {
			return SecretRef{}, clierr.Usage(fmt.Sprintf("--%s: environment variable %s is empty", flag, name))
		}
		return SecretRef{Kind: SecretEnv, Name: name, Value: v}, nil
	case strings.HasPrefix(value, storedPrefix):
		name := strings.TrimPrefix(value, storedPrefix)
		if name == "" {
			return SecretRef{}, clierr.Usage(fmt.Sprintf("--%s: secret: needs a name", flag))
		}
		return SecretRef{Kind: SecretStored, Name: name}, nil
	case value == "":
		return SecretRef{}, clierr.Usage(fmt.Sprintf("--%s: empty secret", flag))
	}
	return SecretRef{Kind: SecretLiteral, Value: value}, nil
}

// ResolveSecret returns the secret of value, the value of --flag of a command
// which has no stored secrets.
func ResolveSecret(flag, value string, r *SecretReader) (string, error) {
	ref, err := ParseSecret(flag, value, r)
	if err != nil {
		return "", err
	}
	if ref.Kind == SecretStored {
		return "", clierr.Usage(fmt.Sprintf("--%s: secret:NAME only applies to agent settings; pass env:VAR, - or the value", flag))
	}
	return ref.Value, nil
}

// ResolveSecretHeader returns the value of a "Name: value" header given to
// --flag. The value may end in a secret value, as in "Authorization: Bearer
// env:TOKEN".
func ResolveSecretHeader(flag, header string, r *SecretReader) (name, value string, err error) {
	k, v, ok := strings.Cut(header, ":")
	name = strings.TrimSpace(k)
	if !ok || name == "" {
		return "", "", clierr.Usage(fmt.Sprintf("--%s must be \"Name: value\", got %q", flag, header))
	}
	v = strings.TrimSpace(v)
	head, last := "", v
	if i := strings.LastIndexByte(v, ' '); i >= 0 {
		head, last = v[:i+1], v[i+1:]
	}
	if last != SecretStdin && !strings.HasPrefix(last, envPrefix) {
		return name, v, nil
	}
	secret, err := ResolveSecret(flag+" "+name, last, r)
	if err != nil {
		return "", "", err
	}
	return name, head + secret, nil
}

// SecretReader reads secret values typed on a terminal without echo, or piped
// to stdin, which only one flag may take.
type SecretReader struct {
	in       *os.File
	prompt   io.Writer
	consumed bool
}

// NewSecretReader returns a reader of in, which prompts on prompt.
func NewSecretReader(in *os.File, prompt io.Writer) *SecretReader {
	return &SecretReader{in: in, prompt: prompt}
}

// Terminal reports whether the reader prompts on a terminal.
func (r *SecretReader) Terminal() bool { return term.IsTerminal(int(r.in.Fd())) }

// Read returns a secret: from a prompt labeled label on a terminal, or else
// all of stdin.
func (r *SecretReader) Read(label string) (string, error) {
	if r.Terminal() {
		if _, err := fmt.Fprintf(r.prompt, "%s: ", label); err != nil {
			return "", err
		}
		b, err := term.ReadPassword(int(r.in.Fd()))
		if _, perr := fmt.Fprintln(r.prompt); perr != nil {
			return "", perr
		}
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	if r.consumed {
		return "", clierr.Usage("only one flag can read its secret from stdin; pass the others as env:VAR")
	}
	r.consumed = true
	b, err := io.ReadAll(r.in)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
