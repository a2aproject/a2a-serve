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

// Package netinstall finds and installs the network providers the server
// runs: the well-known providers that ship with a2a-serve or are pinned in
// it, and providers from other sources that the user trusted by their
// fingerprint. Providers live in the data directory, outside PATH.
package netinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/download"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/shellquote"
	"github.com/a2aproject/a2a-serve/netprovider"
	"github.com/a2aproject/a2a-serve/netprovider/host"
)

// Default is the provider of http(s) URLs without --network-provider.
const Default = "agentmesh"

// Archive is a release archive of a provider for one platform.
type Archive struct {
	URL    string
	SHA256 string
}

// Entry is a well-known provider.
type Entry struct {
	Name    string
	Version string
	// Binary is the name of the executable, without .exe.
	Binary string
	// Archives maps GOOS/GOARCH to the pinned release archive.
	Archives map[string]Archive
	// Source is the provider's directory in the a2a-serve repository, for
	// builds from source.
	Source string
	// Setup is what a build from source runs first in Source, if anything.
	Setup string
}

// WellKnown are the providers that ship with a2a-serve or are pinned in
// it. Their release archives are pinned here once a release publishes them.
var WellKnown = []Entry{
	{Name: "agentmesh", Version: "v0.2.0", Binary: "netprovider-agentmesh", Source: "providers/agentmesh", Setup: "export CGO_ENABLED=0"},
	{Name: "slim", Version: "v0.1.0", Binary: "netprovider-slim", Source: "providers/slim",
		Setup: "go run github.com/agntcy/slim-bindings-go/v3/cmd/slim-bindings-setup"},
}

// Lookup returns the well-known provider name.
func Lookup(name string) (Entry, bool) {
	i := slices.IndexFunc(WellKnown, func(e Entry) bool { return e.Name == name })
	if i < 0 {
		return Entry{}, false
	}
	return WellKnown[i], true
}

// IsWellKnown reports whether name is a well-known provider.
func IsWellKnown(name string) bool {
	_, ok := Lookup(name)
	return ok
}

// Names returns the names of the well-known providers.
func Names() []string {
	out := make([]string, 0, len(WellKnown))
	for _, e := range WellKnown {
		out = append(out, e.Name)
	}
	return out
}

// IsCustom reports whether spec, a --network-provider value, names a
// provider outside the well-known list: a release URL or a path.
func IsCustom(spec string) bool {
	return strings.HasPrefix(spec, "https://") || strings.HasPrefix(spec, "http://") || isPath(spec)
}

