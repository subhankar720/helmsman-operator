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

package access

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	accessv1alpha1 "github.com/subhankar720/helmsman-operator/api/access/v1alpha1"
)

var _ = Describe("AccessGrant Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		const rbName = "helmsman-ag-" + resourceName

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		rbKey := types.NamespacedName{Name: rbName, Namespace: "default"}

		var controllerReconciler *AccessGrantReconciler

		reconcileOnce := func() {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
		}

		BeforeEach(func() {
			controllerReconciler = &AccessGrantReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("creating the custom resource for the Kind AccessGrant")
			err := k8sClient.Get(ctx, typeNamespacedName, &accessv1alpha1.AccessGrant{})
			if err != nil && errors.IsNotFound(err) {
				resource := &accessv1alpha1.AccessGrant{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					// A valid spec: every field the CRD marks as required.
					Spec: accessv1alpha1.AccessGrantSpec{
						Subject:     "user:test@example.com",
						Cluster:     "helmsman-onprem",
						Namespace:   "default",
						Permission:  accessv1alpha1.PermissionReadOnly,
						TTL:         "1h",
						Environment: "dev",
						Reason:      "envtest",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &accessv1alpha1.AccessGrant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())

			By("Cleanup the specific resource instance AccessGrant")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			// Let the controller run its finalizer so the object is really gone.
			reconcileOnce()
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &accessv1alpha1.AccessGrant{}))
			}).Should(BeTrue())
			_ = k8sClient.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: rbName, Namespace: "default"}})
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			reconcileOnce()
		})

		It("should activate the grant and create a RoleBinding to the reader role", func() {
			reconcileOnce() // adds the finalizer
			reconcileOnce() // validates policy, creates the RoleBinding, sets Active

			ag := &accessv1alpha1.AccessGrant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, ag)).To(Succeed())
			Expect(ag.Status.State).To(Equal(accessv1alpha1.StateActive))
			Expect(ag.Status.RoleBindingRef).To(Equal(rbName))
			Expect(ag.Status.ExpiresAt).NotTo(BeNil())

			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, rbKey, rb)).To(Succeed())
			Expect(rb.RoleRef.Name).To(Equal(roleReader))
			Expect(rb.Subjects).To(HaveLen(1))
			Expect(rb.Subjects[0].Kind).To(Equal("User"))
			Expect(rb.Subjects[0].Name).To(Equal("test@example.com"))
		})

		It("should not recreate the RoleBinding once the grant is Expired", func() {
			reconcileOnce() // adds the finalizer

			ag := &accessv1alpha1.AccessGrant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, ag)).To(Succeed())
			ag.Status.State = accessv1alpha1.StateExpired
			Expect(k8sClient.Status().Update(ctx, ag)).To(Succeed())

			reconcileOnce()

			Expect(errors.IsNotFound(k8sClient.Get(ctx, rbKey, &rbacv1.RoleBinding{}))).To(BeTrue())
			Expect(k8sClient.Get(ctx, typeNamespacedName, ag)).To(Succeed())
			Expect(ag.Status.State).To(Equal(accessv1alpha1.StateExpired))
		})
	})

	Context("When the subject is longer than a label value may be", func() {
		const resourceName = "long-subject"
		const rbName = "helmsman-ag-" + resourceName
		// 5 + 45 + 24 = 74 characters: too long for a label value as is.
		longEmail := strings.Repeat("a", 45) + "@example-corporation.example"
		longSubject := "user:" + longEmail

		ctx := context.Background()
		key := types.NamespacedName{Name: resourceName, Namespace: "default"}
		rbKey := types.NamespacedName{Name: rbName, Namespace: "default"}
		var controllerReconciler *AccessGrantReconciler

		reconcileOnce := func() {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}

		BeforeEach(func() {
			Expect(len(longSubject)).To(BeNumerically(">=", 70))
			controllerReconciler = &AccessGrantReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(k8sClient.Create(ctx, &accessv1alpha1.AccessGrant{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
				Spec: accessv1alpha1.AccessGrantSpec{
					Subject:     longSubject,
					Cluster:     "helmsman-onprem",
					Namespace:   "default",
					Permission:  accessv1alpha1.PermissionReadOnly,
					TTL:         "1h",
					Environment: "dev",
					Reason:      "envtest: 70+ character subject",
				},
			})).To(Succeed())
		})

		AfterEach(func() {
			ag := &accessv1alpha1.AccessGrant{}
			Expect(k8sClient.Get(ctx, key, ag)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ag)).To(Succeed())
			reconcileOnce()
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, key, &accessv1alpha1.AccessGrant{}))
			}).Should(BeTrue())
			_ = k8sClient.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: rbName, Namespace: "default"}})
		})

		It("should activate the grant and create its RoleBinding with a valid subject label", func() {
			reconcileOnce() // adds the finalizer
			reconcileOnce() // creates the RoleBinding, sets Active

			ag := &accessv1alpha1.AccessGrant{}
			Expect(k8sClient.Get(ctx, key, ag)).To(Succeed())
			Expect(ag.Status.State).To(Equal(accessv1alpha1.StateActive))

			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, rbKey, rb)).To(Succeed())
			Expect(rb.Subjects).To(HaveLen(1))
			Expect(rb.Subjects[0].Name).To(Equal(longEmail)) // the binding itself uses the full name
			label := rb.Labels["access.helmsman.dev/subject"]
			Expect(len(label)).To(BeNumerically("<=", 63))
			Expect(validation.IsValidLabelValue(label)).To(BeEmpty())
			Expect(label).To(HavePrefix("user-aaaaaaaa"))
		})
	})
})
