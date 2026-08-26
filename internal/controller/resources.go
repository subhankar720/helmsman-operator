package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/subhankar720/helmsman-operator/api/v1alpha1"
)

// commonLabels returns the standard label set applied to all resources
// created by the operator for this AppDeployment.
func commonLabels(appDep *platformv1alpha1.AppDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       appDep.Spec.AppName,
		"app.kubernetes.io/managed-by": "helmsman-operator",
		"helmsman.dev/owning-team":     appDep.Spec.OwningTeam,
	}
}

// ensureServiceAccount creates the ServiceAccount for the app if it doesn't exist.
// automountServiceAccountToken is false — pods don't get the K8s API token
// unless they explicitly request it.
func (r *AppDeploymentReconciler) ensureServiceAccount(ctx context.Context, appDep *platformv1alpha1.AppDeployment) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appDep.Spec.AppName,
			Namespace: appDep.Namespace,
			Labels:    commonLabels(appDep),
		},
	}
	// automount set to false — security best practice
	f := false
	sa.AutomountServiceAccountToken = &f

	if err := controllerutil.SetControllerReference(appDep, sa, r.Scheme); err != nil {
		return err
	}

	return createOrSkip(ctx, r.Client, sa, &corev1.ServiceAccount{})
}

// ensureNetworkPolicies creates three NetworkPolicies:
// - default-deny: blocks all ingress and egress
// - allow-ingress: opens the traffic port (4180 if OIDC, app port otherwise)
// - allow-dns: opens UDP/TCP port 53 for DNS resolution
func (r *AppDeploymentReconciler) ensureNetworkPolicies(ctx context.Context, appDep *platformv1alpha1.AppDeployment) error {
	labels := commonLabels(appDep)
	selector := metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app.kubernetes.io/name": appDep.Spec.AppName,
		},
	}

	// Determine which port to open for ingress
	trafficPort := intstr.FromInt32(appDep.Spec.Container.Port)
	if appDep.Spec.OIDC.Enabled {
		trafficPort = intstr.FromInt32(4180)
	}

	// Policy 1: Default deny all
	denyAll := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appDep.Spec.AppName + "-default-deny",
			Namespace: appDep.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: selector,
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		},
	}

	// Policy 2: Allow ingress on traffic port
	tcpProto := corev1.ProtocolTCP
	allowIngress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appDep.Spec.AppName + "-allow-ingress",
			Namespace: appDep.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: selector,
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: &trafficPort, Protocol: &tcpProto},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}

	// Policy 3: Allow DNS egress (UDP + TCP port 53)
	udpProto := corev1.ProtocolUDP
	dnsPort := intstr.FromInt32(53)
	allowDNS := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appDep.Spec.AppName + "-allow-dns",
			Namespace: appDep.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: selector,
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: &dnsPort, Protocol: &udpProto},
						{Port: &dnsPort, Protocol: &tcpProto},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}

	// Policy 4: Allow egress to Keycloak (port 80)
	keycloakPort := intstr.FromInt32(80)
	allowKeycloakEgress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appDep.Spec.AppName + "-allow-keycloak-egress",
			Namespace: appDep.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: selector,
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: &keycloakPort, Protocol: &tcpProto},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}

	// Policy 5: Allow egress to Vault (port 8200)
	vaultPort := intstr.FromInt32(8200)
	allowVaultEgress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appDep.Spec.AppName + "-allow-vault-egress",
			Namespace: appDep.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: selector,
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: &vaultPort, Protocol: &tcpProto},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}

	for _, np := range []*networkingv1.NetworkPolicy{denyAll, allowIngress, allowDNS, allowKeycloakEgress, allowVaultEgress} {
		if err := controllerutil.SetControllerReference(appDep, np, r.Scheme); err != nil {
			return err
		}
		if err := createOrSkip(ctx, r.Client, np, &networkingv1.NetworkPolicy{}); err != nil {
			return fmt.Errorf("failed to ensure NetworkPolicy %s: %w", np.Name, err)
		}
	}

	return nil
}

