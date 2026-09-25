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

---

# Incident: 2026-09-13 — dev-up-gemini.sh Recovery Failure, Sanity Script Redesign, Spoke CNI Degradation

## Summary

**Trigger:** Asked to run `dev-up-gemini.sh` in recovery mode (no `--reset`) after a Docker Desktop restart, then to review the companion `helmsman-fleet/scripts/helmsman-sanity.sh`.
**Environment:** kind-helmsman-hub + kind-helmsman-onprem, both 13 days old at the time (long uptime, several prior Docker restarts).
**Outcome:** 6 distinct bugs found and fixed across `dev-up-gemini.sh`, `helmsman-sanity.sh`, and one live-cluster CNI issue. Final state: `dev-up-gemini.sh` recovery mode completes end-to-end (exit 0), `helmsman-sanity.sh` reports 44/44 checks passing with zero mutation of cluster state.

---

## Issue 1: `dev-up-gemini.sh` recovery mode silently died after Stage 6

**Symptom:** Running `./dev-up-gemini.sh` (no `--reset`) printed output through "Stage 7: ESO ClusterSecretStore Sync" and then just stopped — no error banner, no summary, exit code 1 with no visible cause in the piped log.

**How it was debugged:**
1. Ran the script under `bash -x` and captured the full trace to a file — the last lines showed the `kubectl apply -f -` for the `ClusterSecretStore` heredoc running, then immediately jumping to the `cleanup()` trap and exiting. Under `set -euo pipefail`, an unguarded failing command kills the whole script — this narrowed it to that one `kubectl apply`.
2. Re-ran the exact same heredoc manually (outside the script, with stderr visible) to see the real error:
   ```
   error: resource mapping not found for name: "vault-backend" namespace: "" from "STDIN": no matches for kind "ClusterSecretStore" in version "external-secrets.io/v1"
   ensure CRDs are installed first
   ```
3. Checked what versions the installed ESO CRD actually serves: `kubectl get crd clustersecretstores.external-secrets.io -o jsonpath='{.spec.versions[*].name}'` → `v1alpha1 v1beta1`. The script hardcoded `external-secrets.io/v1`, which doesn't exist on this ESO version.

**Root cause:** `dev-up-gemini.sh` (both the Stage 7 initial apply and the Stage 10 re-apply) used `apiVersion: external-secrets.io/v1` for `ClusterSecretStore`, but the installed ESO CRD only serves `v1alpha1`/`v1beta1`. Because the Stage 7 apply wasn't wrapped in an `if`/`||`, its failure took down the entire script under `set -e` — so every recovery run since this drifted silently stopped before ever reaching ArgoCD login/sync (Stages 9–10).

**Fix** (`dev-up-gemini.sh`):
- Changed `apiVersion: external-secrets.io/v1` → `external-secrets.io/v1beta1` at both call sites.
- Hardened the Stage 7 apply with `|| log_warn "ClusterSecretStore apply failed"` so a future API-version drift degrades to a warning instead of killing the script.

---

## Issue 2: ArgoCD admin password unrecoverable after `/tmp` was cleared

**Symptom:** After fixing Issue 1, the script ran further but every ArgoCD CLI login attempt in Stage 9 failed silently (no log line at all — the original code only logged on success).

**How it was debugged:**
1. Checked `/tmp/helmsman-argocd-pass` — the only place `dev-up-gemini.sh` ever stored the password — and it didn't exist (cleared since the last `--reset`, days ago).
2. Checked whether `argocd-initial-admin-secret` (the fallback source) still existed on the hub — it didn't; `--reset` mode deletes it right after bootstrap by design.
3. Checked the live `argocd-secret` — it had a bcrypt hash (`admin.password`), proving *some* password was set, but with no way to recover the plaintext from a hash.
4. Manually reset it to a known value: `argocd account bcrypt --password '<pass>'`, patched `argocd-secret` with the new hash, restarted `argocd-server`, and confirmed login worked.

**Root cause:** The password was only ever persisted to `/tmp/helmsman-argocd-pass`, written once during `--reset`. Once that file is gone (reboot, `/tmp` cleanup, or simply time), recovery mode has no way to know the real password and falls back to a hardcoded default that no longer matches the live bcrypt hash — with zero diagnostic output pointing at *why* login was failing.

**Fix** (`dev-up-gemini.sh`):
- Moved the password file to `~/.helmsman-dev/argocd-admin-password` (durable, survives `/tmp` clears and reboots), `chmod 600`.
- Reset mode now always writes it there; recovery mode reads from there first (env var still overrides).
- Added `log_warn` per failed login attempt and a `log_error` block with concrete remediation commands (the exact `argocd account bcrypt` + patch + restart sequence) if all 5 attempts fail.

---

## Issue 3: `sample-app-0`'s oauth2-proxy container crash-looping post-recovery

**Symptom:** After the script finished, `sample-app-0` showed `2/3 Ready`, `CrashLoopBackOff`. Logs:
```
error building OIDC ProviderVerifier: ... error performing request: Get "http://172.18.0.3:30081/realms/helmsman/.well-known/openid-configuration": dial tcp 172.18.0.3:30081: connect: connection refused
```

