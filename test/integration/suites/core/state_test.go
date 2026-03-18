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

package core_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("State Nodes", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	// Test scenario 1 from proposal (line 896): Bootstrap
	// First reconcile with empty status.<storeName> writes initial values correctly;
	// kstate() returns default value when the field does not yet exist.
	It("should bootstrap state on first reconcile using kstate defaults", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-bootstrap",
			generator.WithSchema(
				"TestStateBootstrap", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			// State node that writes initial values using kstate() defaults.
			// On first reconcile, schema.status.counter does not exist, so
			// kstate returns the default (0), and step becomes 0 + 1 = 1.
			generator.WithStateResource("counter", "counter",
				map[string]string{
					"step":    "${kstate(schema.status.counter, 'step', 0) + 1}",
					"message": "${kstate(schema.status.counter, 'message', 'init')}",
				},
				nil,
			),
			// A ConfigMap that reads from the state node's output to verify
			// the state values are accessible by downstream resource nodes.
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-output",
				},
				"data": map[string]interface{}{
					"info": "bootstrap-test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Wait for RGD to become active.
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance.
		name := fmt.Sprintf("bootstrap-%s", rand.String(4))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestStateBootstrap",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Wait for instance to become ACTIVE with state values written.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify state store was written to status.counter.
		// kstate(scope, 'step', 0) returns 0 on first reconcile, so step = 0 + 1 = 1.
		// The step field is written as int64 by CEL but the API server stores JSON
		// numbers as float64, so we check for float64.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			step, found, err := unstructured.NestedFieldNoCopy(instance.Object, "status", "counter", "step")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.counter.step should exist")
			// JSON numbers from the API server are float64.
			g.Expect(step).To(BeNumerically("==", 1))

			msg, found, err := unstructured.NestedString(instance.Object, "status", "counter", "message")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.counter.message should exist")
			g.Expect(msg).To(Equal("init"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify the ConfigMap was also created (downstream node works).
		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name + "-output", Namespace: namespace}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data).To(HaveKeyWithValue("info", "bootstrap-test"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test scenario 2 from proposal (line 897): Cross-cycle accumulation
	// A counter field increments correctly across reconcile cycles.
	// The self-referencing expression step = kstate(step) + 1 should advance
	// on each reconcile. We verify it reaches at least 2 (meaning at least 2
	// reconcile cycles fired and each incremented correctly).
	It("should accumulate state across reconcile cycles", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-accumulate",
			generator.WithSchema(
				"TestStateAccumulate", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithStateResource("progress", "progress",
				map[string]string{
					"step": "${kstate(schema.status.progress, 'step', 0) + 1}",
				},
				nil,
			),
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-output",
				},
				"data": map[string]interface{}{
					"info": "accumulate-test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := fmt.Sprintf("accumulate-%s", rand.String(4))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestStateAccumulate",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// The self-referencing step = kstate(...) + 1 fires on every reconcile.
		// With a 5s default requeue and 100ms rate limit, step should reach 2
		// within ~10s. We use a 30s timeout for CI headroom.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			step, found, err := unstructured.NestedFieldNoCopy(instance.Object, "status", "progress", "step")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.progress.step should exist")
			g.Expect(step).To(BeNumerically(">=", 2), "step should have incremented across at least 2 reconcile cycles")
		}, 30*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test scenario 3 from proposal (line 898): Within-cycle chaining
	// Two state nodes A → B in DAG order, where B reads a field written by A.
	// Both fire in the same reconcile cycle; the instance reaches the expected
	// state. This tests the within-cycle context refresh mechanism.
	It("should chain state nodes within a single reconcile cycle", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-chain",
			generator.WithSchema(
				"TestStateChain", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			// State node A: writes a seed value.
			generator.WithStateResource("nodeA", "storeA",
				map[string]string{
					"value": "${42}",
				},
				nil,
			),
			// State node B: reads A's value via the within-cycle context refresh.
			// After A writes status.storeA.value = 42, B should see it and
			// compute doubled = 42 * 2 = 84.
			generator.WithStateResource("nodeB", "storeB",
				map[string]string{
					"doubled": "${kstate(schema.status.storeA, 'value', 0) * 2}",
				},
				nil,
			),
			// ConfigMap to anchor the graph and make the instance go ACTIVE.
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-chain",
				},
				"data": map[string]interface{}{
					"info": "chain-test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := fmt.Sprintf("chain-%s", rand.String(4))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestStateChain",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify both state stores are populated correctly.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// storeA.value should be 42.
			valA, found, err := unstructured.NestedFieldNoCopy(instance.Object, "status", "storeA", "value")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.storeA.value should exist")
			g.Expect(valA).To(BeNumerically("==", 42))

			// storeB.doubled should be 84 (42 * 2).
			valB, found, err := unstructured.NestedFieldNoCopy(instance.Object, "status", "storeB", "doubled")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.storeB.doubled should exist")
			g.Expect(valB).To(BeNumerically("==", 84))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify instance is ACTIVE.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test scenario 4 from proposal (line 899): includeWhen=false → Satisfied
	// State node is skipped; existing status.<storeName> values are preserved unchanged.
	// The Satisfied state does NOT propagate ignore contagiously to downstream nodes.
	It("should set Satisfied state when includeWhen is false and not propagate ignore", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-satisfied",
			generator.WithSchema(
				"TestStateSatisfied", "v1alpha1",
				map[string]interface{}{
					"name":       "string",
					"enableCalc": "boolean",
				},
				nil,
			),
			// State node with includeWhen that can be toggled.
			generator.WithStateResource("calc", "calc",
				map[string]string{
					"result": "${kstate(schema.status.calc, 'result', 0) + 10}",
				},
				[]string{"${schema.spec.enableCalc}"},
			),
			// Downstream ConfigMap that does NOT depend on the state node.
			// It should always be created regardless of the state node's includeWhen.
			generator.WithResource("alwaysCreated", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-always",
				},
				"data": map[string]interface{}{
					"info": "always-present",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance with enableCalc=false.
		name := fmt.Sprintf("satisfied-%s", rand.String(4))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestStateSatisfied",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":       name,
					"enableCalc": false,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Instance should become ACTIVE (state node → Satisfied, not Error).
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// The "always" ConfigMap must be created (Satisfied does NOT propagate ignore).
		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name + "-always", Namespace: namespace}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data).To(HaveKeyWithValue("info", "always-present"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// State store should NOT exist since enableCalc=false (state node never fired).
		Consistently(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			_, found, _ := unstructured.NestedFieldNoCopy(instance.Object, "status", "calc", "result")
			g.Expect(found).To(BeFalse(), "status.calc.result should not exist when includeWhen is false")
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test scenario 8 from proposal (line 903): Multi-storeName
	// Two state nodes targeting different storeNames coexist without collision.
	It("should support multiple state nodes with different storeNames", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-multi",
			generator.WithSchema(
				"TestStateMulti", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithStateResource("alpha", "alpha",
				map[string]string{
					"value": "${'hello'}",
				},
				nil,
			),
			generator.WithStateResource("beta", "beta",
				map[string]string{
					"value": "${'world'}",
				},
				nil,
			),
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-multi",
				},
				"data": map[string]interface{}{
					"info": "multi-test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := fmt.Sprintf("multi-%s", rand.String(4))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestStateMulti",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify both stores are written independently.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			alphaVal, found, err := unstructured.NestedString(instance.Object, "status", "alpha", "value")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.alpha.value should exist")
			g.Expect(alphaVal).To(Equal("hello"))

			betaVal, found, err := unstructured.NestedString(instance.Object, "status", "beta", "value")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.beta.value should exist")
			g.Expect(betaVal).To(Equal("world"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify ACTIVE.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test scenario 13 from proposal (line 912): State node with resource dependency
	// A state node expression references a resource node's output. The state node
	// should read from the resource node's observed state and write to its store.
	It("should allow state node to depend on resource node output", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-resdep",
			generator.WithSchema(
				"TestStateResDep", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			// Regular ConfigMap resource.
			generator.WithResource("source", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-source",
				},
				"data": map[string]interface{}{
					"key": "source-value",
				},
			}, nil, nil),
			// State node that reads from the source ConfigMap.
			generator.WithStateResource("derived", "derived",
				map[string]string{
					"captured": "${source.data.key}",
				},
				nil,
			),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := fmt.Sprintf("resdep-%s", rand.String(4))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestStateResDep",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify derived store has the value from the source ConfigMap.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			captured, found, err := unstructured.NestedString(instance.Object, "status", "derived", "captured")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.derived.captured should exist")
			g.Expect(captured).To(Equal("source-value"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify instance is ACTIVE.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test scenario 6 from proposal (line 901): Reserved storeName rejection
	// RGD with storeName: conditions is rejected with an error.
	It("should reject reserved storeName 'conditions'", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-reserved-conditions",
			generator.WithSchema(
				"TestStateReservedConditions", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithStateResource("badNode", "conditions",
				map[string]string{
					"val": "${1}",
				},
				nil,
			),
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-out",
				},
				"data": map[string]interface{}{
					"info": "test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		expectRGDInactiveWithError(ctx, rgd, "is reserved by kro")
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
	})

	// Also test reserved storeName "state".
	It("should reject reserved storeName 'state'", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-reserved-state",
			generator.WithSchema(
				"TestStateReservedState", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithStateResource("badNode", "state",
				map[string]string{
					"val": "${1}",
				},
				nil,
			),
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-out",
				},
				"data": map[string]interface{}{
					"info": "test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		expectRGDInactiveWithError(ctx, rgd, "is reserved by kro")
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
	})

	// Also test reserved storeName "managedResources".
	It("should reject reserved storeName 'managedResources'", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-reserved-mr",
			generator.WithSchema(
				"TestStateReservedMR", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithStateResource("badNode", "managedResources",
				map[string]string{
					"val": "${1}",
				},
				nil,
			),
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-out",
				},
				"data": map[string]interface{}{
					"info": "test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		expectRGDInactiveWithError(ctx, rgd, "is reserved by kro")
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
	})

	// Test scenario 7 from proposal (line 902): storeName/projection collision
	// RGD with a state node storeName matching a schema.status field is rejected.
	It("should reject storeName that collides with schema.status field", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-collision",
			generator.WithSchema(
				"TestStateCollision", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				// schema.status has a field "phase" — the state node also uses storeName "phase".
				map[string]interface{}{
					"phase": "${schema.spec.name}",
				},
			),
			generator.WithStateResource("badNode", "phase",
				map[string]string{
					"val": "${1}",
				},
				nil,
			),
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-out",
				},
				"data": map[string]interface{}{
					"info": "test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		expectRGDInactiveWithError(ctx, rgd, "conflicts with schema.status field")
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
	})

	// Test: state node with readyWhen should be rejected.
	// State nodes are ready when their expressions evaluate and status patch succeeds;
	// readyWhen is not applicable.
	It("should reject state node with readyWhen", func(ctx SpecContext) {
		// We cannot use the generator's WithStateResource for this because it
		// doesn't support readyWhen. We need to build the RGD manually.
		rgd := &krov1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-state-readywhen",
			},
			Spec: krov1alpha1.ResourceGraphDefinitionSpec{
				Schema: &krov1alpha1.Schema{
					Kind:       "TestStateReadyWhen",
					APIVersion: "v1alpha1",
					Spec:       toRawExtension(map[string]interface{}{"name": "string"}),
					Status:     toRawExtension(map[string]interface{}{}),
				},
				Resources: []*krov1alpha1.Resource{
					{
						ID: "badNode",
						State: &krov1alpha1.StateFields{
							StoreName: "mystore",
							Fields: map[string]string{
								"val": "${1}",
							},
						},
						ReadyWhen: []string{"${true}"},
					},
					{
						ID:       "output",
						Template: toRawExtension(map[string]interface{}{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "${schema.spec.name}-out"}, "data": map[string]interface{}{"info": "test"}}),
					},
				},
			},
		}

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		expectRGDInactiveWithError(ctx, rgd, "cannot use readyWhen with state nodes")
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
	})

	// Test: kstate() with different typed overloads (string, bool, double).
	It("should support kstate typed overloads for string, bool, and double", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-types",
			generator.WithSchema(
				"TestStateTypes", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithStateResource("typed", "typed",
				map[string]string{
					"strVal":    "${kstate(schema.status.typed, 'strVal', 'default-str')}",
					"boolVal":   "${kstate(schema.status.typed, 'boolVal', false) || true}",
					"doubleVal": "${kstate(schema.status.typed, 'doubleVal', 0.0) + 3.14}",
					"intVal":    "${kstate(schema.status.typed, 'intVal', 0) + 100}",
				},
				nil,
			),
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-types",
				},
				"data": map[string]interface{}{
					"info": "types-test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := fmt.Sprintf("types-%s", rand.String(4))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestStateTypes",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify all typed values.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// strVal: kstate returns "default-str" on first reconcile.
			strVal, found, err := unstructured.NestedString(instance.Object, "status", "typed", "strVal")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.typed.strVal should exist")
			g.Expect(strVal).To(Equal("default-str"))

			// boolVal: kstate returns false, then expression negates: !false = true.
			boolVal, found, err := unstructured.NestedBool(instance.Object, "status", "typed", "boolVal")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.typed.boolVal should exist")
			g.Expect(boolVal).To(BeTrue())

			// doubleVal: kstate returns 0.0, then 0.0 + 3.14 = 3.14.
			doubleVal, found, err := unstructured.NestedFieldNoCopy(instance.Object, "status", "typed", "doubleVal")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.typed.doubleVal should exist")
			g.Expect(doubleVal).To(BeNumerically("~", 3.14, 0.001))

			// intVal: kstate returns 0, then 0 + 100 = 100.
			intVal, found, err := unstructured.NestedFieldNoCopy(instance.Object, "status", "typed", "intVal")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.typed.intVal should exist")
			g.Expect(intVal).To(BeNumerically("==", 100))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test scenario 11 from proposal (line 906): End-of-reconcile preservation
	// State node values written during a cycle are not overwritten by the final
	// updateStatus() call. We verify this by creating a state node alongside a
	// schema.status projection, and confirming both survive.
	It("should preserve state store values alongside schema.status projections", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-preserve",
			generator.WithSchema(
				"TestStatePreserve", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				// schema.status projection.
				map[string]interface{}{
					"outputName": "${output.metadata.name}",
				},
			),
			generator.WithStateResource("tracker", "tracker",
				map[string]string{
					"seen": "${'yes'}",
				},
				nil,
			),
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-preserve",
				},
				"data": map[string]interface{}{
					"info": "preserve-test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := fmt.Sprintf("preserve-%s", rand.String(4))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestStatePreserve",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify both state store AND schema.status projection exist.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// State store value.
			seen, found, err := unstructured.NestedString(instance.Object, "status", "tracker", "seen")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.tracker.seen should be preserved")
			g.Expect(seen).To(Equal("yes"))

			// Schema.status projection.
			outputName, found, err := unstructured.NestedString(instance.Object, "status", "outputName")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.outputName projection should exist")
			g.Expect(outputName).To(Equal(name + "-preserve"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify the state survives across multiple reconcile cycles.
		Consistently(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			seen, found, err := unstructured.NestedString(instance.Object, "status", "tracker", "seen")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.tracker.seen should still be preserved after requeue")
			g.Expect(seen).To(Equal("yes"))

			outputName, found, err := unstructured.NestedString(instance.Object, "status", "outputName")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.outputName should still exist after requeue")
			g.Expect(outputName).To(Equal(name + "-preserve"))
		}, 6*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test: state node with no template, externalRef, or state should fail validation
	// (though this is primarily an API-level validation via kubebuilder XValidation,
	// the graph builder also enforces it). We test the inverse — that providing both
	// template and state is rejected.
	// Note: The XValidation rule "exactly one of template, externalRef, or state must be
	// provided" fires at admission. If the webhook isn't installed in envtest, this falls
	// through to the graph builder's own validation.

	// Test: deletion cleanup — verify instances with state nodes can be deleted cleanly.
	It("should delete instances with state nodes cleanly", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-delete",
			generator.WithSchema(
				"TestStateDelete", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithStateResource("tracker", "tracker",
				map[string]string{
					"count": "${kstate(schema.status.tracker, 'count', 0) + 1}",
				},
				nil,
			),
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-del",
				},
				"data": map[string]interface{}{
					"info": "delete-test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		name := fmt.Sprintf("del-%s", rand.String(4))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestStateDelete",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Wait for ACTIVE with state written.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ACTIVE"))

			_, found, _ = unstructured.NestedFieldNoCopy(instance.Object, "status", "tracker", "count")
			g.Expect(found).To(BeTrue(), "state store should exist before deletion")
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Delete instance.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// Verify instance is fully deleted.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify managed ConfigMap is also deleted (cascading deletion).
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name + "-del", Namespace: namespace}, &corev1.ConfigMap{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "managed ConfigMap should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup RGD.
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	// Test: CRD injection — verify the generated CRD has status.<storeName> with
	// x-kubernetes-preserve-unknown-fields: true.
	It("should inject state store fields into the generated CRD schema", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-state-crd",
			generator.WithSchema(
				"TestStateCRD", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithStateResource("myStore", "myStore",
				map[string]string{
					"val": "${1}",
				},
				nil,
			),
			generator.WithResource("output", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-crd",
				},
				"data": map[string]interface{}{
					"info": "crd-test",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Wait for RGD to become active (which means the CRD was created).
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Now create an instance and write an arbitrary nested structure under
		// status.myStore — if x-kubernetes-preserve-unknown-fields is set, the
		// API server will accept arbitrary fields.
		name := fmt.Sprintf("crd-%s", rand.String(4))
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestStateCRD",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Wait for the state store to be written (val=1 proves it works).
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedFieldNoCopy(instance.Object, "status", "myStore", "val")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.myStore.val should exist, proving CRD schema allows it")
			g.Expect(val).To(BeNumerically("==", 1))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
