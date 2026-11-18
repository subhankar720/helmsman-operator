package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/subhankar720/helmsman-operator/api/v1alpha1"
)

// externalSecretGVK is the ESO API the platform serves (storage version).
var externalSecretGVK = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1beta1", Kind: "ExternalSecret"}

// clusterSecretStoreName is the platform's ClusterSecretStore backed by Vault.
const clusterSecretStoreName = "vault-backend"

// oidcSecretKeys are exactly the keys the adc (oauth2-proxy) sidecar reads from
// <app>-oidc (see buildADCContainer). They are also the properties
// writeOIDCCredsToVault stores at secret/apps/<app>/oidc.
var oidcSecretKeys = []string{"client-id", "client-secret", "issuer-url", "cookie-secret"}

// oidcSecretName is the Secret (and ExternalSecret) name the pod spec uses.
func oidcSecretName(appDep *platformv1alpha1.AppDeployment) string {
	return appDep.Spec.AppName + "-oidc"
}

// desiredOIDCExternalSecret builds the ExternalSecret that materialises <app>-oidc
// from secret/apps/<app>/oidc through the platform ClusterSecretStore.
func desiredOIDCExternalSecret(appDep *platformv1alpha1.AppDeployment) *unstructured.Unstructured {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	es.SetName(oidcSecretName(appDep))
	es.SetNamespace(appDep.Namespace)
	es.SetLabels(commonLabels(appDep))

	data := make([]interface{}, 0, len(oidcSecretKeys))
	for _, k := range oidcSecretKeys {
		data = append(data, map[string]interface{}{
			"secretKey": k,
			"remoteRef": map[string]interface{}{
				"key":      "apps/" + appDep.Spec.AppName + "/oidc",
				"property": k,
			},
		})
	}
	es.Object["spec"] = map[string]interface{}{
		"refreshInterval": "1h",
		"secretStoreRef":  map[string]interface{}{"kind": "ClusterSecretStore", "name": clusterSecretStoreName},
		"target": map[string]interface{}{
			"name":           oidcSecretName(appDep),
			"creationPolicy": "Owner", // the Secret is owned by the ExternalSecret and goes with it
		},
		"data": data,
	}
	return es
}

// ensureOIDCExternalSecret creates the <app>-oidc ExternalSecret, owned by the
// AppDeployment. It never takes over objects it did not create: if an
// ExternalSecret of that name exists and is not controlled by this
// AppDeployment (sample-app: Argo CD's chart), or a Secret of that name already
// exists without one, it skips and reports why. It returns (skipped reason, error).
func (r *AppDeploymentReconciler) ensureOIDCExternalSecret(ctx context.Context, appDep *platformv1alpha1.AppDeployment) (string, error) {
	logger := log.FromContext(ctx)
	name := oidcSecretName(appDep)
	key := types.NamespacedName{Name: name, Namespace: appDep.Namespace}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(externalSecretGVK)
	err := r.Get(ctx, key, existing)
	switch {
	case err == nil:
		if metav1.IsControlledBy(existing, appDep) {
			return "", nil // ours, already there
		}
		return fmt.Sprintf("ExternalSecret %s/%s exists and is not owned by this AppDeployment (managed elsewhere, e.g. Argo CD); left unchanged", appDep.Namespace, name), nil
	case !errors.IsNotFound(err):
		return "", fmt.Errorf("failed to read ExternalSecret %s: %w", name, err)
	}

	// No ExternalSecret. A Secret of that name that we do not own means someone
	// else provides it: do not create a competing ExternalSecret.
	var secret corev1.Secret
	if err := r.Get(ctx, key, &secret); err == nil {
		return fmt.Sprintf("Secret %s/%s already exists without an operator-owned ExternalSecret; left unchanged", appDep.Namespace, name), nil
	} else if !errors.IsNotFound(err) {
		return "", fmt.Errorf("failed to read Secret %s: %w", name, err)
	}

	desired := desiredOIDCExternalSecret(appDep)
	if err := controllerutil.SetControllerReference(appDep, desired, r.Scheme); err != nil {
		return "", fmt.Errorf("failed to set owner reference on ExternalSecret: %w", err)
	}
	logger.Info("Creating ExternalSecret", "name", name, "keys", oidcSecretKeys)
	if err := r.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("failed to create ExternalSecret %s: %w", name, err)
	}
	return "", nil
}
