# Incident Runbook: Operator Sample App Deployment Issues

## Incident Summary
**Date:** 2026-08-21
**Environment:** kind-helmsman-onprem cluster
**Application:** operator-sample-app (AppDeployment)
**Severity:** High - Application pods stuck in ContainerCreating/CrashLoopBackOff

---

## Root Cause Analysis

### Issue 1: ExternalSecret Not Syncing (SecretSyncedError)
**Symptoms:**
- ExternalSecret `operator-sample-app-oidc` showed `Ready: False`
- Error: `could not get secret data from provider: error retrieving secret at .data[0], key: apps/operator-sample-app/oidc, err: Secret does not exist`

**Root Cause:**
The ClusterSecretStore `vault-backend` was configured with `path: secret` (the KV v2 mount path), but the ExternalSecret was using key `apps/operator-sample-app/oidc`. The external-secrets controller combines the ClusterSecretStore path with the ExternalSecret key to form the full Vault path: `secret/apps/operator-sample-app/oidc`.

The secret DID exist in Vault at `secret/apps/operator-sample-app/oidc` but the initial sync was failing due to transient connectivity or timing issues. The secret was properly stored with all required properties:
- client-id
- client-secret
- issuer-url
- cookie-secret

**Resolution:** The ExternalSecret eventually synced successfully after the operator reconciled. No code change was needed for this issue - it was a transient state.

---

### Issue 2: Missing Fluent Bit ConfigMap
**Symptoms:**
- Pod stuck in `ContainerCreating`
- Events showed: `MountVolume.SetUp failed for volume "fluent-bit-config" : configmap "operator-sample-app-fluent-bit-config" not found`

**Root Cause:**
The controller code in `internal/controller/resources.go` referenced a ConfigMap volume named `fluent-bit-config` (line 235-243) but **never created the ConfigMap**. The reconciliation loop in `appdeployment_controller.go` had no step to ensure the ConfigMap exists.

**Code Location:**
- `internal/controller/resources.go:235-243` - Volume definition referencing ConfigMap
- `internal/controller/resources.go:472-474` - Fluent Bit container mounting the ConfigMap
- `internal/controller/appdeployment_controller.go` - Missing reconciliation step

**Resolution:** Added two functions in `internal/controller/resources.go`:
1. `buildFluentBitConfigMap()` - Creates the ConfigMap with fluent-bit.conf and parsers.conf
2. `ensureFluentBitConfigMap()` - Ensures the ConfigMap exists during reconciliation

Added reconciliation step in `appdeployment_controller.go` (Step 8) to call `ensureFluentBitConfigMap()`.

---

### Issue 3: OIDC Issuer URL Unreachable from Cluster
**Symptoms:**
- oauth2-proxy (adc) container crashing with: `Get "http://localhost:8081/realms/helmsman/.well-known/openid-configuration": dial tcp [::1]:8081: connect: connection refused`
- Then after first fix: `Get "http://172.29.23.92:8081/...": i/o timeout`

**Root Cause:**
The Vault secret stored `issuer-url: http://localhost:8081/realms/helmsman`. This works on the host machine where Keycloak runs, but **inside the Kind cluster containers**, `localhost` resolves to the container's own loopback interface, not the host.

Even using the host's LAN IP (172.29.23.92) failed because Keycloak was bound only to `127.0.0.1` (not 0.0.0.0).

**Resolution:** Updated the Vault secret to use `http://host.docker.internal:8081/realms/helmsman` which Docker Desktop resolves to the host machine from within containers.

---

## Detailed Fixes Applied

### Quick Fixes (Manual Commands Run During Incident)

#### 1. Fixed Missing Fluent Bit ConfigMap (Code Fix)
**Files Modified:**
- `internal/controller/resources.go` - Added `buildFluentBitConfigMap()` and `ensureFluentBitConfigMap()`
- `internal/controller/appdeployment_controller.go` - Added reconciliation step (Step 8)

**Commands to Deploy:**
```bash
cd /home/subhankar/projects/helmsman/helmsman-operator
# Build and push new operator image
make docker-build docker-push IMG=ghcr.io/subhankar720/helmsman-operator:latest

# Update deployment to use new image
kubectl set image deployment/helmsman-operator-controller-manager \
  -n helmsman-operator-system \
  manager=ghcr.io/subhankar720/helmsman-operator:latest \
  --context kind-helmsman-onprem
```

**Or Run Locally for Testing:**
```bash
cd /home/subhankar/projects/helmsman/helmsman-operator
go run ./cmd/main.go --leader-elect=false &
# Trigger reconciliation
kubectl annotate appdeployment operator-sample-app \
  -n sample-app --context kind-helmsman-onprem \
  reconcile-trigger="$(date +%s)" --overwrite
```

#### 2. Fixed OIDC Issuer URL in Vault (Manual Quick Fix)
**Command Run:**
```bash
curl -sH "X-Vault-Token: root" -X POST \
  -d '{"data":{
    "client-id":"operator-sample-app",
    "client-secret":"VfC9ezp2JfbVbJIowxXYmovlTTEe1cfd",
    "cookie-secret":"9YJRwQnK5DN4N4-uk4CFi0kG-bP9Ryph0uwf0M1TZQs=",
    "issuer-url":"http://host.docker.internal:8081/realms/helmsman"
  }}' \
  http://localhost:8082/v1/secret/data/apps/operator-sample-app/oidc | python3 -m json.tool
```

**Force ExternalSecret Resync:**
```bash
kubectl annotate externalsecret operator-sample-app-oidc \
  -n sample-app --context kind-helmsman-onprem \
  force-sync="$(date +%s)" --overwrite
```

**Restart Pod to Pick Up New Secret:**
```bash
kubectl delete pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem
```

#### 3. Permanent Fix: Updated Platform Config Secret
**This ensures the fix survives reconciliation cycles and Docker Desktop restarts.**

**Command Run:**
```bash
# Update the platform config secret with host.docker.internal
# keycloak-url = http://host.docker.internal:8081 (base64 encoded)
kubectl patch secret helmsman-platform-config -n sample-app \
  --context kind-helmsman-onprem \
  -p '{"data":{"keycloak-url":"aHR0cDovL2hvc3QuZG9ja2VyLmludGVybmFsOjgwODE="}}'
```

**Verification:**
```bash
# Verify the secret was updated
kubectl get secret helmsman-platform-config -n sample-app \
  --context kind-helmsman-onprem \
  -o jsonpath='{.data.keycloak-url}' | base64 -d
# Output: http://host.docker.internal:8081

# Trigger reconciliation to propagate to Vault
kubectl annotate appdeployment operator-sample-app \
  -n sample-app --context kind-helmsman-onprem \
  reconcile-trigger="$(date +%s)" --overwrite

# Verify Vault was updated
sleep 5
curl -sH "X-Vault-Token: root" \
  http://localhost:8082/v1/secret/data/apps/operator-sample-app/oidc | \
  python3 -c "import sys, json; d=json.load(sys.stdin); print(d['data']['data']['issuer-url'])"
# Output: http://host.docker.internal:8081/realms/helmsman
```

---

### Where Did the Secret Values Come From?

#### client-id: `operator-sample-app`
- **Source:** Derived from `appDep.Spec.AppName` in the AppDeployment CR
- **Generated by:** `registerKeycloakClient()` in `internal/controller/keycloak.go:146`
- **Keycloak Client ID:** Same as the AppDeployment name

#### client-secret: `VfC9ezp2JfbVbJIowxXYmovlTTEe1cfd`
- **Source:** Generated by Keycloak when creating the OIDC client
- **Retrieved by:** `registerKeycloakClient()` via Keycloak Admin API at:
  `GET {keycloak-url}/admin/realms/{realm}/clients/{client-id}/client-secret`
