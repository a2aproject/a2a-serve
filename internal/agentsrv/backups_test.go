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

package agentsrv

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/backup"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func TestServerTakesBackups(t *testing.T) {
	t.Parallel()
	to := t.TempDir()
	ts := startServer(t, serverOpts{
		secrets: agentdir.Secrets{"BACKUP_KEY": "k"},
		configure: func(c *agentdir.Config) {
			c.Backup = agentdir.BackupConfig{To: to, Key: "secret:BACKUP_KEY", Interval: "1h"}
		},
	})
	ctx := context.Background()
	// The snapshot's key comes from scrypt, which is slow under the race
	// detector on a busy machine.
	deadline := time.Now().Add(60 * time.Second)
	var st *controlapi.Status
	for {
		var err error
		st, err = controlapi.NewClient(ts.dir).Status(ctx)
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if st.Backup != nil && !st.Backup.Last.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Status().Backup = %+v, want a snapshot taken", st.Backup)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Backup.To != to || st.Backup.LastError != "" || time.Until(st.Backup.Next) < 59*time.Minute {
		t.Fatalf("Status().Backup = %+v, want the next snapshot in an hour", st.Backup)
	}
	dest, err := backup.Open(to, nil)
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := backup.List(ctx, dest, "coder")
	if err != nil || len(snaps) != 1 {
		t.Fatalf("List() = %v, %v, want one snapshot", snaps, err)
	}
}
