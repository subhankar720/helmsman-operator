package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AppDeploymentSpec defines the desired state of AppDeployment.
// This is the developer-facing contract — the equivalent of spec.json in Webstax
// or app.platform.yaml in Helmsman's GitOps phase.
type AppDeploymentSpec struct {
	// AppName is the unique identifier for this application.
	// Used as the name prefix for all created resources.
	// +kubebuilder:validation:MinLength=1
	AppName string `json:"appName"`

	// OwningTeam is the team responsible for this application.
	// Used for governance labels and alerting.
	// +optional
	OwningTeam string `json:"owningTeam,omitempty"`

	// CostCenterId maps to the organisation's cost allocation system.
	// Equivalent to eon_id in Webstax.
	// +optional
	CostCenterId string `json:"costCenterId,omitempty"`

	// Image specifies the container image to deploy.
	Image ImageSpec `json:"image"`

	// Container specifies the application container configuration.
	Container ContainerSpec `json:"container"`

	// Resources specifies CPU and memory requests/limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Replicas is the desired number of pod replicas.
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// OIDC specifies the OpenID Connect configuration.
	// When enabled, an oauth2-proxy sidecar is injected and
	// a Keycloak client is registered automatically.
	// +optional
	OIDC OIDCSpec `json:"oidc,omitempty"`
}

// ImageSpec defines the container image reference.
type ImageSpec struct {
	// Repository is the image repository (e.g. ghcr.io/org/app).
	Repository string `json:"repository"`

	// Tag is the image tag to deploy.
	// +kubebuilder:default=latest
	Tag string `json:"tag"`

	// PullPolicy specifies when to pull the image.
	// +kubebuilder:default=IfNotPresent
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// ContainerSpec defines the application container configuration.
type ContainerSpec struct {
	// Port is the port the application listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Command overrides the image's default ENTRYPOINT.
	// +optional
	Command []string `json:"command,omitempty"`

	// Env is a map of environment variables to set in the container.
	// +optional
	Env map[string]string `json:"env,omitempty"`
}

// OIDCSpec defines the OIDC configuration for the application.
type OIDCSpec struct {
	// Enabled determines whether OIDC authentication is enforced.
	// When true, an oauth2-proxy sidecar is injected and the Service
	// exposes the proxy port rather than the application port directly.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// AppDeploymentStatus defines the observed state of AppDeployment.
type AppDeploymentStatus struct {
	// Conditions represent the latest available observations of the AppDeployment state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// OIDCClientID is the Keycloak client ID registered for this application.
	// Populated by the operator after successful Keycloak registration.
	// +optional
	OIDCClientID string `json:"oidcClientId,omitempty"`

	// Namespace is the Kubernetes namespace the application was deployed into.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Cluster is the name of the cluster sourced from helmsman-platform-config.
	// +optional
	Cluster string `json:"cluster,omitempty"`

	// Endpoint is the internal service endpoint for this application.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Phase is a human-readable summary: Pending, Deploying, Ready, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`


	// ReadyReplicas is the number of pods in the Ready state.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Condition type constants
const (
	// ConditionReady indicates the AppDeployment is fully reconciled and healthy.
	ConditionReady = "Ready"

	// ConditionOIDCRegistered indicates the Keycloak client has been registered.
	ConditionOIDCRegistered = "OIDCRegistered"

	// ConditionWorkloadCreated indicates the StatefulSet and Services exist.
	ConditionWorkloadCreated = "WorkloadCreated"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=appdep
// +kubebuilder:printcolumn:name="App",type="string",JSONPath=".spec.appName"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="OIDC",type="boolean",JSONPath=".spec.oidc.enabled"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AppDeployment is the Schema for the appdeployments API.
// It is the typed, self-healing replacement for the golden-path Helm chart's
// app.platform.yaml values file. Each AppDeployment CR results in a complete
// platform-managed application deployment including StatefulSet, Services,
// NetworkPolicy, ServiceAccount, and optional OIDC registration.
type AppDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppDeploymentSpec   `json:"spec,omitempty"`
	Status AppDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AppDeploymentList contains a list of AppDeployment.
type AppDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AppDeployment{}, &AppDeploymentList{})
}