- **Code Location:** `internal/controller/keycloak.go:129-141`
- **Note:** This is a static secret generated once per client creation

#### cookie-secret: `9YJRwQnK5DN4N4-uk4CFi0kG-bP9Ryph0uwf0M1TZQs=`
- **Source:** Generated by the operator (32 random bytes, base64url encoded)
- **Generated by:** `writeOIDCCredsToVault()` in `internal/controller/keycloak.go:176-184`
- **Logic:** 
  1. First checks if existing secret exists in Vault (to preserve sessions)
  2. If not, generates new: `rand.Read(32 bytes)` → `base64.URLEncoding.EncodeToString()`
- **Purpose:** Used by oauth2-proxy for session cookie encryption

#### issuer-url: `http://host.docker.internal:8081/realms/helmsman`
- **Source:** Constructed from platform config secret
- **Format:** `{keycloak-url}/realms/{keycloak-realm}`
- **Code Location:** `internal/controller/keycloak.go:149`
  ```go
  IssuerURL: fmt.Sprintf("%s/realms/%s", cfg.KeycloakURL, cfg.KeycloakRealm)
  ```
- **Platform Config Secret:** `helmsman-platform-config` in app namespace
  - `keycloak-url`: `http://host.docker.internal:8081` (updated)
  - `keycloak-realm`: `helmsman`

---

## Complete Permanent Fix Summary

### What Was Fixed Permanently:

| Component | Fix Type | Details |
|-----------|----------|---------|
| Fluent Bit ConfigMap | **Code** | Added `ensureFluentBitConfigMap()` reconciliation step |
| OIDC Issuer URL | **Config** | Updated `helmsman-platform-config` secret to use `host.docker.internal` |
| Vault Secret Sync | **Auto** | Operator now writes correct URL from platform config on every reconcile |

### What Happens on Next Reconcile / Docker Desktop Restart:

1. **Operator starts** → Reads `helmsman-platform-config` secret
2. **Gets Keycloak URL** → `http://host.docker.internal:8081` (from secret)
3. **Constructs Issuer URL** → `http://host.docker.internal:8081/realms/helmsman`
4. **Writes to Vault** → Via `writeOIDCCredsToVault()`
5. **ExternalSecret syncs** → Creates/updates K8s secret `operator-sample-app-oidc`
6. **Pod restarts** → Picks up new `ISSUER_URL` env var
7. **oauth2-proxy works** → Can reach Keycloak via `host.docker.internal`

---

## Testing & Verification Procedures

### Quick Health Check (Run After Any Fix)
```bash
#!/bin/bash
# save as: verify-deployment.sh

CONTEXT="kind-helmsman-onprem"
NAMESPACE="sample-app"
APP="operator-sample-app"

echo "=== 1. Pod Status ==="
kubectl get pod ${APP}-0 -n $NAMESPACE --context $CONTEXT

echo -e "\n=== 2. All Containers Ready ==="
kubectl get pod ${APP}-0 -n $NAMESPACE --context $CONTEXT \
  -o jsonpath='{range .status.containerStatuses[*]}{.name}: ready={.ready} restartCount={.restartCount}{"\n"}{end}'

echo -e "\n=== 3. ExternalSecret Status ==="
kubectl get externalsecret ${APP}-oidc -n $NAMESPACE --context $CONTEXT \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'

echo -e "\n=== 4. ConfigMap Exists ==="
kubectl get configmap ${APP}-fluent-bit-config -n $NAMESPACE --context $CONTEXT

echo -e "\n=== 5. Secret Values ==="
echo "Issuer URL:"
kubectl get secret ${APP}-oidc -n $NAMESPACE --context $CONTEXT \
  -o jsonpath='{.data.issuer-url}' | base64 -d
echo -e "\nClient ID:"
kubectl get secret ${APP}-oidc -n $NAMESPACE --context $CONTEXT \
  -o jsonpath='{.data.client-id}' | base64 -d

echo -e "\n=== 6. Container Logs (last 5 lines each) ==="
for c in app adc fluent-bit; do
  echo "--- $c ---"
  kubectl logs ${APP}-0 -n $NAMESPACE --context $CONTEXT -c $c --tail=5
done

echo -e "\n=== 7. oauth2-proxy Health (ping endpoint) ==="
kubectl exec ${APP}-0 -n $NAMESPACE --context $CONTEXT -c adc -- \
  wget -q -O - http://localhost:4180/ping

echo -e "\n=== 8. App Health ==="
kubectl exec ${APP}-0 -n $NAMESPACE --context $CONTEXT -c app -- \
  wget -q -O - http://localhost:8080/health
```

**Expected Output:**
```
=== 1. Pod Status ===
NAME                    READY   STATUS    RESTARTS   AGE
operator-sample-app-0   3/3     Running   0          2m

=== 2. All Containers Ready ===
app: ready=true restartCount=0
adc: ready=true restartCount=0
fluent-bit: ready=true restartCount=0

=== 3. ExternalSecret Status ===
Secret was synced

=== 4. ConfigMap Exists ===
NAME                                    DATA   AGE
operator-sample-app-fluent-bit-config   2      1h

=== 5. Secret Values ===
Issuer URL:
http://host.docker.internal:8081/realms/helmsman
Client ID:
operator-sample-app

=== 6. Container Logs (last 5 lines each) ===
--- app ---
2026/08/22 05:30:00 helmsman-sample-app starting on :8080
--- adc ---
[2026/08/22 05:30:01] [proxy.go:89] mapping path "/" => upstream "http://127.0.0.1:8080"
[2026/08/22 05:30:01] [oauthproxy.go:172] OAuthProxy configured for OpenID Connect Client ID: operator-sample-app
10.244.1.1:42844 - ccd979f8-61c2-44c1-bad6-fd7585bc249b - - [2026/08/22 05:30:10] 10.244.1.24:4180 GET - "/ping" HTTP/1.1 "kube-probe/1.34" 200 2 0.000
--- fluent-bit ---
[2026/08/22 05:30:00] [ info] [fluent bit] version=3.2.10
[2026/08/22 05:30:00] [ info] [http_server] listen iface=0.0.0.0 tcp_port=2020

=== 7. oauth2-proxy Health (ping endpoint) ===
OK

=== 8. App Health ===
{"status":"healthy","time":"2026-08-22T05:30:15Z"}
```

---

### Integration Test: Full OIDC Flow
```bash
# Test that oauth2-proxy redirects to Keycloak
kubectl run -n sample-app --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  oidc-test -- curl -s -I http://operator-sample-app.sample-app.svc.cluster.local/ | head -20

# Expected: 302 redirect to Keycloak login
# Location: http://host.docker.internal:8081/realms/helmsman/protocol/openid-connect/auth?...
```

---

### Test Keycloak Connectivity from Cluster
```bash
# From inside cluster pod (simulates oauth2-proxy)
kubectl run -n sample-app --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  keycloak-reachability -- \
  curl -s http://host.docker.internal:8081/realms/helmsman/.well-known/openid-configuration | \
  python3 -c "import sys, json; print(json.load(sys.stdin)['issuer'])"

# Expected output: http://host.docker.internal:8081/realms/helmsman
```

---

### Test Vault Connectivity from Cluster
```bash
# From external-secrets namespace (simulates ESO controller)
kubectl run -n external-secrets --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  vault-reachability -- \
  curl -sH "X-Vault-Token: root" \
  http://172.18.0.2:30082/v1/secret/data/apps/operator-sample-app/oidc | \
  python3 -c "import sys, json; d=json.load(sys.stdin); print('client-id:', d['data']['data']['client-id']); print('issuer-url:', d['data']['data']['issuer-url'])"
```

---

## Runbook: Fixing Deployment Issues

