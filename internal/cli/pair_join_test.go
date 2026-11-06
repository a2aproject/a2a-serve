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
	"testing"

	"github.com/google/go-cmp/cmp"

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
