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
	"os"
	"path/filepath"
	"testing"
)

func TestForegroundHintSkippedOutsideTerminal(t *testing.T) {
	t.Parallel()
	logFile, err := os.Create(filepath.Join(t.TempDir(), "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := logFile.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	var buf bytes.Buffer

	printForegroundHint(&buf)
	printForegroundHint(logFile)

	info, err := logFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 || info.Size() != 0 {
		t.Fatalf("printForegroundHint() wrote %q and %d bytes to a log file, want nothing outside a terminal", buf.String(), info.Size())
	}
}