### Prerequisites
```bash
# Required tools
- kubectl (configured for kind-helmsman-onprem context)
- curl
- python3 (for JSON formatting)
- Vault CLI or curl with root token
- Access to Docker host
```

---

### Scenario 1: Pod Stuck in ContainerCreating

**Diagnosis:**
```bash
kubectl describe pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem
# Look for Events: "MountVolume.SetUp failed for volume ... configmap ... not found"
```

**Fix:**
```bash
# 1. Ensure operator is running with latest code
cd /home/subhankar/projects/helmsman/helmsman-operator
go run ./cmd/main.go --leader-elect=false &

# 2. Trigger reconciliation
kubectl annotate appdeployment operator-sample-app \
  -n sample-app --context kind-helmsman-onprem \
  reconcile-trigger="$(date +%s)" --overwrite

# 3. Verify ConfigMap created
kubectl get configmap operator-sample-app-fluent-bit-config -n sample-app --context kind-helmsman-onprem

# 4. Verify pod progresses
kubectl get pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -w
```

---

### Scenario 2: ExternalSecret Not Syncing (SecretSyncedError)

**Diagnosis:**
```bash
kubectl get externalsecret operator-sample-app-oidc -n sample-app --context kind-helmsman-onprem -o yaml
# Check status.conditions[].message for "Secret does not exist"

kubectl describe externalsecret operator-sample-app-oidc -n sample-app --context kind-helmsman-onprem
```

**Fix:**
```bash
# 1. Verify secret exists in Vault
curl -sH "X-Vault-Token: root" \
  http://localhost:8082/v1/secret/data/apps/operator-sample-app/oidc | python3 -m json.tool

# 2. Verify ClusterSecretStore is healthy
kubectl get clustersecretstore vault-backend --context kind-helmsman-onprem -o yaml
# Check status.conditions[].status == "True"

# 3. Test cluster-to-Vault connectivity
kubectl run -n external-secrets --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  curl-test -- curl -sH "X-Vault-Token: root" \
  http://172.18.0.2:30082/v1/secret/data/apps/operator-sample-app/oidc

# 4. Force ExternalSecret resync
kubectl annotate externalsecret operator-sample-app-oidc \
  -n sample-app --context kind-helmsman-onprem \
  force-sync="$(date +%s)" --overwrite

# 5. Wait and verify
sleep 10
kubectl get externalsecret operator-sample-app-oidc -n sample-app --context kind-helmsman-onprem -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
```

---

### Scenario 3: oauth2-proxy (adc) Container Crashing - OIDC Discovery Failed

**Diagnosis:**
```bash
kubectl logs operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -c adc --tail=20
# Look for: "dial tcp ... connect: connection refused" or "i/o timeout"

kubectl get secret operator-sample-app-oidc -n sample-app --context kind-helmsman-onprem -o jsonpath='{.data.issuer-url}' | base64 -d
```

**Fix (Quick - Manual Vault Update):**
```bash
# 1. Test Keycloak accessibility from cluster
kubectl run -n sample-app --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  curl-test -- curl -s http://host.docker.internal:8081/realms/helmsman/.well-known/openid-configuration

# 2. If host.docker.internal works, update Vault secret directly
curl -sH "X-Vault-Token: root" -X POST \
  -d '{"data":{"client-id":"operator-sample-app","client-secret":"VfC9ezp2JfbVbJIowxXYmovlTTEe1cfd","cookie-secret":"9YJRwQnK5DN4N4-uk4CFi0kG-bP9Ryph0uwf0M1TZQs=","issuer-url":"http://host.docker.internal:8081/realms/helmsman"}}' \
  http://localhost:8082/v1/secret/data/apps/operator-sample-app/oidc | python3 -m json.tool

# 3. Force ExternalSecret resync
kubectl annotate externalsecret operator-sample-app-oidc \
  -n sample-app --context kind-helmsman-onprem \
  force-sync="$(date +%s)" --overwrite

# 4. Wait for secret update
sleep 10
kubectl get secret operator-sample-app-oidc -n sample-app --context kind-helmsman-onprem -o jsonpath='{.data.issuer-url}' | base64 -d

# 5. Restart pod to pick up new secret
kubectl delete pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem

# 6. Verify all containers ready
kubectl get pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -w
```

**Fix (Permanent - Update Platform Config):**
```bash
# Update the platform config secret (survives reconciles)
kubectl patch secret helmsman-platform-config -n sample-app \
  --context kind-helmsman-onprem \
  -p '{"data":{"keycloak-url":"aHR0cDovL2hvc3QuZG9ja2VyLmludGVybmFsOjgwODE="}}'

# Verify
kubectl get secret helmsman-platform-config -n sample-app --context kind-helmsman-onprem -o jsonpath='{.data.keycloak-url}' | base64 -d

# Trigger reconciliation to propagate to Vault
kubectl annotate appdeployment operator-sample-app \
  -n sample-app --context kind-helmsman-onprem \
  reconcile-trigger="$(date +%s)" --overwrite

# Wait and verify Vault updated
sleep 5
curl -sH "X-Vault-Token: root" http://localhost:8082/v1/secret/data/apps/operator-sample-app/oidc | python3 -c "import sys, json; d=json.load(sys.stdin); print(d['data']['data']['issuer-url'])"

# Restart pod
kubectl delete pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem
```

---

### Scenario 4: Post Docker Desktop Restart - Full Recovery

**Run this sequence after Docker Desktop restart:**

```bash
#!/bin/bash
# save as: recover-after-docker-restart.sh

set -e

CONTEXT="kind-helmsman-onprem"
NAMESPACE="sample-app"
APP_NAME="operator-sample-app"

echo "=== Step 1: Verify cluster is up ==="
kubectl get nodes --context $CONTEXT

echo "=== Step 2: Start operator ==="
cd /home/subhankar/projects/helmsman/helmsman-operator
go run ./cmd/main.go --leader-elect=false &
OPERATOR_PID=$!
sleep 3

echo "=== Step 3: Check Keycloak accessibility ==="
# Test from cluster perspective
kubectl run -n $NAMESPACE --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context $CONTEXT \
  keycloak-test -- curl -s http://host.docker.internal:8081/realms/helmsman/.well-known/openid-configuration > /dev/null

if [ $? -eq 0 ]; then
    ISSUER_URL="http://host.docker.internal:8081/realms/helmsman"
else
    # Fallback: try to detect correct URL
    echo "host.docker.internal failed, trying alternative..."
    # Add logic here if needed
    ISSUER_URL="http://host.docker.internal:8081/realms/helmsman"
fi

echo "=== Step 4: Update platform config secret (permanent fix) ==="
# keycloak-url = http://host.docker.internal:8081 (base64 encoded)
kubectl patch secret helmsman-platform-config -n $NAMESPACE \
  --context $CONTEXT \
  -p '{"data":{"keycloak-url":"aHR0cDovL2hvc3QuZG9ja2VyLmludGVybmFsOjgwODE="}}'

echo "=== Step 5: Trigger full reconciliation ==="
kubectl annotate appdeployment $APP_NAME \
  -n $NAMESPACE --context $CONTEXT \
  reconcile-trigger="$(date +%s)" --overwrite

echo "=== Step 6: Force ExternalSecret sync ==="
kubectl annotate externalsecret ${APP_NAME}-oidc \
  -n $NAMESPACE --context $CONTEXT \
  force-sync="$(date +%s)" --overwrite

echo "=== Step 7: Wait for secret propagation ==="
sleep 15

echo "=== Step 8: Restart application pod ==="
kubectl delete pod ${APP_NAME}-0 -n $NAMESPACE --context $CONTEXT

echo "=== Step 9: Wait for pod ready ==="
kubectl wait --for=condition=Ready pod/${APP_NAME}-0 -n $NAMESPACE --context $CONTEXT --timeout=180s

echo "=== Step 10: Verify all containers running ==="
kubectl get pod ${APP_NAME}-0 -n $NAMESPACE --context $CONTEXT

# Cleanup
kill $OPERATOR_PID 2>/dev/null || true

echo "=== Recovery complete ==="
```