// ensureServices creates the headless Service (required by StatefulSet)
// and the traffic Service (the actual entrypoint for requests).
func (r *AppDeploymentReconciler) ensureServices(ctx context.Context, appDep *platformv1alpha1.AppDeployment) error {
	labels := commonLabels(appDep)
	selector := map[string]string{"app.kubernetes.io/name": appDep.Spec.AppName}

	// Headless Service — gives each StatefulSet pod a stable DNS name
	headless := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appDep.Spec.AppName + "-headless",
			Namespace: appDep.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector:  selector,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       appDep.Spec.Container.Port,
					TargetPort: intstr.FromString("http"),
				},
			},
		},
	}

	// Traffic Service — what clients actually connect to
	// Exposes port 4180 (oauth2-proxy) when OIDC is enabled,
	// the app port directly when OIDC is disabled.
	targetPort := intstr.FromString("http")
	if appDep.Spec.OIDC.Enabled {
		targetPort = intstr.FromInt32(4180)
	}
	traffic := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appDep.Spec.AppName,
			Namespace: appDep.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: targetPort,
				},
			},
		},
	}

	for _, svc := range []*corev1.Service{headless, traffic} {
		if err := controllerutil.SetControllerReference(appDep, svc, r.Scheme); err != nil {
			return err
		}
		if err := createOrSkip(ctx, r.Client, svc, &corev1.Service{}); err != nil {
			return fmt.Errorf("failed to ensure Service %s: %w", svc.Name, err)
		}
	}

	return nil
}

// ensureStatefulSet creates or updates the StatefulSet.
// It builds the full pod spec including all sidecars.
func (r *AppDeploymentReconciler) ensureStatefulSet(ctx context.Context, appDep *platformv1alpha1.AppDeployment) error {
	logger := log.FromContext(ctx)
	labels := commonLabels(appDep)
	replicas := appDep.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	// Build container list starting with the app container
	containers := []corev1.Container{buildAppContainer(appDep)}

	// Add oauth2-proxy sidecar when OIDC is enabled
	if appDep.Spec.OIDC.Enabled {
		containers = append(containers, buildADCContainer(appDep))
	}

	// Fluent Bit sidecar is always present
	containers = append(containers, buildFluentBitContainer(appDep))

	// Build volume list
	volumes := []corev1.Volume{
		{
			Name:         "app-logs",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: "fluent-bit-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: appDep.Spec.AppName + "-fluent-bit-config",
					},
				},
			},
		},
	}

	// OIDC Secret volume — only when OIDC enabled
	if appDep.Spec.OIDC.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "oidc-secret",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: appDep.Spec.AppName + "-oidc",
				},
			},
		})
	}

	parallelPolicy := appsv1.ParallelPodManagement
	nonRoot := true
	runAsUser := int64(1000)
	fsGroup := int64(1000)

	desired := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appDep.Spec.AppName,
			Namespace: appDep.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            &replicas,
			ServiceName:         appDep.Spec.AppName + "-headless",
			PodManagementPolicy: parallelPolicy,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": appDep.Spec.AppName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: appDep.Spec.AppName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &nonRoot,
						RunAsUser:    &runAsUser,
						FSGroup:      &fsGroup,
					},
					Volumes:    volumes,
					Containers: containers,
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(appDep, desired, r.Scheme); err != nil {
		return err
	}

	// Create if not exists, update if replicas changed
	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		logger.Info("Creating StatefulSet", "name", desired.Name)
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}

	// Only update replicas — avoid unnecessary pod restarts
	if *existing.Spec.Replicas != replicas {
		existing.Spec.Replicas = &replicas
		return r.Update(ctx, existing)
	}

	return nil
}

// buildAppContainer constructs the main application container spec.
func buildAppContainer(appDep *platformv1alpha1.AppDeployment) corev1.Container {
	envVars := []corev1.EnvVar{
		{Name: "SERVER_PORT", Value: fmt.Sprintf("%d", appDep.Spec.Container.Port)},
	}
	for k, v := range appDep.Spec.Container.Env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	falseVal := false
	c := corev1.Container{
		Name:            "app",
		Image:           appDep.Spec.Image.Repository + ":" + appDep.Spec.Image.Tag,
		ImagePullPolicy: appDep.Spec.Image.PullPolicy,
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: appDep.Spec.Container.Port, Protocol: corev1.ProtocolTCP},
		},
		Env:       envVars,
		Resources: appDep.Spec.Resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "app-logs", MountPath: "/var/log/app"},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromString("http"),
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       15,
			FailureThreshold:    3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromString("http"),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}

	if len(appDep.Spec.Container.Command) > 0 {
		c.Command = appDep.Spec.Container.Command
	}

	return c
}

