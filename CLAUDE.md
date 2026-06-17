# ArgoCD ITSM RFC Validation — Architecture Guide

## Project overview

This project extends the ArgoCD UI on an AWS EKS cluster to enforce ITSM Change Management
RFC validation before allowing pod restarts from the ArgoCD interface. An engineer
right-clicks a Deployment resource in the ArgoCD application tree and selects
"Restart (RFC required)" from the three-dot action menu. This triggers a two-part flow:
a Lua custom action writes a pending annotation on the Deployment, and a React resource
tab extension detects that annotation and surfaces the RFC validation form inline inside
the resource detail panel. Only after the RFC is validated does the tab apply the actual
`restartedAt` patch to trigger the rolling restart.

Validation is enforced at two levels:
- **UI gate** — the `registerResourceExtension` React tab drives the engineer through
  RFC input and approval before the restart patch is applied.
- **Hard backstop** — a `ValidatingWebhookConfiguration` independently verifies a valid
  RFC approval exists in Redis before admitting any `restartedAt` annotation patch on a
  Deployment, regardless of how it was triggered (UI or direct `kubectl`).

### Why this two-part approach

ArgoCD Lua resource actions execute server-side and complete immediately — they cannot
pause mid-execution to show a browser modal or await user input. The right-click action
menu therefore cannot host a validation dialog. The correct pattern is:

1. The Lua action does the minimum: writes `rfc-validation/status: pending` on the
   Deployment. This is the signal.
2. The React resource tab reads that annotation from the live resource props and renders
   the RFC validation UI. This is the gate.

The two parts are decoupled and communicate only via the Deployment annotation.

---

## Repository structure

```
.
├── argocd-extension/
│   ├── ui/                              # React UI extension bundle
│   │   ├── src/
│   │   │   ├── RFCValidationTab.tsx     # registerResourceExtension component (main gate)
│   │   │   ├── RFCStatusBadge.tsx       # Inline approved / rejected / pending badge
│   │   │   └── index.tsx                # Registers extension with extensionsAPI
│   │   ├── webpack.config.js
│   │   └── package.json
│   └── manifests/
│       ├── extension-cm.yaml            # ConfigMap mounting extension.js into argocd-server
│       └── argocd-cm-patch.yaml         # argocd-cm patch: Lua action + proxy extension config
│
├── rfc-middleware/
│   ├── src/
│   │   ├── main.go                      # HTTP server entrypoint
│   │   ├── validator.go                 # RFC state / window / scope validation logic
│   │   ├── itsm_client.go               # mTLS ITSM API client
│   │   └── cache.go                     # Redis cache read/write (TTL 60s)
│   ├── Dockerfile
│   └── manifests/
│       ├── deployment.yaml              # Deployment + ClusterIP Service
│       ├── networkpolicy.yaml           # Ingress: argocd-server only; egress: Redis + on-prem CIDR
│       ├── redis.yaml                   # Redis Deployment + ClusterIP (in-cluster cache)
│       └── secret.yaml                  # ITSM API key + mTLS client cert (or ESO ref)
│
├── admission-webhook/
│   ├── src/
│   │   └── webhook.go                   # ValidatingWebhookConfiguration handler
│   ├── Dockerfile
│   └── manifests/
│       ├── webhook-deployment.yaml
│       └── validatingwebhookconfig.yaml
│
└── audit/
    └── manifests/
        └── cloudwatch-fluentbit.yaml    # FluentBit DaemonSet → CloudWatch log group
```

---

## Component reference

### 1. Lua custom action (right-click menu entry)

**What it is:** A custom resource action defined in `argocd-cm` as a Lua script, scoped to
`apps/Deployment`. It appears in the three-dot action menu on every Deployment resource
inside any ArgoCD application. When clicked it does one thing only: writes a pending
annotation on the Deployment. It does not perform the restart itself.