**How it was debugged:**
1. Checked the live `sample-app-oidc` secret's `issuer-url` — it already read the *correct* current hub IP (`172.18.0.2`), not the stale `172.18.0.3` from the crash log.
2. Checked `kubectl describe pod` events — `SandboxChanged 15m ago`, meaning the pod's containers had been restarting in place for 15 minutes, i.e. since *before* the platform-config secret was corrected this run.
3. Concluded: Kubernetes resolves `secretKeyRef` env vars once when a Pod's spec is materialized; a container *restarting inside the same Pod* does not re-fetch the secret. The secret was already fixed — the running Pod object just hadn't been recreated to pick it up.

**Root cause:** Not a bug in the script — a Kubernetes semantics gap the script doesn't handle: after `helmsman-platform-config`/`sample-app-oidc` change, any pod that was already running with the old values needs to be recreated (not just have its container restarted) to see the new env.

**Fix (this session, not code):** `kubectl delete pod sample-app-0 -n sample-app` → StatefulSet recreated it with fresh secret data → 3/3 Running immediately.

---

## Issue 4: `helmsman-sanity.sh` was doing dev-up-gemini.sh's job (and had its own bugs)

**Symptom:** Asked to run `helmsman-fleet/scripts/helmsman-sanity.sh` to double check the environment. It ran 27 checks, auto-fixed 3 things along the way (mutating cluster state), and still failed 1 check (ArgoCD CLI login).

**How it was debugged:** Read the full 960-line script. It interleaved genuine checks with extensive auto-remediation — `docker restart`/`docker start`, `kubectl rollout restart` on 7+ components, `kubectl patch`/`apply`/`delete` on secrets and ClusterSecretStores, `argocd app sync`/`refresh`, and force-deleting pods — which duplicates and risks racing with `dev-up-gemini.sh`. Traced the login failure to the same root cause as Issue 2: `ARGOCD_PASS` was sourced only from `argocd-initial-admin-secret` (deleted post-bootstrap), with no fallback to a durable file.

Also spotted while reading: the script wrote `helmsman-platform-config`'s `keycloak-url`/`vault-url` using the hub **control-plane** container IP, while separately probing Vault reachability against hub **worker** IPs — two different "hub IP" definitions used inconsistently in the same script.

**Root cause:** Scope creep — the script had accreted auto-fix logic over time until it was a second, overlapping recovery script instead of a pure status report, and it inherited the same password-persistence bug as `dev-up-gemini.sh` without the fix.

