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
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/egress"
)

const injectHost = "api.test"

// seen is what the upstream of the injection tests got.
type seen struct {
	Proto, Method, Path, Query, Host string
	Headers                          map[string]string
}

// injectUpstream serves TLS for injectHost with a certificate that roots
// trust, and answers each request with what it saw. It answers /upload
// before it reads the request body.
func injectUpstream(t *testing.T, configure ...func(*http.Server)) (port uint16, roots *x509.CertPool) {
	t.Helper()
	ca, err := NewCA([]string{injectHost})
	if err != nil {
		t.Fatalf("NewCA() error = %v", err)
	}
	cert, err := ca.leaf(injectHost)
	if err != nil {
		t.Fatalf("CA.leaf() error = %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			w.WriteHeader(http.StatusOK)
			// The flush can report the stream closed after it sent the
			// headers, when the boundary closes right then; the client then
			// has its answer.
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/upload" {
			w.WriteHeader(http.StatusOK)
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flushing the headers error = %v", err)
				return
			}
			if _, err := io.ReadAll(r.Body); err != nil {
				return
			}
		}
		got := seen{Proto: r.Proto, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Host: r.Host, Headers: map[string]string{}}
		for _, h := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "X-Other"} {
			if v := r.Header.Get(h); v != "" {
				got.Headers[h] = v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(got); err != nil {
			t.Errorf("encoding the answer error = %v", err)
		}
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	for _, c := range configure {
		c(srv.Config)
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ap, err := net.ResolveTCPAddr("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.PEM())
	return uint16(ap.Port), roots
}

// injectBoundary serves a boundary with inj, and returns a client that
// reaches it as a container does, trusting its CA.
func injectBoundary(t *testing.T, inj Injection, roots *x509.CertPool, logs *logRecords, h2 bool) (*Boundary, *http.Client) {
	t.Helper()
	ca, err := NewCA([]string{injectHost})
	if err != nil {
		t.Fatalf("NewCA() error = %v", err)
	}
	b, sock := listen(t, BoundaryConfig{
		Policy:        func() egress.Policy { return mustPolicy(t, "", injectHost, "127.0.0.1") },
		Resolver:      fakeResolver{injectHost: addrs("127.0.0.1")},
		Log:           slog.New(logs),
		Inject:        []Injection{inj},
		CA:            ca,
		UpstreamRoots: roots,
	})
	trusted := x509.NewCertPool()
	trusted.AppendCertsFromPEM(ca.PEM())
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialTunnel(ctx, sock, addr)
		},
		TLSClientConfig:   &tls.Config{RootCAs: trusted},
		ForceAttemptHTTP2: h2,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return b, &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// dialTunnel opens a CONNECT tunnel to addr on the boundary socket.
func dialTunnel(ctx context.Context, sock, addr string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr); err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CONNECT %s status = %d", addr, resp.StatusCode)
	}
	return c, nil
}

// send sends a request with client and returns the status, the protocol
// of the client's connection and what the upstream got.
func send(t *testing.T, client *http.Client, method, url string, header http.Header) (int, string, seen) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Client.Do(%s %s) error = %v", method, url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the body error = %v", err)
		}
	}()
	var got seen
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decoding the answer error = %v", err)
		}
	}
	return resp.StatusCode, resp.Proto, got
}

func TestBoundaryInjectsHeader(t *testing.T) {
	t.Parallel()
	for _, h2 := range []bool{false, true} {
		t.Run("h2="+strconv.FormatBool(h2), func(t *testing.T) {
			t.Parallel()
			port, roots := injectUpstream(t)
			logs := &logRecords{}
			inj := Injection{Host: injectHost, Port: port, Header: "Authorization", Value: "Bearer ghp_real", Secrets: []string{"GH"}}
			_, client := injectBoundary(t, inj, roots, logs, h2)
			base := "https://" + net.JoinHostPort(injectHost, strconv.Itoa(int(port)))

			status, proto, got := send(t, client, http.MethodPost, base+"/repos/x?page=2", http.Header{"Authorization": {"Bearer fake"}, "X-Other": {"kept"}})
			if status != http.StatusOK {
				t.Fatalf("POST status = %d, want 200", status)
			}
			wantProto := "HTTP/1.1"
			if h2 {
				wantProto = "HTTP/2.0"
			}
			if proto != wantProto {
				t.Fatalf("the sandbox's connection speaks %s, want %s", proto, wantProto)
			}
			want := seen{Proto: "HTTP/2.0", Method: http.MethodPost, Path: "/repos/x", Query: "page=2", Host: net.JoinHostPort(injectHost, strconv.Itoa(int(port))), Headers: map[string]string{"Authorization": "Bearer ghp_real", "X-Other": "kept"}}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("upstream got wrong request (-want +got) diff = %s", diff)
			}
			for _, rec := range logs.all() {
				if strings.Contains(rec, "ghp_real") || strings.Contains(rec, "/repos") || strings.Contains(rec, "page=2") {
					t.Fatalf("log record %q has the secret or the URL", rec)
				}
			}
		})
	}
}

