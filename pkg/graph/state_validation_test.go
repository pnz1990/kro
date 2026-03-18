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

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func TestValidateStoreName(t *testing.T) {
	tests := []struct {
		name         string
		storeName    string
		statusFields sets.String
		wantErr      string
	}{
		{
			name:         "valid storeName",
			storeName:    "migration",
			statusFields: sets.NewString("bossState", "heroHP"),
		},
		{
			name:         "reserved: state",
			storeName:    "state",
			statusFields: sets.NewString(),
			wantErr:      "reserved by kro",
		},
		{
			name:         "reserved: conditions",
			storeName:    "conditions",
			statusFields: sets.NewString(),
			wantErr:      "reserved by kro",
		},
		{
			name:         "reserved: managedResources",
			storeName:    "managedResources",
			statusFields: sets.NewString(),
			wantErr:      "reserved by kro",
		},
		{
			name:         "collision with status projection field",
			storeName:    "bossState",
			statusFields: sets.NewString("bossState", "heroHP"),
			wantErr:      "conflicts with schema.status field",
		},
		{
			name:         "no collision with different status fields",
			storeName:    "routing",
			statusFields: sets.NewString("bossState", "heroHP"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStoreName(tt.storeName, tt.statusFields)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidateCombinableResourceFields_State(t *testing.T) {
	tests := []struct {
		name    string
		res     *v1alpha1.Resource
		wantErr string
	}{
		{
			name: "state only is valid",
			res: &v1alpha1.Resource{
				ID: "migrator",
				State: &v1alpha1.StateFields{
					StoreName: "migration",
					Fields: map[string]string{
						"step": "${schema.status.migration.step + 1}",
					},
				},
			},
		},
		{
			name: "state with forEach is invalid",
			res: &v1alpha1.Resource{
				ID: "migrator",
				State: &v1alpha1.StateFields{
					StoreName: "migration",
					Fields:    map[string]string{"step": "${0}"},
				},
				ForEach: []v1alpha1.ForEachDimension{{"item": "${schema.spec.items}"}},
			},
			wantErr: "cannot use state with forEach",
		},
		{
			name: "state with readyWhen is invalid",
			res: &v1alpha1.Resource{
				ID: "migrator",
				State: &v1alpha1.StateFields{
					StoreName: "migration",
					Fields:    map[string]string{"step": "${0}"},
				},
				ReadyWhen: []string{"${true}"},
			},
			wantErr: "cannot use readyWhen with state nodes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCombinableResourceFields(tt.res)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}
