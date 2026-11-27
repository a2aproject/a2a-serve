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

package agentrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func userKey(i int) string { return fmt.Sprintf("user:%d", i) }

// testMax bounds the processes of the pools of these tests.
const testMax = 10

// startUsers starts the agent processes of n users and returns them.
func startUsers(t *testing.T, u *procs, n int) []io.Closer {
	t.Helper()
	closers := make([]io.Closer, n)
	for i := range n {
		a := u.get(userKey(i))
		release, err := u.reserve(a)
		if err != nil {
			t.Fatalf("procs.reserve(%d) error = %v", i, err)
		}
		closers[i] = &nopCloser{}
		u.started(a, closers[i], nil)
		release()
	}
	return closers
}

func TestProcsReserveRefusesStartOverLimit(t *testing.T) {
	t.Parallel()
	u := &procs{max: testMax}
	closers := startUsers(t, u, testMax-1)
	last := u.get(userKey(testMax - 1))
	release, err := u.reserve(last)
	if err != nil {
		t.Fatalf("procs.reserve() of the last free slot error = %v", err)
	}

	if _, err := u.reserve(u.get(userKey(testMax))); !errors.As(err, new(*AgentsBusyError)) {
		t.Fatalf("procs.reserve() while the last slot starts error = %v, want an AgentsBusyError", err)
	}
	if !u.hasRoom(userKey(testMax - 1)) {
		t.Fatalf("procs.hasRoom() of the starting process = false, want true")
	}
	release()
	u.exited(u.get(userKey(0)), closers[0])
	if _, err := u.reserve(u.get(userKey(testMax))); err != nil {
		t.Fatalf("procs.reserve() after a process exited error = %v", err)
	}
}

func TestHoldStartsKeepsKeysUntilTheirProcessesStop(t *testing.T) {
	t.Parallel()
	u := &procs{}
	startUsers(t, u, 1)
	releaseStart, err := u.reserve(u.get(userKey(1)))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	defer releaseStart()
	ending := u.get(userKey(2))
	ending.startMu.Lock()
	defer ending.startMu.Unlock()
	endTurn := u.turn(userKey(3))
	defer endTurn()
	u.get(userKey(4))

	keys, release := u.holdStarts()
	defer release()
	slices.Sort(keys)
	if diff := cmp.Diff([]string{userKey(0), userKey(1), userKey(2), userKey(3)}, keys); diff != "" {
		t.Fatalf("holdStarts() wrong keys (-want +got) diff = %s", diff)
	}
}