func TestBoundaryInjectsModelKey(t *testing.T) {
	t.Parallel()
	port, roots := injectUpstream(t)
	p, _ := egress.LookupProvider("gemini")
	placeholder := p.Placeholder("GEMINI_API_KEY")
	inj := Injection{
		Host: injectHost, Port: port, Header: "X-Goog-Api-Key", Value: "AIza_real",
		Model: true, QueryKey: "key", Endpoints: p.Endpoints,
	}
	_, client := injectBoundary(t, inj, roots, &logRecords{}, false)
	base := "https://" + net.JoinHostPort(injectHost, strconv.Itoa(int(port)))
	tests := []struct {
		name        string
		method      string
		path        string
		header      http.Header
		wantQuery   string
		wantHeaders map[string]string
	}{
		{
			name: "placeholder in its header", method: http.MethodPost, path: "/v1beta/models/g:streamGenerateContent?alt=sse",
			header: http.Header{"X-Goog-Api-Key": {placeholder}}, wantQuery: "alt=sse",
			wantHeaders: map[string]string{"X-Goog-Api-Key": "AIza_real"},
		},
		{
			name: "placeholder in another credential header", method: http.MethodPost, path: "/v1beta/models/g:generateContent",
			header:      http.Header{"Authorization": {"Bearer " + placeholder}, "X-Api-Key": {"agent-own"}},
			wantHeaders: map[string]string{"X-Goog-Api-Key": "AIza_real"},
		},
		{
			name: "placeholder in the query", method: http.MethodPost, path: "/v1beta/models/g:generateContent?alt=sse&key=" + placeholder,
			wantQuery: "alt=sse", wantHeaders: map[string]string{"X-Goog-Api-Key": "AIza_real"},
		},
		{
			name: "another endpoint", method: http.MethodGet, path: "/v1beta/files?key=" + placeholder,
			header:      http.Header{"X-Goog-Api-Key": {placeholder}, "X-Other": {"kept"}},
			wantHeaders: map[string]string{"X-Other": "kept"},
		},
		{
			name: "a path that leaves the endpoint", method: http.MethodPost, path: "/v1beta/models/..%2Ffiles:generateContent",
			header: http.Header{"X-Goog-Api-Key": {placeholder}}, wantHeaders: map[string]string{},
		},
		{
			name: "a path with a backslash", method: http.MethodPost, path: "/v1beta/models/..%5Cfiles:generateContent",
			header: http.Header{"X-Goog-Api-Key": {placeholder}}, wantHeaders: map[string]string{},
		},
		{
			name: "a placeholder inside other text in another header", method: http.MethodPost, path: "/v1beta/models/g:generateContent",
			header:      http.Header{"Authorization": {"Bearer x" + placeholder}},
			wantHeaders: map[string]string{"X-Goog-Api-Key": "AIza_real"},
		},
		{
			name: "a repeated placeholder", method: http.MethodPost, path: "/v1beta/models/g:generateContent",
			header:      http.Header{"X-Goog-Api-Key": {placeholder, placeholder}, "Authorization": {"Bearer " + placeholder}},
			wantHeaders: map[string]string{"X-Goog-Api-Key": "AIza_real"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, _, got := send(t, client, tc.method, base+tc.path, tc.header)
			if status != http.StatusOK {
				t.Fatalf("%s %s status = %d, want 200", tc.method, tc.path, status)
			}
			if got.Query != tc.wantQuery {
				t.Fatalf("upstream got the query %q, want %q", got.Query, tc.wantQuery)
			}
			if diff := cmp.Diff(tc.wantHeaders, got.Headers); diff != "" {
				t.Fatalf("upstream got wrong headers (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestBoundaryForwardsAnAnswerThatComesBeforeTheBody(t *testing.T) {
	t.Parallel()
	port, roots := injectUpstream(t)
	inj := Injection{Host: injectHost, Port: port, Header: "Authorization", Value: "Bearer real"}
	_, client := injectBoundary(t, inj, roots, &logRecords{}, false)
	url := "https://" + net.JoinHostPort(injectHost, strconv.Itoa(int(port))) + "/upload"

	resp, err := client.Post(url, "application/json", &lateReader{parts: []string{"{", " ", " ", "}"}, delay: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Client.Post() error = %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the body error = %v", err)
		}
	}()
	var got seen
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding the answer error = %v", err)
	}
	if got.Path != "/upload" {
		t.Fatalf("upstream got the path %q, want %q", got.Path, "/upload")
	}
}

func TestBoundaryKeepsTheConnectionAfterAnAnswerThatComesBeforeTheBody(t *testing.T) {
	t.Parallel()
	// The host answers without reading the body and opens a window of the
	// initial size only, so the boundary forwards a part of the body and
	// leaves the rest unread when the answer ends.
	port, roots := injectUpstream(t, func(s *http.Server) {
		s.HTTP2 = &http.HTTP2Config{MaxReceiveBufferPerStream: 64<<10 - 1}
	})
	inj := Injection{Host: injectHost, Port: port, Header: "Authorization", Value: "Bearer real"}
	_, client := injectBoundary(t, inj, roots, &logRecords{}, false)
	tr := client.Transport.(*http.Transport)
	authority := net.JoinHostPort(injectHost, strconv.Itoa(int(port)))
	raw, err := tr.DialContext(t.Context(), "tcp", authority)
	if err != nil {
		t.Fatalf("dialing the tunnel error = %v", err)
	}
	conn := tls.Client(raw, &tls.Config{RootCAs: tr.TLSClientConfig.RootCAs, ServerName: injectHost, NextProtos: []string{"http/1.1"}})
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("closing the connection error = %v", err)
		}
	})
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	request := func(body string) string {
		return "POST /repos/x HTTP/1.1\r\nHost: " + authority + "\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	}
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(conn, request(strings.Repeat(" ", 200<<10)))
		written <- err
	}()
	if status, err := readAnswer(br); err != nil || status != http.StatusOK {
		t.Fatalf("first answer status = %d, error = %v, want 200", status, err)
	}
	if err := <-written; err != nil {
		t.Fatalf("writing the first request error = %v", err)
	}

	if _, err := io.WriteString(conn, request("{}")); err != nil {
		t.Fatalf("writing the second request error = %v", err)
	}
	status, err := readAnswer(br)

	if err != nil {
		t.Fatalf("reading the second answer on the kept connection error = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("second answer status = %d, want 200", status)
	}
}

// readAnswer reads an answer to a POST and its body from br.
func readAnswer(br *bufio.Reader) (int, error) {
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return 0, err
	}
	return resp.StatusCode, resp.Body.Close()
}

// lateReader returns its first part at once, and each next part and its
// end after delay.
type lateReader struct {
	parts []string
	delay time.Duration
	read  bool
}

func (r *lateReader) Read(p []byte) (int, error) {
	if r.read {
		time.Sleep(r.delay)
	}
	r.read = true
	if len(r.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.parts[0])
	r.parts = r.parts[1:]
	return n, nil
}

func TestBoundaryInjectRefusesOtherNames(t *testing.T) {
	t.Parallel()
	port, roots := injectUpstream(t)
	inj := Injection{Host: injectHost, Port: port, Header: "Authorization", Value: "Bearer real"}
	_, client := injectBoundary(t, inj, roots, &logRecords{}, false)
	authority := net.JoinHostPort(injectHost, strconv.Itoa(int(port)))

	req, err := http.NewRequest(http.MethodGet, "https://"+authority+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "other.test"
	tr := client.Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = injectHost
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("Client.Do() error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("a request for another host status = %d, want %d", resp.StatusCode, http.StatusMisdirectedRequest)
	}

	tr = client.Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "other.test"
	tr.TLSClientConfig.InsecureSkipVerify = true
	if _, err := (&http.Client{Transport: tr}).Get("https://" + authority + "/"); err == nil {
		t.Fatalf("a TLS connection that names another server error = nil, want a failed handshake")
	}
}

func TestCAConstrainsNames(t *testing.T) {
	t.Parallel()
	ca, err := NewCA([]string{injectHost})
	if err != nil {
		t.Fatalf("NewCA() error = %v", err)
	}
	if _, err := ca.leaf("other.test"); err == nil {
		t.Fatalf("CA.leaf(%q) error = nil, want an error", "other.test")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.PEM())
	leaf, err := ca.leaf(injectHost)
	if err != nil {
		t.Fatalf("CA.leaf() error = %v", err)
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: injectHost, Roots: roots}); err != nil {
		t.Fatalf("Verify(%s) error = %v, want nil", injectHost, err)
	}
	// A certificate that the key signs for another host still fails, as a
	// leaked key would.
	serial, err := randomSerial()
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: serial, DNSNames: []string{"bank.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, ca.key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	other, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Verify(x509.VerifyOptions{DNSName: "bank.example", Roots: roots}); err == nil {
		t.Fatalf("Verify(bank.example) error = nil, want a name constraint error")
	}
}

func TestNewRefusesInjectionWithoutCA(t *testing.T) {
	t.Parallel()
	ca, err := NewCA([]string{"a.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*CA{nil, ca} {
		_, err := New(BoundaryConfig{Policy: func() egress.Policy { return egress.Policy{} }, Inject: []Injection{{Host: injectHost, Port: 443}}, CA: c})
		if err == nil {
			t.Fatalf("New() with a CA that doesn't cover %s error = nil, want an error", injectHost)
		}
	}
}

func TestBoundaryCloseEndsInjectedRequests(t *testing.T) {
	t.Parallel()
	port, roots := injectUpstream(t)
	inj := Injection{Host: injectHost, Port: port, Header: "Authorization", Value: "Bearer real"}
	b, client := injectBoundary(t, inj, roots, &logRecords{}, true)
	resp, err := client.Get("https://" + net.JoinHostPort(injectHost, strconv.Itoa(int(port))) + "/stream")
	if err != nil {
		t.Fatalf("Client.Get() error = %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing the body: %v", err)
		}
	}()
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Boundary.Close() error = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Boundary.Close() didn't return while a request streamed")
	}
}