func isPath(spec string) bool {
	return filepath.IsAbs(spec) || strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(spec, "~/") ||
		strings.HasPrefix(spec, `.\`) || strings.HasPrefix(spec, `..\`)
}

var (
	namePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)
)

// Check checks a --network-provider value.
func Check(spec string) error {
	switch {
	case spec == "":
		return nil
	case strings.HasPrefix(spec, "http://"):
		return errors.New("a provider's release URL must use https")
	case IsCustom(spec):
		if strings.HasPrefix(spec, "https://") {
			u, err := url.Parse(spec)
			if err != nil || u.Host == "" || u.User != nil {
				return errors.New("want a release URL such as https://example.com/provider_linux_amd64.tar.gz")
			}
		}
		return nil
	}
	if _, ok := Lookup(spec); !ok {
		return fmt.Errorf("unknown network provider; want one of %s, a release URL or a path", strings.Join(Names(), ", "))
	}
	return nil
}

// DataDir returns the a2a-serve data directory: $XDG_DATA_HOME/a2a-serve
// or ~/.local/share/a2a-serve, ~/Library/Application Support/a2a-serve on
// macOS and %LocalAppData%\a2a-serve on Windows.
func DataDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		d := os.Getenv("LocalAppData")
		if d == "" {
			return "", errors.New("%LocalAppData% is not set")
		}
		return filepath.Join(d, "a2a-serve"), nil
	case "darwin":
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "a2a-serve"), nil
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "a2a-serve"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "a2a-serve"), nil
}

// Installer finds and installs providers.
type Installer struct {
	// Dir is where providers are installed: <data dir>/network-providers.
	Dir string
	// Executable is the running a2a-serve, next to which release archives
	// put the well-known providers. Link is the path it was started by,
	// when that is a symlink, such as Homebrew's bin/a2a-serve.
	Executable string
	Link       string
	HTTP       *http.Client
	// GOOS and GOARCH select release archives.
	GOOS, GOARCH string
	// Logf prints progress.
	Logf func(string, ...any)
	// Pins replace the archives of [WellKnown], for tests.
	Pins map[string]map[string]Archive
}

func (i *Installer) archive(e Entry) (Archive, bool) {
	archives := e.Archives
	if p, ok := i.Pins[e.Name]; ok {
		archives = p
	}
	a, ok := archives[i.GOOS+"/"+i.GOARCH]
	return a, ok
}

// New returns an installer for this machine.
func New(logf func(string, ...any)) (*Installer, error) {
	data, err := DataDir()
	if err != nil {
		return nil, fmt.Errorf("finding the data directory: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	link := ""
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != exe {
		link, exe = exe, resolved
	}
	return &Installer{
		Dir: filepath.Join(data, "network-providers"), Executable: exe, Link: link, HTTP: &http.Client{Timeout: 10 * time.Minute},
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Logf: logf,
	}, nil
}

func (i *Installer) logf(format string, args ...any) {
	if i.Logf != nil {
		i.Logf(format, args...)
	}
}

func (i *Installer) exe(name string) string {
	if i.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// Provider is a provider binary and the name the server gives it.
type Provider struct {
	Name string
	Path string
}

// ErrNotTrusted is returned for a custom provider nobody installed with
// this fingerprint.
var ErrNotTrusted = errors.New("the network provider isn't installed")

// Resolve returns the provider of spec without asking anything: a
// well-known provider next to a2a-serve, installed or downloaded from its
// pinned release, or a custom provider the user installed.
func (i *Installer) Resolve(ctx context.Context, spec string) (Provider, error) {
	if spec == "" {
		spec = Default
	}
	if IsCustom(spec) {
		rec, ok, err := i.trusted(spec)
		if err != nil {
			return Provider{}, err
		}
		if !ok {
			return Provider{}, fmt.Errorf("%w from %s; run `a2a-serve config set --network-provider %s` to install it", ErrNotTrusted, spec, spec)
		}
		if err := checkSum(rec.Path, rec.SHA256); err != nil {
			return Provider{}, fmt.Errorf("the installed network provider from %s changed: %w; run `a2a-serve config set --network-provider %s` to install it again", spec, err, spec)
		}
		return Provider{Name: rec.Name, Path: rec.Path}, nil
	}
	e, ok := Lookup(spec)
	if !ok {
		return Provider{}, Check(spec)
	}
	return i.wellKnown(ctx, e)
}

// Bundled returns where release archives put the well-known provider e
// next to a2a-serve: in its directory, or in ../libexec/a2a-serve, where
// Homebrew keeps it.
// A provider next to the symlink a2a-serve was started by comes first, so
// that one built next to `command -v a2a-serve` is found.
func (i *Installer) Bundled(e Entry) []string {
	var out []string
	if i.Link != "" {
		out = append(out, filepath.Join(filepath.Dir(i.Link), i.exe(e.Binary)))
	}
	dir := filepath.Dir(i.Executable)
	return append(out,
		filepath.Join(dir, i.exe(e.Binary)),
		filepath.Join(dir, "..", "libexec", "a2a-serve", i.exe(e.Binary)),
	)
}

// Installed returns where the pinned release of e is installed.
func (i *Installer) Installed(e Entry) string {
	return filepath.Join(i.Dir, e.Name, e.Version, i.exe(e.Binary))
}

// ErrDownload is returned by [Installer.Find] for a well-known provider
// that isn't on this machine and that the server downloads on start.
var ErrDownload = errors.New("the network provider is downloaded on the next start")

// Find returns the provider of spec as [Installer.Resolve] does, without
// downloading it: it returns [ErrDownload] for a pinned release that isn't
// installed yet.
func (i *Installer) Find(spec string) (Provider, error) {
	if spec == "" {
		spec = Default
	}
	e, ok := Lookup(spec)
	if !ok {
		return i.Resolve(context.Background(), spec)
	}
	if p, ok := i.local(e); ok {
		return p, nil
	}
	if _, ok := i.archive(e); ok {
		return Provider{Name: e.Name}, ErrDownload
	}
	return Provider{}, i.noRelease(e)
}

func (i *Installer) local(e Entry) (Provider, bool) {
	for _, p := range append(i.Bundled(e), i.Installed(e)) {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return Provider{Name: e.Name, Path: p}, true
		}
	}
	return Provider{}, false
}

func (i *Installer) noRelease(e Entry) error {
	build := "go build -o " + shellquote.Quote(i.Bundled(e)[0]) + " ."
	if e.Setup != "" {
		build = e.Setup + " && " + build
	}
	return fmt.Errorf("the network provider %s isn't installed next to a2a-serve, and this a2a-serve pins no release of it for %s/%s; build it there from a clone of github.com/a2aproject/a2a-serve: (cd %s && %s)",
		e.Name, i.GOOS, i.GOARCH, e.Source, build)
}

func (i *Installer) wellKnown(ctx context.Context, e Entry) (Provider, error) {
	if p, ok := i.local(e); ok {
		return p, nil
	}
	path := i.Installed(e)
	a, ok := i.archive(e)
	if !ok {
		return Provider{}, i.noRelease(e)
	}
	i.logf("… installing network provider %s %s (sha256 %s…)", e.Name, e.Version, a.SHA256[:min(len(a.SHA256), 12)])
	data, _, err := i.fetch(ctx, a.URL, a.SHA256, e.Binary)
	if err != nil {
		return Provider{}, fmt.Errorf("installing the network provider %s: %w", e.Name, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Provider{}, err
	}
	if err := fsutil.WriteAtomic(path, data, 0o755); err != nil {
		return Provider{}, err
	}
	return Provider{Name: e.Name, Path: path}, nil
}

// fetch returns the binary of a release archive or a bare binary at u, and
// the SHA-256 of what it downloaded.
func (i *Installer) fetch(ctx context.Context, u, want, binary string) ([]byte, string, error) {
	tmp, err := os.CreateTemp("", "a2a-netprovider-*")
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if err := errors.Join(tmp.Close(), os.Remove(tmp.Name())); err != nil && !errors.Is(err, os.ErrClosed) {
			i.logf("! removing %s: %v", tmp.Name(), err)
		}
	}()
	sum, err := download.ToFile(ctx, i.HTTP, u, tmp, want)
	if err != nil {
		return nil, sum, err
	}
	data, err := binaryOf(tmp, u, i.exe(binary))
	return data, sum, err
}

// binaryOf returns the binary in the archive f, or f itself when it isn't
// an archive. A custom archive has one executable, whatever its name.
func binaryOf(f *os.File, name, binary string) ([]byte, error) {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".zip") {
		if binary != "" {
			return download.Extract(f, lower, binary)
		}
		return download.ExtractOnly(f, lower)
	}
	return io.ReadAll(io.LimitReader(f, download.MaxBinary))
}

// record is a custom provider the user trusted.
type record struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Path   string `json:"path"`
}

func (i *Installer) trustFile() string { return filepath.Join(i.Dir, "trust.json") }

func (i *Installer) records() (map[string]record, error) {
	data, err := os.ReadFile(i.trustFile())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]record{}, nil
	}
	if err != nil {
		return nil, err
	}
	recs := map[string]record{}
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("reading %s: %w", i.trustFile(), err)
	}
	return recs, nil
}

func (i *Installer) trusted(spec string) (record, bool, error) {
	recs, err := i.records()
	if err != nil {
		return record{}, false, err
	}
	r, ok := recs[Canonical(spec)]
	return r, ok, nil
}

// Canonical returns spec with a path made absolute, as config.yaml keeps
// it, so that the server finds the provider from any directory.
func Canonical(spec string) string {
	if isPath(spec) {
		if strings.HasPrefix(spec, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				spec = filepath.Join(home, spec[2:])
			}
		}
		if abs, err := filepath.Abs(spec); err == nil {
			return abs
		}
	}
	return spec
}

func checkSum(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("SHA-256 %s, want %s", got, want)
	}
	return nil
}

// Candidate is a custom provider fetched for the user to confirm. Nothing
// runs it before [Installer.Trust].
type Candidate struct {
	Spec string
	// Source is where it came from, as the user should see it.
	Source string
	// SHA256 is the fingerprint of the binary.
	SHA256 string
	// Changed reports that the user trusted another binary from Spec.
	Changed bool
	// Trusted reports that the user already trusted this binary.
	Trusted bool

	data []byte
}

// Fetch gets the custom provider spec for the user to confirm with
// [Installer.Trust].
func (i *Installer) Fetch(ctx context.Context, spec string) (*Candidate, error) {
	c := &Candidate{Spec: Canonical(spec), Source: Canonical(spec)}
	var err error
	if isPath(spec) {
		c.data, err = os.ReadFile(c.Spec)
		if err == nil && (strings.HasSuffix(c.Spec, ".tar.gz") || strings.HasSuffix(c.Spec, ".tgz") || strings.HasSuffix(c.Spec, ".zip")) {
			var f *os.File
			if f, err = os.Open(c.Spec); err == nil {
				c.data, err = binaryOf(f, c.Spec, "")
				err = errors.Join(err, f.Close())
			}
		}
	} else {
		c.data, _, err = i.fetch(ctx, spec, "", "")
	}
	if err != nil {
		return nil, fmt.Errorf("getting the network provider from %s: %w", c.Source, err)
	}
	sum := sha256.Sum256(c.data)
	c.SHA256 = hex.EncodeToString(sum[:])
	rec, ok, err := i.trusted(spec)
	if err != nil {
		return nil, err
	}
	// A trusted provider whose installed copy changed installs again.
	c.Trusted = ok && rec.SHA256 == c.SHA256 && checkSum(rec.Path, rec.SHA256) == nil
	c.Changed = ok && rec.SHA256 != c.SHA256
	return c, nil
}

// describe runs the candidate from a private copy and asks it what it is.
func (i *Installer) describe(ctx context.Context, c *Candidate) (netprovider.Description, error) {
	dir, err := os.MkdirTemp("", "a2a-net-")
	if err != nil {
		return netprovider.Description{}, err
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			i.logf("! removing %s: %v", dir, err)
		}
	}()
	bin := filepath.Join(dir, i.exe("provider"))
	if err := os.WriteFile(bin, c.data, 0o700); err != nil {
		return netprovider.Description{}, err
	}
	p, err := host.Start(host.Config{Path: bin, RunDir: dir, Env: host.Environ(os.Environ())})
	if err != nil {
		return netprovider.Description{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := p.Describe(ctx)
	if err := errors.Join(err, p.Close()); err != nil {
		return netprovider.Description{}, fmt.Errorf("asking the network provider from %s what it is: %w", c.Source, err)
	}
	return d, nil
}

// Trust installs the candidate the user confirmed, asks it what it is, and
// records its name and fingerprint for its spec. A custom provider can't
// take the name of a well-known provider, of tailscale, or of another custom
// provider, whose grants and state it would get.
func (i *Installer) Trust(ctx context.Context, c *Candidate) (Provider, string, error) {
	d, err := i.describe(ctx, c)
	if err != nil {
		return Provider{}, "", err
	}
	if !versionPattern.MatchString(d.Version) {
		return Provider{}, "", fmt.Errorf("the network provider from %s reports the version %q; want letters, digits, '.', '_', '+' and '-'", c.Source, d.Version)
	}
	if !namePattern.MatchString(d.Name) || d.Name == "tailscale" || IsWellKnown(d.Name) {
		return Provider{}, "", fmt.Errorf("the network provider from %s calls itself %q; want lower-case letters, digits and dashes, and not tailscale or %s", c.Source, d.Name, strings.Join(Names(), ", "))
	}
	path := filepath.Join(i.Dir, "custom", c.SHA256[:16], i.exe(d.Name))
	err = fsutil.WithLock(filepath.Join(i.Dir, "trust.lock"), func() error {
		recs, err := i.records()
		if err != nil {
			return err
		}
		// The user confirmed this binary, so it replaces a provider of the
		// same name from another source, such as an older release, and
		// keeps its state and grants.
		for spec, r := range recs {
			if r.Name == d.Name && spec != c.Spec {
				delete(recs, spec)
				i.logf("! the network provider %s from %s replaces the one from %s", d.Name, c.Source, spec)
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := fsutil.WriteAtomic(path, c.data, 0o755); err != nil {
			return err
		}
		recs[c.Spec] = record{Name: d.Name, SHA256: c.SHA256, Path: path}
		data, err := json.MarshalIndent(recs, "", "  ")
		if err != nil {
			return err
		}
		return fsutil.WriteAtomic(i.trustFile(), data, 0o600)
	})
	if err != nil {
		return Provider{}, "", fmt.Errorf("installing the network provider from %s: %w", c.Source, err)
	}
	return Provider{Name: d.Name, Path: path}, d.Version, nil
}