**Critical note on built-in action replacement:** Defining
`resource.customizations.actions.apps_Deployment` in `argocd-cm` replaces the built-in
restart action for that resource kind entirely. You must explicitly re-declare the built-in
`restart` action in the same `definitions` block alongside the custom action, or engineers
lose the quick restart option. Use `discovery.lua` to control which actions are visible and
when (e.g. hide the built-in restart in production namespaces, show it freely in dev).

**`argocd-cm-patch.yaml` — Lua action section:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-cm
  namespace: argocd
data:
  # ── Custom resource actions for apps/Deployment ──────────────────────────
  resource.customizations.actions.apps_Deployment: |
    discovery.lua: |
      actions = {}
      -- Always show the RFC-gated restart
      actions["restart (RFC required)"] = { ["disabled"] = false }
      -- Re-declare built-in restart; optionally disable in prod namespaces
      actions["restart"] = { ["disabled"] = false }
      return actions

    definitions:
    - name: restart (RFC required)
      action.lua: |
        -- Write pending annotation; React tab detects this and renders the RFC form
        if obj.metadata.annotations == nil then
          obj.metadata.annotations = {}
        end
        -- os.* is unavailable in ArgoCD's Lua sandbox; timestamp is set by the React tab
        obj.metadata.annotations["rfc-validation/status"] = "pending"
        return obj

    - name: restart
      action.lua: |
        -- Built-in restart behaviour (patch restartedAt directly, no RFC gate)
        if obj.spec.template.metadata == nil then
          obj.spec.template.metadata = {}
        end
        if obj.spec.template.metadata.annotations == nil then
          obj.spec.template.metadata.annotations = {}
        end
        obj.spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"] =
          os.date("!%Y-%m-%dT%H:%M:%SZ")
        return obj
```

**User experience:**
1. Engineer right-clicks a Deployment in the ArgoCD resource tree.
2. Three-dot menu shows: `restart` and `restart (RFC required)`.
3. Engineer selects `restart (RFC required)`.
4. ArgoCD applies the Lua action → Deployment now has `rfc-validation/status: pending`.
5. Engineer clicks the "RFC Validation" tab in the resource detail panel — the React
   component has already detected the annotation and rendered the input form.

---

### 2. React resource tab extension (`registerResourceExtension`)

**What it is:** A React component registered via `window.extensionsAPI.registerResourceExtension`
for `apps/Deployment`. It adds an "RFC Validation" tab to the resource detail panel that
appears whenever an engineer clicks on a Deployment resource. The tab is always present but
only renders the validation form when the `rfc-validation/status: pending` annotation is
detected on the live resource.

**How it is mounted:**
```yaml
# extension-cm.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-rfc-extension
  namespace: argocd
  labels:
    app.kubernetes.io/name: argocd-rfc-extension
data:
  extension.js: |
    <webpack-bundled output of RFCValidationTab.tsx>
```

The `argocd-extension-installer` init container copies `extension.js` from this ConfigMap
into the `argocd-server` pod at `/tmp/extensions/rfc/extension.js` at startup. No changes
to the `argocd-server` image are required.

**Registration (`index.tsx`):**
```typescript
window.extensionsAPI.registerResourceExtension(
  RFCValidationTab,
  'apps',        // API group
  'Deployment',  // Kind
  'RFC Validation',
  { icon: 'fa-shield-check' }
);
```

**Component logic (`RFCValidationTab.tsx`):**

The component receives `{ application, resource, tree }` props from ArgoCD. `resource` is
the live Deployment manifest including its current annotations.

```typescript
const RFCValidationTab: React.FC<ResourceExtensionProps> = ({ application, resource }) => {
  const status = resource?.metadata?.annotations?.['rfc-validation/status'];

  // Tab states:
  // - No annotation / status absent → neutral state, show informational message
  // - status === 'pending'          → show RFC input form
  // - status === 'approved'         → show approved badge, restart already complete
  // - status === 'rejected'         → show rejection reason

  if (status !== 'pending') {
    return <RFCStatusBadge status={status} />;
  }

  return (
    <RFCValidationForm
      namespace={application.spec.destination.namespace}
      deploymentName={resource.metadata.name}
      onApproved={handleApproved}
    />
  );
};
```

**On RFC approval (`handleApproved`):**
1. Call `PATCH /apis/apps/v1/namespaces/{ns}/deployments/{name}` via ArgoCD's API proxy
   to apply:
   ```json
   {
     "metadata": {
       "annotations": {
         "rfc-validation/status":  "approved",
         "rfc-validation/rfc-id":  "<validated RFC ID>",
         "rfc-validation/approver": "<ITSM approver>",
         "kubectl.kubernetes.io/restartedAt": "<current UTC timestamp>"
       }
     }
   }
   ```
2. The `restartedAt` annotation change triggers the Kubernetes rolling restart.
3. The admission webhook intercepts this patch, verifies the RFC approval in Redis, and
   admits or denies accordingly.

**Proxy extension config (same `argocd-cm-patch.yaml`):**
```yaml
  extension.config: |
    extensions:
      - name: rfc-validator
        backend:
          services:
            - url: http://rfc-validator.argocd.svc.cluster.local:8080
