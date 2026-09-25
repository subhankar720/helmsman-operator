# Enterprise Kubernetes & AI Platform Guide

> **Audience:** Developers, platform engineers, and AI engineers who are new to Kubernetes, GitOps, and serving LLMs at enterprise scale.
> **Goal:** Start from the homelab you already have, then scale that same pattern to multiple environments, teams, clouds, and AI workloads — with production-grade CI/CD, agents, and observability.

---

## Table of Contents

1. [Foundations](#1-foundations)
2. [Homelab Architecture Deep Dive](#2-homelab-architecture-deep-dive)
3. [Multi-Environment Strategy](#3-multi-environment-strategy)
4. [GitOps & CI/CD Pipelines](#4-gitops--ci-cd-pipelines)
5. [Deployment Agents & Debugging Agents](#5-deployment-agents--debugging-agents)
6. [Serving LLMs on Kubernetes](#6-serving-llms-on-kubernetes)
7. [Security, Governance & Observability](#7-security-governance--observability)
8. [Enterprise Day-2 Operations](#8-enterprise-day-2-operations)
9. [Hands-On Roadmap](#9-hands-on-roadmap)

---

## 1. Foundations

### 1.1 What is Kubernetes?

Kubernetes (K8s) is a **container orchestration platform**. Think of it as an operating system for your data center or cloud:
- **Nodes** = machines (physical or virtual)
- **Pods** = small groups of containers that run together
- **Deployments/StatefulSets** = declare *how many* copies of a Pod should run
- **Services** = stable network names and load balancers for Pods
- **ConfigMaps/Secrets** = configuration and credentials
- **Operators** = software that extends Kubernetes with custom logic

### 1.2 Why an Operator?

Most applications are stateless. But AI platforms have stateful, complex components:
- Models that need persistent storage
- Secrets that must rotate
- Backends that must register with identity providers
- Network policies that must stay synchronized

An **Operator** packages that operational knowledge as code. The `AppDeployment` CR in this repo is a perfect example: you declare an app once, and the operator creates the StatefulSet, Service, ConfigMap, ExternalSecret, Keycloak client, NetworkPolicies, and Fluent Bit config automatically.

### 1.3 Core Concepts Used Here

| Concept | Where Used | Purpose |
|---------|-----------|---------|
| **Custom Resource Definition (CRD)** | `AppDeployment` | Adds a new kind of object to Kubernetes |
| **Controller / Reconciler** | Operator | Watches CRDs and makes cluster state match desired state |
| **Finalizer** | AppDeployment | Ensures cleanup happens before deletion |
| **Owner Reference** | All created resources | Ties child resources to parent CR for automatic GC |
| **ConfigMap** | Fluent Bit config | Non-secret configuration |
| **Secret** | Platform config, Vault sync | Sensitive configuration |
| **ServiceAccount** | Operator, app pods | Identity for pods |
| **NetworkPolicy** | Per-app egress/ingress | Zero-trust network segmentation |
| **External Secrets Operator (ESO)** | Vault → K8s Secret | Bridges external secret stores into Kubernetes |
| **ArgoCD** | GitOps | Continuously reconciles cluster state from Git |

---

## 2. Homelab Architecture Deep Dive

### 2.1 Component Diagram

```
┌─────────────────────────────────────────────────────────────────────────┐
│                            kind-helmsman-hub                            │
│                         (Production-like cluster)                       │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────────────────┐  │
│  │   ArgoCD    │  │   Keycloak   │  │      External Secrets         │  │
│  │  (GitOps)   │  │  (Identity)  │  │         Operator             │  │
│  └──────┬──────┘  └──────┬──────┘  └──────────────┬───────────────┘  │
│         │                │                         │                   │
│  ┌──────┴────────────────┴─────────────────────────┴───────────────┐  │
│  │              helmsman-operator (controller)                     │  │
│  │  Watches AppDeployment CRs → creates StatefulSets, Services,   │  │
│  │  ConfigMaps, NetworkPolicies, ExternalSecrets, Keycloak clients│  │
│  └──────────────────────────────────────────────────────────────────┘  │
│                                                                         │
│  ┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐   │
│  │   sample-app    │    │   sample-app    │    │   sample-app    │   │
│  │   StatefulSet   │    │   Service       │    │   NetworkPolicy │   │
│  │  ┌────────────┐ │    │  (ClusterIP)    │    │  (default-deny) │   │
│  │  │ app (8080) │ │    └────────┬────────┘    └─────────────────┘   │
│  │  │ adc (4180) │ │             │ port 80                              │
│  │  │ fluent-bit │ │             │                                      │
│  │  └────────────┘ │             │                                      │
│  └─────────────────┘             │                                      │
│                                   │                                      │
│  ┌────────────────────────────────┘                                      │
│  │  oauth2-proxy → Keycloak (OIDC)                                       │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                                                         │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  Vault (secret storage)                                           │   │
│  │  Path: secret/data/apps/<app>/oidc                                │   │
│  └──────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│                          kind-helmsman-onprem                           │
│                         (Development / testing)                         │
│  Same stack as hub, but operator may run locally (go run) or in-cluster │
└─────────────────────────────────────────────────────────────────────────┘
```

### 2.2 Data Flow

1. **You** create an `AppDeployment` CR in Git
2. **ArgoCD** syncs it to the cluster
3. **Operator** reconciles:
   - Creates Keycloak OIDC client → writes creds to Vault
   - Creates StatefulSet, Services, ConfigMap, NetworkPolicies
   - Creates ExternalSecret → ESO syncs Vault → K8s Secret
   - Pods start with OIDC sidecar (`adc`) and Fluent Bit sidecar
4. **Users** access the app through the Service, authenticated by Keycloak via `adc`

### 2.3 Dual URL Architecture

Keycloak needs two URLs because:
- **Admin URL** (`keycloak-url`): Used by the **operator** for management (token endpoint, client CRUD). Must be reachable from where the operator runs.
- **OIDC URL** (`keycloak-oidc-url`): Used by **pods** for OIDC discovery (`issuer-url`). Must be reachable from inside the cluster.

```yaml
# helmsman-platform-config secret
keycloak-url: http://keycloak.keycloak:80          # operator → Keycloak admin
keycloak-oidc-url: http://keycloak-headless.keycloak:8080  # pod → Keycloak OIDC
```

---

## 3. Multi-Environment Strategy

### 3.1 Environment Tiers

| Environment | Purpose | Cluster | Data | Access |
|-------------|---------|---------|------|--------|
| **dev** | Developer testing | `kind-helmsman-onprem` | Synthetic | Team |
| **staging** | Pre-production validation | `kind-helmsman-hub` or cloud | Anonymized prod-like | Wider team |
| **prod** | Production | Cloud/managed K8s | Real | Limited |

### 3.2 Environment Isolation Patterns

#### Option A: Separate Clusters (Recommended for Production)

```
┌──────────────┐   ┌──────────────┐   ┌──────────────┐
│  dev cluster │   │ staging clstr│   │  prod cluster│
│ kind-onprem  │   │  kind-hub    │   │  EKS/GKE/AKS │
└──────────────┘   └──────────────┘   └──────────────┘
```

**Pros:**
- Strong blast-radius isolation
- Independent upgrade cycles
- Clear promotion path: dev → staging → prod

**Cons:**
- More infrastructure to manage
- Higher cost

#### Option B: Namespace-Based Isolation (Good for Dev/Staging)

```
┌─────────────────────────────────────┐
│         Single Cluster               │
│  ┌─────────┐ ┌─────────┐ ┌──────┐ │
│  │ ns-dev  │ │ ns-stg  │ │ns-prod│ │
│  └─────────┘ └─────────┘ └──────┘ │
└─────────────────────────────────────┘
```

**Pros:**
- Lower cost
- Easier to share resources

**Cons:**
- Weaker isolation (namespace admin can affect others)
- Resource quotas required

### 3.3 Promoting Changes Across Environments

Use **Git branches** or **directory overlays** to promote:

```
repo/
├── bases/
│   └── appdeployments/
│       └── sample-app.yaml        # Base definition
├── environments/
│   ├── dev/
│   │   └── kustomization.yaml     # Dev overlays (replicas: 1)
│   ├── staging/
│   │   └── kustomization.yaml     # Staging overlays (replicas: 2)
│   └── prod/
│       └── kustomization.yaml     # Prod overlays (replicas: 5)
```

With Kustomize or Helm, each environment gets its own values while sharing the base definition.

### 3.4 Platform Config Per Environment

Each environment needs its own `helmsman-platform-config` secret:

```bash
# dev
kubectl create secret generic helmsman-platform-config \
  -n sample-app \
  --from-literal=keycloak-url=http://keycloak.keycloak:80 \
  --from-literal=keycloak-oidc-url=http://keycloak-headless.keycloak:8080 \
  --from-literal=vault-url=http://vault.vault:8200 \
  --context kind-helmsman-onprem

# staging
kubectl create secret generic helmsman-platform-config \
  -n sample-app \
  --from-literal=keycloak-url=https://keycloak.staging.example.com \
  --from-literal=keycloak-oidc-url=https://keycloak.staging.example.com \
  --from-literal=vault-url=https://vault.staging.example.com \
  --context kind-helmsman-hub

# prod
kubectl create secret generic helmsman-platform-config \
  -n sample-app \
  --from-literal=keycloak-url=https://keycloak.prod.example.com \
  --from-literal=keycloak-oidc-url=https://keycloak.prod.example.com \
  --from-literal=vault-url=https://vault.prod.example.com \
  --context prod-cluster
```

---

## 4. GitOps & CI/CD Pipelines

### 4.1 GitOps Principles

1. **Git is the single source of truth** — all cluster state is declared in Git
2. **Automated sync** — ArgoCD continuously reconciles Git → cluster
3. **Pull-based** — cluster pulls changes from Git (not CI pushing to cluster)
4. **Immutable infrastructure** — changes happen via new commits, not manual kubectl

### 4.2 ArgoCD Application Structure

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: helmsman-platform
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/your-org/helmsman-fleet.git
    targetRevision: main
    path: environments/prod
  destination:
    server: https://kubernetes.default.svc
    namespace: helmsman-operator-system
  syncPolicy:
    automated:
      prune: true      # Delete resources removed from Git
      selfHeal: true    # Fix drift between Git and cluster
    syncOptions:
    - CreateNamespace=true
```

### 4.3 CI/CD Pipeline for AppDeployment Changes

```yaml
# .github/workflows/appdeployment-promote.yml
name: AppDeployment Promotion Pipeline

on:
  push:
    branches: [main]
    paths:
      - 'appdeployments/**'

jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Validate YAML
        run: |
          for f in appdeployments/*.yaml; do
            kubectl apply --dry-run=client -f "$f"
          done

  test:
    needs: validate
    runs-on: ubuntu-latest
    steps:
      - name: Deploy to dev
        run: |
          kubectl apply -f appdeployments/sample-app.yaml \
            -n sample-app --context kind-helmsman-onprem
      - name: Wait for ready
        run: |
          kubectl wait --for=condition=Ready pod \
            -l app.kubernetes.io/name=sample-app \
            -n sample-app --context kind-helmsman-onprem --timeout=300s
      - name: Run integration tests
        run: |
          kubectl exec -n sample-app \
            $(kubectl get pod -n sample-app -l app=sample-app -o jsonpath='{.items[0].metadata.name}') \
            --context kind-helmsman-onprem \
            -- wget -q -O - http://localhost:8080/health

  promote-staging:
    needs: test
    runs-on: ubuntu-latest
    environment: staging
    steps:
      - name: Deploy to staging
        run: |
          kubectl apply -f appdeployments/sample-app.yaml \
            -n sample-app --context kind-helmsman-hub

  promote-prod:
    needs: promote-staging
    runs-on: ubuntu-latest
    environment: production
    steps:
      - name: Approve production deployment
        uses: trstringer/manual-approval@v1
        with:
          secret: ${{ secrets.GITHUB_TOKEN }}
          approvers: platform-team-lead,security-lead
      - name: Deploy to prod
        run: |
          kubectl apply -f appdeployments/sample-app.yaml \
            -n sample-app --context prod-cluster
```

### 4.4 Image Promotion Pattern

```
┌─────────────┐   ┌─────────────┐   ┌─────────────┐
│  dev image  │ → │ staging img │ → │  prod image │
│  :dev       │   │  :staging   │   │  :v1.2.3    │
└─────────────┘   └─────────────┘   └─────────────┘
```

In your `AppDeployment`:
```yaml
# environments/dev/kustomization.yaml
images:
- name: ghcr.io/your-org/your-app
  newName: ghcr.io/your-org/your-app
  newTag: dev

# environments/prod/kustomization.yaml
images:
- name: ghcr.io/your-org/your-app
  newName: ghcr.io/your-org/your-app
  newTag: v1.2.3  # Pinned, immutable tag
```

---

## 5. Deployment Agents & Debugging Agents

### 5.1 What is a Deployment Agent?

A **deployment agent** is a controller that runs in the cluster and handles deployment-specific logic:
- Rolling updates with health checks
- Canary deployments
- Blue-green deployments
- Automated rollback on failure

With the `AppDeployment` operator, you already have a form of deployment agent. To extend it:

```go
// In appdeployment_controller.go
func (r *AppDeploymentReconciler) rolloutRestartIfNeeded(ctx context.Context, appDep *platformv1alpha1.AppDeployment) error {
    // Check if rollout is needed based on image hash or config change
    // Trigger rolling restart
    // Wait for new pod to be ready
    // Update status
}
```

### 5.2 Debugging Agent Pattern

A **debugging agent** is a sidecar or DaemonSet that captures diagnostic information:

```yaml
# Debug sidecar for a pod
containers:
- name: app
  image: your-app:latest
- name: debug-agent
  image: busybox:latest
  command: ["sh", "-c", "while true; do sleep 3600; done"]
  volumeMounts:
  - name: debug-logs
    mountPath: /var/log/app
  - name: debug-config
    mountPath: /etc/app/config
```

Or as a DaemonSet for cluster-wide debugging:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: node-debug-agent
  namespace: kube-system
spec:
  selector:
    matchLabels:
      app: node-debug-agent
  template:
    metadata:
      labels:
        app: node-debug-agent
    spec:
      containers:
      - name: agent
        image: your-org/node-debug-agent:latest
        securityContext:
          privileged: true
        volumeMounts:
        - name: host-root
          mountPath: /host
        - name: docker-socket
          mountPath: /var/run/docker.sock
      volumes:
      - name: host-root
        hostPath:
          path: /
      - name: docker-socket
        hostPath:
          path: /var/run/docker.sock
```

### 5.3 Using kubectl-debug for Ephemeral Debugging

```bash
# Install kubectl-debug
kubectl krew install debug

# Attach debug container to a running pod
kubectl debug -it sample-app-0 -n sample-app --image=busybox --target=app

# Capture network trace
kubectl debug -it sample-app-0 -n sample-app --image=nicolaka/netshoot \
  --target=app -- tcpdump -i any -w /tmp/capture.pcap
```

### 5.4 Operator-Level Debugging

The operator itself can emit debug information:

```go
// In keycloak.go
func (r *AppDeploymentReconciler) debugKeycloakClient(ctx context.Context, appName string) {
    token, _ := getKeycloakAdminToken(cfg)
    clients, _ := listKeycloakClients(token, realmURL)
    r.Log.Info("Keycloak clients", "clients", clients, "appName", appName)
}
```

Enable debug logging:
```bash
kubectl set env deployment/helmsman-operator \
  -n helmsman-operator \
  --context kind-helmsman-onprem \
  LOG_LEVEL=debug
```

---

## 6. Serving LLMs on Kubernetes

### 6.1 LLM Serving Patterns

There are three main patterns for serving LLMs on Kubernetes:

| Pattern | Use Case | Tooling |
|---------|----------|---------|
| **vLLM** | High-throughput inference | vLLM + K8s |
| **TGI** | Hugging Face models | Text Generation Inference |
| **Ollama** | Local/dev models | Ollama + K8s |
| **KServe** | Enterprise model serving | KServe + Knative |

### 6.2 Basic LLM Serving with Ollama

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ollama-models-pvc
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 20Gi
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: ollama
spec:
  serviceName: ollama
  replicas: 1
  selector:
    matchLabels:
      app: ollama
  template:
    metadata:
      labels:
        app: ollama
    spec:
      containers:
      - name: ollama
        image: ollama/ollama:latest
        ports:
        - containerPort: 11434
        volumeMounts:
        - name: models
          mountPath: /root/.ollama
        resources:
          limits:
            nvidia.com/gpu: 1  # Requires NVIDIA device plugin
  volumeClaimTemplates:
  - metadata:
      name: models
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 20Gi
---
apiVersion: v1
kind: Service
metadata:
  name: ollama
spec:
  selector:
    app: ollama
  ports:
  - port: 11434
    targetPort: 11434
```

### 6.3 LLM Serving with KServe (Enterprise)

KServe provides:
- Serverless inference with Knative
- Autoscaling to zero
- Canary deployments
- Explainability and monitoring

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: llama-2-7b
spec:
  predictor:
    model:
      modelFormat:
        name: vLLM
      storageUri: "s3://models/llama-2-7b"
      resources:
        limits:
          memory: "32Gi"
          cpu: "8"
          nvidia.com/gpu: "1"
```

### 6.4 AI Agent Architecture on Kubernetes

```
┌─────────────────────────────────────────────────────────────┐
│                      Kubernetes Cluster                       │
│                                                              │
│  ┌────────────────┐    ┌────────────────┐                   │
│  │  Agent Pod 1   │    │  Agent Pod 2   │                   │
│  │ ┌────────────┐ │    │ ┌────────────┐ │                   │
│  │ │ LangChain   │ │    │ │ LangChain   │ │                   │
│  │ │ Agent       │ │    │ │ Agent       │ │                   │
│  │ └──────┬─────┘ │    │ └──────┬─────┘ │                   │
│  │        │       │    │        │       │                   │
│  │ ┌──────▼─────┐ │    │ ┌──────▼─────┐ │                   │
│  │ │ vLLM       │ │    │ │ vLLM       │ │                   │
│  │ │ (LLM)      │ │    │ │ (LLM)      │ │                   │
│  │ └────────────┘ │    │ └────────────┘ │                   │
│  └────────────────┘    └────────────────┘                   │
│         │                      │                             │
│  ┌──────▼──────────────────────▼─────┐                      │
│  │     Vector DB (Pinecone/Milvus)    │                      │
│  └───────────────────────────────────┘                      │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │              API Gateway / Ingress                     │   │
│  │  (authenticates via Keycloak, routes to agents)       │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

### 6.5 Extending AppDeployment for AI Workloads

```yaml
apiVersion: platform.helmsman.dev/v1alpha1
kind: AppDeployment
metadata:
  name: ai-agent
  namespace: ai-agents
spec:
  appName: ai-agent
  owningTeam: ai-platform
  costCenterId: "000002"
  image:
    repository: ghcr.io/your-org/ai-agent
    tag: v1.0.0
    pullPolicy: IfNotPresent
  container:
    port: 8080
    command: ["python", "-m", "ai_agent.main"]
    env:
      OPENAI_API_KEY: ""  # Injected from Vault
      MODEL_NAME: "llama-2-7b"
  resources:
    requests:
      cpu: "2000m"
      memory: "8Gi"
      nvidia.com/gpu: "1"
    limits:
      cpu: "4000m"
      memory: "16Gi"
      nvidia.com/gpu: "1"
  replicas: 2
  oidc:
    enabled: true
  # New fields for AI workloads
  ai:
    model: llama-2-7b
    vectorDB: pinecone
    embeddingModel: all-MiniLM-L6-v2
```

---

## 7. Security, Governance & Observability

### 7.1 Zero-Trust Networking

The NetworkPolicies created by the operator implement zero-trust:
- **Default deny all** ingress and egress
- **Allow only required ports** (HTTP, DNS, Keycloak, Vault)
- **No lateral movement** between namespaces by default

Extend this with:
```yaml
# Deny all cross-namespace traffic by default
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-cross-namespace
  namespace: sample-app
spec:
  podSelector: {}
  policyTypes:
  - Ingress
  - Egress
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: sample-app
  egress:
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: sample-app
```

### 7.2 Secret Management

All secrets flow through Vault:
- **Operator** writes OIDC credentials to Vault
- **ESO** syncs Vault → K8s Secret
- **Pods** consume K8s Secret as environment variables or files
- **Never** commit secrets to Git

### 7.3 Observability Stack

```
┌──────────────────────────────────────────────────────────┐
│                     Observability Stack                     │
│                                                            │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────┐   │
│  │ Prometheus  │  │   Grafana   │  │     Alertmanager│   │
│  │ (metrics)   │  │ (dashboards)│  │   (alerts)      │   │
│  └──────┬──────┘  └──────┬──────┘  └─────────────────┘   │
│         │                │                                │
│  ┌──────▼────────────────▼─────────────────────────┐     │
│  │              Metrics Server / kube-state         │     │
│  └──────────────────────────────────────────────────┘     │
│                                                            │
│  ┌────────────────┐  ┌────────────────┐                   │
│  │  Fluent Bit    │  │  Elasticsearch │  ┌─────────────┐ │
│  │ (log shipper)  │→ │  / OpenSearch  │  │   Kibana    │ │
│  └────────────────┘  └────────────────┘  └─────────────┘ │
│                                                            │
│  ┌────────────────────────────────────────────────────┐   │
│  │              Jaeger / Tempo (tracing)              │   │
│  └────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────┘
```

### 7.4 Policy Enforcement with Kyverno or OPA Gatekeeper

```yaml
# Kyverno policy: require resource requests/limits
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: require-resource-requests
spec:
  validationFailureAction: enforce
  rules:
  - name: require-resources
    match:
      any:
      - resources:
          kinds:
          - Pod
    validate:
      message: "CPU and memory limits are required"
      pattern:
        spec:
          containers:
          - resources:
              requests:
                memory: "?*"
                cpu: "?*"
              limits:
                memory: "?*"
                cpu: "?*"
```

---

## 8. Enterprise Day-2 Operations

### 8.1 Backup and Disaster Recovery

| Component | Backup Strategy | Restore Procedure |
|-----------|-----------------|-------------------|
| **etcd** (K8s state) | Regular etcd snapshots | Restore etcd from snapshot |
| **Vault** | Raft snapshots + S3 | `vault operator raft snapshot restore` |
| **Keycloak** | DB export + realm export | Import realm JSON |
| **ArgoCD** | Git is source of truth | Re-run `argocd app sync` |
| **AppDeployment CRs** | Git repositories | `git push` restores |

### 8.2 Upgrading the Stack

```bash
# 1. Upgrade ESO
helm upgrade external-secrets external-secrets/external-secrets \
  -n external-secrets \
  --set webhook.port=9443

# 2. Upgrade Keycloak
helm upgrade keycloak bitnami/keycloak \
  -n keycloak \
  --set auth.adminPassword=helmsman123

# 3. Upgrade Operator
# Update code, build image, load to kind, rollout restart
```

### 8.3 Cost Management

```bash
# View resource usage by namespace
kubectl top namespace --context kind-helmsman-hub

# View resource usage by pod
kubectl top pod --all-namespaces --context kind-helmsman-hub

# Set resource quotas per namespace
kubectl apply -f - <<EOF
apiVersion: v1
kind: ResourceQuota
metadata:
  name: sample-app-quota
  namespace: sample-app
spec:
  hard:
    requests.cpu: "4"
    requests.memory: 8Gi
    limits.cpu: "8"
    limits.memory: 16Gi
    pods: "10"
EOF
```

### 8.4 Access Control

```bash
# Create team namespace with limited access
kubectl create namespace team-alpha

# Give team-alpha edit access to their namespace
kubectl create rolebinding team-alpha-admin \
  --clusterrole=edit \
  --serviceaccount=team-alpha:default \
  -n team-alpha

# Deny access to other namespaces (NetworkPolicy)
kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-other-namespaces
  namespace: team-alpha
spec:
  podSelector: {}
  policyTypes:
  - Ingress
  - Egress
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: team-alpha
  egress:
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: team-alpha
EOF
```

---

## 9. Hands-On Roadmap

### Week 1: Get Comfortable with Kubernetes
- [ ] Deploy the homelab (`kind create cluster`, install Keycloak, Vault, operator)
- [ ] Create your first `AppDeployment` CR
- [ ] Use `kubectl get`, `describe`, `logs`, `exec` to inspect resources
- [ ] Break something and fix it using the runbook

### Week 2: Understand GitOps
- [ ] Set up ArgoCD on the hub cluster
- [ ] Create an Application pointing to your Git repo
- [ ] Make a change in Git and watch ArgoCD sync it
- [ ] Try `kubectl diff` to preview changes

### Week 3: Extend the Operator
- [ ] Add a new field to `AppDeployment` (e.g., `sidecars`)
- [ ] Update the operator to create the sidecar container
- [ ] Build and test locally with `docker build` + `kind load`
- [ ] Push and watch ArgoCD deploy the new operator version

### Week 4: Add AI Workloads
- [ ] Deploy Ollama or vLLM on the cluster
- [ ] Create an `AppDeployment` for an AI agent
- [ ] Connect the agent to the LLM and test inference
- [ ] Add a Vector DB for RAG (Retrieval-Augmented Generation)

### Week 5: Enterprise Hardening
- [ ] Set up Prometheus + Grafana for metrics
- [ ] Set up Fluent Bit + OpenSearch for logs
- [ ] Add Kyverno policies for security
- [ ] Create backup scripts for Vault and Keycloak
- [ ] Document run procedures

### Week 6: Multi-Environment
- [ ] Create a second Kind cluster or cloud cluster
- [ ] Set up cross-cluster ArgoCD
- [ ] Implement promotion pipeline: dev → staging → prod
- [ ] Test disaster recovery: restore from backups

---

## Appendix A: Quick Reference Cheatsheet

### kubectl Essentials
```bash
# Get resources
kubectl get pods -n <ns> --context <ctx>
kubectl get all,networkpolicies,serviceaccount -n <ns>

# Describe for debugging
kubectl describe pod <pod> -n <ns>
kubectl describe appdeployment <app> -n <ns>

# Logs
kubectl logs <pod> -n <ns> -c <container> --tail=100
kubectl logs -f <pod> -n <ns> -c <container>  # Follow

# Exec into pod
kubectl exec -it <pod> -n <ns> -- /bin/sh

# Port forward
kubectl port-forward -n <ns> svc/<svc> 8080:80
```

### Operator Development
```bash
# Build locally
go build -o /tmp/helmsman-operator ./cmd/main.go

# Build Docker image
docker build -t helmsman-operator:dev -f Dockerfile .

# Load into Kind
kind load docker-image helmsman-operator:dev --name <cluster>

# Update deployment
kubectl set image deployment/<deploy> <container>=helmsman-operator:dev
kubectl patch deployment <deploy> --type='json' \
  -p='[{"op": "add", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]'
```

### ArgoCD
```bash
# Login
argocd login localhost:8080 --insecure --username admin --password <pass>

# Sync app
argocd app sync <app-name>
argocd app get <app-name>
```

### Vault
```bash
# Read secret
curl -sH "X-Vault-Token: root" http://localhost:8082/v1/secret/data/apps/<app>/oidc | jq

# Write secret
curl -sX POST -H "X-Vault-Token: root" -H "Content-Type: application/json" \
  -d '{"data": {"key": "value"}}' http://localhost:8082/v1/secret/data/path
```

### Keycloak
```bash
# Get admin token
TOKEN=$(curl -s -d "username=admin&password=helmsman123&grant_type=password&client_id=admin-cli" \
  http://localhost:8081/realms/master/protocol/openid-connect/token | jq -r '.access_token')

# List clients
curl -sH "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients" | jq

# Create realm
curl -sX POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"realm":"myrealm","enabled":true}' \
  http://localhost:8081/admin/realms
```

---

## Appendix B: Further Reading

### Kubernetes
- **Kubernetes Docs:** https://kubernetes.io/docs/
- **Kubebuilder:** https://book.kubebuilder.io/ (for building operators)
- **ArgoCD:** https://argo-cd.readthedocs.io/

### GitOps
- **GitOps Principles:** https://opengitops.dev/
- **ArgoCD Best Practices:** https://argo-cd.readthedocs.io/en/stable/user-guide/best_practices/

### AI/LLM on Kubernetes
- **KServe:** https://kserve.github.io/website/
- **vLLM:** https://docs.vllm.ai/
- **Ollama:** https://ollama.ai/
- **LangChain:** https://python.langchain.com/

### Observability
- **Prometheus:** https://prometheus.io/docs/
- **Grafana:** https://grafana.com/docs/
- **OpenSearch:** https://opensearch.org/docs/

### Security
- **Kyverno:** https://kyverno.io/docs/
- **OPA Gatekeeper:** https://open-policy-agent.github.io/gatekeeper/
- **Pod Security Standards:** https://kubernetes.io/docs/concepts/security/pod-security-standards/

---

## Appendix C: Common Pitfalls for Newcomers

1. **Forgetting `imagePullPolicy: Never`** when using local images in Kind
2. **Mixing contexts** — always specify `--context kind-helmsman-onprem` or `--context kind-helmsman-hub`
3. **Force-deleting CRDs** — breaks Helm ownership; use `helm uninstall` instead
4. **Missing RBAC** — operator can't function without proper ClusterRole/ClusterRoleBinding
5. **Single URL for Keycloak** — always set both `keycloak-url` and `keycloak-oidc-url`
6. **NetworkPolicy without egress** — `default-deny` blocks everything; add rules for Keycloak, Vault, DNS
7. **ESO CRD version mismatch** — ensure `v1beta1` is served if operator uses it, or update to `v1`
8. **Not using Owner References** — child resources become orphaned when parent is deleted
9. **Hardcoding secrets in Git** — always use Vault + ESO
10. **No resource limits** — always set requests/limits to prevent noisy neighbors

---

*This guide was built from real production debugging sessions on the helmsman-operator homelab. Every section has been validated against actual cluster state and operator behavior.*

