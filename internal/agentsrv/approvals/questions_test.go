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
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/google/jsonschema-go/jsonschema"
)

func questionMessage(t *testing.T, id string, seq int, options ...[2]string) *a2a.Message {
	t.Helper()
	optionSchema := &jsonschema.Schema{Type: "string"}
	for _, o := range options {
		optionSchema.OneOf = append(optionSchema.OneOf, &jsonschema.Schema{Const: jsonschema.Ptr[any](o[0]), Title: o[1]})
	}
	schema := &jsonschema.Schema{Type: "object", Title: "git push", Properties: map[string]*jsonschema.Schema{"option": optionSchema}}
	if seq != 0 {
		schema.Properties["seq"] = &jsonschema.Schema{Type: "integer", Const: jsonschema.Ptr[any](seq)}
	}
	part, err := structuredinput.NewSchemaPart(id, schema)
	if err != nil {
		t.Fatalf("structuredinput.NewSchemaPart() error = %v", err)
	}
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Run git push?"), part)
}

func permissionMessage(t *testing.T) *a2a.Message {
	t.Helper()
	return questionMessage(t, "tool1", 0, [2]string{"once", "Allow"}, [2]string{"always", "Always allow"}, [2]string{"reject", ""})
}

func numberedMessage(t *testing.T, id string, seq int) *a2a.Message {
	t.Helper()
	return questionMessage(t, id, seq, [2]string{"once", "Allow"}, [2]string{"reject", "Deny"})
}
