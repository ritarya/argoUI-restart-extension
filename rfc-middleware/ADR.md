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

### 1.1 Who this affects

Application developers own and operate services deployed on an AWS EKS cluster. Their
applications are managed through ArgoCD, which they use daily to monitor deployment health,
inspect resource state, and observe rolling updates. Developers are responsible for the
operational health of their own services — including responding to degraded pods, memory
leaks, or stuck processes that require a pod restart to recover.

Developers are not platform or infrastructure engineers. They do not have deep Kubernetes
expertise, and they should not need it for a routine operational task like restarting a pod.

### 1.2 The current developer experience problem

Today, when an application developer needs to restart a pod — for example, to clear a
memory leak, recover from a deadlock, or apply a configuration reload — the process is:

1. **Raise an RFC** in the organisation's ITSM system (ServiceNow / Jira Service
   Management) and wait for change approval, which can take minutes to hours depending on
   the change window and approver availability.
2. **Request jumphost access** or locate existing credentials to reach a bastion host that
   has network access to the EKS cluster.
3. **Log into the jumphost** via SSH or a VPN-gated remote session — often requiring MFA,
   a corporate VPN, and a session token that may have a short expiry.
4. **Locate the correct `kubeconfig`** context for the target cluster and namespace, which
   may differ across environments (dev, staging, production).
5. **Execute `kubectl` commands** to identify the correct pod name and trigger the restart:
   ```bash
   kubectl get pods -n <namespace>
   kubectl delete pod <pod-name> -n <namespace>
   # or
   kubectl rollout restart deployment/<deployment-name> -n <namespace>
   ```
6. **Monitor the restart** manually by polling `kubectl get pods` — with no visual feedback
   on rollout progress, health checks, or whether the new pod came up successfully.

This flow has several compounding problems from a developer experience (DX) standpoint:

- **High friction for a routine task** — a pod restart, which takes seconds to execute,
  requires navigating multiple systems (ITSM, VPN, jumphost, CLI) before a single command
  can be run. The total elapsed time from decision to action can exceed 15–30 minutes.
