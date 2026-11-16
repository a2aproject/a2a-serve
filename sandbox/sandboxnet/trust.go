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
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// EnvEgressCA passes the certificate of the egress [CA], as base64 of its
// PEM, to the launcher, which makes the agent trust it.
const EnvEgressCA = "A2A_EGRESS_CA"

// Files of the egress CA in the container: the CA alone, which
// NODE_EXTRA_CA_CERTS names, and a bundle with the image's CAs, which
// SSL_CERT_FILE and REQUESTS_CA_BUNDLE name.
const (
	ContainerCAFile   = "/run/a2a-egress-ca.pem"
	ContainerCABundle = "/run/a2a-egress-ca-bundle.pem"
)

// systemBundles are where images keep their CA bundle: Debian and Ubuntu,
// Fedora and RHEL, and Alpine.
var systemBundles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem"}

// EgressCAEnv returns the variable that passes the PEM certificate cert to
// the launcher.
func EgressCAEnv(cert []byte) string {
	return EnvEgressCA + "=" + base64.StdEncoding.EncodeToString(cert)
}

// trustCA makes the agent trust the egress CA that env passes, in the file
// system at root, and returns the agent's environment. Node and Bun read
// NODE_EXTRA_CA_CERTS; Go, OpenSSL and Python read SSL_CERT_FILE or
// REQUESTS_CA_BUNDLE; tools such as git read the system bundle, which gets
// the CA appended. A certificate that the variables named before stays
// trusted. The returned environment never has [EnvEgressCA], even with an
// error. Without the CA, the agent's connections to the hosts that get
// headers fail, and others work.
func trustCA(root string, env []string) ([]string, error) {
	encoded, found := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, EnvEgressCA+"="); ok {
			encoded, found = v, true
		}
	}
	if !found {
		return env, nil
	}
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, EnvEgressCA+"=") })
	ca, err := decodeCA(encoded)
	if err != nil {
		return env, err
	}
	at := func(p string) string { return filepath.Join(root, p) }
	var errs []error
	extra := withCA(readEnvFile(root, env, "NODE_EXTRA_CA_CERTS"), ca)
	if err := os.WriteFile(at(ContainerCAFile), extra, 0o644); err != nil {
		return env, fmt.Errorf("writing %s: %w", ContainerCAFile, err)
	}
	env = setEnv(env, "NODE_EXTRA_CA_CERTS", ContainerCAFile)
	system := ""
	for _, p := range systemBundles {
		if _, err := os.Stat(at(p)); err == nil {
			system = p
			break
		}
	}
	base := readEnvFile(root, env, "SSL_CERT_FILE")
	if base == nil && system != "" {
		var err error
		if base, err = os.ReadFile(at(system)); err != nil {
			errs = append(errs, fmt.Errorf("reading %s: %w", system, err))
		}
	}
	if requests := readEnvFile(root, env, "REQUESTS_CA_BUNDLE"); requests != nil && !bytes.Equal(requests, base) {
		base = append(withNewline(base), requests...)
	}
	if err := os.WriteFile(at(ContainerCABundle), withCA(base, ca), 0o644); err != nil {
		return env, errors.Join(append(errs, fmt.Errorf("writing %s: %w", ContainerCABundle, err))...)
	}
	env = setEnv(setEnv(env, "SSL_CERT_FILE", ContainerCABundle), "REQUESTS_CA_BUNDLE", ContainerCABundle)
	if system != "" {
		if err := appendFile(at(system), ca); err != nil {
			errs = append(errs, fmt.Errorf("adding the egress CA to %s: %w", system, err))
		}
	}
	return env, errors.Join(errs...)
}

// decodeCA returns the PEM of the certificate that encoded holds.
func decodeCA(encoded string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", EnvEgressCA, err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s holds no certificate", EnvEgressCA)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", EnvEgressCA, err)
	}
	return pem.EncodeToMemory(block), nil
}

// maxBundle bounds a CA file that a variable names.
const maxBundle = 8 << 20

// readEnvFile returns the file that the variable name of env names, or nil.
// The image or the owner sets the variable, which may name a link, as
// update-ca-certificates makes them; the launcher reads only a regular
// file of at most maxBundle bytes, so that a device or a pipe can't stall
// it.
func readEnvFile(root string, env []string, name string) []byte {
	for _, kv := range slices.Backward(env) {
		v, ok := strings.CutPrefix(kv, name+"=")
		if !ok || v == "" {
			continue
		}
		path := filepath.Join(root, v)
		if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() || fi.Size() > maxBundle {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		return data
	}
	return nil
}

func withCA(certs, ca []byte) []byte { return append(withNewline(slices.Clone(certs)), ca...) }

// withNewline returns b ending in a line break, unless it is empty.
func withNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] != '\n' {
		return append(b, '\n')
	}
	return b
}

func setEnv(env []string, name, value string) []string {
	env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, name+"=") })
	return append(env, name+"="+value)
}

// appendFile appends ca to the file at path, after a line break when the
// file doesn't end in one.
func appendFile(path string, ca []byte) (err error) {
	cur, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if len(cur) > 0 && cur[len(cur)-1] != '\n' {
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	_, err = f.Write(ca)
	return err
}
