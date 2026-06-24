# ADR-001: ArgoCD UI Extension for ITSM RFC Validation Before Pod Restarts

| Field        | Detail                                      |
|--------------|---------------------------------------------|
| **ADR ID**   | ADR-001                                     |
| **Status**   | Accepted                                    |
| **Date**     | 2026-06-24                                  |
| **Authors**  | Platform Engineering Team                   |
| **Reviewers**| SRE, Security, Change Management            |

---

## 1. Context and Problem Statement

The platform engineering team operates multiple production workloads on an AWS EKS cluster
managed via ArgoCD. Engineers have direct access to the ArgoCD UI to perform operational
tasks including pod restarts on running Deployments. Pod restarts in production environments
are classified as changes under the organisation's ITSM Change Management process and
require a pre-approved Request for Change (RFC) before execution.

Currently, there is no enforcement mechanism between the ArgoCD UI and the on-premises ITSM
system (ServiceNow / Jira Service Management). Engineers can trigger pod restarts from the
ArgoCD UI or directly via `kubectl` without referencing or validating an approved RFC. This
creates the following risks:

- **Compliance violations** — production changes executed outside the approved change window
  or without a valid RFC breach the organisation's ITIL-aligned change management policy.
- **Audit gaps** — there is no automated record linking a pod restart event to an RFC,
  making post-incident reviews and compliance audits manual and error-prone.
- **Dual enforcement gap** — even if a UI-level control is added, engineers with `kubectl`
  access can bypass it entirely.
- **Operational risk** — uncoordinated restarts during peak hours or outside change windows
  increase the risk of unplanned service disruption.

The problem is therefore: **how do we enforce RFC validation as a prerequisite for pod
restarts initiated from the ArgoCD UI, while also blocking direct `kubectl` bypass, without
disrupting the existing ArgoCD release cycle or requiring a custom ArgoCD build?**

---

## 2. Decision Drivers

- **Compliance** — Every pod restart in a production namespace must be traceable to an
  approved RFC in the ITSM system. This is a non-negotiable audit requirement.
- **Enforcement depth** — The control must not be bypassable via direct `kubectl` access.
  UI-only gates are insufficient for engineers with cluster credentials.
- **Operational velocity** — The solution must not introduce excessive friction for
  legitimate, pre-approved changes. Validation should complete in seconds.
- **ArgoCD upgrade independence** — The extension must have its own release cycle and must
  not require an ArgoCD version upgrade to ship a fix or new feature.
- **In-cluster deployment** — The ITSM server is on-premises. All middleware must run
  inside the EKS cluster and communicate with the ITSM API over a secured network boundary
  (VPN / Direct Connect).
- **Minimal footprint** — Avoid adding sidecars to `argocd-server` or modifying the ArgoCD
  image. Prefer configuration-driven approaches.
- **Security** — ITSM credentials must never be exposed to the browser. mTLS must be
  enforced on the cross-boundary call to the on-prem ITSM API.
- **Auditability** — Every validation event (approved or rejected) must be durably logged
  with the RFC ID, user identity, target deployment, namespace, and timestamp.

---

## 3. Considered Options

### Option A — Lua Custom Action + `registerResourceExtension` React Tab *(Selected)*

A two-part in-cluster mechanism using ArgoCD's native extension APIs with a hard backstop:

1. A **Lua custom action** (`restart (RFC required)`) defined in `argocd-cm` appears in
   the three-dot action menu on every Deployment resource. When clicked it writes a
   `rfc-validation/status: pending` annotation on the Deployment — nothing more.
2. A **React resource tab** registered via `registerResourceExtension` for `apps/Deployment`
   detects the pending annotation and renders the RFC input form inline in the resource
   detail panel. It calls the RFC middleware via ArgoCD's proxy extension, validates the RFC
   against the on-prem ITSM API, and on approval patches the Deployment with
   `restartedAt` to trigger the rolling restart.

### Option B — ArgoCD Notification / Webhook Trigger + External Approval Gate

Use ArgoCD's notification system to emit a webhook event when a restart action is triggered.
An external approval service receives the event, calls the ITSM API, and either allows or
blocks the action by patching the resource.

### Option C — OPA / Gatekeeper Policy Engine as the Sole Gate

