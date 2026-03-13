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
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// CSV returns a CEL library that provides functions for manipulating
// comma-separated value strings. This is useful for managing simple
// string-encoded lists in Kubernetes resource spec fields.
//
// Library functions:
//
//	csv.remove(csv, item)       — removes first exact occurrence of item from CSV
//	csv.add(csv, item, cap)     — appends item to CSV if current count < cap
//	csv.contains(csv, item)     — true if CSV contains exact item match
//
// Examples:
//
//	csv.remove("a,b,c", "b")       // returns "a,c"
//	csv.add("a,b", "c", 5)         // returns "a,b,c"
//	csv.add("a,b,c", "d", 3)       // returns "a,b,c" (at capacity)
//	csv.contains("a,b,c", "b")     // returns true
//	csv.contains("a,b,c", "x")     // returns false
func CSV() cel.EnvOption {
	return cel.Lib(&csvLibrary{})
}

type csvLibrary struct{}

func (l *csvLibrary) LibraryName() string {
	return "kro.csv"
}

func (l *csvLibrary) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		// csv.remove(csv string, item string) -> string
		// Removes the first exact occurrence of item from a comma-separated string.
		// Returns the original string if item is not found.
		cel.Function("csv.remove",
			cel.Overload("csv.remove_string_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				cel.StringType,
				cel.BinaryBinding(csvRemove),
			),
		),

		// csv.add(csv string, item string, cap int) -> string
		// Appends item to csv if current count < cap. Returns unchanged csv if full.
		cel.Function("csv.add",
			cel.Overload("csv.add_string_string_int",
				[]*cel.Type{cel.StringType, cel.StringType, cel.IntType},
				cel.StringType,
				cel.FunctionBinding(csvAdd),
			),
		),

		// csv.contains(csv string, item string) -> bool
		// Returns true if csv contains an exact match for item (delimiter-bounded).
		cel.Function("csv.contains",
			cel.Overload("csv.contains_string_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				cel.BoolType,
				cel.BinaryBinding(csvContains),
			),
		),
	}
}

func (l *csvLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

// csvRemove removes the first exact occurrence of item from a CSV string.
func csvRemove(csvVal ref.Val, itemVal ref.Val) ref.Val {
	if csvVal.Type() != types.StringType {
		return types.NewErr("csv.remove: csv must be a string")
	}
	if itemVal.Type() != types.StringType {
		return types.NewErr("csv.remove: item must be a string")
	}

	csv := string(csvVal.(types.String))
	item := string(itemVal.(types.String))

	if csv == "" {
		return types.String("")
	}

	parts := strings.Split(csv, ",")
	found := false
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if !found && p == item {
			found = true
			continue
		}
		result = append(result, p)
	}

	return types.String(strings.Join(result, ","))
}

// csvAdd appends item to csv if the current count is below cap.
func csvAdd(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("csv.add: expected 3 arguments (csv, item, cap)")
	}

	csvVal := args[0]
	itemVal := args[1]
	capVal := args[2]

	if csvVal.Type() != types.StringType {
		return types.NewErr("csv.add: csv must be a string")
	}
	if itemVal.Type() != types.StringType {
		return types.NewErr("csv.add: item must be a string")
	}
	if capVal.Type() != types.IntType {
		return types.NewErr("csv.add: cap must be an integer")
	}

	csv := string(csvVal.(types.String))
	item := string(itemVal.(types.String))
	cap := int64(capVal.(types.Int))

	// Count current items
	count := int64(0)
	if csv != "" {
		count = int64(len(strings.Split(csv, ",")))
	}

	if count >= cap {
		return types.String(csv) // full — return unchanged
	}

	if csv == "" {
		return types.String(item)
	}
	return types.String(csv + "," + item)
}

// csvContains checks if csv contains an exact match for item.
func csvContains(csvVal ref.Val, itemVal ref.Val) ref.Val {
	if csvVal.Type() != types.StringType {
		return types.NewErr("csv.contains: csv must be a string")
	}
	if itemVal.Type() != types.StringType {
		return types.NewErr("csv.contains: item must be a string")
	}

	csv := string(csvVal.(types.String))
	item := string(itemVal.(types.String))

	if csv == "" {
		return types.Bool(false)
	}

	parts := strings.Split(csv, ",")
	for _, p := range parts {
		if p == item {
			return types.Bool(true)
		}
	}

	return types.Bool(false)
}