```

**Enable the proxy extension feature flag** on `argocd-server` Deployment:
```yaml
containers:
  - name: argocd-server
    args:
      - --enable-proxy-extension   # alpha feature, required explicitly (ArgoCD v2.7+)
```

**Request flow through the proxy:**
```
Browser (RFCValidationTab)
  →  POST /extensions/rfc-validator/validate
     Cookie: argocd.token  (ArgoCD JWT — validated automatically by argocd-server)
  →  argocd-server proxies to rfc-validator.argocd.svc.cluster.local:8080/validate
     with forwarded headers: Argocd-Username, Argocd-User-Groups, Argocd-Application-Name
  →  RFC middleware validates against on-prem ITSM API
  ←  { approved, rfc_title, approver, window_start, window_end, reason }
```

---

### 3. RFC middleware service

**What it is:** A standalone Go HTTP service deployed as a Kubernetes `Deployment` with a
`ClusterIP` Service in the `argocd` namespace. Not exposed outside the cluster. Called
exclusively via the ArgoCD proxy extension from the React tab.

**Service DNS:** `rfc-validator.argocd.svc.cluster.local:8080`

**Validation logic (`validator.go`):**

On each `POST /validate` the middleware:
1. Checks Redis for a cached result keyed `rfc:{id}:{namespace}` (TTL 60s).
2. On cache miss, calls the on-prem ITSM API via `GET /api/now/table/change_request/{id}`
   (ServiceNow) or equivalent Jira SM endpoint, over mTLS through the VPN/Direct Connect.
3. Validates three conditions:
   - `state == "approved"` (or `"implement"` for ServiceNow)
   - Current UTC time falls within `start_date` – `end_date` of the change window
   - RFC's `cmdb_ci` or target environment label matches the pod's namespace
4. Writes result to Redis with 60s TTL.
5. Returns `{ approved, rfc_title, approver, window_start, window_end, reason }`.

**ITSM client (`itsm_client.go`):**
- mTLS enforced: client cert and key loaded from Kubernetes Secret
  `rfc-middleware-itsm-tls` (or pulled from AWS Secrets Manager via External Secrets Operator).
- Timeout: 5s. Retry: 2 attempts with 1s backoff.
- On ITSM API unavailability: returns `{ approved: false, reason: "itsm_unreachable" }`.
  Do not fail open.

**Environment variables:**
```
ITSM_BASE_URL        https://your-instance.service-now.com   # or Jira SM base URL
ITSM_TLS_CERT_PATH   /etc/itsm-tls/tls.crt
ITSM_TLS_KEY_PATH    /etc/itsm-tls/tls.key
REDIS_ADDR           redis.argocd.svc.cluster.local:6379
REDIS_TTL_SECONDS    60
LOG_LEVEL            info
```

**Deployment manifest key settings:**
```yaml
replicas: 2
resources:
  requests: { cpu: 50m, memory: 64Mi }
  limits:   { cpu: 200m, memory: 128Mi }
