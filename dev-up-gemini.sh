#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# dev-up.sh — Helmsman Local Platform Bootstrap & Recovery Script
# =============================================================================

HUB_CLUSTER_NAME="helmsman-hub"
SPOKE_CLUSTER_NAME="helmsman-onprem"
HUB_CTX="kind-${HUB_CLUSTER_NAME}"
SPOKE_CTX="kind-${SPOKE_CLUSTER_NAME}"

ARGOCD_PASS_FILE="${ARGOCD_PASS_FILE:-$HOME/.helmsman-dev/argocd-admin-password}"
if [ -z "${ARGOCD_PASS:-}" ] && [ -f "$ARGOCD_PASS_FILE" ]; then
  ARGOCD_PASS="$(cat "$ARGOCD_PASS_FILE")"
fi
ARGOCD_PASS="${ARGOCD_PASS:-nyKTpDW-m4jQnODE}"
ARGOCD_USER="admin"
ARGOCD_NAMESPACE="argocd"
ARGOCD_PF_PORT="9090"
ARGOCD_PF_PID=""

KEYCLOAK_ADMIN_USER="${KEYCLOAK_ADMIN_USER:-admin}"
KEYCLOAK_ADMIN_PASS="${KEYCLOAK_ADMIN_PASS:-admin}"
VAULT_TOKEN="${VAULT_TOKEN:-root}"

FLEET_REPO="https://github.com/subhankar720/helmsman-fleet.git"
CLUSTER_SECRET_NAME="cluster-helmsman-onprem"

RESET_MODE=false

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

log_info()  { echo -e "${BLUE}[INFO]${NC}  $1"; }
log_ok()    { echo -e "${GREEN}[OK]${NC}    $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_step()  { echo -e "\n${BOLD}${CYAN}── $1 ──${NC}"; }

kill_port_process() {
    local port="$1"
    if command -v fuser >/dev/null 2>&1; then
        fuser -k "${port}/tcp" 2>/dev/null || true
    elif command -v lsof >/dev/null 2>&1; then
        lsof -ti:"${port}" | xargs kill -9 2>/dev/null || true
    fi
    pkill -f "port-forward.*${port}" 2>/dev/null || true
}

cleanup() {
    if [ -n "$ARGOCD_PF_PID" ]; then
        kill "$ARGOCD_PF_PID" 2>/dev/null || true
    fi
    kill_port_process "${ARGOCD_PF_PORT}"
}
trap cleanup EXIT

for arg in "$@"; do
  case $arg in
    --reset|--clean|-c) RESET_MODE=true ;;
    --help|-h)
      echo "Usage: ./scripts/dev-up.sh [OPTIONS]"
      echo "  --reset, --clean, -c    Full rebuild from scratch"
      echo "  --help, -h              Show this help"
      exit 0 ;;
  esac
done

echo -e "\n${BOLD}${BLUE}========================================${NC}"
echo -e "${BOLD}${BLUE}  Helmsman Platform Bootstrap           ${NC}"
if $RESET_MODE; then
  echo -e "${BOLD}${RED}  Mode: FULL RESET                      ${NC}"
else
  echo -e "${BOLD}${GREEN}  Mode: RECOVERY (Docker restart)       ${NC}"
fi
echo -e "${BOLD}${BLUE}========================================${NC}\n"

