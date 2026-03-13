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
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// Lists returns a CEL library that provides additional list manipulation
// functions beyond the standard CEL ext.Lists() library.
//
// Library functions:
//
//	lists.set(arr, index, value)  — returns a new list with arr[index] replaced by value
//
// Example:
//
//	lists.set([10, 20, 30], 1, 99)  // returns [10, 99, 30]
func Lists() cel.EnvOption {
	return cel.Lib(&listsLibrary{})
}

type listsLibrary struct{}

func (l *listsLibrary) LibraryName() string {
	return "kro.lists"
}

func (l *listsLibrary) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		// lists.set(arr list(int), index int, value int) -> list(int)
		// Returns a new list with the element at index replaced by value.
		// Index must be in bounds [0, len(arr)).
		cel.Function("lists.set",
			cel.Overload("lists.set_list_int_int",
				[]*cel.Type{cel.ListType(cel.IntType), cel.IntType, cel.IntType},
				cel.ListType(cel.IntType),
				cel.FunctionBinding(listsSet),
			),
		),
	}
}

func (l *listsLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

// listsSet returns a new list with arr[index] replaced by value.
func listsSet(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("lists.set: expected 3 arguments (arr, index, value)")
	}

	arrVal := args[0]
	idxVal := args[1]
	valVal := args[2]

	// Validate arr is a list
	lister, ok := arrVal.(traits.Lister)
	if !ok {
		return types.NewErr("lists.set: first argument must be a list")
	}

	// Validate index is int
	if idxVal.Type() != types.IntType {
		return types.NewErr("lists.set: index must be an integer")
	}

	// Validate value is int
	if valVal.Type() != types.IntType {
		return types.NewErr("lists.set: value must be an integer")
	}

	idx := int64(idxVal.(types.Int))
	size := int64(lister.Size().(types.Int))

	if idx < 0 || idx >= size {
		return types.NewErr("lists.set: index %d out of bounds [0, %d)", idx, size)
	}

	// Build new list with the element replaced (using native int64 values)
	newVal := int64(valVal.(types.Int))
	newArr := make([]int64, size)
	for i := int64(0); i < size; i++ {
		if i == idx {
			newArr[i] = newVal
		} else {
			newArr[i] = int64(lister.Get(types.Int(i)).(types.Int))
		}
	}

	return types.DefaultTypeAdapter.NativeToValue(newArr)
}
