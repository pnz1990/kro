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
	"github.com/google/cel-go/common/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newKStateEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(
		KState(),
		cel.Variable("scope", cel.MapType(cel.StringType, cel.DynType)),
	)
	require.NoError(t, err)
	return env
}

func evalKState(t *testing.T, env *cel.Env, expr string, scope map[string]interface{}) interface{} {
	t.Helper()
	ast, iss := env.Compile(expr)
	require.NoError(t, iss.Err(), "compile %q", expr)

	prg, err := env.Program(ast)
	require.NoError(t, err, "program %q", expr)

	out, _, err := prg.Eval(map[string]interface{}{
		"scope": scope,
	})
	require.NoError(t, err, "eval %q", expr)
	return out.Value()
}

func TestKState_IntOverload(t *testing.T) {
	env := newKStateEnv(t)

	// Field present — should return the value
	result := evalKState(t, env, `kstate(scope, 'step', 0)`, map[string]interface{}{
		"step": int64(42),
	})
	assert.Equal(t, int64(42), result)

	// Field absent — should return default
	result = evalKState(t, env, `kstate(scope, 'step', 0)`, map[string]interface{}{})
	assert.Equal(t, int64(0), result)
}

func TestKState_StringOverload(t *testing.T) {
	env := newKStateEnv(t)

	result := evalKState(t, env, `kstate(scope, 'dest', "default")`, map[string]interface{}{
		"dest": "production",
	})
	assert.Equal(t, "production", result)

	result = evalKState(t, env, `kstate(scope, 'dest', "default")`, map[string]interface{}{})
	assert.Equal(t, "default", result)
}

func TestKState_BoolOverload(t *testing.T) {
	env := newKStateEnv(t)

	result := evalKState(t, env, `kstate(scope, 'active', false)`, map[string]interface{}{
		"active": true,
	})
	assert.Equal(t, true, result)

	result = evalKState(t, env, `kstate(scope, 'active', false)`, map[string]interface{}{})
	assert.Equal(t, false, result)
}

func TestKState_DoubleOverload(t *testing.T) {
	env := newKStateEnv(t)

	result := evalKState(t, env, `kstate(scope, 'score', 0.0)`, map[string]interface{}{
		"score": 3.14,
	})
	assert.Equal(t, 3.14, result)

	result = evalKState(t, env, `kstate(scope, 'score', 0.0)`, map[string]interface{}{})
	assert.Equal(t, 0.0, result)
}

func TestKState_EmptyScope(t *testing.T) {
	env := newKStateEnv(t)

	result := evalKState(t, env, `kstate(scope, 'missing', 99)`, map[string]interface{}{})
	assert.Equal(t, int64(99), result)
}

func TestKState_TypeInference_Int(t *testing.T) {
	env := newKStateEnv(t)
	ast, iss := env.Compile(`kstate(scope, 'step', 0)`)
	require.NoError(t, iss.Err())
	assert.Equal(t, "int", ast.OutputType().String())
}

func TestKState_TypeInference_String(t *testing.T) {
	env := newKStateEnv(t)
	ast, iss := env.Compile(`kstate(scope, 'dest', "")`)
	require.NoError(t, iss.Err())
	assert.Equal(t, "string", ast.OutputType().String())
}

func TestKState_TypeInference_Bool(t *testing.T) {
	env := newKStateEnv(t)
	ast, iss := env.Compile(`kstate(scope, 'active', false)`)
	require.NoError(t, iss.Err())
	assert.Equal(t, "bool", ast.OutputType().String())
}

func TestKState_TypeInference_Double(t *testing.T) {
	env := newKStateEnv(t)
	ast, iss := env.Compile(`kstate(scope, 'score', 0.0)`)
	require.NoError(t, iss.Err())
	assert.Equal(t, "double", ast.OutputType().String())
}

func TestKState_ImplDirectly(t *testing.T) {
	// Test wrong number of args
	result := kstateImpl()
	assert.True(t, types.IsError(result))

	// Test non-map first arg
	result = kstateImpl(types.String("notmap"), types.String("field"), types.Int(0))
	assert.True(t, types.IsError(result))

	// Test non-string second arg
	result = kstateImpl(
		types.DefaultTypeAdapter.NativeToValue(map[string]interface{}{}),
		types.Int(123),
		types.Int(0),
	)
	assert.True(t, types.IsError(result))
}