livenessProbe:  GET /healthz  initialDelaySeconds: 5
readinessProbe: GET /readyz   initialDelaySeconds: 3
```

---

### 4. Redis cache

In-cluster Redis `Deployment` + `ClusterIP` in the `argocd` namespace. Used by both the
RFC middleware (to cache ITSM validation results) and the admission webhook (to verify
approval at patch time).

- Key schema: `rfc:{rfc_id}:{namespace}`
- Value: `{ approved: bool, approver: string, window_end: timestamp }`
- TTL: 60 seconds (configurable via `REDIS_TTL_SECONDS`)
- No persistence needed (`appendonly no`). Data is ephemeral by design.
- Accessed only by `rfc-validator` and `argocd-rfc-webhook` pods (enforced via
  `NetworkPolicy`).

---

### 5. Admission webhook (hard backstop)

**What it is:** A `ValidatingWebhookConfiguration` that intercepts `PATCH` operations on
`Deployment` resources in labelled namespaces. It is the hard enforcement layer — it
independently verifies that a valid RFC approval exists in Redis before admitting any patch
that includes the `kubectl.kubernetes.io/restartedAt` annotation, regardless of whether
the patch came from the ArgoCD UI tab or direct `kubectl`.

**Webhook server:** Deployed as a separate `Deployment` + `ClusterIP` in the `argocd`
namespace. TLS certificate required (use cert-manager for automated rotation).

**Logic:**
1. On `PATCH Deployment` admission review, check if the patch includes
   `kubectl.kubernetes.io/restartedAt` in `spec.template.metadata.annotations`.
2. If not present → pass through (not a restart patch, not our concern).
3. If present → read annotation `rfc-validation/rfc-id` from the patched object.
4. If `rfc-id` annotation is absent → deny: `"RFC ID annotation required for restart"`.
5. If `rfc-id` is present → query Redis for `rfc:{id}:{namespace}`.
6. If cache entry exists and `approved: true` → admit.
7. If absent or `approved: false` → deny: `"RFC {id} is not approved or has expired"`.

**Scope:** Apply only to namespaces managed by ArgoCD. Use `namespaceSelector` to avoid
impacting system namespaces.

```yaml
namespaceSelector:
  matchLabels:
    argocd-rfc-enforcement: "enabled"
```

Label target namespaces:
```bash
kubectl label namespace <your-app-ns> argocd-rfc-enforcement=enabled
```

**Break-glass (emergency bypass):**
Deployments patched with annotation `rfc-validation/emergency-bypass: "true"` by a member
of the `cluster-admins` group are admitted. Every bypass fires a CloudWatch metric
`RFC/EmergencyBypass` and a Slack/PagerDuty alert. Bypasses are mandatory post-incident
review items.

---

### 6. On-prem ITSM connectivity

**Connectivity:** AWS Site-to-Site VPN or AWS Direct Connect between the EKS VPC and the
corporate data centre hosting the ITSM server.

**Networking requirements:**
- EKS node group NAT/EIP whitelisted on the corporate firewall for the ITSM host:port.
- Outbound from `rfc-validator` pods to on-prem CIDR only (enforced via `NetworkPolicy`).
- mTLS mutual authentication: ITSM server CA cert stored in ConfigMap
  `rfc-middleware-itsm-ca`; client cert/key in Secret `rfc-middleware-itsm-tls`.

**Secrets management:** Use AWS Secrets Manager + External Secrets Operator to sync the
ITSM API key and client cert into Kubernetes Secrets. Rotate on a 90-day schedule.

---

### 7. Audit log

Every RFC validation event is written by the middleware as a structured JSON log line:

```json
{
  "event":       "rfc_validation",
  "rfc_id":      "CHG0012345",
  "user":        "jane.doe@corp.com",
  "deployment":  "payments-api",
  "namespace":   "payments-prod",
  "result":      "approved",
  "reason":      "",
  "timestamp":   "2026-06-10T08:32:11Z"
}
```

FluentBit DaemonSet ships these logs to a dedicated CloudWatch log group
`/argocd/rfc-validation`. Set a 90-day retention policy. For long-term compliance archival,
configure a CloudWatch → S3 export.

---

## End-to-end request flow

```
1.  Engineer opens an ArgoCD application → resource tree shows Deployments.

