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
	"errors"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/auth"
)

// The budget of anonymous callers on each listener.
const (
	anonymousTasksPerHour    = 20
	anonymousConcurrentTasks = 2
)

var (
	errAnonymousBusy = errors.New("this agent is busy with other anonymous callers; try again in a few minutes")
	errAnonymousRate = errors.New("this agent took all the anonymous tasks it takes in an hour; try again later")
)

// anonymousBudget limits the new and the running tasks of anonymous
// callers on each listener.
type anonymousBudget struct {
	perHour    int
	concurrent int
	now        func() time.Time

	mu        sync.Mutex
	listeners map[string]*listenerBudget
}

type listenerBudget struct {
	// starts are the times of the tasks started in the last hour.
	starts  []time.Time
	running int
}

func newAnonymousBudget() *anonymousBudget {
	return &anonymousBudget{perHour: anonymousTasksPerHour, concurrent: anonymousConcurrentTasks, now: time.Now, listeners: map[string]*listenerBudget{}}
}

// budgetListener is where an anonymous caller arrived: a listener, or a
// messenger.
func budgetListener(p auth.Principal) string {
	if p.Network == auth.NetworkChat {
		return auth.NetworkChat + ":" + p.Provider
	}
	return p.Network
}

// admit counts a new task on a listener, or refuses it when the listener
// used up its budget.
func (b *anonymousBudget) admit(listener string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.listener(listener)
	switch {
	case l.running >= b.concurrent:
		return errAnonymousBusy
	case len(l.starts) >= b.perHour:
		return errAnonymousRate
	}
	l.starts = append(l.starts, b.now())
	return nil
}

// CheckRate is checkRate of the listener of the anonymous caller p. It
// implements [messenger.Budget].
func (b *anonymousBudget) CheckRate(p auth.Principal) error {
	return b.checkRate(budgetListener(p))
}

// checkRate reports the error admit would return because the listener
// started all the tasks it may in an hour, without counting a task.
func (b *anonymousBudget) checkRate(listener string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.listener(listener).starts) >= b.perHour {
		return errAnonymousRate
	}
	return nil
}

// run counts a running task on a listener until release is called, or
// refuses it when the listener runs as many as it may.
func (b *anonymousBudget) run(listener string) (release func(), err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.listener(listener)
	if l.running >= b.concurrent {
		return nil, errAnonymousBusy
	}
	l.running++
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			l.running--
		})
	}, nil
}

// listener returns the budget of a listener, without the starts older than
// an hour. b.mu must be held.
func (b *anonymousBudget) listener(name string) *listenerBudget {
	l := b.listeners[name]
	if l == nil {
		l = &listenerBudget{}
		b.listeners[name] = l
	}
	cutoff := b.now().Add(-time.Hour)
	i := 0
	for i < len(l.starts) && !l.starts[i].After(cutoff) {
		i++
	}
	l.starts = l.starts[i:]
	return l
}
