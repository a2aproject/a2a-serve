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

// Package download downloads release archives, checks their SHA-256 and
// extracts binaries from them.
package download

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// MaxArchive bounds a downloaded archive.
const MaxArchive = 512 << 20

// MaxBinary bounds an extracted binary.
const MaxBinary = 512 << 20

// ToFile downloads url into dst, and returns the SHA-256 of what it wrote.
// With want set, a download with another SHA-256 is an error. dst is left
// at its start.
func ToFile(ctx context.Context, hc *http.Client, url string, dst *os.File, want string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing the download", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, MaxArchive+1))
	if err != nil {
		return "", err
	}
	if n > MaxArchive {
		return "", fmt.Errorf("%s is larger than %d MB", url, MaxArchive>>20)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if want != "" && got != want {
		return got, fmt.Errorf("the download of %s has SHA-256 %s, want %s; not running it", url, got, want)
	}
	_, err = dst.Seek(0, io.SeekStart)
	return got, err
}

// Extract returns the regular file binary from the archive, a .zip or a
// .tar.gz as archiveName says.
func Extract(archive *os.File, archiveName, binary string) ([]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		fi, err := archive.Stat()
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(archive, fi.Size())
		if err != nil {
			return nil, err
		}
		for _, zf := range zr.File {
			if zf.Name != binary {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(io.LimitReader(rc, MaxBinary))
			return data, errors.Join(err, rc.Close())
		}
		return nil, fmt.Errorf("no %s in the archive", binary)
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no %s in the archive", binary)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == binary && hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, MaxBinary))
		}
	}
}

// ExtractOnly returns the only regular file in the archive.
func ExtractOnly(archive *os.File, archiveName string) ([]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		fi, err := archive.Stat()
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(archive, fi.Size())
		if err != nil {
			return nil, err
		}
		var only *zip.File
		for _, zf := range zr.File {
			if !zf.Mode().IsRegular() {
				continue
			}
			if only != nil {
				return nil, errors.New("the archive has more than one file; want one executable")
			}
			only = zf
		}
		if only == nil {
			return nil, errors.New("the archive has no file")
		}
		rc, err := only.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, MaxBinary))
		return data, errors.Join(err, rc.Close())
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var data []byte
	found := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Mode&0o111 == 0 {
			continue
		}
		if found {
			return nil, errors.New("the archive has more than one executable; want one")
		}
		if data, err = io.ReadAll(io.LimitReader(tr, MaxBinary)); err != nil {
			return nil, err
		}
		found = true
	}
	if !found {
		return nil, errors.New("the archive has no executable")
	}
	return data, nil
}
