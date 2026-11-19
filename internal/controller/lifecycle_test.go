package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/subhankar720/helmsman-operator/api/v1alpha1"
)

// fakePlatform is an in-memory Keycloak admin API + Vault KV v2 with failure injection.
type fakePlatform struct {
	mu                 sync.Mutex
	clients            map[string]bool // clientId -> exists
	vault              map[string]bool // app -> secret/apps/<app>/oidc exists
	failKeycloakDelete bool
	failVaultDelete    bool
	srv                *httptest.Server
}

func newFakePlatform() *fakePlatform {
	f := &fakePlatform{clients: map[string]bool{}, vault: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *fakePlatform) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/realms/master/protocol/openid-connect/token":
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "fake"})
	case r.Method == http.MethodGet && p == "/admin/realms/helmsman/clients":
		id := r.URL.Query().Get("clientId")
		if f.clients[id] {
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "id-" + id}})
		} else {
			_, _ = w.Write([]byte("[]"))
		}
	case r.Method == http.MethodPost && p == "/admin/realms/helmsman/clients":
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id, _ := body["clientId"].(string)
		f.clients[id] = true
		w.Header().Set("Location", f.srv.URL+"/admin/realms/helmsman/clients/id-"+id)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/client-secret"):
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "fake-client-secret"})
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/admin/realms/helmsman/clients/id-"):
		if f.failKeycloakDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		delete(f.clients, strings.TrimPrefix(p, "/admin/realms/helmsman/clients/id-"))
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(p, "/v1/secret/data/apps/") && strings.HasSuffix(p, "/oidc"):
		app := strings.TrimSuffix(strings.TrimPrefix(p, "/v1/secret/data/apps/"), "/oidc")
		if r.Method == http.MethodPost {
			f.vault[app] = true
			w.WriteHeader(http.StatusOK)
			return
		}
		if !f.vault[app] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"data":{"cookie-secret":"fake-cookie"}}}`))
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/v1/secret/metadata/apps/"):
		if f.failVaultDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		delete(f.vault, strings.TrimSuffix(strings.TrimPrefix(p, "/v1/secret/metadata/apps/"), "/oidc"))
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakePlatform) has(app string) (client, vault bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients[app], f.vault[app]
}

func (f *fakePlatform) set(kcFail, vaultFail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failKeycloakDelete, f.failVaultDelete = kcFail, vaultFail
}

// configSecret is a helmsman-platform-config pointing at the fake platform (test values only).
func (f *fakePlatform) configSecret(ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: platformConfigSecretName, Namespace: ns},
		StringData: map[string]string{
			"keycloak-url": f.srv.URL, "keycloak-oidc-url": f.srv.URL, "keycloak-realm": "helmsman",
			"keycloak-admin-user": "test-admin", "keycloak-admin-password": "test-only",
			"vault-url": f.srv.URL, "vault-token": "test-only", "cluster-name": "envtest",
		},
	}
}

func newNamespace(ctx context.Context, name string) {
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
}

func oidcAppDeployment(ns, app string) *platformv1alpha1.AppDeployment {
	return &platformv1alpha1.AppDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: app, Namespace: ns},
		Spec: platformv1alpha1.AppDeploymentSpec{
			AppName:    app,
			OwningTeam: "platform-team",
			Image:      platformv1alpha1.ImageSpec{Repository: "example.com/" + app, Tag: "v1"},
			Container:  platformv1alpha1.ContainerSpec{Port: 8080},
			OIDC:       platformv1alpha1.OIDCSpec{Enabled: true},
			Replicas:   1,
		},
	}
}

// drainEvents returns every event the fake recorder has buffered.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

var _ = Describe("AppDeployment lifecycle (WO-1.1c)", func() {
	var (
		ctx  context.Context
		fake *fakePlatform
		rec  *record.FakeRecorder
		r    *AppDeploymentReconciler
		seq  int
	)

	BeforeEach(func() {
		ctx = context.Background()
		fake = newFakePlatform()
		rec = record.NewFakeRecorder(100)
		seq++
		opNS := fmt.Sprintf("opns-%d", seq)
		newNamespace(ctx, opNS)
		r = &AppDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: rec, OperatorNamespace: opNS}
	})
	AfterEach(func() { fake.srv.Close() })

	reconcileApp := func(ns, app string) error {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: app}})
		return err
	}
	createAndReconcile := func(ns, app string) *platformv1alpha1.AppDeployment {
		ad := oidcAppDeployment(ns, app)
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		Expect(reconcileApp(ns, app)).To(Succeed()) // adds the finalizer
		Expect(reconcileApp(ns, app)).To(Succeed()) // creates everything
		return ad
	}
	gone := func(ns, app string) bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: app}, &platformv1alpha1.AppDeployment{}))
	}

	It("labels every object managed-by=helmsman, keeps selectors on the name only, and creates the OIDC ExternalSecret", func() {
		ns, app := "lc-labels", "lc-labels"
		newNamespace(ctx, ns)
		Expect(k8sClient.Create(ctx, fake.configSecret(ns))).To(Succeed())
		createAndReconcile(ns, app)

		objs := []client.Object{&corev1.ServiceAccount{}, &corev1.ConfigMap{}, &corev1.Service{}, &corev1.Service{}, &appsv1.StatefulSet{}}
		names := []string{app, app + "-fluent-bit-config", app, app + "-headless", app}
		for _, np := range []string{"-default-deny", "-allow-ingress", "-allow-dns", "-allow-keycloak-egress", "-allow-vault-egress"} {
			objs = append(objs, &networkingv1.NetworkPolicy{})
			names = append(names, app+np)
		}
		for i, o := range objs {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: names[i]}, o)).To(Succeed(), names[i])
			Expect(o.GetLabels()).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "helmsman"), names[i])
			Expect(o.GetLabels()).To(HaveKeyWithValue("app.kubernetes.io/name", app), names[i])
		}
		kcNP := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: app + "-allow-keycloak-egress"}, kcNP)).To(Succeed())
		Expect(kcNP.Spec.Egress[0].Ports[0].Port.IntVal).To(Equal(keycloakEgressPort(&platformConfig{KeycloakURL: fake.srv.URL})))
		Expect(kcNP.Spec.Egress[0].Ports[0].Port.IntVal).NotTo(Equal(int32(80)), "fake Keycloak listens on a random port, not 80")

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: app}, sts)).To(Succeed())
		Expect(sts.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "helmsman"))
		Expect(sts.Spec.Selector.MatchLabels).To(Equal(map[string]string{"app.kubernetes.io/name": app}))

		es := &unstructured.Unstructured{}
		es.SetGroupVersionKind(externalSecretGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: app + "-oidc"}, es)).To(Succeed())
		Expect(es.GetLabels()).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "helmsman"))
		owner := metav1.GetControllerOf(es)
		Expect(owner).NotTo(BeNil())
		Expect(owner.Kind).To(Equal("AppDeployment"))
		Expect(owner.Name).To(Equal(app))
		store, _, _ := unstructured.NestedString(es.Object, "spec", "secretStoreRef", "name")
		Expect(store).To(Equal("vault-backend"))
		target, _, _ := unstructured.NestedString(es.Object, "spec", "target", "name")
		Expect(target).To(Equal(app + "-oidc"))
		data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
		var keys []string
		for _, d := range data {
			m := d.(map[string]interface{})
			keys = append(keys, m["secretKey"].(string))
			ref := m["remoteRef"].(map[string]interface{})
			Expect(ref["key"]).To(Equal("apps/" + app + "/oidc"))
			Expect(ref["property"]).To(Equal(m["secretKey"]))
		}
		// exactly the keys the adc sidecar reads from <app>-oidc
		var adcKeys []string
		for _, e := range buildADCContainer(oidcAppDeployment(ns, app)).Env {
			adcKeys = append(adcKeys, e.ValueFrom.SecretKeyRef.Key)
		}
		Expect(keys).To(ConsistOf(adcKeys))

		kc, v := fake.has(app)
		Expect(kc).To(BeTrue())
		Expect(v).To(BeTrue())
	})

	It("deletes the Keycloak client and then the Vault credentials, and releases the finalizer", func() {
		ns, app := "lc-delete", "lc-delete"
		newNamespace(ctx, ns)
		Expect(k8sClient.Create(ctx, fake.configSecret(ns))).To(Succeed())
		ad := createAndReconcile(ns, app)
		drainEvents(rec)

		Expect(k8sClient.Delete(ctx, ad)).To(Succeed())
		Expect(reconcileApp(ns, app)).To(Succeed())
		kc, v := fake.has(app)
		Expect(kc).To(BeFalse())
		Expect(v).To(BeFalse())
		Expect(gone(ns, app)).To(BeTrue())
		Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("Normal CleanupSucceeded")))
	})

	It("counts not-found as success (idempotent delete)", func() {
		cfg := &platformConfig{KeycloakURL: fake.srv.URL, KeycloakRealm: "helmsman", VaultURL: fake.srv.URL}
		Expect(deleteKeycloakClient(ctx, "never-existed", cfg)).To(Succeed())
		Expect(deleteOIDCCredsFromVault(ctx, "never-existed", cfg)).To(Succeed())
	})

	It("keeps the finalizer, emits a Warning and a Cleanup condition when a step fails, and completes after recovery", func() {
		ns, app := "lc-failing", "lc-failing"
		newNamespace(ctx, ns)
		Expect(k8sClient.Create(ctx, fake.configSecret(ns))).To(Succeed())
		ad := createAndReconcile(ns, app)
		drainEvents(rec)

		By("Keycloak delete fails")
		fake.set(true, false)
		Expect(k8sClient.Delete(ctx, ad)).To(Succeed())
		err := reconcileApp(ns, app)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("KeycloakDeleteFailed"))
		cur := &platformv1alpha1.AppDeployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: app}, cur)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(cur, finalizerName)).To(BeTrue())
		Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("Warning KeycloakDeleteFailed")))
		Expect(cur.Status.Conditions).To(ContainElement(SatisfyAll(
			HaveField("Type", conditionCleanup), HaveField("Status", metav1.ConditionFalse), HaveField("Reason", "KeycloakDeleteFailed"))))
		_, v := fake.has(app)
		Expect(v).To(BeTrue(), "Vault must not be deleted before the Keycloak client is gone")

		By("Keycloak recovers, Vault delete fails")
		fake.set(false, true)
		err = reconcileApp(ns, app)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("VaultDeleteFailed"))
		Expect(gone(ns, app)).To(BeFalse())
		Expect(drainEvents(rec)).To(ContainElement(ContainSubstring("Warning VaultDeleteFailed")))

		By("everything recovers")
		fake.set(false, false)
		Expect(reconcileApp(ns, app)).To(Succeed())
		Expect(gone(ns, app)).To(BeTrue())
		kc, v := fake.has(app)
		Expect(kc).To(BeFalse())
		Expect(v).To(BeFalse())
	})

	It("falls back to the operator namespace's config for create and for cleanup", func() {
		ns, app := "lc-fallback", "lc-fallback"
		newNamespace(ctx, ns) // no config Secret here
		Expect(k8sClient.Create(ctx, fake.configSecret(r.OperatorNamespace))).To(Succeed())
		ad := createAndReconcile(ns, app)
		kc, v := fake.has(app)
		Expect(kc).To(BeTrue())
		Expect(v).To(BeTrue())
		drainEvents(rec)

		Expect(k8sClient.Delete(ctx, ad)).To(Succeed())
		Expect(reconcileApp(ns, app)).To(Succeed())
		Expect(gone(ns, app)).To(BeTrue())
		kc, v = fake.has(app)
		Expect(kc).To(BeFalse())
		Expect(v).To(BeFalse())
		Expect(drainEvents(rec)).To(ContainElement(SatisfyAll(ContainSubstring("Normal CleanupSucceeded"), ContainSubstring(r.OperatorNamespace))))
	})

	It("releases the finalizer with a Warning naming the leak when no config exists anywhere", func() {
		ns, app := "lc-noconfig", "lc-noconfig"
		newNamespace(ctx, ns)
		ad := oidcAppDeployment(ns, app)
		controllerutil.AddFinalizer(ad, finalizerName)
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		Expect(k8sClient.Delete(ctx, ad)).To(Succeed())
		Expect(reconcileApp(ns, app)).To(Succeed())
		Expect(gone(ns, app)).To(BeTrue())
		Expect(drainEvents(rec)).To(ContainElement(SatisfyAll(
			ContainSubstring("Warning CleanupSkipped"), ContainSubstring(`Keycloak client "lc-noconfig"`),
			ContainSubstring("secret/apps/lc-noconfig/oidc"))))
	})

	It("never modifies objects it did not create (the sample-app hybrid)", func() {
		ns, app := "lc-hybrid", "lc-hybrid"
		newNamespace(ctx, ns)
		Expect(k8sClient.Create(ctx, fake.configSecret(ns))).To(Succeed())

		By("pre-existing Argo-style objects: StatefulSet, ExternalSecret (not owned by the AppDeployment)")
		replicas := int32(1)
		argoLabels := map[string]string{"app.kubernetes.io/name": app, "app.kubernetes.io/managed-by": "helmsman", "app.kubernetes.io/instance": "argo-app"}
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: app, Namespace: ns, Labels: argoLabels},
			Spec: appsv1.StatefulSetSpec{
				Replicas: &replicas, ServiceName: app + "-headless",
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": app}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: argoLabels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.com/argo:v1"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sts)).To(Succeed())
		foreignES := desiredOIDCExternalSecret(oidcAppDeployment(ns, app))
		foreignES.SetLabels(argoLabels)
		Expect(k8sClient.Create(ctx, foreignES)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sts), sts)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreignES), foreignES)).To(Succeed())
		stsRV, esRV := sts.ResourceVersion, foreignES.GetResourceVersion()

		createAndReconcile(ns, app)

		after := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sts), after)).To(Succeed())
		Expect(after.ResourceVersion).To(Equal(stsRV), "existing StatefulSet must not be modified")
		Expect(after.Labels).To(Equal(argoLabels))
		esAfter := &unstructured.Unstructured{}
		esAfter.SetGroupVersionKind(externalSecretGVK)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreignES), esAfter)).To(Succeed())
		Expect(esAfter.GetResourceVersion()).To(Equal(esRV), "foreign ExternalSecret must not be modified")
		Expect(metav1.GetControllerOf(esAfter)).To(BeNil())
	})

	It("does not create an ExternalSecret when a Secret <app>-oidc already exists without one", func() {
		ns, app := "lc-secret-exists", "lc-secret-exists"
		newNamespace(ctx, ns)
		Expect(k8sClient.Create(ctx, fake.configSecret(ns))).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: app + "-oidc", Namespace: ns}})).To(Succeed())
		createAndReconcile(ns, app)
		es := &unstructured.Unstructured{}
		es.SetGroupVersionKind(externalSecretGVK)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: app + "-oidc"}, es))).To(BeTrue())
	})
})
