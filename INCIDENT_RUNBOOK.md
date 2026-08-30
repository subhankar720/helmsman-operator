# Incident Runbook: Operator Sample App Deployment Issues

## Incident Summary
**Date:** 2026-08-30
**Environment:** kind-helmsman-onprem cluster
**Application:** sample-app (AppDeployment)
**Severity:** High - Application pods stuck in ContainerCreating/CrashLoopBackOff

---

## Root Cause Analysis

### Issue 1: Missing ClusterSecretStore on Spoke Cluster
**Symptoms:**
- ExternalSecret `sample-app-oidc` showed `Ready: False`
- Error: `could not get secret data from provider: error retrieving secret at .data[0], key: apps/sample-app/oidc, err: could not get ClusterSecretStore "vault-backend", ClusterSecretStore.external-secrets.io "vault-backend" not found`

**Root Cause:**
The `vault-backend` ClusterSecretStore was never created on the spoke cluster. The dev-up-gemini.sh script attempts to create it in Stage 7, but the ESO CRDs weren't ready yet, causing the apply to be skipped. Later stages didn't retry the creation.

**Resolution:** Manually created the ClusterSecretStore using the correct API version (v1alpha1, as v1beta1 was deprecated).

---

### Issue 2: Wrong OIDC Issuer URL in Vault
**Symptoms:**
- oauth2-proxy (adc) container crashing with: `Get "http://keycloak-headless.keycloak:8080/realms/helmsman/.well-known/openid-configuration": dial tcp: lookup keycloak-headless.keycloak on 10.96.0.10:53: no such host`

**Root Cause:**
In `internal/controller/keycloak.go:174`, the operator was using `cfg.KeycloakOIDCURL` (internal DNS name: `http://keycloak-headless.keycloak:8080`) to construct the issuer URL stored in Vault. This internal DNS name is only resolvable within the hub cluster, not from the spoke cluster pods.

The platform config secret had:
- `keycloak-url`: `http://172.18.0.3:30081` (external, accessible from spoke)
- `keycloak-oidc-url`: `http://keycloak-headless.keycloak:8080` (internal, only works in hub)

**Resolution:** Changed the issuer URL construction in `keycloak.go:174` to use `cfg.KeycloakURL` (the external URL) instead of `cfg.KeycloakOIDCURL`.

---

### Issue 3: Controller/ExternalSecret Ownership Conflict
**Symptoms:**
- Operator logs: `Object sample-app/sample-app-oidc is already owned by another ExternalSecret controller sample-app-oidc`
- The operator's `reconcileOIDCSecret()` was creating the secret directly while the Helm chart's ExternalSecret also owned the same secret

**Root Cause:**
The operator was doing dual secret management:
1. Creating an ExternalSecret via `ensureExternalSecret()` (managed by ESO)
2. Directly creating/updating the K8s secret via `reconcileOIDCSecret()` (managed by operator)

Both tried to own the same secret `sample-app-oidc`, causing a conflict.

**Resolution:** Removed the direct secret reconciliation (`reconcileOIDCSecret()` call) from the controller. The operator now only:
1. Registers the Keycloak client
2. Writes credentials to Vault
The Helm chart's ExternalSecret handles syncing from Vault to K8s secret.

---

## Detailed Fixes Applied

### Fix 1: Corrected Issuer URL in keycloak.go
**File:** `internal/controller/keycloak.go`
**Line:** 174
**Change:**
```go
// Before (wrong - uses internal DNS)
IssuerURL:    fmt.Sprintf("%s/realms/%s", cfg.KeycloakOIDCURL, cfg.KeycloakRealm),

// After (correct - uses external URL accessible from spoke)
IssuerURL:    fmt.Sprintf("%s/realms/%s", cfg.KeycloakURL, cfg.KeycloakRealm),
```

### Fix 2: Removed Conflicting Secret Reconciliation
**File:** `internal/controller/appdeployment_controller.go`
**Change:** Removed the `reconcileOIDCSecret()` call from the reconciliation loop (Step 10). The operator now relies solely on the ExternalSecret managed by the Helm chart.

### Fix 3: Created Missing ClusterSecretStore
**Command:**
```bash
kubectl --context kind-helmsman-onprem apply -f - <<EOF
apiVersion: external-secrets.io/v1alpha1
kind: ClusterSecretStore
metadata:
  name: vault-backend
spec:
  provider:
    vault:
      server: "http://172.18.0.3:30082"
      path: "secret"
      version: "v2"
      auth:
        tokenSecretRef:
          name: vault-token
          namespace: external-secrets
          key: token
EOF
```

---

## Current Status (After Fixes)
- ✅ **AppDeployment**: Ready=True
- ✅ **Pod**: 3/3 containers Running (app, adc/oauth2-proxy, fluent-bit)
- ✅ **OIDC Secret**: Correctly synced with external issuer URL (`http://172.18.0.3:30081/realms/helmsman`)
- ✅ **Operator**: Reconciling successfully without errors
- ✅ **oauth2-proxy**: Responding to health checks (200 OK on /ping)

