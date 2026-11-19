package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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

// defaultOperatorNamespace is where the operator runs on the platform clusters.
const defaultOperatorNamespace = "helmsman-operator-system"

// conditionCleanup reports the state of deletion cleanup (Keycloak, Vault).
const conditionCleanup = "Cleanup"

// errPlatformConfigNotFound means helmsman-platform-config exists in neither
// the AppDeployment's namespace nor the operator's namespace.
var errPlatformConfigNotFound = errors.New("platform config not found")

// AppDeploymentReconciler reconciles AppDeployment objects.
type AppDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Kubernetes Events on the AppDeployment (nil-safe).
	Recorder record.EventRecorder
	// OperatorNamespace is the fallback location of helmsman-platform-config.
	// Empty means: OPERATOR_NAMESPACE, then the pod's service-account
	// namespace, then helmsman-operator-system.
	OperatorNamespace string
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

// +kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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
	cfg, cfgSource, err := r.getPlatformConfig(ctx, appDep.Namespace)
	if err == nil && cfgSource != appDep.Namespace {
		logger.V(1).Info("Using platform config from the operator namespace", "namespace", cfgSource)
	}
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

		// The <app>-oidc Secret the adc sidecar reads, via an ExternalSecret.
		skipped, err := r.ensureOIDCExternalSecret(ctx, &appDep)
		if err != nil {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, err
		}
		if skipped != "" {
			logger.Info("Not creating the OIDC ExternalSecret", "reason", skipped)
		}
	}

	// ── Step 6: Ensure ServiceAccount ─────────────────────────────────────────
	if err := r.ensureServiceAccount(ctx, &appDep); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	// ── Step 7: Ensure NetworkPolicies ────────────────────────────────────────
	if err := r.ensureNetworkPolicies(ctx, &appDep, cfg); err != nil {
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

// handleDeletion removes what the operator created outside the cluster:
// first the Keycloak client, then the Vault credentials. In-cluster objects
// (StatefulSet, Services, ExternalSecret, …) carry ownerReferences and are
// garbage-collected. The finalizer is released only when every step succeeded
// or found nothing to delete. Any other failure keeps the finalizer, records a
// Warning Event and a Cleanup=False condition, and returns the error so the
// request is retried with exponential backoff.
//
// One deliberate exception: if helmsman-platform-config exists neither in the
// AppDeployment's namespace nor in the operator's namespace, cleanup is
// impossible, so the finalizer is released after a Warning Event that names
// exactly what is left behind; otherwise namespace deletion could hang forever.
func (r *AppDeploymentReconciler) handleDeletion(ctx context.Context, appDep *platformv1alpha1.AppDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(appDep, finalizerName) {
		return ctrl.Result{}, nil
	}

	logger.Info("Running cleanup for deleted AppDeployment", "app", appDep.Spec.AppName)
	app := appDep.Spec.AppName

	if appDep.Spec.OIDC.Enabled {
		cfg, cfgSource, err := r.getPlatformConfig(ctx, appDep.Namespace)
		switch {
		case errors.Is(err, errPlatformConfigNotFound):
			msg := fmt.Sprintf("%s not found in namespace %s or %s: cannot clean up; Keycloak client %q (realm unknown) and Vault path secret/apps/%s/oidc are left behind and must be removed by hand",
				platformConfigSecretName, appDep.Namespace, r.operatorNamespace(), app, app)
			logger.Error(err, "Cleanup impossible, releasing the finalizer", "leaks", msg)
			r.event(appDep, corev1.EventTypeWarning, "CleanupSkipped", msg)
		case err != nil:
			return r.cleanupFailed(ctx, appDep, "PlatformConfigUnreadable", err)
		default:
			if cfgSource != appDep.Namespace {
				logger.Info("Cleanup uses platform config from the operator namespace", "namespace", cfgSource)
			}
			if err := deleteKeycloakClient(ctx, app, cfg); err != nil {
				return r.cleanupFailed(ctx, appDep, "KeycloakDeleteFailed", err)
			}
			logger.Info("Keycloak client deleted (or already absent)", "clientID", app)
			if err := deleteOIDCCredsFromVault(ctx, app, cfg); err != nil {
				return r.cleanupFailed(ctx, appDep, "VaultDeleteFailed", err)
			}
			logger.Info("Vault credentials deleted (or already absent)", "path", "secret/apps/"+app+"/oidc")
			r.event(appDep, corev1.EventTypeNormal, "CleanupSucceeded",
				fmt.Sprintf("Keycloak client %q and Vault path secret/apps/%s/oidc removed (config from namespace %s)", app, app, cfgSource))
		}
	}

	controllerutil.RemoveFinalizer(appDep, finalizerName)
	if err := r.Update(ctx, appDep); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// cleanupFailed keeps the finalizer: it records the failure on the object and
// returns the error so controller-runtime retries with exponential backoff.
func (r *AppDeploymentReconciler) cleanupFailed(ctx context.Context, appDep *platformv1alpha1.AppDeployment, reason string, cause error) (ctrl.Result, error) {
	msg := fmt.Sprintf("cleanup step failed, finalizer kept, will retry: %v", cause)
	r.event(appDep, corev1.EventTypeWarning, reason, msg)
	r.setCondition(appDep, conditionCleanup, metav1.ConditionFalse, reason, msg)
	if err := r.Status().Update(ctx, appDep); err != nil {
		log.FromContext(ctx).V(1).Info("Could not record the cleanup condition", "error", err.Error())
	}
	return ctrl.Result{}, fmt.Errorf("%s: %w", reason, cause)
}

// event records a Kubernetes Event when a recorder is configured.
func (r *AppDeploymentReconciler) event(obj runtime.Object, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventType, reason, msg)
	}
}

// operatorNamespace returns the fallback namespace for helmsman-platform-config.
func (r *AppDeploymentReconciler) operatorNamespace() string {
	if r.OperatorNamespace != "" {
		return r.OperatorNamespace
	}
	if ns := os.Getenv("OPERATOR_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return defaultOperatorNamespace
}

// getPlatformConfig reads helmsman-platform-config from the AppDeployment's
// namespace first and, if it is not there, from the operator's namespace. It
// returns the namespace it used. errPlatformConfigNotFound means neither has it.
func (r *AppDeploymentReconciler) getPlatformConfig(ctx context.Context, namespace string) (*platformConfig, string, error) {
	candidates := []string{namespace}
	if opNS := r.operatorNamespace(); opNS != namespace {
		candidates = append(candidates, opNS)
	}
	for _, ns := range candidates {
		var secret corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Name: platformConfigSecretName, Namespace: ns}, &secret)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("secret %s/%s unreadable: %w", ns, platformConfigSecretName, err)
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
			return nil, "", fmt.Errorf("secret %s/%s is missing required fields (keycloak-url, vault-url)", ns, platformConfigSecretName)
		}
		return cfg, ns, nil
	}
	return nil, "", fmt.Errorf("%w: secret %s in namespaces %v", errPlatformConfigNotFound, platformConfigSecretName, candidates)
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
