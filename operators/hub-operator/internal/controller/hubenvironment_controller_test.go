/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	opsv1alpha1 "github.com/soloz-io/zero-ops/operators/hub-operator/api/v1alpha1"
)

var _ = Describe("HubEnvironment Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		hubenvironment := &opsv1alpha1.HubEnvironment{}

		BeforeEach(func() {
			// Namespaces the reconcile addresses. They exist in a real cluster from the
			// platform install; envtest starts with none, so the first Create into one
			// fails with "namespaces ... not found" long before any assertion.
			By("ensuring the platform namespaces the reconcile writes into")
			for _, ns := range []string{"platform-security", "platform-core", "infisical"} {
				err := k8sClient.Create(ctx, &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: ns},
				})
				if err != nil && !errors.IsAlreadyExists(err) {
					Expect(err).NotTo(HaveOccurred())
				}
			}

			By("creating the custom resource for the Kind HubEnvironment")
			err := k8sClient.Get(ctx, typeNamespacedName, hubenvironment)
			if err != nil && errors.IsNotFound(err) {
				resource := &opsv1alpha1.HubEnvironment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					// domain and environment are required and validated: domain by
					// pattern, environment by enum, and three CEL rules key off
					// environment. An empty spec is rejected by the API server, so the
					// fixture has to carry a valid one for the reconcile to be reached.
					Spec: opsv1alpha1.HubEnvironmentSpec{
						Domain:      "dev.example.com",
						Environment: "dev",
						// The cluster this box is, which is what keys the escrow. Not
						// metadata.name: that is the singleton "hub-environment" on every
						// box, and using it put the master keys at one constant escrow
						// path and looked for the admin kubeconfig at a secret CAPI never
						// creates.
						ClusterName: "test-hub",
						// database is a required struct; omitting it leaves the reconciler
						// addressing objects with an empty namespace, which the API server
						// rejects on create.
						Database: opsv1alpha1.DatabaseConfig{
							ClusterRef: "infisical-db",
							Namespace:  "default",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &opsv1alpha1.HubEnvironment{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance HubEnvironment")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		// The escrow is not optional, and this is the assertion that says so.
		//
		// It read "should successfully reconcile the resource" and asserted only
		// that Reconcile returned no error -- which it did, on a box with no
		// escrow, by logging that fact at Info and continuing with a nil client.
		// So the test passed for exactly the configuration in which the Infisical
		// master keys are generated, used, and kept nowhere but inside the cluster
		// they decrypt. envtest configures no escrow, so the refusal is what this
		// environment must produce.
		It("refuses to generate master keys when there is no escrow", func() {
			// ISOLATED FROM THE CHECKOUT, and this is load-bearing.
			//
			// The escrow settings are read from the environment and, failing that,
			// from `k8-secrets/infisical/` in the checkout -- which is what lets an
			// operator run the CLI without exporting four values. This test runs
			// inside the checkout, so without this it finds the REAL escrow
			// credentials of whoever ran it: the reconcile then succeeds, the
			// assertion below fails, and a developer is one mistake from a test
			// that writes to the live escrow.
			//
			// Chdir rather than clearing the environment, because the environment is
			// not where the value comes from.
			prevWd, err := os.Getwd()
			Expect(err).NotTo(HaveOccurred())
			Expect(os.Chdir(GinkgoT().TempDir())).To(Succeed())
			defer func() { Expect(os.Chdir(prevWd)).To(Succeed()) }()
			for _, k := range []string{
				"INFISICAL_ESCROW_URL", "INFISICAL_ESCROW_PROJECT_ID",
				"INFISICAL_ESCROW_CLIENT_ID", "INFISICAL_ESCROW_CLIENT_SECRET",
			} {
				GinkgoT().Setenv(k, "")
			}

			By("Reconciling the created resource")
			controllerReconciler := &HubEnvironmentReconciler{
				Client: k8sClient,
				// Reconcile reads operational secrets through UncachedClient; left nil
				// by the scaffold it nil-derefs before reaching any logic. envtest has
				// no cache to bypass, so the same client serves both roles.
				UncachedClient: k8sClient,
				Scheme:         k8sClient.Scheme(),
			}

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("escrow"))
		})
	})
})
