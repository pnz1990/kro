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
	"sync"
	"time"
)

// stateNodeRateLimiter enforces a minimum interval between writes to the same
// storeName on the same instance. This prevents runaway write loops when a
// state node's expression increments a counter on every reconcile cycle.
//
// Scope: per storeName per instance (keyed by instance UID + storeName).
// Storage: in-memory only — resets on controller restart.
type stateNodeRateLimiter struct {
	mu          sync.Mutex
	minInterval time.Duration
	lastWrite   map[string]time.Time // key: instanceUID/storeName
}

// newStateNodeRateLimiter creates a rate limiter with the given minimum interval.
func newStateNodeRateLimiter(minInterval time.Duration) *stateNodeRateLimiter {
	return &stateNodeRateLimiter{
		minInterval: minInterval,
		lastWrite:   make(map[string]time.Time),
	}
}

// Allow checks whether a write is allowed for the given instance+storeName.
// Returns (allowed bool, remaining time.Duration). If not allowed, remaining
// indicates how long until the next write is permitted.
func (r *stateNodeRateLimiter) Allow(instanceUID, storeName string) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := instanceUID + "/" + storeName
	last, ok := r.lastWrite[key]
	if !ok {
		return true, 0
	}

	elapsed := time.Since(last)
	if elapsed >= r.minInterval {
		return true, 0
	}

	return false, r.minInterval - elapsed
}

// RecordWrite records that a successful write occurred for the given instance+storeName.
func (r *stateNodeRateLimiter) RecordWrite(instanceUID, storeName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := instanceUID + "/" + storeName
	r.lastWrite[key] = time.Now()
}

// Cleanup removes rate limit entries for the given instance UID.
// Should be called when an instance is deleted.
func (r *stateNodeRateLimiter) Cleanup(instanceUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for key := range r.lastWrite {
		if len(key) > len(instanceUID) && key[:len(instanceUID)+1] == instanceUID+"/" {
			delete(r.lastWrite, key)
		}
	}
}
