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

package instance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStateNodeRateLimiter_AllowFirstWrite(t *testing.T) {
	rl := newStateNodeRateLimiter(100 * time.Millisecond)

	allowed, remaining := rl.Allow("uid-1", "migration")
	assert.True(t, allowed)
	assert.Equal(t, time.Duration(0), remaining)
}

func TestStateNodeRateLimiter_BlocksWithinInterval(t *testing.T) {
	rl := newStateNodeRateLimiter(100 * time.Millisecond)

	rl.RecordWrite("uid-1", "migration")

	allowed, remaining := rl.Allow("uid-1", "migration")
	assert.False(t, allowed)
	assert.Greater(t, remaining, time.Duration(0))
	assert.LessOrEqual(t, remaining, 100*time.Millisecond)
}

func TestStateNodeRateLimiter_AllowsAfterInterval(t *testing.T) {
	rl := newStateNodeRateLimiter(10 * time.Millisecond)

	rl.RecordWrite("uid-1", "migration")
	time.Sleep(15 * time.Millisecond)

	allowed, _ := rl.Allow("uid-1", "migration")
	assert.True(t, allowed)
}

func TestStateNodeRateLimiter_IndependentInstances(t *testing.T) {
	rl := newStateNodeRateLimiter(100 * time.Millisecond)

	rl.RecordWrite("uid-1", "migration")

	// Different instance should be allowed
	allowed, _ := rl.Allow("uid-2", "migration")
	assert.True(t, allowed)
}

func TestStateNodeRateLimiter_IndependentStoreNames(t *testing.T) {
	rl := newStateNodeRateLimiter(100 * time.Millisecond)

	rl.RecordWrite("uid-1", "migration")

	// Different storeName should be allowed
	allowed, _ := rl.Allow("uid-1", "routing")
	assert.True(t, allowed)
}

func TestStateNodeRateLimiter_Cleanup(t *testing.T) {
	rl := newStateNodeRateLimiter(100 * time.Millisecond)

	rl.RecordWrite("uid-1", "migration")
	rl.RecordWrite("uid-1", "routing")
	rl.RecordWrite("uid-2", "migration")

	rl.Cleanup("uid-1")

	// uid-1 entries should be cleared
	allowed, _ := rl.Allow("uid-1", "migration")
	assert.True(t, allowed)

	// uid-2 entries should remain
	allowed, _ = rl.Allow("uid-2", "migration")
	assert.False(t, allowed)
}
