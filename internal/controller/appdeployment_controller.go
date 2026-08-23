package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/subhankar720/helmsman-operator/api/v1alpha1"
)

// finalizerName is the finalizer we place on every AppDeployment.
// When a CR is deleted, the controller runs cleanup (Keycloak client deletion)
// before allowing Kubernetes to garbage-collect the CR itself.
const finalizerName = "platform.helmsman.dev/finalizer"

// platformConfigSecretName is the Secret the operator reads for Keycloak/Vault URLs.
// Must be created in each app namespace (same as helmsman-platform-config).
const platformConfigSecretName = "helmsman-platform-config"

// AppDeploymentReconciler reconciles AppDeployment objects.
// It holds a Kubernetes client (to create/update/delete resources)
// and the scheme (to convert between Go types and Kubernetes API objects).
type AppDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// platformConfig holds the values read from helmsman-platform-config Secret.
type platformConfig struct {
	KeycloakURL       string
	KeycloakOIDCURL   string
	KeycloakAdminUser string
	KeycloakAdminPass string
	KeycloakRealm     string
	VaultURL          string
	VaultToken        string
}

// RBAC markers — kubebuilder reads these and generates the ClusterRole YAML.
// Every resource the operator creates or reads needs a corresponding marker.
//
//+kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is called by controller-runtime whenever:
// - An AppDeployment CR is created, updated, or deleted
// - A resource owned by an AppDeployment changes (via ownership references)
// - The requeue timer fires after a previous error
//
// The function must be idempotent — it may be called many times for the
// same desired state, and the result must always be the same.
func (r *AppDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling AppDeployment", "name", req.Name, "namespace", req.Namespace)

	// ── Step 1: Fetch the AppDeployment CR ───────────────────────────────────
	// If it doesn't exist (e.g. already deleted), IgnoreNotFound returns nil
	// so we don't return an error and requeue unnecessarily.
	var appDep platformv1alpha1.AppDeployment
	if err := r.Get(ctx, req.NamespacedName, &appDep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// ── Step 2: Handle deletion ───────────────────────────────────────────────
	// DeletionTimestamp is set by Kubernetes when someone deletes the CR.
	// We must process our finalizer before the CR can be fully removed.
	if !appDep.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &appDep)
	}

	// ── Step 3: Add finalizer if not present ──────────────────────────────────
	// The finalizer prevents Kubernetes from deleting the CR until we've
	// completed our cleanup (Keycloak client deletion).
	if !controllerutil.ContainsFinalizer(&appDep, finalizerName) {
		logger.Info("Adding finalizer", "finalizer", finalizerName)
		controllerutil.AddFinalizer(&appDep, finalizerName)
		if err := r.Update(ctx, &appDep); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		// Requeue so we continue reconciliation with the updated object
		return ctrl.Result{Requeue: true}, nil
	}

	// ── Step 4: Read platform configuration ──────────────────────────────────
	// The operator needs to know where Keycloak and Vault are.
	// This information lives in the helmsman-platform-config Secret
	// in the same namespace as the AppDeployment.
	cfg, err := r.getPlatformConfig(ctx, appDep.Namespace)
	if err != nil {
		logger.Error(err, "Failed to read platform config")
		r.setCondition(&appDep, platformv1alpha1.ConditionReady,
			metav1.ConditionFalse, "PlatformConfigMissing", err.Error())
		_ = r.Status().Update(ctx, &appDep)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// ── Step 5: Register OIDC client (if enabled) ─────────────────────────────
	// This mirrors what the PreSync Job does, but now owned by the controller.
	// The controller retries on failure (RequeueAfter) rather than failing the
	// entire deployment like a failed Job would.
	if appDep.Spec.OIDC.Enabled {
		creds, err := registerKeycloakClient(ctx, appDep.Spec.AppName, cfg)
		if err != nil {
			logger.Error(err, "Failed to register Keycloak client")
			r.setCondition(&appDep, platformv1alpha1.ConditionOIDCRegistered,
				metav1.ConditionFalse, "RegistrationFailed", err.Error())
			_ = r.Status().Update(ctx, &appDep)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// Write credentials to Vault — ESO syncs them into the K8s Secret
		if err := writeOIDCCredsToVault(ctx, appDep.Spec.AppName, creds, cfg); err != nil {
			logger.Error(err, "Failed to write OIDC credentials to Vault")
			r.setCondition(&appDep, platformv1alpha1.ConditionOIDCRegistered,
				metav1.ConditionFalse, "VaultWriteFailed", err.Error())
			_ = r.Status().Update(ctx, &appDep)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		r.setCondition(&appDep, platformv1alpha1.ConditionOIDCRegistered,
			metav1.ConditionTrue, "Registered", "Keycloak client registered and credentials written to Vault")
		appDep.Status.OIDCClientID = creds.ClientID
		logger.Info("Keycloak client registered and Vault written", "clientID", creds.ClientID)
	}

	// ── Step 6: Ensure ServiceAccount ─────────────────────────────────────────
	if err := r.ensureServiceAccount(ctx, &appDep); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	// ── Step 7: Ensure NetworkPolicies ────────────────────────────────────────
	if err := r.ensureNetworkPolicies(ctx, &appDep); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	// ── Step 8: Ensure Fluent Bit ConfigMap ──────────────────────────────────
	if err := r.ensureFluentBitConfigMap(ctx, &appDep); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	// ── Step 9: Ensure Services ───────────────────────────────────────────────
	if err := r.ensureServices(ctx, &appDep); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	// ── Step 10: Ensure ExternalSecret (OIDC credentials from Vault) ───────────
	if appDep.Spec.OIDC.Enabled {
		if err := r.ensureExternalSecret(ctx, &appDep); err != nil {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, err
		}
	}

	// ── Step 11: Ensure StatefulSet ───────────────────────────────────────────
	if err := r.ensureStatefulSet(ctx, &appDep); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	// ── Step 11: Update status ────────────────────────────────────────────────
	r.setCondition(&appDep, platformv1alpha1.ConditionWorkloadCreated,
		metav1.ConditionTrue, "Created", "All workload resources are present")
	r.setCondition(&appDep, platformv1alpha1.ConditionReady,
		metav1.ConditionTrue, "Reconciled", "AppDeployment reconciled successfully")
	appDep.Status.ObservedGeneration = appDep.Generation

	if err := r.Status().Update(ctx, &appDep); err != nil {
		// Status update failures are common when the object was just modified.
		// We log and requeue rather than returning an error that would
		// cause exponential backoff.
		logger.Error(err, "Failed to update status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	logger.Info("AppDeployment reconciled successfully", "app", appDep.Spec.AppName)
	return ctrl.Result{}, nil
}

// handleDeletion processes the finalizer and performs cleanup.
// Called when DeletionTimestamp is set on the AppDeployment.
func (r *AppDeploymentReconciler) handleDeletion(ctx context.Context, appDep *platformv1alpha1.AppDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(appDep, finalizerName) {
		// Finalizer already removed, nothing to do
		return ctrl.Result{}, nil
	}

	logger.Info("Running cleanup for deleted AppDeployment", "app", appDep.Spec.AppName)

	// Clean up the Keycloak client if OIDC was enabled.
	// If this fails, we log the error but still remove the finalizer —
	// we don't want a Keycloak outage to permanently block CR deletion.
	if appDep.Spec.OIDC.Enabled {
		cfg, err := r.getPlatformConfig(ctx, appDep.Namespace)
		if err != nil {
			logger.Error(err, "Could not read platform config during cleanup — skipping Keycloak deletion")
		} else {
			if err := deleteKeycloakClient(ctx, appDep.Spec.AppName, cfg); err != nil {
				logger.Error(err, "Failed to delete Keycloak client — continuing with finalizer removal")
			} else {
				logger.Info("Keycloak client deleted", "clientID", appDep.Spec.AppName)
			}
		}
	}

	// Remove the finalizer — Kubernetes can now garbage-collect the CR
	// and all owned resources (StatefulSet, Services, etc.) via owner references.
	controllerutil.RemoveFinalizer(appDep, finalizerName)
	if err := r.Update(ctx, appDep); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

// getPlatformConfig reads the helmsman-platform-config Secret from the
// given namespace and returns its values as a structured config.
func (r *AppDeploymentReconciler) getPlatformConfig(ctx context.Context, namespace string) (*platformConfig, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Name: platformConfigSecretName, Namespace: namespace}
	if err := r.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("secret %s/%s not found: %w", namespace, platformConfigSecretName, err)
	}

	get := func(k string) string { return string(secret.Data[k]) }

	cfg := &platformConfig{
		KeycloakURL:       get("keycloak-url"),
		KeycloakOIDCURL:   get("keycloak-oidc-url"),
		KeycloakAdminUser: get("keycloak-admin-user"),
		KeycloakAdminPass: get("keycloak-admin-password"),
		KeycloakRealm:     get("keycloak-realm"),
		VaultURL:          get("vault-url"),
		VaultToken:        get("vault-token"),
	}

	if cfg.KeycloakURL == "" || cfg.VaultURL == "" {
		cfg.KeycloakOIDCURL = cfg.KeycloakURL
		return nil, fmt.Errorf("platform config Secret is missing required fields")
	}

	return cfg, nil
}

// setCondition updates or appends a condition in the AppDeployment's status.
// Uses metav1.Condition which includes LastTransitionTime and ObservedGeneration.
func (r *AppDeploymentReconciler) setCondition(
	appDep *platformv1alpha1.AppDeployment,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	now := metav1.Now()
	for i, c := range appDep.Status.Conditions {
		if c.Type == condType {
			if c.Status != status || c.Reason != reason {
				appDep.Status.Conditions[i].Status = status
				appDep.Status.Conditions[i].Reason = reason
				appDep.Status.Conditions[i].Message = message
				appDep.Status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}
	// Condition not found — append new one
	appDep.Status.Conditions = append(appDep.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: appDep.Generation,
	})
}

// ensureExternalSecret creates or updates an ExternalSecret that tells ESO
// to sync OIDC credentials from Vault into a Kubernetes Secret.
// We use the unstructured client because the ESO CRD types aren't
// imported as a Go dependency — this keeps our dependency tree small.
func (r *AppDeploymentReconciler) ensureExternalSecret(ctx context.Context, appDep *platformv1alpha1.AppDeployment) error {
	gvk := schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1beta1",
		Kind:    "ExternalSecret",
	}

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(gvk)
	desired.SetName(appDep.Spec.AppName + "-oidc")
	desired.SetNamespace(appDep.Namespace)
	desired.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": "helmsman-operator",
		"app.kubernetes.io/name":       appDep.Spec.AppName,
	})

	// Set owner reference so the ExternalSecret is deleted when the CR is deleted
	if err := controllerutil.SetControllerReference(appDep, desired, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on ExternalSecret: %w", err)
	}

	spec := map[string]interface{}{
		"refreshInterval": "1h",
		"secretStoreRef": map[string]interface{}{
			"name": "vault-backend",
			"kind": "ClusterSecretStore",
		},
		"target": map[string]interface{}{
			"name":           appDep.Spec.AppName + "-oidc",
			"creationPolicy": "Owner",
		},
		"data": []interface{}{
			map[string]interface{}{
				"secretKey": "client-id",
				"remoteRef": map[string]interface{}{
					"key":      "apps/" + appDep.Spec.AppName + "/oidc",
					"property": "client-id",
				},
			},
			map[string]interface{}{
				"secretKey": "client-secret",
				"remoteRef": map[string]interface{}{
					"key":      "apps/" + appDep.Spec.AppName + "/oidc",
					"property": "client-secret",
				},
			},
			map[string]interface{}{
				"secretKey": "issuer-url",
				"remoteRef": map[string]interface{}{
					"key":      "apps/" + appDep.Spec.AppName + "/oidc",
					"property": "issuer-url",
				},
			},
			map[string]interface{}{
				"secretKey": "cookie-secret",
				"remoteRef": map[string]interface{}{
					"key":      "apps/" + appDep.Spec.AppName + "/oidc",
					"property": "cookie-secret",
				},
			},
		},
	}
	if err := unstructured.SetNestedMap(desired.Object, spec, "spec"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	err := r.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}, existing)

	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}

	// Update spec if it exists
	existing.Object["spec"] = desired.Object["spec"]
	return r.Update(ctx, existing)
}

// SetupWithManager registers the controller with the manager and declares
// which resource types trigger reconciliation.
func (r *AppDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.AppDeployment{}).
		// Watch owned resources — if someone deletes a Service the operator
		// created, the controller is notified and recreates it (self-healing).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
