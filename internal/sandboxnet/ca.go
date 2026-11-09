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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"sync"
	"time"
)

// Lifetimes of the certificates of a [CA].
const (
	caLifetime   = 5 * 365 * 24 * time.Hour
	leafLifetime = 30 * 24 * time.Hour
	// leafRenew is how long before its end a certificate is replaced.
	leafRenew = 24 * time.Hour
)

// CA signs the certificates with which the boundaries of one server answer
// the containers' TLS connections to the hosts that get headers. Its key
// lives in memory only: containers stop with the server, and each start
// makes a new CA. Its name constraints limit it to those hosts, so that
// the CA can't vouch for any other host, even to a container that trusts
// it.
type CA struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	pem   []byte
	hosts []string

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// NewCA returns a CA for hosts.
func NewCA(hosts []string) (*CA, error) {
	if len(hosts) == 0 {
		return nil, errors.New("a CA needs at least one host")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "a2a-serve egress CA"},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.Add(caLifetime),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         slices.Clone(hosts),
		ExcludedIPRanges:            []*net.IPNet{{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, {IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating the egress CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{
		cert: cert, key: key, hosts: slices.Clone(hosts),
		pem:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		leaves: map[string]*tls.Certificate{},
	}, nil
}

// PEM returns the certificate of the CA, which the containers trust.
func (c *CA) PEM() []byte { return slices.Clone(c.pem) }

// Covers reports whether the CA can sign the certificate of host.
func (c *CA) Covers(host string) bool { return slices.Contains(c.hosts, host) }

// leaf returns the certificate of host, signing a new one when there is
// none or it is about to end.
func (c *CA) leaf(host string) (*tls.Certificate, error) {
	if !c.Covers(host) {
		return nil, fmt.Errorf("the egress CA doesn't cover %s", host)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leaves[host]; ok && time.Until(l.Leaf.NotAfter) > leafRenew {
		return l, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	notAfter := now.Add(leafLifetime)
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("signing the certificate of %s: %w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	l := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key, Leaf: leaf}
	c.leaves[host] = l
	return l, nil
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}
