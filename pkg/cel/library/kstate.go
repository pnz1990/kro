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
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// KState returns a CEL library that provides the kstate() helper function for
// safe access to state node storage scopes. kstate reads a named field from a
// map scope and returns a typed default when the field is absent.
//
// # kstate
//
// Reads fieldName from scope and returns defaultVal if absent. Typed overloads
// ensure the return type matches the default's type at compile time.
//
//	kstate(scope map(string, dyn), field string, default int)    -> int
//	kstate(scope map(string, dyn), field string, default string) -> string
//	kstate(scope map(string, dyn), field string, default bool)   -> bool
//	kstate(scope map(string, dyn), field string, default double) -> double
//
// Complex types (lists, maps) fall through to dyn at runtime — a dyn overload
// is deferred to v2 to avoid CEL overload collisions.
//
// Examples:
//
//	kstate(schema.status.migration, 'step', 0)         // returns int
//	kstate(schema.status.routing, 'dest', "default")   // returns string
//	kstate(schema.status.flags, 'active', false)        // returns bool
//	kstate(schema.status.metrics, 'score', 0.0)         // returns double
func KState() cel.EnvOption {
	return cel.Lib(&kstateLibrary{})
}

type kstateLibrary struct{}

func (l *kstateLibrary) LibraryName() string {
	return "kro.kstate"
}

func (l *kstateLibrary) CompileOptions() []cel.EnvOption {
	mapType := cel.MapType(cel.StringType, cel.DynType)
	return []cel.EnvOption{
		cel.Function("kstate",
			// Typed overload: int default -> int return
			cel.Overload("kstate_map_string_int",
				[]*cel.Type{mapType, cel.StringType, cel.IntType},
				cel.IntType,
				cel.FunctionBinding(kstateImpl),
			),
			// Typed overload: string default -> string return
			cel.Overload("kstate_map_string_string",
				[]*cel.Type{mapType, cel.StringType, cel.StringType},
				cel.StringType,
				cel.FunctionBinding(kstateImpl),
			),
			// Typed overload: bool default -> bool return
			cel.Overload("kstate_map_string_bool",
				[]*cel.Type{mapType, cel.StringType, cel.BoolType},
				cel.BoolType,
				cel.FunctionBinding(kstateImpl),
			),
			// Typed overload: double default -> double return
			cel.Overload("kstate_map_string_double",
				[]*cel.Type{mapType, cel.StringType, cel.DoubleType},
				cel.DoubleType,
				cel.FunctionBinding(kstateImpl),
			),
		),
	}
}

func (l *kstateLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

// kstateImpl is the shared implementation for all kstate overloads.
// It reads a field from a map scope and returns the default value if the field
// is absent. The overload selection at compile time ensures the return type
// matches the caller's expectation.
func kstateImpl(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("kstate: expected 3 arguments (scope, field, default)")
	}

	// First argument: scope (map)
	scope, ok := args[0].(traits.Mapper)
	if !ok {
		return types.NewErr("kstate: first argument must be a map, got %s", args[0].Type())
	}

	// Second argument: field name (string)
	if args[1].Type() != types.StringType {
		return types.NewErr("kstate: second argument must be a string, got %s", args[1].Type())
	}
	fieldName := args[1]

	// Third argument: default value (any type)
	defaultVal := args[2]

	// Look up the field in the map. Use the Find method which returns (ref.Val, bool).
	val, found := scope.Find(fieldName)
	if !found {
		return defaultVal
	}
	// If the value is an error type (e.g., field exists but conversion failed),
	// return the default.
	if types.IsError(val) {
		return defaultVal
	}

	// Convert the value to the expected type based on the default value's type.
	// This handles the case where the map stores dyn values but we need a
	// specific type. If conversion fails, return the raw value (dyn fallback).
	if defaultVal.Type() != types.ErrType {
		converted := val.ConvertToType(defaultVal.Type())
		if !types.IsError(converted) {
			return converted
		}
	}

	// For dyn fallback or when type conversion isn't possible, return raw value.
	return val
}

// KStateFieldName extracts the field name from a kstate call for validation.
// This is a utility for static analysis, not for runtime use.
func KStateFieldName(fieldNameArg string) (string, error) {
	if fieldNameArg == "" {
		return "", fmt.Errorf("kstate field name cannot be empty")
	}
	return fieldNameArg, nil
}