Deploy Open Policy Agent (OPA) Gatekeeper with a `ConstraintTemplate` that intercepts
Deployment `PATCH` operations and calls an external ITSM validation service via a Gatekeeper
external data provider before admitting the request. No ArgoCD UI changes; enforcement is
purely at the admission layer.

### Option D — Modify ArgoCD Server Source Code (Custom Build)

Fork ArgoCD and add RFC validation logic directly into the `argocd-server` binary — e.g.
intercept the existing restart action handler and inject an ITSM API call before executing
the Kubernetes patch. Ship a custom ArgoCD image to the cluster.

### Option E — ServiceNow / ITSM Webhook Calling ArgoCD API (Reverse Flow)

Reverse the flow: engineers raise an RFC in ITSM and, upon approval, the ITSM system calls
the ArgoCD API to trigger the pod restart automatically. The ArgoCD UI is used only for
read-only observation.

---

## 4. Pros and Cons of the Options

### Option A — Lua Custom Action + React Tab *(Selected)*

**Pros**

- Uses only ArgoCD's officially supported, stable extension APIs — no forking, no custom
  builds, no changes to the ArgoCD image.
- The UI extension has a fully independent release cycle. Updating the React bundle requires
  only a ConfigMap update and an `argocd-server` rolling restart — no ArgoCD upgrade needed.
- The Lua action script in `argocd-cm` is version-controlled, diff-able, and deployable
  via standard GitOps. Changes take effect after a ConfigMap update and pod restart.
- `registerResourceExtension` places the RFC form exactly where the engineer is already
  working — inside the Deployment detail panel — with no context switch to a separate tab
  or external tool.
- The RFC middleware is a plain HTTP service with its own `Deployment` and release pipeline,
  fully decoupled from ArgoCD's lifecycle.
- mTLS between the middleware and the ITSM API ensures credentials are never exposed to the
  browser or logged.
- RBAC on the `restart (RFC required)` action is configurable via `argocd-rbac-cm` using
  standard ArgoCD policy syntax.
- Every validation event is structured-logged and shipped to CloudWatch for audit and
  compliance reporting.

**Cons**

- Defining `resource.customizations.actions.apps_Deployment` in `argocd-cm` replaces the
  built-in restart action entirely — the stock restart button disappears unless it is
  explicitly re-declared in the same block. This is a one-time configuration risk that must
  be handled carefully.
- The proxy extension feature (`--enable-proxy-extension`) is alpha in ArgoCD v2.7 and
  disabled by default. It must be explicitly opted into and monitored for breaking changes
  across ArgoCD upgrades.
- An `argocd-server` rolling restart is required to pick up changes to `extension.js` or
  `argocd-cm`. This causes a brief UI disruption (seconds) for active users.
- The two-part mechanism (Lua action → annotation → React tab) is indirect. Engineers must
  understand that clicking the action opens no modal; they must navigate to the "RFC
  Validation" tab manually.

---

### Option B — ArgoCD Notification + External Approval Gate

**Pros**

- No changes to `argocd-server` configuration or UI.
- The approval gate is a standalone service with a clean interface boundary.
- Can be reused for other ArgoCD events beyond pod restarts.

**Cons**

- ArgoCD's notification system is designed for observability (Slack, email), not for
  blocking or gating actions. There is no native mechanism to pause an action and await
  an external approval before proceeding.
- Requires a custom webhook receiver service and significant glue code to correlate
  notification events back to ArgoCD resource actions.
- No UI feedback in ArgoCD — the engineer has no in-context signal that validation is in
  progress or has failed.
- Does not solve the `kubectl` bypass problem. The admission webhook would still be needed,
  making this option additive rather than a standalone solution.
- High latency: notification → external service → ITSM API → response → ArgoCD API is a
  multi-hop async flow with no guaranteed completion time.

---

### Option C — OPA / Gatekeeper as the Sole Gate

**Pros**

- Enforcement is purely at the Kubernetes API layer — consistent regardless of how the
  restart is triggered (ArgoCD UI, `kubectl`, CI pipelines, operators).
- OPA Gatekeeper is a CNCF graduated project with strong community support and
  well-understood operational patterns.
- `ConstraintTemplate` policies are declarative, version-controlled, and testable with
  `conftest` or `opa eval`.
- No changes to ArgoCD or its configuration.

**Cons**