---

## Will the Issue Occur Again with `dev-up-gemini.sh --reset`?

### YES - The Issue WILL Occur Again with --reset

**Reason:** The `dev-up-gemini.sh` script has these problems that will cause the same issues on a fresh reset:

1. **ClusterSecretStore Creation Timing (Stage 7):** The script tries to create the ClusterSecretStore in Stage 7 but only retries once if CRDs aren't ready. On a fresh cluster, ESO CRDs take longer to become available.

2. **No Automatic Retry for ClusterSecretStore:** The script applies the ClusterSecretStore once and doesn't verify it was created successfully or retry if it failed.

3. **Platform Config Secret Uses Internal URL:** The script creates the `helmsman-platform-config` secret with `keycloak-oidc-url` set to the internal DNS name (`http://keycloak-headless.keycloak:8080`), which will cause the same issuer URL issue when the operator writes to Vault.

---

## How to Fix dev-up-gemini.sh to Prevent Recurrence

### Required Changes to dev-up-gemini.sh:

1. **Update Platform Config Secret with Correct URLs:**
   ```bash
   # In Stage 6, change:
   --from-literal=keycloak-oidc-url="http://keycloak-headless.keycloak:8080" \
   # To:
   --from-literal=keycloak-oidc-url="http://${HUB_IP}:30081" \
   ```
   (Use the same external URL as `keycloak-url`)

2. **Add Robust ClusterSecretStore Creation with Retry:**
   ```bash
   # In Stage 7, replace with:
   log_step "Stage 7: ESO ClusterSecretStore Sync"
   
   log_info "Waiting for External Secrets CRDs on Spoke cluster..."
   CRD_READY=false
   for i in {1..60}; do
     if kubectl --context "$SPOKE_CTX" get crd clustersecretstores.external-secrets.io >/dev/null 2>&1; then
       CRD_READY=true
       break
     fi
     sleep 5
   done
   
   if $CRD_READY; then
     # Apply with retry
     for attempt in 1 2 3; do
       if kubectl --context "$SPOKE_CTX" apply -f - <<EOF > /dev/null 2>&1
   apiVersion: external-secrets.io/v1alpha1
   kind: ClusterSecretStore
   metadata:
     name: vault-backend
   spec:
     provider:
       vault:
         server: "http://${HUB_IP}:30082"
         path: "secret"
         version: "v2"
         auth:
           tokenSecretRef:
             name: vault-token
             namespace: external-secrets
             key: token
   EOF
       then
         log_ok "ClusterSecretStore vault-backend applied on Spoke"
         break
       else
         log_warn "ClusterSecretStore apply failed, attempt $attempt/3"
         sleep 10
       fi
     done
   else
     log_error "External Secrets CRDs not available on Spoke after 5 minutes"
     exit 1
   fi
   ```

3. **Verify ClusterSecretStore is Ready Before Proceeding:**
   ```bash
   # Add after Stage 7:
   log_info "Waiting for ClusterSecretStore to be ready..."
   for i in {1..30}; do
     STATUS=$(kubectl --context "$SPOKE_CTX" get clustersecretstore vault-backend -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
     if [ "$STATUS" = "True" ]; then
       log_ok "ClusterSecretStore vault-backend is Ready"
       break
     fi
     sleep 5
   done
   ```

---

## Quick Fix Commands for Future Incidents

If the issue occurs again, run these commands to fix it quickly:

```bash
# 1. Fix the platform config secret (use external URL for both)
kubectl --context kind-helmsman-onprem patch secret helmsman-platform-config -n sample-app \
  -p '{"data":{"keycloak-oidc-url":"aHR0cDovLzE3Mi4xOC4wLjM6MzAwODE="}}'
# (base64 of: http://172.18.0.3:30081)

# 2. Ensure ClusterSecretStore exists
kubectl --context kind-helmsman-onprem apply -f - <<EOF
apiVersion: external-secrets.io/v1alpha1
kind: ClusterSecretStore
metadata:
  name: vault-backend
spec:
  provider:
    vault:
      server: "http://172.18.0.3:30082"
      path: "secret"
      version: "v2"
      auth:
        tokenSecretRef:
          name: vault-token
          namespace: external-secrets
          key: token
EOF

# 3. Restart operator to pick up config change
kubectl --context kind-helmsman-onprem rollout restart deployment/helmsman-operator-controller-manager -n helmsman-operator-system

# 4. Force ExternalSecret resync
kubectl --context kind-helmsman-onprem annotate externalsecret sample-app-oidc -n sample-app \
  force-sync="$(date +%s)" --overwrite

# 5. Restart app pod to pick up new secret
kubectl --context kind-helmsman-onprem delete pod sample-app-0 -n sample-app

# 6. Verify all containers ready
kubectl --context kind-helmsman-onprem get pod sample-app-0 -n sample-app -w
```

---

## Verification Checklist After Fix