2.  Engineer right-clicks a Deployment → three-dot menu appears.
    Menu shows: "restart" and "restart (RFC required)".

3.  Engineer selects "restart (RFC required)".
    ArgoCD executes the Lua action server-side:
      → writes rfc-validation/status: pending on the Deployment annotation.
      → writes rfc-validation/requested-at: <timestamp>.
    No restart happens yet.

4.  Engineer clicks the "RFC Validation" tab in the Deployment detail panel.
    React component (RFCValidationTab) reads resource props:
      → detects rfc-validation/status: pending
      → renders RFC number input form.

5.  Engineer enters RFC/change number → clicks "Validate".

6.  Browser POSTs to /extensions/rfc-validator/validate  (argocd.token cookie).
    argocd-server validates JWT + RBAC → proxies to rfc-validator ClusterIP.

7.  RFC middleware checks Redis cache (key: rfc:{id}:{ns}).
      CACHE HIT  → skip to step 10.
      CACHE MISS → continue.

8.  RFC middleware GETs /change/{id}/state from on-prem ITSM API (mTLS over VPN).

9.  Middleware validates:
      - state == approved
      - current time within change window
      - namespace matches RFC scope
    Writes result to Redis (TTL 60s).

10. Result returned to browser:
      APPROVED → tab shows approval badge (RFC title, approver, window).
                 "Confirm restart" button unlocks.
      REJECTED → tab shows rejection reason. Action remains blocked.

11. Engineer clicks "Confirm restart" (only available after approval).

12. React tab PATCHes the Deployment via K8s API:
      annotations:
        rfc-validation/status:                  approved
        rfc-validation/rfc-id:                  <RFC ID>
        rfc-validation/approver:                <ITSM approver name>
        kubectl.kubernetes.io/restartedAt:      <current UTC timestamp>

13. ValidatingWebhookConfiguration intercepts the PATCH.
    Webhook reads rfc-validation/rfc-id from the patched object.
    Queries Redis for rfc:{id}:{ns}:
      → approved: true  → admit → rolling restart begins.
      → missing/false   → deny  → patch rejected, restart blocked.

14. Kubernetes rolling restart completes (respects maxSurge / maxUnavailable).

15. RFC middleware writes audit log entry → FluentBit → CloudWatch.
```

---

## Annotation lifecycle on the Deployment

| Stage | Annotation | Value |
|---|---|---|
| After Lua action fires | `rfc-validation/status` | `pending` |
| After Lua action fires | `rfc-validation/requested-at` | ISO 8601 UTC timestamp |
| After RFC approved + restart confirmed | `rfc-validation/status` | `approved` |
| After RFC approved + restart confirmed | `rfc-validation/rfc-id` | e.g. `CHG0012345` |
| After RFC approved + restart confirmed | `rfc-validation/approver` | ITSM approver name |
| After RFC approved + restart confirmed | `kubectl.kubernetes.io/restartedAt` | ISO 8601 UTC timestamp |

---

## Kubernetes manifests — quick reference

| Manifest | Kind | Namespace | Notes |
|---|---|---|---|
| `extension-cm.yaml` | ConfigMap | argocd | React bundle; mounted by init container |
| `argocd-cm-patch.yaml` | ConfigMap (patch) | argocd | Lua action + proxy extension config |
| `deployment.yaml` | Deployment + Service | argocd | RFC middleware, 2 replicas, ClusterIP |
| `networkpolicy.yaml` | NetworkPolicy | argocd | Ingress: argocd-server + webhook; egress: Redis + on-prem CIDR |
| `redis.yaml` | Deployment + Service | argocd | In-cluster cache, no persistence |
| `secret.yaml` | Secret / ESO ref | argocd | ITSM API key + mTLS cert |
| `webhook-deployment.yaml` | Deployment + Service | argocd | Webhook server with TLS |
| `validatingwebhookconfig.yaml` | ValidatingWebhookConfiguration | cluster | Scoped to Deployment PATCH; namespace label selector |
| `cloudwatch-fluentbit.yaml` | DaemonSet + ConfigMap | argocd | Audit log shipping |

---

## Local development

### Run RFC middleware locally

```bash
cd rfc-middleware
export ITSM_BASE_URL=https://dev-instance.service-now.com
export ITSM_TLS_CERT_PATH=./certs/dev-client.crt
export ITSM_TLS_KEY_PATH=./certs/dev-client.key
export REDIS_ADDR=localhost:6379
go run ./src/main.go
```

### Run UI extension dev server

```bash
cd argocd-extension/ui
npm install
npm run dev       # watches src/, rebuilds extension.js on change
```

Point the proxy extension URL in your local `argocd-cm` to `http://localhost:8080` for
end-to-end local testing against a local ArgoCD instance.