- OPA Gatekeeper external data providers (for calling the ITSM API) are a complex and
  relatively immature feature. The sync model (pull-based, cached) may not reflect
  real-time ITSM state reliably.
- Provides no UI feedback in ArgoCD. When a restart is blocked, the engineer sees only a
  generic Kubernetes admission error — not a contextual message explaining which RFC is
  required or why it was rejected.
- Does not surface the RFC input form in the ArgoCD UI. Engineers must obtain and enter
  RFC metadata out-of-band, with no guided flow.
- Introduces OPA Gatekeeper as a new platform dependency if it is not already deployed.
- Admission webhooks from Gatekeeper add latency to all matching API calls, not just
  restart-related ones.
- Does not address the engineer experience requirement — RFC validation should be integrated
  into the ArgoCD workflow, not hidden in a Kubernetes admission error message.

---

### Option D — Custom ArgoCD Build

**Pros**

- Complete control over the validation flow — the RFC check can be injected precisely at
  the point where the restart action handler executes, with full access to ArgoCD's
  internal context (user identity, application, resource).
- No indirect annotation-based signalling needed between components.
- The UI can show a native modal (not a resource tab) because the code has direct access to
  ArgoCD's React internals.

**Cons**

- Every ArgoCD upstream release requires a rebase and rebuild of the custom fork. This
  creates a permanent, high-cost maintenance burden on the platform team.
- Security patches from the ArgoCD project cannot be applied by simply upgrading the Helm
  chart — they require a fork rebase, build, and re-deployment cycle.
- Divergence from upstream increases over time, making future alignment progressively harder.
- Requires maintaining a private container registry, build pipeline, and image scanning
  process for the custom ArgoCD image.
- Not supportable by the ArgoCD community if issues arise — all bug reports and support
  requests must be handled internally.
- Violates the decision driver of ArgoCD upgrade independence.

---

### Option E — ITSM-Initiated Restart (Reverse Flow)

**Pros**

- RFC approval is the trigger — a restart can only happen if an RFC was formally approved
  through the ITSM process. There is no possibility of bypassing the ITSM system.
- Removes human error from the ArgoCD UI — engineers do not need to remember to validate
  an RFC before acting.
- Creates a clean audit trail in the ITSM system itself.

**Cons**

- Fundamentally changes the operational model — engineers lose the ability to self-serve
  restart operations from the ArgoCD UI, which is a significant workflow regression for
  incident response and routine operations.
- Requires the on-prem ITSM system to have outbound network access to the ArgoCD API on
  EKS — a significant and likely unacceptable security boundary change.
- ITSM systems are typically not designed for real-time infrastructure automation. Approval
  workflows can take minutes to hours, making this unsuitable for incident response
  scenarios requiring immediate pod restarts.
- Does not solve the `kubectl` bypass problem.
- Places operational dependency on the ITSM system's availability — if ITSM is down, no
  restarts can be performed even in an emergency.

---

## 5. Decision Outcome

**Selected option: Option A — Lua Custom Action + `registerResourceExtension` React Tab

### 5.1 Rationale

Option A is the only approach that satisfies all decision drivers simultaneously:

- It uses ArgoCD's officially supported extension APIs, preserving upgrade independence and
  avoiding a custom build.
- The RFC validation form is surfaced inline in the ArgoCD resource detail panel — exactly
  where the engineer is already working — meeting the operational velocity requirement
  without external tool context-switching.
- The RFC middleware is fully decoupled from ArgoCD's lifecycle. It can be updated,
  scaled, and released independently.
- The proxy extension ensures ITSM credentials are never exposed to the browser — they
  remain inside the cluster boundary.
- Every component (Lua action, React bundle, RFC middleware) is independently
  deployable via GitOps, with no ArgoCD upgrade required to ship changes.

Options B and C were rejected primarily because they provide no UI-integrated feedback loop
— engineers would receive no contextual guidance on RFC requirements within the ArgoCD
interface. Option D was rejected due to the unsustainable maintenance burden of a custom
ArgoCD fork. Option E was rejected because it inverts the operational model in a way that
is incompatible with incident response requirements and requires a security boundary change.

### 5.2 Consequences

#### Positive

- **Compliance enforced automatically** — pod restarts in labelled production namespaces
  cannot proceed without a valid, approved RFC, regardless of how the restart is initiated.
