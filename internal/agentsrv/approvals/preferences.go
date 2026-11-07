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

package approvals

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// SuggestAfter is in how many conversations in a row a person allows the
// same rule always before the server offers to allow it in all their
// conversations.
const SuggestAfter = 3

// Preferences are the tool calls that people allowed in all the tasks
// they start, and the offers to allow them.
type Preferences struct {
	store *store.Store
	// added is called after a preference is added, so that it answers the
	// questions that wait. Optional.
	added func(ctx context.Context)
}

// NewPreferences returns the preferences kept in st. added is called after
// a preference is added; it may be nil.
func NewPreferences(st *store.Store, added func(ctx context.Context)) *Preferences {
	return &Preferences{store: st, added: added}
}

// List returns the rules user allowed in all their tasks.
func (p *Preferences) List(ctx context.Context, user string) ([]string, error) {
	return p.store.ToolPreferences(ctx, user)
}

// Allow adds rule to the preferences of user, and starts the count of
// user's taps for it over.
func (p *Preferences) Allow(ctx context.Context, user, rule string) error {
	if err := toolrules.Validate(rule); err != nil {
		return fmt.Errorf("invalid rule %q: %w", rule, err)
	}
	if err := p.store.AddToolPreference(ctx, user, rule); err != nil {
		return err
	}
	slog.Info("tool calls allowed in all conversations", "user", user, "rule", toolrules.Redact(rule))
	p.forgetTaps(ctx, user, rule)
	if p.added != nil {
		p.added(ctx)
	}
	return nil
}

// Remove removes rule from the preferences of user. It returns
// [store.ErrNotFound] if user didn't allow it.
func (p *Preferences) Remove(ctx context.Context, user, rule string) error {
	if err := p.store.DeleteToolPreference(ctx, user, rule); err != nil {
		return err
	}
	slog.Info("tool calls no longer allowed in all conversations", "user", user, "rule", toolrules.Redact(rule))
	return nil
}

// Decline starts the count of user's taps for rule over, after they
// declined the offer to allow it in all their conversations.
func (p *Preferences) Decline(ctx context.Context, user, rule string) {
	p.forgetTaps(ctx, user, rule)
}

// Due reports whether to offer user to allow rule in all their
// conversations: they allowed it always in SuggestAfter conversations in
// a row, and it isn't one of their preferences yet.
func (p *Preferences) Due(ctx context.Context, user, rule string) (bool, error) {
	n, err := p.store.ToolTaps(ctx, user, rule)
	if err != nil || n < SuggestAfter {
		return false, err
	}
	prefs, err := p.store.ToolPreferences(ctx, user)
	if err != nil {
		return false, err
	}
	return !slices.Contains(prefs, rule), nil
}

func (p *Preferences) forgetTaps(ctx context.Context, user, rule string) {
	if err := p.store.ResetToolTaps(ctx, user, rule); err != nil {
		slog.Warn("resetting the taps on always allow", "user", user, "error", err)
	}
}
