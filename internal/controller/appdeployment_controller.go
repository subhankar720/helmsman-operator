package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
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
const finalizerName = "platform.helmsman.dev/finalizer"

// platformConfigSecretName is the Secret the operator reads for Keycloak/Vault URLs.
const platformConfigSecretName = "helmsman-platform-config"

// AppDeploymentReconciler reconciles AppDeployment objects.
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
	ClusterName       string // name of this cluster (for status enrichment)
}

//+kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

func (r *AppDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling AppDeployment", "name", req.Name, "namespace", req.Namespace)

	// ── Step 1: Fetch the AppDeployment CR ───────────────────────────────────
	var appDep platformv1alpha1.AppDeployment
	if err := r.Get(ctx, req.NamespacedName, &appDep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// ── Step 2: Handle deletion ───────────────────────────────────────────────
	if !appDep.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &appDep)
	}

	// ── Step 3: Add finalizer if not present ──────────────────────────────────
	if !controllerutil.ContainsFinalizer(&appDep, finalizerName) {
		logger.Info("Adding finalizer", "finalizer", finalizerName)
		controllerutil.AddFinalizer(&appDep, finalizerName)
		if err := r.Update(ctx, &appDep); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// ── Step 4: Read platform configuration ──────────────────────────────────
	cfg, err := r.getPlatformConfig(ctx, appDep.Namespace)
	if err != nil {
		logger.Error(err, "Failed to read platform config")
		r.setCondition(&appDep, platformv1alpha1.ConditionReady,
			metav1.ConditionFalse, "PlatformConfigMissing", err.Error())
		_ = r.Status().Update(ctx, &appDep)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// ── Step 5: Register OIDC client (if enabled) ─────────────────────────────
	if appDep.Spec.OIDC.Enabled {
		creds, err := registerKeycloakClient(ctx, appDep.Spec.AppName, cfg)
		if err != nil {
			logger.Error(err, "Failed to register Keycloak client")
			r.setCondition(&appDep, platformv1alpha1.ConditionOIDCRegistered,
				metav1.ConditionFalse, "RegistrationFailed", err.Error())
			_ = r.Status().Update(ctx, &appDep)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// Write credentials to Vault
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

	// ── Step 10.5: Enrich status with deployment details ────────────────────
	appDep.Status.Namespace = appDep.Namespace
	appDep.Status.Cluster = cfg.ClusterName
	appDep.Status.Endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local", appDep.Spec.AppName, appDep.Namespace)
	appDep.Status.Phase = "Ready"

	// ── Step 10: Ensure StatefulSet ───────────────────────────────────────────
	if err := r.ensureStatefulSet(ctx, &appDep); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	// ── Step 12: Update status ────────────────────────────────────────────────
	r.setCondition(&appDep, platformv1alpha1.ConditionWorkloadCreated,
		metav1.ConditionTrue, "Created", "All workload resources are present")
	r.setCondition(&appDep, platformv1alpha1.ConditionReady,
		metav1.ConditionTrue, "Reconciled", "AppDeployment reconciled successfully")
	appDep.Status.ObservedGeneration = appDep.Generation

	if err := r.Status().Update(ctx, &appDep); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	logger.Info("AppDeployment reconciled successfully", "app", appDep.Spec.AppName)
	return ctrl.Result{}, nil
}

// reconcileOIDCSecret creates/updates the <appName>-oidc Kubernetes secret
// using dynamic URL resolution from platformConfig to handle restarts.
func (r *AppDeploymentReconciler) reconcileOIDCSecret(
	ctx context.Context,
	appDep *platformv1alpha1.AppDeployment,
	cfg *platformConfig,
	clientSecret string,
) error {
	secretName := fmt.Sprintf("%s-oidc", appDep.Spec.AppName)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: appDep.Namespace,
		},
	}

	issuerURL := cfg.KeycloakOIDCURL
	if issuerURL == "" {
		issuerURL = cfg.KeycloakURL
	}
	if cfg.KeycloakRealm != "" && !strings.Contains(issuerURL, "/realms/") {
		issuerURL = fmt.Sprintf("%s/realms/%s", strings.TrimSuffix(issuerURL, "/"), cfg.KeycloakRealm)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}

		secret.Data["issuer-url"] = []byte(issuerURL)
		secret.Data["client-id"] = []byte(appDep.Spec.AppName)

		if clientSecret != "" {
			secret.Data["client-secret"] = []byte(clientSecret)
		}

		if len(secret.Data["cookie-secret"]) != 32 {
			randomCookieSecret := make([]byte, 16)
			if _, err := rand.Read(randomCookieSecret); err != nil {
				hash := sha256.Sum256([]byte(appDep.Spec.AppName + "-cookie-seed"))
				secret.Data["cookie-secret"] = hash[:32]
			} else {
				encoded := hex.EncodeToString(randomCookieSecret)
				secret.Data["cookie-secret"] = []byte(encoded)
			}
		}

		return controllerutil.SetControllerReference(appDep, secret, r.Scheme)
	})

	return err
}

func (r *AppDeploymentReconciler) handleDeletion(ctx context.Context, appDep *platformv1alpha1.AppDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(appDep, finalizerName) {
		return ctrl.Result{}, nil
	}

	logger.Info("Running cleanup for deleted AppDeployment", "app", appDep.Spec.AppName)

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

	controllerutil.RemoveFinalizer(appDep, finalizerName)
	if err := r.Update(ctx, appDep); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

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
		ClusterName:       get("cluster-name"),
	}

	if cfg.KeycloakURL == "" || cfg.VaultURL == "" {
		cfg.KeycloakOIDCURL = cfg.KeycloakURL
		return nil, fmt.Errorf("platform config Secret is missing required fields")
	}

	return cfg, nil
}

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
	appDep.Status.Conditions = append(appDep.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: appDep.Generation,
	})
}

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

	existing.Object["spec"] = desired.Object["spec"]
	return r.Update(ctx, existing)
}

func (r *AppDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.AppDeployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
