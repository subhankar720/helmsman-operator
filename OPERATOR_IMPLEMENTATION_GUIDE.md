# ArgoCD Deployments & Operator Implementation Guide

> **Audience:** Developers and platform engineers who want to understand how this repository’s ArgoCD setup, Kubernetes operator, scripts, and manifests are designed — and how to extend them.
> **Goal:** Explain the actual deployment architecture, code flow, build scripts, and GitOps mechanics used in this project, without rehashing generic Kubernetes basics.

---

## Table of Contents

1. [Project Layout](#1-project-layout)
2. [ArgoCD Deployment Architecture](#2-argocd-deployment-architecture)
3. [Kustomize Manifests & Image Management](#3-kustomize-manifests--image-management)
4. [Operator Design & Reconciliation Flow](#4-operator-design--reconciliation-flow)
5. [Key Scripts & Make Targets](#5-key-scripts--make-targets)
6. [Platform Config & Secret Flow](#6-platform-config--secret-flow)
7. [Multi-Cluster Promotion Flow](#7-multi-cluster-promotion-flow)
8. [Extending the Operator](#8-extending-the-operator)
9. [Debugging Checklist](#9-debugging-checklist)

---

## 1. Project Layout

```
helmsman-operator/
├── cmd/
│   └── main.go                 # Operator entrypoint
├── internal/
│   ├── controller/
│   │   ├── appdeployment_controller.go   # Reconciler + controller setup
│   │   ├── keycloak.go                   # Keycloak/Vault HTTP integration
│   │   └── resources.go                  # K8s resource builders (StatefulSet, Service, etc.)
│   └── ...
├── config/
│   ├── crd/bases/
│   │   └── appdeployments.platform.helmsman.dev.yaml   # AppDeployment CRD
│   ├── rbac/
│   │   ├── role.yaml            # Generated ClusterRole
│   │   └── role_binding.yaml    # Generated ClusterRoleBinding
│   ├── manager/
│   │   └── manager.yaml         # Deployment manifest for operator
│   ├── default/
│   │   └── kustomization.yaml   # Root kustomization (CRD + RBAC + manager)
│   └── samples/
│       └── platform_v1alpha1_appdeployment.yaml   # Example AppDeployment
├── helm/
│   └── helmsman-operator/       # Helm chart for operator
├── Dockerfile                   # Multi-stage build for operator image
├── Makefile                     # Build, manifest, deploy, test targets
└── INCIDENT_RUNBOOK.md          # Detailed incident history and fixes
```

---

## 2. ArgoCD Deployment Architecture

### 2.1 Two-Cluster Model

```
kind-helmsman-hub  ← ArgoCD lives here, manages cluster state
kind-helmsman-onprem  ← Optional secondary cluster for development
```

### 2.2 ArgoCD Application for the Operator

The operator itself is **not** deployed by ArgoCD in this homelab. Instead, ArgoCD deploys **AppDeployment CRs** from the `helmsman-fleet` repo.

```yaml
# argocd/helmsman-appdeployments.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: helmsman-appdeployments
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/subhankar720/helmsman-fleet.git
    targetRevision: main
    path: appdeployments
  destination:
    server: https://172.18.0.6:6443
    namespace: sample-app
  syncPolicy:
    automated:
      prune: true      # Delete resources removed from Git
      selfHeal: true    # Fix drift between Git and cluster
    syncOptions:
    - CreateNamespace=true
```

**Important:** The `repoURL` points to `helmsman-fleet`, not `helmsman-operator`. AppDeployment CRs are stored in the fleet repo, while the operator code lives in `helmsman-operator`.

### 2.3 ArgoCD App for ArgoCD Itself (App of Apps Pattern)

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: argocd
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/subhankar720/helmsman-operator.git
    targetRevision: main
    path: argocd
  destination:
    server: https://kubernetes.default.svc
    namespace: argocd
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

This creates the **App of Apps** pattern where one ArgoCD Application manages other Applications.

### 2.4 How ArgoCD Syncs AppDeployments

1. You push a change to `helmsman-fleet/appdeployments/sample-app.yaml`
2. ArgoCD detects the change via webhook or polling
3. ArgoCD applies the YAML to the target cluster (`kind-helmsman-hub`)
4. The operator’s controller sees the new/updated `AppDeployment` CR
5. The operator reconciles: creates all K8s resources

```bash
# Manual sync
argocd app sync helmsman-appdeployments

# Watch sync status
argocd app get helmsman-appdeployments --refresh
```

---

## 3. Kustomize Manifests & Image Management

### 3.1 Manifest Generation Flow

```
controller-gen  →  config/crd/bases/*.yaml  +  config/rbac/*.yaml
kustomize      →  config/default/          →  dist/install.yaml
```

### 3.2 controller-gen Annotations

In `internal/controller/appdeployment_controller.go`:

```go
//+kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=platform.helmsman.dev,resources=appdeployments/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
```

Running `make manifests` generates `config/rbac/role.yaml` with these permissions.

### 3.3 Kustomize Overlays

```
config/
├── default/
│   └── kustomization.yaml      # Base: CRD + RBAC + manager
├── manager/
│   └── manager.yaml            # Deployment spec
└── crd/
    └── bases/
        └── appdeployments.platform.helmsman.dev.yaml
```

The default kustomization adds:
- Namespace prefix `helmsman-operator-`
- Namespace `helmsman-operator-system`
- Metrics patch
- NetworkPolicy (optional)

### 3.4 Image Replacement

The base `manager.yaml` has:
```yaml
image: controller:latest
```

After `make kustomize`, the image is updated:
```bash
cd config/manager && kustomize edit set image controller=ghcr.io/subhankar720/helmsman-operator:latest
```

Or for local development:
```bash
cd config/manager && kustomize edit set image controller=helmsman-operator:dev
```

### 3.5 Generated Manifest Output

```bash
make manifests kustomize
/home/subhankar/projects/helmsman/helmsman-operator/bin/kustomize build config/default > /tmp/helmsman-operator-manifests.yaml
```

Apply to cluster:
```bash
kubectl apply -f /tmp/helmsman-operator-manifests.yaml --context kind-helmsman-hub
```

---

## 4. Operator Design & Reconciliation Flow

### 4.1 Entrypoint

```go
// cmd/main.go
func main() {
    // 1. Create manager
    mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
        Scheme:         scheme,
        MetricsBindAddress: ":8083",
        HealthProbeBindAddress: ":8081",
        LeaderElection:       true,
        LeaderElectionID:     "65799e13.helmsman.dev",
    })

    // 2. Setup controller
    if err = (&AppDeploymentReconciler{
        Client: mgr.GetClient(),
        Scheme: mgr.GetScheme(),
    }).SetupWithManager(mgr); err != nil {
        panic(err)
    }

    // 3. Start manager
    if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
        panic(err)
    }
}
```

### 4.2 Controller Setup

```go
// internal/controller/appdeployment_controller.go
func (r *AppDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
    // Watch AppDeployment CRs
    return ctrl.NewControllerManagedBy(mgr).
        For(&platformv1alpha1.AppDeployment{}).
        Owns(&appsv1.StatefulSet{}).
        Owns(&corev1.Service{}).
        Owns(&corev1.ServiceAccount{}).
        Owns(&networkingv1.NetworkPolicy{}).
        Owns(&externalsecretsv1beta1.ExternalSecret{}).
        Complete(r)
}
```

The `.Owns()` calls set up watches on child resources. When a child changes, the parent AppDeployment is re-reconciled.

### 4.3 Reconciliation Flow

```
AppDeployment Created/Updated
         │
         ▼
+------------------+
| Reconcile()      |
+------------------+
         │
         ├─► 1. Get AppDeployment from cache
         ├─► 2. Handle deletion (finalizer)
         ├─► 3. Ensure platform config secret exists
         ├─► 4. Register Keycloak client → write creds to Vault
         ├─► 5. Ensure StatefulSet
         ├─► 6. Ensure Services (headless + traffic)
         ├─► 7. Ensure ServiceAccount
         ├─► 8. Ensure NetworkPolicies (default-deny, ingress, DNS, Keycloak, Vault)
         ├─► 9. Ensure ExternalSecret (ESO syncs Vault → K8s Secret)
         └─► 10. Update status
```

### 4.4 Finalizer Flow

```go
// internal/controller/appdeployment_controller.go
func (r *AppDeploymentReconciler) handleDeletion(ctx context.Context, appDep *platformv1alpha1.AppDeployment) error {
    // 1. Read platform config
    cfg, err := r.readPlatformConfig(ctx, appDep.Namespace)
    
    // 2. Delete Keycloak client
    err = deleteKeycloakClient(ctx, appDep.Spec.AppName, cfg)
    
    // 3. Remove finalizer
    controllerutil.RemoveFinalizer(appDep, "platform.helmsman.dev/finalizer")
    
    return nil
}
```

The finalizer ensures Keycloak cleanup happens before the CR is fully deleted. Without it, the Keycloak client would be orphaned.

### 4.5 Owner References

```go
// All child resources get owner reference to AppDeployment
controllerutil.SetControllerReference(appDep, statefulSet, r.Scheme)
controllerutil.SetControllerReference(appDep, service, r.Scheme)
```

When the AppDeployment is deleted, Kubernetes automatically garbage-collects all owned resources.

---

## 5. Key Scripts & Make Targets

### 5.1 Makefile Targets

```makefile
# Generate CRDs and RBAC from code annotations
manifests: controller-gen
    $(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

# Build kustomize and set image
kustomize: $(KUSTOMIZE)
    cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}

# Generate code, manifests, kustomize, then build installer
build-installer: manifests generate kustomize
    $(KUSTOMIZE) build config/default > dist/install.yaml

# Deploy to current kubectl context
deploy: manifests kustomize
    kubectl apply -f config/default

# Undeploy from current kubectl context
undeploy: kustomize
    kubectl delete -f config/default

# Build Docker image
docker-build:
    $(CONTAINER_TOOL) build -t ${IMG} .

# Push Docker image
docker-push:
    $(CONTAINER_TOOL) push ${IMG}
```

### 5.2 Local Development Script

```bash
#!/bin/bash
# scripts/local-dev.sh

set -e

echo "=== Building operator image ==="
docker build -t helmsman-operator:dev \
  --build-arg TARGETOS=linux \
  --build-arg TARGETARCH=amd64 \
  -f Dockerfile .

echo "=== Loading into Kind clusters ==="
kind load docker-image helmsman-operator:dev --name helmsman-onprem
kind load docker-image helmsman-operator:dev --name helmsman-hub

echo "=== Updating deployments ==="
kubectl set image deployment/helmsman-operator \
  -n helmsman-operator manager=helmsman-operator:dev \
  --context kind-helmsman-onprem

kubectl set image deployment/helmsman-operator-controller-manager \
  -n helmsman-operator-system manager=helmsman-operator:dev \
  --context kind-helmsman-hub

echo "=== Setting imagePullPolicy to Never ==="
kubectl patch deployment helmsman-operator -n helmsman-operator \
  --context kind-helmsman-onprem \
  --type='json' \
  -p='[{"op": "add", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]'

kubectl patch deployment helmsman-operator-controller-manager -n helmsman-operator-system \
  --context kind-helmsman-hub \
  --type='json' \
  -p='[{"op": "add", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]'

echo "=== Waiting for rollout ==="
kubectl rollout status deployment/helmsman-operator -n helmsman-operator \
  --context kind-helmsman-onprem --timeout=120s

kubectl rollout status deployment/helmsman-operator-controller-manager \
  -n helmsman-operator-system --context kind-helmsman-hub --timeout=120s

echo "=== Done ==="
```

### 5.3 Recovery Script After Docker Restart

```bash
#!/bin/bash
# scripts/recover-after-docker-restart.sh

set -e

echo "=== Waiting for Docker ==="
while ! docker info >/dev/null 2>&1; do
  echo "Waiting for Docker..."
  sleep 2
done

echo "=== Recreating Kind cluster ==="
kind create cluster --name helmsman-onprem --config kind-config.yaml
export CONTEXT="kind-helmsman-onprem"

echo "=== Installing infrastructure ==="
kubectl wait --for=condition=Ready node --all --context $CONTEXT --timeout=120s

# Install Vault
helm install vault hashicorp/vault \
  --namespace vault --create-namespace \
  --set "server.dev.enabled=true" \
  --set "server.dev.rootToken=root" \
  --set "server.service.type=NodePort" \
  --set "server.service.nodePorts.http=30082" \
  --context $CONTEXT

# Install Keycloak
helm install keycloak bitnami/keycloak \
  --namespace keycloak --create-namespace \
  --set auth.adminPassword=helmsman123 \
  --set httpPort=8081 \
  --set service.type=NodePort \
  --set service.nodePorts.http=8081 \
  --context $CONTEXT

# Install ESO
helm install external-secrets external-secrets/external-secrets \
  -n external-secrets --create-namespace \
  --set webhook.port=9443 \
  --set serviceMonitor.enabled=false \
  --context $CONTEXT

# Wait for pods
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=vault \
  -n vault --context $CONTEXT --timeout=120s
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=keycloak \
  -n keycloak --context $CONTEXT --timeout=180s
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=external-secrets \
  -n external-secrets --context $CONTEXT --timeout=120s

echo "=== Creating secrets ==="
kubectl create secret generic vault-token \
  -n external-secrets \
  --from-literal=token=root \
  --context $CONTEXT

kubectl apply -f - --context $CONTEXT <<EOF
apiVersion: external-secrets.io/v1
kind: ClusterSecretStore
metadata:
  name: vault-backend
spec:
  provider:
    vault:
      server: "http://vault.vault:8200"
      path: "secret"
      version: "v2"
      auth:
        tokenSecretRef:
          name: vault-token
          key: token
          namespace: external-secrets
EOF

echo "=== Deploying operator ==="
kubectl apply -f /tmp/helmsman-operator-manifests.yaml --context $CONTEXT

echo "=== Creating sample-app ==="
kubectl create namespace sample-app --context $CONTEXT

kubectl create secret generic helmsman-platform-config \
  -n sample-app \
  --from-literal=keycloak-url=http://keycloak.keycloak:80 \
  --from-literal=keycloak-oidc-url=http://keycloak-headless.keycloak:8080 \
  --from-literal=keycloak-realm=helmsman \
  --from-literal=keycloak-admin-user=admin \
  --from-literal=keycloak-admin-password=helmsman123 \
  --from-literal=vault-url=http://vault.vault:8200 \
  --from-literal=vault-token=root \
  --context $CONTEXT

echo "=== Recovery complete ==="
```

---

## 6. Platform Config & Secret Flow

### 6.1 Platform Config Secret Structure

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: helmsman-platform-config
  namespace: sample-app
type: Opaque
stringData:
  keycloak-url: http://keycloak.keycloak:80           # Operator uses this for admin API
  keycloak-oidc-url: http://keycloak-headless.keycloak:8080  # Pods use this for OIDC
  keycloak-realm: helmsman
  keycloak-admin-user: admin
  keycloak-admin-password: helmsman123
  vault-url: http://vault.vault:8200
  vault-token: root
```

### 6.2 Secret Flow Diagram

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│  Platform    │     │   Operator   │     │    Vault     │
│  Config      │────▶│              │────▶│              │
│  Secret      │     │ 1. Reads     │     │ 2. Writes    │
│  (K8s)       │     │    config    │     │    OIDC      │
└──────────────┘     └──────────────┘     │    creds     │
                                           └──────┬──────┘
                                                  │
                                                  │ 3. ESO syncs
                                                  ▼
                                           ┌──────────────┐
                                           │ ExternalSecret│
                                           │   (K8s)       │
                                           └──────┬──────┘
                                                  │
                                                  │ 4. Creates
                                                  ▼
                                           ┌──────────────┐
                                           │   sample-app  │
                                           │   -oidc Secret│
                                           │   (K8s)       │
                                           └──────┬──────┘
                                                  │
                                                  │ 5. Mounts as
                                                  ▼
                                           ┌──────────────┐
                                           │  sample-app   │
                                           │  Pod          │
                                           │  - app        │
                                           │  - adc        │
                                           │  - fluent-bit │
                                           └──────────────┘
```

### 6.3 Operator Reads Platform Config

```go
// internal/controller/appdeployment_controller.go
func (r *AppDeploymentReconciler) readPlatformConfig(ctx context.Context, ns string) (*platformConfig, error) {
    secret := &corev1.Secret{}
    err := r.Get(ctx, types.NamespacedName{Name: "helmsman-platform-config", Namespace: ns}, secret)
    if err != nil {
        return nil, err
    }
    
    cfg := &platformConfig{
        KeycloakURL:      string(secret.Data["keycloak-url"]),
        KeycloakOIDCURL:  string(secret.Data["keycloak-oidc-url"]),
        KeycloakRealm:    string(secret.Data["keycloak-realm"]),
        KeycloakAdminUser: string(secret.Data["keycloak-admin-user"]),
        KeycloakAdminPass: string(secret.Data["keycloak-admin-password"]),
        VaultURL:         string(secret.Data["vault-url"]),
        VaultToken:       string(secret.Data["vault-token"]),
    }
    
    // Fallback for backward compatibility
    if cfg.KeycloakOIDCURL == "" {
        cfg.KeycloakOIDCURL = cfg.KeycloakURL
    }
    
    return cfg, nil
}
```

### 6.4 Vault Write Path

```go
// internal/controller/keycloak.go
func writeOIDCCredsToVault(ctx context.Context, appName string, creds *OIDCCredentials, cfg *platformConfig) error {
    vaultPath := fmt.Sprintf("%s/v1/secret/data/apps/%s/oidc", cfg.VaultURL, appName)
    
    // Preserve existing cookie-secret to avoid invalidating sessions
    existingCookieSecret := ""
    req, _ := http.NewRequest("GET", vaultPath, nil)
    req.Header.Set("X-Vault-Token", cfg.VaultToken)
    resp, err := httpClient.Do(req)
    if err == nil && resp.StatusCode == http.StatusOK {
        defer resp.Body.Close()
        var existing struct {
            Data struct {
                Data map[string]string `json:"data"`
            } `json:"data"`
        }
        if err := json.NewDecoder(resp.Body).Decode(&existing); err == nil {
            existingCookieSecret = existing.Data.Data["cookie-secret"]
        }
    }
    
    // Generate cookie secret if none exists
    cookieSecret := existingCookieSecret
    if cookieSecret == "" {
        raw := make([]byte, 32)
        if _, err := rand.Read(raw); err != nil {
            return err
        }
        cookieSecret = base64.URLEncoding.EncodeToString(raw)
    }
    creds.CookieSecret = cookieSecret
    
    // Write to Vault KV v2
    payload := map[string]interface{}{
        "data": map[string]string{
            "client-id":     creds.ClientID,
            "client-secret": creds.ClientSecret,
            "issuer-url":    creds.IssuerURL,
            "cookie-secret": creds.CookieSecret,
        },
    }
    // ... POST to Vault
}
```

---

## 7. Multi-Cluster Promotion Flow

### 7.1 Cluster Registry

```bash
# Add clusters to kubectl context
kind create cluster --name helmsman-onprem
kind create cluster --name helmsman-hub

# Verify contexts
kubectl config get-contexts
# CURRENT   NAME                   CLUSTER                AUTHINFO
#           kind-helmsman-hub      kind-helmsman-hub      kind-helmsman-hub
# *         kind-helmsman-onprem   kind-helmsman-onprem   kind-helmsman-onprem
```

### 7.2 ArgoCD Cluster Registration

```bash
# Add hub cluster to ArgoCD
argocd cluster add kind-helmsman-hub --name hub --namespace argocd

# Add onprem cluster to ArgoCD
argocd cluster add kind-helmsman-onprem --name onprem --namespace argocd
```

### 7.3 Multi-Cluster Application

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: sample-app-multi
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/subhankar720/helmsman-fleet.git
    targetRevision: main
    path: appdeployments
  destination:
    server: https://kubernetes.default.svc  # ArgoCD's cluster
    namespace: sample-app
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

### 7.4 Promotion via Git Branches

```bash
# Promote from dev to staging
git checkout main
git merge dev/appdeployment-sample-app
git push origin main

# ArgoCD detects change and syncs to staging/prod
```

### 7.5 Promotion via ArgoCD Projects and Destinations

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: sample-app
  namespace: argocd
spec:
  sourceRepos:
  - https://github.com/subhankar720/helmsman-fleet.git
  destinations:
  - namespace: sample-app
    server: https://172.18.0.6:6443  # staging
  - namespace: sample-app
    server: https://172.18.0.5:6443  # prod
  clusterResourceWhitelist:
  - group: ""
    kind: Namespace
```

---

## 8. Extending the Operator

### 8.1 Adding a New Field to AppDeployment

**Step 1: Update the CRD**

```yaml
# config/crd/bases/platform_v1alpha1_appdeployment.yaml
spec:
  group: platform.helmsman.dev
  names:
    kind: AppDeployment
    plural: appdeployments
  scope: Namespaced
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        properties:
          spec:
            properties:
              sidecars:
                items:
                  properties:
                    name: {type: string}
                    image: {type: string}
                    ports:
                      items:
                        properties:
                          containerPort: {type: integer}
                      type: array
                  type: object
                type: array
```

**Step 2: Update the Go types**

```go
// api/v1alpha1/appdeployment_types.go
type AppDeploymentSpec struct {
    AppName     string            `json:"appName"`
    OwningTeam  string            `json:"owningTeam"`
    Image       ImageSpec         `json:"image"`
    Container   ContainerSpec     `json:"container"`
    Resources   corev1.ResourceRequirements `json:"resources"`
    Replicas    int32             `json:"replicas"`
    OIDC        OIDCConfig        `json:"oidc"`
    Sidecars    []SidecarSpec     `json:"sidecars,omitempty"`  // NEW
}

type SidecarSpec struct {
    Name  string `json:"name"`
    Image string `json:"image"`
    Ports []int32 `json:"ports,omitempty"`
}
```

**Step 3: Update the reconciler**

```go
// internal/controller/appdeployment_controller.go
func (r *AppDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // ... existing reconciliation ...
    
    // Ensure sidecars
    if err := r.ensureSidecars(ctx, appDep); err != nil {
        return ctrl.Result{}, err
    }
    
    return ctrl.Result{}, nil
}

func (r *AppDeploymentReconciler) ensureSidecars(ctx context.Context, appDep *platformv1alpha1.AppDeployment) error {
    for _, sidecar := range appDep.Spec.Sidecars {
        // Create or update sidecar container in StatefulSet
    }
    return nil
}
```

**Step 4: Update the StatefulSet builder**

```go
// internal/controller/resources.go
func buildStatefulSet(appDep *platformv1alpha1.AppDeployment) (*appsv1.StatefulSet, error) {
    containers := []corev1.Container{appContainer(appDep)}
    
    // Add sidecars
    for _, sidecar := range appDep.Spec.Sidecars {
        containers = append(containers, corev1.Container{
            Name:  sidecar.Name,
            Image: sidecar.Image,
            Ports: []corev1.ContainerPort{
                {ContainerPort: port},
            },
        })
    }
    
    // ... rest of StatefulSet spec
}
```

**Step 5: Regenerate and test**

```bash
make manifests kustomize
docker build -t helmsman-operator:dev -f Dockerfile .
kind load docker-image helmsman-operator:dev --name helmsman-onprem
kubectl set image deployment/helmsman-operator \
  -n helmsman-operator manager=helmsman-operator:dev \
  --context kind-helmsman-onprem
kubectl patch deployment helmsman-operator -n helmsman-operator \
  --context kind-helmsman-onprem \
  --type='json' \
  -p='[{"op": "add", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]'
kubectl rollout status deployment/helmsman-operator -n helmsman-operator \
  --context kind-helmsman-onprem --timeout=120s
```

---

## 9. Debugging Checklist

When something goes wrong, follow this order:

### 9.1 Operator Not Running

```bash
# Check pod status
kubectl get pods -n helmsman-operator --context kind-helmsman-onprem

# Check logs
kubectl logs -n helmsman-operator deployment/helmsman-operator \
  --context kind-helmsman-onprem --tail=50

# Common issues:
# - CrashLoopBackOff → check RBAC, missing permissions
# - ImagePullBackOff → check image name, imagePullPolicy
# - Pending → check node resources, PVCs
```

### 9.2 AppDeployment Not Reconciling

```bash
# Check AppDeployment status
kubectl get appdeployment -n <ns> --context <ctx>
kubectl describe appdeployment <app> -n <ns> --context <ctx>

# Check operator logs for reconciliation errors
kubectl logs -n helmsman-operator deployment/helmsman-operator \
  --context kind-helmsman-onprem --tail=50

# Trigger manual reconciliation
kubectl annotate appdeployment <app> -n <ns> \
  reconcile-trigger="$(date +%s)" --overwrite
```

### 9.3 Resources Not Created

```bash
# Check what the operator created
kubectl get all,configmap,networkpolicy,serviceaccount,externalsecret \
  -n <ns> --context <ctx> -l app.kubernetes.io/name=<app>

# Check for errors in operator logs
kubectl logs -n helmsman-operator deployment/helmsman-operator \
  --context kind-helmsman-onprem | grep -i error

# Check RBAC
kubectl auth can-i create statefulsets \
  --as=system:serviceaccount:helmsman-operator:helmsman-operator \
  --context kind-helmsman-onprem
```

### 9.4 ExternalSecret Not Syncing

```bash
# Check ExternalSecret status
kubectl get externalsecret <app>-oidc -n <ns> --context <ctx>
kubectl describe externalsecret <app>-oidc -n <ns> --context <ctx>

# Check ESO controller logs
kubectl logs -n external-secrets \
  -l app.kubernetes.io/name=external-secrets \
  --context <ctx> --tail=50

# Check ClusterSecretStore
kubectl get clustersecretstore vault-backend -o yaml --context <ctx>

# Test Vault connectivity from ESO pod
kubectl run -n external-secrets --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 \
  --context <ctx> vault-test -- \
  curl -sH "X-Vault-Token: root" http://vault.vault:8200/v1/secret/data/apps/<app>/oidc
```

### 9.5 Keycloak Issues

```bash
# Test Keycloak connectivity from operator
kubectl run -n helmsman-operator --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 \
  --context kind-helmsman-onprem kc-test -- \
  curl -s http://keycloak.keycloak:80/realms/helmsman/.well-known/openid-configuration | head -1

# Check Keycloak pod
kubectl get pods -n keycloak --context <ctx>
kubectl logs -n keycloak -l app.kubernetes.io/name=keycloak --context <ctx> --tail=50

# List clients
TOKEN=$(curl -s -d "username=admin&password=helmsman123&grant_type=password&client_id=admin-cli" \
  http://localhost:8081/realms/master/protocol/openid-connect/token | jq -r '.access_token')
curl -sH "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients?clientId=<app>" | jq
```

### 9.6 Network Connectivity Issues

```bash
# Test from app namespace
kubectl run -n <ns> --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 \
  --context <ctx> net-test -- \
  curl -s --connect-timeout 5 http://keycloak.keycloak:80/realms/helmsman/

# Check NetworkPolicies
kubectl get networkpolicy -n <ns> --context <ctx>
kubectl describe networkpolicy <app>-default-deny -n <ns> --context <ctx>

# Temporarily remove NetworkPolicies for debugging
kubectl delete networkpolicy -n <ns> -l app.kubernetes.io/name=<app> --context <ctx>
```

---

## Appendix A: Complete File Reference

### ArgoCD Configs
- `argocd/helmsman-appdeployments.yaml` — ArgoCD Application for AppDeployment CRs
- `argocd/argocd-project.yaml` — ArgoCD Project with cluster restrictions

### Operator Code
- `cmd/main.go` — Entrypoint, manager creation, signal handling
- `internal/controller/appdeployment_controller.go` — Controller setup, reconciliation, finalizers
- `internal/controller/keycloak.go` — Keycloak admin API, Vault writes
- `internal/controller/resources.go` — StatefulSet, Service, NetworkPolicy builders

### Manifests
- `config/crd/bases/appdeployments.platform.helmsman.dev.yaml` — AppDeployment CRD
- `config/rbac/role.yaml` — Generated ClusterRole
- `config/rbac/role_binding.yaml` — Generated ClusterRoleBinding
- `config/manager/manager.yaml` — Operator Deployment
- `config/default/kustomization.yaml` — Root kustomization

### Scripts
- `Makefile` — Build, deploy, test targets
- `scripts/local-dev.sh` — Local development workflow
- `scripts/recover-after-docker-restart.sh` — Full environment recovery

### Documentation
- `INCIDENT_RUNBOOK.md` — Detailed incident history and fixes
- `HOMELAB_OPERATIONS_GUIDE.md` — Beginner-friendly operations guide

---

## Appendix B: Common Patterns Used

### Pattern: Controller with Finalizer

```go
// Ensure finalizer exists
if !controllerutil.ContainsFinalizer(appDep, finalizerName) {
    controllerutil.AddFinalizer(appDep, finalizerName)
}

// Handle deletion
if !appDep.DeletionTimestamp.IsZero() {
    return r.handleDeletion(ctx, appDep)
}

// Normal reconciliation
return r.normalReconcile(ctx, appDep)
```

### Pattern: Owner References

```go
// Set owner so child is deleted when parent is deleted
if err := controllerutil.SetControllerReference(appDep, child, r.Scheme); err != nil {
    return err
}
```

### Pattern: Create or Skip

```go
func createOrSkip[T any](ctx context.Context, c client.Client, desired, existing T) error {
    // Try to create
    if err := c.Create(ctx, desired); err != nil {
        if apierrors.IsAlreadyExists(err) {
            // Update if exists
            return c.Update(ctx, desired)
        }
        return err
    }
    return nil
}
```

### Pattern: Unstructured Client for External CRDs

```go
// When you don't want to import external CRD types
gvk := schema.GroupVersionKind{
    Group:   "external-secrets.io",
    Version: "v1beta1",
    Kind:    "ExternalSecret",
}
desired := &unstructured.Unstructured{}
desired.SetGroupVersionKind(gvk)
desired.SetName(name)
desired.SetNamespace(namespace)
// ... set spec via map[string]interface{}
```

---

## Appendix C: Script Design Principles

1. **Idempotent** — Running a script twice produces the same result
2. **Fail-fast** — Scripts exit on first error (`set -e`)
3. **Explicit context** — Always pass `--context` to kubectl commands
4. **Wait for readiness** — Use `kubectl wait` and `kubectl rollout status`
5. **No secrets in code** — Use `--from-literal` for secrets, never hardcode
6. **Version pinning** — Pin chart versions, image tags, and tool versions
7. **Cleanup on failure** — Scripts should clean up partially-created resources

---

*This document is specific to the helmsman-operator repository and reflects the actual implementation as of commit ab60aef.*

