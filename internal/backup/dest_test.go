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

package backup

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// fakeCLI records commands and answers them from outputs, keyed by the
// command line.
type fakeCLI struct {
	mu      sync.Mutex
	calls   []string
	stdin   map[string]string
	outputs map[string]string
	errs    map[string]error
}

func (f *fakeCLI) run(_ context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error {
	line := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, line)
	if stdin != nil {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		f.stdin[line] = string(data)
	}
	if _, err := io.WriteString(stdout, f.outputs[line]); err != nil {
		return err
	}
	return f.errs[line]
}

func TestCLIDest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		to        string
		listOut   string
		wantNames []string
		wantCalls []string
	}{
		{
			name:      "gcs",
			to:        "gs://bucket/agents/coder/",
			listOut:   "gs://bucket/agents/coder/coder-20261102T193500Z.a2abak\ngs://bucket/agents/coder/old/\n",
			wantNames: []string{"coder-20261102T193500Z.a2abak"},
			wantCalls: []string{
				"gcloud storage cp - gs://bucket/agents/coder/s1",
				"gcloud storage ls gs://bucket/agents/coder/",
				"gcloud storage cat gs://bucket/agents/coder/s1",
				"gcloud storage rm gs://bucket/agents/coder/s1",
			},
		},
		{
			name:      "s3",
			to:        "s3://bucket/agents/coder",
			listOut:   "                           PRE old/\n2026-11-02 19:35:00       1234 coder-20261102T193500Z.a2abak\n",
			wantNames: []string{"coder-20261102T193500Z.a2abak"},
			wantCalls: []string{
				"aws s3 cp - s3://bucket/agents/coder/s1",
				"aws s3 ls s3://bucket/agents/coder/",
				"aws s3 cp s3://bucket/agents/coder/s1 -",
				"aws s3 rm s3://bucket/agents/coder/s1",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := &fakeCLI{stdin: map[string]string{}, outputs: map[string]string{
				tc.wantCalls[1]: tc.listOut,
				tc.wantCalls[2]: "snapshot bytes",
			}}
			dest, err := Open(tc.to, f.run)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			if err := dest.Put(ctx, "s1", strings.NewReader("snapshot bytes")); err != nil {
				t.Fatalf("Put() error = %v", err)
			}
			names, err := dest.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			rc, err := dest.Get(ctx, "s1")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			got, err := io.ReadAll(rc)
			if err != nil || string(got) != "snapshot bytes" {
				t.Fatalf("Get() read %q, %v, want the snapshot", got, err)
			}
			if err := dest.Delete(ctx, "s1"); err != nil {
				t.Fatalf("Delete() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantNames, names); diff != "" {
				t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantCalls, f.calls); diff != "" {
				t.Fatalf("commands wrong (-want +got) diff = %s", diff)
			}
			if f.stdin[tc.wantCalls[0]] != "snapshot bytes" {
				t.Fatalf("Put() sent %q, want the snapshot", f.stdin[tc.wantCalls[0]])
			}
		})
	}
}

func TestCLIDestListEmpty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		to      string
		line    string
		err     error
		wantErr bool
	}{
		{name: "gcs prefix without objects", to: "gs://b/p", line: "gcloud storage ls gs://b/p/", err: errors.New("gcloud: exit status 1: ERROR: One or more URLs matched no objects.")},
		{name: "s3 prefix without objects", to: "s3://b/p", line: "aws s3 ls s3://b/p/", err: errors.New("aws: exit status 1")},
		{name: "gcs permission denied", to: "gs://b/p", line: "gcloud storage ls gs://b/p/", err: errors.New("gcloud: exit status 1: AccessDeniedException: 403"), wantErr: true},
		{name: "s3 no credentials", to: "s3://b/p", line: "aws s3 ls s3://b/p/", err: errors.New("aws: exit status 255: Unable to locate credentials"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCLI{stdin: map[string]string{}, errs: map[string]error{tc.line: tc.err}}
			dest, err := Open(tc.to, f.run)
			if err != nil {
				t.Fatal(err)
			}
			names, err := dest.List(context.Background())
			if (err != nil) != tc.wantErr || len(names) != 0 {
				t.Fatalf("List() = %v, %v, want no names and error %v", names, err, tc.wantErr)
			}
		})
	}
}

func TestOpenErrors(t *testing.T) {
	t.Parallel()
	for _, to := range []string{"", "ftp://host/dir"} {
		if _, err := Open(to, nil); err == nil {
			t.Fatalf("Open(%q) error = nil, want error", to)
		}
	}
}