---

### Verification Checklist

After any fix, verify:
- [ ] `kubectl get pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem` shows `3/3 Running`
- [ ] `kubectl get externalsecret operator-sample-app-oidc -n sample-app --context kind-helmsman-onprem` shows `SecretSynced: True`
- [ ] `kubectl get configmap operator-sample-app-fluent-bit-config -n sample-app --context kind-helmsman-onprem` exists
- [ ] `kubectl logs operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -c adc` shows "OAuthProxy configured" and "/ping" returning 200
- [ ] `kubectl logs operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -c app` shows "helmsman-sample-app starting on :8080"
- [ ] `kubectl logs operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -c fluent-bit` shows Fluent Bit started without TLS errors

---

## Debugging Commands Reference

Complete list of all commands used during incident investigation for future reference.

### Initial State Assessment
```bash
# Get all resources for the app
kubectl get all,networkpolicies,serviceaccount,externalsecret \
  -n sample-app --context kind-helmsman-onprem \
  -l app.kubernetes.io/name=operator-sample-app

# Describe ExternalSecret to see sync error
kubectl describe externalsecret operator-sample-app-oidc \
  -n sample-app --context kind-helmsman-onprem

# Check pod events for ContainerCreating issues
kubectl describe pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem
```

### Vault Secret Investigation
```bash
# Check if secret exists at expected path (KV v2)
curl -sH "X-Vault-Token: root" \
  http://localhost:8082/v1/secret/data/apps/operator-sample-app/oidc | python3 -m json.tool

# Check wrong path (common mistake - missing 'secret/' prefix)
curl -sH "X-Vault-Token: root" \
  http://localhost:8082/v1/apps/data/operator-sample-app/oidc | python3 -m json.tool

# List all mounted secret engines
curl -sH "X-Vault-Token: root" \
  http://localhost:8082/v1/sys/mounts | python3 -m json.tool

# List all keys under secret/ mount
curl -sH "X-Vault-Token: root" \
  "http://localhost:8082/v1/secret/metadata?list=true" | python3 -m json.tool

# Check specific secret metadata (versions, etc.)
curl -sH "X-Vault-Token: root" \
  "http://localhost:8082/v1/secret/metadata/apps/operator-sample-app/oidc" | python3 -m json.tool
```

### ClusterSecretStore & External-Secrets Controller
```bash
# Check ClusterSecretStore configuration and status
kubectl get clustersecretstore vault-backend --context kind-helmsman-onprem -o yaml

# Check Vault token secret used by ClusterSecretStore
kubectl get secret vault-token -n external-secrets --context kind-helmsman-onprem -o yaml

# Check external-secrets controller logs
kubectl get pods -n external-secrets --context kind-helmsman-onprem
kubectl logs -n external-secrets platform-eso-external-secrets-5fcbc445f9-k5fnm \
  --context kind-helmsman-onprem --tail=100

# Test cluster-to-Vault connectivity (from external-secrets namespace)
kubectl run -n external-secrets --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  curl-test -- curl -sH "X-Vault-Token: root" \
  http://172.18.0.2:30082/v1/secret/data/apps/operator-sample-app/oidc
```

### Test ExternalSecret Key Path Variations
```bash
# Test with full path (secret/ prefix)
cat <<'EOF' | kubectl apply --context kind-helmsman-onprem -f -
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: test-vault-secret
  namespace: sample-app
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: ClusterSecretStore
  target:
    name: test-vault-secret
    creationPolicy: Owner
  data:
  - secretKey: client-id
    remoteRef:
      key: secret/apps/operator-sample-app/oidc
      property: client-id
EOF

# Test without secret/ prefix (relies on ClusterSecretStore path)
cat <<'EOF' | kubectl apply --context kind-helmsman-onprem -f -
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: test-vault-secret2
  namespace: sample-app
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: ClusterSecretStore
  target:
    name: test-vault-secret2
    creationPolicy: Owner
  data:
  - secretKey: client-id
    remoteRef:
      key: apps/operator-sample-app/oidc
      property: client-id
EOF

# Check test ExternalSecret status
kubectl get externalsecret test-vault-secret -n sample-app --context kind-helmsman-onprem -o yaml
kubectl get externalsecret test-vault-secret2 -n sample-app --context kind-helmsman-onprem -o yaml
```

### Pod & Container Debugging
```bash
# Check pod status and events
kubectl describe pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem

# Check app container health from inside pod
kubectl exec operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -c app -- \
  wget -q -O - http://localhost:8080/health

# Check oauth2-proxy (adc) logs
kubectl logs operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -c adc --tail=50

# Check fluent-bit logs
kubectl logs operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -c fluent-bit --tail=50

# Test app health via pod IP
kubectl get pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -o jsonpath='{.status.podIP}'
kubectl run -n sample-app --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  curl-test -- curl -s http://<POD_IP>:8080/health
```

### Network Policy Debugging
```bash
# List all network policies in namespace
kubectl get networkpolicy -n sample-app --context kind-helmsman-onprem -o yaml
```

### Keycloak Connectivity Testing
```bash
# Test Keycloak from host
curl -s http://localhost:8081/realms/helmsman/.well-known/openid-configuration | python3 -m json.tool

# Test Keycloak from cluster (via host.docker.internal)
kubectl run -n sample-app --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  curl-test -- curl -s http://host.docker.internal:8081/realms/helmsman/.well-known/openid-configuration

# Test Keycloak from cluster (via docker bridge IP)
kubectl run -n sample-app --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  curl-test -- curl -s http://172.17.0.1:8081/realms/helmsman/.well-known/openid-configuration

# Test Keycloak from cluster (via kind network gateway)
docker network inspect kind | grep -A5 Gateway
kubectl run -n sample-app --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 --context kind-helmsman-onprem \
  curl-test -- curl -s http://172.18.0.1:8081/realms/helmsman/.well-known/openid-configuration
```

### Platform Config Secret
```bash
# Check platform config secret
kubectl get secret helmsman-platform-config -n sample-app --context kind-helmsman-onprem -o yaml

# Decode keycloak-url
kubectl get secret helmsman-platform-config -n sample-app --context kind-helmsman-onprem \
  -o jsonpath='{.data.keycloak-url}' | base64 -d

# Update keycloak-url to host.docker.internal
kubectl patch secret helmsman-platform-config -n sample-app \
  --context kind-helmsman-onprem \
  -p '{"data":{"keycloak-url":"aHR0cDovL2hvc3QuZG9ja2VyLmludGVybmFsOjgwODE="}}'
```

### Trigger Reconciliation & Sync
```bash
# Trigger AppDeployment reconciliation
kubectl annotate appdeployment operator-sample-app \
  -n sample-app --context kind-helmsman-onprem \
  reconcile-trigger="$(date +%s)" --overwrite

# Force ExternalSecret resync
kubectl annotate externalsecret operator-sample-app-oidc \
  -n sample-app --context kind-helmsman-onprem \
  force-sync="$(date +%s)" --overwrite

# Restart pod to pick up new secrets
kubectl delete pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem
```

