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

package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/secretref"
)

// settingsFlags are the agent settings shared by `a2a-serve start <agent>` and
// `a2a-serve config set`, so both speak one vocabulary.
type settingsFlags struct {
	s agentdir.Settings
}

// secretUsage is the help of a flag which takes a secret value.
func secretUsage(usage string) string {
	return usage + " (" + secretref.Syntax + ")"
}

// listUsage labels the help of a list flag. split says whether commas also
// separate values.
func listUsage(usage string, split bool) string {
	if split {
		return usage + " (repeatable; commas also separate values)"
	}
	return usage + " (repeatable)"
}

func (sf *settingsFlags) register(cmd *cobra.Command, sets agentdir.FlagSet) {
	for _, fl := range sf.s.Flags(sets) {
		if fl.Name == "command" && cmd.Name() == agentdir.HarnessACP {
			fl.Usage = "Command line of the ACP agent, as shell `words`, such as \"gemini --experimental-acp\""
		}
		registerSettingFlag(cmd, fl)
	}
}

// registerSettingFlag adds a settings flag to cmd, in its help section.
func registerSettingFlag(cmd *cobra.Command, fl agentdir.Flag) {
	f := cmd.Flags()
	usage := fl.Usage
	if fl.Secret {
		usage = secretUsage(usage)
	}
	switch v := fl.Value.(type) {
	case *string:
		f.StringVar(v, fl.Name, "", usage)
	case *int:
		f.IntVar(v, fl.Name, 0, usage)
	case *[]string:
		if fl.Split {
			f.StringSliceVar(v, fl.Name, nil, listUsage(usage, true))
		} else {
			f.StringArrayVar(v, fl.Name, nil, listUsage(usage, false))
		}
	default:
		panic(fmt.Sprintf("flag --%s has an unsupported value %T", fl.Name, v))
	}
	if fl.Section != "" {
		setSection(f, fl.Section, fl.Name)
	}
	if len(fl.Values) > 0 {
		must(cmd.RegisterFlagCompletionFunc(fl.Name, fixedCompletions(fl.Values...)))
	}
}

// settings returns the parsed settings. Secret-valued flags set to - are
// read with r, and so are --secret values missing from the environment
// when a terminal can prompt for them.
func (sf *settingsFlags) settings(r *secretref.Reader) (agentdir.Settings, error) {
	s := sf.s
	if err := readSecretFlags(&s, agentdir.CoreFlags|agentdir.OperationalFlags, r); err != nil {
		return s, err
	}
	s.Secrets = slices.Clone(s.Secrets)
	for i, entry := range s.Secrets {
		name, value, ok := strings.Cut(entry, "=")
		s.Secrets[i] = name
		if !ok && (os.Getenv(name) != "" || !r.Terminal()) {
			continue
		}
		if !ok {
			value = secretref.Stdin
		}
		ref, err := secretref.Parse("secret "+name, value, r)
		if err != nil {
			return s, err
		}
		if ref.Kind == secretref.Stored {
			if s.SecretSources == nil {
				s.SecretSources = map[string]string{}
			}
			s.SecretSources[name] = ref.Name
			continue
		}
		if s.SecretValues == nil {
			s.SecretValues = agentdir.Secrets{}
		}
		s.SecretValues[name] = ref.Value
	}
	return s, nil
}

// readSecretFlags reads the values of the secret-valued scalar flags of
// sets in s that are set to - or prompted for.
func readSecretFlags(s *agentdir.Settings, sets agentdir.FlagSet, r *secretref.Reader) error {
	for _, fl := range s.Flags(sets) {
		dst, ok := fl.Value.(*string)
		if !fl.Secret || !ok || *dst == "" {
			continue
		}
		if err := readSecretFlag(fl.Name, dst, r); err != nil {
			return err
		}
	}
	return nil
}

// readSecretFlag replaces the value of a secret-valued flag set to - or
// prompted for with the secret. Env and stored references stay references,
// which the settings store under their names.
func readSecretFlag(flag string, dst *string, r *secretref.Reader) error {
	ref, err := secretref.Parse(flag, *dst, r)
	if err != nil {
		return err
	}
	if ref.Kind == secretref.Literal || ref.Kind == secretref.Prompted {
		*dst = ref.Value
	}
	return nil
}
