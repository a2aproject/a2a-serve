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
	"bytes"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func TestPrintJoinedAs(t *testing.T) {
	t.Parallel()
	c := credstore.Credential{URL: "http://x", Agent: "coder", Principal: "anna"}
	tests := []struct {
		name        string
		helperSetup bool
		want        string
	}{
		{name: "helper configured", want: "✓ joined coder as anna; token saved to /creds.json\n"},
		{name: "helper missing", helperSetup: true, want: "✓ joined coder as anna; token saved to /creds.json\n  (" + credstore.HelperSetup + ")\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			cfg := &globalConfig{Printer: output.NewPrinter(&buf, output.ModeText)}

			err := printJoinedAs(cfg, c, "/creds.json", tc.helperSetup)

			if err != nil {
				t.Fatalf("printJoinedAs() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, buf.String()); diff != "" {
				t.Fatalf("printJoinedAs() wrong output (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestCheckNotJoinedNamesWhatReplaceKeeps(t *testing.T) {
	t.Parallel()
	store := credstore.At(t.TempDir())
	if err := store.Put(credstore.Credential{URL: "http://x", Token: "t", Agent: "coder", Principal: "anna"}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	err := checkNotJoined(store, "http://x")

	var ce *clierr.Error
	if !errors.As(err, &ce) {
		t.Fatalf("checkNotJoined() error = %v, want a usage error", err)
	}
	want := "you already joined coder as anna; joining again replaces that token in " + store.Path + ", and with a code for another pairing you lose access to the conversations and tasks of anna"
	if ce.Message != want {
		t.Fatalf("checkNotJoined() message = %q, want %q", ce.Message, want)
	}
	wantHint := "pass --replace to join anyway; a code from `a2a-serve pair set --rotate` keeps the conversations and tasks, and a code for a webhook node replaces nothing"
	if ce.Hint != wantHint {
		t.Fatalf("checkNotJoined() hint = %q, want %q", ce.Hint, wantHint)
	}
}