- **Context switching overhead** — developers must leave the ArgoCD UI (where they already
  have full visibility of their application's health) to perform the restart in a completely
  separate environment, then return to ArgoCD to observe the outcome.
- **Jumphost as a bottleneck** — jumphost sessions are shared infrastructure. Access may
  be limited by concurrent session caps, stale credentials, or network path issues that
  are unrelated to the developer's actual task.
- **Error-prone CLI operations** — developers unfamiliar with `kubectl` syntax may
  accidentally target the wrong namespace, delete the wrong pod, or omit the rolling
  restart flag — causing unintended disruption. There is no confirmation prompt or
  pre-flight validation in the raw CLI.
- **No RFC linkage at execution time** — the approved RFC exists in the ITSM system, but
  nothing connects it to the `kubectl` command that was actually run. If an incident occurs,
  auditors must manually correlate timestamps and usernames across three systems (ITSM,
  jumphost session log, Kubernetes audit log) to reconstruct what happened.
- **Knowledge barrier** — junior developers or developers new to the team may not know the
  correct jumphost hostname, the kubeconfig location, or the namespace naming convention,
  creating a dependency on senior engineers or the platform team for a task that should be
  self-serviceable.

### 1.3 The opportunity: ArgoCD UI as the operational interface

Developers already have the ArgoCD UI open when they observe a pod issue. ArgoCD displays
the exact Deployment, its replica set, the failing pod, its logs, and its health status —
all in one place. It is the natural and correct place for a developer to initiate a pod
restart, without any context switch.

The platform team is introducing a **pod restart capability directly in the ArgoCD UI** —
surfaced as a right-click action on Deployment resources — so that developers can restart
pods from the same interface where they diagnose the problem. This eliminates the jumphost
dependency entirely for this use case and reduces the time from decision to action from
15–30 minutes to under 60 seconds.

### 1.4 The compliance constraint that shapes the solution

Pod restarts in production namespaces are classified as Standard Changes under the
organisation's ITIL-aligned Change Management policy. Before any pod restart is performed
in production, an approved RFC must exist in the ITSM system (ServiceNow / Jira SM)
covering the change, including a valid change window that encompasses the time of execution.

This requirement does not go away because the restart is initiated from a UI instead of a
CLI. The platform team must therefore ensure that:

- The RFC is validated against the ITSM system **before** the restart is permitted.
- The validated RFC ID is **recorded alongside the restart event** for audit purposes.
- The control cannot be **bypassed** by developers who still have jumphost access, CI
  pipeline service accounts, or any other `kubectl`-capable client.

The problem is therefore: **how do we deliver a self-service pod restart experience in the
ArgoCD UI that eliminates the jumphost dependency for developers, while enforcing RFC
validation inline in the same workflow — and blocking bypass paths — without requiring a
custom ArgoCD build or coupling the extension to ArgoCD's upgrade cycle?**

---

## 2. Decision Drivers

- **Compliance** — Every pod restart in a production namespace must be traceable to an
  approved RFC in the ITSM system. This is a non-negotiable audit requirement.
- **Developer experience** — The solution must eliminate the jumphost dependency entirely.
  RFC validation must be inline in the ArgoCD UI, not an out-of-band step in a separate
  tool or terminal session.
- **Operational velocity** — The solution must not introduce excessive friction for
  legitimate, pre-approved changes. Validation should complete in seconds, not minutes.
- **ArgoCD upgrade independence** — The extension must have its own release cycle and must
  not require an ArgoCD version upgrade to ship a fix or new feature.
- **In-cluster deployment** — The ITSM server is on-premises. All middleware must run
  inside the EKS cluster and communicate with the ITSM API over a secured network boundary
  (VPN / Direct Connect).
- **Minimal footprint** — Avoid adding sidecars to `argocd-server` or modifying the ArgoCD
  image. Prefer configuration-driven approaches with the smallest possible set of new
  components.
- **Security** — ITSM credentials must never be exposed to the browser. mTLS must be
  enforced on the cross-boundary call to the on-prem ITSM API.
- **Auditability** — Every validation event (approved or rejected) must be durably logged
  with the RFC ID, user identity, target deployment, namespace, and timestamp.

---

## 3. Considered Options

### Option A — Lua Custom Action + `registerResourceExtension` React Tab *(Selected)*

A two-part in-cluster mechanism using ArgoCD's native extension APIs:

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
- `registerResourceExtension` places the RFC form exactly where the developer is already
  working — inside the Deployment detail panel — eliminating the jumphost dependency and
  any context switch to a separate tool or terminal session.
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
- The two-part mechanism (Lua action → annotation → React tab) is indirect. Developers must
  understand that clicking the action opens no modal; they must navigate to the "RFC
  Validation" tab manually. Onboarding documentation is required.
- Adds one new operational component: the RFC middleware Deployment, which requires
  monitoring, alerting, patching, and an on-call runbook.

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
- No UI feedback in ArgoCD — the developer has no in-context signal that validation is in
  progress or has failed, defeating the DX improvement goal.
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

**Selected option: Option A — Lua Custom Action + `registerResourceExtension` React Tab**

### 5.1 Rationale

Option A is the only approach that satisfies all decision drivers simultaneously:

- It uses ArgoCD's officially supported extension APIs, preserving upgrade independence and
  avoiding a custom build.
- The RFC validation form is surfaced inline in the ArgoCD resource detail panel — exactly
  where the developer already is when they diagnose a pod issue — meeting the DX goal of
  eliminating the jumphost dependency without any context switch.
- The RFC middleware is fully decoupled from ArgoCD's lifecycle. It can be updated,
  scaled, and released independently via its own Docker image and rollout.
- The proxy extension ensures ITSM credentials are never exposed to the browser — they
  remain inside the cluster boundary at all times.
- Every component (Lua action, React bundle, RFC middleware) is independently deployable
  via GitOps, with no ArgoCD upgrade required to ship changes.

Options B and C were rejected primarily because they provide no UI-integrated feedback loop
— developers would receive no contextual guidance on RFC requirements within the ArgoCD
interface, which directly undermines the DX improvement goal. Option D was rejected due to
the unsustainable maintenance burden of a custom ArgoCD fork. Option E was rejected because
it inverts the operational model in a way that is incompatible with incident response
requirements and requires a security boundary change.

### 5.2 Consequences

#### Positive

- **Jumphost dependency eliminated** — developers can restart pods directly from the ArgoCD
  UI with no SSH session, VPN tunnel, or `kubectl` knowledge required. The time from
  decision to action drops from 15–30 minutes to under 60 seconds.
- **RFC validation inline** — the RFC input and approval status are surfaced inside the
  same Deployment detail panel where the developer is already working. No context switch,
  no separate tool, no separate browser tab.
- **Compliance enforced automatically** — pod restarts cannot proceed through the UI without
  a valid, approved RFC, closing the current audit gap for UI-initiated restarts.
- **Full audit trail** — every validation event is structured-logged (RFC ID, user,
  deployment, namespace, result, timestamp) and shipped to CloudWatch, enabling compliance
  reporting without manual correlation across systems.
- **ArgoCD upgrade independence** — the extension has its own release pipeline. A new
  extension version requires only a ConfigMap update and `argocd-server` rolling restart,
  not an ArgoCD version upgrade.
- **Credential security** — ITSM API credentials (mTLS cert + API key) are stored in
  Kubernetes Secrets (or AWS Secrets Manager via ESO) and never exposed to the browser or
  included in logs.
- **RBAC-aligned** — access to the `restart (RFC required)` action is controlled via
  `argocd-rbac-cm`, consistent with how all other ArgoCD permissions are managed.
- **Minimal new components** — only one net-new runtime component is introduced: the RFC
  middleware Deployment. Everything else is configuration (argocd-cm, ConfigMap bundle).

#### Negative

- **`argocd-server` restart required for extension updates** — any change to `extension.js`
  or `argocd-cm` (Lua action, proxy config) requires a rolling restart of `argocd-server`,
  causing a brief UI disruption for active users.
- **Built-in restart action must be explicitly re-declared** — defining
  `resource.customizations.actions.apps_Deployment` in `argocd-cm` removes the built-in
  restart action unless it is re-declared in the same block. A misconfigured `argocd-cm`
  patch could silently remove the standard restart option for all developers.
- **Proxy extension is alpha** — the `--enable-proxy-extension` flag is an alpha feature
  in ArgoCD v2.7. It must be monitored across ArgoCD upgrades for breaking changes or
  graduation to stable.
- **Indirect UX flow** — the two-part mechanism (click action → annotation written →
  navigate to RFC Validation tab) is less intuitive than a native blocking modal. Developers
  require onboarding documentation and may initially find the two-step flow confusing.
- **VPN / Direct Connect dependency** — the RFC middleware's connection to the on-prem ITSM
  API introduces a network dependency. If the VPN or Direct Connect link is degraded, ITSM
  validation will fail and the restart will be blocked. A break-glass procedure must exist
  for incident response scenarios where the ITSM API is unreachable.
- **RFC middleware is a new operational dependency** — the middleware Deployment requires
  monitoring, alerting, patching, and an on-call runbook. An outage of the middleware
  blocks all RFC-gated restarts from the UI.

---

## 6. Implementation Notes

### Components deployed

| Component | Kind | Namespace | Release cycle |
|---|---|---|---|
| React resource tab (`extension.js`) | ConfigMap + init container | argocd | Independent — ConfigMap update + pod restart |
| Lua custom action | argocd-cm patch | argocd | Independent — ConfigMap update + pod restart |
| Proxy extension config | argocd-cm patch | argocd | Independent — ConfigMap update + pod restart |
| RFC middleware | Deployment + ClusterIP | argocd | Independent — own Docker image + rollout |
| FluentBit audit shipper | DaemonSet | argocd | Independent — version pin in manifest |

### Key configuration constraints

- ArgoCD v2.7 or later required for proxy extension support.
- `--enable-proxy-extension` must be set on `argocd-server`.
- The built-in `restart` action must be explicitly re-declared in
  `resource.customizations.actions.apps_Deployment` alongside `restart (RFC required)`.
- RFC middleware must only be reachable from `argocd-server` pods via `NetworkPolicy`
  (ingress) and may only reach the on-prem ITSM CIDR range (egress).

### Open decisions

| Decision | Options | Owner | Due |
|---|---|---|---|
| VPN vs Direct Connect for ITSM connectivity | Site-to-Site VPN · Direct Connect | Networking team | Before implementation |
| ITSM scope matching granularity | Per-cluster · per-namespace · per-app label | Change Management | Before implementation |
| ESO vs manual Secret for ITSM credentials | External Secrets Operator · manual `kubectl create secret` | Security team | Before implementation |
| Middleware failure behaviour | Fail closed (block restart) · fail open with alert | SRE + Security | Before production go-live |

---

## 7. References

- [ArgoCD UI Extensions documentation](https://argo-cd.readthedocs.io/en/stable/developer-guide/extensions/ui-extensions/)
- [ArgoCD Proxy Extensions documentation](https://argo-cd.readthedocs.io/en/stable/developer-guide/extensions/proxy-extensions/)
- [ArgoCD Resource Customizations — Custom Actions](https://argo-cd.readthedocs.io/en/stable/operator-manual/resource_actions/)
- [argocd-extension-installer](https://github.com/argoproj-labs/argocd-extension-installer)
- [ServiceNow Change Management REST API](https://developer.servicenow.com/dev.do#!/reference/api/tokyo/rest/change-management-api)
- CLAUDE.md — Full architecture guide for this project
