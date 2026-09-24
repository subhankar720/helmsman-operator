package access

import (
	"context"
	"fmt"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	accessv1alpha1 "github.com/subhankar720/helmsman-operator/api/access/v1alpha1"
)

const (
	accessGrantFinalizer = "access.helmsman.dev/finalizer"

	// Predefined ClusterRole names the controller binds to
	roleReader   = "helmsman-namespace-reader"
	roleOperator = "helmsman-namespace-operator"
	roleAdmin    = "helmsman-namespace-admin"

	// TTL limits by environment
	maxTTLProd    = 8 * time.Hour
	maxTTLNonProd = 24 * time.Hour
)

// AccessGrantReconciler reconciles AccessGrant objects.
type AccessGrantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=access.helmsman.dev,resources=accessgrants,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=access.helmsman.dev,resources=accessgrants/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=access.helmsman.dev,resources=accessgrants/finalizers,verbs=update
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch

func (r *AccessGrantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// ── Step 1: Fetch the AccessGrant ────────────────────────────────────
	var ag accessv1alpha1.AccessGrant
	if err := r.Get(ctx, req.NamespacedName, &ag); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// ── Step 2: Handle deletion ───────────────────────────────────────────
	if !ag.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &ag)
	}

	// ── Step 3: Add finalizer ─────────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&ag, accessGrantFinalizer) {
		controllerutil.AddFinalizer(&ag, accessGrantFinalizer)
		if err := r.Update(ctx, &ag); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// ── Step 4: Policy validation ─────────────────────────────────────────
	// Parse TTL, enforce environment limits
	ttl, err := time.ParseDuration(ag.Spec.TTL)
	if err != nil {
		return r.setFailedState(ctx, &ag, "InvalidTTL",
			fmt.Sprintf("TTL %q is not a valid duration: %v", ag.Spec.TTL, err))
	}

	maxTTL := maxTTLNonProd
	if ag.Spec.Environment == "prod" {
		maxTTL = maxTTLProd
	}
	if ttl > maxTTL {
		return r.setFailedState(ctx, &ag, "TTLExceedsLimit",
			fmt.Sprintf("TTL %s exceeds maximum %s for environment %s",
				ag.Spec.TTL, maxTTL, ag.Spec.Environment))
	}

	r.setCondition(&ag, accessv1alpha1.ConditionPolicyValidated,
		metav1.ConditionTrue, "Valid", "TTL and permission within policy limits")
	logger.Info("Policy validated", "ttl", ttl, "env", ag.Spec.Environment)

	// ── Step 5: Check expiry (if already Active) ──────────────────────────
	if ag.Status.State == accessv1alpha1.StateActive && ag.Status.ExpiresAt != nil {
		if time.Now().After(ag.Status.ExpiresAt.Time) {
			logger.Info("AccessGrant TTL expired — revoking", "subject", ag.Spec.Subject)
			return r.expireGrant(ctx, &ag)
		}
		// Still active — requeue at expiry time
		remaining := time.Until(ag.Status.ExpiresAt.Time)
		logger.Info("Grant still active", "remaining", remaining.Round(time.Second))
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	// ── Step 6: Production approval gate ──────────────────────────────────
	// Prod grants require explicit approval before RoleBinding is created.
	// The broker API sets an annotation to signal approval.
	if ag.Spec.Environment == "prod" &&
		ag.Status.State != accessv1alpha1.StateActive {

		approved := ag.Annotations["access.helmsman.dev/approved-by"] != ""
		if !approved {
			if ag.Status.State != accessv1alpha1.StatePendingApproval {
				ag.Status.State = accessv1alpha1.StatePendingApproval
				ag.Status.Message = "Production access requires approval. " +
					"Set annotation access.helmsman.dev/approved-by to approve."
				r.setCondition(&ag, accessv1alpha1.ConditionApproved,
					metav1.ConditionFalse, "AwaitingApproval",
					"Production grant pending approval")
				_ = r.Status().Update(ctx, &ag)
			}
			logger.Info("Production grant awaiting approval", "subject", ag.Spec.Subject)
			// Requeue every 30s to check for approval
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// Approval found — record who approved it
		ag.Status.ApprovedBy = ag.Annotations["access.helmsman.dev/approved-by"]
		now := metav1.Now()
		ag.Status.ApprovedAt = &now
	}

	r.setCondition(&ag, accessv1alpha1.ConditionApproved,
		metav1.ConditionTrue, "Approved", "Grant approved")

	// ── Step 7: Create or verify RoleBinding ──────────────────────────────
	clusterRoleName := permissionToClusterRole(ag.Spec.Permission)
	rbName := fmt.Sprintf("helmsman-ag-%s", ag.Name)

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rbName,
			Namespace: ag.Spec.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":  "helmsman-operator",
				"access.helmsman.dev/grant":     ag.Name,
				"access.helmsman.dev/subject":   sanitizeLabel(ag.Spec.Subject),
				"access.helmsman.dev/grantedBy": "access-broker",
			},
		},
		Subjects: []rbacv1.Subject{
			subjectFromSpec(ag.Spec.Subject),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRoleName,
		},
	}

	// RoleBinding is NOT owned by the AccessGrant via ownerReference because
	// it lives in a different namespace (target namespace vs grant namespace).
	// Cleanup is handled explicitly in the finalizer.
	existing := &rbacv1.RoleBinding{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      rbName,
		Namespace: ag.Spec.Namespace,
	}, existing)

	if errors.IsNotFound(err) {
		if err := r.Create(ctx, rb); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create RoleBinding: %w", err)
		}
		logger.Info("RoleBinding created",
			"name", rbName,
			"namespace", ag.Spec.Namespace,
			"clusterRole", clusterRoleName,
			"subject", ag.Spec.Subject)
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// ── Step 8: Set Active state and schedule expiry requeue ──────────────
	now := metav1.Now()
	expiresAt := metav1.NewTime(now.Add(ttl))

	ag.Status.State = accessv1alpha1.StateActive
	ag.Status.ExpiresAt = &expiresAt
	ag.Status.RoleBindingRef = rbName
	ag.Status.Message = fmt.Sprintf("Access active until %s", expiresAt.Format(time.RFC3339))
	r.setCondition(&ag, accessv1alpha1.ConditionAccessGrantReady,
		metav1.ConditionTrue, "Active", "RoleBinding created and access is live")

	if err := r.Status().Update(ctx, &ag); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("AccessGrant activated",
		"subject", ag.Spec.Subject,
		"namespace", ag.Spec.Namespace,
		"permission", ag.Spec.Permission,
		"expiresAt", expiresAt.Format(time.RFC3339))

	// Requeue exactly when the TTL expires to trigger cleanup
	return ctrl.Result{RequeueAfter: ttl}, nil
}

