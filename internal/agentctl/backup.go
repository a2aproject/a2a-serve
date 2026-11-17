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

package agentctl

import (
	"context"
	"errors"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/backup"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// TakeBackup takes a snapshot of the agent in d with the backup settings
// of its config.yaml. A running agent that takes backups with the same
// settings takes it itself, so status reports it and the next scheduled
// snapshot moves. Otherwise, such as while the agent is stopped or runs an
// older server, the snapshot is taken here.
func TakeBackup(ctx context.Context, d agentdir.Dir, client *controlapi.Client) (backup.Snapshot, error) {
	cfg, err := d.LoadConfig()
	if err != nil {
		return backup.Snapshot{}, err
	}
	secrets, err := d.LoadSecrets()
	if err != nil {
		return backup.Snapshot{}, err
	}
	dest, bcfg, err := backup.ForAgent(cfg, secrets, nil)
	if err != nil {
		return backup.Snapshot{}, err
	}
	server, err := serverTakesBackups(ctx, client, cfg)
	if err != nil {
		return backup.Snapshot{}, err
	}
	if server {
		snap, err := client.TakeBackup(ctx)
		if !errors.Is(err, controlapi.ErrUnsupported) {
			return snap, err
		}
	}
	return backup.Take(ctx, d, dest, bcfg, time.Now())
}

// serverTakesBackups reports whether the agent runs and takes backups with
// the backup settings of cfg, those of config.yaml.
func serverTakesBackups(ctx context.Context, client *controlapi.Client, cfg *agentdir.Config) (bool, error) {
	st, err := client.Status(ctx)
	if errors.Is(err, agenterr.ErrNotRunning) {
		return false, nil
	}
	if err != nil || st.Backup == nil {
		return false, err
	}
	running, err := client.Config(ctx)
	if errors.Is(err, controlapi.ErrUnsupported) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return running.Backup == cfg.Backup, nil
}