**Fix:** Rewrote `helmsman-sanity.sh` (v8 → v9) as a strictly read-only report:
- Removed every mutating action: no more `docker start`/`restart`, no `kubectl rollout restart`/`patch`/`apply`/`delete secret`/`annotate`, no `argocd app sync`/`refresh`/`wait`, no force-deleting pods. (Ephemeral `curl` probe pods used purely as connectivity tests, which delete themselves via `--rm`, were kept — they don't mutate persistent state.)
- Replaced the `PASS`/`FIXED` counters with `PASS`/`WARN`/`FAIL` — drift that used to trigger an auto-fix now prints a `⚠` warning describing the drift and pointing at `dev-up-gemini.sh` to actually fix it.
- Fixed the password lookup to read `~/.helmsman-dev/argocd-admin-password` first (same file `dev-up-gemini.sh` now writes), falling back to `argocd-initial-admin-secret` only if present.
- Unified the two conflicting "hub IP" definitions into one `HUB_IP` (worker IP, falling back to control-plane) used consistently for every check, matching what `dev-up-gemini.sh` actually configures.
- Script exits 0 only when `WARN==0 && FAIL==0`; otherwise it prints a single "Fix with: ./dev-up-gemini.sh" hint rather than trying to fix anything itself.

---

## Issue 5: ArgoCD CLI's native `--port-forward` mode ignores `--kube-context` from `ARGOCD_OPTS`

**Symptom:** After fixing the password lookup, the rewritten sanity script's login step still failed:
```
cannot find ready pod with selector: [app.kubernetes.io/name=argocd-server] - use the --{component}-name flag...
```
even though `argocd-server` was `1/1 Running` on the hub.

**How it was debugged:**
1. Checked `kubectl config current-context` — it was `kind-helmsman-onprem` (the **spoke**), not the hub. The script sets `export ARGOCD_OPTS="--port-forward --port-forward-namespace argocd --kube-context kind-helmsman-hub --grpc-web"` and expected `argocd login` to honor it.
2. Reproduced directly: `argocd login` with only `ARGOCD_OPTS` set → same failure. `argocd login` with the identical flags passed **directly on the command line** → succeeded immediately.
3. Confirmed the pattern generalizes: `argocd cluster list`/`app list` also ignored `ARGOCD_OPTS` for `--kube-context` and had to be given `--server`/`--insecure` explicitly.

**Root cause:** A CLI quirk/bug in this ArgoCD CLI version (v3.4.4) — `ARGOCD_OPTS` is not applied to `--kube-context`/`--port-forward` for `login`, `cluster list`, or `app list`, so those commands silently fell back to whatever kubectl context happened to be active, searching for the `argocd-server` pod in the wrong cluster (`kind-helmsman-onprem` has no `argocd` namespace at all).

**Fix:** Abandoned native `--port-forward` mode. Switched the sanity script's login flow to the same approach `dev-up-gemini.sh` already uses successfully: a plain `kubectl port-forward svc/argocd-server -n argocd 9091:80 &`, then `argocd login localhost:9091 --insecure`, with a `trap` to kill the port-forward on exit. Every downstream `argocd cluster list`/`app list`/`app get` call now passes `--server localhost:9091 --insecure` explicitly instead of relying on env-var context propagation.

---

## Issue 6: Spoke cluster's `kindnet` CNI in a degraded state — pod-to-pod traffic timing out

**Symptom:** The rewritten sanity script's last remaining failure: `OIDC sidecar /ping endpoint not reachable through sample-app service`. `curl` to the ClusterIP timed out after 5s.

**How it was debugged (ruled out layer by layer):**
1. Checked the Service/Endpoints — correct, pointed at the live pod IP.
2. Checked kubelet's own readiness probe on the same container — it was passing (`3/3 Ready`), meaning node→pod worked.
3. Restarted spoke `kube-proxy` (the standard first fix for ClusterIP issues) — did **not** help. Ruled out iptables/Service-routing as the sole cause.
4. Curled the pod's IP **directly**, bypassing the Service entirely — still timed out, and the probe pod was on the *same node* as the target pod. This ruled out cross-node routing and pointed at something lower than kube-proxy: the CNI layer itself.
5. Checked for `NetworkPolicy` objects that might be blocking it — found several (`sample-app-default-deny` + explicit allow rules for port 4180), but `kindnet` (kind's default CNI) doesn't enforce `NetworkPolicy` at all, so these were ruled out as inert.
6. Checked `kindnet` pod logs on the worker node — found the real signal:
   ```
   netlink receive: no such file or directory
   Failed to watch: ... dial tcp: lookup helmsman-onprem-control-plane: i/o timeout
   ```
7. Checked Docker's embedded DNS resolution of that hostname from inside the worker container (`getent hosts helmsman-onprem-control-plane`) — intermittently slow/failing, a known Docker Desktop/WSL2 issue that gets worse the longer containers stay up (this cluster was 13 days old with multiple prior Docker restarts).

**Root cause:** `kindnet`'s internal reconciliation loop (which watches Namespaces/NetworkPolicies via the Kubernetes API and programs routes) was intermittently failing because Docker's embedded DNS resolver (127.0.0.11) couldn't reliably resolve sibling container hostnames from inside the worker container. This degraded `kindnet` to the point that even same-node pod-to-pod routes weren't reliably programmed — a failure mode one layer below kube-proxy/Services, which is why restarting kube-proxy alone didn't fix it.

**Fix:** `kubectl rollout restart daemonset/kindnet -n kube-system --context kind-helmsman-onprem`. Confirmed fixed: direct probe went from timeout to `HTTP:200`, and the full sanity script then reported 44/44 passing with zero warnings/failures.

**Not yet fixed in code:** `dev-up-gemini.sh`'s network-recovery stage only restarts `kube-proxy`/`coredns` on the **hub** cluster; it has no equivalent recovery step for the **spoke** cluster's `kube-proxy`/`kindnet`. This class of failure (spoke-side CNI degradation after long uptime) currently requires the manual command above. Proposed follow-up: add a spoke-side network recovery stage to `dev-up-gemini.sh` mirroring the hub's, escalating from spoke `kube-proxy` restart to spoke `kindnet` restart if a ClusterIP connectivity probe still fails. Not yet applied — pending confirmation this is wanted.

---

## Files Changed This Session

| File | Change |
|---|---|
| `helmsman-operator/dev-up-gemini.sh` | Fixed `ClusterSecretStore` apiVersion (`v1`→`v1beta1`) at 2 call sites; guarded Stage 7 apply against `set -e`; moved ArgoCD password persistence from `/tmp` to `~/.helmsman-dev/argocd-admin-password`; added failure logging to the Stage 9 login retry loop |
| `helmsman-fleet/scripts/helmsman-sanity.sh` | Rewritten v8→v9: removed all auto-remediation (now purely read-only PASS/WARN/FAIL reporting); fixed password lookup; fixed inconsistent hub-IP source; replaced broken native `--port-forward` login with manual `kubectl port-forward` + explicit `--server`/`--insecure` on every ArgoCD CLI call |

## Live Fixes Applied (not code — cluster state)
- Reset the live `argocd-secret` bcrypt hash to match the persisted password file (one-time; future `--reset` runs regenerate this correctly on their own).
- Deleted `sample-app-0` once to clear a stale cached `secretKeyRef` env value.
- Restarted spoke `kube-proxy` and `kindnet` daemonsets to clear CNI degradation.