// handleDeletion removes the RoleBinding and the finalizer.
func (r *AccessGrantReconciler) handleDeletion(ctx context.Context, ag *accessv1alpha1.AccessGrant) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(ag, accessGrantFinalizer) {
		return ctrl.Result{}, nil
	}

	if err := r.deleteRoleBinding(ctx, ag); err != nil {
		logger.Error(err, "Failed to delete RoleBinding during cleanup")
		// Continue — don't block CR deletion on RoleBinding cleanup failure
	}

	controllerutil.RemoveFinalizer(ag, accessGrantFinalizer)
	return ctrl.Result{}, r.Update(ctx, ag)
}

// expireGrant deletes the RoleBinding and sets the grant to Expired state.
func (r *AccessGrantReconciler) expireGrant(ctx context.Context, ag *accessv1alpha1.AccessGrant) (ctrl.Result, error) {
	if err := r.deleteRoleBinding(ctx, ag); err != nil {
		return ctrl.Result{}, err
	}

	ag.Status.State = accessv1alpha1.StateExpired
	ag.Status.Message = fmt.Sprintf("Grant expired at %s", ag.Status.ExpiresAt.Format(time.RFC3339))
	r.setCondition(ag, accessv1alpha1.ConditionAccessGrantReady,
		metav1.ConditionFalse, "Expired", "TTL elapsed — RoleBinding deleted")
	_ = r.Status().Update(ctx, ag)

	return ctrl.Result{}, nil
}

