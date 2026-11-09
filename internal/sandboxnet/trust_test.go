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

package sandboxnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestTrustCA(t *testing.T) {
	t.Parallel()
	ca, err := NewCA([]string{"api.test"})
	if err != nil {
		t.Fatal(err)
	}
	pem := string(ca.PEM())
	const system, corp = "SYSTEM CERTS", "CORP CERT\n"
	root := t.TempDir()
	for path, data := range map[string]string{
		"/etc/ssl/certs/ca-certificates.crt": system,
		"/corp.pem":                          corp,
		"/run/.keep":                         "",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("corp.pem", filepath.Join(root, "corp-link.pem")); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/bin", "NODE_EXTRA_CA_CERTS=/corp-link.pem", EgressCAEnv(ca.PEM())}

	got, err := trustCA(root, env)
	if err != nil {
		t.Fatalf("trustCA() error = %v, want nil", err)
	}
	wantEnv := []string{"PATH=/bin", "NODE_EXTRA_CA_CERTS=" + ContainerCAFile, "SSL_CERT_FILE=" + ContainerCABundle, "REQUESTS_CA_BUNDLE=" + ContainerCABundle}
	if diff := cmp.Diff(wantEnv, got); diff != "" {
		t.Fatalf("trustCA() wrong environment (-want +got) diff = %s", diff)
	}
	for path, want := range map[string]string{
		ContainerCAFile:                      corp + pem,
		ContainerCABundle:                    system + "\n" + pem,
		"/etc/ssl/certs/ca-certificates.crt": system + "\n" + pem,
	} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q, want %q", path, data, want)
		}
	}
}

func TestTrustCAWithoutCA(t *testing.T) {
	t.Parallel()
	env := []string{"PATH=/bin"}
	got, err := trustCA(t.TempDir(), env)
	if err != nil {
		t.Fatalf("trustCA() error = %v, want nil", err)
	}
	if diff := cmp.Diff(env, got); diff != "" {
		t.Fatalf("trustCA() wrong environment (-want +got) diff = %s", diff)
	}
}

func TestTrustCARefusesGarbage(t *testing.T) {
	t.Parallel()
	got, err := trustCA(t.TempDir(), []string{"PATH=/bin", EnvEgressCA + "=bm90IGEgY2VydA=="})
	if err == nil || !strings.Contains(err.Error(), "holds no certificate") {
		t.Fatalf("trustCA() error = %v, want one about no certificate", err)
	}
	if diff := cmp.Diff([]string{"PATH=/bin"}, got); diff != "" {
		t.Fatalf("trustCA() wrong environment (-want +got) diff = %s", diff)
	}
}
