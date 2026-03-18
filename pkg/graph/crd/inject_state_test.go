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

package crd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestInjectStateFields(t *testing.T) {
	t.Run("single storeName", func(t *testing.T) {
		crd := makeCRDWithStatus()
		InjectStateFields(crd, []string{"migration"})

		statusProps := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
		require.Contains(t, statusProps.Properties, "migration")

		migrationProp := statusProps.Properties["migration"]
		assert.Equal(t, "object", migrationProp.Type)
		assert.NotNil(t, migrationProp.XPreserveUnknownFields)
		assert.True(t, *migrationProp.XPreserveUnknownFields)
	})

	t.Run("multiple storeNames", func(t *testing.T) {
		crd := makeCRDWithStatus()
		InjectStateFields(crd, []string{"migration", "routing", "metrics"})

		statusProps := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
		assert.Contains(t, statusProps.Properties, "migration")
		assert.Contains(t, statusProps.Properties, "routing")
		assert.Contains(t, statusProps.Properties, "metrics")
	})

	t.Run("duplicate storeNames deduplicated", func(t *testing.T) {
		crd := makeCRDWithStatus()
		InjectStateFields(crd, []string{"migration", "migration"})

		statusProps := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
		assert.Contains(t, statusProps.Properties, "migration")
		// Should only have the one entry plus any existing
	})

	t.Run("empty storeNames is no-op", func(t *testing.T) {
		crd := makeCRDWithStatus()
		InjectStateFields(crd, nil)
		// Should not panic
	})

	t.Run("no versions is no-op", func(t *testing.T) {
		crd := &extv1.CustomResourceDefinition{}
		InjectStateFields(crd, []string{"migration"})
		// Should not panic
	})

	t.Run("does not overwrite existing status property", func(t *testing.T) {
		crd := makeCRDWithStatus()
		// Add an existing property under status
		statusProps := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
		statusProps.Properties["migration"] = extv1.JSONSchemaProps{
			Type: "string",
		}
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"] = statusProps

		InjectStateFields(crd, []string{"migration"})

		// Should NOT overwrite the existing property
		migrationProp := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["migration"]
		assert.Equal(t, "string", migrationProp.Type)
	})
}

func makeCRDWithStatus() *extv1.CustomResourceDefinition {
	return &extv1.CustomResourceDefinition{
		Spec: extv1.CustomResourceDefinitionSpec{
			Versions: []extv1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &extv1.CustomResourceValidation{
						OpenAPIV3Schema: &extv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]extv1.JSONSchemaProps{
								"status": {
									Type:       "object",
									Properties: map[string]extv1.JSONSchemaProps{},
								},
							},
						},
					},
				},
			},
		},
	}
}
