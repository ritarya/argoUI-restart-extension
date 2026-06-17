# ── Configuration ────────────────────────────────────────────────────────────
REGISTRY        ?= your-registry
TAG             ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo latest)
NAMESPACE       ?= argocd

RFC_IMAGE       := $(REGISTRY)/rfc-validator:$(TAG)

KUBECTL         := kubectl
DOCKER          := docker

.DEFAULT_GOAL := help

# ── All-in-one ────────────────────────────────────────────────────────────────
.PHONY: all
all: build push deploy ## Build image + UI, push to registry, and deploy everything
	@echo ""
	@echo "All done. Run 'make verify' to confirm all components are healthy."

# ── Help ─────────────────────────────────────────────────────────────────────
.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-28s\033[0m %s\n", $$1, $$2}' | sort
	@echo ""
	@echo "  Variables (override with VAR=value):"
	@echo "    REGISTRY=$(REGISTRY)"
	@echo "    TAG=$(TAG)"
	@echo "    NAMESPACE=$(NAMESPACE)"

# ── Build ─────────────────────────────────────────────────────────────────────
.PHONY: build build-middleware build-ui

build: build-middleware build-ui ## Build the middleware image and UI bundle

build-middleware: ## Build the RFC middleware Docker image
	$(DOCKER) build -t $(RFC_IMAGE) rfc-middleware/

build-ui: ## Build the React UI extension bundle (outputs to argocd-extension/ui/dist/)
	cd argocd-extension/ui && npm install && npm run build

# ── Push ──────────────────────────────────────────────────────────────────────
.PHONY: push

push: build-middleware ## Build and push the RFC middleware image to the registry
	$(DOCKER) push $(RFC_IMAGE)

# ── Test ──────────────────────────────────────────────────────────────────────
.PHONY: test test-middleware test-ui lint type-check

test: test-middleware test-ui ## Run all tests

test-middleware: ## Run RFC middleware unit tests
	cd rfc-middleware && go test ./...

test-ui: ## Run UI extension tests
	cd argocd-extension/ui && npm test

lint: ## Lint the UI extension source
	cd argocd-extension/ui && npm run lint

type-check: ## TypeScript type-check the UI extension
	cd argocd-extension/ui && npm run type-check

# ── Deploy ────────────────────────────────────────────────────────────────────
.PHONY: deploy deploy-secrets deploy-redis deploy-middleware \
        deploy-ui patch-argocd-cm restart-argocd-server

deploy: ## Full deploy in dependency order (runs all deploy-* targets in sequence)
	$(MAKE) deploy-secrets
	$(MAKE) deploy-redis
	$(MAKE) deploy-middleware
	$(MAKE) deploy-ui
	$(MAKE) patch-argocd-cm
	$(MAKE) restart-argocd-server
	@echo ""
	@echo "Deployment complete. Run 'make verify' to check component health."

deploy-secrets: ## Apply ITSM credentials secret (or ESO ref)
	$(KUBECTL) apply -f rfc-middleware/manifests/secret.yaml

deploy-redis: ## Deploy in-cluster Redis cache
	$(KUBECTL) apply -f rfc-middleware/manifests/redis.yaml

deploy-middleware: ## Deploy RFC middleware + NetworkPolicy
	@echo "Deploying RFC middleware with image $(RFC_IMAGE)..."
	$(KUBECTL) apply -f rfc-middleware/manifests/deployment.yaml
	$(KUBECTL) apply -f rfc-middleware/manifests/networkpolicy.yaml
	@echo "Updating middleware image to $(RFC_IMAGE)..."
	$(KUBECTL) set image deployment/rfc-validator \
		rfc-validator=$(RFC_IMAGE) -n $(NAMESPACE)

deploy-ui: build-ui ## Build the React bundle and load it into the argocd-rfc-extension ConfigMap
	$(KUBECTL) create configmap argocd-rfc-extension \
		-n $(NAMESPACE) \
		--from-file=extension.js=argocd-extension/ui/dist/extension.js \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) label configmap argocd-rfc-extension \
		-n $(NAMESPACE) app.kubernetes.io/name=argocd-rfc-extension --overwrite

patch-argocd-cm: ## Patch argocd-cm with Lua action + proxy extension config
	$(KUBECTL) patch configmap argocd-cm -n $(NAMESPACE) \
		--patch-file argocd-extension/manifests/argocd-cm-patch.yaml
	$(KUBECTL) apply -f argocd-extension/manifests/argocd-rbac-cm-patch.yaml

restart-argocd-server: ## Rollout-restart argocd-server to load extension.js and argocd-cm changes
	$(KUBECTL) rollout restart deployment argocd-server -n $(NAMESPACE)
	$(KUBECTL) rollout status deployment argocd-server -n $(NAMESPACE) --timeout=120s

# ── Update image only (no manifest re-apply) ──────────────────────────────────
.PHONY: update