# =============================================================================
# RESET MODE — destroy and recreate clusters
# =============================================================================
if $RESET_MODE; then
  log_step "Stage R1: Destroying existing Kind clusters"
  kind delete cluster --name "$HUB_CLUSTER_NAME" 2>/dev/null && log_ok "Hub cluster deleted" || log_warn "Hub cluster not found"
  kind delete cluster --name "$SPOKE_CLUSTER_NAME" 2>/dev/null && log_ok "Spoke cluster deleted" || log_warn "Spoke cluster not found"

  log_step "Stage R2: Creating fresh Kind clusters"
  SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
  CLUSTERS_DIR="$(dirname "$SCRIPT_DIR")/clusters"
  
  if [ ! -f "$CLUSTERS_DIR/hub-cluster.yaml" ] && [ -d "${SCRIPT_DIR}/../helmsman-fleet/clusters" ]; then
    CLUSTERS_DIR="${SCRIPT_DIR}/../helmsman-fleet/clusters"
  fi
  
  REPO_DIR="$SCRIPT_DIR"

  if [ -f "$CLUSTERS_DIR/hub-cluster.yaml" ]; then
    kind create cluster --config "$CLUSTERS_DIR/hub-cluster.yaml"
  else
    log_error "hub-cluster.yaml not found in $CLUSTERS_DIR"
    exit 1
  fi
  log_ok "Hub cluster created"

  if [ -f "$CLUSTERS_DIR/onprem-spoke-cluster.yaml" ]; then
    kind create cluster --config "$CLUSTERS_DIR/onprem-spoke-cluster.yaml"
  else
    log_error "onprem-spoke-cluster.yaml not found in $CLUSTERS_DIR"
    exit 1
  fi
  log_ok "Spoke cluster created"

  log_step "Stage R3: Installing Argo CD on Hub"
  kubectl --context "$HUB_CTX" create namespace argocd --dry-run=client -o yaml | \
    kubectl --context "$HUB_CTX" apply -f -

  kubectl --context "$HUB_CTX" apply --server-side --force-conflicts -n argocd \
    -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml

  kubectl --context "$HUB_CTX" patch configmap argocd-cmd-params-cm \
    -n argocd --type merge \
    -p '{"data":{"reposerver.git.request.timeout":"300"}}'

  log_info "Waiting for Argo CD backend dependencies (redis, repo-server)..."
  kubectl --context "$HUB_CTX" rollout status deployment/argocd-redis -n argocd --timeout=300s
  kubectl --context "$HUB_CTX" rollout status deployment/argocd-repo-server -n argocd --timeout=300s

  log_info "Waiting for argocd-server to be ready..."
  kubectl --context "$HUB_CTX" rollout status deployment/argocd-server -n argocd --timeout=300s

  kubectl --context "$HUB_CTX" wait --for=condition=ready pod \
    -l app.kubernetes.io/name=argocd-server -n argocd --timeout=60s

  kubectl --context "$HUB_CTX" patch svc argocd-server -n argocd \
    -p '{"spec":{"type":"NodePort","ports":[{"port":443,"targetPort":8080,"nodePort":30080}]}}'

  ARGOCD_PASS=$(kubectl --context "$HUB_CTX" \
    get secret argocd-initial-admin-secret -n argocd \
    -o jsonpath="{.data.password}" | base64 -d)
  mkdir -p "$(dirname "$ARGOCD_PASS_FILE")"
  echo -n "$ARGOCD_PASS" > "$ARGOCD_PASS_FILE"
  chmod 600 "$ARGOCD_PASS_FILE"
  log_ok "Argo CD installed. Admin password saved to $ARGOCD_PASS_FILE"
  kubectl --context "$HUB_CTX" delete secret argocd-initial-admin-secret -n argocd

  kubectl --context "$HUB_CTX" rollout restart deployment/argocd-repo-server -n argocd
  kubectl --context "$HUB_CTX" rollout status deployment/argocd-repo-server \
    -n argocd --timeout=120s

  log_step "Stage R3.5: Pre-seeding Hub Platform Bootstrap Secrets"
  kubectl --context "$HUB_CTX" create namespace keycloak --dry-run=client -o yaml | \
    kubectl --context "$HUB_CTX" apply -f -
  
  kubectl --context "$HUB_CTX" create secret generic keycloak-admin-credentials \
    -n keycloak \
    --from-literal=admin-user="$KEYCLOAK_ADMIN_USER" \
    --from-literal=admin-password="$KEYCLOAK_ADMIN_PASS" \
    --from-literal=username="$KEYCLOAK_ADMIN_USER" \
    --from-literal=password="$KEYCLOAK_ADMIN_PASS" \
    --dry-run=client -o yaml | kubectl --context "$HUB_CTX" apply -f -
  log_ok "Keycloak admin credentials pre-seeded on Hub"

  log_step "Stage R4: Setting up Argo CD port-forward and login"
  kill_port_process "${ARGOCD_PF_PORT}"
  sleep 2
  nohup kubectl --context "$HUB_CTX" port-forward svc/argocd-server \
    -n argocd "${ARGOCD_PF_PORT}":80 > /tmp/argocd-pf.log 2>&1 &
  ARGOCD_PF_PID=$!
  sleep 8
  
  if [ -f "$ARGOCD_PASS_FILE" ]; then
    ARGOCD_PASS=$(cat "$ARGOCD_PASS_FILE")
  fi

  for attempt in 1 2 3; do
    if argocd login "localhost:${ARGOCD_PF_PORT}" \
        --username "$ARGOCD_USER" \
        --password "$ARGOCD_PASS" \
        --insecure > /dev/null 2>&1; then
      log_ok "Argo CD CLI logged in"
      break
    fi
    log_warn "Login attempt $attempt failed, retrying..."
    sleep 10
  done

  log_step "Stage R5: Registering fleet repository"
  argocd repo add "$FLEET_REPO" --server "localhost:${ARGOCD_PF_PORT}" --insecure 2>/dev/null || true
  log_ok "Fleet repo registered: $FLEET_REPO"

  log_step "Stage R6: Registering spoke cluster"
  kubectl --context "$SPOKE_CTX" apply -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: argocd-manager
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: argocd-manager-role
rules:
- apiGroups: ['*']
  resources: ['*']
  verbs: ['*']