- [ ] `kubectl get pod sample-app-0 -n sample-app --context kind-helmsman-onprem` shows `3/3 Running`
- [ ] `kubectl get externalsecret sample-app-oidc -n sample-app --context kind-helmsman-onprem` shows `SecretSynced: True`
- [ ] `kubectl get secret sample-app-oidc -n sample-app --context kind-helmsman-onprem -o jsonpath='{.data.issuer-url}' | base64 -d` outputs `http://172.18.0.3:30081/realms/helmsman`
- [ ] `kubectl logs sample-app-0 -n sample-app --context kind-helmsman-onprem -c adc` shows "OAuthProxy configured" and "/ping" returning 200
- [ ] `kubectl logs sample-app-0 -n sample-app --context kind-helmsman-onprem -c app` shows "helmsman-sample-app starting on :8080"

---

## Root Cause Summary: Dual URL Architecture

| Component | URL Used | Must Be Accessible From |
|-----------|----------|------------------------|
| Operator (Keycloak admin) | `keycloak-url` | Where operator runs (hub cluster) |
| Operator (Vault write) | `vault-url` | Where operator runs (hub cluster) |
| ESO Controller (Vault read) | `vault-url` (via ClusterSecretStore) | Spoke cluster (via service IP) |
| oauth2-proxy / App (OIDC discovery) | `keycloak-oidc-url` (via issuer-url in secret) | Inside application pods (spoke cluster) |

**Key Insight:** The `keycloak-oidc-url` in platform config is used to construct the `issuer-url` stored in Vault. This URL must be accessible from the **spoke cluster pods**, not from the hub cluster where the operator runs. Using the internal hub cluster DNS (`keycloak-headless.keycloak`) breaks OIDC discovery from the spoke cluster.

---

## Handling IP Changes After Docker/Kind Restart

### The Problem
The hub cluster's Docker container IP (e.g., `172.18.0.3`) changes after a Docker/Kind restart. The platform config secret must be updated with the new IP for OIDC discovery to work from spoke cluster pods.

### The Solution: dev-up-gemini.sh Auto-Recovery

The `dev-up-gemini.sh` script handles this automatically:

1. **Stage 2 (IP Discovery):** Re-discovers current container IPs:
   ```bash
   HUB_IP=$(docker inspect "${HUB_CLUSTER_NAME}-worker" \
     --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
   ```

2. **Stage 6 (Platform Secrets Sync):** Updates `helmsman-platform-config` with the **new** HUB_IP for BOTH URLs:
   ```bash
   --from-literal=keycloak-url="http://${HUB_IP}:30081" \
   --from-literal=keycloak-oidc-url="http://${HUB_IP}:30081" \
   ```

3. **Operator Reconciliation:** On next reconcile (happens automatically every ~30s or on config change):
   - Reads updated platform config (Step 4)
   - Constructs new issuer URL using `cfg.KeycloakURL` (the external IP)
   - Writes new credentials to Vault (Step 5)

4. **ExternalSecret Sync:** ESO controller syncs the updated secret from Vault to K8s

5. **Pod Restart:** The pod needs to restart to pick up the new `ISSUER_URL` env var. This can be triggered by:
   - Manual: `kubectl delete pod sample-app-0 -n sample-app`
   - Or the operator could be enhanced to trigger rollout when issuer URL changes

### Verification After Restart

Run `dev-up-gemini.sh` (without --reset) after Docker restart:

```bash
./dev-up-gemini.sh
```

Then verify:
```bash
# Check platform config has new IP
kubectl get secret helmsman-platform-config -n sample-app -o jsonpath='{.data.keycloak-oidc-url}' | base64 -d
# Should output: http://<NEW_HUB_IP>:30081

# Check Vault has new issuer URL
curl -sH "X-Vault-Token: root" http://localhost:8082/v1/secret/data/apps/sample-app/oidc | \
  python3 -c "import sys, json; print(json.load(sys.stdin)['data']['data']['issuer-url'])"

# Restart pod to pick up new secret
kubectl delete pod sample-app-0 -n sample-app

# Verify all containers ready
kubectl get pod sample-app-0 -n sample-app -w
```

### If Running Without dev-up-gemini.sh (Manual Recovery)

```bash
# 1. Get new hub worker IP
HUB_IP=$(docker inspect helmsman-hub-worker --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')

# 2. Update platform config secret with new IP for BOTH URLs
kubectl patch secret helmsman-platform-config -n sample-app \
  -p "{\"data\":{\"keycloak-url\":\"$(echo -n \"http://${HUB_IP}:30081\" | base64 -w0)\",\"keycloak-oidc-url\":\"$(echo -n \"http://${HUB_IP}:30081\" | base64 -w0)\"}}"

# 3. Trigger operator reconciliation
kubectl annotate appdeployment sample-app -n sample-app reconcile-trigger="$(date +%s)" --overwrite

# 4. Force ExternalSecret sync
kubectl annotate externalsecret sample-app-oidc -n sample-app force-sync="$(date +%s)" --overwrite

# 5. Wait for sync, then restart pod
sleep 10
kubectl delete pod sample-app-0 -n sample-app
```