### Verify Fixes
```bash
# Check Vault secret after reconciliation
curl -sH "X-Vault-Token: root" \
  http://localhost:8082/v1/secret/data/apps/operator-sample-app/oidc | \
  python3 -c "import sys, json; d=json.load(sys.stdin); print(d['data']['data']['issuer-url'])"

# Check K8s secret after ExternalSecret sync
kubectl get secret operator-sample-app-oidc -n sample-app --context kind-helmsman-onprem \
  -o jsonpath='{.data.issuer-url}' | base64 -d

# Check pod status
kubectl get pod operator-sample-app-0 -n sample-app --context kind-helmsman-onprem -w

# Check all resources
kubectl get all,networkpolicies,serviceaccount,externalsecret,configmap \
  -n sample-app --context kind-helmsman-onprem \
  -l app.kubernetes.io/name=operator-sample-app
```

### Operator Development
```bash
# Build operator locally
cd /home/subhankar/projects/helmsman/helmsman-operator
go build -o /tmp/helmsman-operator ./cmd/main.go

# Run operator locally
go run ./cmd/main.go --leader-elect=false &

# Build and push Docker image
make docker-build docker-push IMG=ghcr.io/subhankar720/helmsman-operator:latest

# Update deployment image
kubectl set image deployment/helmsman-operator-controller-manager \
  -n helmsman-operator-system \
  manager=ghcr.io/subhankar720/helmsman-operator:latest \
  --context kind-helmsman-onprem
```

---

## Additional Issues Discovered During Cleanup (2026-08-23)

### Issue 4: AppDeployment Stuck in Deletion (Finalizer Blocking)

**Symptoms:**
```bash
kubectl delete appdeployment operator-sample-app -n sample-app --context kind-helmsman-onprem
# Command returns but resource stays in "Terminating" state
kubectl get appdeployment operator-sample-app -n sample-app --context kind-helmsman-onprem -o yaml
# Shows: deletionTimestamp set, finalizers: ["platform.helmsman.dev/finalizer"]
```

**Root Cause:**
The AppDeployment has a finalizer (`platform.helmsman.dev/finalizer`) that blocks deletion until the operator's `handleDeletion` function completes cleanup. The operator must be running to process the deletion.

**Why It Was Stuck:**
1. **Operator not running** - The helmsman-operator was not running (was running locally earlier but stopped)
2. **Finalizer cleanup logic** - In `internal/controller/appdeployment_controller.go:197-233`, `handleDeletion()`:
   - Reads platform config from secret
   - Calls `deleteKeycloakClient()` to remove Keycloak client
   - Removes finalizer to allow CR deletion
   - **All owned resources (StatefulSet, Services, etc.) are deleted via owner references once finalizer is removed**

**Debugging Steps:**
```bash
# Check if operator is running
ps aux | grep helmsman-operator
# If not running, start it:
cd /home/subhankar/projects/helmsman/helmsman-operator
go run ./cmd/main.go --leader-elect=false &

# Check AppDeployment status
kubectl get appdeployment operator-sample-app -n sample-app --context kind-helmsman-onprem -o yaml
# Look for: deletionTimestamp, finalizers
```

**Fix:**
```bash
# Start operator (if not running)
cd /home/subhankar/projects/helmsman/helmsman-operator
go run ./cmd/main.go --leader-elect=false &

# Wait for reconciliation to process deletion
sleep 10

# Verify deletion
kubectl get appdeployment operator-sample-app -n sample-app --context kind-helmsman-onprem
# Should return: Error from server (NotFound)
```

**Force Delete (Emergency Only - Skips Keycloak Cleanup):**
```bash
# WARNING: Leaves Keycloak client orphaned
kubectl patch appdeployment operator-sample-app -n sample-app \
  --context kind-helmsman-onprem \
  -p '{"metadata":{"finalizers":[]}}' --type=merge
```

---

### Issue 5: Keycloak Client Not Deleted - Dual URL Configuration Required

**Symptoms:**
- Operator logs during deletion:
  ```
  ERROR Failed to delete Keycloak client — continuing with finalizer removal
  error: "failed to get admin token: failed to call Keycloak token endpoint: 
  Post \"http://host.docker.internal:8081/realms/master/protocol/openid-connect/token\": 
  context deadline exceeded"
  ```
- Keycloak client `operator-sample-app` still exists after AppDeployment deletion

**Root Cause:**
The operator runs on the **host machine** (not in Docker), so `host.docker.internal` doesn't resolve. The platform config secret only had one URL:
```yaml
keycloak-url: http://host.docker.internal:8081  # Only this was set
```

But the code in `internal/controller/appdeployment_controller.go` and `keycloak.go` supports **two separate URLs**:
- `KeycloakURL` (from `keycloak-url`): Used for **admin operations** (token endpoint, client management) - must be accessible from where operator runs
- `KeycloakOIDCURL` (from `keycloak-oidc-url`): Used for **in-cluster OIDC discovery** (issuer URL in Vault) - must be accessible from pods

**Code References:**
- `appdeployment_controller.go:247-248` - Reads both from secret
- `appdeployment_controller.go:256-257` - Falls back if `keycloak-oidc-url` not set
- `keycloak.go:26` - Uses `KeycloakURL` for admin token endpoint
- `keycloak.go:81,218` - Uses `KeycloakURL` for admin realm API
- `keycloak.go:149` - Uses `KeycloakOIDCURL` for issuer URL construction

**Fix - Update Platform Config Secret with Both URLs:**
```bash
# keycloak-url = http://localhost:8081 (for operator on host)
# keycloak-oidc-url = http://host.docker.internal:8081 (for in-cluster pods)
kubectl patch secret helmsman-platform-config -n sample-app \
  --context kind-helmsman-onprem \
  -p '{"data":{
    "keycloak-url":"aHR0cDovL2xvY2FsaG9zdDo4MDgx",
    "keycloak-oidc-url":"aHR0cDovL2hvc3QuZG9ja2VyLmludGVybmFsOjgwODE="
  }}'

# Verify both URLs
kubectl get secret helmsman-platform-config -n sample-app --context kind-helmsman-onprem \
  -o jsonpath='{.data}' | python3 -c "
import sys, json, base64
d=json.load(sys.stdin)
print('keycloak-url:', base64.b64decode(d['keycloak-url']).decode())
print('keycloak-oidc-url:', base64.b64decode(d['keycloak-oidc-url']).decode())
"
```

**Expected Output:**
```
keycloak-url: http://localhost:8081
keycloak-oidc-url: http://host.docker.internal:8081
```

---

### Issue 6: Orphaned Keycloak Client Cleanup

**Situation:**
After AppDeployment was force-deleted (finalizer removed but Keycloak cleanup failed), the Keycloak client `operator-sample-app` remained in Keycloak.

**Manual Cleanup Steps:**
```bash
# 1. Get admin token from master realm
TOKEN=$(curl -s -d "username=admin&password=helmsman123&grant_type=password&client_id=admin-cli" \
  http://localhost:8081/realms/master/protocol/openid-connect/token | jq -r '.access_token')

# 2. Find the client ID
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients?clientId=operator-sample-app" | python3 -m json.tool

# 3. Delete the client (use the 'id' field from output)
CLIENT_ID="0dfcedd2-a9ef-47d0-b736-ab1e3688ba78"  # From step 2
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients/$CLIENT_ID"

# 4. Verify deletion
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients?clientId=operator-sample-app" | python3 -m json.tool
# Should return: []
```

---

## Complete Platform Config Secret Reference

The `helmsman-platform-config` secret in each app namespace must contain:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: helmsman-platform-config
  namespace: <app-namespace>
data:
  # Keycloak Admin URL - accessible from operator (host or in-cluster)
  # For operator running on host: http://localhost:8081
  # For operator running in-cluster: http://keycloak:8080 or http://host.docker.internal:8081
  keycloak-url: <base64>

  # Keycloak OIDC URL - accessible from application pods for OIDC discovery
  # For pods in Kind/Docker Desktop: http://host.docker.internal:8081
  # For pods in real cluster: http://keycloak:8080
  keycloak-oidc-url: <base64>

  keycloak-realm: <base64>           # e.g., helmsman
  keycloak-admin-user: <base64>      # e.g., admin
  keycloak-admin-password: <base64>  # e.g., helmsman123

  # Vault URL - accessible from operator and external-secrets controller
  vault-url: <base64>                # e.g., http://172.18.0.2:30082 (for in-cluster)
  vault-token: <base64>              # e.g., root (dev mode)