- nonResourceURLs: ['*']
  verbs: ['*']
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: argocd-manager-role-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: argocd-manager-role
subjects:
- kind: ServiceAccount
  name: argocd-manager
  namespace: kube-system
---
apiVersion: v1
kind: Secret
metadata:
  name: argocd-manager-token
  namespace: kube-system
  annotations:
    kubernetes.io/service-account.name: argocd-manager
type: kubernetes.io/service-account-token
EOF

  sleep 5
  SPOKE_TOKEN=$(kubectl --context "$SPOKE_CTX" \
    get secret argocd-manager-token -n kube-system \
    -o jsonpath='{.data.token}' | base64 -d)

  SPOKE_IP=$(docker inspect "${SPOKE_CLUSTER_NAME}-control-plane" \
    --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')

  kubectl --context "$HUB_CTX" apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${CLUSTER_SECRET_NAME}
  namespace: ${ARGOCD_NAMESPACE}
  labels:
    argocd.argoproj.io/secret-type: cluster
    platform-enabled: "true"
type: Opaque
stringData:
  name: helmsman-onprem
  server: https://${SPOKE_IP}:6443
  config: |
    {"bearerToken":"${SPOKE_TOKEN}","tlsClientConfig":{"insecure":true}}
EOF
  log_ok "Spoke cluster registered at https://${SPOKE_IP}:6443"

  log_step "Stage R6.5: Pre-seeding Spoke Platform Bootstrap Secrets"
  kubectl --context "$SPOKE_CTX" create namespace external-secrets --dry-run=client -o yaml | \
    kubectl --context "$SPOKE_CTX" apply -f -
  
  kubectl --context "$SPOKE_CTX" create secret generic vault-token \
    -n external-secrets \
    --from-literal=token="$VAULT_TOKEN" \
    --dry-run=client -o yaml | kubectl --context "$SPOKE_CTX" apply -f -
  log_ok "Vault token secret pre-seeded on Spoke for ESO"

  log_step "Stage R7: Applying platform Argo CD Applications from fleet repo"
  TMP_FLEET=$(mktemp -d)
  git clone --depth=1 "$FLEET_REPO" "$TMP_FLEET" 2>/dev/null
  
  for app_file in "$TMP_FLEET"/platform/*/argocd-application.yaml \
                  "$TMP_FLEET"/platform/kyverno/policies/argocd-application.yaml; do
    if [ -f "$app_file" ]; then
      kubectl --context "$HUB_CTX" apply -f "$app_file"
      log_ok "Applied: $app_file"
    fi
  done

  if [ -f "$TMP_FLEET/applicationsets/apps-appset.yaml" ]; then
    kubectl --context "$HUB_CTX" apply -f "$TMP_FLEET/applicationsets/apps-appset.yaml"
    log_ok "Applied: helmsman-apps ApplicationSet"
  fi

  rm -rf "$TMP_FLEET"
  log_info "Waiting 30s for platform apps to begin syncing..."
  sleep 30
fi

# =============================================================================
# Stage 0: Helmsman Operator Deployment (Spoke)
# =============================================================================
log_step "Stage 0: Helmsman Operator Deployment"

if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  log_info "Building operator image..."
  docker build -t helmsman-operator:dev \
    --build-arg TARGETOS=linux \
    --build-arg TARGETARCH=amd64 \
    -f Dockerfile . > /dev/null 2>&1 || log_warn "Operator image build failed, will use published image"
  
  kind load docker-image helmsman-operator:dev --name "$SPOKE_CLUSTER_NAME" > /dev/null 2>&1 || true
  log_ok "Operator image loaded into Spoke cluster"
fi

OPERATOR_MANIFESTS="/tmp/helmsman-operator-manifests.yaml"
rm -f "$OPERATOR_MANIFESTS"

log_info "Generating operator manifests..."
if make manifests > /dev/null 2>&1; then
  log_ok "CRDs and RBAC generated"
else
  log_warn "make manifests failed"
fi

if [ -x "./bin/kustomize" ]; then
  if ./bin/kustomize build config/default > "$OPERATOR_MANIFESTS" 2>/dev/null; then
    log_ok "Manifests built via kustomize ($(wc -l < "$OPERATOR_MANIFESTS") lines)"
  else
    log_warn "kustomize build failed"
  fi
fi

if [ -f "$OPERATOR_MANIFESTS" ] && [ -s "$OPERATOR_MANIFESTS" ]; then
  sed -i 's|image: controller:latest|image: helmsman-operator:dev|g' "$OPERATOR_MANIFESTS"
  sed -i 's|image: ghcr.io/subhankar720/helmsman-operator:latest|image: helmsman-operator:dev|g' "$OPERATOR_MANIFESTS"
  
  log_info "Applying operator manifests to Spoke..."
  kubectl --context "$SPOKE_CTX" apply -f "$OPERATOR_MANIFESTS" > /dev/null 2>&1 || true
  log_ok "Operator manifests applied to Spoke"
fi

OPERATOR_DEPLOY="helmsman-operator-controller-manager"
OPERATOR_NS="helmsman-operator-system"
if kubectl --context "$SPOKE_CTX" get deployment "$OPERATOR_DEPLOY" -n "$OPERATOR_NS" >/dev/null 2>&1; then
  log_info "Waiting for operator deployment to be ready..."
  kubectl --context "$SPOKE_CTX" rollout status "deployment/$OPERATOR_DEPLOY" \
    -n "$OPERATOR_NS" --timeout=180s > /dev/null 2>&1 || true
  
  kubectl --context "$SPOKE_CTX" patch deployment "$OPERATOR_DEPLOY" -n "$OPERATOR_NS" \
    --type='json' \
    -p='[{"op": "add", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]' > /dev/null 2>&1 || true
  
  OPERATOR_READY=false
  for i in {1..30}; do
    if kubectl --context "$SPOKE_CTX" get pods -n "$OPERATOR_NS" -l app.kubernetes.io/name=helmsman-operator --no-headers 2>/dev/null | grep -q "Running"; then
      log_ok "Helmsman operator is running on Spoke"
      OPERATOR_READY=true
      break
    fi
    sleep 2
  done
fi

# =============================================================================
# Stage 1: Container health — start any stopped containers
# =============================================================================
log_step "Stage 1: Kind Container Health"

ALL_CONTAINERS=(
  "${HUB_CLUSTER_NAME}-control-plane"
  "${HUB_CLUSTER_NAME}-worker"
  "${HUB_CLUSTER_NAME}-worker2"
  "${SPOKE_CLUSTER_NAME}-control-plane"
  "${SPOKE_CLUSTER_NAME}-worker"
)

CONTAINERS_STARTED=false
for CONTAINER in "${ALL_CONTAINERS[@]}"; do
  STATUS=$(docker inspect "$CONTAINER" --format='{{.State.Status}}' 2>/dev/null || echo "not_found")
  CONTAINER_IP=$(docker inspect "$CONTAINER" \
    --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' 2>/dev/null || echo "")
  case "$STATUS" in
    running)
      if [ -z "$CONTAINER_IP" ] || [ "$CONTAINER_IP" = "invalid IP" ]; then
        log_warn "$CONTAINER has no IP — restarting..."
        docker restart "$CONTAINER" > /dev/null 2>&1 || true
        CONTAINERS_STARTED=true
      else
        log_ok "$CONTAINER running ($CONTAINER_IP)"
      fi
      ;;
    exited|stopped|created)
      log_warn "$CONTAINER is stopped — starting..."
      docker start "$CONTAINER" > /dev/null 2>&1 || true
      CONTAINERS_STARTED=true
      ;;
    not_found)
      log_error "$CONTAINER not found. Run with --reset to recreate clusters."
      exit 1
      ;;
  esac
done

if $CONTAINERS_STARTED; then
  log_info "Containers started — waiting 25s for API servers..."
  sleep 25
fi

# =============================================================================
# Stage 2: Discover IPs
# =============================================================================
log_step "Stage 2: IP Discovery"

HUB_IP=$(docker inspect "${HUB_CLUSTER_NAME}-worker" \
  --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' 2>/dev/null || \
  docker inspect "${HUB_CLUSTER_NAME}-control-plane" \
  --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')

SPOKE_IP=$(docker inspect "${SPOKE_CLUSTER_NAME}-control-plane" \
  --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')

log_ok "Hub worker IP: $HUB_IP"
log_ok "Spoke control-plane IP: $SPOKE_IP"

kubectl --context "$HUB_CTX" get nodes --no-headers > /dev/null 2>&1 && \
  log_ok "Hub API server reachable" || { log_error "Hub API server not responding"; exit 1; }
kubectl --context "$SPOKE_CTX" get nodes --no-headers > /dev/null 2>&1 && \
  log_ok "Spoke API server reachable" || { log_error "Spoke API server not responding"; exit 1; }

# =============================================================================
# Stage 3: Network Recovery
# =============================================================================
log_step "Stage 3: Network Recovery"

COREDNS_READY=$(kubectl --context "$HUB_CTX" get pods -n kube-system \
  -l k8s-app=kube-dns --no-headers 2>/dev/null | grep -c "Running" || echo "0")

if $CONTAINERS_STARTED || [ "${COREDNS_READY:-0}" -lt 1 ]; then
  log_info "Running network recovery (kube-proxy + CoreDNS)..."
  kubectl --context "$HUB_CTX" rollout restart daemonset/kube-proxy -n kube-system > /dev/null 2>&1
  kubectl --context "$HUB_CTX" rollout restart deployment/coredns -n kube-system > /dev/null 2>&1
  sleep 15
else
  log_ok "Network healthy — skipping recovery"
fi

# =============================================================================
# Stage 4: Argo CD Recovery
# =============================================================================
log_step "Stage 4: Argo CD Recovery"

if $CONTAINERS_STARTED; then
  for COMPONENT in argocd-redis argocd-server argocd-applicationset-controller; do
    kubectl --context "$HUB_CTX" rollout restart "deployment/$COMPONENT" -n argocd > /dev/null 2>&1
  done
  sleep 15
else
  log_ok "Argo CD recovery not needed"
fi

# =============================================================================
# Stage 5: Update Spoke Cluster Secret
# =============================================================================
log_step "Stage 5: Spoke Cluster IP Sync"

STORED_SERVER=$(kubectl --context "$HUB_CTX" \
  get secret "$CLUSTER_SECRET_NAME" -n argocd \
  -o jsonpath='{.data.server}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
EXPECTED_SERVER="https://${SPOKE_IP}:6443"

if [ "$STORED_SERVER" != "$EXPECTED_SERVER" ]; then
  log_warn "Spoke IP drift detected: $STORED_SERVER → $EXPECTED_SERVER"
  SPOKE_TOKEN=$(kubectl --context "$SPOKE_CTX" \
    get secret argocd-manager-token -n kube-system \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d)

  kubectl --context "$HUB_CTX" apply -f - <<EOF > /dev/null
apiVersion: v1
kind: Secret
metadata:
  name: ${CLUSTER_SECRET_NAME}
  namespace: ${ARGOCD_NAMESPACE}
  labels:
    argocd.argoproj.io/secret-type: cluster
    platform-enabled: "true"
type: Opaque
stringData:
  name: helmsman-onprem
  server: https://${SPOKE_IP}:6443
  config: |
    {"bearerToken":"${SPOKE_TOKEN}","tlsClientConfig":{"insecure":true}}
EOF
  log_ok "Cluster Secret updated → https://${SPOKE_IP}:6443"
else
  log_ok "Spoke cluster IP current ($SPOKE_IP)"
fi

# =============================================================================
# Stage 6: Update Platform Config & Secrets
# =============================================================================
log_step "Stage 6: Platform Secrets Sync"

APP_NAMESPACES=$(kubectl --context "$SPOKE_CTX" get namespaces \
  --no-headers -o custom-columns=":metadata.name" 2>/dev/null \
  | grep -v "^kube-\|^default\|^local-path\|^external-secrets\|^kyverno" || echo "sample-app")

for NS in $APP_NAMESPACES; do
  kubectl --context "$SPOKE_CTX" create namespace "$NS" --dry-run=client -o yaml | \
    kubectl --context "$SPOKE_CTX" apply -f - > /dev/null 2>&1 || true

  kubectl --context "$SPOKE_CTX" create secret generic helmsman-platform-config \
    -n "$NS" \
    --from-literal=keycloak-url="http://${HUB_IP}:30081" \
    --from-literal=keycloak-oidc-url="http://${HUB_IP}:30081" \
    --from-literal=keycloak-admin-user="$KEYCLOAK_ADMIN_USER" \
    --from-literal=keycloak-admin-password="$KEYCLOAK_ADMIN_PASS" \
    --from-literal=keycloak-realm="helmsman" \
    --from-literal=vault-url="http://${HUB_IP}:30082" \
    --from-literal=vault-token="$VAULT_TOKEN" \
    --dry-run=client -o yaml | \
    kubectl --context "$SPOKE_CTX" apply -f - > /dev/null 2>&1 || true
  log_ok "helmsman-platform-config updated in namespace: $NS"
done

# =============================================================================
# Stage 7: Update ESO ClusterSecretStore Vault URL (SPOKE Cluster)
# =============================================================================
log_step "Stage 7: ESO ClusterSecretStore Sync"

log_info "Waiting for External Secrets CRDs on Spoke cluster..."
CRD_READY=false
for i in {1..30}; do
  if kubectl --context "$SPOKE_CTX" get crd clustersecretstores.external-secrets.io >/dev/null 2>&1; then
    CRD_READY=true
    break
  fi
  sleep 3
done

if $CRD_READY; then
  kubectl --context "$SPOKE_CTX" apply -f - <<EOF > /dev/null 2>&1 || log_warn "ClusterSecretStore apply failed"
apiVersion: external-secrets.io/v1beta1
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
  log_ok "ClusterSecretStore vault-backend applied on Spoke → http://${HUB_IP}:30082"
else
  log_warn "External Secrets CRDs not available on Spoke yet. Skipping initial ClusterSecretStore apply."
fi

# =============================================================================
# Stage 8: Label app namespaces for Kyverno
# =============================================================================
log_step "Stage 8: Kyverno Namespace Labels"

for NS in $APP_NAMESPACES; do
  kubectl --context "$SPOKE_CTX" label namespace "$NS" \
    helmsman.dev/managed=true \
    --overwrite > /dev/null 2>&1 || true
  log_ok "Kyverno label applied: $NS"
done

# =============================================================================
# Stage 9: Argo CD CLI Login
# =============================================================================
log_step "Stage 9: Argo CD CLI Login"

if [ -f "$ARGOCD_PASS_FILE" ]; then
  ARGOCD_PASS=$(cat "$ARGOCD_PASS_FILE")
fi

kill_port_process "${ARGOCD_PF_PORT}"
sleep 1

nohup kubectl --context "$HUB_CTX" port-forward svc/argocd-server \
  -n argocd "${ARGOCD_PF_PORT}":80 > /tmp/argocd-pf.log 2>&1 &
ARGOCD_PF_PID=$!

sleep 3

LOGIN_OK=false
LOGIN_ERR=""
for attempt in 1 2 3 4 5; do
  if LOGIN_ERR=$(argocd login "localhost:${ARGOCD_PF_PORT}" \
      --username "$ARGOCD_USER" \
      --password "$ARGOCD_PASS" \
      --insecure 2>&1 > /dev/null); then
    log_ok "Argo CD CLI logged in via port-forward :${ARGOCD_PF_PORT}"
    LOGIN_OK=true
    break
  fi
  log_warn "Login attempt $attempt failed: ${LOGIN_ERR}"
  sleep 3
done

if ! $LOGIN_OK; then
  log_error "Argo CD CLI login failed after 5 attempts. If the password file is stale, reset it:"
  log_error "  argocd account bcrypt --password '<new-pass>' | xargs -I{} kubectl --context $HUB_CTX -n argocd patch secret argocd-secret -p '{\"stringData\":{\"admin.password\":\"{}\"}}'"
  log_error "  echo -n '<new-pass>' > $ARGOCD_PASS_FILE && kubectl --context $HUB_CTX -n argocd rollout restart deployment/argocd-server"
fi

# =============================================================================
# Stage 10: Sync platform and application ArgoCD apps
# =============================================================================
log_step "Stage 10: ArgoCD App Sync"

if $LOGIN_OK; then
  PLATFORM_APPS=("platform-eso" "platform-vault" "platform-keycloak" "platform-kyverno" "platform-kyverno-policies")

  for app in "${PLATFORM_APPS[@]}"; do
    if argocd app get "$app" --server "localhost:${ARGOCD_PF_PORT}" --insecure > /dev/null 2>&1; then
      argocd app sync "$app" --async --server "localhost:${ARGOCD_PF_PORT}" --insecure > /dev/null 2>&1 || true
      log_ok "Sync triggered: $app"
    fi
  done

  log_info "Waiting for Keycloak deployment on Hub to be ready..."
  kubectl --context "$HUB_CTX" rollout status deployment/keycloak -n keycloak --timeout=300s > /dev/null 2>&1 || log_warn "Keycloak rollout wait timed out"

  log_info "Waiting for platform-eso deployment to settle on Spoke..."
  sleep 15

  if ! kubectl --context "$SPOKE_CTX" get clustersecretstore vault-backend >/dev/null 2>&1; then
    log_info "Re-applying ClusterSecretStore vault-backend post ESO sync..."
    kubectl --context "$SPOKE_CTX" apply -f - <<EOF > /dev/null 2>&1 || true
apiVersion: external-secrets.io/v1beta1
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
  fi

  if argocd app get sample-app-helmsman-onprem --server "localhost:${ARGOCD_PF_PORT}" --insecure > /dev/null 2>&1; then
    argocd app sync sample-app-helmsman-onprem --force --server "localhost:${ARGOCD_PF_PORT}" --insecure > /dev/null 2>&1 || true
    log_ok "Sync triggered: sample-app-helmsman-onprem"
  fi
fi

# =============================================================================
# Stage 11: Health Verification & Sanity Checks
# =============================================================================
log_step "Stage 11: Health Verification & Sanity Checks"

SANITY_PASS=true

OPERATOR_PODS=$(kubectl --context "$SPOKE_CTX" get pods -n "$OPERATOR_NS" -l app.kubernetes.io/name=helmsman-operator --no-headers 2>/dev/null | grep -c "Running" || echo "0")
OPERATOR_PODS=$(echo "$OPERATOR_PODS" | tr -d '\n' | awk '{print $NF}')
if [ "${OPERATOR_PODS:-0}" -gt 0 ] 2>/dev/null; then
  log_ok "Helmsman operator: $OPERATOR_PODS pod(s) Running in $OPERATOR_NS"
else
  log_error "Helmsman operator: NOT running in $OPERATOR_NS"
  SANITY_PASS=false
fi

if kubectl --context "$SPOKE_CTX" get clustersecretstore vault-backend >/dev/null 2>&1; then
  log_ok "ClusterSecretStore vault-backend present on Spoke"
else
  log_error "ClusterSecretStore vault-backend missing on Spoke"
  SANITY_PASS=false
fi

if $SANITY_PASS; then
  echo -e "\n${BOLD}${GREEN}========================================${NC}"
  echo -e "${BOLD}${GREEN}  BOOTSTRAP COMPLETED SUCCESSFULLY      ${NC}"
  echo -e "${BOLD}${GREEN}========================================${NC}"
else
  echo -e "\n${BOLD}${YELLOW}========================================${NC}"
  echo -e "${BOLD}${YELLOW}  SANITY CHECK COMPLETED WITH WARNINGS  ${NC}"
  echo -e "${BOLD}${YELLOW}========================================${NC}"
fi

log_info "ArgoCD UI: http://localhost:${ARGOCD_PF_PORT}"
log_info "Username: ${ARGOCD_USER}"
log_info "Password: ${ARGOCD_PASS}"