### Test the Lua action locally

```bash
# Apply the argocd-cm patch to a local or dev ArgoCD instance
kubectl apply -f argocd-extension/manifests/argocd-cm-patch.yaml

# Restart argocd-server to pick up changes
kubectl rollout restart deployment argocd-server -n argocd

# Verify actions are registered on a Deployment
argocd app actions list <app-name> --kind Deployment
# Expected output includes:
#   Deployment   <name>   restart
#   Deployment   <name>   restart (RFC required)
```

### Run tests

```bash
# RFC middleware unit tests
cd rfc-middleware && go test ./...

# UI extension tests
cd argocd-extension/ui && npm test
```

---

## Deployment

### Prerequisites

- ArgoCD v2.7+ installed on the EKS cluster
- `--enable-proxy-extension` flag set on `argocd-server`
- AWS VPN or Direct Connect established to corporate DC
- ITSM API service account created with read-only access to Change records
- cert-manager installed (for admission webhook TLS)
- External Secrets Operator installed (optional, for Secrets Manager integration)

### Deploy order

```bash
# 1. Secrets first
kubectl apply -f rfc-middleware/manifests/secret.yaml

# 2. Redis
kubectl apply -f rfc-middleware/manifests/redis.yaml

# 3. RFC middleware + NetworkPolicy
kubectl apply -f rfc-middleware/manifests/deployment.yaml
kubectl apply -f rfc-middleware/manifests/networkpolicy.yaml

# 4. Admission webhook
kubectl apply -f admission-webhook/manifests/webhook-deployment.yaml
kubectl apply -f admission-webhook/manifests/validatingwebhookconfig.yaml

# 5. ArgoCD UI extension ConfigMap (React bundle)
kubectl apply -f argocd-extension/manifests/extension-cm.yaml

# 6. Patch argocd-cm with Lua action + proxy extension config
kubectl patch configmap argocd-cm -n argocd \
  --patch-file argocd-extension/manifests/argocd-cm-patch.yaml

# 7. Restart argocd-server to load the new extension.js and pick up argocd-cm changes
kubectl rollout restart deployment argocd-server -n argocd

# 8. Label namespaces to enable webhook enforcement
kubectl label namespace <app-namespace> argocd-rfc-enforcement=enabled

# 9. Audit log shipping
kubectl apply -f audit/manifests/cloudwatch-fluentbit.yaml
```

### Verify Lua actions are registered

```bash
argocd app actions list <app-name> --kind Deployment
# Expected:
#   KIND         NAME    ACTION
#   Deployment   <name>  restart
#   Deployment   <name>  restart (RFC required)
```

### Verify the proxy extension is active

```bash
kubectl logs deployment/argocd-server -n argocd | grep -i "proxy extension"
# Expected: "Proxy extension rfc-validator registered at /extensions/rfc-validator"
```

### Verify the webhook is active

```bash
kubectl get validatingwebhookconfigurations
kubectl describe validatingwebhookconfiguration argocd-rfc-webhook
```

---

## Operational runbook

### RFC validation tab shows "pending" but form is not appearing

1. Verify the extension.js is present on the argocd-server pod:
   `kubectl exec deployment/argocd-server -n argocd -- ls /tmp/extensions/rfc/`