- **Full audit trail** — every validation event is structured-logged (RFC ID, user,
  deployment, namespace, result, timestamp) and shipped to CloudWatch, satisfying audit and
  compliance reporting requirements without manual correlation.
- **ArgoCD upgrade independence** — the extension has its own release pipeline. A new
  extension version requires only a ConfigMap update and `argocd-server` rolling restart,
  not an ArgoCD upgrade.
- **Credential security** — ITSM API credentials (mTLS cert + API key) are stored in
  Kubernetes Secrets (or AWS Secrets Manager via ESO) and never exposed to the browser or
  included in logs.
- **RBAC-aligned** — access to the `restart (RFC required)` action is controlled via
  `argocd-rbac-cm`, consistent with how all other ArgoCD permissions are managed.
- **Break-glass available** — a documented emergency bypass mechanism exists for P0
  incidents, with mandatory automated alerting and post-incident review requirements.

#### Negative

- **Operational surface area increases** — three new components are added to the platform:
  RFC middleware Deployment. It requires monitoring, alerting, patching, and on-call runbooks.
- **`argocd-server` restart required for extension updates** — any change to `extension.js`
  or `argocd-cm` (Lua action, proxy config) requires a rolling restart of `argocd-server`,
  causing a brief UI disruption for active users.
- **Built-in restart action must be explicitly re-declared** — defining
  `resource.customizations.actions.apps_Deployment` in `argocd-cm` removes the built-in
  restart action unless it is re-declared in the same block. A misconfigured `argocd-cm`
  patch could silently remove the standard restart option for all engineers.
- **Proxy extension is alpha** — the `--enable-proxy-extension` flag is an alpha feature
  in ArgoCD v2.7. It must be monitored across ArgoCD upgrades for breaking changes or
  graduation to stable.
- **Indirect UX flow** — the two-part mechanism (click action → annotation written →
  navigate to RFC Validation tab) is less intuitive than a native modal. Engineers require
  onboarding documentation and may initially find the flow confusing.
- **VPN / Direct Connect dependency** — the RFC middleware's connection to the on-prem ITSM
  API introduces a network dependency. If the VPN or Direct Connect link is degraded, ITSM
  validation will fail. The middleware fails closed (does not fail open), which could block
  legitimate restarts during a network outage without a break-glass procedure.

---

## 6. Implementation Notes

### Components deployed

| Component | Kind | Namespace | Release cycle |
|---|---|---|---|
| React resource tab (`extension.js`) | ConfigMap + init container | argocd | Independent — ConfigMap update + pod restart |
| Lua custom action | argocd-cm patch | argocd | Independent — ConfigMap update + pod restart |
| Proxy extension config | argocd-cm patch | argocd | Independent — ConfigMap update + pod restart |
| RFC middleware | Deployment + ClusterIP | argocd | Independent — own Docker image + rollout |

### Key configuration constraints

- ArgoCD v2.7 or later required for proxy extension support.
- `--enable-proxy-extension` must be set on `argocd-server`.
- The built-in `restart` action must be explicitly re-declared in
  `resource.customizations.actions.apps_Deployment` alongside `restart (RFC required)`.

### Open decisions

| Decision | Options | Owner | Due |
|---|---|---|---|
| VPN vs Direct Connect for ITSM connectivity | Site-to-Site VPN · Direct Connect | Networking team | Before implementation |
| `failurePolicy` per environment | `Fail` (strict) · `Ignore` (permissive) | SRE + Security | Before go-live |
| ITSM scope matching granularity | Per-cluster · per-namespace · per-app label | Change Management | Before implementation |
| ESO vs manual Secret for ITSM credentials | External Secrets Operator · manual `kubectl create secret` | Security team | Before implementation |

---

## 7. References

- [ArgoCD UI Extensions documentation](https://argo-cd.readthedocs.io/en/stable/developer-guide/extensions/ui-extensions/)
- [ArgoCD Proxy Extensions documentation](https://argo-cd.readthedocs.io/en/stable/developer-guide/extensions/proxy-extensions/)
- [ArgoCD Resource Customizations — Custom Actions](https://argo-cd.readthedocs.io/en/stable/operator-manual/resource_actions/)
- [argocd-extension-installer](https://github.com/argoproj-labs/argocd-extension-installer)
- [ServiceNow Change Management REST API](https://developer.servicenow.com/dev.do#!/reference/api/tokyo/rest/change-management-api)