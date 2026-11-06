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
	"io"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/schedule"
)

// Manager changes what an agent keeps besides its settings: pairings,
// contacts, schedules and channels. A running agent applies the changes at
// once, and a stopped one when it starts.
type Manager interface {
	// Running reports whether the changes go to the running server.
	Running() bool
	// Pair issues a pairing code.
	Pair(ctx context.Context, req controlapi.PairCodeRequest) (*controlapi.PairCodeResponse, error)
	// Pairings lists the pairings.
	Pairings(ctx context.Context) ([]controlapi.Pairing, error)
	// Pairing describes a pairing.
	Pairing(ctx context.Context, name string) (*controlapi.Pairing, error)
	// UpdatePairing changes a pairing.
	UpdatePairing(ctx context.Context, name string, req controlapi.PairingUpdate) (*controlapi.PairingUpdateResponse, error)
	// Revoke removes a pairing, or revokes the credentials of the local one.
	Revoke(ctx context.Context, name string) (*controlapi.RevokeResponse, error)
	// PreviewRevoke describes what Revoke would remove, and removes nothing.
	PreviewRevoke(ctx context.Context, name string) (*controlapi.RemovalPreview, error)
	// SignOut revokes the Google sign-in tokens of an email.
	SignOut(ctx context.Context, email string) (*controlapi.SignOutResponse, error)
	// Contacts lists the contacts.
	Contacts(ctx context.Context) ([]controlapi.ContactInfo, error)
	// AddContact adds a contact.
	AddContact(ctx context.Context, req controlapi.ContactRequest) (*controlapi.ContactInfo, error)
	// RefreshContacts fetches the cards of contacts again.
	RefreshContacts(ctx context.Context, req controlapi.ContactRefreshRequest) ([]controlapi.ContactInfo, error)
	// DeleteContact removes a contact.
	DeleteContact(ctx context.Context, name string) error
	// Schedules lists the schedules.
	Schedules(ctx context.Context) ([]controlapi.ScheduleInfo, error)
	// AddSchedule adds a schedule made by the owner.
	AddSchedule(ctx context.Context, req schedule.Request) (*controlapi.ScheduleInfo, error)
	// DeleteSchedule removes a schedule.
	DeleteSchedule(ctx context.Context, name string) error
	// AddChannel checks the channel's credentials and saves it.
	AddChannel(ctx context.Context, req controlapi.ChannelRequest) (*controlapi.ChannelStatus, error)
	// DeleteChannel removes a channel.
	DeleteChannel(ctx context.Context, kind string) error
}

// Open returns the Manager of the agent in d. While the agent runs, it
// calls the server through client. Otherwise it changes the agent directory,
// which stays locked against a start until the closer is closed.
func Open(ctx context.Context, d agentdir.Dir, client *controlapi.Client) (Manager, io.Closer, error) {
	release, err := connect(ctx, d, client)
	if err != nil {
		return nil, nil, err
	}
	if release == nil {
		return online{client}, nopCloser{}, nil
	}
	o, err := openOffline(d, release)
	if err != nil {
		return nil, nil, errors.Join(err, release())
	}
	return o, o, nil
}

// lockWait is how long connect waits for the lock of the agent directory.
// Another command holds it for a moment, and the server for as long as it
// starts or stops.
const lockWait = 3 * time.Second

// connect finds out whether the agent in d runs. If it doesn't, connect
// locks the agent directory and returns the function that releases it;
// release is nil while the agent runs. It returns an
// [agenterr.BusyError] while the server starts or stops: then it holds
// the directory without serving its control socket. It waits for the lock
// for lockWait, or until the deadline of ctx if that comes first.
func connect(ctx context.Context, d agentdir.Dir, client *controlapi.Client) (release func() error, err error) {
	_, err = client.Status(ctx)
	if err == nil || !errors.Is(err, agenterr.ErrNotRunning) {
		return nil, err
	}
	if err := d.Init(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWait)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	busy := func(err error) error {
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return busyError(d)
		}
		return err
	}
	for {
		release, err = d.Lock()
		if !errors.Is(err, fsutil.ErrLocked) {
			return release, err
		}
		_, err = client.Status(ctx)
		if err == nil || !errors.Is(err, agenterr.ErrNotRunning) {
			return nil, busy(err)
		}
		if time.Now().After(deadline) {
			return nil, busyError(d)
		}
		select {
		case <-ctx.Done():
			return nil, busy(ctx.Err())
		case <-time.After(min(100*time.Millisecond, time.Until(deadline))):
		}
	}
}

// busyError names the agent in d by its config, which may not parse while
// the agent is being repaired.
func busyError(d agentdir.Dir) error {
	cfg, err := d.ReadConfig()
	if err != nil {
		slog.Debug("reading the agent's name", "error", err)
		return &agenterr.BusyError{Name: d.DefaultName()}
	}
	return &agenterr.BusyError{Name: cfg.Name}
}

// online manages a running agent through its control socket.
type online struct{ *controlapi.Client }

func (online) Running() bool { return true }

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
