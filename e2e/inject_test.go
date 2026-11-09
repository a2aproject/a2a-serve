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

//go:build e2e

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
)

// newTLSStubModel starts a stubModel that serves HTTPS on this host's
// loopback for host.docker.internal, which the server reaches for the
// container, and that answers only requests with auth. The server of u
// trusts its certificate.
func newTLSStubModel(t *testing.T, u *user, script modelScript, auth string) *stubModel {
	t.Helper()
	cert, caPEM := stubCertificate(t, "host.docker.internal")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	m := &stubModel{t: t, script: script, auth: auth}
	m.srv = &httptest.Server{Listener: l, Config: &http.Server{Handler: m}, TLS: &tls.Config{Certificates: []tls.Certificate{cert}}}
	m.srv.StartTLS()
	t.Cleanup(m.srv.Close)
	m.url = "https://host.docker.internal:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port) + "/v1"

	bundle := filepath.Join(u.home, "ca-bundle.pem")
	system, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
	if err != nil {
		t.Logf("no system CA bundle: %v", err)
	}
	writeFile(t, bundle, string(system)+"\n"+string(caPEM))
	u.env = append(u.env, "SSL_CERT_FILE="+bundle)
	return m
}

// stubCertificate returns a certificate for host and the PEM of the CA
// that signed it.
func stubCertificate(t *testing.T, host string) (tls.Certificate, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "e2e stub CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{host}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func TestHarnessOpencodeInjectHeader(t *testing.T) {
	t.Parallel()
	requireHarness(t, "opencode")
	const key = "stub-key-from-the-server"
	owner := newUser(t)
	owner.env = append(owner.env, "STUB_KEY="+key)
	model := newTLSStubModel(t, owner, modelScript{
		calls:  []toolCall{bash("echo key=${STUB_KEY:-unset} ca=$NODE_EXTRA_CA_CERTS")},
		answer: "Done.",
	}, "Bearer "+key)
	coder := owner.newAgent("coder")
	writeJSON(t, filepath.Join(coder.dir, "opencode.json"), map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{"stub": map[string]any{
			"npm":     "@ai-sdk/openai-compatible",
			"options": map[string]any{"baseURL": model.url, "apiKey": "{env:STUB_KEY}"},
			"models":  map[string]any{"scripted": map[string]any{}},
		}},
	})
	target := strings.TrimSuffix(strings.TrimPrefix(model.url, "https://"), "/v1")
	coder.start("opencode", "--sandbox", "docker", "--image", harnessImage, "--model", "stub/scripted", "--allow-tools", "execute",
		"--inject-header", target+"=Authorization: Bearer $STUB_KEY")

	task := owner.mustSend(coder.url, "Use the tools.")

	if diff := cmp.Diff(taskResult{State: a2a.TaskStateCompleted, Text: "Done."}, resultOf(task)); diff != "" {
		t.Fatalf("a2a send wrong result (-want +got) diff = %s", diff)
	}
	if got, want := model.toolResults()["bash"], "key=a2a-serve-placeholder ca=/run/a2a-egress-ca.pem"; !strings.Contains(got, want) {
		t.Fatalf("the agent's environment = %q, want it to contain %q", got, want)
	}
	for _, got := range model.authorizations() {
		if got != "Bearer "+key {
			t.Fatalf("the model got Authorization %q, want the key from the server", got)
		}
	}
	if out, want := coder.serve("status"), "inject   "+target+" gets STUB_KEY"; !strings.Contains(out, want) {
		t.Fatalf("a2a-serve status = %q, want it to contain %q", out, want)
	}
}
