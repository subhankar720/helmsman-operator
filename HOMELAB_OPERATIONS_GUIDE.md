# Homelab Operations Guide

Welcome to the Helmsman homelab. This guide covers everything you need to know to set up, operate, and troubleshoot the environment.

---

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [Environment Setup](#environment-setup)
3. [Hub Cluster Setup](#hub-cluster-setup)
4. [Teardown & Cleanup](#teardown--cleanup)
5. [Docker Desktop Restart Procedure](#docker-desktop-restart-procedure)
6. [Common Issues & Fixes](#common-issues--fixes)
7. [Day-to-Day Operations](#day-to-day-operations)
8. [Quick Reference](#quick-reference)

---

## Architecture Overview

### Components

| Component | Purpose | Runs On | Port |
|-----------|---------|---------|------|
| Kind Cluster (`kind-helmsman-onprem`) | Kubernetes cluster for testing | Docker | - |
| Keycloak | Identity & access management | Host/Docker | 8081 |
| Vault | Secret storage | Docker | 8082 |
| Helmsman Operator | AppDeployment controller | Host (local) or in-cluster | - |
| External Secrets Operator (ESO) | Syncs Vault secrets to K8s | In-cluster | - |
| oauth2-proxy (adc) | OIDC reverse proxy for apps | In-cluster pods | 4180 |
| Fluent Bit | Log forwarding | In-cluster pods | 2020 |

### Data Flow

```
AppDeployment CR
    │
    ▼
Helmsman Operator (reconciles)
    │
    ├──► Keycloak (registers OIDC client)
    │         │
    │         ▼
    │     Writes creds to Vault
    │
    └──► Creates StatefulSet, Services, ConfigMaps
              │
              ▼
         Pods start
              │
              ├──► ExternalSecret syncs Vault → K8s Secret
              │
              └──► oauth2-proxy uses OIDC config
```

### Key Concepts

- **AppDeployment CR**: Custom Resource that defines an application. The operator watches these and creates all necessary K8s resources.
- **Platform Config Secret**: `helmsman-platform-config` in each app namespace contains Keycloak/Vault URLs and credentials.
- **Dual URL Architecture**: Keycloak needs two URLs:
  - `keycloak-url`: Used by the operator for admin operations (must be reachable from where the operator runs)
  - `keycloak-oidc-url`: Used by pods for OIDC discovery (must be reachable from inside pods)

---

## Environment Setup

### Prerequisites

```bash
# Required tools
- Docker Desktop (running)
- kubectl
- kind
- helm
- curl
- python3
- jq (optional but helpful)
- go (for operator development)
```

### Initial Cluster Creation

```bash
# Create Kind cluster with required configurations
kind create cluster --name helmsman-onprem --config kind-config.yaml

# Verify cluster is up
kubectl get nodes --context kind-helmsman-onprem
```

### Install Infrastructure Components

```bash
# 1. Install Vault
helm install vault hashicorp/vault \
  --namespace vault --create-namespace \
  --set "server.dev.enabled=true" \
  --set "server.dev.rootToken=root" \
  --set "server.service.type=NodePort" \
  --set "server.service.nodePorts.http=30082" \
  --context kind-helmsman-onprem

# Wait for Vault to be ready
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=vault \
  -n vault --context kind-helmsman-onprem --timeout=120s

# 2. Install Keycloak
helm install keycloak bitnami/keycloak \
  --namespace keycloak --create-namespace \
  --set auth.adminPassword=helmsman123 \
  --set httpPort=8081 \
  --set service.type=NodePort \
  --set service.nodePorts.http=8081 \
  --context kind-helmsman-onprem

# Wait for Keycloak to be ready
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=keycloak \
  -n keycloak --context kind-helmsman-onprem --timeout=180s

# 3. Install External Secrets Operator
helm install external-secrets external-secrets/external-secrets \
  -n external-secrets --create-namespace \
  --set webhook.port=9443 \
  --set serviceMonitor.enabled=false \
  --context kind-helmsman-onprem

# Wait for ESO to be ready
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=external-secrets \
  -n external-secrets --context kind-helmsman-onprem --timeout=120s

# 4. Create ClusterSecretStore for Vault
kubectl apply -f - --context kind-helmsman-onprem <<EOF
apiVersion: external-secrets.io/v1beta1
kind: ClusterSecretStore
metadata:
  name: vault-backend
spec:
  provider:
    vault:
      server: "http://172.18.0.2:30082"
      path: "secret"
      version: "v2"
      auth:
        tokenSecretRef:
          name: vault-token
          key: token
EOF

# Create vault-token secret
kubectl create secret generic vault-token \
  -n external-secrets \
  --from-literal=token=root \
  --context kind-helmsman-onprem
```

### Install Helmsman Operator

```bash
# Deploy the operator via Helm
helm install helmsman-operator ./helm/helmsman-operator \
  --namespace helmsman-operator-system --create-namespace \
  --context kind-helmsman-onprem

# Verify operator is running
kubectl get pods -n helmsman-operator-system --context kind-helmsman-onprem
```

### Create Sample Application Namespace

```bash
# Create namespace
kubectl create namespace sample-app --context kind-helmsman-onprem

# Create platform config secret
kubectl create secret generic helmsman-platform-config \
  -n sample-app \
  --from-literal=keycloak-url=http://localhost:8081 \
  --from-literal=keycloak-oidc-url=http://host.docker.internal:8081 \
  --from-literal=keycloak-realm=helmsman \
  --from-literal=keycloak-admin-user=admin \
  --from-literal=keycloak-admin-password=helmsman123 \
  --from-literal=vault-url=http://172.18.0.2:30082 \
  --from-literal=vault-token=root \
  --context kind-helmsman-onprem

# Create AppDeployment
kubectl apply -f config/samples/platform_v1alpha1_appdeployment.yaml \
  -n sample-app --context kind-helmsman-onprem
```

---

## Hub Cluster Setup

This section covers setting up the helmsman-operator on the `kind-helmsman-hub` cluster (production-like environment managed by ArgoCD).

### Prerequisites

- `kind-helmsman-hub` cluster running
- Keycloak installed in `keycloak` namespace
- Vault installed in `vault` namespace
- ArgoCD installed in `argocd` namespace

### 1. Install External Secrets Operator

```bash
helm install external-secrets external-secrets/external-secrets \
  -n external-secrets --create-namespace \
  --set webhook.port=9443 \
  --set serviceMonitor.enabled=false \
  --kube-context kind-helmsman-hub

# Wait for ESO to be ready
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=external-secrets \
  -n external-secrets --kube-context kind-helmsman-hub --timeout=120s
```

### 2. Create ClusterSecretStore for Vault

```bash
# Create vault-token secret in external-secrets namespace
kubectl create secret generic vault-token \
  -n external-secrets \
  --from-literal=token=root \
  --context kind-helmsman-hub

# Create ClusterSecretStore (use v1 API)
kubectl apply -f - --context kind-helmsman-hub <<EOF
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
```

**Important:** Always specify `namespace` in `tokenSecretRef` to avoid "secret not found" errors.

### 3. Deploy Helmsman Operator

```bash
# Generate manifests
cd /home/subhankar/projects/helmsman/helmsman-operator
make manifests kustomize

# Deploy to hub cluster
/home/subhankar/projects/helmsman/helmsman-operator/bin/kustomize build config/default | \
  kubectl apply --context kind-helmsman-hub -f -

# Update operator image to published version
kubectl set image deployment/helmsman-operator-controller-manager \
  -n helmsman-operator-system \
  manager=ghcr.io/subhankar720/helmsman-operator:latest \
  --context kind-helmsman-hub

# Wait for operator to be ready
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=helmsman-operator-controller-manager \
  -n helmsman-operator-system --context kind-helmsman-hub --timeout=120s
```

### 4. Fix RBAC Permissions

The default ClusterRole may be missing permissions. Add them:

```bash
# Add ConfigMap permissions
kubectl patch clusterrole helmsman-operator-manager-role --type='json' \
  -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": [""], "resources": ["configmaps"], "verbs": ["create", "delete", "get", "list", "patch", "update", "watch"]}}]' \
  --context kind-helmsman-hub

# Add CRD list permissions
kubectl patch clusterrole helmsman-operator-manager-role --type='json' \
  -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": ["apiextensions.k8s.io"], "resources": ["customresourcedefinitions"], "verbs": ["get", "list", "watch"]}}]' \
  --context kind-helmsman-hub

# Restart operator to pick up new RBAC
kubectl rollout restart deployment/helmsman-operator-controller-manager \
  -n helmsman-operator-system --context kind-helmsman-hub
```

### 5. Create Application Namespace

```bash
# Create namespace
kubectl create namespace sample-app --context kind-helmsman-hub

# Create platform config secret
# IMPORTANT: Use headless Keycloak service for OIDC URL to avoid network policy issues
kubectl create secret generic helmsman-platform-config \
  -n sample-app \
  --from-literal=keycloak-url=http://keycloak.keycloak:80 \
  --from-literal=keycloak-oidc-url=http://keycloak-headless.keycloak:8080 \
  --from-literal=keycloak-realm=helmsman \
  --from-literal=keycloak-admin-user=admin \
  --from-literal=keycloak-admin-password=helmsman123 \
  --from-literal=vault-url=http://vault.vault:8200 \
  --from-literal=vault-token=root \
  --context kind-helmsman-hub
```

**Key Points:**
- `keycloak-url`: Used by operator for admin operations → use `http://keycloak.keycloak:80`
- `keycloak-oidc-url`: Used by pods for OIDC discovery → use `http://keycloak-headless.keycloak:8080` (headless service bypasses NetworkPolicy)

### 6. Deploy via ArgoCD

```bash
# Create ArgoCD Application
argocd app create helmsman-appdeployments \
  --repo https://github.com/subhankar720/helmsman-operator.git \
  --path appdeployments \
  --dest-server https://172.18.0.6:6443 \
  --dest-namespace sample-app \
  --sync-policy automated \
  --auto-prune

# Or if Application already exists, update repo
argocd app set helmsman-appdeployments \
  --repo https://github.com/subhankar720/helmsman-operator.git \
  --path appdeployments
```

### 7. Verify Setup

```bash
# Check operator is running
kubectl get pods -n helmsman-operator-system --context kind-helmsman-hub

# Check AppDeployment was created
kubectl get appdeployment -n sample-app --context kind-helmsman-hub

# Check operator logs for reconciliation
kubectl logs -n helmsman-operator-system --context kind-helmsman-hub \
  deploy/helmsman-operator-controller-manager --tail=30

# Check resources were created
kubectl get all,configmap,externalsecret -n sample-app --context kind-helmsman-hub
```

---

## Teardown & Cleanup

### Stop Everything

```bash
# Delete Kind cluster (removes all K8s resources)
kind delete cluster --name helmsman-onprem

# Stop Keycloak and Vault containers
docker stop keycloak vault
docker rm keycloak vault

# Stop any running operator processes
pkill -f helmsman-operator || true
```

### Clean Docker Resources

```bash
# Remove unused images (optional)
docker image prune -f

# Remove Kind network (recreated on next cluster creation)
docker network rm kind
```

### Full Reset

```bash
#!/bin/bash
# save as: full-reset.sh

set -e

echo "=== Deleting Kind cluster ==="
kind delete cluster --name helmsman-onprem

echo "=== Stopping containers ==="
docker stop keycloak vault 2>/dev/null || true
docker rm keycloak vault 2>/dev/null || true

echo "=== Removing volumes (WARNING: destroys Vault data) ==="
docker volume rm kind-vault 2>/dev/null || true

echo "=== Cleaning up ==="
docker network rm kind 2>/dev/null || true
docker image prune -f

echo "=== Reset complete ==="
echo "Run setup commands from Environment Setup section to recreate."
```

---

## Docker Desktop Restart Procedure

After Docker Desktop restarts, the Kind cluster and containers are gone. Follow this procedure to restore the environment.

### Quick Recovery Script

```bash
#!/bin/bash
# save as: recover-after-docker-restart.sh

set -e

echo "=== Step 1: Start Docker Desktop ==="
# Wait for Docker to be ready
while ! docker info >/dev/null 2>&1; do
  echo "Waiting for Docker..."
  sleep 2
done
echo "Docker is ready"

echo "=== Step 2: Recreate Kind cluster ==="
kind create cluster --name helmsman-onprem --config kind-config.yaml
export CONTEXT="kind-helmsman-onprem"

echo "=== Step 3: Reinstall infrastructure ==="
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

# Wait for all pods
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=vault \
  -n vault --context $CONTEXT --timeout=120s
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=keycloak \
  -n keycloak --context $CONTEXT --timeout=180s
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=external-secrets \
  -n external-secrets --context $CONTEXT --timeout=120s

echo "=== Step 4: Recreate ClusterSecretStore ==="
kubectl apply -f - --context $CONTEXT <<EOF
apiVersion: external-secrets.io/v1beta1
kind: ClusterSecretStore
metadata:
  name: vault-backend
spec:
  provider:
    vault:
      server: "http://172.18.0.2:30082"
      path: "secret"
      version: "v2"
      auth:
        tokenSecretRef:
          name: vault-token
          key: token
EOF

kubectl create secret generic vault-token \
  -n external-secrets \
  --from-literal=token=root \
  --context $CONTEXT

echo "=== Step 5: Reinstall Helmsman Operator ==="
helm install helmsman-operator ./helm/helmsman-operator \
  --namespace helmsman-operator-system --create-namespace \
  --context $CONTEXT

echo "=== Step 6: Recreate app namespace and secrets ==="
kubectl create namespace sample-app --context $CONTEXT

kubectl create secret generic helmsman-platform-config \
  -n sample-app \
  --from-literal=keycloak-url=http://localhost:8081 \
  --from-literal=keycloak-oidc-url=http://host.docker.internal:8081 \
  --from-literal=keycloak-realm=helmsman \
  --from-literal=keycloak-admin-user=admin \
  --from-literal=keycloak-admin-password=helmsman123 \
  --from-literal=vault-url=http://172.18.0.2:30082 \
  --from-literal=vault-token=root \
  --context $CONTEXT

echo "=== Step 7: Recreate AppDeployment ==="
kubectl apply -f config/samples/platform_v1alpha1_appdeployment.yaml \
  -n sample-app --context $CONTEXT

echo "=== Step 8: Start operator locally (if developing) ==="
cd /home/subhankar/projects/helmsman/helmsman-operator
go run ./cmd/main.go --leader-elect=false &

echo "=== Recovery complete ==="
echo "Verify with: kubectl get all -n sample-app --context $CONTEXT"
```

### Important: Docker Desktop Settings

Ensure these Docker Desktop settings are configured:

1. **Resources**: At least 4 CPUs, 8GB RAM, 2GB Swap
2. **Networking**: `host.docker.internal` should resolve correctly (default in Docker Desktop)
3. **File Sharing**: Project directory `/home/subhankar/projects/helmsman` should be shared

---

## Common Issues & Fixes

### Issue 1: Pod Stuck in ContainerCreating

**Symptoms:**
```bash
kubectl describe pod <app-pod> -n <namespace>
# Events show: "MountVolume.SetUp failed for volume ... configmap ... not found"
```

**Fix:**
```bash
# Ensure operator is running
cd /home/subhankar/projects/helmsman/helmsman-operator
go run ./cmd/main.go --leader-elect=false &

# Trigger reconciliation
kubectl annotate appdeployment <app-name> \
  -n <namespace> --context kind-helmsman-onprem \
  reconcile-trigger="$(date +%s)" --overwrite
```

### Issue 2: ExternalSecret Not Syncing

**Symptoms:**
```bash
kubectl get externalsecret <app>-oidc -n <namespace>
# Shows: Ready: False, message: "Secret does not exist"
```

**Fix:**
```bash
# 1. Verify secret exists in Vault
curl -sH "X-Vault-Token: root" \
  http://localhost:8082/v1/secret/data/apps/<app-name>/oidc | python3 -m json.tool

# 2. Verify ClusterSecretStore is healthy
kubectl get clustersecretstore vault-backend -o yaml

# 3. Force sync
kubectl annotate externalsecret <app>-oidc \
  -n <namespace> --context kind-helmsman-onprem \
  force-sync="$(date +%s)" --overwrite
```

### Issue 3: oauth2-proxy Crashing (OIDC Discovery Failed)

**Symptoms:**
```bash
kubectl logs <pod> -n <namespace> -c adc
# Shows: "dial tcp ... connect: connection refused" or "i/o timeout"
```

**Root Cause:** Keycloak URL not reachable from inside cluster.

**Diagnosis:**
```bash
# Test Keycloak reachability from cluster
kubectl run -n <namespace> --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  test-keycloak -- \
  curl -s http://host.docker.internal:8081/realms/helmsman/.well-known/openid-configuration
```

**Fix:**
```bash
# Verify platform config has correct URLs
kubectl get secret helmsman-platform-config -n <namespace> \
  --context kind-helmsman-onprem \
  -o jsonpath='{.data}' | python3 -c "
import sys, json, base64
d=json.load(sys.stdin)
print('keycloak-url:', base64.b64decode(d['keycloak-url']).decode())
print('keycloak-oidc-url:', base64.b64decode(d['keycloak-oidc-url']).decode())
"

# Should show:
# keycloak-url: http://localhost:8081
# keycloak-oidc-url: http://host.docker.internal:8081

# If incorrect, update:
kubectl patch secret helmsman-platform-config -n <namespace> \
  --context kind-helmsman-onprem \
  -p '{"data":{
    "keycloak-url":"aHR0cDovL2xvY2FsaG9zdDo4MDgx",
    "keycloak-oidc-url":"aHR0cDovL2hvc3QuZG9ja2VyLmludGVybmFsOjgwODE="
  }}'

# Trigger reconciliation
kubectl annotate appdeployment <app-name> \
  -n <namespace> --context kind-helmsman-onprem \
  reconcile-trigger="$(date +%s)" --overwrite

# Restart pod
kubectl delete pod <pod-name> -n <namespace> --context kind-helmsman-onprem
```

### Issue 4: AppDeployment Stuck in Terminating

**Symptoms:**
```bash
kubectl get appdeployment <app-name> -n <namespace>
# Shows: deletionTimestamp set, finalizers: ["platform.helmsman.dev/finalizer"]
```

**Root Cause:** Operator not running to process finalizer cleanup.

**Fix:**
```bash
# Start operator
cd /home/subhankar/projects/helmsman/helmsman-operator
go run ./cmd/main.go --leader-elect=false &

# Wait for cleanup
sleep 10

# Verify deletion
kubectl get appdeployment <app-name> -n <namespace>
```

**Emergency Force Delete (skips Keycloak cleanup):**
```bash
kubectl patch appdeployment <app-name> -n <namespace> \
  -p '{"metadata":{"finalizers":[]}}' --type=merge
```

### Issue 5: Orphaned Keycloak Client

After force-deleting an AppDeployment, the Keycloak client may remain.

**Manual Cleanup:**
```bash
# 1. Get admin token
TOKEN=$(curl -s -d "username=admin&password=helmsman123&grant_type=password&client_id=admin-cli" \
  http://localhost:8081/realms/master/protocol/openid-connect/token | jq -r '.access_token')

# 2. Find client ID
CLIENT_ID=$(curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients?clientId=<app-name>" | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['id'])")

# 3. Delete client
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients/$CLIENT_ID"

# 4. Verify
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients?clientId=<app-name>" | python3 -m json.tool
# Should return: []
```

### Issue 6: Operator CrashLoopBackOff Due to Cache Sync Timeout

**Symptoms:**
```bash
kubectl get pods -n helmsman-operator --context kind-helmsman-onprem
# NAME                                 READY   STATUS             RESTARTS   AGE
# helmsman-operator-78596998d-8496j   0/1     CrashLoopBackOff   104 (2m27s ago)   23h

kubectl logs -n helmsman-operator deployment/helmsman-operator --tail=20
# ERROR controller-runtime.source.Kind failed to get informer from cache
#   {"error": "Timeout: failed waiting for *v1.Service Informer to sync"}
# ERROR setup problem running manager
#   {"error": "failed to wait for appdeployment caches to sync kind source:
#     *v1alpha1.AppDeployment: timed out waiting for cache to be synced for Kind *v1alpha1.AppDeployment"}
```

**Root Cause:**
The operator's ClusterRoleBinding referenced a **non-existent** ClusterRole:
```yaml
# ClusterRoleBinding: helmsman-operator
roleRef:
  name: helmsman-operator-role  # ❌ DOES NOT EXIST
```

Because the ClusterRole didn't exist, the operator's service account had **zero permissions**. Every `list/watch` request to the API server was rejected with `403 Forbidden`. The informer caches could never sync, so after multiple timeouts the manager gave up and the pod crashed.

**Debugging Steps:**

1. **Check if the referenced ClusterRole exists:**
   ```bash
   kubectl get clusterrole helmsman-operator-role --context kind-helmsman-onprem
   # Error from server (NotFound): clusterroles.rbac.authorization.k8s.io "helmsman-operator-role" not found
   ```

2. **Check what ClusterRole the binding actually points to:**
   ```bash
   kubectl get clusterrolebinding helmsman-operator -o yaml --context kind-helmsman-onprem
   # roleRef:
   #   name: helmsman-operator-role  # ❌ Non-existent
   ```

3. **Verify operator SA has no permissions:**
   ```bash
   # Test from within the cluster using a test pod
   kubectl run -n helmsman-operator --rm -i --restart=Never \
     --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
     sa-test -- sh -c '
       TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
       curl -sk https://kubernetes.default.svc/api/v1/services \
         -H "Authorization: Bearer $TOKEN" | head -c 200
     '
   # Returns: {"kind":"Status","status":"Failure","message":"services is forbidden...
   ```

4. **Check operator logs for forbidden errors:**
   ```bash
   kubectl logs -n helmsman-operator deployment/helmsman-operator --tail=50
   # Shows repeated: failed to get informer from cache: Timeout...
   ```

**Fix:**

1. **Recreate the ClusterRoleBinding to point to the existing ClusterRole:**
   ```bash
   # The correct ClusterRole is: helmsman-operator-manager-role
   kubectl delete clusterrolebinding helmsman-operator --context kind-helmsman-onprem
   kubectl create clusterrolebinding helmsman-operator \
     --clusterrole=helmsman-operator-manager-role \
     --serviceaccount=helmsman-operator:helmsman-operator \
     --context kind-helmsman-onprem
   ```

2. **Add missing RBAC permissions:**
   ```bash
   # Add ConfigMap permissions
   kubectl patch clusterrole helmsman-operator-manager-role --type='json' \
     -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": [""], "resources": ["configmaps"], "verbs": ["create", "delete", "get", "list", "patch", "update", "watch"]}}]' \
     --context kind-helmsman-onprem

   # Add CRD list permissions
   kubectl patch clusterrole helmsman-operator-manager-role --type='json' \
     -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": ["apiextensions.k8s.io"], "resources": ["customresourcedefinitions"], "verbs": ["get", "list", "watch"]}}]' \
     --context kind-helmsman-onprem
   ```

3. **Restart the operator:**
   ```bash
   kubectl rollout restart deployment/helmsman-operator -n helmsman-operator \
     --context kind-helmsman-onprem
   kubectl rollout status deployment/helmsman-operator -n helmsman-operator \
     --context kind-helmsman-onprem --timeout=120s
   ```

4. **Verify the fix:**
   ```bash
   kubectl get pods -n helmsman-operator --context kind-helmsman-onprem
   # NAME                                 READY   STATUS    RESTARTS   AGE
   # helmsman-operator-5d6d8fc875-kx6b6   1/1     Running   0          28s
   ```

**Prevention:**

1. **Validate ClusterRoleBindings after deployment:**
   ```bash
   for crb in $(kubectl get clusterrolebinding -o json | \
     jq -r '.items[] | select(.subjects[].kind=="ServiceAccount") | \
     select(.subjects[].namespace=="helmsman-operator") | .metadata.name'); do
     role=$(kubectl get clusterrolebinding $crb -o jsonpath='{.roleRef.name}')
     if ! kubectl get clusterrole $role >/dev/null 2>&1; then
       echo "ERROR: ClusterRoleBinding $crb references non-existent ClusterRole $role"
     fi
   done
   ```

2. **Add RBAC validation to CI/CD:**
   - After deploying, verify the SA can list/watch all required resources
   - Fail deployment if any permission is missing

3. **Use `kubectl auth can-i` for debugging:**
   ```bash
   kubectl auth can-i list services \
     --as=system:serviceaccount:helmsman-operator:helmsman-operator
   ```

### Issue 7: ExternalSecret API Version Mismatch

**Symptoms:**
```bash
# Operator logs show:
# ERROR Reconciler error: no matches for kind "ExternalSecret" in version "external-secrets.io/v1beta1"
```

**Root Cause:**
The operator creates ExternalSecrets using `v1beta1`, but the installed ESO version has `v1beta1` disabled. Only `v1` is served.

**Fix:**
```bash
# Re-enable v1beta1 on the CRD
kubectl patch crd externalsecrets.external-secrets.io \
  --type='json' \
  -p='[{"op": "replace", "path": "/spec/versions/1/served", "value": true}]'

# Restart operator
kubectl rollout restart deployment/helmsman-operator-controller-manager \
  -n helmsman-operator-system --context kind-helmsman-onprem
```

**Better Fix:** Update operator code to use `external-secrets.io/v1` instead of `v1beta1`.

### Issue 7: NetworkPolicy Blocking Egress to Keycloak

**Symptoms:**
```bash
# oauth2-proxy (adc) container logs show:
# Get "http://keycloak.keycloak:80/realms/helmsman/.well-known/openid-configuration": dial tcp ... i/o timeout

# Pod shows 2/3 containers ready, adc is not ready
```

**Root Cause:**
The operator creates a `default-deny` NetworkPolicy that blocks all egress. It only creates an `allow-dns` policy, not an egress rule for Keycloak.

**Fix:**
```bash
# Create explicit egress policy for Keycloak
kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: <app>-allow-keycloak
  namespace: <namespace>
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: <app>
  egress:
  - to:
    - podSelector:
        matchLabels:
          app: keycloak
    ports:
    - port: 80
      protocol: TCP
  policyTypes:
  - Egress
EOF

# Restart pod
kubectl delete pod <pod-name> -n <namespace>
```

**Prevention:** Update operator code to create egress policies for required services (Keycloak, Vault).

### Issue 8: Wrong Keycloak Service Type Causing Timeouts

**Symptoms:**
```bash
# adc container cannot reach Keycloak via ClusterIP service
# Error: dial tcp 10.96.142.54:80: i/o timeout
```

**Root Cause:**
The `keycloak` NodePort service doesn't route correctly from within the cluster due to network policies. Use the headless service instead.

**Fix:**
```bash
# Update keycloak-oidc-url to use headless service
echo -n "http://keycloak-headless.keycloak:8080" | base64
# Output: aHR0cDovL2tleWNsb2FrLWhlYWRsZXNzLmtleWNsb2FrOjgwODA=

kubectl patch secret helmsman-platform-config -n <namespace> \
  -p '{"data":{"keycloak-oidc-url":"aHR0cDovL2tleWNsb2FrLWhlYWRsZXNzLmtleWNsb2FrOjgwODA="}}'

# Trigger reconciliation
kubectl annotate appdeployment <app-name> -n <namespace> \
  reconcile-trigger="$(date +%s)" --overwrite

# Restart pod
kubectl delete pod <pod-name> -n <namespace>
```

### Issue 9: ClusterSecretStore Token Secret Not Found

**Symptoms:**
```bash
# ExternalSecret status shows:
# could not get ClusterSecretStore "vault-backend": cannot get Kubernetes secret "vault-token" from namespace "<namespace>": secrets "vault-token" not found
```

**Root Cause:**
The `ClusterSecretStore` referenced a `tokenSecretRef` without specifying the namespace. ESO looked for the secret in the ExternalSecret's namespace instead of the `external-secrets` namespace.

**Fix:**
```bash
# Add namespace to tokenSecretRef
kubectl patch clustersecretstore vault-backend --type='json' \
  -p='[{"op": "add", "path": "/spec/provider/vault/auth/tokenSecretRef/namespace", "value": "external-secrets"}]'

# Restart ESO controller
kubectl rollout restart deployment/external-secrets -n external-secrets
```

### Issue 10: Missing RBAC Permissions for ConfigMaps and CRDs

**Symptoms:**
```bash
# Operator logs show:
# ERROR Failed to watch: configmaps is forbidden: User "system:serviceaccount:<ns>:<sa>" cannot list resource "configmaps" in API group "" at the cluster scope
```

**Fix:**
```bash
# Add ConfigMap permissions
kubectl patch clusterrole helmsman-operator-manager-role --type='json' \
  -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": [""], "resources": ["configmaps"], "verbs": ["create", "delete", "get", "list", "patch", "update", "watch"]}}]'

# Add CRD list permissions
kubectl patch clusterrole helmsman-operator-manager-role --type='json' \
  -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": ["apiextensions.k8s.io"], "resources": ["customresourcedefinitions"], "verbs": ["get", "list", "watch"]}}]'

# Restart operator
kubectl rollout restart deployment/helmsman-operator-controller-manager \
  -n helmsman-operator-system
```

---

### Issue 11: Keycloak Client List Unmarshalling Bug

**Symptoms:**
```bash
# Operator logs show:
# ERROR failed to decode client list: json: cannot unmarshal object into Go value of type []struct { ID string }
```

**Root Cause:**
The Keycloak Admin API `/clients?clientId=<app-name>` endpoint can return either a JSON array or a single JSON object when exactly one client matches. The original code only handled arrays.

**Fix:**
This was fixed in the operator code by adding `decodeKeycloakClientList()` which handles both response shapes. Redeploy the operator with the latest code.

**Prevention:**
- Always handle both JSON array and object responses from REST APIs
- Add unit tests for edge cases in API response shapes

### Issue 12: ExternalSecret CRD Conversion Webhook Missing

**Symptoms:**
```bash
# Operator logs show:
# ERROR conversion webhook for external-secrets.io/v1beta1 failed: 
# Post "https://<webhook-service>/convert": service not found
```

**Root Cause:**
When ESO is reinstalled or CRDs are recreated without webhook configuration, the conversion webhook service is missing.

**Fix:**
```bash
# Option 1: Remove webhook from CRD if not running ESO webhook
kubectl patch crd externalsecrets.external-secrets.io --type='json' \
  -p='[{"op": "remove", "path": "/spec/conversion"}]' \
  --context kind-helmsman-onprem

# Option 2: Install ESO properly to provide the webhook
helm install external-secrets external-secrets/external-secrets \
  -n external-secrets --create-namespace
```

**Prevention:**
- Never force-delete CRDs; use `helm uninstall` properly
- If manual CRD recreation is needed, preserve the webhook configuration

### Issue 13: ESO Helm Install Fails Due to Orphaned CRDs

**Symptoms:**
```bash
# helm install external-secrets ... fails with:
# CustomResourceDefinition "externalsecrets.external-secrets.io" exists and cannot be imported:
# invalid ownership metadata; missing key "meta.helm.sh/release-name"
```

**Root Cause:**
Old ESO CRDs were created outside of Helm or force-deleted without removing annotations. Helm requires CRDs to have specific annotations to claim ownership.

**Fix:**
```bash
# Option 1: Delete orphaned CRDs and retry
kubectl delete crd externalsecrets.external-secrets.io --grace-period=0 --force
helm install external-secrets external-secrets/external-secrets -n external-secrets

# Option 2: Use --skip-crds if CRDs already exist
helm install external-secrets external-secrets/external-secrets \
  -n external-secrets --skip-crds
```

**Prevention:**
- Always use `helm uninstall` to cleanly remove releases
- If force-deleting, also remove CRDs

---

## Day-to-Day Operations

### Starting the Environment

```bash
# 1. Ensure Docker Desktop is running
docker info

# 2. Start Kind cluster (if not exists)
kind get clusters | grep helmsman-onprem || \
  kind create cluster --name helmsman-onprem --config kind-config.yaml

# 3. Start operator (for development)
cd /home/subhankar/projects/helmsman/helmsman-operator
go run ./cmd/main.go --leader-elect=false &

# 4. Verify everything is running
kubectl get pods --all-namespaces --context kind-helmsman-onprem
```

### Stopping the Environment

```bash
# Stop operator (if running locally)
pkill -f helmsman-operator || true

# Stop Kind cluster (preserves Helm releases)
kind stop cluster --name helmsman-onprem

# Or delete cluster (fresh start next time)
kind delete cluster --name helmsman-onprem
```

### Deploying a New Application

```bash
# 1. Create namespace
kubectl create namespace my-app --context kind-helmsman-onprem

# 2. Create platform config secret
kubectl create secret generic helmsman-platform-config \
  -n my-app \
  --from-literal=keycloak-url=http://localhost:8081 \
  --from-literal=keycloak-oidc-url=http://host.docker.internal:8081 \
  --from-literal=keycloak-realm=helmsman \
  --from-literal=keycloak-admin-user=admin \
  --from-literal=keycloak-admin-password=helmsman123 \
  --from-literal=vault-url=http://172.18.0.2:30082 \
  --from-literal=vault-token=root \
  --context kind-helmsman-onprem

# 3. Create AppDeployment CR
kubectl apply -f my-app-deployment.yaml -n my-app --context kind-helmsman-onprem

# 4. Watch reconciliation
kubectl get pods -n my-app -w --context kind-helmsman-onprem
```

### Viewing Application Logs

```bash
# All containers
kubectl logs -l app.kubernetes.io/name=<app-name> -n <namespace> --context kind-helmsman-onprem --tail=100

# Specific container
kubectl logs <pod> -n <namespace> -c <container-name> --context kind-helmsman-onprem --tail=100
```

### Accessing Application

```bash
# Port forward
kubectl port-forward -n <namespace> <pod> 8080:8080 --context kind-helmsman-onprem

# Or use service
kubectl port-forward -n <namespace> svc/<service-name> 8080:8080 --context kind-helmsman-onprem
```

### Updating Platform Config

```bash
# Check current values
kubectl get secret helmsman-platform-config -n <namespace> \
  --context kind-helmsman-onprem \
  -o jsonpath='{.data}' | python3 -c "
import sys, json, base64
d=json.load(sys.stdin)
for k, v in d.items():
  print(f'{k}: {base64.b64decode(v).decode()}')
"

# Update a value
kubectl patch secret helmsman-platform-config -n <namespace> \
  --context kind-helmsman-onprem \
  -p '{"data":{"keycloak-url":"<base64-encoded-value>"}}'

# Trigger reconciliation to propagate changes
kubectl annotate appdeployment <app-name> \
  -n <namespace> --context kind-helmsman-onprem \
  reconcile-trigger="$(date +%s)" --overwrite
```

### Restarting Application Pod

```bash
kubectl delete pod <pod-name> -n <namespace> --context kind-helmsman-onprem
```

---

## Quick Reference

### Context
```bash
export CONTEXT="kind-helmsman-onprem"
```

### Common kubectl Commands

```bash
# Get all resources for an app
kubectl get all,networkpolicies,serviceaccount,externalsecret \
  -n <namespace> --context $CONTEXT \
  -l app.kubernetes.io/name=<app-name>

# Describe pod for debugging
kubectl describe pod <pod> -n <namespace> --context $CONTEXT

# Get events
kubectl get events -n <namespace> --context $CONTEXT --sort-by='.lastTimestamp'

# Check ConfigMap exists
kubectl get configmap <app>-fluent-bit-config -n <namespace> --context $CONTEXT
```

### Health Check Script

```bash
#!/bin/bash
# save as: health-check.sh

CONTEXT="kind-helmsman-onprem"
NAMESPACE="sample-app"
APP="operator-sample-app"

echo "=== Pod Status ==="
kubectl get pod ${APP}-0 -n $NAMESPACE --context $CONTEXT

echo -e "\n=== Container Status ==="
kubectl get pod ${APP}-0 -n $NAMESPACE --context $CONTEXT \
  -o jsonpath='{range .status.containerStatuses[*]}{.name}: ready={.ready} restartCount={.restartCount}{"\n"}{end}'

echo -e "\n=== ExternalSecret Status ==="
kubectl get externalsecret ${APP}-oidc -n $NAMESPACE --context $CONTEXT \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'

echo -e "\n=== Logs (last 5 lines) ==="
for c in app adc fluent-bit; do
  echo "--- $c ---"
  kubectl logs ${APP}-0 -n $NAMESPACE --context $CONTEXT -c $c --tail=5 2>/dev/null || echo "(no logs yet)"
done
```

### Base64 Encoding Helpers

```bash
# Encode values for platform config secret
echo -n "http://localhost:8081" | base64
echo -n "http://host.docker.internal:8081" | base64
echo -n "helmsman" | base64
echo -n "admin" | base64
echo -n "helmsman123" | base64
echo -n "http://172.18.0.2:30082" | base64
echo -n "root" | base64
```

### Important File Locations

| Path | Purpose |
|------|---------|
| `/home/subhankar/projects/helmsman/helmsman-operator` | Operator source code |
| `internal/controller/appdeployment_controller.go` | Main reconciliation logic |
| `internal/controller/resources.go` | Resource builders |
| `internal/controller/keycloak.go` | Keycloak/Vault integration |
| `config/samples/platform_v1alpha1_appdeployment.yaml` | Example AppDeployment |
| `helm/helmsman-operator/` | Helm chart for operator |

---

## Escalation

If you're stuck after following this guide:

1. Check operator logs: `kubectl logs -n helmsman-operator-system -l app.kubernetes.io/name=helmsman-operator --context kind-helmsman-onprem`
2. Check ESO logs: `kubectl logs -n external-secrets -l app.kubernetes.io/name=external-secrets --context kind-helmsman-onprem`
3. Check Vault logs: `kubectl logs -n vault -l app.kubernetes.io/name=vault --context kind-helmsman-onprem`
4. Check Keycloak logs: `kubectl logs -n keycloak -l app.kubernetes.io/name=keycloak --context kind-helmsman-onprem`

For code-level issues, refer to `INCIDENT_RUNBOOK.md`.