2. Check browser console for JS errors from the extension bundle.
3. Confirm the init container ran successfully:
   `kubectl describe pod -l app.kubernetes.io/name=argocd-server -n argocd | grep -A5 init`

### RFC validation is failing for all requests

1. Check RFC middleware pod logs: `kubectl logs -l app=rfc-validator -n argocd`
2. Verify Redis is reachable:
   `kubectl exec -it <rfc-validator-pod> -n argocd -- redis-cli -h redis.argocd.svc.cluster.local ping`
3. Verify ITSM API is reachable from the pod:
   `kubectl exec -it <rfc-validator-pod> -n argocd -- curl -k https://<itsm-host>/healthcheck`
4. Check ITSM mTLS cert expiry.

### Admission webhook is blocking all Deployment patches

1. Check webhook pod logs: `kubectl logs -l app=argocd-rfc-webhook -n argocd`
2. Verify Redis cache entries exist: `kubectl exec -it <redis-pod> -n argocd -- redis-cli keys "rfc:*"`
3. If urgent: temporarily set `failurePolicy: Ignore` on the `ValidatingWebhookConfiguration`
   (this disables enforcement — use only as last resort and revert immediately).

### Lua action does not appear in the three-dot menu

1. Confirm `argocd-cm` contains `resource.customizations.actions.apps_Deployment`:
   `kubectl get configmap argocd-cm -n argocd -o yaml | grep -A30 'apps_Deployment'`
2. Verify `argocd-server` was restarted after the ConfigMap patch.
3. Check `argocd-server` logs for Lua parse errors:
   `kubectl logs deployment/argocd-server -n argocd | grep -i lua`

### Emergency break-glass

```bash
kubectl patch deployment <name> -n <namespace> --type=merge -p '{
  "metadata": {
    "annotations": {
      "rfc-validation/emergency-bypass": "true",
      "rfc-validation/bypass-reason": "P0 incident INC0098765",
      "kubectl.kubernetes.io/restartedAt": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'"
    }
  }
}'
```

This triggers an automated alert. File a post-incident review within 24 hours.

### Rotate ITSM credentials

```bash
# Update the secret (or update in Secrets Manager if using ESO)
kubectl create secret generic rfc-middleware-itsm-tls \
  --from-file=tls.crt=new-client.crt \
  --from-file=tls.key=new-client.key \
  --dry-run=client -o yaml | kubectl apply -f -

# Restart middleware to reload
kubectl rollout restart deployment rfc-validator -n argocd
```

---

## RBAC

Control which ArgoCD users can invoke the `restart (RFC required)` action via `argocd-rbac-cm`:

```yaml
# argocd-rbac-cm
policy.csv: |
  # Allow developers to invoke the RFC-gated restart on any app
  p, role:developer, applications, action/apps/Deployment/restart (RFC required), *, allow

  # Allow ops to invoke either restart action
  p, role:ops, applications, action/apps/Deployment/restart, *, allow
  p, role:ops, applications, action/apps/Deployment/restart (RFC required), *, allow

  # Deny restart actions for read-only users
  p, role:viewer, applications, action/*, *, deny
```

---

## Security considerations

- The Lua action writes only a `pending` annotation — it cannot read secrets, call external
  APIs, or perform the restart itself. The blast radius of a misconfigured Lua script is
  limited to annotation writes on the source Deployment.
- The RFC middleware never exposes ITSM credentials to the browser. The proxy extension
  ensures the browser only sees the middleware's response, never the ITSM API directly.
- `NetworkPolicy` ensures only `argocd-server` pods can reach the RFC middleware, and the
  middleware can only reach Redis and the on-prem CIDR.
- The admission webhook is defence-in-depth: engineers with direct `kubectl` access cannot
  patch `restartedAt` on Deployments in labelled namespaces without a cached, approved RFC.
- All bypass events are logged, alerted, and subject to mandatory review.
- ITSM API credentials are never logged. Log lines contain only `rfc_id`, `user`,
  `deployment`, `namespace`, and `result`.
