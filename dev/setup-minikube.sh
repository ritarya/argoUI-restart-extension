#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
UI_DIR="$ROOT_DIR/argocd-extension/ui"
MANIFESTS_DIR="$ROOT_DIR/argocd-extension/manifests"

echo "==> Checking ArgoCD version (need 2.7+ for proxy extensions)"
ARGOCD_VERSION=$(kubectl get deployment argocd-server -n argocd \
  -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+' | head -1 || echo "unknown")
echo "    ArgoCD image tag: $ARGOCD_VERSION"

echo ""
echo "==> Building UI extension bundle"
cd "$UI_DIR"
npm install
npm run build
echo "    Built: $UI_DIR/dist/extension.js"

echo ""
echo "==> Deploying mock RFC backend"
kubectl apply -f "$SCRIPT_DIR/mock-backend.yaml"
kubectl rollout status deployment/mock-rfc-validator -n argocd --timeout=60s

echo ""
echo "==> Creating ConfigMap with extension bundle"
kubectl create configmap argocd-rfc-extension \
  --from-file=extension.js="$UI_DIR/dist/extension.js" \
  --namespace argocd \
  --dry-run=client -o yaml | kubectl apply -f -

echo ""
echo "==> Patching argocd-cm with proxy extension config"
kubectl patch configmap argocd-cm -n argocd --patch-file "$MANIFESTS_DIR/argocd-cm-patch.yaml"

echo ""
echo "==> Enabling --enable-proxy-extension on argocd-server"
kubectl patch deployment argocd-server -n argocd --type=json -p='[
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/args/-",
    "value": "--enable-proxy-extension"
  }
]'

echo ""
echo "==> Patching argocd-server to load the extension ConfigMap via init container"
kubectl patch deployment argocd-server -n argocd --type=strategic -p "$(cat <<'EOF'
spec:
  template:
    spec:
      initContainers:
        - name: install-rfc-extension
          image: busybox
          command: [sh, -c, "cp /extension-src/extension.js /shared/extension.js"]
          volumeMounts:
            - name: extension-src
              mountPath: /extension-src
            - name: extensions
              mountPath: /shared
      containers:
        - name: argocd-server
          volumeMounts:
            - name: extensions
              mountPath: /tmp/extensions/rfc-validator
      volumes:
        - name: extension-src
          configMap:
            name: argocd-rfc-extension
        - name: extensions
          emptyDir: {}
EOF
)"

echo ""
echo "==> Waiting for argocd-server rollout"
kubectl rollout status deployment/argocd-server -n argocd --timeout=120s

echo ""
echo "==> Done. Access the ArgoCD UI:"
echo "    minikube service argocd-server -n argocd --url"
echo "    (or: kubectl port-forward svc/argocd-server -n argocd 8080:443)"
echo ""
echo "    Initial admin password:"
echo "    kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d"
