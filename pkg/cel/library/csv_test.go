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

func TestCsvRemove(t *testing.T) {
	env, err := cel.NewEnv(CSV())
	require.NoError(t, err)

	tests := []struct {
		name string
		expr string
		want string
	}{
		{
			name: "remove from middle",
			expr: "csv.remove('a,b,c', 'b')",
			want: "a,c",
		},
		{
			name: "remove first",
			expr: "csv.remove('a,b,c', 'a')",
			want: "b,c",
		},
		{
			name: "remove last",
			expr: "csv.remove('a,b,c', 'c')",
			want: "a,b",
		},
		{
			name: "remove only item",
			expr: "csv.remove('a', 'a')",
			want: "",
		},
		{
			name: "item not found",
			expr: "csv.remove('a,b,c', 'x')",
			want: "a,b,c",
		},
		{
			name: "empty csv",
			expr: "csv.remove('', 'a')",
			want: "",
		},
		{
			name: "remove first of duplicates",
			expr: "csv.remove('a,b,a', 'a')",
			want: "b,a",
		},
		{
			name: "hyphenated items",
			expr: "csv.remove('weapon-common,hppotion-rare,armor-epic', 'hppotion-rare')",
			want: "weapon-common,armor-epic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, issues := env.Compile(tt.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.Value().(string))
		})
	}
}

func TestCsvAdd(t *testing.T) {
	env, err := cel.NewEnv(CSV())
	require.NoError(t, err)

	tests := []struct {
		name string
		expr string
		want string
	}{
		{
			name: "add to empty",
			expr: "csv.add('', 'sword', 8)",
			want: "sword",
		},
		{
			name: "add to existing",
			expr: "csv.add('sword', 'shield', 8)",
			want: "sword,shield",
		},
		{
			name: "add when full",
			expr: "csv.add('a,b,c', 'x', 3)",
			want: "a,b,c",
		},
		{
			name: "add when one below cap",
			expr: "csv.add('a,b', 'c', 3)",
			want: "a,b,c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, issues := env.Compile(tt.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.Value().(string))
		})
	}
}

func TestCsvContains(t *testing.T) {
	env, err := cel.NewEnv(CSV())
	require.NoError(t, err)

	tests := []struct {
		name string
		expr string
		want bool
	}{
		{
			name: "found",
			expr: "csv.contains('a,b,c', 'b')",
			want: true,
		},
		{
			name: "not found",
			expr: "csv.contains('a,b,c', 'x')",
			want: false,
		},
		{
			name: "empty csv",
			expr: "csv.contains('', 'a')",
			want: false,
		},
		{
			name: "no substring match",
			expr: "csv.contains('weapon-common,armor-rare', 'weapon')",
			want: false,
		},
		{
			name: "exact match with hyphens",
			expr: "csv.contains('weapon-common,armor-rare', 'weapon-common')",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, issues := env.Compile(tt.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.Value().(bool))
		})
	}
}
