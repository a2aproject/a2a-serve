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

// Package storetest opens pre-migrated SQLite stores for tests so every test
// does not repeat schema migrations.
package storetest

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/store"
)

var migratedDB = sync.OnceValues(buildMigratedDB)

func buildMigratedDB() (_ []byte, err error) {
	dir, err := os.MkdirTemp("", "a2a-store-seed-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(dir))
	}()
	path := filepath.Join(dir, "server.db")
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	if err := st.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Open opens a migrated on-disk store in t.TempDir() and closes it when the
// test finishes.
func Open(t testing.TB) *store.Store {
	t.Helper()
	data, err := migratedDB()
	if err != nil {
		t.Fatalf("migrating the template database: %v", err)
	}
	path := filepath.Join(t.TempDir(), "server.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing the template database: %v", err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return st
}
