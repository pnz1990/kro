// Copyright 2025 The Kubernetes Authors.
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

package library

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListsSet(t *testing.T) {
	env, err := cel.NewEnv(Lists())
	require.NoError(t, err)

	tests := []struct {
		name    string
		expr    string
		want    interface{}
		wantErr bool
		errMsg  string
	}{
		{
			name: "set middle element",
			expr: "lists.set([10, 20, 30], 1, 99)",
			want: []int64{10, 99, 30},
		},
		{
			name: "set first element",
			expr: "lists.set([10, 20, 30], 0, 0)",
			want: []int64{0, 20, 30},
		},
		{
			name: "set last element",
			expr: "lists.set([10, 20, 30], 2, 50)",
			want: []int64{10, 20, 50},
		},
		{
			name:    "index out of bounds",
			expr:    "lists.set([10, 20], 5, 99)",
			wantErr: true,
			errMsg:  "out of bounds",
		},
		{
			name:    "negative index",
			expr:    "lists.set([10, 20], -1, 99)",
			wantErr: true,
			errMsg:  "out of bounds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, issues := env.Compile(tt.expr)
			if tt.wantErr && issues != nil {
				return
			}
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			if tt.wantErr {
				if err != nil {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
				return
			}
			require.NoError(t, err)

			// Convert result to []int64 for comparison
			resultRaw := out.Value()
			var result []int64
			switch v := resultRaw.(type) {
			case []int64:
				result = v
			case []interface{}:
				result = make([]int64, len(v))
				for i, elem := range v {
					result[i] = elem.(int64)
				}
			default:
				t.Fatalf("unexpected result type %T", resultRaw)
			}
			assert.Equal(t, tt.want, result)
		})
	}
}
