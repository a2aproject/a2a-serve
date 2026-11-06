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
	"slices"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
)

// FieldError is an invalid entry of a setting. It names the setting by its
// flag, or by its key in config.yaml once [FileProblems] or
// [FileProblemsOf] found the entry there.
type FieldError struct {
	Flag  string
	Entry string
	// Reason says why the entry is invalid, such as "want docker or local".
	Reason string
	inFile bool
}

func (e *FieldError) Error() string {
	if f, ok := lookupField(e.Flag); ok && e.inFile && f.path != "" {
		return fmt.Sprintf("%s: %s has %q: %s", configFile, f.path, e.Entry, e.Reason)
	}
	return fmt.Sprintf("invalid --%s %q: %s", e.Flag, e.Entry, e.Reason)
}

// fileError is a problem of config.yaml that has no entry of its own.
type fileError struct{ err error }

func (e *fileError) Error() string { return configFile + ": " + e.err.Error() }

func (e *fileError) Unwrap() error { return e.err }

// valueError is an invalid value, whose reason a [FieldError] can take.
type valueError struct{ what, value, reason string }

func (e *valueError) Error() string {
	return fmt.Sprintf("invalid %s %q: %s", e.what, e.value, e.reason)
}

// reasonOf returns why err rejected a value, without the value.
func reasonOf(err error) string {
	var ve *valueError
	if errors.As(err, &ve) {
		return ve.reason
	}
	return err.Error()
}

// FileProblems returns err, an error of [Config.Validate], [Dir.CheckConfig]
// or [ParseConfig], as problems of config.yaml, with a hint at the command
// that fixes them.
func FileProblems(err error) error {
	return markProblems(err, func(error) bool { return true })
}

// FileProblemsOf is [FileProblems] for the problems of err that old, the
// error of the saved config, has too. The others are problems of the
// settings just given, and keep naming their flags.
func FileProblemsOf(err, old error) error {
	var saved []string
	for _, p := range problems(old) {
		saved = append(saved, p.Error())
	}
	return markProblems(err, func(p error) bool { return slices.Contains(saved, p.Error()) })
}

func markProblems(err error, inFile func(error) bool) error {
	if err == nil {
		return nil
	}
	var (
		out  []error
		hint = agenterr.HintConfigEdit
		arg  string
		n    int
	)
	for _, p := range problems(err) {
		if !inFile(p) {
			out = append(out, p)
			continue
		}
		n++
		var fe *FieldError
		if !errors.As(p, &fe) {
			out = append(out, &fileError{err: p})
			continue
		}
		marked := *fe
		marked.inFile = true
		out = append(out, &marked)
		if n == 1 {
			hint, arg = fixHint(fe)
		}
	}
	joined := errors.Join(out...)
	if n == 0 {
		return joined
	}
	if n > 1 {
		hint, arg = agenterr.HintConfigEdit, ""
	}
	return agenterr.WithHint(joined, hint, arg)
}

// fixHint names the command that removes the entry of e from config.yaml.
func fixHint(e *FieldError) (agenterr.Hint, string) {
	f, ok := lookupField(e.Flag)
	if !ok || CheckUnset(e.Flag) != nil {
		return agenterr.HintConfigEdit, ""
	}
	if !f.list {
		return agenterr.HintConfigUnset, e.Flag
	}
	return agenterr.HintConfigUnset, e.Flag + " " + shellQuote(e.Entry)
}

// problems returns the problems joined in err.
func problems(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}
