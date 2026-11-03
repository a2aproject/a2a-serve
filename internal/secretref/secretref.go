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

// Package secretref parses the values of flags which take a secret: a
// reference to an environment variable, a prompt or stdin, a secret already
// stored with the agent, or the value itself.
package secretref

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/a2aproject/a2a-serve/internal/clierr"
)

// Syntax is the help text of a flag which takes a secret value.
const Syntax = "secret value: env:VAR, -, or the value"

// Help is the paragraph on secret values in the long help of commands with
// secret-valued flags.
const Help = `Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.`

const (
	envPrefix    = "env:"
	storedPrefix = "secret:"
	// Stdin is the value which reads the secret from a prompt or stdin.
	Stdin = "-"
)

// Kind is the form of a secret value.
type Kind int

const (
	// Literal is the value itself.
	Literal Kind = iota
	// Env is env:VAR.
	Env
	// Prompted is -, read from a prompt or stdin.
	Prompted
	// Stored is secret:NAME.
	Stored
)

// Ref is a parsed secret value.
type Ref struct {
	Kind Kind
	// Name is the variable of Env, or the stored secret of Stored.
	Name string
	// Value is the secret, empty for Stored.
	Value string
}

// Parse parses value, the value of --flag, reading - with r. Stored values
// are not read: only an agent's settings resolve them.
func Parse(flag, value string, r *Reader) (Ref, error) {
	switch {
	case value == Stdin:
		v, err := r.Read("--" + flag)
		if err != nil {
			return Ref{}, err
		}
		if v == "" {
			return Ref{}, clierr.Usage(fmt.Sprintf("--%s: empty secret", flag))
		}
		return Ref{Kind: Prompted, Value: v}, nil
	case strings.HasPrefix(value, envPrefix):
		name := strings.TrimPrefix(value, envPrefix)
		v := os.Getenv(name)
		if v == "" {
			return Ref{}, clierr.Usage(fmt.Sprintf("--%s: environment variable %s is empty", flag, name))
		}
		return Ref{Kind: Env, Name: name, Value: v}, nil
	case strings.HasPrefix(value, storedPrefix):
		name := strings.TrimPrefix(value, storedPrefix)
		if name == "" {
			return Ref{}, clierr.Usage(fmt.Sprintf("--%s: secret: needs a name", flag))
		}
		return Ref{Kind: Stored, Name: name}, nil
	case value == "":
		return Ref{}, clierr.Usage(fmt.Sprintf("--%s: empty secret", flag))
	}
	return Ref{Kind: Literal, Value: value}, nil
}

// Resolve returns the secret of value, the value of --flag of a command
// which has no stored secrets.
func Resolve(flag, value string, r *Reader) (string, error) {
	ref, err := Parse(flag, value, r)
	if err != nil {
		return "", err
	}
	if ref.Kind == Stored {
		return "", clierr.Usage(fmt.Sprintf("--%s: secret:NAME only applies to agent settings; pass env:VAR, - or the value", flag))
	}
	return ref.Value, nil
}

// ResolveHeader returns the value of a "Name: value" header given to --flag.
// The value may end in a secret value, as in "Authorization: Bearer
// env:TOKEN".
func ResolveHeader(flag, header string, r *Reader) (name, value string, err error) {
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
	if last != Stdin && !strings.HasPrefix(last, envPrefix) {
		return name, v, nil
	}
	secret, err := Resolve(flag+" "+name, last, r)
	if err != nil {
		return "", "", err
	}
	return name, head + secret, nil
}

// Reader reads secret values typed on a terminal without echo, or piped to
// stdin, which only one flag may take.
type Reader struct {
	in       *os.File
	prompt   io.Writer
	consumed bool
}

// NewReader returns a reader of in, which prompts on prompt.
func NewReader(in *os.File, prompt io.Writer) *Reader {
	return &Reader{in: in, prompt: prompt}
}

// Terminal reports whether the reader prompts on a terminal.
func (r *Reader) Terminal() bool { return term.IsTerminal(int(r.in.Fd())) }

// Read returns a secret: from a prompt labeled label on a terminal, or else
// all of stdin.
func (r *Reader) Read(label string) (string, error) {
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
