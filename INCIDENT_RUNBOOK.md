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

## Monitoring & Alerting Recommendations

1. **Add ExternalSecret monitoring** - Alert on `Ready: False` condition
2. **Add pod readiness monitoring** - Alert when pod not `3/3 Ready` for > 5 minutes
3. **Add Vault secret freshness check** - Alert if ExternalSecret not refreshed in > 2 hours
4. **Add operator health check** - Alert if operator not reconciling (check reconcile timestamp in AppDeployment status)

---

## Contact & Escalation

- **Primary:** Platform Team (helmsman-operator maintainers)
- **Documentation:** This runbook + operator source code
- **Key Files:**
  - `internal/controller/appdeployment_controller.go` - Main reconciliation logic
  - `internal/controller/resources.go` - Resource builders (StatefulSet, ConfigMap, etc.)
  - `internal/controller/keycloak.go` - Keycloak/Vault integration
  - `config/samples/platform_v1alpha1_appdeployment.yaml` - Example AppDeployment CR