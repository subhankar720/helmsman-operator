package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Permission defines the access level granted to the subject.
// +kubebuilder:validation:Enum=ro;rw;admin
type Permission string

const (
	PermissionReadOnly  Permission = "ro"
	PermissionReadWrite Permission = "rw"
	PermissionAdmin     Permission = "admin"
)

// AccessGrantState represents the lifecycle state of an AccessGrant.
// +kubebuilder:validation:Enum=Pending;PendingApproval;Active;Expired;Revoked
type AccessGrantState string

const (
	StatePending         AccessGrantState = "Pending"
	StatePendingApproval AccessGrantState = "PendingApproval"
	StateActive          AccessGrantState = "Active"
	StateExpired         AccessGrantState = "Expired"
	StateRevoked         AccessGrantState = "Revoked"
)

// AccessGrantSpec defines the desired access to be granted.
type AccessGrantSpec struct {
	// Subject is the user or service account receiving access.
	// Format: user:<email> or serviceaccount:<namespace>/<name>
	// +kubebuilder:validation:MinLength=1
	Subject string `json:"subject"`

	// Cluster is the target cluster name (matches Argo CD cluster registration name).
	// +kubebuilder:validation:MinLength=1
	Cluster string `json:"cluster"`

	// Namespace is the target namespace on the cluster.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Permission defines the access level: ro, rw, or admin.
	// +kubebuilder:default=ro
	Permission Permission `json:"permission"`

	// TTL is the duration for which this grant is active.
	// Format: Go duration string e.g. "4h", "30m", "1h30m".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default="4h"
	TTL string `json:"ttl"`

	// Environment indicates the target environment type.
	// Determines whether production approval gate is required.
	// +kubebuilder:validation:Enum=dev;staging;prod
	// +kubebuilder:default=dev
	Environment string `json:"environment,omitempty"`

	// Reason is a human-readable justification for the access request.
	// Required for prod environment grants.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// AccessGrantStatus defines the observed state of an AccessGrant.
type AccessGrantStatus struct {
	// State is the current lifecycle state of the grant.
	// +optional
	State AccessGrantState `json:"state,omitempty"`

	// ExpiresAt is the absolute time at which this grant expires.
	// Populated when the grant moves to Active state.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// RoleBindingRef is the name of the RoleBinding created by this grant.
	// +optional
	RoleBindingRef string `json:"roleBindingRef,omitempty"`

	// ApprovedBy is the identity that approved a PendingApproval grant.
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`

	// ApprovedAt is when the approval was granted.
	// +optional
	ApprovedAt *metav1.Time `json:"approvedAt,omitempty"`

	// Message provides human-readable context for the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions represent the latest observations of the AccessGrant state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AccessGrant condition type constants
const (
	ConditionAccessGrantReady = "Ready"
	ConditionPolicyValidated  = "PolicyValidated"
	ConditionApproved         = "Approved"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag
// +kubebuilder:printcolumn:name="Subject",type="string",JSONPath=".spec.subject"
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.cluster"
// +kubebuilder:printcolumn:name="Namespace",type="string",JSONPath=".spec.namespace"
// +kubebuilder:printcolumn:name="Permission",type="string",JSONPath=".spec.permission"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state"
// +kubebuilder:printcolumn:name="ExpiresAt",type="date",JSONPath=".status.expiresAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AccessGrant is the Schema for the accessgrants API.
// It represents a time-bound, approval-gated request for namespace-scoped
// access to a Kubernetes cluster. The controller creates a RoleBinding when
// the grant is approved and active, and deletes it when the TTL expires.
type AccessGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AccessGrantSpec   `json:"spec,omitempty"`
	Status AccessGrantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AccessGrantList contains a list of AccessGrant.
type AccessGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AccessGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AccessGrant{}, &AccessGrantList{})
}