// deleteRoleBinding removes the RoleBinding created for this grant.
func (r *AccessGrantReconciler) deleteRoleBinding(ctx context.Context, ag *accessv1alpha1.AccessGrant) error {
	if ag.Status.RoleBindingRef == "" {
		return nil
	}
	rb := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      ag.Status.RoleBindingRef,
		Namespace: ag.Spec.Namespace,
	}, rb)
	if errors.IsNotFound(err) {
		return nil // already gone
	}
	if err != nil {
		return err
	}
	return r.Delete(ctx, rb)
}

// setFailedState sets the grant to a failed/invalid state with a reason.
func (r *AccessGrantReconciler) setFailedState(ctx context.Context, ag *accessv1alpha1.AccessGrant, reason, message string) (ctrl.Result, error) {
	ag.Status.State = accessv1alpha1.StatePending
	ag.Status.Message = message
	r.setCondition(ag, accessv1alpha1.ConditionPolicyValidated,
		metav1.ConditionFalse, reason, message)
	_ = r.Status().Update(ctx, ag)
	return ctrl.Result{}, fmt.Errorf("%s: %s", reason, message)
}

// setCondition updates or appends a condition on the AccessGrant status.
func (r *AccessGrantReconciler) setCondition(ag *accessv1alpha1.AccessGrant,
	condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range ag.Status.Conditions {
		if c.Type == condType {
			ag.Status.Conditions[i].Status = status
			ag.Status.Conditions[i].Reason = reason
			ag.Status.Conditions[i].Message = message
			ag.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	ag.Status.Conditions = append(ag.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: ag.Generation,
	})
}

// permissionToClusterRole maps a Permission value to a predefined ClusterRole name.
func permissionToClusterRole(p accessv1alpha1.Permission) string {
	switch p {
	case accessv1alpha1.PermissionReadWrite:
		return roleOperator
	case accessv1alpha1.PermissionAdmin:
		return roleAdmin
	default:
		return roleReader
	}
}

// subjectFromSpec parses the subject string into an RBAC Subject.
// Supports: user:<email> and serviceaccount:<namespace>/<name>
func subjectFromSpec(subject string) rbacv1.Subject {
	if len(subject) > 16 && subject[:16] == "serviceaccount:" {
		rest := subject[16:]
		parts := splitN(rest, "/", 2)
		if len(parts) == 2 {
			return rbacv1.Subject{
				Kind:      "ServiceAccount",
				Name:      parts[1],
				Namespace: parts[0],
			}
		}
	}
	// Default: treat as user
	name := subject
	if len(subject) > 5 && subject[:5] == "user:" {
		name = subject[5:]
	}
	return rbacv1.Subject{
		Kind:     "User",
		Name:     name,
		APIGroup: "rbac.authorization.k8s.io",
	}
}

// sanitizeLabel replaces characters invalid in label values with dashes.
func sanitizeLabel(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' {
			result[i] = c
		} else {
			result[i] = '-'
		}
	}
	return string(result)
}

// splitN splits s by sep at most n times.
func splitN(s, sep string, n int) []string {
	var result []string
	for n > 1 {
		idx := indexOf(s, sep)
		if idx < 0 {
			break
		}
		result = append(result, s[:idx])
		s = s[idx+len(sep):]
		n--
	}
	return append(result, s)
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// SetupWithManager registers the controller with the manager.
func (r *AccessGrantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&accessv1alpha1.AccessGrant{}).
		Complete(r)
}