update: push ## Push a new middleware image and roll it out
	$(KUBECTL) set image deployment/rfc-validator \
		rfc-validator=$(RFC_IMAGE) -n $(NAMESPACE)
	$(KUBECTL) rollout status deployment/rfc-validator -n $(NAMESPACE) --timeout=120s

# ── Verify ────────────────────────────────────────────────────────────────────
.PHONY: verify verify-lua verify-proxy verify-middleware

verify: verify-lua verify-proxy verify-middleware ## Run all post-deploy verification checks

verify-lua: ## Check Lua actions are registered (requires ARGOCD_APP=<app-name>)
	@if [ -z "$(ARGOCD_APP)" ]; then \
		echo "Skipping Lua action check (set ARGOCD_APP=<app-name> to run)."; \
	else \
		argocd app actions list $(ARGOCD_APP) --kind Deployment; \
	fi

verify-proxy: ## Check the proxy extension is registered in argocd-server logs
	$(KUBECTL) logs deployment/argocd-server -n $(NAMESPACE) --tail=100 \
		| grep -i "proxy extension" || echo "No proxy extension log line found yet — try again after restart."

verify-middleware: ## Check RFC middleware pod readiness and recent logs
	$(KUBECTL) rollout status deployment/rfc-validator -n $(NAMESPACE)
	$(KUBECTL) logs -l app=rfc-validator -n $(NAMESPACE) --tail=20

# ── Teardown ──────────────────────────────────────────────────────────────────
.PHONY: undeploy

undeploy: ## Remove all deployed resources (does NOT revert the argocd-cm patch)
	-$(KUBECTL) delete -f argocd-extension/manifests/extension-cm.yaml
	-$(KUBECTL) delete -f rfc-middleware/manifests/networkpolicy.yaml
	-$(KUBECTL) delete -f rfc-middleware/manifests/deployment.yaml
	-$(KUBECTL) delete -f rfc-middleware/manifests/redis.yaml
	-$(KUBECTL) delete -f rfc-middleware/manifests/secret.yaml
	@echo ""
	@echo "Resources removed. The argocd-cm patch (Lua action + proxy config) was NOT"
	@echo "reverted — patch argocd-cm manually and restart argocd-server if needed."

# ── Minikube ──────────────────────────────────────────────────────────────────
# Builds images directly inside minikube's Docker daemon (no registry or push needed).
# Usage: make minikube-deploy [TAG=dev]
MINIKUBE_TAG ?= dev

.PHONY: minikube-deploy minikube-build minikube-load

minikube-deploy: minikube-build deploy ## Build into minikube and deploy everything (no registry needed)
	@echo ""
	@echo "Minikube deploy complete. Run 'make verify' to confirm component health."

minikube-build: ## Build the middleware Docker image directly inside minikube's daemon
	@echo "Switching docker context to minikube..."
	$(eval export DOCKER_TLS_VERIFY=$(shell minikube docker-env | grep DOCKER_TLS_VERIFY | cut -d= -f2 | tr -d '"'))
	$(eval export DOCKER_HOST=$(shell minikube docker-env | grep DOCKER_HOST | cut -d= -f2 | tr -d '"'))
	$(eval export DOCKER_CERT_PATH=$(shell minikube docker-env | grep DOCKER_CERT_PATH | cut -d= -f2 | tr -d '"'))
	$(eval export MINIKUBE_ACTIVE_DOCKERD=$(shell minikube docker-env | grep MINIKUBE_ACTIVE_DOCKERD | cut -d= -f2 | tr -d '"'))
	DOCKER_TLS_VERIFY=$(DOCKER_TLS_VERIFY) DOCKER_HOST=$(DOCKER_HOST) DOCKER_CERT_PATH=$(DOCKER_CERT_PATH) \
		$(DOCKER) build -t $(REGISTRY)/rfc-validator:$(MINIKUBE_TAG) rfc-middleware/
	@echo "Image built inside minikube — no push needed."
	@echo "Setting imagePullPolicy: Never on rfc-validator deployment..."
	-$(KUBECTL) patch deployment rfc-validator -n $(NAMESPACE) \
		-p '{"spec":{"template":{"spec":{"containers":[{"name":"rfc-validator","imagePullPolicy":"Never"}]}}}}'

minikube-load: build ## Build with local docker then load the image into minikube
	minikube image load $(REGISTRY)/rfc-validator:$(TAG)
	@echo "Image loaded into minikube."

# ── Local dev ─────────────────────────────────────────────────────────────────
.PHONY: dev-ui dev-middleware dev-setup

dev-ui: ## Start the UI extension webpack dev server (watch mode)
	cd argocd-extension/ui && npm install && npm run dev

dev-middleware: ## Run the RFC middleware locally (requires env vars or .env file)
	cd rfc-middleware && go run ./src/

dev-setup: ## Run the minikube dev environment setup script
	bash dev/setup-minikube.sh