```

**Generate base64 values:**
```bash
echo -n "http://localhost:8081" | base64  # aHR0cDovL2xvY2FsaG9zdDo4MDgx
echo -n "http://host.docker.internal:8081" | base64  # aHR0cDovL2hvc3QuZG9ja2VyLmludGVybmFsOjgwODE=
echo -n "helmsman" | base64  # aGVsbXNtYW4=
echo -n "admin" | base64  # YWRtaW4=
echo -n "helmsman123" | base64  # aGVsbXNtYW4xMjM=
echo -n "http://172.18.0.2:30082" | base64  # aHR0cDovLzE3Mi4xOC4wLjI6MzAwODI=
echo -n "root" | base64  # cm9vdA==
```

---

## Scenario 5: AppDeployment Stuck in Terminating

**Diagnosis:**
```bash
kubectl get appdeployment operator-sample-app -n sample-app --context kind-helmsman-onprem -o yaml
# Check: deletionTimestamp is set, finalizers contains "platform.helmsman.dev/finalizer"

# Check if operator is running
ps aux | grep helmsman-operator
```

**Fix:**
```bash
# 1. Start operator if not running
cd /home/subhankar/projects/helmsman/helmsman-operator
go run ./cmd/main.go --leader-elect=false &

# 2. Wait for reconciliation (check logs for "Running cleanup for deleted AppDeployment")
sleep 15

# 3. Verify deletion complete
kubectl get appdeployment operator-sample-app -n sample-app --context kind-helmsman-onprem
# Should return: Error from server (NotFound)

