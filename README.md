# ArgoCD ITSM RFC Validation Extension

> **Status: Experimental** — This project is under active development and not yet recommended for production use without thorough testing in your environment.

## Overview

This project extends the ArgoCD UI on an AWS EKS cluster to enforce ITSM Change Management RFC validation before allowing pod restarts from the ArgoCD interface.

An engineer right-clicks a Deployment resource in the ArgoCD application tree and selects **Restart (RFC required)** from the three-dot action menu. This triggers a two-part flow:

1. A Lua custom action writes a `pending` annotation on the Deployment.
2. A React resource tab extension detects that annotation, surfaces the RFC validation form inline, and — only after the RFC is validated — applies the `restartedAt` patch to trigger the rolling restart.

## Installation

### Prerequisites

- ArgoCD v3.x installed on the EKS cluster with `--enable-proxy-extension` flag set on `argocd-server`
- `kubectl` configured against your cluster
- `docker` logged into your container registry
- `go` 1.22+
- `node` 18+ and `npm`
- AWS VPN or Direct Connect to the corporate data centre hosting the ITSM server
- Okta application credentials (client ID + secret) with access to the ITSM service account

### 1. Fill in secrets

Edit `rfc-middleware/manifests/secret.yaml` with your base64-encoded values:

```bash
echo -n 'your-okta-client-id'     | base64
echo -n 'your-okta-client-secret' | base64
echo -n 'your-itsm-username'      | base64
echo -n 'your-itsm-password'      | base64
echo -n 'https://your-instance.service-now.com' | base64
echo -n 'https://your-okta-domain/oauth2/v1/token' | base64
```

### 2. Deploy everything

```bash
make all REGISTRY=<your-registry> TAG=<version>
```

This runs in order: build images → push to registry → deploy secrets → Redis → RFC middleware → UI extension ConfigMap → patch argocd-cm → patch argocd-server (init container) → restart argocd-server.

### 3. Verify

```bash
make verify
```

---

## Usage

1. Open an ArgoCD application and navigate to the resource tree.
2. Right-click a **Deployment** resource → three-dot menu → select **Restart (RFC required)**.
3. Click the **RFC Validation** tab in the Deployment detail panel.
4. Enter your RFC/change number (e.g. `CHG0012345`) and click **Validate**.
5. If the RFC is in `Implement` state and the current time falls within the change window, the **Confirm restart** button unlocks.
6. Click **Confirm restart** — the rolling restart begins immediately.

### RFC validation rules

| Check | Required value |
|---|---|
| `change_state` | `Implement` |
| Current time | Within `change_start_date` – `change_end_date` |

---

## Local development

### Run the RFC middleware locally

```bash
cd rfc-middleware
export ITSM_BASE_URL=https://dev-instance.service-now.com
export ITSM_TOKEN_URL=https://your-okta-domain/oauth2/v1/token
export ITSM_CLIENT_ID=your-client-id
export ITSM_CLIENT_SECRET=your-client-secret
export ITSM_USERNAME=your-itsm-username
export ITSM_PASSWORD=your-itsm-password
export REDIS_ADDR=localhost:6379
go run ./src/
```

### Run the UI extension dev server

```bash
cd argocd-extension/ui
npm install
npm run dev   # watches src/ and rebuilds extension.js on change
```

### Deploy to minikube

```bash
# Start minikube and install ArgoCD, then:
make minikube-deploy REGISTRY=local TAG=dev
```

This builds the Docker image directly inside minikube's daemon (no registry push needed) and deploys all components.

### Run tests

```bash
make test
```

Runs Go unit tests for the RFC middleware and Jest tests for the UI extension.

### Available make targets

```bash
make help
```
