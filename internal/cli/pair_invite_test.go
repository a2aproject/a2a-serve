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
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func TestPrintPairCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		resp *controlapi.PairCodeResponse
		want string
	}{
		{
			name: "without a messenger",
			resp: &controlapi.PairCodeResponse{Code: "K7Q2M9XA", PairCommand: "a2a-serve pair join http://x K7Q2M9XA"},
			want: "→ code K7Q2M9XA (15 min, single use)\n  caller:    a2a-serve pair join http://x K7Q2M9XA\n",
		},
		{
			name: "with a messenger",
			resp: &controlapi.PairCodeResponse{Code: "K7Q2M9XA", PairCommand: "a2a-serve pair join http://x K7Q2M9XA", Messenger: true, Hints: []string{"telegram: https://t.me/bot?start=K7Q2M9XA"}},
			want: "→ code K7Q2M9XA (15 min, single use)\n  caller:    a2a-serve pair join http://x K7Q2M9XA\n  messenger: send \"join K7Q2M9XA\" to the agent's bot\n  telegram: https://t.me/bot?start=K7Q2M9XA\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			cfg := &globalConfig{Printer: output.NewPrinter(&buf, output.ModeText)}
			tc.resp.Expires = time.Now().Add(15*time.Minute + 10*time.Second)

			err := printPairCode(cfg, tc.resp)

			if err != nil {
				t.Fatalf("printPairCode() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, buf.String()); diff != "" {
				t.Fatalf("printPairCode() wrong output (-want +got) diff = %s", diff)
			}
		})
	}
}