func TestProcsHasRoom(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		running int
		busy    int
		key     string
		want    bool
	}{
		{name: "free slot", running: testMax - 1, busy: testMax - 1, key: userKey(testMax), want: true},
		{name: "idle process to stop", running: testMax, busy: testMax - 1, key: userKey(testMax), want: true},
		{name: "all busy", running: testMax, busy: testMax, key: userKey(testMax)},
		{name: "own process runs", running: testMax, busy: testMax, key: userKey(0), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := &procs{max: testMax}
			startUsers(t, u, tc.running)
			for i := range tc.busy {
				u.turn(userKey(i))
			}
			if got := u.hasRoom(tc.key); got != tc.want {
				t.Fatalf("procs.hasRoom(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestProcsTurnEndFreesProcess(t *testing.T) {
	t.Parallel()
	u := &procs{max: testMax}
	startUsers(t, u, testMax)
	ends := make([]func(), testMax)
	for i := range testMax {
		ends[i] = u.turn(userKey(i))
	}
	ends[0]()
	ends[0]()
	if !u.hasRoom(userKey(testMax)) {
		t.Fatalf("procs.hasRoom() after a turn ended = false, want true")
	}
	u.turn(userKey(0))
	if u.hasRoom(userKey(testMax)) {
		t.Fatalf("procs.hasRoom() after a new turn = true, want false")
	}
}

// changedFrom reports whether a process started with another state than
// want.
func changedFrom(want string) func(any) bool {
	return func(state any) bool { return state != want }
}

type countingCloser struct{ closed int }

func (c *countingCloser) Close() error {
	c.closed++
	return nil
}

func TestProcsAdmit(t *testing.T) {
	t.Parallel()
	old, next := "shared", "read-only"
	tests := []struct {
		name       string
		busy       bool
		answer     bool
		want       string
		wantErr    error
		wantClosed int
	}{
		{name: "same sandbox", want: old},
		{name: "idle with another sandbox stops first", want: next, wantClosed: 1},
		{name: "busy with another sandbox refuses a turn", busy: true, want: next, wantErr: ErrStale},
		{name: "busy with another sandbox takes an answer", busy: true, answer: true, want: next},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := &procs{max: testMax}
			key := userKey(0)
			proc := &countingCloser{}
			u.started(u.get(key), proc, old)
			if tt.busy {
				defer u.turn(key)()
			}

			release, err := u.admit(key, changedFrom(tt.want), tt.answer)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("procs.admit() error = %v, want %v", err, tt.wantErr)
			}
			if proc.closed != tt.wantClosed {
				t.Fatalf("procs.admit() closed the process %d times, want %d", proc.closed, tt.wantClosed)
			}
			if err != nil {
				return
			}
			if stopped, err := u.closeStale(key, changedFrom("empty")); stopped || err != nil {
				t.Fatalf("procs.closeStale() during an admitted turn = %v, %v, want false, nil", stopped, err)
			}
			release()
		})
	}
}

func TestStopStopsTheProcessesUnderTheKey(t *testing.T) {
	t.Parallel()
	const parent, child, other = "user:a", "user:a#c", "user:b"
	r := &Runner{
		cfg: Config{Callers: fakeCallers{envs: map[string]Environment{
			parent: {Pool: "users"},
			child:  {Pool: "users", Process: &ProcessOptions{Parent: parent}},
			other:  {Pool: "users"},
		}}},
		pools: map[string]*procs{"users": {}},
	}
	p := r.pools["users"]
	closers := map[string]*countingCloser{}
	for _, key := range []string{parent, child, other} {
		a := p.get(key)
		p.setParent(a, map[string]string{child: parent}[key])
		closers[key] = &countingCloser{}
		p.started(a, closers[key], nil)
	}

	release, err := r.Stop(context.Background(), parent)
	if err != nil {
		t.Fatalf("Runner.Stop() error = %v", err)
	}
	if held := p.holdIdle(child); held != nil {
		held()
		t.Fatalf("Runner.Stop() left the start of the child free, want it held until release")
	}
	release()

	got := map[string]int{}
	for key, c := range closers {
		got[key] = c.closed
	}
	if diff := cmp.Diff(map[string]int{parent: 1, child: 1, other: 0}, got); diff != "" {
		t.Fatalf("Runner.Stop() closed processes wrong result (-want +got) diff = %s", diff)
	}
}

func TestLookupsOfUnknownKeysFail(t *testing.T) {
	t.Parallel()
	r := &Runner{cfg: Config{Callers: fakeCallers{}}, pools: map[string]*procs{"users": {}}}
	ctx := context.Background()

	if _, err := r.Stop(ctx, "user:gone"); err == nil {
		t.Fatalf("Runner.Stop() of a key Callers can't describe error = nil, want an error")
	}
	if _, err := r.HasRoom(ctx, "user:gone"); err == nil {
		t.Fatalf("Runner.HasRoom() of a key Callers can't describe error = nil, want an error")
	}
	if _, _, err := r.HoldIdle(ctx, "user:gone"); err == nil {
		t.Fatalf("Runner.HoldIdle() of a key Callers can't describe error = nil, want an error")
	}
	if _, ok := r.StartedWith("user:gone"); ok {
		t.Fatalf("Runner.StartedWith() of a key no pool knows = true, want false")
	}
}