# 4. Verify Keycloak client deleted
TOKEN=$(curl -s -d "username=admin&password=helmsman123&grant_type=password&client_id=admin-cli" \
  http://localhost:8081/realms/master/protocol/openid-connect/token | jq -r '.access_token')
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients?clientId=operator-sample-app" | python3 -m json.tool
# Should return: []
```

---

## Scenario 6: Keycloak Client Orphaned After Deletion

**Diagnosis:**
```bash
# Check if Keycloak client still exists
TOKEN=$(curl -s -d "username=admin&password=helmsman123&grant_type=password&client_id=admin-cli" \
  http://localhost:8081/realms/master/protocol/openid-connect/token | jq -r '.access_token')
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients?clientId=operator-sample-app" | python3 -m json.tool
```

**Fix:**
```bash
# Get client ID from output above, then delete
CLIENT_ID="<id-from-output>"
TOKEN=$(curl -s -d "username=admin&password=helmsman123&grant_type=password&client_id=admin-cli" \
  http://localhost:8081/realms/master/protocol/openid-connect/token | jq -r '.access_token')
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients/$CLIENT_ID"
echo "Deleted"

# Verify
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8081/admin/realms/helmsman/clients?clientId=operator-sample-app" | python3 -m json.tool
# Should return: []
```

---

## Root Cause Summary: Dual URL Architecture

| Component | URL Used | Must Be Accessible From |
|-----------|----------|------------------------|
| Operator (Keycloak admin) | `keycloak-url` | Where operator runs (host or in-cluster) |
| Operator (Vault write) | `vault-url` | Where operator runs |
| ESO Controller (Vault read) | `vault-url` (via ClusterSecretStore) | Cluster (via service IP) |
| oauth2-proxy / App (OIDC discovery) | `keycloak-oidc-url` (via issuer-url in secret) | Inside application pods |
| Keycloak (self) | N/A | Binds to 0.0.0.0:8081 in container |

**Common Mistake:** Using only `keycloak-url` with `host.docker.internal` works for in-cluster pods but breaks operator running on host.

**Correct Setup:**
```bash
# For operator on host + pods in Kind/Docker Desktop:
keycloak-url=http://localhost:8081          # Operator reaches Keycloak via localhost
keycloak-oidc-url=http://host.docker.internal:8081  # Pods reach Keycloak via Docker DNS
```

---

## Monitoring & Alerting Recommendations

1. **Add ExternalSecret monitoring** - Alert on `Ready: False` condition
2. **Add pod readiness monitoring** - Alert when pod not `3/3 Ready` for > 5 minutes
3. **Add Vault secret freshness check** - Alert if ExternalSecret not refreshed in > 2 hours
4. **Add operator health check** - Alert if operator not reconciling (check reconcile timestamp in AppDeployment status)

---

## Additional Issues Discovered During Hub Cluster Setup (2026-08-24)

### Issue 7: ArgoCD Admin Password Reset After Secret Deletion

**Symptoms:**
```bash
argocd login localhost:8080 --insecure --username admin --password <password>
# Returns: Invalid username or password
```

**Root Cause:**
The `argocd-secret` in the `argocd` namespace was deleted. ArgoCD does not auto-recreate this secret. When a new `argocd-server` pod starts, it crashes with `secret "argocd-secret" not found`.

**Debugging Steps:**
```bash
# Check if argocd-secret exists
kubectl get secret argocd-secret -n argocd --context kind-helmsman-hub

# Check pod status
kubectl get pods -n argocd --context kind-helmsman-hub -l app.kubernetes.io/name=argocd-server

# Check logs for fatal error
kubectl logs -n argocd --context kind-helmsman-hub <argocd-server-pod> --tail=5
# Shows: {"level":"fatal","msg":"secret \"argocd-secret\" not found"}
```

**Fix:**
```bash
# 1. Generate bcrypt hash for new password
BCRYPT_HASH=$(python3 -c "import crypt; print(crypt.crypt('admin123', crypt.mksalt(crypt.METHOD_BLOWFISH)))")

# 2. Generate server secret key
SERVER_SECRET=$(openssl rand -base64 32)

# 3. Create argocd-secret with required fields
kubectl create secret generic argocd-secret \
  -n argocd \
  --from-literal=admin.password="$BCRYPT_HASH" \
  --from-literal=admin.passwordMtime="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --from-literal=server.secretkey="$SERVER_SECRET" \
  --context kind-helmsman-hub

# 4. Restart argocd-server deployment
kubectl rollout restart deployment argocd-server -n argocd --context kind-helmsman-hub
kubectl rollout status deployment argocd-server -n argocd --context kind-helmsman-hub --timeout=120s

# 5. Login with new password
argocd login localhost:8080 --insecure --username admin --password admin123
```

**Prevention:**
- Do not delete `argocd-secret` manually
- If password reset is needed, use `argocd update-password` command instead
- Backup `argocd-secret` before making changes

---

### Issue 8: ExternalSecret API Version Mismatch (v1beta1 Not Served)

**Symptoms:**
```bash
kubectl get externalsecret <name> -n <namespace>
# Error: no matches for kind "ExternalSecret" in version "external-secrets.io/v1beta1"
```

Operator logs show:
```
ERROR Reconciler error: no matches for kind "ExternalSecret" in version "external-secrets.io/v1beta1"
```

**Root Cause:**
The operator code creates ExternalSecrets using `external-secrets.io/v1beta1`, but the installed ESO version (v2.9.0) has `v1beta1` disabled by default. Only `v1` is served.

**Debugging Steps:**
```bash
# Check which versions are served
kubectl get crd externalsecrets.external-secrets.io -o json | \
  python3 -c "import sys, json; d=json.load(sys.stdin); [print(f'{v[\"name\"]}: served={v.get(\"served\", False)}') for v in d['spec']['versions']]"

# Check operator logs for version errors
kubectl logs -n <operator-namespace> -l app.kubernetes.io/name=<operator> --tail=20
```

**Fix:**
```bash
# Re-enable v1beta1 on the CRD
kubectl patch crd externalsecrets.external-secrets.io \
  --type='json' \
  -p='[{"op": "replace", "path": "/spec/versions/1/served", "value": true}]'

# Restart operator to pick up the change
kubectl rollout restart deployment/<operator-deployment> -n <operator-namespace>
```

**Better Fix (Code Change):**
Update operator code to use `external-secrets.io/v1` instead of `v1beta1`:
```go
// In internal/controller/appdeployment_controller.go and resources.go
Group:   "external-secrets.io",
Version: "v1",
Kind:    "ExternalSecret",
```

**Prevention:**
- Pin ESO version in Helm chart and test operator against that version
- Update operator code to use stable API versions (`v1`) before they are deprecated
- Add CI test that validates CRD versions match operator expectations

---

### Issue 9: Missing RBAC Permissions for ConfigMaps and CRDs

**Symptoms:**
```bash
# Operator logs show:
ERROR Failed to watch: configmaps is forbidden: User "system:serviceaccount:<ns>:<sa>" cannot list resource "configmaps" in API group "" at the cluster scope
```

**Root Cause:**
The operator's ClusterRole was missing permissions for:
1. `configmaps` - needed for Fluent Bit ConfigMap creation
2. `customresourcedefinitions` - needed to discover available API versions

**Debugging Steps:**
```bash
# Check operator logs for forbidden errors
kubectl logs -n <operator-namespace> -l app.kubernetes.io/name=<operator> --tail=50

# Check ClusterRole rules
kubectl get clusterrole <operator-role> -o yaml | grep -A 10 "rules:"
```

**Fix:**
```bash
# Add ConfigMap permissions
kubectl patch clusterrole <operator-role> --type='json' \
  -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": [""], "resources": ["configmaps"], "verbs": ["create", "delete", "get", "list", "patch", "update", "watch"]}}]'

# Add CRD list permissions
kubectl patch clusterrole <operator-role> --type='json' \
  -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": ["apiextensions.k8s.io"], "resources": ["customresourcedefinitions"], "verbs": ["get", "list", "watch"]}}]'

# Restart operator
kubectl rollout restart deployment/<operator-deployment> -n <operator-namespace>
```

**Prevention:**
- Review ClusterRole rules when adding new resource types to operator
- Add RBAC tests to CI pipeline
- Use `make manifests` to regenerate RBAC from code annotations

---

### Issue 10: ClusterSecretStore Token Secret Not Found

**Symptoms:**
```bash
# ExternalSecret status shows:
# could not get ClusterSecretStore "vault-backend": cannot get Kubernetes secret "vault-token" from namespace "<namespace>": secrets "vault-token" not found
```

**Root Cause:**
The `ClusterSecretStore` referenced a `tokenSecretRef` without specifying the namespace. ESO looked for the secret in the ExternalSecret's namespace instead of the `external-secrets` namespace where it was created.

**Debugging Steps:**
```bash
# Check ClusterSecretStore configuration
kubectl get clustersecretstore <name> -o yaml

# Check token secret location
kubectl get secret vault-token -n external-secrets
kubectl get secret vault-token -n <app-namespace>  # Should not exist here
```

**Fix:**
```bash
# Add namespace to tokenSecretRef
kubectl patch clustersecretstore <name> --type='json' \
  -p='[{"op": "add", "path": "/spec/provider/vault/auth/tokenSecretRef/namespace", "value": "external-secrets"}]'

# Restart ESO controller to pick up change
kubectl rollout restart deployment/external-secrets -n external-secrets
```

**Prevention:**
- Always specify `namespace` in `tokenSecretRef` for ClusterSecretStore
- Document required secrets and their locations in deployment guide
- Add validation in operator to ensure ClusterSecretStore is properly configured

---

### Issue 11: NetworkPolicy Blocking Egress to Keycloak

**Symptoms:**
```bash
# oauth2-proxy (adc) container logs show:
# Get "http://keycloak.keycloak:80/realms/helmsman/.well-known/openid-configuration": dial tcp 10.96.142.54:80: i/o timeout

# Pod shows 2/3 containers ready, adc is not ready
```

**Root Cause:**
The operator creates a `default-deny` NetworkPolicy that blocks all egress. While it creates an `allow-dns` policy, it does not create an egress rule for Keycloak. The `allow-ingress` policy only opens ingress port 4180, not egress to Keycloak.

**Debugging Steps:**
```bash
# Check NetworkPolicies in namespace
kubectl get networkpolicy -n <namespace>

# Check which ports are allowed
kubectl describe networkpolicy <default-deny-policy> -n <namespace>

# Test Keycloak connectivity from within namespace
kubectl run -n <namespace> --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 \
  keycloak-test -- curl -s --connect-timeout 5 http://<keycloak-service>/realms/<realm>/
```

**Fix:**
```bash
# Option 1: Create explicit egress policy for Keycloak
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

# Option 2: Use headless service to bypass NetworkPolicy
# Update keycloak-oidc-url to use headless service
kubectl patch secret helmsman-platform-config -n <namespace> \
  -p '{"data":{"keycloak-oidc-url":"<base64-of-headless-url>"}}'

# Restart pod to pick up changes
kubectl delete pod <pod-name> -n <namespace>
```

**Prevention:**
- Update operator code to create egress policies for required services (Keycloak, Vault)
- Make egress rules configurable based on AppDeployment spec
- Add network connectivity tests to reconciliation

---

### Issue 12: Wrong Keycloak Service Type Causing Connectivity Issues

**Symptoms:**
```bash
# adc container cannot reach Keycloak via ClusterIP service
# Error: dial tcp 10.96.142.54:80: i/o timeout
```

**Root Cause:**
The `keycloak` service is `NodePort` type, but within the cluster, pods should use the headless service `keycloak-headless.keycloak:8080` for direct pod access. The ClusterIP service may not correctly route to the Keycloak pod depending on network policy and service configuration.

**Debugging Steps:**
```bash
# List Keycloak services
kubectl get svc -n keycloak

# Test connectivity from app namespace
kubectl run -n <namespace> --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 \
  keycloak-test -- curl -s --connect-timeout 5 http://keycloak.keycloak:80/realms/<realm>/

# Test headless service
kubectl run -n <namespace> --rm -i --restart=Never \
  --image=curlimages/curl:8.8.0 \
  keycloak-test -- curl -s --connect-timeout 5 http://keycloak-headless.keycloak:8080/realms/<realm>/
```

**Fix:**
```bash
# Use headless service URL in platform config
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

**Prevention:**
- Document recommended Keycloak service endpoints for in-cluster access
- Use headless services for intra-cluster communication
- Add connectivity tests to operator reconciliation

---

## Issue 13: Operator CrashLoopBackOff Due to Cache Sync Timeout

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

2. **Add missing RBAC permissions (if not already present):**
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

   kubectl logs -n helmsman-operator deployment/helmsman-operator --tail=20
   # Shows: Starting Controller, Starting workers, Reconciling AppDeployment...
   ```

**Prevention:**

1. **Validate ClusterRoleBindings after deployment:**
   ```bash
   # Check that all referenced ClusterRoles exist
   for crb in $(kubectl get clusterrolebinding -o json | \
     jq -r '.items[] | select(.subjects[].kind=="ServiceAccount") | \
     select(.subjects[].namespace=="helmsman-operator") | .metadata.name'); do
     role=$(kubectl get clusterrolebinding $crb -o jsonpath='{.roleRef.name}')
     if ! kubectl get clusterrole $role >/dev/null 2>&1; then
       echo "ERROR: ClusterRoleBinding $crb references non-existent ClusterRole $role"
     fi
   done
   ```

2. **Add RBAC validation to CI/CD pipeline:**
   - After deploying the operator, run a job that verifies the service account can list/watch all required resources
   - Fail the deployment if any permission is missing

3. **Document required ClusterRoles:**
   Add to deployment guides that the `helmsman-operator-manager-role` ClusterRole must exist, or update the ClusterRoleBinding to use it.

4. **Use `kubectl auth can-i` for debugging:**
   ```bash
   # Test if operator SA has permissions
   kubectl auth can-i list services \
     --as=system:serviceaccount:helmsman-operator:helmsman-operator
   kubectl auth can-i list appdeployments.platform.helmsman.dev \
     --as=system:serviceaccount:helmsman-operator:helmsman-operator
   ```

5. **Add startup health checks:**
   The operator should fail fast with a clear error message if it cannot list required resources, rather than timing out after 2+ minutes.

---

## Hub Cluster Setup Checklist

When setting up the operator in a new cluster (e.g., `kind-helmsman-hub`), ensure:

1. **Install Infrastructure:**
   - Vault with NodePort service
   - Keycloak with NodePort/headless service
   - External Secrets Operator

2. **Create ClusterSecretStore:**
   ```bash
   kubectl apply -f - <<EOF
   apiVersion: external-secrets.io/v1
   kind: ClusterSecretStore
   metadata:
     name: vault-backend
   spec:
     provider:
       vault:
         server: "http://<vault-service>:<port>"
         path: "secret"
         version: "v2"
         auth:
           tokenSecretRef:
             name: vault-token
             key: token
             namespace: external-secrets
   EOF
   ```

3. **Deploy Operator:**
   ```bash
   # Generate manifests
   make manifests kustomize -C /path/to/helmsman-operator
   
   # Update image in deployment
   kubectl set image deployment/helmsman-operator-controller-manager \
     -n helmsman-operator-system \
     manager=ghcr.io/subhankar720/helmsman-operator:latest
   
   # Apply RBAC patches if needed
   kubectl patch clusterrole helmsman-operator-manager-role --type='json' \
     -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": [""], "resources": ["configmaps"], "verbs": ["create", "delete", "get", "list", "patch", "update", "watch"]}}]'
   kubectl patch clusterrole helmsman-operator-manager-role --type='json' \
     -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": ["apiextensions.k8s.io"], "resources": ["customresourcedefinitions"], "verbs": ["get", "list", "watch"]}}]'
   ```

4. **Create Platform Config Secret:**
   ```bash
   kubectl create secret generic helmsman-platform-config \
     -n <app-namespace> \
     --from-literal=keycloak-url=http://<keycloak-admin-url> \
     --from-literal=keycloak-oidc-url=http://<keycloak-oidc-url> \
     --from-literal=keycloak-realm=<realm> \
     --from-literal=keycloak-admin-user=<user> \
     --from-literal=keycloak-admin-password=<password> \
     --from-literal=vault-url=<vault-url> \
     --from-literal=vault-token=<token>
   ```

5. **Verify CRD Versions:**
   ```bash
   # Ensure ExternalSecret v1beta1 is served if operator uses it
   kubectl get crd externalsecrets.external-secrets.io -o json | \
     python3 -c "import sys, json; d=json.load(sys.stdin); [print(f'{v[\"name\"]}: served={v.get(\"served\", False)}') for v in d['spec']['versions']]"
   ```

---

## Issue 14: Keycloak Client List Unmarshalling Bug

**Symptoms:**
```bash
# Operator logs show:
# ERROR failed to decode client list: json: cannot unmarshal object into Go value of type []struct { ID string }
```

**Root Cause:**
The Keycloak Admin API `/clients?clientId=<app-name>` endpoint can return either:
- A JSON array `[{...}]` when multiple/no clients match
- A single JSON object `{...}` when exactly one client matches

The original code only handled arrays:
```go
var clients []struct{ ID string `json:"id"` }
json.NewDecoder(resp.Body).Decode(&clients) // Fails when response is a single object
```

**Fix Applied:**
Added `decodeKeycloakClientList()` helper in `internal/controller/keycloak.go` that tries array first, then falls back to single object:
```go
func decodeKeycloakClientList(data []byte) ([]keycloakClientListItem, error) {
    var asArray []keycloakClientListItem
    if err := json.Unmarshal(data, &asArray); err == nil {
        return asArray, nil
    }
    var asObject keycloakClientListItem
    if err := json.Unmarshal(data, &asObject); err != nil {
        return nil, err
    }
    return []keycloakClientListItem{asObject}, nil
}
```

**Prevention:**
- Always handle both JSON array and object responses from REST APIs
- Add unit tests for edge cases in API response shapes

---

## Issue 15: NetworkPolicy Missing Egress Rules for Keycloak/Vault

**Symptoms:**
```bash
# oauth2-proxy (adc) container logs show:
# Get "http://keycloak.keycloak:80/realms/helmsman/.well-known/openid-configuration": dial tcp ... i/o timeout

# Pod shows 2/3 containers ready, adc is not ready
```

**Root Cause:**
The operator creates a `default-deny` NetworkPolicy that blocks all egress, but only creates an `allow-dns` policy (port 53). Pods cannot reach Keycloak (port 80) or Vault (port 8200).

**Fix Applied:**
Added two new NetworkPolicies in `internal/controller/resources.go`:
1. `<app>-allow-keycloak-egress` - allows TCP port 80
2. `<app>-allow-vault-egress` - allows TCP port 8200

**Prevention:**
- The operator should automatically create egress rules for all required backend services
- Add network connectivity tests to the reconciliation loop
- Document required egress ports in the AppDeployment spec

---

## Issue 16: ExternalSecret CRD Conversion Webhook Missing After Manual CRD Recreation

**Symptoms:**
```bash
# Operator logs show:
# ERROR Reconciler error: conversion webhook for external-secrets.io/v1beta1 failed: 
# Post "https://<webhook-service>/convert": service not found
```

**Root Cause:**
When ESO was force-uninstalled and the CRD was recreated without webhook configuration, the new CRD lacked the conversion webhook. The operator tried to use v1beta1, which requires webhook conversion.

**Fix:**
```bash
# Option 1: Remove webhook from CRD (if not running ESO webhook)
kubectl patch crd externalsecrets.external-secrets.io --type='json' \
  -p='[{"op": "remove", "path": "/spec/conversion"}]'

# Option 2: Or install ESO properly to provide the webhook
helm install external-secrets external-secrets/external-secrets \
  -n external-secrets --create-namespace
```

**Prevention:**
- Never force-delete CRDs; use `helm uninstall` properly
- If manual CRD recreation is needed, preserve the webhook configuration

---

## Issue 17: ESO Helm Install Fails Due to Orphaned CRDs Without Helm Metadata

**Symptoms:**
```bash
# helm install external-secrets ... fails with:
# CustomResourceDefinition "externalsecrets.external-secrets.io" exists and cannot be imported:
# invalid ownership metadata; missing key "meta.helm.sh/release-name"
```

**Root Cause:**
Old ESO CRDs were created outside of Helm (or force-deleted without removing annotations). Helm requires CRDs to have specific annotations to claim ownership.

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
- If force-deleting, also remove CRDs: `helm uninstall ... && kubectl delete crd ...`

---

## Contact & Escalation

- **Primary:** Platform Team (helmsman-operator maintainers)
- **Documentation:** This runbook + operator source code
- **Key Files:**
  - `internal/controller/appdeployment_controller.go` - Main reconciliation logic
  - `internal/controller/resources.go` - Resource builders (StatefulSet, ConfigMap, etc.)
  - `internal/controller/keycloak.go` - Keycloak/Vault integration
  - `config/samples/platform_v1alpha1_appdeployment.yaml` - Example AppDeployment CR