// buildADCContainer constructs the oauth2-proxy sidecar container spec.
// This is the ADC (Application Delivery Controller) equivalent of Webstax.
func buildADCContainer(appDep *platformv1alpha1.AppDeployment) corev1.Container {
	falseVal := false
	return corev1.Container{
		Name:  "adc",
		Image: "quay.io/oauth2-proxy/oauth2-proxy:v7.8.2",
		Args: []string{
			"--provider=oidc",
			"--oidc-issuer-url=$(ISSUER_URL)",
			"--client-id=$(CLIENT_ID)",
			"--client-secret=$(CLIENT_SECRET)",
			fmt.Sprintf("--upstream=http://127.0.0.1:%d", appDep.Spec.Container.Port),
			"--http-address=0.0.0.0:4180",
			"--email-domain=*",
			"--cookie-secure=false",
			"--skip-provider-button=true",
			"--redirect-url=http://localhost:4180/oauth2/callback",
			"--cookie-name=_helmsman_session",
		},
		Env: []corev1.EnvVar{
			{
				Name: "ISSUER_URL",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: appDep.Spec.AppName + "-oidc"},
					Key:                  "issuer-url",
				}},
			},
			{
				Name: "CLIENT_ID",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: appDep.Spec.AppName + "-oidc"},
					Key:                  "client-id",
				}},
			},
			{
				Name: "CLIENT_SECRET",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: appDep.Spec.AppName + "-oidc"},
					Key:                  "client-secret",
				}},
			},
			{
				Name: "OAUTH2_PROXY_COOKIE_SECRET",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: appDep.Spec.AppName + "-oidc"},
					Key:                  "cookie-secret",
				}},
			},
		},
		Ports: []corev1.ContainerPort{
			{Name: "proxy", ContainerPort: 4180},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/ping",
					Port: intstr.FromInt32(4180),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// buildFluentBitContainer constructs the Fluent Bit log-shipping sidecar.
func buildFluentBitContainer(appDep *platformv1alpha1.AppDeployment) corev1.Container {
	falseVal := false
	return corev1.Container{
		Name:  "fluent-bit",
		Image: "fluent/fluent-bit:3.2",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "app-logs", MountPath: "/var/log/app", ReadOnly: true},
			{Name: "fluent-bit-config", MountPath: "/fluent-bit/etc/"},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// createOrSkip creates the resource if it doesn't exist.
// If it already exists, it does nothing (idempotent).
// The `existing` parameter must be an empty pointer of the correct type
// so the Get call can populate it.
func createOrSkip(ctx context.Context, c client.Client, desired client.Object, existing client.Object) error {
	err := c.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}, existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	return err
}

// buildFluentBitConfigMap creates the Fluent Bit configuration ConfigMap.
func buildFluentBitConfigMap(appDep *platformv1alpha1.AppDeployment) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appDep.Spec.AppName + "-fluent-bit-config",
			Namespace: appDep.Namespace,
			Labels:    commonLabels(appDep),
		},
		Data: map[string]string{
			"fluent-bit.conf": `[SERVICE]
    Flush         1
    Log_Level     info
    Daemon        off
    Parsers_File  parsers.conf
    HTTP_Server   On
    HTTP_Listen   0.0.0.0
    HTTP_Port     2020

[INPUT]
    Name              tail
    Path              /var/log/app/*.log
    Parser            docker
    Tag               kube.*
    Refresh_Interval  5
    Mem_Buf_Limit     5MB
    Skip_Long_Lines   On

[FILTER]
    Name                kubernetes
    Match               kube.*
    Kube_URL            https://kubernetes.default.svc:443
    Kube_CA_File        /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
    Kube_Token_File     /var/run/secrets/kubernetes.io/serviceaccount/token
    Kube_Tag_Prefix     kube.var.log.containers.
    Merge_Log           On
    Merge_Log_Key       log_processed
    K8S-Logging.Parser  On
    K8S-Logging.Exclude On

[OUTPUT]
    Name  stdout
    Match *
`,
			"parsers.conf": `[PARSER]
    Name   docker
    Format json
    Time_Key time
    Time_Format %Y-%m-%dT%H:%M:%S.%L
    Time_Keep On
`,
		},
	}
}

// ensureFluentBitConfigMap creates the Fluent Bit ConfigMap if it doesn't exist.
func (r *AppDeploymentReconciler) ensureFluentBitConfigMap(ctx context.Context, appDep *platformv1alpha1.AppDeployment) error {
	cm := buildFluentBitConfigMap(appDep)
	if err := controllerutil.SetControllerReference(appDep, cm, r.Scheme); err != nil {
		return err
	}
	return createOrSkip(ctx, r.Client, cm, &corev1.ConfigMap{})
}
