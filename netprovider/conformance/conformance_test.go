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

package conformance

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var (
	provider = flag.String("provider", "", "provider binary to test (default: the loopnet reference provider)")
	network  = flag.String("url", "", "network URL to join (default: a loopnet network)")
	token    = flag.String("token", "", "network token to join with")
)

func TestConformance(t *testing.T) {
	cfg := Config{Binary: *provider, URL: *network, Token: *token}
	if cfg.Binary == "" {
		bin := filepath.Join(t.TempDir(), "loopnet")
		if out, err := exec.Command("go", "build", "-o", bin, "github.com/a2aproject/a2a-serve/netprovider/internal/loopnet").CombinedOutput(); err != nil {
			t.Fatalf("building loopnet: %v\n%s", err, out)
		}
		dir, err := os.MkdirTemp("", "loop")
		if err != nil {
			t.Fatalf("os.MkdirTemp() error = %v", err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(dir); err != nil {
				t.Errorf("os.RemoveAll() error = %v", err)
			}
		})
		cfg.Binary, cfg.URL = bin, "loopnet://"+dir
	}
	Run(t, cfg)
}
